# Router Reboot Reconciliation Specification

## Overview

### Problem Statement

When the router reboots, all operator-configured state is lost:
- Macvlan interfaces are deleted
- udhcpc daemon processes are killed
- Proxy ARP configurations are cleared
- Kubernetes PublicIPClaim resources remain in "Ready" state but IPs are non-functional
- LoadBalancer services become unreachable until manual intervention

The operator currently has **no mechanism** to detect or automatically recover from this state.

### Solution

Integrate with the **SSH Connection Management layer** (see `SSH_MANAGEMENT_SPEC.md`) to:
1. **Detect router state changes** via SSH connection events (reboots, drops)
2. **Automatically trigger reconciliation** when events occur
3. **Verify router configuration** matches Kubernetes desired state
4. **Reapply configuration** when drift is detected
5. **Periodic safety checks** (60 min) to catch edge cases

### Key Design Principles

- ✅ **Event-driven reconciliation** - React immediately to SSH connection events
- ✅ **Idempotent operations** - Safe to reconcile multiple times
- ✅ **State verification** - Always verify before assuming success
- ✅ **Dual-trigger approach** - Connection events + periodic checks
- ✅ **Observable** - Metrics, events, and status conditions

---

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Cilium DHCP WAN IP Operator                                 │
│                                                               │
│  ┌────────────────────────────────────────────────────────┐ │
│  │  PublicIPClaim Controller                               │ │
│  │                                                          │ │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐             │ │
│  │  │ Claim 1  │  │ Claim 2  │  │ Claim 3  │             │ │
│  │  │(wan0.d1) │  │(wan0.d2) │  │(wan1.d1) │             │ │
│  │  └────┬─────┘  └────┬─────┘  └────┬─────┘             │ │
│  │       │             │             │                     │ │
│  │       │  Register event handlers  │                     │ │
│  │       └─────────────┴─────────────┘                     │ │
│  │                     │                                    │ │
│  └─────────────────────┼────────────────────────────────────┘ │
│                        │                                      │
│                        ▼                                      │
│  ┌─────────────────────────────────────────────────────────┐ │
│  │  SSH Manager Registry (from SSH_MANAGEMENT_SPEC.md)     │ │
│  │  - Connection pooling per router                        │ │
│  │  - Reboot detection via uptime tracking                 │ │
│  │  - Automatic reconnection                               │ │
│  │  - Event notification system                            │ │
│  └─────────────────────────────────────────────────────────┘ │
│                        │                                      │
└────────────────────────┼──────────────────────────────────────┘
                         │ SSH (port 22)
                         ▼
                  ┌──────────────────┐
                  │   Router/Gateway  │
                  │   192.168.1.1     │
                  │                   │
                  │   - SSH Server    │
                  │   - macvlan ifaces│
                  │   - udhcpc daemons│
                  └───────────────────┘
```

### Reconciliation Triggers

```
┌─────────────────────────────────────────────────────────────┐
│                    Reconciliation Triggers                   │
└─────────────────────────────────────────────────────────────┘

1. SSH Connection Events (via SSH Manager)
   ├─> Router reboot detected (uptime decreased)
   ├─> Connection drop (network partition)
   └─> Handlers notify controller → Enqueue reconciliation

2. Periodic Reconciliation (Controller-driven)
   └─> Every 60 minutes → Verify state → Reapply if needed

3. User-Initiated Changes
   ├─> Claim created/updated
   └─> Kubernetes watch events → Standard reconciliation

┌─────────────────────────────────────────────────────────────┐
│              Work Queue (Rate Limited)                       │
│  - Prevents event storms during reconnection                │
│  - Rate limiting for failed reconciliations                 │
│  - Prevents duplicate work                                  │
└─────────────────────────────────────────────────────────────┘
```

---

## Controller Integration

### Controller Structure

```go
type PublicIPClaimReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder

    // SSH manager registry (singleton)
    sshRegistry *ssh.SSHManagerRegistry

    // Work queue for triggering reconciliation events
    // This replaces a buffered channel to prevent event loss
    reconcileQueue workqueue.RateLimitingInterface

    // Track router assignments so we can clean up handlers on address changes
    routerAssignments   map[string]string  // Key: "namespace/name", Value: router address
    routerAssignmentsMu sync.RWMutex
}
```

### Setup and Lifecycle

```go
// SetupWithManager sets up the controller
func (r *PublicIPClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // Initialize SSH registry
    r.sshRegistry = ssh.NewRegistry()

    // Initialize work queue with rate limiting for async events
    r.reconcileQueue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
    r.routerAssignments = make(map[string]string)

    // Close all SSH connections and work queue on shutdown
    mgr.Add(&sshCleanupRunnable{
        registry: r.sshRegistry,
        queue:    r.reconcileQueue,
    })

    // Start work queue processor
    go r.processWorkQueue(mgr.GetClient())

    return ctrl.NewControllerManagedBy(mgr).
        For(&v1.PublicIPClaim{}).
        // Trigger periodic reconciliation
        WithOptions(controller.Options{
            MaxConcurrentReconciles: 3,
        }).
        Complete(r)
}

