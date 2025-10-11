/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1alpha1 "serialx.net/cilium-dhcp-wanip-operator/api/v1alpha1"
	sshpkg "serialx.net/cilium-dhcp-wanip-operator/internal/ssh"
)

// GVRs for Cilium pool (try v2 first, then v2alpha1)
var (
	gvrPoolV2  = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumloadbalancerippools"}
	gvrPoolV2a = schema.GroupVersionResource{Group: "cilium.io", Version: "v2alpha1", Resource: "ciliumloadbalancerippools"}
)

const (
	finalizerName       = "serialx.net/cleanup-wan-interface"
	reconcileAnnotation = "serialx.net/last-reconnect"
)

// PublicIPClaimReconciler reconciles a PublicIPClaim object
type PublicIPClaimReconciler struct {
	client.Client
	Kube     *kubernetes.Clientset
	Dynamic  dynamic.Interface
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// SSH connection management
	SSHRegistry         *sshpkg.SSHManagerRegistry
	reconcileQueue      workqueue.RateLimitingInterface
	routerAssignments   map[string]string
	routerAssignmentsMu sync.RWMutex
	sshHandlerMu        sync.RWMutex
	sshHandlers         map[string]uint64 // key: "namespace/name" -> handler ID

	runRouterScriptFn        func(context.Context, *networkv1alpha1.PublicIPClaim, string, string) (string, error)
	ensureIPInPoolFn         func(context.Context, string, string) error
	removeIPFromPoolFn       func(context.Context, string, string) error
	cleanupRouterInterfaceFn func(context.Context, *networkv1alpha1.PublicIPClaim) error
}

type sshCleanupRunnable struct {
	registry *sshpkg.SSHManagerRegistry
	queue    workqueue.RateLimitingInterface
}

func (r *sshCleanupRunnable) Start(ctx context.Context) error {
	<-ctx.Done()

	if r.queue != nil {
		r.queue.ShutDown()
	}

	if r.registry != nil {
		return r.registry.CloseAll()
	}

	return nil
}

// processWorkQueue processes asynchronous reconciliation triggers coming from
// SSH connection events. It touches the claim annotations to ensure the
// controller runtime enqueues a reconcile request.
func (r *PublicIPClaimReconciler) processWorkQueue(client client.Client) {
	if r.reconcileQueue == nil {
		return
	}

	logger := ctrllog.Log.WithName("publicipclaim-queue")

	for {
		item, shutdown := r.reconcileQueue.Get()
		if shutdown {
			return
		}

		func() {
			defer r.reconcileQueue.Done(item)

			req, ok := item.(reconcile.Request)
			if !ok {
				logger.Error(nil, "invalid item type in reconcile queue", "item", item)
				r.reconcileQueue.Forget(item)
				return
			}

			ctx := context.Background()
			claim := &networkv1alpha1.PublicIPClaim{}
			if err := client.Get(ctx, req.NamespacedName, claim); err != nil {
				if !apierrors.IsNotFound(err) {
					logger.Error(err, "failed to get claim for async reconcile", "request", req)
					r.reconcileQueue.AddRateLimited(item)
					return
				}
				r.reconcileQueue.Forget(item)
				return
			}

			if claim.Annotations == nil {
				claim.Annotations = make(map[string]string)
			}
			claim.Annotations[reconcileAnnotation] = time.Now().UTC().Format(time.RFC3339)

			if err := client.Update(ctx, claim); err != nil {
				logger.Error(err, "failed to update claim annotation for async reconcile", "claim", req)
				r.reconcileQueue.AddRateLimited(item)
				return
			}

			r.reconcileQueue.Forget(item)
		}()
	}
}

