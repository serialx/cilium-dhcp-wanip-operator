package ssh

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestCommands_InterfaceExists(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up responses
	server.SetCommandResponse("ip link show existing-interface", "2: existing-interface: <BROADCAST,MULTICAST,UP,LOWER_UP>")

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
	defer func() { _ = manager.Close() }()

	// Test existing interface
	exists, err := manager.InterfaceExists("existing-interface")
	if err != nil {
		t.Fatalf("InterfaceExists failed: %v", err)
	}
	if !exists {
		t.Error("Interface should exist")
	}

	// Test non-existing interface (will return exit code 1)
	exists, err = manager.InterfaceExists("non-existing-interface")
	if err != nil {
		t.Fatalf("InterfaceExists should handle non-existing interface gracefully: %v", err)
	}
	if exists {
		t.Error("Interface should not exist")
	}
}

func TestCommands_IsUdhcpcRunning(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up responses
	server.SetCommandResponse("ps aux | grep '[u]dhcpc.*running-interface'", "root  1234  0.0  0.0  udhcpc -i running-interface")

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
	defer func() { _ = manager.Close() }()

	// Test running udhcpc
	running, err := manager.IsUdhcpcRunning("running-interface")
	if err != nil {
		t.Fatalf("IsUdhcpcRunning failed: %v", err)
	}
	if !running {
		t.Error("udhcpc should be running")
	}

	// Test non-running udhcpc
	running, err = manager.IsUdhcpcRunning("not-running-interface")
	if err != nil {
		t.Fatalf("IsUdhcpcRunning should handle non-running case gracefully: %v", err)
	}
	if running {
		t.Error("udhcpc should not be running")
	}
}

func TestCommands_GetInterfaceMAC(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up responses
	server.SetCommandResponse("ip link show test-interface | grep 'link/ether' | awk '{print $2}'", "02:aa:bb:cc:dd:ee")

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
	defer func() { _ = manager.Close() }()

	mac, err := manager.GetInterfaceMAC("test-interface")
	if err != nil {
		t.Fatalf("GetInterfaceMAC failed: %v", err)
	}

	expected := "02:aa:bb:cc:dd:ee"
	if mac != expected {
		t.Errorf("Expected MAC %s, got %s", expected, mac)
	}
}

func TestCommands_IsProxyARPEnabled(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up responses
	server.SetCommandResponse("cat /proc/sys/net/ipv4/conf/enabled-interface/proxy_arp", "1")
	server.SetCommandResponse("cat /proc/sys/net/ipv4/conf/disabled-interface/proxy_arp", "0")

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
	defer func() { _ = manager.Close() }()

	// Test enabled proxy ARP
	enabled, err := manager.IsProxyARPEnabled("enabled-interface")
	if err != nil {
		t.Fatalf("IsProxyARPEnabled failed: %v", err)
	}
	if !enabled {
		t.Error("Proxy ARP should be enabled")
	}

	// Test disabled proxy ARP
	enabled, err = manager.IsProxyARPEnabled("disabled-interface")
	if err != nil {
		t.Fatalf("IsProxyARPEnabled failed: %v", err)
	}
	if enabled {
		t.Error("Proxy ARP should be disabled")
	}
}

func TestCommands_ListManagedInterfaces(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up response with multiple interfaces
	response := "wan0.dhcp1\nwan0.dhcp2\neth8.dhcp1"
	server.SetCommandResponse("ip link show | grep -E '^[0-9]+: (wan|eth).*\\.dhcp[0-9]+' | awk -F': ' '{print $2}' | cut -d'@' -f1", response)

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
	defer func() { _ = manager.Close() }()

	interfaces, err := manager.ListManagedInterfaces()
	if err != nil {
		t.Fatalf("ListManagedInterfaces failed: %v", err)
	}

	expected := []string{"wan0.dhcp1", "wan0.dhcp2", "eth8.dhcp1"}
	if len(interfaces) != len(expected) {
		t.Errorf("Expected %d interfaces, got %d", len(expected), len(interfaces))
	}

	for i, iface := range interfaces {
		if i >= len(expected) || iface != expected[i] {
			t.Errorf("Expected interface %s at position %d, got %s", expected[i], i, iface)
		}
	}
}

func TestCommands_GetInterfaceIP(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up response
	server.SetCommandResponse("ip addr show test-interface | grep 'inet ' | awk '{print $2}' | cut -d'/' -f1", "192.168.1.100")

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
	defer func() { _ = manager.Close() }()

	ip, err := manager.GetInterfaceIP("test-interface")
	if err != nil {
		t.Fatalf("GetInterfaceIP failed: %v", err)
	}

	expected := "192.168.1.100"
	if ip != expected {
		t.Errorf("Expected IP %s, got %s", expected, ip)
	}
}

func TestCommands_IsInterfaceUp(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Set up responses (up interface will succeed, down interface will fail)
	server.SetCommandResponse("ip link show up-interface | grep -q 'state UP'", "")

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
	defer func() { _ = manager.Close() }()

	// Test interface that is up
	up, err := manager.IsInterfaceUp("up-interface")
	if err != nil {
		t.Fatalf("IsInterfaceUp failed: %v", err)
	}
	if !up {
		t.Error("Interface should be up")
	}

	// Test interface that is down (command will return exit code 1)
	up, err = manager.IsInterfaceUp("down-interface")
	if err != nil {
		t.Fatalf("IsInterfaceUp should handle down interface gracefully: %v", err)
	}
	if up {
		t.Error("Interface should be down")
	}
}