// sshCleanupRunnable ensures SSH connections and work queue are closed on shutdown
type sshCleanupRunnable struct {
    registry *ssh.SSHManagerRegistry
    queue    workqueue.RateLimitingInterface
}

func (r *sshCleanupRunnable) Start(ctx context.Context) error {
    <-ctx.Done()
    r.queue.ShutDown()
    return r.registry.CloseAll()
}
```

### Work Queue Processing

```go
// processWorkQueue processes items from the work queue
func (r *PublicIPClaimReconciler) processWorkQueue(client client.Client) {
    for {
        item, shutdown := r.reconcileQueue.Get()
        if shutdown {
            return
        }

        func() {
            defer r.reconcileQueue.Done(item)

            req, ok := item.(reconcile.Request)
            if !ok {
                r.reconcileQueue.Forget(item)
                log.Log.Error(nil, "Invalid item type in work queue", "item", item)
                return
            }

            // Trigger reconciliation by re-enqueuing to controller
            ctx := context.Background()
            claim := &v1.PublicIPClaim{}
            if err := client.Get(ctx, req.NamespacedName, claim); err != nil {
                if !errors.IsNotFound(err) {
                    log.Log.Error(err, "Failed to get claim from work queue", "request", req)
                    r.reconcileQueue.AddRateLimited(item)
                    return
                }
                // Claim not found, forget it
                r.reconcileQueue.Forget(item)
                return
            }

            // Update claim to trigger reconciliation (touch annotation)
            if claim.Annotations == nil {
                claim.Annotations = make(map[string]string)
            }
            claim.Annotations["serialx.net/last-reconnect"] = time.Now().Format(time.RFC3339)
            if err := client.Update(ctx, claim); err != nil {
                log.Log.Error(err, "Failed to trigger reconciliation", "claim", req)
                r.reconcileQueue.AddRateLimited(item)
                return
            }

            r.reconcileQueue.Forget(item)
        }()
    }
}
```

### SSH Manager Retrieval

```go
// validateSSHCredentials ensures all claims for a router use the same SSH credentials
func (r *PublicIPClaimReconciler) validateSSHCredentials(ctx context.Context, routerAddr string, secretRef corev1.SecretReference) error {
    claims := &v1.PublicIPClaimList{}
    if err := r.List(ctx, claims); err != nil {
        return fmt.Errorf("failed to list claims: %w", err)
    }

    for _, claim := range claims.Items {
        if claim.Spec.RouterAddress == routerAddr {
            // Check if secret reference matches
            existingNs := claim.Spec.SSHSecretRef.Namespace
            if existingNs == "" {
                existingNs = "kube-system"
            }
            newNs := secretRef.Namespace
            if newNs == "" {
                newNs = "kube-system"
            }

            if claim.Spec.SSHSecretRef.Name != secretRef.Name || existingNs != newNs {
                return fmt.Errorf("SSH credential mismatch: claim %s/%s uses secret %s/%s but this claim uses %s/%s",
                    claim.Namespace, claim.Name,
                    existingNs, claim.Spec.SSHSecretRef.Name,
                    newNs, secretRef.Name)
            }
        }
    }
    return nil
}