// +kubebuilder:rbac:groups=network.serialx.net,resources=publicipclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=network.serialx.net,resources=publicipclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.serialx.net,resources=publicipclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumloadbalancerippools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// nolint:gocyclo // Reconcile function is inherently complex due to multiple phases
func (r *PublicIPClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	log.V(1).Info("reconciling PublicIPClaim")

	var claim networkv1alpha1.PublicIPClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !claim.DeletionTimestamp.IsZero() {
		log.Info("deleting PublicIPClaim", "wanInterface", claim.Status.WanInterface)
		if controllerutil.ContainsFinalizer(&claim, finalizerName) {
			// Cleanup router interface
			log.Info("cleaning up router interface", "wanInterface", claim.Status.WanInterface, "router", claim.Spec.Router.Host)
			if err := r.cleanupRouterInterface(ctx, &claim); err != nil {
				log.Error(err, "failed to cleanup router interface", "wanInterface", claim.Status.WanInterface)
				return ctrl.Result{}, err
			}
			log.Info("router interface cleaned up successfully", "wanInterface", claim.Status.WanInterface)

			// Remove IP from Cilium pool
			if claim.Status.AssignedIP != "" {
				log.Info("removing IP from Cilium pool", "pool", claim.Spec.PoolName, "ip", claim.Status.AssignedIP)
				if err := r.removeIPFromPool(ctx, claim.Spec.PoolName, claim.Status.AssignedIP); err != nil {
					log.Error(err, "failed to remove IP from pool", "pool", claim.Spec.PoolName)
					return ctrl.Result{}, err
				}
				log.Info("IP removed from Cilium pool successfully", "pool", claim.Spec.PoolName, "ip", claim.Status.AssignedIP)
			}

			// Remove finalizer
			controllerutil.RemoveFinalizer(&claim, finalizerName)
			if err := r.Update(ctx, &claim); err != nil {
				log.Error(err, "failed to remove finalizer")
				return ctrl.Result{}, err
			}
			log.Info("finalizer removed")
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&claim, finalizerName) {
		log.Info("adding finalizer")
		controllerutil.AddFinalizer(&claim, finalizerName)
		if err := r.Update(ctx, &claim); err != nil {
			log.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		// Return to trigger a new reconcile with the finalizer present
		return ctrl.Result{RequeueAfter: time.Nanosecond}, nil
	}

	if claim.Status.Phase == networkv1alpha1.ClaimPhaseReady && claim.Status.AssignedIP != "" {
		mgr, err := r.getSSHManager(ctx, &claim)
		if err != nil {
			log.Error(err, "failed to get SSH manager for verification")
			if r.Recorder != nil {
				r.Recorder.Event(&claim, corev1.EventTypeWarning, "SSHError", err.Error())
			}
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		verifyErr := r.verifyClaimState(verifyCtx, &claim, mgr)
		cancel()
		if verifyErr == nil {
			if err := r.Status().Update(ctx, &claim); err != nil {
				log.Error(err, "failed to update status after verification")
				return ctrl.Result{}, err
			}
			log.V(1).Info("state verification succeeded", "wanInterface", claim.Status.WanInterface)
			return ctrl.Result{RequeueAfter: 60 * time.Minute}, nil
		}

		log.Info("state verification failed, re-provisioning required", "reason", verifyErr.Error())

		claim.Status.Phase = networkv1alpha1.ClaimPhasePending
		claim.Status.Message = fmt.Sprintf("Reapplying after verification failure: %v", verifyErr)
		claim.Status.AssignedIP = ""
		claim.Status.WanInterface = ""
		claim.Status.MacAddress = ""
		claim.Status.ConfigurationVerified = false
		claim.Status.LastReconciliationReason = "reapply_after_verification"
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ReapplyPending",
			Message:            verifyErr.Error(),
			ObservedGeneration: claim.Generation,
		})
		if err := r.Status().Update(ctx, &claim); err != nil {
			log.Error(err, "failed to update status after verification failure")
			return ctrl.Result{}, err
		}
	}

	// Skip if work is already in progress (WanInterface populated but not Ready yet)
	// This prevents duplicate reconciles from running the router script concurrently
	if claim.Status.WanInterface != "" && claim.Status.Phase != networkv1alpha1.ClaimPhaseFailed {
		log.V(1).Info("allocation already in progress, skipping", "wanInterface", claim.Status.WanInterface)
		return ctrl.Result{}, nil
	}

	// Requeue failed claims after 5 minutes for retry
	if claim.Status.Phase == networkv1alpha1.ClaimPhaseFailed {
		log.Info("claim in Failed state, will retry in 5 minutes", "message", claim.Status.Message)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Validate required fields before attempting allocation
	if claim.Spec.PoolName == "" {
		log.Info("poolName not specified, skipping allocation")
		return r.fail(ctx, &claim, fmt.Errorf("spec.poolName is required"))
	}
	if claim.Spec.Router.Host == "" {
		log.Info("router host not specified, skipping allocation")
		return r.fail(ctx, &claim, fmt.Errorf("spec.router.host is required"))
	}
	if claim.Spec.Router.SSHSecretRef == "" {
		log.Info("SSH secret ref not specified, skipping allocation")
		return r.fail(ctx, &claim, fmt.Errorf("spec.router.sshSecretRef is required"))
	}
	if claim.Spec.Router.User == "" {
		log.Info("router user not specified, skipping allocation")
		return r.fail(ctx, &claim, fmt.Errorf("spec.router.user is required"))
	}
	if claim.Spec.Router.WanParent == "" {
		log.Info("WAN parent interface not specified, skipping allocation")
		return r.fail(ctx, &claim, fmt.Errorf("spec.router.wanParent is required"))
	}
	if claim.Spec.Router.Command == "" {
		log.Info("router command not specified, skipping allocation")
		return r.fail(ctx, &claim, fmt.Errorf("spec.router.command is required"))
	}

	log.Info("starting IP allocation", "pool", claim.Spec.PoolName, "router", claim.Spec.Router.Host)

	// 0) Generate WAN interface name and MAC if not specified
	wanIf := claim.Spec.Router.WanInterface
	if wanIf == "" {
		// Auto-generate from claim name: sanitize and prefix
		wanIf = "wan-" + sanitizeName(claim.Name)
		log.Info("generated WAN interface name", "wanInterface", wanIf)
	}

	macAddr := claim.Spec.Router.MacAddress
	if macAddr == "" {
		// Generate unique locally-administered MAC (02:xx:xx:xx:xx:xx)
		macAddr = generateUniqueMAC()
		log.Info("generated MAC address", "macAddress", macAddr)
	}

	// Persist WAN interface and MAC address BEFORE running script
	// This allows concurrent reconciles to detect work in progress
	claim.Status.WanInterface = wanIf
	claim.Status.MacAddress = macAddr
	if err := r.Status().Update(ctx, &claim); err != nil {
		log.Error(err, "failed to update status with WanInterface")
		return ctrl.Result{}, err
	}
	log.V(1).Info("status updated with WanInterface and MacAddress", "wanInterface", wanIf, "macAddress", macAddr)

	// 1) Run remote script via SSH -> returns a single IPv4 address on stdout
	log.Info("connecting to router via SSH", "router", claim.Spec.Router.Host, "port", coalesceInt(claim.Spec.Router.Port, 22), "user", claim.Spec.Router.User)
	ip, err := r.runRouterScript(ctx, &claim, wanIf, macAddr)
	if err != nil {
		log.Error(err, "router script execution failed")
		return r.fail(ctx, &claim, fmt.Errorf("router script: %w", err))
	}
	log.Info("router script completed successfully", "ip", ip)

	// 2) Validate IP format
	if net.ParseIP(ip) == nil {
		log.Error(fmt.Errorf("invalid IP format"), "IP validation failed", "ip", ip)
		return r.fail(ctx, &claim, fmt.Errorf("invalid IP returned by router: %q", ip))
	}
	log.V(1).Info("IP validation successful", "ip", ip)

	// 3) Ensure IP is in the Cilium pool
	log.Info("adding IP to Cilium pool", "pool", claim.Spec.PoolName, "ip", ip)
	if err := r.ensureIPInPool(ctx, claim.Spec.PoolName, ip); err != nil {
		log.Error(err, "failed to add IP to pool", "pool", claim.Spec.PoolName)
		return r.fail(ctx, &claim, fmt.Errorf("ensure pool: %w", err))
	}
	log.Info("IP added to Cilium pool successfully", "pool", claim.Spec.PoolName, "ip", ip)

	if mgr, err := r.getSSHManager(ctx, &claim); err == nil {
		uptimeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if uptime, err := mgr.GetRouterUptime(uptimeCtx); err == nil {
			claim.Status.RouterUptime = int64(uptime / time.Second)
		}
		cancel()
	}

	// 4) Update status with all fields at once (single update to avoid conflicts)
	now := metav1.Now()
	claim.Status.AssignedIP = ip
	claim.Status.Phase = networkv1alpha1.ClaimPhaseReady
	claim.Status.Message = "Assigned"
	claim.Status.LastVerified = &now
	claim.Status.ConfigurationVerified = true
	claim.Status.LastReconciliationReason = "configuration_applied"
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "ConfigurationApplied",
		Message:            "Configuration applied successfully",
		ObservedGeneration: claim.Generation,
		LastTransitionTime: now,
	})
	if r.Recorder != nil {
		r.Recorder.Event(&claim, corev1.EventTypeNormal, "ConfigurationApplied", "Configuration applied successfully")
	}
	if err := r.Status().Update(ctx, &claim); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	log.Info("✓ public IP assigned successfully", "ip", ip, "pool", claim.Spec.PoolName, "wanInterface", wanIf, "macAddress", macAddr)
	return ctrl.Result{}, nil
}

