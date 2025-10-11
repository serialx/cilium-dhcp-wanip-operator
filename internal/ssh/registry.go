package ssh

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// SSHManagerRegistry maintains a singleton manager per router address.
type SSHManagerRegistry struct {
	mu       sync.RWMutex
	managers map[string]map[string]*SSHConnectionManager
}

// NewRegistry returns an initialized registry.
func NewRegistry() *SSHManagerRegistry {
	return &SSHManagerRegistry{managers: make(map[string]map[string]*SSHConnectionManager)}
}

// GetOrCreate returns an existing manager for the address or constructs a new one.
func (r *SSHManagerRegistry) GetOrCreate(cfg RouterConfig) (*SSHConnectionManager, error) {
	key := configIdentity(cfg)

	r.mu.Lock()
	defer r.mu.Unlock()

	if entries, ok := r.managers[cfg.Address]; ok {
		if mgr, ok := entries[key]; ok {
			return mgr, nil
		}
	}

	mgr, err := NewSSHConnectionManager(cfg)
	if err != nil {
		return nil, err
	}
	if r.managers[cfg.Address] == nil {
		r.managers[cfg.Address] = make(map[string]*SSHConnectionManager)
	}
	r.managers[cfg.Address][key] = mgr
	return mgr, nil
}

// Lookup returns the first manager registered for the provided address.
// It returns nil when no manager exists.
func (r *SSHManagerRegistry) Lookup(address string) *SSHConnectionManager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entries, ok := r.managers[address]; ok {
		for _, mgr := range entries {
			return mgr
		}
	}
	return nil
}

// Remove removes a manager from the registry without closing it.
func (r *SSHManagerRegistry) Remove(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.managers, address)
}

// CloseAll closes and removes all managers.
func (r *SSHManagerRegistry) CloseAll() error {
	r.mu.Lock()
	managers := make([]*SSHConnectionManager, 0)
	for addr, entries := range r.managers {
		for key, mgr := range entries {
			managers = append(managers, mgr)
			delete(entries, key)
		}
		delete(r.managers, addr)
	}
	r.mu.Unlock()

	var errs []error
	for _, mgr := range managers {
		if err := mgr.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// Len returns the number of tracked managers.
func (r *SSHManagerRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, entries := range r.managers {
		total += len(entries)
	}
	return total
}

func configIdentity(cfg RouterConfig) string {
	parts := []string{
		cfg.Address,
		cfg.Username,
		cfg.Timeout.String(),
		cfg.KeepAliveInterval.String(),
		cfg.KeepAliveCommand,
		cfg.MaxBackoff.String(),
		cfg.InitialBackoff.String(),
		pointerIdentity(cfg.AuthMethod),
		pointerIdentity(cfg.HostKeyCallback),
		pointerIdentity(cfg.Dial),
		pointerIdentity(cfg.Runner),
	}
	return strings.Join(parts, "|")
}

func pointerIdentity(value interface{}) string {
	if value == nil {
		return "nil"
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return "nil"
	}
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() {
			return fmt.Sprintf("%s:nil", rv.Type().String())
		}
		return pointerIdentity(rv.Elem().Interface())
	case reflect.Func, reflect.Ptr, reflect.Map, reflect.Chan, reflect.UnsafePointer:
		return fmt.Sprintf("%s:%#x", rv.Type().String(), rv.Pointer())
	default:
		return fmt.Sprintf("%T:%#v", value, value)
	}
}