// getSSHManager returns or creates an SSH manager for a claim's router
func (r *PublicIPClaimReconciler) getSSHManager(ctx context.Context, claim *v1.PublicIPClaim) (*ssh.SSHConnectionManager, error) {
    logger := log.FromContext(ctx)

    claimKey := fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)

    // Get router connection details from claim spec
    routerAddr := claim.Spec.RouterAddress
    if routerAddr == "" {
        return nil, fmt.Errorf("router address not specified")
    }

    // Validate SSH credentials are consistent across claims for this router
    if err := r.validateSSHCredentials(ctx, routerAddr, claim.Spec.SSHSecretRef); err != nil {
        return nil, err
    }

    // Get SSH credentials from secret
    secret := &corev1.Secret{}
    secretName := claim.Spec.SSHSecretRef.Name
    secretNamespace := claim.Spec.SSHSecretRef.Namespace
    if secretNamespace == "" {
        secretNamespace = "kube-system"
    }

    if err := r.Get(ctx, types.NamespacedName{
        Name:      secretName,
        Namespace: secretNamespace,
    }, secret); err != nil {
        return nil, fmt.Errorf("failed to get SSH secret: %w", err)
    }

    privateKey, ok := secret.Data["id_rsa"]
    if !ok {
        return nil, fmt.Errorf("SSH secret missing 'id_rsa' key")
    }

    // Parse private key
    signer, err := sshlib.ParsePrivateKey(privateKey)
    if err != nil {
        return nil, fmt.Errorf("failed to parse private key: %w", err)
    }

    desiredAddress := fmt.Sprintf("%s:22", routerAddr)

    // If the claim previously pointed at a different router, unregister the old handler
    r.routerAssignmentsMu.RLock()
    previousRouter := r.routerAssignments[claimKey]
    r.routerAssignmentsMu.RUnlock()

    if previousRouter != "" && previousRouter != desiredAddress {
        if prevMgr := r.sshRegistry.LookupManager(previousRouter); prevMgr != nil {
            prevMgr.UnregisterHandler(claimKey)
            if prevMgr.HandlerCount() == 0 {
                if err := prevMgr.Close(); err != nil {
                    logger.Error(err, "Failed to close SSH manager after router change", "router", previousRouter)
                }
                r.sshRegistry.RemoveManager(previousRouter)
            }
        }
    }

    // Load host key verification if available
    // TODO: Support loading from ConfigMap/Secret
    var hostKeyCallback ssh.HostKeyCallback
    knownHostsPath := "/etc/ssh-operator/known_hosts"
    if _, err := os.Stat(knownHostsPath); err == nil {
        hostKeyCallback, err = knownhosts.New(knownHostsPath)
        if err != nil {
            logger.Error(err, "Failed to load known_hosts, falling back to insecure verification")
            hostKeyCallback = nil  // Will trigger InsecureIgnoreHostKey fallback
        } else {
            logger.Info("Using host key verification from known_hosts")
        }
    }

    // Build router config
    config := ssh.RouterConfig{
        Address:         desiredAddress,
        Username:        "root",  // TODO: Make configurable
        AuthMethod:      sshlib.PublicKeys(signer),
        HostKeyCallback: hostKeyCallback,
        Timeout:         30 * time.Second,
    }

    // Get or create manager (pass context for proper lifecycle)
    mgr := r.sshRegistry.GetManager(ctx, config)

    // Update claim status with current connection state
    if mgr.IsConnected() {
        claim.Status.ConnectionState = "connected"
    } else if mgr.IsReconnecting() {
        claim.Status.ConnectionState = "reconnecting"
    } else {
        claim.Status.ConnectionState = "disconnected"
    }

    // Register event handler for this claim (idempotent - overwrites if exists)
    // Capture claimKey and namespace/name for the closure
    namespace := claim.Namespace
    name := claim.Name
    mgr.RegisterHandler(claimKey, func() {
        logger.Info("SSH connection event, triggering reconciliation",
            "router", routerAddr,
            "claim", claimKey)

        // Enqueue reconciliation request to work queue
        // Work queue provides rate limiting and prevents event loss
        r.reconcileQueue.Add(reconcile.Request{
            NamespacedName: types.NamespacedName{
                Namespace: namespace,
                Name:      name,
            },
        })
    })

    // Record the router assignment for future comparisons
    r.routerAssignmentsMu.Lock()
    r.routerAssignments[claimKey] = config.Address
    r.routerAssignmentsMu.Unlock()

    return mgr, nil
}
```

---

## Reconciliation Flow

### Main Reconcile Method

```go
func (r *PublicIPClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Fetch the PublicIPClaim
    claim := &v1.PublicIPClaim{}
    if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Handle deletion
    if !claim.DeletionTimestamp.IsZero() {
        return r.reconcileDelete(ctx, claim)
    }

    // Get SSH manager (or create if doesn't exist)
    sshMgr, err := r.getSSHManager(ctx, claim)
    if err != nil {
        logger.Error(err, "Failed to get SSH manager")
        r.Recorder.Event(claim, "Warning", "SSHError", err.Error())
        return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
    }

    // Verify current state
    if err := r.verifyClaimState(ctx, claim, sshMgr); err != nil {
        logger.Error(err, "State verification failed, reapplying configuration")
        if err := r.applyConfiguration(ctx, claim, sshMgr); err != nil {
            logger.Error(err, "Failed to apply configuration")
            return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
        }
    }

    // Update status
    if err := r.Status().Update(ctx, claim); err != nil {
        logger.Error(err, "Failed to update status")
        return ctrl.Result{}, err
    }

    // Requeue for periodic verification (safety net)
    return ctrl.Result{RequeueAfter: 60 * time.Minute}, nil
}
```

### Reconciliation Decision Tree

```
[Reconcile Triggered]
       │
       ├─> Trigger: Connection event
       │   └─> Reason: "reboot" or "connection_drop"
       │
       ├─> Trigger: Periodic (60 min)
       │   └─> Reason: "periodic_check"
       │
       └─> Trigger: User update
           └─> Reason: "spec_change"

       ↓

