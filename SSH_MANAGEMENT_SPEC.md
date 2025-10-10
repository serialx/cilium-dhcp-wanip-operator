# SSH Connection Management Specification

## Overview

### Problem Statement

The operator needs to manage configuration on remote routers via SSH. Router reboots cause all operator-configured state to be lost:
- Macvlan interfaces are deleted
- udhcpc daemon processes are killed
- Proxy ARP configurations are cleared
- Configuration must be reapplied to restore functionality

**Challenge:** How do we detect router state changes (reboots, connection losses) quickly and reliably without polling or requiring router-side modifications?

### Solution

Maintain **long-running SSH connections** to each router with:
1. **Active connection monitoring** via keep-alive packets (30s intervals)
2. **Router reboot detection** by tracking uptime changes
3. **Automatic reconnection** when connection drops
4. **Event notification system** to trigger external handlers (e.g., reconciliation)
5. **Connection pooling** - one SSH manager per router address, shared by multiple claims

### Key Design Principles

- ✅ **No router script modifications** - Use direct SSH commands for all verification
- ✅ **Standard Linux commands** - Portable across router types
- ✅ **Per-router connection pooling** - Multiple claims share one SSH connection
- ✅ **Fast detection** - Reboots/connection drops detected within ~40 seconds (30s check interval + 10s timeout)
- ✅ **Thread-safe** - Safe for concurrent command execution from multiple goroutines
- ✅ **Observable** - Logging and metrics for monitoring connection health

---

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────┐
│  Cilium DHCP WAN IP Operator                     │
│                                                   │
│  ┌──────────────────────────────────────────┐   │
│  │  Multiple Claims/Consumers                │   │
│  │                                            │   │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐│   │
│  │  │ Claim 1  │  │ Claim 2  │  │ Claim 3  ││   │
│  │  │ (wan0.d1)│  │ (wan0.d2)│  │ (wan1.d1)││   │
│  │  └────┬─────┘  └────┬─────┘  └────┬─────┘│   │
│  │       │             │             │       │   │
│  │       └─────────────┴─────────────┘       │   │
│  │                     │                     │   │
│  │                     ▼                     │   │
│  │       ┌─────────────────────────────┐    │   │
│  │       │  SSH Manager Registry       │    │   │
│  │       │  (manager per router addr)  │    │   │
│  │       └──────────┬──────────────────┘    │   │
│  └──────────────────┼─────────────────────────┘ │
│                     │                             │
│                     ▼                             │
│       ┌─────────────────────────────────┐        │
│       │  SSH Connection Manager         │        │
│       │  (192.168.1.1)                  │        │
│       │                                 │        │
│       │  State:                         │        │
│       │  - conn: *ssh.Client            │        │
│       │  - connected: true              │        │
│       │  - lastUptime: 123456.78        │        │
│       │                                 │        │
│       │  Goroutines:                    │        │
│       │  - Keep-alive loop (30s)        │        │
│       │  - Auto-reconnect handler       │        │
│       └──────────┬──────────────────────┘        │
│                  │                                │
└──────────────────┼────────────────────────────────┘
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

### Connection Lifecycle

```
[Consumer Requests Manager]
       │
       ▼
[Get SSH Manager for Router]
       │
       ├─> Manager exists? ──Yes──> Use existing
       │
       └─> No
           │
           ▼
    [Create SSH Manager]
           │
           ├─> Initialize connection config
           ├─> Register event handler
           └─> Start connection goroutine
                   │
                   ▼
            [Connecting...]
                   │
                   ├──error──> [Retry with exponential backoff]
                   │              1s → 2s → 4s → 8s → max 60s
                   │              │
                   │              └──────────┐
                   │                         │
                   │success                  │
                   ▼                         │
            [Connected] <───────────────────┘
                   │
                   ├─> Start keep-alive loop (30s)
                   │
                   └─> Execute commands as needed


[Keep-Alive Loop]
       │
       ▼
[Every 30 seconds]
       │
       ├─> Execute: cat /proc/uptime
       │
       ├─> Success?
       │   │
       │   ├─Yes─> Compare with lastUptime
       │   │       │
       │   │       ├─> Uptime increased? ──> Normal (update lastUptime)
       │   │       │
       │   │       └─> Uptime decreased? ──> REBOOT DETECTED!
       │   │                                  │
       │   │                                  └─> Notify event handlers
       │   │
       │   └─No──> Connection lost!
       │           │
       │           └─> [Handle Connection Drop]
       │                   │
       │                   ├─> Notify all event handlers
       │                   │
       │                   └─> [Reconnecting...]
       │                           │
       │                           └─> Exponential backoff → [Connected]
       │
       └─> [Manager Shutdown]
               │
               └─> Gracefully close connection
```

