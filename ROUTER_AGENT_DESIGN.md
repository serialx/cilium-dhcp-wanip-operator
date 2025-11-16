# Router Agent Design Document

## Executive Summary

Replace SSH-based shell scripts with a simple Go agent running on the router. The agent uses the `github.com/insomniacslk/dhcp` library to handle DHCP operations with raw sockets, enabling proper DHCP renewal management without IP binding requirements.

## The Problem

**Current approach**: Remove IP from interface → udhcpc can't bind() → falls back to broadcast renewals → unreliable behavior

**Solution**: Use Go DHCP library with raw sockets → send broadcast renewals without IP binding → reliable, RFC-compliant renewals

## Goals

1. ✅ **Reliable DHCP renewals** via raw sockets (broadcast, RFC 2131 compliant)
2. ✅ **No IP binding required** - renewals work even after IP removal from interface
3. ✅ **Simple HTTP API** instead of SSH
4. ✅ **Easy debugging** with structured logs

**Note on Broadcast vs Unicast**: While the `github.com/insomniacslk/dhcp` library supports both renewal methods, unicast renewals don't work reliably in practice. We use broadcast renewals which are fully RFC 2131 compliant and work correctly with raw sockets even when the IP is not bound to the interface.

## Architecture Overview

```
┌────────────────────────────────────────┐
│ K8s Cluster                            │
│  ┌──────────────────────────┐          │
│  │ Operator                 │          │
│  │ - Watches PublicIPClaim  │          │
│  │ - Creates SSH tunnel     │          │
│  │ - Calls agent via tunnel │          │
│  │ - Updates Cilium pool    │          │
│  └────────┬─────────────────┘          │
│           │ SSH tunnel                 │
│           │ (localhost:random → 127.0.0.1:8692)
└───────────┼────────────────────────────┘
            │ Encrypted SSH
┌───────────▼────────────────────────────┐
│ Router (UDM-Pro)                       │
│  ┌──────────────────────────┐          │
│  │ dhcp-wan-agent           │          │
│  │ - Binds to 127.0.0.1:8692│          │
│  │ - DHCP via raw sockets   │          │
│  │ - Auto renewal @ 50%     │          │
│  └────────┬─────────────────┘          │
│     [wan-001] [wan-002]                │
│         │         │                     │
│         └────┬────┘                     │
│         [eth9 WAN]                      │
└──────────────┼─────────────────────────┘
               │
         ISP DHCP Server
```

## Components

### Router Agent (`dhcp-wan-agent`)

Single Go binary running as systemd service on UDM-Pro.

**What it does:**

- HTTP API on **localhost:8692 only** (no network exposure)
- Create/delete macvlan interfaces
- DHCP via `github.com/insomniacslk/dhcp` (raw sockets!)
- Auto-renewal every 50% of lease time with REBIND fallback
- Save state atomically to `/var/lib/dhcp-wan-agent/leases.json`
- Concurrent request safety with mutex protection

**Tech stack:**
- Go 1.21+ stdlib (keep it simple)
- `github.com/insomniacslk/dhcp` for DHCP
- `github.com/vishvananda/netlink` for network config

### Operator Changes

**Replace this:**
```go
ip, err := r.runRouterScript(ctx, &claim, wanIf, macAddr) // SSH + shell
```

**With this:**
```go
// Create SSH tunnel and HTTP client
agentClient, err := router.NewAgentClient(ctx, routerAddr, sshConfig)
defer agentClient.Close()

// Call agent through tunnel (automatically encrypted)
ip, err := agentClient.AllocateLease(ctx, wanIf, macAddr)
```

## Security Model

**Agent**: Binds to `127.0.0.1:8692` only (unreachable from network)

**Operator**: Creates SSH tunnel for all API calls
```go
// Tunnel: localhost:random → router:127.0.0.1:8692
sshClient.Dial("tcp", "127.0.0.1:8692")
```

**Why this is secure and elegant:**

- ✅ Agent unreachable from network (localhost only)
- ✅ All traffic encrypted via SSH
- ✅ Authentication via SSH keys (already configured for operator)
- ✅ No custom auth/crypto code to write or maintain
- ✅ Zero additional dependencies