[Get SSH Manager]
       │
       ├─> Manager exists ──> Use existing
       └─> Manager missing ──> Create new
                               Register event handler

       ↓

[Verify Current State]
       │
       ├─> Get router uptime
       │   └─> Uptime < previous? ──> REBOOT DETECTED
       │
       ├─> Check interface exists
       │   └─> Missing? ──> DRIFT DETECTED
       │
       ├─> Check udhcpc running
       │   └─> Not running? ──> DRIFT DETECTED
       │
       └─> Check proxy ARP enabled
           └─> Not enabled? ──> DRIFT DETECTED

       ↓

[State OK?]
   │
   ├─> YES ──> Update status, requeue 60 min
   │
   └─> NO ──> [Apply Configuration]
               │
               ├─> Execute router script
               ├─> Verify application
               └─> Update status

               ↓

       [Configuration Applied]
               │
               └─> Requeue 60 min (periodic check)
```

### State Verification

```go
func (r *PublicIPClaimReconciler) verifyClaimState(ctx context.Context, claim *v1.PublicIPClaim, sshMgr *ssh.SSHConnectionManager) error {
    log := log.FromContext(ctx)

    // 1. Check router uptime (detect reboots)
    currentUptime, err := sshMgr.GetRouterUptime()
    if err != nil {
        return fmt.Errorf("failed to get router uptime: %w", err)
    }

    if claim.Status.RouterUptime > 0 && currentUptime < float64(claim.Status.RouterUptime) {
        log.Info("Router reboot detected via uptime comparison",
            "previousUptime", claim.Status.RouterUptime,
            "currentUptime", currentUptime)
        r.Recorder.Event(claim, "Warning", "RouterRebooted", "Router reboot detected, reapplying configuration")
        claim.Status.LastReconciliationReason = "router_reboot"
        return fmt.Errorf("router rebooted")
    }

    // Update uptime
    claim.Status.RouterUptime = int64(currentUptime)

    // 2. Verify interface exists
    ifname := claim.Status.InterfaceName
    if ifname == "" {
        // Interface not yet created, need full reconciliation
        return fmt.Errorf("interface not yet created")
    }

    exists, err := sshMgr.InterfaceExists(ifname)
    if err != nil {
        return fmt.Errorf("failed to check interface existence: %w", err)
    }

    if !exists {
        log.Info("Interface does not exist", "interface", ifname)
        r.Recorder.Event(claim, "Warning", "ConfigurationDrift", "Interface missing, reapplying configuration")
        claim.Status.LastReconciliationReason = "interface_missing"
        return fmt.Errorf("interface missing")
    }

    // 3. Verify udhcpc is running
    running, err := sshMgr.IsUdhcpcRunning(ifname)
    if err != nil {
        return fmt.Errorf("failed to check udhcpc status: %w", err)
    }

    if !running {
        log.Info("DHCP client not running", "interface", ifname)
        r.Recorder.Event(claim, "Warning", "DHCPClientStopped", "DHCP client not running, reapplying configuration")
        claim.Status.LastReconciliationReason = "dhcp_client_stopped"
        return fmt.Errorf("dhcp client not running")
    }

    // 4. Verify proxy ARP is enabled
    proxyARPEnabled, err := sshMgr.IsProxyARPEnabled(ifname)
    if err != nil {
        return fmt.Errorf("failed to check proxy ARP: %w", err)
    }

    if !proxyARPEnabled {
        log.Info("Proxy ARP not enabled", "interface", ifname)
        r.Recorder.Event(claim, "Warning", "ProxyARPDisabled", "Proxy ARP disabled, reapplying configuration")
        claim.Status.LastReconciliationReason = "proxy_arp_disabled"
        return fmt.Errorf("proxy arp disabled")
    }

    // 5. All checks passed
    now := metav1.Now()
    claim.Status.LastVerified = &now
    claim.Status.ConfigurationVerified = true
    claim.Status.LastReconciliationReason = "verified"

    // Update condition
    meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
        Type:               "Ready",
        Status:             metav1.ConditionTrue,
        Reason:             "ConfigurationVerified",
        Message:            "Interface, DHCP client, and proxy ARP verified",
        ObservedGeneration: claim.Generation,
    })

    log.V(1).Info("State verification passed", "interface", ifname)
    return nil
}
```

### Configuration Application

```go
func (r *PublicIPClaimReconciler) applyConfiguration(ctx context.Context, claim *v1.PublicIPClaim, sshMgr *ssh.SSHConnectionManager) error {
    log := log.FromContext(ctx)

    // Build script command
    scriptPath := "/usr/local/bin/dhcp-wanip.sh"  // TODO: Make configurable

    ifname := claim.Spec.InterfaceName
    if ifname == "" {
        // Generate interface name from claim name
        ifname = generateInterfaceName(claim.Name)
    }

    cmd := fmt.Sprintf(
        "%s create --interface %s --mac %s --parent %s --router-ip %s",
        scriptPath,
        ifname,
        claim.Spec.MACAddress,
        claim.Spec.ParentInterface,
        claim.Spec.RouterIP,
    )

    log.Info("Applying configuration", "command", cmd)

    output, err := sshMgr.Execute(cmd)
    if err != nil {
        log.Error(err, "Failed to execute router script", "output", output)
        r.Recorder.Event(claim, "Warning", "ConfigurationFailed", fmt.Sprintf("Failed to apply configuration: %v", err))

        // Update condition
        meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
            Type:               "Ready",
            Status:             metav1.ConditionFalse,
            Reason:             "ConfigurationFailed",
            Message:            fmt.Sprintf("Failed to apply configuration: %v", err),
            ObservedGeneration: claim.Generation,
        })

        return err
    }

    log.Info("Configuration applied successfully", "output", output)
    r.Recorder.Event(claim, "Normal", "ConfigurationApplied", "Configuration applied successfully")

    // Update status
    claim.Status.InterfaceName = ifname
    claim.Status.ConfigurationVerified = true
    claim.Status.LastReconciliationReason = "configuration_applied"
    now := metav1.Now()
    claim.Status.LastVerified = &now

    // Update condition
    meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
        Type:               "Ready",
        Status:             metav1.ConditionTrue,
        Reason:             "ConfigurationApplied",
        Message:            "Configuration applied successfully",
        ObservedGeneration: claim.Generation,
    })

    return nil
}
```

### Claim Deletion

```go
func (r *PublicIPClaimReconciler) reconcileDelete(ctx context.Context, claim *v1.PublicIPClaim) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    claimKey := fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)

    // Unregister handler from SSH manager and cleanup if no more claims
    if claim.Spec.RouterAddress != "" {
        r.routerAssignmentsMu.Lock()
        delete(r.routerAssignments, claimKey)
        r.routerAssignmentsMu.Unlock()

        if mgr := r.sshRegistry.LookupManager(claim.Spec.RouterAddress); mgr != nil {
            mgr.UnregisterHandler(claimKey)
            log.Info("Unregistered claim handler", "claimKey", claimKey)

            // Close and remove manager if no more claims are using it
            if mgr.HandlerCount() == 0 {
                log.Info("No more claims for router, closing SSH manager",
                    "router", claim.Spec.RouterAddress)
                if err := mgr.Close(); err != nil {
                    log.Error(err, "Failed to close SSH manager", "router", claim.Spec.RouterAddress)
                    // Continue with deletion even if close fails
                }
                r.sshRegistry.RemoveManager(claim.Spec.RouterAddress)
            }
        }
    }

    // Existing cleanup: execute router script to remove interface, kill udhcpc, etc.
    // ... (existing deletion logic here)

    // Remove finalizer and allow deletion
    controllerutil.RemoveFinalizer(claim, "serialx.net/publicipclaim-finalizer")
    if err := r.Update(ctx, claim); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