---

## SSH Connection Manager

### Package Structure

```
internal/
├── ssh/
│   ├── manager.go          # SSHConnectionManager implementation
│   ├── registry.go         # SSHManagerRegistry (singleton map)
│   ├── config.go           # RouterConfig, SSHConfig
│   └── commands.go         # High-level command wrappers
```

### Core Types

```go
// ReconnectionReason indicates why a reconnection event was triggered
type ReconnectionReason string

const (
    ReasonReboot         ReconnectionReason = "reboot"
    ReasonConnectionDrop ReconnectionReason = "connection_drop"
)

// RouterConfig contains router connection details
type RouterConfig struct {
    Address         string               // Router SSH address (e.g., "192.168.1.1:22")
    Username        string               // SSH username
    AuthMethod      ssh.AuthMethod       // Key-based or password auth
    HostKeyCallback ssh.HostKeyCallback  // Host key verification (nil = insecure)
    Timeout         time.Duration        // Connection timeout (default: 30s)
}

// SSHConnectionManager manages a single SSH connection to a router
type SSHConnectionManager struct {
    config       RouterConfig
    conn         *ssh.Client
    mu           sync.RWMutex

    // State
    connected    bool
    reconnecting bool
    lastUptime   float64 // Last observed uptime, preserved across reconnects
    uptimeReady  bool    // Indicates lastUptime contains a baseline measurement

    // Lifecycle
    ctx          context.Context
    cancel       context.CancelFunc
    wg           sync.WaitGroup

    // Event handlers (map key: handler ID like "namespace/name")
    handlers   map[string]func(ReconnectionReason)
    handlersMu sync.RWMutex
}

// SSHManagerRegistry is a singleton that manages SSH managers per router
type SSHManagerRegistry struct {
    managers map[string]*SSHConnectionManager  // Key: router address
    mu       sync.RWMutex
    baseCtx  context.Context                   // Long-lived context controlling manager lifecycles
}
```

### Key Methods

#### Connection Management

```go
// Start initiates the SSH connection and background goroutines
func (m *SSHConnectionManager) Start(ctx context.Context) error {
    // Prevent calling Start() multiple times (guard against goroutine/context leaks)
    m.mu.Lock()
    if m.ctx != nil {
        m.mu.Unlock()
        return fmt.Errorf("SSH manager already started")
    }
    m.ctx, m.cancel = context.WithCancel(ctx)
    m.mu.Unlock()

    // Initial connection
    if err := m.connect(); err != nil {
        // Record reconnecting state before starting background loop
        m.mu.Lock()
        m.connected = false
        m.reconnecting = true
        m.mu.Unlock()

        // Start reconnection in background
        m.wg.Add(1)
        go m.reconnectLoop()
        return fmt.Errorf("initial connection failed, reconnecting in background: %w", err)
    }

    // Start keep-alive loop
    m.wg.Add(1)
    go m.keepAliveLoop()

    return nil
}

// connect establishes the SSH connection
func (m *SSHConnectionManager) connect() error {
    m.mu.Lock()
    defer m.mu.Unlock()

    // Use host key verification from config
    // If not provided, fall back to InsecureIgnoreHostKey (not recommended for production)
    hostKeyCallback := m.config.HostKeyCallback
    if hostKeyCallback == nil {
        log.Warn("Using insecure host key verification - not recommended for production",
            "router", m.config.Address)
        hostKeyCallback = ssh.InsecureIgnoreHostKey()
    }

    timeout := m.config.Timeout
    if timeout == 0 {
        timeout = 30 * time.Second // Default timeout when not supplied
    }

    sshConfig := &ssh.ClientConfig{
        User: m.config.Username,
        Auth: []ssh.AuthMethod{m.config.AuthMethod},
        HostKeyCallback: hostKeyCallback,
        Timeout: timeout,
    }

    conn, err := ssh.Dial("tcp", m.config.Address, sshConfig)
    if err != nil {
        return fmt.Errorf("SSH dial failed: %w", err)
    }

    m.conn = conn
    m.connected = true

    // Preserve lastUptime so the next keep-alive can detect reboots after reconnects

    log.Info("SSH connection established", "router", m.config.Address)
    return nil
}

// Close gracefully shuts down the SSH connection
func (m *SSHConnectionManager) Close() error {
    // Check if cancel exists before calling (prevents panic if Close() called before Start())
    if m.cancel != nil {
        m.cancel()  // Signal all goroutines to stop
    }
    m.wg.Wait() // Wait for goroutines to finish

    m.mu.Lock()
    defer m.mu.Unlock()

    if m.conn != nil {
        err := m.conn.Close()
        m.connected = false
        return err
    }
    return nil
}

// IsConnected returns whether the SSH connection is currently active
func (m *SSHConnectionManager) IsConnected() bool {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.connected
}

// IsReconnecting returns whether the manager is attempting to reconnect
func (m *SSHConnectionManager) IsReconnecting() bool {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.reconnecting
}
```