func (r *PublicIPClaimReconciler) runRouterScript(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
	if r.runRouterScriptFn != nil {
		return r.runRouterScriptFn(ctx, claim, wanIf, macAddr)
	}
	return r.defaultRunRouterScript(ctx, claim, wanIf, macAddr)
}

func (r *PublicIPClaimReconciler) defaultRunRouterScript(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
	log := ctrllog.FromContext(ctx)

	// Get SSH manager for this router (connection pooling)
	log.V(1).Info("getting SSH manager", "router", claim.Spec.Router.Host)
	mgr, err := r.getSSHManager(ctx, claim)
	if err != nil {
		return "", err
	}

	// Build command with environment variables
	cmd := fmt.Sprintf("export WAN_PARENT=%q WAN_IF=%q WAN_MAC=%q && %s",
		claim.Spec.Router.WanParent, wanIf, macAddr, claim.Spec.Router.Command)

	log.Info("executing router script via SSH manager",
		"router", claim.Spec.Router.Host,
		"script", claim.Spec.Router.Command,
		"wanParent", claim.Spec.Router.WanParent,
		"wanInterface", wanIf,
		"macAddress", macAddr)

	// Execute command via SSH manager with timeout
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	output, err := mgr.RunCommand(cmdCtx, cmd)
	if err != nil {
		return "", fmt.Errorf("router script failed: %w", err)
	}

	log.V(1).Info("script output received", "lines", len(strings.Split(string(output), "\n")))

	// Extract the last non-empty line as the IP address
	// This allows the script to output debug information on earlier lines
	lines := strings.Split(string(output), "\n")
	var ip string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			ip = line
			break
		}
	}

	if ip == "" {
		return "", fmt.Errorf("router script produced no output")
	}

	log.Info("router script completed successfully", "ip", ip)
	return ip, nil
}

