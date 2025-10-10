package ssh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// MockSSHServer provides a test SSH server for unit tests
type MockSSHServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	uptime   float64
	mu       sync.RWMutex
	commands map[string]string // command -> response mapping
	stopped  bool
}

// NewMockSSHServer creates a new mock SSH server
func NewMockSSHServer() (*MockSSHServer, error) {
	// Generate a test host key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	private, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return nil, err
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true, // No authentication for testing
	}
	config.AddHostKey(private)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	server := &MockSSHServer{
		listener: listener,
		config:   config,
		uptime:   1000.0,
		commands: make(map[string]string),
	}

	// Set default command responses
	server.commands["cat /proc/uptime"] = "1000.00 500.00"
	server.commands["ip link show test-interface"] = "2: test-interface: <BROADCAST,MULTICAST,UP,LOWER_UP>"

	go server.serve()

	return server, nil
}

// SetUptime sets the mock uptime value
func (m *MockSSHServer) SetUptime(uptime float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uptime = uptime
	m.commands["cat /proc/uptime"] = fmt.Sprintf("%.2f 0.00", uptime)
}

// SetCommandResponse sets the response for a specific command
func (m *MockSSHServer) SetCommandResponse(cmd, response string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands[cmd] = response
}

// Address returns the server's listen address
func (m *MockSSHServer) Address() string {
	return m.listener.Addr().String()
}

// Close stops the server
func (m *MockSSHServer) Close() error {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	return m.listener.Close()
}

func (m *MockSSHServer) serve() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			m.mu.RLock()
			stopped := m.stopped
			m.mu.RUnlock()
			if stopped {
				return
			}
			continue
		}

		go m.handleConnection(conn)
	}
}

func (m *MockSSHServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, m.config)
	if err != nil {
		return
	}
	defer sshConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}

		go m.handleSession(channel, requests)
	}
}

func (m *MockSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	for req := range requests {
		if req.Type == "exec" {
			// Extract command from payload
			cmdLen := int(req.Payload[3])
			cmd := string(req.Payload[4 : 4+cmdLen])

			m.mu.RLock()
			response, exists := m.commands[cmd]
			m.mu.RUnlock()

			if exists {
				channel.Write([]byte(response + "\n"))
				channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			} else {
				channel.SendRequest("exit-status", false, []byte{0, 0, 0, 1})
			}
			req.Reply(true, nil)
			return
		}
		req.Reply(false, nil)
	}
}

