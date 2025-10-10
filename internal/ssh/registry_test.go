package ssh

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHManagerRegistry_GetManager(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	registry := NewSSHManagerRegistry(context.Background())

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// Test getting a new manager
	manager1 := registry.GetManager(config)
	if manager1 == nil {
		t.Fatal("GetManager should return a manager")
	}

	// Test getting the same manager again (should be singleton)
	manager2 := registry.GetManager(config)
	if manager1 != manager2 {
		t.Error("GetManager should return the same instance for the same address")
	}

	// Clean up
	_ = manager1.Close()
	registry.RemoveManager(config.Address)
}

func TestSSHManagerRegistry_NormalizeAddress(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"192.168.1.1", "192.168.1.1:22"},
		{"192.168.1.1:2222", "192.168.1.1:2222"},
		{"example.com", "example.com:22"},
		{"example.com:2222", "example.com:2222"},
	}

	for _, test := range tests {
		result := normalizeRouterAddress(test.input)
		if result != test.expected {
			t.Errorf("normalizeRouterAddress(%q) = %q, want %q", test.input, result, test.expected)
		}
	}
}

func TestSSHManagerRegistry_LookupManager(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	registry := NewSSHManagerRegistry(context.Background())

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// Test lookup for non-existent manager
	manager := registry.LookupManager(config.Address)
	if manager != nil {
		t.Error("LookupManager should return nil for non-existent manager")
	}

	// Create a manager
	createdManager := registry.GetManager(config)

	// Test lookup for existing manager
	foundManager := registry.LookupManager(config.Address)
	if foundManager != createdManager {
		t.Error("LookupManager should return the same manager instance")
	}

	// Clean up
	_ = createdManager.Close()
	registry.RemoveManager(config.Address)
}

func TestSSHManagerRegistry_RemoveManager(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	registry := NewSSHManagerRegistry(context.Background())

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// Create a manager
	manager := registry.GetManager(config)

	// Verify it exists
	found := registry.LookupManager(config.Address)
	if found != manager {
		t.Error("Manager should exist before removal")
	}

	// Remove it
	_ = manager.Close()
	registry.RemoveManager(config.Address)

	// Verify it's gone
	found = registry.LookupManager(config.Address)
	if found != nil {
		t.Error("Manager should not exist after removal")
	}
}

func TestSSHManagerRegistry_CloseAll(t *testing.T) {
	server1, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server 1: %v", err)
	}
	defer func() { _ = server1.Close() }()

	server2, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server 2: %v", err)
	}
	defer func() { _ = server2.Close() }()

	registry := NewSSHManagerRegistry(context.Background())

	config1 := RouterConfig{
		Address:         server1.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	config2 := RouterConfig{
		Address:         server2.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// Create multiple managers
	manager1 := registry.GetManager(config1)
	manager2 := registry.GetManager(config2)

	// Wait for connections to establish
	time.Sleep(100 * time.Millisecond)

	// Verify they're connected
	if !manager1.IsConnected() {
		t.Error("Manager1 should be connected")
	}
	if !manager2.IsConnected() {
		t.Error("Manager2 should be connected")
	}

	// Close all managers
	err = registry.CloseAll()
	if err != nil {
		t.Fatalf("CloseAll failed: %v", err)
	}

	// Verify they're disconnected
	if manager1.IsConnected() {
		t.Error("Manager1 should be disconnected after CloseAll")
	}
	if manager2.IsConnected() {
		t.Error("Manager2 should be disconnected after CloseAll")
	}
}

func TestSSHManagerRegistry_DefaultRegistry(t *testing.T) {
	server, err := NewMockSSHServer()
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer func() { _ = server.Close() }()

	// Save original default registry
	originalRegistry := GetDefaultRegistry()

	// Create a test registry
	testRegistry := NewSSHManagerRegistry(context.Background())
	SetDefaultRegistry(testRegistry)

	config := RouterConfig{
		Address:         server.Address(),
		Username:        "test",
		AuthMethod:      ssh.Password("password"),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// Test convenience functions
	manager := GetManager(config)
	if manager == nil {
		t.Fatal("GetManager should return a manager")
	}

	foundManager := LookupManager(config.Address)
	if foundManager != manager {
		t.Error("LookupManager should return the same manager")
	}

	// Clean up
	_ = manager.Close()
	RemoveManager(config.Address)

	// Verify removal
	foundManager = LookupManager(config.Address)
	if foundManager != nil {
		t.Error("Manager should be removed")
	}

	// Restore original registry
	SetDefaultRegistry(originalRegistry)
}