func (r *PublicIPClaimReconciler) ensureIPInPool(ctx context.Context, poolName, ip string) error {
	if r.ensureIPInPoolFn != nil {
		return r.ensureIPInPoolFn(ctx, poolName, ip)
	}
	return r.defaultEnsureIPInPool(ctx, poolName, ip)
}

func (r *PublicIPClaimReconciler) defaultEnsureIPInPool(ctx context.Context, poolName, ip string) error {
	log := ctrllog.FromContext(ctx)

	// pick available GVR
	log.V(1).Info("detecting Cilium API version", "pool", poolName)
	gvr := gvrPoolV2
	if _, err := r.Dynamic.Resource(gvr).Get(ctx, poolName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("pool not found in cilium.io/v2, trying v2alpha1")
			gvr = gvrPoolV2a
		} else {
			return fmt.Errorf("failed to check pool: %w", err)
		}
	}
	log.V(1).Info("using Cilium API version", "group", gvr.Group, "version", gvr.Version)

	log.V(1).Info("fetching Cilium pool", "pool", poolName)
	pool, err := r.Dynamic.Resource(gvr).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pool %s: %w", poolName, err)
	}

	// Append a new /32 block if it doesn't already exist
	spec, found, _ := unstructured.NestedSlice(pool.Object, "spec", "blocks")
	if !found {
		log.V(1).Info("pool has no existing blocks, creating new list")
		spec = []interface{}{}
	}

	cidr := fmt.Sprintf("%s/32", ip)
	for _, b := range spec {
		m := b.(map[string]interface{})
		if m["cidr"] == cidr { // already present
			log.Info("IP already exists in pool, skipping", "pool", poolName, "cidr", cidr)
			return nil
		}
	}

	log.Info("adding IP block to pool", "pool", poolName, "cidr", cidr)
	spec = append(spec, map[string]interface{}{"cidr": cidr})
	if err := unstructured.SetNestedSlice(pool.Object, spec, "spec", "blocks"); err != nil {
		return fmt.Errorf("failed to set blocks in pool: %w", err)
	}

	log.V(1).Info("updating pool", "pool", poolName, "totalBlocks", len(spec))
	_, err = r.Dynamic.Resource(gvr).Update(ctx, pool, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update pool %s: %w", poolName, err)
	}

	return nil
}

func (r *PublicIPClaimReconciler) removeIPFromPool(ctx context.Context, poolName, ip string) error {
	if r.removeIPFromPoolFn != nil {
		return r.removeIPFromPoolFn(ctx, poolName, ip)
	}
	return r.defaultRemoveIPFromPool(ctx, poolName, ip)
}