## API Design

Simple JSON-over-HTTP **through SSH tunnel**. No additional auth needed.

### Endpoints

**POST /leases** - Allocate new IP

```json
// Request
{
  "interface": "wan-001",
  "wanParent": "eth9",
  "macAddress": "02:aa:bb:cc:dd:01"
}

// Response (201 Created)
{
  "ipAddress": "203.0.113.45",
  "expiresAt": "2025-11-13T10:30:00Z"
}

// Error responses
// 409 Conflict - interface already exists
{"error": "interface wan-001 already has active lease"}

// 503 Service Unavailable - DHCP timeout/failure
{"error": "DHCP server not responding after 30s"}

// 500 Internal Server Error
{"error": "failed to create interface: permission denied"}
```

**GET /leases** - List all active leases

```json
{
  "leases": [
    {
      "interface": "wan-001",
      "ipAddress": "203.0.113.45",
      "expiresAt": "2025-11-13T10:30:00Z",
      "renewalCount": 5
    },
    {
      "interface": "wan-002",
      "ipAddress": "203.0.113.46",
      "expiresAt": "2025-11-13T11:00:00Z",
      "renewalCount": 3
    }
  ]
}
```

**GET /leases/{interface}** - Get lease status

```json
{
  "ipAddress": "203.0.113.45",
  "expiresAt": "2025-11-13T10:30:00Z",
  "renewalCount": 5,
  "status": "active",           // "active" or "stale"
  "interfaceExists": true       // true if kernel interface exists
}

// 404 Not Found - interface doesn't exist
{"error": "lease not found: wan-999"}
```

**DELETE /leases/{interface}** - Release IP

```http
204 No Content (success)

404 Not Found - interface doesn't exist
{"error": "lease not found: wan-999"}
```

**GET /health** - Health check

```http
200 OK (agent healthy)
503 Service Unavailable (shutting down)
```

## DHCP Implementation

Key insight: `github.com/insomniacslk/dhcp` uses **raw sockets (PF_PACKET)** which can send packets with arbitrary source IPs without bind(). This solves our problem!

### Lease Data Structure

```go
type Lease struct {
    Interface       string        `json:"interface"`
    IPAddress       net.IP        `json:"ipAddress"`
    DHCPServerIP    net.IP        `json:"dhcpServerIP"`    // Server identifier from DHCP ACK (required for REBIND)
    MACAddress      net.HardwareAddr `json:"macAddress"`
    ExpiresAt       time.Time     `json:"expiresAt"`
    LeaseTime       time.Duration `json:"leaseTime"`
    RenewalCount    int           `json:"renewalCount"`
    Status          string        `json:"status"`          // "active", "stale", "expired"
    InterfaceExists bool          `json:"interfaceExists"` // true if kernel interface exists
}
```

### Core Code

