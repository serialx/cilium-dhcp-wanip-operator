# Cilium LB IP Pool Operator — starter (Go/Kubebuilder)

A minimal operator that:

1. Watches a custom resource `PublicIPClaim` (one claim per public IP you want).
2. On a new claim, SSHes into your router and runs your script to allocate a fresh public IP (by DHCP/macvlan or your preferred method). The script should **print just the IP** on stdout (e.g., `203.0.113.45`).
3. Patches/updates **`CiliumLoadBalancerIPPool`** to include the new `/32` (or `/128`) in `spec.blocks`.
4. Reports the IP in `status.assignedIP` and marks the claim `Ready`.

This keeps your Cilium pool in sync with what your router actually acquired.

> **Note on Cilium API version**: Cilium moved the pool CRD from `cilium.io/v2alpha1` to `cilium.io/v2`. The controller below auto-detects and uses whichever exists.

---

## 0) Router-side script (Proxy ARP + Cilium BGP)

Router-side script (`/usr/local/bin/alloc_public_ip.sh`) that:

* creates a macvlan interface and obtains a DHCP lease for a new public IP,
* configures **proxy ARP** so the router responds to ARP requests for the IP on the WAN interface,
* relies on **Cilium BGP** to advertise routing information (no static routes needed).

```sh
#!/bin/bash
set -euo pipefail

# These will be passed as environment variables from the operator
# WAN_PARENT, WAN_IF, WAN_MAC are provided by the PublicIPClaim
: "${WAN_PARENT:?WAN_PARENT must be set}"
: "${WAN_IF:?WAN_IF must be set}"
: "${WAN_MAC:?WAN_MAC must be set}"

# 1) Prepare macvlan (bridge mode), no primary IP needed
ip link show "$WAN_IF" >/dev/null 2>&1 || {
  ip link add link "$WAN_PARENT" name "$WAN_IF" type macvlan mode bridge
  ip link set "$WAN_IF" address "$WAN_MAC" up
}

# 2) Pull an address by DHCP and run daemon to maintain the lease
#    This will fork to background and handle renewals automatically
#    Note: On UDM routers, use /usr/bin/busybox-legacy/udhcpc
#    The --script can be omitted to use the default, or point to a custom script
/usr/bin/busybox-legacy/udhcpc --interface "$WAN_IF" --script /etc/udhcpc/udhcpc_ip_only --pidfile "/var/run/udhcpc.$WAN_IF.pid" || {
    echo "DHCP failed on $WAN_IF"
    exit 1
}

# Wait for DHCP to complete
sleep 2

# 3) Get the allocated IP from the interface
NEW_IP=$(ip -4 addr show dev "$WAN_IF" | awk '/inet /{print $2}' | cut -d/ -f1 | head -n1)
[ -n "$NEW_IP" ] || { echo "no-ip"; exit 1; }

# Extract CIDR suffix for cleanup
NEW_IP_CIDR=$(ip -4 addr show dev "$WAN_IF" | awk '/inet /{print $2}' | head -n1)

# 4) Remove the IP from the interface - we only need the DHCP lease, not the binding!
#    If the IP stays on the interface, the kernel creates a local route that conflicts with BGP.
#    We want BGP to handle routing to K8s, not have a local route on the WAN interface.
ip addr del "$NEW_IP_CIDR" dev "$WAN_IF" 2>/dev/null || true

# 5) Enable proxy ARP and publish the IP via neighbor proxy (so router answers ARP for NEW_IP)
sysctl -w net.ipv4.conf.$WAN_IF.proxy_arp=1 >/dev/null

# Disable reverse path filtering (required since we unbound the IP from the interface)
# Packets arrive on WAN but route to K8s via LAN - any rp_filter would cause issues
# since the interface has no IP/routes, we must disable rp_filter entirely
sysctl -w net.ipv4.conf.$WAN_IF.rp_filter=0 >/dev/null

# Clear old proxy entries if any
ip neigh show proxy dev "$WAN_IF" | awk '{print $1}' | while read -r ip; do ip neigh del proxy "$ip" dev "$WAN_IF" || true; done
ip neigh add proxy "$NEW_IP" dev "$WAN_IF" || ip neigh replace proxy "$NEW_IP" dev "$WAN_IF"

# 6) NO static route needed - Cilium BGP handles routing!
# Cilium will advertise the IP to the router via BGP, and the router will learn the route dynamically.
# The BGP-learned route will point to the K8s node(s) via the LAN interface, NOT the WAN macvlan.

# IMPORTANT: print ONLY the IP for the operator to parse
printf "%s\n" "$NEW_IP"
```