#### Keep-Alive and Reboot Detection

```go
// keepAliveLoop runs the keep-alive check every 30 seconds
func (m *SSHConnectionManager) keepAliveLoop() {
    defer m.wg.Done()

    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            if err := m.checkConnection(); err != nil {
                log.Error(err, "Keep-alive check failed", "router", m.config.Address)
                m.handleConnectionDrop()
                return  // Exit loop, reconnection will start new loop
            }

        case <-m.ctx.Done():
            return
        }
    }
}

// checkConnection verifies the connection and detects reboots
func (m *SSHConnectionManager) checkConnection() error {
    const maxUptimeJump = 86400.0  // 1 day in seconds - detect suspicious jumps

    // Get current router uptime
    uptime, err := m.GetRouterUptime()
    if err != nil {
        return fmt.Errorf("failed to get router uptime: %w", err)
    }

    // Detect reboot or uptime anomalies (with mutex protection)
    m.mu.Lock()
    lastUptime := m.lastUptime
    hasBaseline := m.uptimeReady
    if !m.uptimeReady {
        // First successful sample after start; establish baseline
        m.lastUptime = uptime
        m.uptimeReady = true
    } else {
        m.lastUptime = uptime
    }
    m.mu.Unlock()

    if hasBaseline {
        if uptime < lastUptime {
            // Uptime decreased - router rebooted
            log.Info("Router reboot detected",
                "router", m.config.Address,
                "previousUptime", lastUptime,
                "currentUptime", uptime)

            // Trigger handlers via notification
            m.notifyHandlers(ReasonReboot)
        } else if uptime - lastUptime > maxUptimeJump {
            // Suspicious jump - possible counter reset or time manipulation
            log.Warn("Suspicious uptime jump detected",
                "router", m.config.Address,
                "previousUptime", lastUptime,
                "currentUptime", uptime,
                "jump", uptime - lastUptime)
            // Don't trigger handlers for forward jumps, just log
        }
    }

    return nil
}

// handleConnectionDrop handles connection failures
func (m *SSHConnectionManager) handleConnectionDrop() {
    m.mu.Lock()
    if !m.connected || m.reconnecting {
        m.mu.Unlock()
        return  // Already handling or reconnecting
    }

    staleConn := m.conn
    m.conn = nil
    m.connected = false
    m.reconnecting = true
    m.mu.Unlock()

    // Drop the previous client to avoid leaking file descriptors or goroutines
    if staleConn != nil {
        _ = staleConn.Close()
    }

    log.Warn("SSH connection dropped", "router", m.config.Address)

    // Notify handlers
    m.notifyHandlers(ReasonConnectionDrop)

    // Start reconnection
    m.wg.Add(1)
    go m.reconnectLoop()
}

// reconnectLoop attempts to reconnect with exponential backoff
func (m *SSHConnectionManager) reconnectLoop() {
    defer m.wg.Done()

    backoff := 1 * time.Second
    maxBackoff := 60 * time.Second

    for {
        select {
        case <-m.ctx.Done():
            return
        case <-time.After(backoff):
            log.Info("Attempting to reconnect", "router", m.config.Address, "backoff", backoff)

            if err := m.connect(); err != nil {
                log.Error(err, "Reconnection attempt failed", "router", m.config.Address)

                // Exponential backoff
                backoff *= 2
                if backoff > maxBackoff {
                    backoff = maxBackoff
                }
                continue
            }

            // Reconnection successful
            m.mu.Lock()
            m.reconnecting = false
            m.mu.Unlock()

            log.Info("Reconnection successful", "router", m.config.Address)

            // Restart keep-alive loop
            m.wg.Add(1)
            go m.keepAliveLoop()
            return
        }
    }
}
```

By keeping `lastUptime` and the `uptimeReady` baseline intact across reconnects, the first keep-alive cycle after a connection drop still compares against the previous uptime sample and can emit a `ReasonReboot` notification when the router counter resets.

#### Command Execution