```go
// Acquire DHCP lease
client, _ := nclient4.New("wan-001", nclient4.WithHWAddr(mac))
lease, _ := client.Request(ctx)

ip := lease.ACK.YourIPAddr        // 203.0.113.45
leaseTime := lease.ACK.IPAddressLeaseTime(0)  // 86400 seconds
dhcpServer := lease.ACK.ServerIdentifier()    // CRITICAL: Must persist this for renewals!

// Store in Lease struct for persistence
lease := &Lease{
    Interface:     "wan-001",
    IPAddress:     ip,
    DHCPServerIP:  dhcpServer,  // ← Must persist for REBIND fallback
    ExpiresAt:     time.Now().Add(leaseTime),
    LeaseTime:     leaseTime,
}

// Renew lease using library's Renew() method (broadcast, no IP binding needed!)
// The library handles the RENEW packet construction and sends it via raw sockets
renewedLease, _ := client.Renew(ctx, &nclient4.Lease{Offer: offer, ACK: ack})
// Works even though IP was removed from interface (raw sockets don't require bind!)
// Sends broadcast RENEW (0.0.0.0 → 255.255.255.255) which is RFC 2131 compliant

// Background renewal with proper error handling
// Note: Use agent's shutdown context, not request context!
func (a *Agent) startRenewalLoop(lease *Lease) {
    go func() {
        // Use timer instead of ticker to handle dynamic intervals with backoff
        nextRenewal := lease.LeaseTime / 2
        timer := time.NewTimer(nextRenewal)
        defer timer.Stop()

        backoff := time.Minute
        maxBackoff := 15 * time.Minute

        for {
            select {
            case <-timer.C:
                // Try unicast RENEW first (use fresh context per attempt)
                renewCtx, cancel := context.WithTimeout(a.shutdownCtx, 30*time.Second)
                err := a.renewLease(renewCtx, lease)
                cancel()  // Always cancel immediately after operation

                if err != nil {
                    log.Warn("RENEW failed, trying REBIND",
                        "interface", lease.Interface,
                        "error", err)

                    // Fallback to broadcast REBIND
                    rebindCtx, rebindCancel := context.WithTimeout(a.shutdownCtx, 30*time.Second)
                    err := a.rebindLease(rebindCtx, lease)
                    rebindCancel()  // Always cancel immediately after operation

                    if err != nil {
                        log.Error("REBIND failed, will retry with backoff",
                            "interface", lease.Interface,
                            "error", err,
                            "backoff", backoff)

                        // Exponential backoff: 1m, 2m, 4m, 8m, max 15m
                        timer.Reset(backoff)
                        backoff = backoff * 2
                        if backoff > maxBackoff {
                            backoff = maxBackoff
                        }
                        continue
                    }
                }

                // Reset backoff and timer on success
                backoff = time.Minute
                timer.Reset(lease.LeaseTime / 2)
                log.Info("lease renewed",
                    "interface", lease.Interface,
                    "expiresAt", lease.ExpiresAt)

            case <-a.shutdownCtx.Done():
                log.Info("stopping renewal goroutine", "interface", lease.Interface)
                return
            }
        }
    }()
}
```

### Network Setup (Same as Current Script)

After getting DHCP lease:

```go
// 1. Send gratuitous ARP (pure Go, no arping dependency!)
if err := sendGratuitousARP("wan-001", ip, mac, 3); err != nil {
    return fmt.Errorf("gratuitous ARP failed: %w", err)
}

// 2. Remove IP from interface (avoid BGP conflicts)
netlink.AddrDel(link, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: /32}})

// 3. Enable proxy ARP
os.WriteFile("/proc/sys/net/ipv4/conf/wan-001/proxy_arp", []byte("1"), 0644)

// 4. Add neighbor proxy
netlink.NeighSet(&netlink.Neigh{IP: ip, Flags: netlink.NTF_PROXY})

// 5. Disable rp_filter
os.WriteFile("/proc/sys/net/ipv4/conf/wan-001/rp_filter", []byte("0"), 0644)
```

### Gratuitous ARP Implementation

Pure Go implementation using raw sockets (no external dependencies):

