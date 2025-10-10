package ssh

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeRunner struct {
	responses map[string][]byte
	err       error
	mu        sync.Mutex
	calls     []string
}

func (f *fakeRunner) Run(ctx context.Context, client *ssh.Client, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, command)
	if f.err != nil {
		return nil, f.err
	}
	if resp, ok := f.responses[command]; ok {
		return resp, nil
	}
	return nil, errors.New("unexpected command")
}

func baseConfig() RouterConfig {
	return RouterConfig{
		Address:    "127.0.0.1:22",
		Username:   "test",
		AuthMethod: ssh.Password("dummy"),
	}
}

func TestParseUptime(t *testing.T) {
	got, err := parseUptime([]byte("123.45 0.00\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 123.45 {
		t.Fatalf("expected 123.45, got %f", got)
	}
}

func TestValidateInterfaceName(t *testing.T) {
	if err := validateInterfaceName("eth0"); err != nil {
		t.Fatalf("expected valid interface, got %v", err)
	}
	if err := validateInterfaceName("invalid iface"); err == nil {
		t.Fatalf("expected error for invalid interface")
	}
}

func TestHandlerLifecycle(t *testing.T) {
	cfg := baseConfig()
	mgr, err := NewSSHConnectionManager(cfg)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	id1 := mgr.RegisterHandler(func(e Event) {
		if e.Reason != ReasonReconnectSuccess {
			t.Fatalf("unexpected reason %s", e.Reason)
		}
		wg.Done()
	})
	if id1 == 0 {
		t.Fatalf("expected non-zero handler id")
	}
	mgr.RegisterHandler(func(Event) { wg.Done() })

	mgr.notifyHandlers(Event{Reason: ReasonReconnectSuccess})

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handlers did not complete")
	}

	mgr.UnregisterHandler(id1)
	if mgr.HandlerCount() != 1 {
		t.Fatalf("expected 1 handler after unregister")
	}
}

func TestRegistryGetOrCreate(t *testing.T) {
	cfg := baseConfig()
	registry := NewRegistry()

	mgr1, err := registry.GetOrCreate(cfg)
	if err != nil {
		t.Fatalf("get manager: %v", err)
	}
	mgr2, err := registry.GetOrCreate(cfg)
	if err != nil {
		t.Fatalf("get manager second time: %v", err)
	}
	if mgr1 != mgr2 {
		t.Fatalf("expected same instance from registry")
	}
	if registry.Len() != 1 {
		t.Fatalf("expected registry length 1, got %d", registry.Len())
	}
}

func TestCommandHelpers(t *testing.T) {
	runner := &fakeRunner{responses: map[string][]byte{
		"cat /proc/uptime": []byte("100.00 0\n"),
		"if [ -d /sys/class/net/eth0 ]; then echo true; else echo false; fi":      []byte("true\n"),
		"if pgrep -x udhcpc >/dev/null 2>&1; then echo true; else echo false; fi": []byte("false\n"),
		"cat /sys/class/net/eth0/address":                                         []byte("aa:bb:cc:dd:ee:ff\n"),
		"cat /proc/sys/net/ipv4/conf/eth0/proxy_arp":                              []byte("1\n"),
		"ls /sys/class/net": []byte("eth0 lo\n"),
	}}
	cfg := baseConfig()
	cfg.Runner = runner
	mgr, err := NewSSHConnectionManager(cfg)
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	mgr.conn = new(ssh.Client)

	uptime, err := mgr.GetRouterUptime(context.Background())
	if err != nil {
		t.Fatalf("GetRouterUptime: %v", err)
	}
	if uptime != 100*time.Second {
		t.Fatalf("unexpected uptime %s", uptime)
	}

	exists, err := mgr.InterfaceExists(context.Background(), "eth0")
	if err != nil || !exists {
		t.Fatalf("InterfaceExists unexpected result: %v %v", exists, err)
	}

	running, err := mgr.IsUdhcpcRunning(context.Background())
	if err != nil {
		t.Fatalf("IsUdhcpcRunning: %v", err)
	}
	if running {
		t.Fatalf("expected udhcpc not running")
	}

	mac, err := mgr.GetInterfaceMAC(context.Background(), "eth0")
	if err != nil {
		t.Fatalf("GetInterfaceMAC: %v", err)
	}
	if mac != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("unexpected mac %s", mac)
	}

	proxyARP, err := mgr.IsProxyARPEnabled(context.Background(), "eth0")
	if err != nil || !proxyARP {
		t.Fatalf("IsProxyARPEnabled unexpected result: %v %v", proxyARP, err)
	}

	ifaces, err := mgr.ListManagedInterfaces(context.Background())
	if err != nil {
		t.Fatalf("ListManagedInterfaces: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("expected 2 interfaces, got %d", len(ifaces))
	}
}