**Key points about the script:**

1. **DHCP lease without binding**: The script obtains a DHCP lease but removes the IP from the interface to avoid kernel creating a local route that would conflict with BGP
2. **Proxy ARP** allows the router to respond to ARP requests for the public IP on the WAN interface
3. **Cilium BGP** handles all routing - no static routes needed! BGP advertises the IP from Cilium to the router dynamically
4. **Traffic flow**: Internet → Router WAN (proxy ARP) → Router checks BGP routing table → Forwards to K8s via LAN → Cilium delivers to Service
5. **Why unbind the IP**: If the IP stays bound to `$WAN_IF`, the kernel creates a direct route (`203.0.113.45/24 dev wan1`) that takes precedence over BGP routes, breaking the traffic flow to K8s

---

## 1) CRD: `PublicIPClaim`

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: publicipclaims.net.serialx.dev
spec:
  group: net.serialx.dev
  names:
    kind: PublicIPClaim
    plural: publicipclaims
    singular: publicipclaim
    shortNames: [pic, pics]
  scope: Cluster
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                poolName:
                  type: string
                router:
                  type: object
                  properties:
                    host: { type: string }
                    port: { type: integer, default: 22 }
                    user: { type: string }
                    sshSecretRef: { type: string } # secret with private key in key `id_rsa`
                    command:
                      type: string
                      description: "Router script to run (prints only the public IP)"
                      default: "/usr/local/bin/alloc_public_ip.sh"
                    wanParent:
                      type: string
                      description: "Physical WAN interface (e.g., eth9, eth0)"
                    wanInterface:
                      type: string
                      description: "Macvlan interface name (auto-generated from claim name if omitted)"
                      maxLength: 15
                      pattern: '^[a-z0-9-]+$'
                    macAddress:
                      type: string
                      description: "MAC address for DHCP (auto-generated if omitted)"
                      pattern: '^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$'
                  required: [host, user, sshSecretRef, wanParent]
              required: [poolName, router]
            status:
              type: object
              properties:
                phase: { type: string }
                message: { type: string }
                assignedIP: { type: string }
                wanInterface: { type: string }
                macAddress: { type: string }
      subresources:
        status: {}
```

### Example claim

```yaml
apiVersion: net.serialx.dev/v1alpha1
kind: PublicIPClaim
metadata:
  name: ip-wan-001
spec:
  poolName: public-pool
  router:
    host: 192.168.1.1
    user: root
    sshSecretRef: router-ssh
    command: /usr/local/bin/alloc_public_ip.sh
    wanParent: eth9  # Required: physical WAN interface
    # wanInterface and macAddress are optional - auto-generated if omitted
    # wanInterface: wan-custom-001
    # macAddress: "02:aa:bb:cc:dd:ee"
```

---

## 2) RBAC for the operator

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cilium-lbip-operator
rules:
  # our CRD
  - apiGroups: ["net.serialx.dev"]
    resources: ["publicipclaims", "publicipclaims/status"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
  # cilium pools (support both API versions)
  - apiGroups: ["cilium.io"]
    resources: ["ciliumloadbalancerippools"]
    verbs: ["get", "list", "watch", "update", "patch"]
  # services if you later want selectors or annotations
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cilium-lbip-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cilium-lbip-operator
subjects:
  - kind: ServiceAccount
    name: cilium-lbip-operator
    namespace: kube-system
```

> Create secrets:
>
> * `router-ssh` with key `id_rsa` (and optional `known_hosts`).

---

## 3) Controller (Go, Kubebuilder)

### `api/v1alpha1/publicipclaim_types.go`