```go
import (
    "net"
    "syscall"
    "time"
)

// sendGratuitousARP sends gratuitous ARP announcements
func sendGratuitousARP(ifaceName string, ip net.IP, mac net.HardwareAddr, count int) error {
    // Get interface
    iface, err := net.InterfaceByName(ifaceName)
    if err != nil {
        return err
    }

    // Create raw socket (AF_PACKET for layer 2)
    fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ARP)))
    if err != nil {
        return err
    }
    defer syscall.Close(fd)

    // Build gratuitous ARP packet
    packet := buildGratuitousARP(mac, ip)

    // Bind to interface
    addr := syscall.SockaddrLinklayer{
        Protocol: htons(syscall.ETH_P_ARP),
        Ifindex:  iface.Index,
    }

    // Send multiple times (typical: 3)
    for i := 0; i < count; i++ {
        if err := syscall.Sendto(fd, packet, 0, &addr); err != nil {
            return err
        }
        if i < count-1 {
            time.Sleep(200 * time.Millisecond)
        }
    }

    return nil
}

// buildGratuitousARP builds an ARP announcement packet
func buildGratuitousARP(mac net.HardwareAddr, ip net.IP) []byte {
    packet := make([]byte, 42)

    // Ethernet header (14 bytes)
    copy(packet[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // Dst: broadcast
    copy(packet[6:12], mac)                                        // Src: our MAC
    packet[12] = 0x08                                              // EtherType: ARP
    packet[13] = 0x06

    // ARP header (28 bytes)
    packet[14] = 0x00; packet[15] = 0x01 // Hardware type: Ethernet
    packet[16] = 0x08; packet[17] = 0x00 // Protocol type: IPv4
    packet[18] = 0x06                     // Hardware size: 6
    packet[19] = 0x04                     // Protocol size: 4
    packet[20] = 0x00; packet[21] = 0x01 // Opcode: Request (gratuitous)

    copy(packet[22:28], mac)              // Sender MAC
    copy(packet[28:32], ip.To4())         // Sender IP
    copy(packet[32:38], []byte{0, 0, 0, 0, 0, 0}) // Target MAC: 00:00:00:00:00:00
    copy(packet[38:42], ip.To4())         // Target IP: same as sender (gratuitous!)

    return packet
}

// htons converts host byte order to network byte order (big-endian)
func htons(v uint16) uint16 {
    return (v << 8) | (v >> 8)
}
```

## Deployment

### On UDM-Pro

```bash
# 1. Copy binary
scp dhcp-wan-agent root@192.168.1.1:/usr/local/bin/

# 2. Create state directory
mkdir -p /var/lib/dhcp-wan-agent
chmod 700 /var/lib/dhcp-wan-agent

# 3. Create systemd service
cat > /etc/systemd/system/dhcp-wan-agent.service <<'EOF'
[Unit]
Description=DHCP WAN Agent
After=network.target

[Service]
ExecStart=/usr/local/bin/dhcp-wan-agent
Restart=always
RestartSec=5

# Security hardening
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

# 4. Start it
systemctl daemon-reload
systemctl enable --now dhcp-wan-agent

# 5. Verify
systemctl status dhcp-wan-agent
journalctl -u dhcp-wan-agent -f
```

### Operator Config

Update `PublicIPClaim` (agentURL removed - SSH config reused):

```yaml
apiVersion: serialx.net/v1alpha1
kind: PublicIPClaim
metadata:
  name: ip-wan-001
spec:
  poolName: public-pool
  router:
    address: 192.168.1.1  # SSH to router (reuse existing config)
    wanParent: eth9
    # No agentURL needed - operator creates SSH tunnel automatically
```

## Operator Implementation

### SSH Tunnel Client

Replace SSH script execution with SSH tunnel + HTTP:

```go
package router

import (
    "context"
    "fmt"
    "io"
    "net"
    "net/http"

    "golang.org/x/crypto/ssh"
)

// AgentClient wraps HTTP client with SSH tunnel
type AgentClient struct {
    sshClient     *ssh.Client
    httpClient    *http.Client
    localAddr     string
    localListener net.Listener
    ctx           context.Context
    cancel        context.CancelFunc
}

// NewAgentClient creates SSH tunnel to router agent
func NewAgentClient(ctx context.Context, routerAddr string, sshConfig *ssh.ClientConfig) (*AgentClient, error) {
    // 1. SSH to router
    sshClient, err := ssh.Dial("tcp", routerAddr+":22", sshConfig)
    if err != nil {
        return nil, fmt.Errorf("ssh dial failed: %w", err)
    }

    // 2. Create local listener for tunnel
    localListener, err := net.Listen("tcp", "127.0.0.1:0")  // Random port
    if err != nil {
        sshClient.Close()
        return nil, fmt.Errorf("local listen failed: %w", err)
    }

    localAddr := localListener.Addr().String()

    // 3. Create cancellable context for tunnel lifecycle
    tunnelCtx, cancel := context.WithCancel(context.Background())

    // 4. Forward connections through SSH tunnel
    go func() {
        defer localListener.Close()
        for {
            select {
            case <-tunnelCtx.Done():
                return  // Tunnel cancelled
            default:
            }

            // Accept connection on localhost
            localConn, err := localListener.Accept()
            if err != nil {
                return  // Listener closed
            }

            // Check if context is cancelled before forwarding
            if tunnelCtx.Err() != nil {
                localConn.Close()
                return
            }

            // Dial agent on router's localhost through SSH
            remoteConn, err := sshClient.Dial("tcp", "127.0.0.1:8692")
            if err != nil {
                localConn.Close()
                continue
            }

            // Bidirectional copy
            go copyConn(localConn, remoteConn)
        }
    }()

    // 5. HTTP client uses local tunnel endpoint with proper timeouts
    // Timeout budget breakdown for DHCP operations:
    //   - Dial (SSH tunnel):           5s  (local connection, should be fast)
    //   - Request write:               5s  (small JSON payload)
    //   - DHCP operation on agent:    60s  (DISCOVER/REQUEST/ACK sequence)
    //   - Response read:               5s  (small JSON response)
    //   - Total with buffer:         120s  (2 minutes)
    httpClient := &http.Client{
        Timeout: 120 * time.Second,  // Overall request timeout
        Transport: &http.Transport{
            DialContext: (&net.Dialer{
                Timeout:   5 * time.Second,   // SSH tunnel connection (local)
                KeepAlive: 30 * time.Second,  // Keep tunnel connections alive
            }).DialContext,
            ResponseHeaderTimeout: 90 * time.Second,  // Wait for agent to complete DHCP + send headers
            ExpectContinueTimeout: 1 * time.Second,
            IdleConnTimeout:       90 * time.Second,
            MaxIdleConns:          10,
            MaxIdleConnsPerHost:   2,
        },
    }

    return &AgentClient{
        sshClient:     sshClient,
        httpClient:    httpClient,
        localAddr:     localAddr,
        localListener: localListener,
        ctx:           tunnelCtx,
        cancel:        cancel,
    }, nil
}

func copyConn(local, remote net.Conn) {
    defer local.Close()
    defer remote.Close()

    // Bidirectional copy with proper synchronization
    // Use WaitGroup to avoid premature connection closure causing
    // "use of closed connection" errors in the other goroutine
    var wg sync.WaitGroup
    wg.Add(2)

    // Local → Remote
    go func() {
        defer wg.Done()
        io.Copy(remote, local)
        // Signal remote that we're done writing
        if conn, ok := remote.(*net.TCPConn); ok {
            conn.CloseWrite()
        }
    }()

    // Remote → Local
    go func() {
        defer wg.Done()
        io.Copy(local, remote)
        // Signal local that we're done writing
        if conn, ok := local.(*net.TCPConn); ok {
            conn.CloseWrite()
        }
    }()

    // Wait for both directions to complete before closing
    wg.Wait()
}

// AllocateLease calls agent through tunnel
func (c *AgentClient) AllocateLease(ctx context.Context, iface, wanParent, macAddr string) (string, error) {
    body, _ := json.Marshal(map[string]string{
        "interface":   iface,
        "wanParent":   wanParent,
        "macAddress":  macAddr,
    })

    url := fmt.Sprintf("http://%s/leases", c.localAddr)
    req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return "", fmt.Errorf("agent request failed: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusCreated {
        var errResp struct {
            Error string `json:"error"`
        }
        json.NewDecoder(resp.Body).Decode(&errResp)
        return "", fmt.Errorf("agent returned %d: %s", resp.StatusCode, errResp.Error)
    }

    var result struct {
        IPAddress string `json:"ipAddress"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return "", fmt.Errorf("decode response failed: %w", err)
    }

    return result.IPAddress, nil
}