```go
// Execute runs a command on the router with a default 10s timeout
func (m *SSHConnectionManager) Execute(cmd string) (string, error) {
    return m.ExecuteWithTimeout(cmd, 10*time.Second)
}

// ExecuteWithTimeout runs a command on the router with specified timeout
func (m *SSHConnectionManager) ExecuteWithTimeout(cmd string, timeout time.Duration) (string, error) {
    m.mu.RLock()
    if !m.connected {
        m.mu.RUnlock()
        return "", fmt.Errorf("not connected to router")
    }
    conn := m.conn
    m.mu.RUnlock()

    session, err := conn.NewSession()
    if err != nil {
        if isSessionFatal(err) {
            m.handleConnectionDrop()
        }
        return "", fmt.Errorf("failed to create SSH session: %w", err)
    }
    defer session.Close()

    // Create context with timeout
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    // Channel for command result
    type result struct {
        output []byte
        err    error
    }
    resultChan := make(chan result, 1)

    // Run command in goroutine
    go func() {
        output, err := session.CombinedOutput(cmd)
        resultChan <- result{output: output, err: err}
    }()

    // Wait for completion or timeout
    select {
    case res := <-resultChan:
        if res.err != nil {
            return string(res.output), fmt.Errorf("command failed: %w", res.err)
        }
        return string(res.output), nil
    case <-ctx.Done():
        // Try to signal the session to stop
        session.Signal(ssh.SIGKILL)
        session.Close()
        return "", fmt.Errorf("command timeout after %v: %s", timeout, cmd)
    }
}

// isSessionFatal returns true for transport-level failures that require rebuilding
// the SSH client and false for channel-level rejections (e.g., MaxSessions reached).
func isSessionFatal(err error) bool {
    if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
        return true
    }

    var netErr *net.OpError
    if errors.As(err, &netErr) {
        return true
    }

    return false
}
```

> Implementation note: the helper relies on `errors`, `io`, `net`, and `context`.

The shorter 10-second default keeps the keep-alive SLO within the stated 40-second target. Callers that expect longer-running operations (e.g., firmware uploads) must explicitly use `ExecuteWithTimeout` to raise the ceiling for that invocation.

#### Event Handlers

Event consumers must accept a `ReconnectionReason` argument so they can differentiate between reboots and transport drops when reacting to notifications.

```go
// RegisterHandler registers a handler for connection events (idempotent)
// handlerKey should be a unique identifier (e.g., "namespace/name" for claims)
func (m *SSHConnectionManager) RegisterHandler(handlerKey string, handler func(ReconnectionReason)) {
    m.handlersMu.Lock()
    defer m.handlersMu.Unlock()
    if m.handlers == nil {
        m.handlers = make(map[string]func(ReconnectionReason))
    }
    m.handlers[handlerKey] = handler  // Overwrites if exists - idempotent
}

// UnregisterHandler removes a handler (e.g., on resource deletion)
func (m *SSHConnectionManager) UnregisterHandler(handlerKey string) {
    m.handlersMu.Lock()
    defer m.handlersMu.Unlock()
    delete(m.handlers, handlerKey)
}

// notifyHandlers calls all registered handlers with reconnection reason
func (m *SSHConnectionManager) notifyHandlers(reason ReconnectionReason) {
    m.handlersMu.RLock()
    handlers := make([]func(ReconnectionReason), 0, len(m.handlers))
    for _, handler := range m.handlers {
        handlers = append(handlers, handler)
    }
    m.handlersMu.RUnlock()

    log.Info("Notifying event handlers",
        "router", m.config.Address,
        "reason", reason,
        "handlerCount", len(handlers))

    for _, handler := range handlers {
        // Surface the reconnection reason to consumers so they can react appropriately
        go func(h func(ReconnectionReason)) {
            defer func() {
                if r := recover(); r != nil {
                    log.Error(nil, "Handler panic recovered",
                        "router", m.config.Address,
                        "reason", reason,
                        "panic", r)
                }
            }()
            h(reason)
        }(handler)  // Call handlers concurrently with panic recovery
    }
}

// HandlerCount returns how many handlers are registered (for lifecycle decisions)
func (m *SSHConnectionManager) HandlerCount() int {
    m.handlersMu.RLock()
    defer m.handlersMu.RUnlock()
    return len(m.handlers)
}
```

### Registry Management