func (r *PublicIPClaimReconciler) defaultRemoveIPFromPool(ctx context.Context, poolName, ip string) error {
	log := ctrllog.FromContext(ctx)

	if ip == "" {
		log.V(1).Info("no IP to remove from pool")
		return nil
	}

	// Detect GVR (v2 or v2alpha1)
	log.V(1).Info("detecting Cilium API version for removal", "pool", poolName)
	gvr := gvrPoolV2
	pool, err := r.Dynamic.Resource(gvr).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("pool not found in cilium.io/v2, trying v2alpha1")
			gvr = gvrPoolV2a
			pool, err = r.Dynamic.Resource(gvr).Get(ctx, poolName, metav1.GetOptions{})
		}
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("pool not found, skipping IP removal", "pool", poolName)
				return nil // Pool doesn't exist, nothing to clean
			}
			return fmt.Errorf("failed to get pool %s: %w", poolName, err)
		}
	}
	log.V(1).Info("using Cilium API version for removal", "group", gvr.Group, "version", gvr.Version)

	// Get existing blocks
	spec, found, _ := unstructured.NestedSlice(pool.Object, "spec", "blocks")
	if !found || len(spec) == 0 {
		log.V(1).Info("pool has no blocks, nothing to remove")
		return nil
	}

	// Remove the matching CIDR block
	cidr := fmt.Sprintf("%s/32", ip)
	newSpec := []interface{}{}
	removed := false
	for _, b := range spec {
		m := b.(map[string]interface{})
		if m["cidr"] == cidr {
			log.Info("found IP block to remove", "pool", poolName, "cidr", cidr)
			removed = true
			continue // Skip this block (remove it)
		}
		newSpec = append(newSpec, b)
	}

	if !removed {
		log.Info("IP not found in pool, nothing to remove", "pool", poolName, "cidr", cidr)
		return nil
	}

	// Update pool with new blocks list
	if err := unstructured.SetNestedSlice(pool.Object, newSpec, "spec", "blocks"); err != nil {
		return fmt.Errorf("failed to set blocks in pool: %w", err)
	}

	log.V(1).Info("updating pool after removal", "pool", poolName, "totalBlocks", len(newSpec))
	_, err = r.Dynamic.Resource(gvr).Update(ctx, pool, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update pool %s: %w", poolName, err)
	}

	return nil
}

// Helper: sanitize claim name for use in interface name (remove invalid chars)
func sanitizeName(name string) string {
	// Keep alphanumeric and hyphens only, max 15 chars for ifname
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(name))

	if len(sanitized) > 10 { // wan- prefix takes 4, leave room
		sanitized = sanitized[:10]
	}
	return sanitized
}

// Helper: generate unique locally-administered unicast MAC (02:xx:xx:xx:xx:xx)
func generateUniqueMAC() string {
	buf := make([]byte, 5)
	_, _ = rand.Read(buf) // Error can be safely ignored - rand.Read from crypto/rand always returns nil error
	// Set locally administered bit (bit 1 of first octet) and unicast (bit 0 = 0)
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4])
}

// Helper: return first value if non-zero, otherwise return second value
func coalesceInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// Helper: update status to failed state
// nolint:unparam // ctrl.Result is intentionally always zero for consistent signature
func (r *PublicIPClaimReconciler) fail(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, err error) (ctrl.Result, error) {
	claim.Status.Phase = networkv1alpha1.ClaimPhaseFailed
	claim.Status.Message = err.Error()
	claim.Status.ConfigurationVerified = false
	claim.Status.LastReconciliationReason = "failed"
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "ProvisioningFailed",
		Message:            err.Error(),
		ObservedGeneration: claim.Generation,
	})
	if r.Recorder != nil {
		r.Recorder.Event(claim, corev1.EventTypeWarning, "ProvisioningFailed", err.Error())
	}
	if updateErr := r.Status().Update(ctx, claim); updateErr != nil {
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{}, err
}

func (r *PublicIPClaimReconciler) validateSSHCredentials(ctx context.Context, routerHost, secretName, claimKey string) error {
	if routerHost == "" || secretName == "" {
		return nil
	}

	claims := &networkv1alpha1.PublicIPClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return fmt.Errorf("failed to list claims: %w", err)
	}

	for _, existing := range claims.Items {
		if existing.Spec.Router.Host != routerHost {
			continue
		}
		existingKey := fmt.Sprintf("%s/%s", existing.Namespace, existing.Name)
		if existingKey == claimKey {
			continue
		}
		if existing.Spec.Router.SSHSecretRef != secretName {
			return fmt.Errorf("SSH credential mismatch for router %s: claim %s/%s uses secret %q but requested %q",
				routerHost,
				existing.Namespace,
				existing.Name,
				existing.Spec.Router.SSHSecretRef,
				secretName,
			)
		}
	}

	return nil
}

