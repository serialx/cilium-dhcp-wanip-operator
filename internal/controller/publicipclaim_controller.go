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

	var claim networkv1alpha1.PublicIPClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !claim.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&claim, finalizerName) {
			// Cleanup router interface
			if err := r.cleanupRouterInterface(ctx, &claim); err != nil {
				log.Error(err, "failed to cleanup router interface")
				return ctrl.Result{}, err
			}

			// Remove finalizer
			controllerutil.RemoveFinalizer(&claim, finalizerName)
			if err := r.Update(ctx, &claim); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&claim, finalizerName) {
		controllerutil.AddFinalizer(&claim, finalizerName)
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Skip if already successfully assigned
	if claim.Status.Phase == networkv1alpha1.ClaimPhaseReady && claim.Status.AssignedIP != "" {
		return ctrl.Result{}, nil
	}

	// Requeue failed claims after 5 minutes for retry
	if claim.Status.Phase == networkv1alpha1.ClaimPhaseFailed {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// 0) Generate WAN interface name and MAC if not specified
	wanIf := claim.Spec.Router.WanInterface
	if wanIf == "" {
		// Auto-generate from claim name: sanitize and prefix
		wanIf = "wan-" + sanitizeName(claim.Name)
	}

	macAddr := claim.Spec.Router.MacAddress
	if macAddr == "" {
		// Generate unique locally-administered MAC (02:xx:xx:xx:xx:xx)
		macAddr = generateUniqueMAC()
	}

	// Store in status (will update at the end)
	claim.Status.WanInterface = wanIf
	claim.Status.MacAddress = macAddr

	// 1) Run remote script via SSH -> returns a single IPv4 address on stdout
	ip, err := r.runRouterScript(ctx, &claim, wanIf, macAddr)
	if err != nil {
		return r.fail(ctx, &claim, fmt.Errorf("router script: %w", err))
	}

	// 2) Validate IP format
	if net.ParseIP(ip) == nil {
		return r.fail(ctx, &claim, fmt.Errorf("invalid IP returned by router: %q", ip))
	}

	// 3) Ensure IP is in the Cilium pool
	if err := r.ensureIPInPool(ctx, claim.Spec.PoolName, ip); err != nil {
		return r.fail(ctx, &claim, fmt.Errorf("ensure pool: %w", err))
	}

	// 4) Update status with all fields at once (single update to avoid conflicts)
	claim.Status.AssignedIP = ip
	claim.Status.Phase = networkv1alpha1.ClaimPhaseReady
	claim.Status.Message = "Assigned"
	if err := r.Status().Update(ctx, &claim); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("public IP assigned", "ip", ip, "pool", claim.Spec.PoolName, "wanIf", wanIf, "mac", macAddr)
	return ctrl.Result{}, nil
}

func (r *PublicIPClaimReconciler) runRouterScript(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
	// SSH secret
	sec := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.Router.SSHSecretRef, Namespace: "kube-system"}, sec); err != nil {
		return "", err
	}
	key := sec.Data["id_rsa"]
	if len(key) == 0 {
		return "", fmt.Errorf("ssh private key not found in secret %s/id_rsa", claim.Spec.Router.SSHSecretRef)
	}

	// Dial SSH
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return "", err
	}

	conf := &ssh.ClientConfig{
		User:            claim.Spec.Router.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

	conn, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	// Set environment variables for the script
	// Using export ensures variables work regardless of shell
	cmd := fmt.Sprintf("export WAN_PARENT=%q WAN_IF=%q WAN_MAC=%q && %s",
		claim.Spec.Router.WanParent, wanIf, macAddr, claim.Spec.Router.Command)

	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, string(out))
	}

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

	return ip, nil
}

func (r *PublicIPClaimReconciler) ensureIPInPool(ctx context.Context, poolName, ip string) error {
	// pick available GVR
	gvr := gvrPoolV2
	if _, err := r.Dynamic.Resource(gvr).Get(ctx, poolName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			gvr = gvrPoolV2a
		} else {
			return err
		}
	}

	pool, err := r.Dynamic.Resource(gvr).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Append a new /32 block if it doesn't already exist
	spec, found, _ := unstructured.NestedSlice(pool.Object, "spec", "blocks")
	if !found {
		spec = []interface{}{}
	}

	cidr := fmt.Sprintf("%s/32", ip)
	for _, b := range spec {
		m := b.(map[string]interface{})
		if m["cidr"] == cidr { // already present
			return nil
		}
	}

	spec = append(spec, map[string]interface{}{"cidr": cidr})
	if err := unstructured.SetNestedSlice(pool.Object, spec, "spec", "blocks"); err != nil {
		return err
	}

	_, err = r.Dynamic.Resource(gvr).Update(ctx, pool, metav1.UpdateOptions{})
	return err
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
	if claim.Status.WanInterface == "" {
		return nil // Nothing to clean up
	}

	// SSH secret
	sec := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.Router.SSHSecretRef, Namespace: "kube-system"}, sec); err != nil {
		return client.IgnoreNotFound(err) // Secret might be deleted already
	}
	key := sec.Data["id_rsa"]
	if len(key) == 0 {
		return nil
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return err
	}

	conf := &ssh.ClientConfig{
		User:            claim.Spec.Router.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

	conn, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		return err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return err
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
	_, err = sess.CombinedOutput(cmd)
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *PublicIPClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkv1alpha1.PublicIPClaim{}).
		Named("publicipclaim").
		Complete(r)
}