```go
// NewSSHManagerRegistry binds all managers to the provided long-lived context.
// Callers should pass a process-scoped context (e.g., operator root context), not
// per-reconcile contexts that are cancelled after each event.
func NewSSHManagerRegistry(baseCtx context.Context) *SSHManagerRegistry {
    if baseCtx == nil {
        baseCtx = context.Background()
    }
    return &SSHManagerRegistry{
        managers: make(map[string]*SSHConnectionManager),
        baseCtx:  baseCtx,
    }
}

// Global singleton registry
var defaultRegistry = NewSSHManagerRegistry(context.Background())

// normalizeRouterAddress ensures consistent address format (always includes port)
func normalizeRouterAddress(addr string) string {
    host, port, err := net.SplitHostPort(addr)
    if err != nil {
        // No port specified, add default SSH port
        return net.JoinHostPort(addr, "22")
    }
    return net.JoinHostPort(host, port)
}

// GetManager returns or creates an SSH manager for a router, sharing the registry's long-lived context.
func (r *SSHManagerRegistry) GetManager(config RouterConfig) *SSHConnectionManager {
    r.mu.Lock()

    key := normalizeRouterAddress(config.Address)
    if mgr, exists := r.managers[key]; exists {
        r.mu.Unlock()
        return mgr
    }

    // Create new manager
    mgr := &SSHConnectionManager{
        config: config,
    }

    // Add to registry before releasing lock
    r.managers[key] = mgr
    r.mu.Unlock()

    // Start connection WITHOUT holding the registry lock
    // This prevents blocking other GetManager() calls during the connection timeout window
    if err := mgr.Start(r.baseCtx); err != nil {
        log.Error(err, "Failed to start SSH manager", "router", config.Address)
    }

    return mgr
}

// LookupManager returns an existing SSH manager without creating one
func (r *SSHManagerRegistry) LookupManager(address string) *SSHConnectionManager {
    r.mu.RLock()
    defer r.mu.RUnlock()
    key := normalizeRouterAddress(address)
    return r.managers[key]
}

// RemoveManager removes a manager from the registry after it has been closed
func (r *SSHManagerRegistry) RemoveManager(address string) {
    r.mu.Lock()
    defer r.mu.Unlock()
    key := normalizeRouterAddress(address)
    delete(r.managers, key)
}

// CloseAll closes all SSH managers
func (r *SSHManagerRegistry) CloseAll() error {
    r.mu.Lock()
    defer r.mu.Unlock()

    var errs []error
    for _, mgr := range r.managers {
        if err := mgr.Close(); err != nil {
            errs = append(errs, err)
        }
    }

    if len(errs) > 0 {
        return fmt.Errorf("errors closing SSH managers: %v", errs)
    }
    return nil
}
```

Because each manager derives its lifecycle from the registry’s `baseCtx`, the operator can safely call `GetManager` from transient reconcile loops without risking premature cancellation; shutting down all SSH activity now requires cancelling the registry context (e.g., during operator shutdown) or explicitly invoking `CloseAll()`.

---

## Verification Commands

### Standard Linux Commands (No Script Modifications)

All state verification uses direct SSH command execution:

#### Router Uptime

```bash
cat /proc/uptime
# Output: "12345.67 98765.43"
# First number is uptime in seconds (what we track)
```

**Go wrapper:**
```go
func (m *SSHConnectionManager) GetRouterUptime() (float64, error) {
    output, err := m.Execute("cat /proc/uptime")
    if err != nil {
        return 0, err
    }

    fields := strings.Fields(strings.TrimSpace(output))
    if len(fields) == 0 {
        return 0, fmt.Errorf("invalid uptime output: %s", output)
    }

    uptime, err := strconv.ParseFloat(fields[0], 64)
    if err != nil {
        return 0, fmt.Errorf("failed to parse uptime: %w", err)
    }

    return uptime, nil
}
```

#### Interface Existence

```bash
ip link show wan0.dhcp1
# Exit code 0 = exists
# Exit code non-zero = does not exist
```

**Go wrapper:**
```go
func (m *SSHConnectionManager) InterfaceExists(ifname string) (bool, error) {
    cmd := fmt.Sprintf("ip link show %s", ifname)
    output, err := m.Execute(cmd)
    if err != nil {
        var exitErr *ssh.ExitError
        if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
            return false, nil
        }
        return false, fmt.Errorf("failed to check interface %s: %w (output: %s)",
            ifname, err, strings.TrimSpace(output))
    }
    return true, nil
}
```

> The wrappers use `errors.As` (standard library) to unwrap `*ssh.ExitError` and translate expected `exit status 1` results—such as "interface not found"—into clean `false`/empty values instead of surfacing them as errors. Make sure the implementation imports the `errors` package alongside `strings`/`fmt`.

#### DHCP Client Process