// getSSHManager retrieves or creates an SSH manager for the claim's router.
// Phase 2: Registers event handlers with deduplication to enable automatic reconciliation.
func (r *PublicIPClaimReconciler) getSSHManager(ctx context.Context, claim *networkv1alpha1.PublicIPClaim) (*sshpkg.SSHConnectionManager, error) {
	log := ctrllog.FromContext(ctx)

	// Get SSH credentials from secret
	claimKey := fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)

	if err := r.validateSSHCredentials(ctx, claim.Spec.Router.Host, claim.Spec.Router.SSHSecretRef, claimKey); err != nil {
		return nil, err
	}

	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.Router.SSHSecretRef, Namespace: "kube-system"}, sec); err != nil {
		return nil, fmt.Errorf("failed to get SSH secret: %w", err)
	}

	key := sec.Data["id_rsa"]
	if len(key) == 0 {
		return nil, fmt.Errorf("ssh private key not found in secret %s/id_rsa", claim.Spec.Router.SSHSecretRef)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SSH private key: %w", err)
	}

	// Build SSH manager config
	addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

	hostKeyCallback := ssh.InsecureIgnoreHostKey()
	if info, err := os.Stat("/etc/ssh-operator/known_hosts"); err == nil && !info.IsDir() {
		if callback, err := knownhosts.New("/etc/ssh-operator/known_hosts"); err == nil {
			hostKeyCallback = callback
		} else {
			log.Error(err, "failed to load known_hosts, falling back to insecure host key verification")
		}
	}

	config := sshpkg.RouterConfig{
		Address:           addr,
		Username:          claim.Spec.Router.User,
		AuthMethod:        ssh.PublicKeys(signer),
		HostKeyCallback:   hostKeyCallback,
		Timeout:           30 * time.Second,
		KeepAliveInterval: 30 * time.Second,
		KeepAliveCommand:  "cat /proc/uptime",
	}

	if r.SSHRegistry == nil {
		r.SSHRegistry = sshpkg.NewRegistry()
	}

	// If the claim previously pointed to a different router, unregister the old handler
	r.routerAssignmentsMu.RLock()
	previousRouter := r.routerAssignments[claimKey]
	r.routerAssignmentsMu.RUnlock()

	if previousRouter != "" && previousRouter != addr {
		if prevMgr := r.SSHRegistry.Lookup(previousRouter); prevMgr != nil {
			r.sshHandlerMu.Lock()
			if handlerID, exists := r.sshHandlers[claimKey]; exists {
				prevMgr.UnregisterHandler(handlerID)
				delete(r.sshHandlers, claimKey)
			}
			r.sshHandlerMu.Unlock()

			if prevMgr.HandlerCount() == 0 {
				if err := prevMgr.Close(); err != nil {
					log.Error(err, "failed to close SSH manager after router change", "router", previousRouter)
				}
				r.SSHRegistry.Remove(previousRouter)
			}
		}
	}

	// Get or create manager from registry
	mgr, err := r.SSHRegistry.GetOrCreate(config)
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH manager: %w", err)
	}

	// Start manager if not already started
	if !mgr.IsConnected() && !mgr.IsReconnecting() {
		if err := mgr.Start(ctx); err != nil {
			log.V(1).Info("SSH manager starting in background", "router", addr, "error", err.Error())
		} else {
			log.Info("SSH manager started", "router", addr)
		}
	}

	switch {
	case mgr.IsConnected():
		claim.Status.ConnectionState = "Connected"
	case mgr.IsReconnecting():
		claim.Status.ConnectionState = "Reconnecting"
	default:
		claim.Status.ConnectionState = "Disconnected"
	}

	// Register event handler with deduplication (Phase 2)
	handlerKey := fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)

	r.sshHandlerMu.Lock()
	if _, exists := r.sshHandlers[handlerKey]; !exists {
		handlerID := mgr.RegisterHandler(func(event sshpkg.Event) {
			bgCtx := context.Background()

			log.Info("Router SSH event detected", "router", addr, "reason", event.Reason, "claim", handlerKey)

			if r.reconcileQueue != nil {
				r.reconcileQueue.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}})
			}

			if r.Recorder != nil {
				currentClaim := &networkv1alpha1.PublicIPClaim{}
				if err := r.Get(bgCtx, types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, currentClaim); err == nil {
					switch event.Reason {
					case sshpkg.ReasonReconnectSuccess:
						r.Recorder.Event(currentClaim, corev1.EventTypeNormal, "SSHConnected", "SSH connection established")
					case sshpkg.ReasonConnectionDrop:
						r.Recorder.Event(currentClaim, corev1.EventTypeWarning, "SSHDisconnected", "SSH connection dropped")
					case sshpkg.ReasonReboot:
						r.Recorder.Event(currentClaim, corev1.EventTypeWarning, "RouterRebooted", "Router reboot detected")
					}
				} else if !apierrors.IsNotFound(err) {
					log.Error(err, "failed to fetch claim for event recording", "claim", handlerKey)
				}
			}
		})

		r.sshHandlers[handlerKey] = handlerID
		log.Info("registered SSH event handler", "claim", handlerKey, "handlerID", handlerID, "router", addr)
	} else {
		log.V(1).Info("SSH event handler already registered, skipping", "claim", handlerKey)
	}
	r.sshHandlerMu.Unlock()

	r.routerAssignmentsMu.Lock()
	r.routerAssignments[claimKey] = addr
	r.routerAssignmentsMu.Unlock()

	return mgr, nil
}