---

## CRD Status Extensions

### New Status Fields

Add to `api/v1/publicipclaim_types.go`:

```go
type PublicIPClaimStatus struct {
    // Existing fields
    Phase          string   `json:"phase,omitempty"`
    AllocatedIP    string   `json:"allocatedIP,omitempty"`
    InterfaceName  string   `json:"interfaceName,omitempty"`
    MACAddress     string   `json:"macAddress,omitempty"`

    // NEW: Reconciliation tracking
    RouterUptime              int64        `json:"routerUptime,omitempty"`
    LastVerified              *metav1.Time `json:"lastVerified,omitempty"`
    ConfigurationVerified     bool         `json:"configurationVerified"`
    LastReconciliationReason  string       `json:"lastReconciliationReason,omitempty"`

    // NEW: Connection state
    ConnectionState           string       `json:"connectionState,omitempty"`

    // Kubernetes Conditions
    Conditions                []metav1.Condition `json:"conditions,omitempty"`
}
```

### Status Field Descriptions

| Field | Type | Description |
|-------|------|-------------|
| `routerUptime` | int64 | Router uptime in seconds at last check (used to detect reboots) |
| `lastVerified` | *metav1.Time | Timestamp when configuration was last verified |
| `configurationVerified` | bool | Whether current configuration has been verified on router |
| `lastReconciliationReason` | string | Reason for last reconciliation: `router_reboot`, `interface_missing`, `dhcp_client_stopped`, `periodic`, `verified` |
| `connectionState` | string | SSH connection state: `connected`, `reconnecting`, `disconnected` |
| `conditions` | []metav1.Condition | Standard Kubernetes conditions |

