package ssh

import "sync"

// SSHManagerRegistry maintains a singleton manager per router address.
type SSHManagerRegistry struct {
	mu       sync.RWMutex
	managers map[string]*SSHConnectionManager
}

// NewRegistry returns an initialized registry.
func NewRegistry() *SSHManagerRegistry {
	return &SSHManagerRegistry{managers: make(map[string]*SSHConnectionManager)}
}

// GetOrCreate returns an existing manager for the address or constructs a new one.
func (r *SSHManagerRegistry) GetOrCreate(cfg RouterConfig) (*SSHConnectionManager, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if mgr, ok := r.managers[cfg.Address]; ok {
		return mgr, nil
	}

	mgr, err := NewSSHConnectionManager(cfg)
	if err != nil {
		return nil, err
	}
	r.managers[cfg.Address] = mgr
	return mgr, nil
}

// Remove removes a manager from the registry without closing it.
func (r *SSHManagerRegistry) Remove(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.managers, address)
}

// CloseAll closes and removes all managers.
func (r *SSHManagerRegistry) CloseAll() {
	r.mu.Lock()
	managers := make([]*SSHConnectionManager, 0, len(r.managers))
	for addr, mgr := range r.managers {
		managers = append(managers, mgr)
		delete(r.managers, addr)
	}
	r.mu.Unlock()

	for _, mgr := range managers {
		_ = mgr.Close()
	}
}

// Len returns the number of tracked managers.
func (r *SSHManagerRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.managers)
}