```go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RouterSpec struct {
    Host         string `json:"host"`
    Port         int    `json:"port,omitempty"`
    User         string `json:"user"`
    SSHSecretRef string `json:"sshSecretRef"`
    Command      string `json:"command,omitempty"`
    WanParent    string `json:"wanParent"`               // Required: physical interface (eth9, eth0, etc.)
    WanInterface string `json:"wanInterface,omitempty"`  // Optional: auto-generated from claim name
    MacAddress   string `json:"macAddress,omitempty"`    // Optional: auto-generated unique MAC
}

type PublicIPClaimSpec struct {
    PoolName   string     `json:"poolName"`
    Router     RouterSpec `json:"router"`
}

type PublicIPClaimStatus struct {
    Phase        string `json:"phase,omitempty"`
    Message      string `json:"message,omitempty"`
    AssignedIP   string `json:"assignedIP,omitempty"`
    WanInterface string `json:"wanInterface,omitempty"`  // Actual interface created
    MacAddress   string `json:"macAddress,omitempty"`    // Actual MAC used
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type PublicIPClaim struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   PublicIPClaimSpec   `json:"spec,omitempty"`
    Status PublicIPClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type PublicIPClaimList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []PublicIPClaim `json:"items"`
}
```

### `controllers/publicipclaim_controller.go`

```go
package controllers

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

    netv1alpha1 "github.com/your/module/api/v1alpha1"
)

// GVRs for Cilium pool (try v2 first, then v2alpha1)
var (
    gvrPoolV2 = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumloadbalancerippools"}
    gvrPoolV2a = schema.GroupVersionResource{Group: "cilium.io", Version: "v2alpha1", Resource: "ciliumloadbalancerippools"}
)

type PublicIPClaimReconciler struct {
    client.Client
    Kube     *kubernetes.Clientset
    Dynamic  dynamic.Interface
    Scheme   *runtime.Scheme
}

func (r *PublicIPClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := ctrllog.FromContext(ctx)

    var claim netv1alpha1.PublicIPClaim
    if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Already done
    if claim.Status.AssignedIP != "" {
        return ctrl.Result{}, nil
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

    // Store in status for tracking
    claim.Status.WanInterface = wanIf
    claim.Status.MacAddress = macAddr
    if err := r.Status().Update(ctx, &claim); err != nil {
        return ctrl.Result{}, err
    }

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

    // 4) Update status
    claim.Status.AssignedIP = ip
    claim.Status.Phase = "Ready"
    claim.Status.Message = "Assigned"
    if err := r.Status().Update(ctx, &claim); err != nil {
        return ctrl.Result{}, err
    }

    log.Info("public IP assigned", "ip", ip, "pool", claim.Spec.PoolName, "wanIf", wanIf, "mac", macAddr)
    return ctrl.Result{}, nil
}

func (r *PublicIPClaimReconciler) runRouterScript(ctx context.Context, claim *netv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
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
    if err != nil { return "", err }

    conf := &ssh.ClientConfig{User: claim.Spec.Router.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second}
    addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

    conn, err := ssh.Dial("tcp", addr, conf)
    if err != nil { return "", err }
    defer conn.Close()

    sess, err := conn.NewSession()
    if err != nil { return "", err }
    defer sess.Close()

    // Set environment variables for the script
    envVars := fmt.Sprintf("WAN_PARENT=%s WAN_IF=%s WAN_MAC=%s",
        claim.Spec.Router.WanParent, wanIf, macAddr)

    cmd := fmt.Sprintf("%s %s", envVars, claim.Spec.Router.Command)
    out, err := sess.CombinedOutput(cmd)
    if err != nil { return "", fmt.Errorf("%v: %s", err, string(out)) }

    ip := strings.TrimSpace(string(out))
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
    if err != nil { return err }

    // Append a new /32 block if it doesn't already exist
    spec, found, _ := unstructured.NestedSlice(pool.Object, "spec", "blocks")
    if !found { spec = []interface{}{} }

    cidr := fmt.Sprintf("%s/32", ip)
    for _, b := range spec {
        m := b.(map[string]interface{})
        if m["cidr"] == cidr { // already present
            return nil
        }
    }

    spec = append(spec, map[string]interface{}{"cidr": cidr})
    if err := unstructured.SetNestedSlice(pool.Object, spec, "spec", "blocks"); err != nil { return err }

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
func (r *PublicIPClaimReconciler) fail(ctx context.Context, claim *netv1alpha1.PublicIPClaim, err error) (ctrl.Result, error) {
    claim.Status.Phase = "Failed"
    claim.Status.Message = err.Error()
    if updateErr := r.Status().Update(ctx, claim); updateErr != nil {
        return ctrl.Result{}, updateErr
    }
    return ctrl.Result{}, err
}
```