```bash
ps aux | grep '[u]dhcpc.*wan0.dhcp1'
# Output non-empty = running
# Output empty = not running
```

**Go wrapper:**
```go
func (m *SSHConnectionManager) IsUdhcpcRunning(ifname string) (bool, error) {
    // Use [u] trick to avoid matching the grep process itself
    cmd := fmt.Sprintf("ps aux | grep '[u]dhcpc.*%s'", ifname)
    output, err := m.Execute(cmd)
    if err != nil {
        var exitErr *ssh.ExitError
        if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
            return false, nil
        }
        return false, fmt.Errorf("failed to inspect udhcpc processes: %w (output: %s)",
            err, strings.TrimSpace(output))
    }

    return strings.TrimSpace(output) != "", nil
}
```

#### Get Interface MAC Address

```bash
ip link show wan0.dhcp1 | grep 'link/ether' | awk '{print $2}'
# Output: "02:xx:xx:xx:xx:xx"
```

**Go wrapper:**
```go
func (m *SSHConnectionManager) GetInterfaceMAC(ifname string) (string, error) {
    cmd := fmt.Sprintf("ip link show %s | grep 'link/ether' | awk '{print $2}'", ifname)
    output, err := m.Execute(cmd)
    if err != nil {
        return "", err
    }

    mac := strings.TrimSpace(output)
    if mac == "" {
        return "", fmt.Errorf("no MAC address found for interface %s", ifname)
    }

    return mac, nil
}
```

#### Proxy ARP Status

```bash
cat /proc/sys/net/ipv4/conf/wan0.dhcp1/proxy_arp
# Output: "1" = enabled, "0" = disabled
```

**Go wrapper:**
```go
func (m *SSHConnectionManager) IsProxyARPEnabled(ifname string) (bool, error) {
    cmd := fmt.Sprintf("cat /proc/sys/net/ipv4/conf/%s/proxy_arp", ifname)
    output, err := m.Execute(cmd)
    if err != nil {
        return false, err
    }

    value := strings.TrimSpace(output)
    return value == "1", nil
}
```

#### List All Managed Interfaces

```bash
ip link show | grep -E '^[0-9]+: (wan|eth).*\.dhcp[0-9]+'
# Lists all interfaces matching pattern: wan0.dhcp1, eth8.dhcp2, etc.
```

**Go wrapper:**
```go
func (m *SSHConnectionManager) ListManagedInterfaces() ([]string, error) {
    cmd := "ip link show | grep -E '^[0-9]+: (wan|eth).*\\.dhcp[0-9]+' | awk -F': ' '{print $2}' | cut -d'@' -f1"
    output, err := m.Execute(cmd)
    if err != nil {
        var exitErr *ssh.ExitError
        if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
            return []string{}, nil
        }
        return nil, fmt.Errorf("failed to list managed interfaces: %w (output: %s)",
            err, strings.TrimSpace(output))
    }

    var interfaces []string
    for _, line := range strings.Split(output, "\n") {
        line = strings.TrimSpace(line)
        if line != "" {
            interfaces = append(interfaces, line)
        }
    }

    return interfaces, nil
}
```

---

## Error Handling & Edge Cases

### Connection Failures

**Scenario:** SSH connection fails during manager startup

**Handling:**
- Manager starts reconnection loop in background
- Exponential backoff: 1s → 2s → 4s → 8s → max 60s
- Consumers notified of "reconnecting" state
- Manager logs warnings but continues running
- Status shows: `IsReconnecting() == true`

```go
if err := mgr.Start(); err != nil {
    log.Error(err, "Failed to start SSH manager, reconnecting in background")
    // Don't fail application startup, let reconnection happen
}
```

### Network Partitions

**Scenario:** Network partition between operator and router

**Handling:**
- Keep-alive timeout (30s) detects partition
- Connection drop handler triggers event handlers
- Reconnection loop attempts to restore connection
- External consumers handle reconnection events appropriately
- Services remain down until connectivity restored

**Metrics:**
- `ssh_connection_drops_total` increases
- `ssh_reconnection_attempts_total` increases

### False Positive Reboots

**Scenario:** Uptime check temporarily fails, appears like router rebooted

**Handling:**
- Event handlers triggered (they should be idempotent)
- Event emitted for observability
- Metrics track event rate

**Mitigation:**
- Consider adding debounce (wait 5s before notifying)
- Track consecutive failures before declaring reboot

### Stale Connections

**Scenario:** Connection appears up but is actually broken (NAT state expired)

**Handling:**
- Keep-alive command (uptime check) fails
- Connection drop detected within 30s + timeout
- Reconnection triggered automatically

