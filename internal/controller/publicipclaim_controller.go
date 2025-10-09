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
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	networkv1alpha1 "serialx.net/cilium-dhcp-wanip-operator/api/v1alpha1"
)

// GVRs for Cilium pool (try v2 first, then v2alpha1)
var (
	gvrPoolV2  = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumloadbalancerippools"}
	gvrPoolV2a = schema.GroupVersionResource{Group: "cilium.io", Version: "v2alpha1", Resource: "ciliumloadbalancerippools"}
)

const finalizerName = "serialx.net/cleanup-wan-interface"

// PublicIPClaimReconciler reconciles a PublicIPClaim object
type PublicIPClaimReconciler struct {
	client.Client
	Kube    *kubernetes.Clientset
	Dynamic dynamic.Interface
	Scheme  *runtime.Scheme
}

// +kubebuilder:rbac:groups=network.serialx.net,resources=publicipclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=network.serialx.net,resources=publicipclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.serialx.net,resources=publicipclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumloadbalancerippools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
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
		return ctrl.Result{Requeue: true}, nil
	}

	// Skip if already successfully assigned
	if claim.Status.Phase == networkv1alpha1.ClaimPhaseReady && claim.Status.AssignedIP != "" {
		log.V(1).Info("claim already in Ready state, skipping", "ip", claim.Status.AssignedIP)
		return ctrl.Result{}, nil
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

	// 4) Update status with all fields at once (single update to avoid conflicts)
	claim.Status.AssignedIP = ip
	claim.Status.Phase = networkv1alpha1.ClaimPhaseReady
	claim.Status.Message = "Assigned"
	if err := r.Status().Update(ctx, &claim); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	log.Info("✓ public IP assigned successfully", "ip", ip, "pool", claim.Spec.PoolName, "wanInterface", wanIf, "macAddress", macAddr)
	return ctrl.Result{}, nil
}

func (r *PublicIPClaimReconciler) runRouterScript(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
	log := ctrllog.FromContext(ctx)

	// SSH secret
	log.V(1).Info("retrieving SSH secret", "secret", claim.Spec.Router.SSHSecretRef, "namespace", "kube-system")
	sec := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.Router.SSHSecretRef, Namespace: "kube-system"}, sec); err != nil {
		return "", fmt.Errorf("failed to get SSH secret: %w", err)
	}
	key := sec.Data["id_rsa"]
	if len(key) == 0 {
		return "", fmt.Errorf("ssh private key not found in secret %s/id_rsa", claim.Spec.Router.SSHSecretRef)
	}
	log.V(1).Info("SSH secret retrieved successfully")

	// Dial SSH
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("failed to parse SSH private key: %w", err)
	}

	conf := &ssh.ClientConfig{
		User:            claim.Spec.Router.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

	log.V(1).Info("establishing SSH connection", "address", addr)
	conn, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		return "", fmt.Errorf("failed to connect via SSH to %s: %w", addr, err)
	}
	defer conn.Close()
	log.V(1).Info("SSH connection established")

	sess, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer sess.Close()

	// Set environment variables for the script
	// Using export ensures variables work regardless of shell
	cmd := fmt.Sprintf("export WAN_PARENT=%q WAN_IF=%q WAN_MAC=%q && %s",
		claim.Spec.Router.WanParent, wanIf, macAddr, claim.Spec.Router.Command)

	log.Info("executing router script", "script", claim.Spec.Router.Command, "wanParent", claim.Spec.Router.WanParent, "wanInterface", wanIf, "macAddress", macAddr)
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, string(out))
	}
	log.V(1).Info("script output received", "lines", len(strings.Split(string(out), "\n")))

	// Extract the last non-empty line as the IP address
	// This allows the script to output debug information on earlier lines
	lines := strings.Split(string(out), "\n")
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

	log.V(1).Info("extracted IP from script output", "ip", ip)
	return ip, nil
}

func (r *PublicIPClaimReconciler) ensureIPInPool(ctx context.Context, poolName, ip string) error {
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
	rand.Read(buf)
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
func (r *PublicIPClaimReconciler) fail(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, err error) (ctrl.Result, error) {
	claim.Status.Phase = networkv1alpha1.ClaimPhaseFailed
	claim.Status.Message = err.Error()
	if updateErr := r.Status().Update(ctx, claim); updateErr != nil {
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{}, err
}

func (r *PublicIPClaimReconciler) cleanupRouterInterface(ctx context.Context, claim *networkv1alpha1.PublicIPClaim) error {
	log := ctrllog.FromContext(ctx)

	if claim.Status.WanInterface == "" {
		log.V(1).Info("no WAN interface to clean up, skipping")
		return nil // Nothing to clean up
	}

	log.V(1).Info("retrieving SSH secret for cleanup", "secret", claim.Spec.Router.SSHSecretRef)
	// SSH secret
	sec := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.Router.SSHSecretRef, Namespace: "kube-system"}, sec); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "failed to get SSH secret for cleanup")
		} else {
			log.Info("SSH secret not found, skipping cleanup (may have been deleted)")
		}
		return client.IgnoreNotFound(err) // Secret might be deleted already
	}
	key := sec.Data["id_rsa"]
	if len(key) == 0 {
		log.Info("SSH key not found in secret, skipping cleanup")
		return nil
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to parse SSH key for cleanup: %w", err)
	}

	conf := &ssh.ClientConfig{
		User:            claim.Spec.Router.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

	log.V(1).Info("connecting to router for cleanup", "address", addr)
	conn, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		return fmt.Errorf("failed to connect to router for cleanup: %w", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session for cleanup: %w", err)
	}
	defer sess.Close()

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

	log.Info("executing cleanup commands on router", "wanInterface", claim.Status.WanInterface)
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		log.Error(err, "cleanup script failed", "output", string(out))
		return fmt.Errorf("failed to execute cleanup: %w", err)
	}
	log.V(1).Info("cleanup commands executed successfully")

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PublicIPClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkv1alpha1.PublicIPClaim{}).
		Named("publicipclaim").
		Complete(r)
}