### `main.go` (skeleton)

```go
package main

import (
    "flag"
    "os"

    "k8s.io/client-go/dynamic"
    "k8s.io/client-go/kubernetes"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/log/zap"

    netv1alpha1 "github.com/your/module/api/v1alpha1"
    "github.com/your/module/controllers"
)

func main() {
    var metricsAddr string
    flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
    flag.Parse()

    ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

    mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{MetricsBindAddress: metricsAddr})
    if err != nil { panic(err) }

    if err := netv1alpha1.AddToScheme(mgr.GetScheme()); err != nil { panic(err) }

    if err := (&controllers.PublicIPClaimReconciler{
        Client:  mgr.GetClient(),
        Kube:    kubernetes.NewForConfigOrDie(mgr.GetConfig()),
        Dynamic: dynamic.NewForConfigOrDie(mgr.GetConfig()),
        Scheme:  mgr.GetScheme(),
    }).SetupWithManager(mgr); err != nil { panic(err) }

    if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil { panic(err) }
}
```

### Controller wiring

```go
func (r *PublicIPClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&netv1alpha1.PublicIPClaim{}).
        Complete(r)
}
```

---

## 4) Deploy the operator

1. Scaffold with **Kubebuilder** (or just use the above files in your repo).
2. `make install` to install the `PublicIPClaim` CRD.
3. Apply RBAC & create secrets:

```sh
kubectl -n kube-system create secret generic router-ssh \
  --from-file=id_rsa=./id_rsa
```

4. Build & deploy the controller image; create a `Deployment` in `kube-system`.

---

## 5) Create/Update the Cilium pool resource (once)

Use a dedicated pool that this operator will append to:

```yaml
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: public-pool
spec:
  blocks: [] # operator will append /32s here
  serviceSelector:
    matchLabels:
      "io.kubernetes.service.namespace": "ingress"
```

> If your Cilium is older, change `apiVersion: cilium.io/v2alpha1`.

---

## 6) Claim a new IP

```yaml
apiVersion: net.serialx.dev/v1alpha1
kind: PublicIPClaim
metadata:
  name: ip-wan-001
spec:
  poolName: public-pool
  router:
    host: 192.168.1.1
    user: root
    sshSecretRef: router-ssh
    command: /usr/local/bin/alloc_public_ip.sh
    wanParent: eth9
```

Watch:

```sh
kubectl get ippools.cilium.io
kubectl get publicipclaims.net.serialx.dev -w -o wide
```

When ready you should see the pool gain a new block:

```yaml
spec:
  blocks:
  - cidr: 203.0.113.45/32
```

---

## 7) Cleanup with Finalizers

When a `PublicIPClaim` is deleted, you should clean up the router-side macvlan interface. Add a finalizer to the controller:

```go
const finalizerName = "net.serialx.dev/cleanup-wan-interface"

func (r *PublicIPClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := ctrllog.FromContext(ctx)

    var claim netv1alpha1.PublicIPClaim
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

    // ... rest of reconcile logic
}

func (r *PublicIPClaimReconciler) cleanupRouterInterface(ctx context.Context, claim *netv1alpha1.PublicIPClaim) error {
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
    if err != nil { return err }

    conf := &ssh.ClientConfig{User: claim.Spec.Router.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second}
    addr := fmt.Sprintf("%s:%d", claim.Spec.Router.Host, coalesceInt(claim.Spec.Router.Port, 22))

    conn, err := ssh.Dial("tcp", addr, conf)
    if err != nil { return err }
    defer conn.Close()

    sess, err := conn.NewSession()
    if err != nil { return err }
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
```