### Concurrent Command Execution

**Scenario:** Multiple consumers execute commands simultaneously on same manager

**Handling:**
- Single SSH connection manager per router (singleton)
- Thread-safe command execution (read lock for connection check)
- Commands serialized automatically by SSH library
- No race conditions between consumers

```go
func (m *SSHConnectionManager) Execute(cmd string) (string, error) {
    m.mu.RLock()  // Read lock for connection check
    if !m.connected {
        m.mu.RUnlock()
        return "", fmt.Errorf("not connected")
    }
    conn := m.conn
    m.mu.RUnlock()

    // Each session is independent, safe for concurrent use
    session, err := conn.NewSession()
    // ...
}
```

### Manager Lifecycle

**Scenario:** All consumers stop using a manager

**Handling:**
- External code checks `HandlerCount()` after unregistering
- If count reaches zero, call `Close()` and `RemoveManager()`
- Manager gracefully shuts down goroutines
- Connection closed cleanly

```go
mgr.UnregisterHandler(handlerKey)
if mgr.HandlerCount() == 0 {
    mgr.Close()
    registry.RemoveManager(routerAddr)
}
```

---

## Security: SSH Host Key Verification

### Overview

By default, the manager falls back to `InsecureIgnoreHostKey()` if no host key callback is configured. This is **not recommended for production** as it's vulnerable to man-in-the-middle attacks.

### Recommended Setup

#### 1. Collect Router Host Keys

First, collect the SSH host keys from your routers:

```bash
# Get host key from router
ssh-keyscan -H 192.168.1.1 > router_known_hosts
```

#### 2. Create ConfigMap with Known Hosts

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ssh-known-hosts
  namespace: kube-system
data:
  known_hosts: |
    |1|base64hash...= ssh-rsa AAAAB3NzaC1yc2EAAAA...
    |1|base64hash...= ecdsa-sha2-nistp256 AAAAE2VjZHNh...
```

```bash
kubectl create configmap ssh-known-hosts \
  --from-file=known_hosts=router_known_hosts \
  -n kube-system
```

#### 3. Load Known Hosts in Application

```go
import (
    "golang.org/x/crypto/ssh/knownhosts"
)

// When creating RouterConfig:
var hostKeyCallback ssh.HostKeyCallback
knownHostsPath := "/etc/ssh-operator/known_hosts"
if _, err := os.Stat(knownHostsPath); err == nil {
    // known_hosts file exists, use it
    hostKeyCallback, err = knownhosts.New(knownHostsPath)
    if err != nil {
        logger.Error(err, "Failed to load known_hosts, falling back to insecure verification")
        hostKeyCallback = nil  // Will trigger InsecureIgnoreHostKey fallback
    } else {
        logger.Info("Using host key verification from known_hosts")
    }
} else {
    logger.Warn("known_hosts file not found, using insecure host key verification",
        "path", knownHostsPath)
}

config := ssh.RouterConfig{
    Address:         "192.168.1.1:22",
    Username:        "root",
    AuthMethod:      ssh.PublicKeys(signer),
    HostKeyCallback: hostKeyCallback,  // Use loaded callback
    Timeout:         30 * time.Second,
}
```

### Host Key Rotation

When router host keys change (e.g., after firmware update):

1. Update the known_hosts file/ConfigMap:
   ```bash
   ssh-keyscan -H 192.168.1.1 > new_known_hosts
   kubectl create configmap ssh-known-hosts \
     --from-file=known_hosts=new_known_hosts \
     -n kube-system \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

2. Restart application to reload the ConfigMap

### Alternative: Per-Router Configuration

For environments with many routers, you can store host keys in per-router configuration:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: router-192-168-1-1-hostkey
  namespace: kube-system
type: Opaque
stringData:
  host_key: "192.168.1.1 ssh-rsa AAAAB3NzaC1yc2EAAAA..."
```

---

## Testing Strategy

### Unit Tests

```go
// Test SSH connection manager
func TestSSHConnectionManager_Connect(t *testing.T) { /* ... */ }
func TestSSHConnectionManager_Reconnect(t *testing.T) { /* ... */ }
func TestSSHConnectionManager_KeepAlive(t *testing.T) { /* ... */ }
func TestSSHConnectionManager_RebootDetection(t *testing.T) { /* ... */ }

// Test command wrappers
func TestGetRouterUptime(t *testing.T) { /* ... */ }
func TestInterfaceExists(t *testing.T) { /* ... */ }
func TestIsUdhcpcRunning(t *testing.T) { /* ... */ }
func TestIsProxyARPEnabled(t *testing.T) { /* ... */ }