func (r *PublicIPClaimReconciler) verifyClaimState(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, mgr *sshpkg.SSHConnectionManager) error {
	log := ctrllog.FromContext(ctx)

	uptimeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	uptime, err := mgr.GetRouterUptime(uptimeCtx)
	if err != nil {
		return fmt.Errorf("failed to get router uptime: %w", err)
	}

	currentSeconds := int64(uptime / time.Second)
	if claim.Status.RouterUptime > 0 && currentSeconds < claim.Status.RouterUptime {
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeWarning, "RouterRebooted", "Router reboot detected, reapplying configuration")
		}
		claim.Status.ConfigurationVerified = false
		claim.Status.RouterUptime = currentSeconds
		claim.Status.LastReconciliationReason = "router_reboot"
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "RouterRebooted",
			Message:            "Router reboot detected",
			ObservedGeneration: claim.Generation,
		})
		return fmt.Errorf("router reboot detected")
	}
	claim.Status.RouterUptime = currentSeconds

	if claim.Status.WanInterface == "" {
		claim.Status.ConfigurationVerified = false
		claim.Status.LastReconciliationReason = "interface_pending"
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "InterfacePending",
			Message:            "WAN interface not yet provisioned",
			ObservedGeneration: claim.Generation,
		})
		return fmt.Errorf("wan interface not yet recorded")
	}

	ifaceCtx, cancelIface := context.WithTimeout(ctx, 10*time.Second)
	exists, err := mgr.InterfaceExists(ifaceCtx, claim.Status.WanInterface)
	cancelIface()
	if err != nil {
		return fmt.Errorf("failed to check interface existence: %w", err)
	}
	if !exists {
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeWarning, "ConfigurationDrift", "Interface missing, reapplying configuration")
		}
		claim.Status.ConfigurationVerified = false
		claim.Status.LastReconciliationReason = "interface_missing"
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "InterfaceMissing",
			Message:            "Router interface missing",
			ObservedGeneration: claim.Generation,
		})
		return fmt.Errorf("interface %s missing", claim.Status.WanInterface)
	}

	dhcpCtx, cancelDHCP := context.WithTimeout(ctx, 10*time.Second)
	running, err := mgr.IsUdhcpcRunningOnInterface(dhcpCtx, claim.Status.WanInterface)
	cancelDHCP()
	if err != nil {
		return fmt.Errorf("failed to check udhcpc status: %w", err)
	}
	if !running {
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeWarning, "DHCPClientStopped", "DHCP client not running, reapplying configuration")
		}
		claim.Status.ConfigurationVerified = false
		claim.Status.LastReconciliationReason = "dhcp_client_stopped"
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "DHCPClientStopped",
			Message:            "DHCP client stopped",
			ObservedGeneration: claim.Generation,
		})
		return fmt.Errorf("dhcp client not running for %s", claim.Status.WanInterface)
	}

	arpCtx, cancelARP := context.WithTimeout(ctx, 10*time.Second)
	proxyARPEnabled, err := mgr.IsProxyARPEnabled(arpCtx, claim.Status.WanInterface)
	cancelARP()
	if err != nil {
		return fmt.Errorf("failed to check proxy ARP: %w", err)
	}
	if !proxyARPEnabled {
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeWarning, "ProxyARPDisabled", "Proxy ARP disabled, reapplying configuration")
		}
		claim.Status.ConfigurationVerified = false
		claim.Status.LastReconciliationReason = "proxy_arp_disabled"
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ProxyARPDisabled",
			Message:            "Proxy ARP disabled",
			ObservedGeneration: claim.Generation,
		})
		return fmt.Errorf("proxy arp disabled for %s", claim.Status.WanInterface)
	}

	now := metav1.Now()
	claim.Status.LastVerified = &now
	claim.Status.ConfigurationVerified = true
	claim.Status.LastReconciliationReason = "verified"
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "ConfigurationVerified",
		Message:            "Interface, DHCP client, and proxy ARP verified",
		ObservedGeneration: claim.Generation,
		LastTransitionTime: now,
	})

	log.V(1).Info("state verification passed", "interface", claim.Status.WanInterface)
	return nil
}

