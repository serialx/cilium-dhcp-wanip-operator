package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/klog/v2"
)

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

// NewSSHConnectionManager creates a new SSH connection manager
func NewSSHConnectionManager(config RouterConfig) *SSHConnectionManager {
	return &SSHConnectionManager{
		config: config,
	}
}

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
		klog.Warningf("Using insecure host key verification - not recommended for production (router: %s)", m.config.Address)
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	}

	timeout := m.config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second // Default timeout when not supplied
	}

	sshConfig := &ssh.ClientConfig{
		User:            m.config.Username,
		Auth:            []ssh.AuthMethod{m.config.AuthMethod},
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}

	conn, err := ssh.Dial("tcp", m.config.Address, sshConfig)
	if err != nil {
		return fmt.Errorf("SSH dial failed: %w", err)
	}

	m.conn = conn
	m.connected = true

	// Preserve lastUptime so the next keep-alive can detect reboots after reconnects

	klog.Infof("SSH connection established (router: %s)", m.config.Address)
	return nil
}

// Close gracefully shuts down the SSH connection
func (m *SSHConnectionManager) Close() error {
	// Check if cancel exists before calling (prevents panic if Close() called before Start())
	if m.cancel != nil {
		m.cancel() // Signal all goroutines to stop
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

// keepAliveLoop runs the keep-alive check every 30 seconds
func (m *SSHConnectionManager) keepAliveLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.checkConnection(); err != nil {
				klog.Errorf("Keep-alive check failed (router: %s): %v", m.config.Address, err)
				m.handleConnectionDrop()
				return // Exit loop, reconnection will start new loop
			}

		case <-m.ctx.Done():
			return
		}
	}
}

// checkConnection verifies the connection and detects reboots
func (m *SSHConnectionManager) checkConnection() error {
	const maxUptimeJump = 86400.0 // 1 day in seconds - detect suspicious jumps

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
			klog.Infof("Router reboot detected (router: %s, previousUptime: %.2f, currentUptime: %.2f)",
				m.config.Address, lastUptime, uptime)

			// Trigger handlers via notification
			m.notifyHandlers(ReasonReboot)
		} else if uptime-lastUptime > maxUptimeJump {
			// Suspicious jump - possible counter reset or time manipulation
			klog.Warningf("Suspicious uptime jump detected (router: %s, previousUptime: %.2f, currentUptime: %.2f, jump: %.2f)",
				m.config.Address, lastUptime, uptime, uptime-lastUptime)
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
		return // Already handling or reconnecting
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

	klog.Warningf("SSH connection dropped (router: %s)", m.config.Address)

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
			klog.Infof("Attempting to reconnect (router: %s, backoff: %v)", m.config.Address, backoff)

			if err := m.connect(); err != nil {
				klog.Errorf("Reconnection attempt failed (router: %s): %v", m.config.Address, err)

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

			klog.Infof("Reconnection successful (router: %s)", m.config.Address)

			// Restart keep-alive loop
			m.wg.Add(1)
			go m.keepAliveLoop()
			return
		}
	}
}

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

// RegisterHandler registers a handler for connection events (idempotent)
// handlerKey should be a unique identifier (e.g., "namespace/name" for claims)
func (m *SSHConnectionManager) RegisterHandler(handlerKey string, handler func(ReconnectionReason)) {
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()
	if m.handlers == nil {
		m.handlers = make(map[string]func(ReconnectionReason))
	}
	m.handlers[handlerKey] = handler // Overwrites if exists - idempotent
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

	klog.Infof("Notifying event handlers (router: %s, reason: %s, handlerCount: %d)",
		m.config.Address, reason, len(handlers))

	for _, handler := range handlers {
		// Surface the reconnection reason to consumers so they can react appropriately
		go func(h func(ReconnectionReason)) {
			defer func() {
				if r := recover(); r != nil {
					klog.Errorf("Handler panic recovered (router: %s, reason: %s, panic: %v)",
						m.config.Address, reason, r)
				}
			}()
			h(reason)
		}(handler) // Call handlers concurrently with panic recovery
	}
}

// HandlerCount returns how many handlers are registered (for lifecycle decisions)
func (m *SSHConnectionManager) HandlerCount() int {
	m.handlersMu.RLock()
	defer m.handlersMu.RUnlock()
	return len(m.handlers)
}

// GetRouterUptime gets the router's uptime in seconds
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