// Test registry
func TestSSHManagerRegistry_GetManager(t *testing.T) { /* ... */ }
func TestSSHManagerRegistry_Singleton(t *testing.T) { /* ... */ }
func TestSSHManagerRegistry_CloseAll(t *testing.T) { /* ... */ }

// Test event handlers
func TestRegisterHandler_Idempotent(t *testing.T) { /* ... */ }
func TestUnregisterHandler(t *testing.T) { /* ... */ }
func TestNotifyHandlers_Concurrent(t *testing.T) { /* ... */ }
```

### Mock SSH Server for Testing

```go
// Example mock SSH server for testing
type MockSSHServer struct {
    listener net.Listener
    config   *ssh.ServerConfig
    uptime   float64
}

func (m *MockSSHServer) SetUptime(uptime float64) {
    m.uptime = uptime
}

func (m *MockSSHServer) HandleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
    for req := range requests {
        if req.Type == "exec" {
            cmd := string(req.Payload[4:]) // Skip length prefix
            if strings.Contains(cmd, "/proc/uptime") {
                channel.Write([]byte(fmt.Sprintf("%.2f 0.00\n", m.uptime)))
            }
            channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
            channel.Close()
        }
    }
}
```

### Integration Test Scenarios

1. **Connection establishment and keep-alive**
   - Start manager, verify connection succeeds
   - Wait for multiple keep-alive cycles
   - Verify uptime tracking works

2. **Reboot detection**
   - Start manager with uptime=1000
   - Simulate reboot by setting uptime=10
   - Verify handler called with `ReasonReboot`

3. **Connection drop and reconnection**
   - Start manager, establish connection
   - Kill SSH server
   - Verify handler called with `ReasonConnectionDrop`
   - Restart SSH server
   - Verify automatic reconnection

4. **Concurrent command execution**
   - Start manager
   - Execute 10 commands concurrently
   - Verify all succeed without errors

5. **Manager lifecycle**
   - Create manager, register handlers
   - Unregister all handlers
   - Verify HandlerCount() returns 0
   - Close manager
   - Verify goroutines cleaned up

---

## Implementation Checklist

### Phase 1: Core Infrastructure

- [ ] Create `internal/ssh` package structure
- [ ] Implement `RouterConfig` and core types
- [ ] Implement `SSHConnectionManager`
  - [ ] Connection establishment with host key verification
  - [ ] Keep-alive loop with uptime checking
  - [ ] Reboot detection logic
  - [ ] Automatic reconnection with exponential backoff
  - [ ] Event handler registration system
  - [ ] Graceful shutdown
- [ ] Implement `SSHManagerRegistry`
  - [ ] Singleton per router address
  - [ ] Thread-safe operations
  - [ ] Manager lifecycle management
- [ ] Implement command wrappers
  - [ ] `GetRouterUptime()`
  - [ ] `InterfaceExists()`
  - [ ] `IsUdhcpcRunning()`
  - [ ] `GetInterfaceMAC()`
  - [ ] `IsProxyARPEnabled()`
  - [ ] `ListManagedInterfaces()`
- [ ] Write unit tests
  - [ ] Mock SSH connections
  - [ ] Test reconnection logic
  - [ ] Test uptime detection
  - [ ] Test handler notifications
  - [ ] Test concurrent access
- [ ] Add structured logging throughout
- [ ] Add basic metrics (connection count, drops, reconnects)

### Phase 2: Security & Hardening

- [ ] Implement host key verification
- [ ] Add ConfigMap support for known_hosts
- [ ] Add timeout handling for all operations
- [ ] Add command execution context cancellation
- [ ] Test edge cases (stale connections, network partitions)

### Phase 3: Documentation

- [ ] Document API usage with examples
- [ ] Add godoc comments to all public functions
- [ ] Create usage guide for consumers
- [ ] Document security best practices

---

## Conclusion

This SSH Connection Management specification defines a **robust, reusable infrastructure layer** for maintaining persistent SSH connections to routers:

✅ **Fast detection** - ~40 seconds worst-case via uptime monitoring
✅ **Automatic recovery** - No manual intervention
✅ **No router modifications** - Standard Linux commands
✅ **Thread-safe** - Safe for concurrent use
✅ **Observable** - Logging and event notifications
✅ **Scalable** - Connection pooling per router

This layer can be consumed by any application needing reliable SSH connectivity with automatic failure detection and recovery.

**Next Steps:**
1. Review this specification
2. Begin Phase 1 implementation
3. Write unit tests alongside implementation
4. Create integration layer specification for consumers