func (r *PublicIPClaimReconciler) cleanupRouterInterface(ctx context.Context, claim *networkv1alpha1.PublicIPClaim) error {
	if r.cleanupRouterInterfaceFn != nil {
		return r.cleanupRouterInterfaceFn(ctx, claim)
	}
	return r.defaultCleanupRouterInterface(ctx, claim)
}

func (r *PublicIPClaimReconciler) defaultCleanupRouterInterface(ctx context.Context, claim *networkv1alpha1.PublicIPClaim) error {
	log := ctrllog.FromContext(ctx)

	if claim.Status.WanInterface == "" {
		log.V(1).Info("no WAN interface to clean up, skipping")
		return nil
	}

	// Try to get SSH manager - it might not exist if router was never reached
	mgr, err := r.getSSHManager(ctx, claim)
	if err != nil {
		log.Info("cannot get SSH manager for cleanup, skipping", "error", err.Error())
		return nil
	}

	// Check if interface exists before cleanup (using command helper)
	cleanupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	exists, err := mgr.InterfaceExists(cleanupCtx, claim.Status.WanInterface)
	if err != nil {
		log.Error(err, "failed to check interface existence", "interface", claim.Status.WanInterface)
	} else if !exists {
		log.Info("interface does not exist, skipping cleanup", "interface", claim.Status.WanInterface)
		goto checkManagerLifecycle
	}

	log.Info("cleaning up router interface", "interface", claim.Status.WanInterface)

	// Execute cleanup commands via SSH manager
	{
		// Kill udhcpc daemon, remove proxy ARP, and delete the interface
		cmd := fmt.Sprintf(`
			WAN_IF="%s"

			# Kill DHCP client daemon
			PID_FILE="/var/run/udhcpc.$WAN_IF.pid"
			if [ -f "$PID_FILE" ]; then
				kill $(cat "$PID_FILE") 2>/dev/null || true
				rm -f "$PID_FILE"
			fi

			# Remove proxy ARP entries
			ip neigh show proxy dev "$WAN_IF" 2>/dev/null | awk '{print $1}' | while read -r ip; do
				ip neigh del proxy "$ip" dev "$WAN_IF" 2>/dev/null || true
			done

			# Delete the interface
			ip link del "$WAN_IF" 2>/dev/null || true
		`, claim.Status.WanInterface)

		if _, err := mgr.RunCommand(cleanupCtx, cmd); err != nil {
			log.Error(err, "cleanup commands failed")
			// Continue anyway - best effort cleanup
		} else {
			log.Info("router interface cleaned up successfully", "interface", claim.Status.WanInterface)
		}
	}

checkManagerLifecycle:
	// Phase 2: Unregister event handler and manage SSH manager lifecycle
	handlerKey := fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)

	r.sshHandlerMu.Lock()
	if handlerID, exists := r.sshHandlers[handlerKey]; exists {
		mgr.UnregisterHandler(handlerID)
		delete(r.sshHandlers, handlerKey)
		log.Info("unregistered SSH event handler",
			"claim", handlerKey,
			"handlerID", handlerID)
	}
	r.sshHandlerMu.Unlock()

	r.routerAssignmentsMu.Lock()
	delete(r.routerAssignments, handlerKey)
	r.routerAssignmentsMu.Unlock()

	// If no more handlers registered for this manager, close it
	addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))
	if mgr.HandlerCount() == 0 {
		log.Info("no more claims using this router, closing SSH manager", "router", addr)
		if err := mgr.Close(); err != nil {
			log.Error(err, "failed to close SSH manager", "router", addr)
		}
		r.SSHRegistry.Remove(addr)
		log.Info("SSH manager removed from registry", "router", addr)
	} else {
		log.V(1).Info("SSH manager still in use by other claims",
			"router", addr,
			"handlerCount", mgr.HandlerCount())
	}

	log.V(1).Info("cleanup complete", "interface", claim.Status.WanInterface)

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PublicIPClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.sshHandlers == nil {
		r.sshHandlers = make(map[string]uint64)
	}
	if r.routerAssignments == nil {
		r.routerAssignments = make(map[string]string)
	}
	if r.SSHRegistry == nil {
		r.SSHRegistry = sshpkg.NewRegistry()
	}
	if r.reconcileQueue == nil {
		r.reconcileQueue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	}

	if err := mgr.Add(&sshCleanupRunnable{
		registry: r.SSHRegistry,
		queue:    r.reconcileQueue,
	}); err != nil {
		return err
	}

	go r.processWorkQueue(mgr.GetClient())

	return ctrl.NewControllerManagedBy(mgr).
		For(&networkv1alpha1.PublicIPClaim{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 3}).
		Named("publicipclaim").
		Complete(r)
}