func TestSSHConnectionManager_Connect(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Close()

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	manager := NewSSHConnectionManager(config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = manager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Close()

	if !manager.IsConnected() {
		t.Error("Manager should be connected")
	}

	if manager.IsReconnecting() {
		t.Error("Manager should not be reconnecting")
	}
}

func TestSSHConnectionManager_Execute(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Close()

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	manager := NewSSHConnectionManager(config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = manager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Close()

	// Test command execution
	output, err := manager.Execute("cat /proc/uptime")
	if err != nil {
		t.Fatalf("Failed to execute command: %v", err)
	}

	if output != "1000.00 500.00\n" {
		t.Errorf("Unexpected output: got %q, want %q", output, "1000.00 500.00\n")
	}
}

func TestSSHConnectionManager_RebootDetection(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Close()

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	manager := NewSSHConnectionManager(config)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Set up handler to detect reboot events
	var rebootDetected bool
	var rebootReason ReconnectionReason
	var mu sync.Mutex

	manager.RegisterHandler("test", func(reason ReconnectionReason) {
		mu.Lock()
		defer mu.Unlock()
		rebootDetected = true
		rebootReason = reason
	})

	err = manager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Close()

	// Manually trigger first uptime check to establish baseline
	err = manager.checkConnection()
	if err != nil {
		t.Fatalf("Initial check connection failed: %v", err)
	}

	// Wait a bit to ensure baseline is set
	time.Sleep(50 * time.Millisecond)

	// Simulate reboot by decreasing uptime
	server.SetUptime(10.0)

	// Manually trigger another check for testing
	err = manager.checkConnection()
	if err != nil {
		t.Fatalf("Check connection failed: %v", err)
	}

	// Wait for handler to execute
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	detected := rebootDetected
	reason := rebootReason
	mu.Unlock()

	if !detected {
		t.Error("Reboot should have been detected")
	}

	if reason != ReasonReboot {
		t.Errorf("Expected ReasonReboot, got %v", reason)
	}
}

func TestSSHConnectionManager_HandlerManagement(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Close()

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	manager := NewSSHConnectionManager(config)

	// Test handler registration
	handler1Called := false
	handler2Called := false

	manager.RegisterHandler("handler1", func(reason ReconnectionReason) {
		handler1Called = true
	})

	manager.RegisterHandler("handler2", func(reason ReconnectionReason) {
		handler2Called = true
	})

	if manager.HandlerCount() != 2 {
		t.Errorf("Expected 2 handlers, got %d", manager.HandlerCount())
	}

	// Test handler notification
	manager.notifyHandlers(ReasonReboot)

	// Give handlers time to execute
	time.Sleep(50 * time.Millisecond)

	if !handler1Called {
		t.Error("Handler1 should have been called")
	}

	if !handler2Called {
		t.Error("Handler2 should have been called")
	}

	// Test handler unregistration
	manager.UnregisterHandler("handler1")

	if manager.HandlerCount() != 1 {
		t.Errorf("Expected 1 handler after unregistration, got %d", manager.HandlerCount())
	}

	// Test idempotent registration (overwrite)
	handler2CalledAgain := false
	manager.RegisterHandler("handler2", func(reason ReconnectionReason) {
		handler2CalledAgain = true
	})

	if manager.HandlerCount() != 1 {
		t.Errorf("Expected 1 handler after overwrite, got %d", manager.HandlerCount())
	}

	manager.notifyHandlers(ReasonReboot)
	time.Sleep(50 * time.Millisecond)

	if !handler2CalledAgain {
		t.Error("Overwritten handler should have been called")
	}
}

func TestSSHConnectionManager_CommandWrappers(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Close()

	// Set up command responses
	server.SetCommandResponse("ip link show test-interface", "2: test-interface: <BROADCAST,MULTICAST,UP,LOWER_UP>")
	server.SetCommandResponse("ip link show missing-interface", "") // Will trigger exit code 1
	server.SetCommandResponse("ps aux | grep '[u]dhcpc.*test-interface'", "root  1234  0.0  0.0  udhcpc -i test-interface")
	server.SetCommandResponse("ps aux | grep '[u]dhcpc.*missing-interface'", "")
	server.SetCommandResponse("ip link show test-interface | grep 'link/ether' | awk '{print $2}'", "02:aa:bb:cc:dd:ee")
	server.SetCommandResponse("cat /proc/sys/net/ipv4/conf/test-interface/proxy_arp", "1")

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	manager := NewSSHConnectionManager(config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = manager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Close()

	// Test GetRouterUptime
	uptime, err := manager.GetRouterUptime()
	if err != nil {
		t.Fatalf("GetRouterUptime failed: %v", err)
	}
	if uptime != 1000.0 {
		t.Errorf("Expected uptime 1000.0, got %f", uptime)
	}

	// Test InterfaceExists (positive case)
	exists, err := manager.InterfaceExists("test-interface")
	if err != nil {
		t.Fatalf("InterfaceExists failed: %v", err)
	}
	if !exists {
		t.Error("Interface should exist")
	}

	// Test IsUdhcpcRunning (positive case)
	running, err := manager.IsUdhcpcRunning("test-interface")
	if err != nil {
		t.Fatalf("IsUdhcpcRunning failed: %v", err)
	}
	if !running {
		t.Error("udhcpc should be running")
	}

	// Test IsUdhcpcRunning (negative case)
	running, err = manager.IsUdhcpcRunning("missing-interface")
	if err != nil {
		t.Fatalf("IsUdhcpcRunning failed: %v", err)
	}
	if running {
		t.Error("udhcpc should not be running for missing interface")
	}

	// Test GetInterfaceMAC
	mac, err := manager.GetInterfaceMAC("test-interface")
	if err != nil {
		t.Fatalf("GetInterfaceMAC failed: %v", err)
	}
	if mac != "02:aa:bb:cc:dd:ee" {
		t.Errorf("Expected MAC 02:aa:bb:cc:dd:ee, got %s", mac)
	}

	// Test IsProxyARPEnabled
	enabled, err := manager.IsProxyARPEnabled("test-interface")
	if err != nil {
		t.Fatalf("IsProxyARPEnabled failed: %v", err)
	}
	if !enabled {
		t.Error("Proxy ARP should be enabled")
	}
}