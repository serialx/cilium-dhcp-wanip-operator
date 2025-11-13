# Agent Integration Guide

This guide shows how to integrate the DHCP WAN Agent client into the operator controller.

## Overview

The agent client (`internal/router/agent_client.go`) provides a Go API to communicate with the agent running on the router via SSH tunnel. This replaces the existing SSH script execution approach.

## Integration Steps

### 1. Import the Router Package

```go
import (
    "serialx.net/cilium-dhcp-wanip-operator/internal/router"
)
```

### 2. Update the Reconciler to Use Agent Client

Replace the existing `runRouterScript` method with agent client calls:

#### Before (SSH Script Approach):
```go
func (r *PublicIPClaimReconciler) runRouterScript(ctx context.Context, claim *PublicIPClaim, wanIf, macAddr string) (string, error) {
    // SSH connection
    // Execute shell script
    // Parse output
    return ip, nil
}
```

#### After (Agent Client Approach):
```go
func (r *PublicIPClaimReconciler) allocateIP(ctx context.Context, claim *serialxnetv1.PublicIPClaim) (string, error) {
    // Get SSH config (reuse existing SSH manager)
    sshConfig, err := r.SSHManager.GetSSHConfig(ctx)
    if err != nil {
        return "", fmt.Errorf("failed to get SSH config: %w", err)
    }

    // Create agent client with SSH tunnel
    agentClient, err := router.NewAgentClient(ctx, claim.Spec.Router.Address, sshConfig)
    if err != nil {
        return "", fmt.Errorf("failed to create agent client: %w", err)
    }
    defer agentClient.Close()

    // Allocate lease through agent
    ip, err := agentClient.AllocateLease(
        ctx,
        claim.Status.WanInterface,
        claim.Spec.Router.WanParent,
        claim.Status.MacAddress,
    )
    if err != nil {
        return "", fmt.Errorf("failed to allocate lease: %w", err)
    }

    return ip, nil
}
```

### 3. Update Cleanup Logic

Replace SSH script cleanup with agent client:

```go
func (r *PublicIPClaimReconciler) releaseIP(ctx context.Context, claim *serialxnetv1.PublicIPClaim) error {
    // Get SSH config
    sshConfig, err := r.SSHManager.GetSSHConfig(ctx)
    if err != nil {
        return fmt.Errorf("failed to get SSH config: %w", err)
    }

    // Create agent client
    agentClient, err := router.NewAgentClient(ctx, claim.Spec.Router.Address, sshConfig)
    if err != nil {
        return fmt.Errorf("failed to create agent client: %w", err)
    }
    defer agentClient.Close()

    // Release lease
    if err := agentClient.ReleaseLease(ctx, claim.Status.WanInterface); err != nil {
        return fmt.Errorf("failed to release lease: %w", err)
    }

    return nil
}
```

### 4. Add Lease Status Checking (Optional)

You can check lease status for reconciliation:

```go
func (r *PublicIPClaimReconciler) verifyLease(ctx context.Context, claim *serialxnetv1.PublicIPClaim) error {
    sshConfig, err := r.SSHManager.GetSSHConfig(ctx)
    if err != nil {
        return fmt.Errorf("failed to get SSH config: %w", err)
    }

    agentClient, err := router.NewAgentClient(ctx, claim.Spec.Router.Address, sshConfig)
    if err != nil {
        return fmt.Errorf("failed to create agent client: %w", err)
    }
    defer agentClient.Close()

    // Get lease status
    status, err := agentClient.GetLease(ctx, claim.Status.WanInterface)
    if err != nil {
        return fmt.Errorf("failed to get lease status: %w", err)
    }

    if status == nil {
        return fmt.Errorf("lease not found on agent")
    }

    // Check if lease is stale (after router reboot)
    if status.Status == "stale" {
        logger.Info("lease is stale, will recreate",
            "interface", claim.Status.WanInterface,
            "ip", status.IPAddress)
        // Trigger recreation logic
    }

    return nil
}
```

### 5. Update Reconciliation Loop

Integrate agent client into your reconciliation logic:

```go
func (r *PublicIPClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Fetch PublicIPClaim
    var claim serialxnetv1.PublicIPClaim
    if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Handle deletion
    if !claim.DeletionTimestamp.IsZero() {
        if controllerutil.ContainsFinalizer(&claim, finalizerName) {
            // Release lease via agent
            if err := r.releaseIP(ctx, &claim); err != nil {
                logger.Error(err, "failed to release lease")
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

    // Allocate IP if not allocated
    if claim.Status.AllocatedIP == "" {
        ip, err := r.allocateIP(ctx, &claim)
        if err != nil {
            logger.Error(err, "failed to allocate IP")
            return ctrl.Result{RequeueAfter: time.Minute}, err
        }

        claim.Status.AllocatedIP = ip
        claim.Status.Phase = "Active"
        if err := r.Status().Update(ctx, &claim); err != nil {
            return ctrl.Result{}, err
        }

        logger.Info("IP allocated successfully", "ip", ip)
    }

    // Update Cilium pool (existing logic)
    // ...

    return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}
```

## Error Handling

The agent client returns detailed errors for different scenarios:

```go
ip, err := agentClient.AllocateLease(ctx, iface, wanParent, macAddr)
if err != nil {
    // Check error type
    if strings.Contains(err.Error(), "already has active lease") {
        // Lease already exists (409 Conflict)
        // Handle accordingly
    } else if strings.Contains(err.Error(), "DHCP server not responding") {
        // DHCP timeout (503 Service Unavailable)
        // Retry with backoff
    } else {
        // Other errors (500 Internal Server Error)
        // Log and handle
    }
}
```

## Benefits Over SSH Script Approach

1. **Type Safety**: Go types instead of parsing shell output
2. **Better Error Handling**: HTTP status codes and structured errors
3. **No Broadcast Issues**: Agent uses raw sockets for unicast renewals
4. **Auto Renewal**: Agent handles lease renewals automatically
5. **State Persistence**: Leases survive agent restarts
6. **Reboot Recovery**: Agent marks stale leases for operator to recreate
7. **Testable**: Can mock agent client for unit tests

## Testing

### Unit Tests with Mock Client

```go
// Create a mock agent client for testing
type MockAgentClient struct {
    AllocateLeaseFunc func(ctx context.Context, iface, wanParent, macAddr string) (string, error)
    ReleaseLeaseFunc  func(ctx context.Context, iface string) error
}

func TestReconciler_AllocateIP(t *testing.T) {
    mock := &MockAgentClient{
        AllocateLeaseFunc: func(ctx context.Context, iface, wanParent, macAddr string) (string, error) {
            return "203.0.113.45", nil
        },
    }

    // Test your reconciler with the mock
    // ...
}
```

### Integration Tests

The agent can be tested with a real DHCP server in a Docker container:

```bash
# Start dnsmasq DHCP server for testing
docker run -d --name dhcp-test \
  --cap-add=NET_ADMIN \
  jpillora/dnsmasq \
  --dhcp-range=192.168.100.50,192.168.100.150,12h
```

## Migration Path

1. **Phase 1**: Deploy agent to router alongside existing SSH scripts
2. **Phase 2**: Update operator to use agent client (existing scripts remain as fallback)
3. **Phase 3**: Test thoroughly in production
4. **Phase 4**: Remove SSH script code once agent is proven stable

## Next Steps

1. Deploy agent to your router using `make install-agent`
2. Update your controller to use the agent client
3. Test lease allocation and renewal
4. Monitor agent logs during production use
5. Remove old SSH script code once stable

## Troubleshooting

### SSH Tunnel Issues

If you see "failed to create agent client" errors:
```bash
# Verify agent is running on router
ssh root@192.168.1.1 systemctl status dhcp-wan-agent

# Check agent logs
ssh root@192.168.1.1 journalctl -u dhcp-wan-agent -f
```

### Timeout Errors

The agent client has a 120-second timeout for DHCP operations. If you see timeouts:
- Check DHCP server connectivity
- Verify network interface configuration
- Review agent logs for DHCP failures

### Lease Conflicts

If you get "already has active lease" errors:
- List existing leases: `agentClient.ListLeases(ctx)`
- Release old lease: `agentClient.ReleaseLease(ctx, iface)`
- Retry allocation

## Support

For issues or questions:
1. Check agent logs: `ssh root@router journalctl -u dhcp-wan-agent -f`
2. Review [ROUTER_AGENT_DESIGN.md](ROUTER_AGENT_DESIGN.md) for architecture details
3. See [deploy/agent/README.md](deploy/agent/README.md) for deployment help