---

## 8) Notes, pitfalls, and hardening

* **MAC address uniqueness**: Each claim needs a unique MAC. The controller auto-generates locally-administered MACs (02:xx:xx:xx:xx:xx). For production, consider tracking used MACs to avoid collisions.
* **WAN interface naming**: Interface names auto-generated from claim names (sanitized to `wan-{name}`). Linux interface names have a 15-character limit.
* **Multi-WAN support**: Different claims can use different `wanParent` values (eth9, eth10, etc.) for multi-ISP setups.
* **Token/keys**: never bake SSH keys into images. Use K8s Secrets as shown.
* **IP pool updates can reshuffle assignments** if you edit large CIDR blocks (see Cilium note). Appending `/32`s is safe for existing services but still be cautious during mass edits.
* **Service selection**: use `serviceSelector` on the pool to constrain which Services can consume these IPs. Alternatively, annotate your Services with the appropriate LoadBalancerClass, e.g. `io.cilium/bgp-control-plane` when you use Cilium BGP.
* **Cilium BGP required**: This operator assumes Cilium BGP is configured and peering with your router. BGP advertises routes dynamically from Cilium to the router via the LAN interface - no static routes needed.
* **Proxy ARP + BGP**: Traffic flow is: Internet → Router WAN (proxy ARP) → Router BGP routing table → K8s via LAN → Cilium LoadBalancer.
* **Unbind IP from interface**: The router script removes the DHCP-assigned IP from the WAN interface after obtaining the lease. This prevents kernel local routes from conflicting with BGP routes. The DHCP daemon continues to renew the lease in the background.
* **Reverse path filtering**: The script disables `rp_filter` (sets to 0) on the WAN interface. This is required because: (1) the interface is unnumbered (no IP bound), and (2) packets arrive on WAN but are routed to K8s via LAN (asymmetric routing). Any rp_filter check would fail since the interface has no routes.
* **Observability**: expose Prometheus metrics (claims, successes, failures) and add leader election for HA.
* **Idempotency**: the controller treats re-runs as no-ops if the `/32` is already present.
* **Cleanup**: Finalizers ensure router interfaces are deleted when claims are removed. Note: IPs are NOT removed from the Cilium pool on claim deletion - this allows you to recreate claims without losing pool state. If you want automatic pool cleanup, add logic to remove the CIDR from `spec.blocks` in the finalizer.
* **Router reboot**: The macvlan interfaces and udhcpc daemons will NOT survive a router reboot. You have two options: (1) Manually re-run the script after reboot, or (2) Set up a systemd/init script to recreate interfaces from a persistent config file. The operator does not automatically detect and fix this.
* **DHCP lease conflicts**: If you delete and recreate a claim quickly, the ISP might still have the old MAC in its DHCP table. Generate a new MAC or wait for the lease to expire.
* **Testing**: stub `runRouterScript` and `cleanupRouterInterface` behind interfaces so you can unit-test reconciliation.

---

## 9) Future enhancements

* A pool **autoscaler** CR that keeps `N` warm free IPs and replenishes via router script.
* Direct **BGP automation** (e.g., toggling FRR/UDM proxy ARP state) via a small agent instead of SSH.
* Per-`PublicIPClaim` **Service binding**: annotate a Service with the chosen IP using `lbipam.cilium.io/ips: <ip>`, then reconcile.
* **Webhooks**: validate that the router host/secret exist; deny duplicate claims.
* **Router state reconciliation**: Periodically check router state and recreate missing interfaces after router reboots. Could use a DaemonSet or cronjob.
* **Persistent router config**: Generate systemd units or init scripts on the router to recreate interfaces on boot.
* **IP pool cleanup**: Optionally remove IPs from Cilium pool when claims are deleted (currently they remain in the pool).

---

## 10) References

* Cilium LB-IPAM pools, blocks (CIDR/range), selectors, and status: [https://docs.cilium.io/en/stable/network/lb-ipam.html](https://docs.cilium.io/en/stable/network/lb-ipam.html)
* `lbipam.cilium.io/ips` per-service override (advanced): search in Cilium issues/PRs for the annotation semantics.