// ListLeases lists all active leases
func (c *AgentClient) ListLeases(ctx context.Context) ([]Lease, error) {
    url := fmt.Sprintf("http://%s/leases", c.localAddr)
    req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var result struct {
        Leases []Lease `json:"leases"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    return result.Leases, nil
}

// Close tears down SSH tunnel
func (c *AgentClient) Close() error {
    // 1. Cancel tunnel context to stop accepting new connections
    c.cancel()

    // 2. Close the local listener
    if c.localListener != nil {
        c.localListener.Close()
    }

    // 3. Close SSH connection (this will close all forwarded connections)
    return c.sshClient.Close()
}
```

### Controller Usage

```go
// In PublicIPClaimReconciler
func (r *PublicIPClaimReconciler) allocateIP(ctx context.Context, claim *PublicIPClaim) (string, error) {
    // Create SSH tunnel + agent client
    agentClient, err := router.NewAgentClient(ctx,
        claim.Spec.Router.Address,
        r.getSSHConfig(),  // Reuse existing SSH key config
    )
    if err != nil {
        return "", fmt.Errorf("failed to connect to agent: %w", err)
    }
    defer agentClient.Close()

    // Call agent through tunnel (automatically encrypted)
    ip, err := agentClient.AllocateLease(ctx,
        claim.Status.WanInterface,
        claim.Spec.Router.WanParent,
        claim.Status.MacAddress,
    )
    if err != nil {
        return "", fmt.Errorf("lease allocation failed: %w", err)
    }

    log.Info("lease acquired",
        "claim", claim.Name,
        "ip", ip,
        "interface", claim.Status.WanInterface)

    return ip, nil
}
```

## State Persistence

### Atomic Writes

Agent saves leases atomically to prevent corruption:

```go
// Write to temp file, then atomic rename
func (a *Agent) saveState() error {
    tmp := "/var/lib/dhcp-wan-agent/leases.json.tmp"

    data, _ := json.MarshalIndent(a.leases, "", "  ")
    if err := ioutil.WriteFile(tmp, data, 0600); err != nil {
        return err
    }

    // Atomic rename (even if process crashes mid-write)
    return os.Rename(tmp, "/var/lib/dhcp-wan-agent/leases.json")
}
```

### Reboot Recovery Strategy

**Problem**: Macvlan interfaces and kernel state do NOT survive router reboots.

**Solution**: Operator-driven reconciliation (keep agent simple)

```go
// On agent startup, verify kernel state matches persisted leases
func (a *Agent) loadState() error {
    data, err := os.ReadFile("/var/lib/dhcp-wan-agent/leases.json")
    if err != nil {
        if os.IsNotExist(err) {
            return nil  // First run, no state to load
        }
        return err
    }

    var leases map[string]*Lease
    if err := json.Unmarshal(data, &leases); err != nil {
        return err
    }

    // Verify each interface exists in kernel
    for iface, lease := range leases {
        link, err := netlink.LinkByName(iface)
        if err != nil {
            // Interface missing (router rebooted) - mark as STALE
            log.Warn("interface missing after reboot",
                "interface", iface,
                "ip", lease.IPAddress)
            lease.Status = "stale"
            lease.InterfaceExists = false
            continue
        }

        // Interface exists - verify IP configuration
        addrs, _ := netlink.AddrList(link, netlink.FAMILY_V4)
        hasIP := false
        for _, addr := range addrs {
            if addr.IP.Equal(lease.IPAddress) {
                hasIP = true
                break
            }
        }

        lease.InterfaceExists = true

        if !hasIP {
            log.Info("IP not bound to interface (expected)",
                "interface", iface,
                "ip", lease.IPAddress)
        }

        // CRITICAL: Verify lease is still valid with DHCP server
        // If router was down longer than lease time, IP may be reassigned
        log.Info("verifying lease validity with DHCP server",
            "interface", iface,
            "ip", lease.IPAddress)

        renewCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        err = a.renewLease(renewCtx, lease)
        cancel()

        if err != nil {
            log.Warn("lease renewal failed after reboot, marking stale",
                "interface", iface,
                "ip", lease.IPAddress,
                "error", err)
            lease.Status = "stale"
            lease.InterfaceExists = true  // Interface exists but lease is invalid
            continue
        }

        // Lease verified and renewed successfully
        lease.Status = "active"
        log.Info("lease verified and renewed successfully",
            "interface", iface,
            "ip", lease.IPAddress,
            "expiresAt", lease.ExpiresAt)

        // Restart renewal loop for active leases
        a.startRenewalLoop(lease)
    }

    a.leases = leases
    return nil
}

// GET /leases/{interface} returns status field:
// - "active": Interface exists, renewals running, lease verified with DHCP server
// - "stale": Interface missing OR lease renewal failed (reboot), operator must recreate
```

This approach:

- ✅ Validates leases with DHCP server on startup (prevents IP conflicts after extended downtime)
- ✅ Avoids agent complexity (no reboot detection logic)
- ✅ Reuses operator's existing reconciliation
- ✅ Operator already watches and fixes drift
- ✅ Simple: Agent is stateless except for lease tracking
- ✅ Clear status field lets operator detect and fix stale leases

### Concurrency Safety

```go
type Agent struct {
    mu          sync.RWMutex
    leases      map[string]*Lease     // Protected by mu

    shutdownCtx    context.Context    // Agent lifecycle context
    shutdownCancel context.CancelFunc

    inflightMu  sync.Mutex
    inflight    map[string]chan struct{}  // Per-interface operation locks

    httpServer  *http.Server
}

// Helper methods for per-interface locking
func (a *Agent) acquireInflight(iface string) bool {
    a.inflightMu.Lock()
    defer a.inflightMu.Unlock()

    if _, exists := a.inflight[iface]; exists {
        return false  // Operation already in progress
    }
    a.inflight[iface] = make(chan struct{})
    return true
}

func (a *Agent) releaseInflight(iface string) {
    a.inflightMu.Lock()
    defer a.inflightMu.Unlock()

    if ch, exists := a.inflight[iface]; exists {
        close(ch)
        delete(a.inflight, iface)
    }
}

func (a *Agent) AllocateLease(ctx context.Context, req *LeaseRequest) (*Lease, error) {
    // Per-interface synchronization (prevents concurrent ops on same interface)
    // IMPORTANT: Acquire this FIRST to prevent race conditions
    if !a.acquireInflight(req.Interface) {
        return nil, fmt.Errorf("operation already in progress for interface %s", req.Interface)
    }
    defer a.releaseInflight(req.Interface)

    // Check if lease exists AFTER acquiring inflight lock (prevents race)
    a.mu.RLock()
    if _, exists := a.leases[req.Interface]; exists {
        a.mu.RUnlock()
        return nil, ErrLeaseExists
    }
    a.mu.RUnlock()

    // ... DHCP operations (30+ seconds, no global lock held!) ...
    // 1. Create macvlan interface
    // 2. Request DHCP lease
    // 3. Configure networking (GARP, proxy ARP, etc.)

    lease := &Lease{
        Interface:    req.Interface,
        IPAddress:    obtainedIP,
        DHCPServerIP: dhcpServerIP,
        ExpiresAt:    expiresAt,
        LeaseTime:    leaseTime,
    }

    // Write lock only for map update (brief)
    a.mu.Lock()
    a.leases[req.Interface] = lease
    a.mu.Unlock()

    // Start background renewal
    a.startRenewalLoop(lease)

    return lease, a.saveState()
}
```

### Graceful Shutdown

```go
// On SIGTERM:
func (a *Agent) Shutdown(ctx context.Context) error {
    log.Info("shutting down gracefully")

    // 1. Stop accepting new requests
    if err := a.httpServer.Shutdown(ctx); err != nil {
        log.Error("http server shutdown failed", "error", err)
    }

    // 2. Cancel all renewal goroutines
    a.shutdownCancel()

    // 3. Wait for in-flight operations to complete (with timeout)
    // IMPORTANT: Collect channels first without holding lock to avoid deadlock.
    // We MUST release the lock before waiting on channels, because:
    // - releaseInflight() (called by defer in AllocateLease) needs inflightMu
    // - If we hold inflightMu while waiting, AllocateLease can't complete
    // - Result: deadlock
    done := make(chan struct{})
    go func() {
        // Step 1: Snapshot all in-flight channels while holding lock
        a.inflightMu.Lock()
        channels := make([]chan struct{}, 0, len(a.inflight))
        interfaces := make([]string, 0, len(a.inflight))
        for iface, ch := range a.inflight {
            interfaces = append(interfaces, iface)
            channels = append(channels, ch)
        }
        a.inflightMu.Unlock()  // ← CRITICAL: Release lock before waiting!

        // Step 2: Wait for all operations without holding lock (no deadlock)
        for i, ch := range channels {
            log.Info("waiting for in-flight operation", "interface", interfaces[i])
            <-ch  // Wait for operation to complete
        }

        close(done)
    }()

    select {
    case <-done:
        log.Info("all in-flight operations completed")
    case <-time.After(30 * time.Second):
        log.Warn("shutdown timeout, some operations may not have completed")
    }

    // 4. Save current state
    if err := a.saveState(); err != nil {
        log.Error("failed to save state", "error", err)
        return err
    }

    // 5. Do NOT release leases (let renewals continue after restart)
    log.Info("shutdown complete")
    return nil
}
```

## Testing Strategy

### Agent Unit Tests

```go
// Test DHCP logic with mock server
func TestDHCPRenewal(t *testing.T) {
    // Use github.com/insomniacslk/dhcp test helpers
}

// Test state persistence
func TestStatePersistence(t *testing.T) {
    // Verify atomic writes, reboot recovery
}

// Test concurrent requests
func TestConcurrentAllocations(t *testing.T) {
    // Verify mutex protection
}
```

### Integration Tests

```go
// Test against real dnsmasq DHCP server in Docker
func TestE2EDHCPFlow(t *testing.T) {
    // 1. Start dnsmasq container
    // 2. Create macvlan interface
    // 3. Allocate lease via agent API
    // 4. Verify renewal works
    // 5. Test release
}
```

### E2E Tests

```go
// Test operator + agent in kind cluster
func TestOperatorWithAgent(t *testing.T) {
    // 1. Deploy mock router with agent
    // 2. Create PublicIPClaim
    // 3. Verify IP allocation
    // 4. Verify Cilium pool update
    // 5. Test claim deletion cleanup
}
```

## Summary

**Why this design is simple and elegant:**

| Aspect | Design Choice | Why It's Elegant |
|--------|---------------|------------------|
| **Security** | SSH tunnel | Zero custom auth code, reuse existing SSH config |
| **Agent binding** | localhost only | Impossible to access from network |
| **API** | JSON-over-HTTP | Simple, debuggable, no gRPC complexity |
| **State** | Single JSON file | No database, atomic writes, easy to inspect |
| **DHCP** | Raw sockets | Solves broadcast problem at the root |
| **Recovery** | Operator-driven | Keep agent simple, reuse reconciliation |
| **Concurrency** | Per-interface locking | Fine-grained control, no blocking on long DHCP ops |

**Comparison to current approach:**

| Feature | SSH Script (udhcpc) | Go Agent |
|---------|------------|----------|
| DHCP Renewals | ⚠️ Unreliable broadcast (can't bind without IP) | ✅ Reliable broadcast via raw sockets (no bind needed) |
| IP Binding | ❌ Required for udhcpc → causes conflicts with BGP | ✅ Not required → clean separation from BGP routing |
| Security | ⚠️ SSH keys | ✅ SSH tunnel (same keys, localhost agent) |
| Code Quality | ⚠️ Shell script | ✅ Testable Go with unit tests |
| Debugging | ⚠️ Parse stderr | ✅ Structured logs + API inspection |
| Error Handling | ⚠️ Exit codes | ✅ Proper errors + retry/fallback (RENEW→REBIND) |
| Recovery | ❌ Manual | ✅ Auto-restart + operator reconciliation |
| Dependencies | ⚠️ `arping`, `ip`, `sysctl` | ✅ Pure Go (raw sockets for everything) |

**Implementation timeline:** 3-4 weeks (realistic)

1. **Week 1**: Core agent
   - DHCP operations with raw sockets
   - HTTP API with error handling
   - State persistence

2. **Week 2**: Robustness
   - Renewal logic with REBIND fallback
   - Graceful shutdown
   - Unit tests

3. **Week 3**: Operator integration
   - SSH tunnel client
   - Update controller to use agent
   - Integration tests

4. **Week 4**: Deployment & polish
   - E2E testing
   - Documentation
   - Production deployment