### Kubernetes Conditions

Standard condition types:

```go
const (
    // ConditionReady indicates the claim is ready and configuration is verified
    ConditionReady = "Ready"

    // ConditionConfigurationDrifted indicates router state doesn't match desired state
    ConditionConfigurationDrifted = "ConfigurationDrifted"

    // ConditionRecovering indicates operator is reapplying configuration
    ConditionRecovering = "Recovering"
)
```

**Example condition usage:**

```go
// Configuration verified
meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
    Type:               ConditionReady,
    Status:             metav1.ConditionTrue,
    Reason:             "ConfigurationVerified",
    Message:            "Interface and DHCP client verified",
    ObservedGeneration: claim.Generation,
})

// Router rebooted, recovering
meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
    Type:               ConditionReady,
    Status:             metav1.ConditionFalse,
    Reason:             "RouterRebooted",
    Message:            "Router reboot detected, reapplying configuration",
    ObservedGeneration: claim.Generation,
})

meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
    Type:               ConditionRecovering,
    Status:             metav1.ConditionTrue,
    Reason:             "ReapplyingConfiguration",
    Message:            "Reapplying configuration after router reboot",
    ObservedGeneration: claim.Generation,
})
```

### Status Display

Users can check status via kubectl:

```bash
# Check claim status
kubectl get publicipclaims
kubectl describe publicipclaim my-claim

# Check conditions
kubectl get publicipclaim my-claim -o jsonpath='{.status.conditions}'

# Check last verification time
kubectl get publicipclaim my-claim -o jsonpath='{.status.lastVerified}'

# Check reconciliation reason
kubectl get publicipclaim my-claim -o jsonpath='{.status.lastReconciliationReason}'
```

Example output:
```yaml
status:
  allocatedIP: 1.2.3.4
  interfaceName: wan0.dhcp1
  macAddress: 02:xx:xx:xx:xx:xx
  phase: Ready
  routerUptime: 123456
  lastVerified: "2025-10-09T12:34:56Z"
  configurationVerified: true
  lastReconciliationReason: verified
  connectionState: connected
  conditions:
  - type: Ready
    status: "True"
    reason: ConfigurationVerified
    message: Interface and DHCP client verified
    lastTransitionTime: "2025-10-09T12:34:56Z"
```

---

## Reconciliation Strategy

### Dual-Trigger Approach

**Primary Trigger: SSH Connection Events**
- SSH Manager detects router reboots via uptime comparison
- SSH Manager detects connection drops when keep-alive fails
- Event handlers trigger immediate reconciliation for affected claims
- **Recovery time: ~40 seconds worst-case** (30s ticker + 10s timeout)

**Safety Net: Periodic Verification**
- Every claim reconciles every 60 minutes
- Catches configuration drift that didn't trigger a connection event
- Verifies interface existence, DHCP client status, proxy ARP
- Handles edge cases (SSH stayed connected but config changed)
- **Recovery time: ≤60 minutes**

### Idempotent Reconciliation

All reconciliation operations must be **idempotent** (safe to run multiple times):

1. **Interface creation**: Script checks if interface exists before creating
2. **DHCP client**: Kill existing process before starting new one
3. **Proxy ARP**: Safe to set multiple times
4. **IP unbinding**: Safe to unbind if not bound

This ensures:
- ✅ Safe to reconcile on false positives (transient network issues)
- ✅ Safe to reconcile multiple claims concurrently
- ✅ Safe to reconcile after operator restart

---

## Observability & Monitoring

### Metrics

Expose Prometheus metrics:

```go
var (
    sshConnectionsTotal = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "dhcp_wanip_ssh_connections_total",
            Help: "Number of active SSH connections to routers",
        },
        []string{"router"},
    )

    reconciliationsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "dhcp_wanip_reconciliations_total",
            Help: "Total number of reconciliations",
        },
        []string{"trigger", "result"},
    )

    configurationDriftDetectedTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "dhcp_wanip_configuration_drift_detected_total",
            Help: "Total number of configuration drifts detected",
        },
        []string{"router", "reason"},
    )

    routerRebootsDetectedTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "dhcp_wanip_router_reboots_detected_total",
            Help: "Total number of router reboots detected",
        },
        []string{"router"},
    )
)
```

### Events

Emit Kubernetes events for key actions:

```go
// Connection events
r.Recorder.Event(claim, "Normal", "SSHConnected", "SSH connection established")
r.Recorder.Event(claim, "Warning", "SSHDisconnected", "SSH connection dropped")

// Reboot events
r.Recorder.Event(claim, "Warning", "RouterRebooted", "Router reboot detected")

// Configuration events
r.Recorder.Event(claim, "Normal", "ConfigurationApplied", "Configuration applied successfully")
r.Recorder.Event(claim, "Warning", "ConfigurationDrift", "Interface missing, reapplying configuration")
r.Recorder.Event(claim, "Warning", "DHCPClientStopped", "DHCP client not running")

// Reconciliation events
r.Recorder.Event(claim, "Normal", "ReconciliationSuccess", "Reconciliation completed successfully")
r.Recorder.Event(claim, "Warning", "ReconciliationFailed", fmt.Sprintf("Reconciliation failed: %v", err))
```

### Logging

Use structured logging with appropriate levels:

```go
// Info level: Normal operations
log.Info("Configuration verified", "claim", claim.Name)

// Warning level: Recoverable issues
log.Warn("Router reboot detected", "router", routerAddr, "uptime", uptime)

// Error level: Non-recoverable errors
log.Error(err, "Failed to apply configuration", "claim", claim.Name)

// Debug level: Detailed diagnostics
log.V(1).Info("State verification passed", "interface", ifname)
log.V(2).Info("Executing command", "cmd", cmd)
```

---

## Error Handling & Edge Cases

### Operator Restarts

**Scenario:** Operator pod restarts or is rescheduled

**Handling:**
1. All SSH connections are lost (old managers destroyed)
2. On first reconciliation for each claim:
   - New SSH manager created
   - Connection established
   - Event handler registered
   - State verification runs
   - Configuration reapplied if needed
3. `LastVerified` timestamp shows when state was last confirmed
4. Old connections cleaned up by OS

**Recovery time:** First reconciliation after operator restart (~1 minute)

### Concurrent Reconciliation

**Scenario:** Multiple claims for same router reconcile simultaneously

**Handling:**
- Single SSH connection manager per router (singleton)
- Thread-safe command execution via SSH Manager
- Commands serialized automatically
- No race conditions between claims
- Work queue provides rate limiting

### Router Firmware Updates

**Scenario:** Router firmware update clears configuration

**Handling:**
- Same as router reboot
- Uptime resets to 0
- SSH Manager detects reboot, triggers handlers
- Controller reapplies configuration
- Recovery within ~40 seconds

### Script Execution Failures

**Scenario:** Router script fails (syntax error, permission denied)

**Handling:**
```go
output, err := sshMgr.Execute(scriptCmd)
if err != nil {
    log.Error(err, "Script execution failed", "output", output)
    r.Recorder.Event(claim, "Warning", "ScriptFailed", output)

    // Update condition
    meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
        Type:    ConditionReady,
        Status:  metav1.ConditionFalse,
        Reason:  "ScriptExecutionFailed",
        Message: fmt.Sprintf("Failed to execute script: %v", err),
    })

    // Retry with exponential backoff
    return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
}
```

### False Positive Drift Detection

**Scenario:** Transient error causes verification to fail

**Handling:**
- Configuration reapplied (idempotent, safe)
- Event emitted for observability
- If error persists, exponential backoff via work queue rate limiter
- Metrics track false positive rate

---

## Implementation Phases

### Phase 1: SSH Infrastructure (Week 1)

See `SSH_MANAGEMENT_SPEC.md` for complete details.

**Deliverables:**
- ✅ `internal/ssh` package with full implementation
- ✅ Unit tests with >80% coverage
- ✅ Documentation for SSH manager API

### Phase 2: Controller Integration (Week 2)

**Goal:** Integrate SSH manager into controller

**Tasks:**
1. Extend PublicIPClaim CRD status
   - Add new fields: `RouterUptime`, `LastVerified`, etc.
   - Run `make manifests generate`
2. Update controller
   - Initialize SSH registry in `SetupWithManager`
   - Implement `getSSHManager()`
   - Add event handlers via SSH Manager
   - Create work queue processor
3. Implement state verification
   - `verifyClaimState()`
   - Reboot detection via uptime
   - Interface existence checks
   - DHCP client checks
   - Proxy ARP checks
4. Implement configuration application
   - `applyConfiguration()`
   - Execute router script via SSH
5. Implement deletion logic
   - Unregister handlers
   - Close SSH managers when no handlers remain
6. Add cleanup on shutdown
   - Close all SSH connections gracefully
   - Shut down work queue

**Deliverables:**
- ✅ Updated CRD with new status fields
- ✅ Controller with SSH integration
- ✅ State verification logic
- ✅ E2E tests for reconciliation

### Phase 3: Observability & Monitoring (Week 3)

**Goal:** Add metrics, events, and logging

**Tasks:**
1. Implement Prometheus metrics
   - Connection metrics (delegate to SSH Manager)
   - Reconciliation metrics
   - Drift detection metrics
   - Reboot detection metrics
