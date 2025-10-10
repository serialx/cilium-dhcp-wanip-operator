package ssh

import (
	"context"
	"fmt"
	"net"
	"sync"

	"k8s.io/klog/v2"
)

// SSHManagerRegistry is a singleton that manages SSH managers per router
type SSHManagerRegistry struct {
	managers map[string]*SSHConnectionManager // Key: router address
	mu       sync.RWMutex
	baseCtx  context.Context // Long-lived context controlling manager lifecycles
}

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
	mgr := NewSSHConnectionManager(config)

	// Add to registry before releasing lock
	r.managers[key] = mgr
	r.mu.Unlock()

	// Start connection WITHOUT holding the registry lock
	// This prevents blocking other GetManager() calls during the connection timeout window
	if err := mgr.Start(r.baseCtx); err != nil {
		klog.Errorf("Failed to start SSH manager (router: %s): %v", config.Address, err)
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

// GetDefaultRegistry returns the global singleton registry
func GetDefaultRegistry() *SSHManagerRegistry {
	return defaultRegistry
}

// SetDefaultRegistry sets the global singleton registry (for testing)
func SetDefaultRegistry(registry *SSHManagerRegistry) {
	defaultRegistry = registry
}

// GetManager is a convenience function that uses the default registry
func GetManager(config RouterConfig) *SSHConnectionManager {
	return defaultRegistry.GetManager(config)
}

// LookupManager is a convenience function that uses the default registry
func LookupManager(address string) *SSHConnectionManager {
	return defaultRegistry.LookupManager(address)
}

// RemoveManager is a convenience function that uses the default registry
func RemoveManager(address string) {
	defaultRegistry.RemoveManager(address)
}

// CloseAll is a convenience function that uses the default registry
func CloseAll() error {
	return defaultRegistry.CloseAll()
}