2. Add Kubernetes events
   - Connection events
   - Reboot events
   - Configuration events
3. Enhance logging
   - Structured logging throughout
   - Appropriate log levels
4. Create monitoring dashboard
   - Grafana dashboard for metrics
   - Sample alerts for:
     - High reconciliation failure rate
     - Router reboots detected
     - SSH connection drops
5. Documentation
   - Update README with reconciliation behavior
   - Add troubleshooting guide
   - Add monitoring guide

**Deliverables:**
- ✅ Metrics exposed on `/metrics`
- ✅ Events visible in `kubectl describe`
- ✅ Grafana dashboard template
- ✅ Comprehensive documentation

### Phase 4: Testing & Hardening (Week 4)

**Goal:** Comprehensive testing and edge case handling

**Tasks:**
1. Integration tests
   - Router reboot scenarios
   - Network partition scenarios
   - Operator restart scenarios
   - Multiple claims per router
2. Chaos testing
   - Kill SSH connections randomly
   - Simulate router delays
   - Simulate configuration drift
3. Performance testing
   - Multiple routers (10+)
   - Multiple claims per router (20+)
   - Measure reconciliation times
   - Measure memory usage
4. Security review
   - SSH key handling
   - Credential validation
   - Host key verification
5. Documentation review
   - Complete all TODOs
   - User guide for troubleshooting
   - Operator guide for monitoring
   - Runbook for common issues

**Deliverables:**
- ✅ Integration test suite
- ✅ Performance benchmarks
- ✅ Security audit passed
- ✅ Production-ready documentation

---

## Testing Strategy

### Integration Tests

```go
// Test full reconciliation flow
func TestReconcile_RouterReboot(t *testing.T) {
    // 1. Create claim
    // 2. Verify configuration applied
    // 3. Simulate router reboot (reset uptime via SSH Manager)
    // 4. Wait for event handler to trigger
    // 5. Wait for reconciliation
    // 6. Verify configuration reapplied
}

func TestReconcile_InterfaceDeleted(t *testing.T) {
    // 1. Create claim
    // 2. Delete interface on router
    // 3. Wait for periodic reconciliation (or trigger manually)
    // 4. Verify interface recreated
}

func TestReconcile_OperatorRestart(t *testing.T) {
    // 1. Create claim
    // 2. Simulate operator restart (recreate controller)
    // 3. Wait for reconciliation
    // 4. Verify SSH manager recreated
    // 5. Verify handler re-registered
    // 6. Verify state restored
}

func TestReconcile_MultipleClaimsSameRouter(t *testing.T) {
    // 1. Create 3 claims for same router
    // 2. Verify single SSH manager created
    // 3. Verify 3 handlers registered
    // 4. Simulate reboot
    // 5. Verify all 3 claims reconciled
}
```

### E2E Tests

```bash
# Run full e2e test suite
make test-e2e

# Scenarios:
# - Router reboot during active claim
# - Network partition between operator and router
# - Multiple claims on same router
# - Claim deletion during reconnection
# - Operator restart with active claims
# - SSH credential changes
```

### Manual Testing Checklist

- [ ] Create claim, verify SSH connection established
- [ ] Reboot router, verify automatic recovery within ~40s
- [ ] Delete interface manually, verify periodic reconciliation recreates it
- [ ] Stop udhcpc manually, verify reconciliation restarts it
- [ ] Restart operator pod, verify claims recover
- [ ] Create multiple claims on same router, verify they share SSH connection
- [ ] Delete one claim, verify SSH connection stays alive (other claims exist)
- [ ] Delete all claims for router, verify SSH connection closes
- [ ] Monitor metrics during router reboot
- [ ] Check events in `kubectl describe publicipclaim`
- [ ] Verify status fields update correctly

---

## Conclusion

This specification defines a **robust, production-ready reconciliation strategy** for handling router state changes:

✅ **Fast detection** - ~40 seconds worst-case via SSH connection events
✅ **Automatic recovery** - No manual intervention
✅ **Event-driven** - React immediately to SSH events
✅ **Observable** - Metrics, events, conditions
✅ **Resilient** - Handles operator restarts, concurrent reconciliation
✅ **Idempotent** - Safe to reconcile multiple times

**Integration with SSH Layer:**
- Leverages `SSH_MANAGEMENT_SPEC.md` for connection management
- Event handlers provide clean integration point
- Work queue handles rate limiting and prevents event loss

**Next Steps:**
1. Review this specification with team
2. Complete Phase 1 (SSH Infrastructure) first
3. Begin Phase 2 (Controller Integration)
4. Iterate based on testing feedback

**Questions or concerns?** Reach out before starting implementation!
