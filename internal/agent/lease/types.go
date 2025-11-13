package lease

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	// StatusActive indicates the lease is active and renewals are running
	StatusActive = "active"
	// StatusStale indicates the interface is missing or renewal failed
	StatusStale = "stale"
	// StatusExpired indicates the lease has expired
	StatusExpired = "expired"
)

// Lease represents a DHCP lease for a macvlan interface
type Lease struct {
	Interface       string           `json:"interface"`
	WanParent       string           `json:"wanParent"`
	IPAddress       net.IP           `json:"ipAddress"`
	DHCPServerIP    net.IP           `json:"dhcpServerIP"` // Required for unicast renewals
	MACAddress      net.HardwareAddr `json:"macAddress"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	LeaseTime       time.Duration    `json:"leaseTime"`
	RenewalCount    int              `json:"renewalCount"`
	Status          string           `json:"status"`          // "active", "stale", "expired"
	InterfaceExists bool             `json:"interfaceExists"` // true if kernel interface exists
	CreatedAt       time.Time        `json:"createdAt"`
}

// Store manages lease persistence and in-memory state
type Store struct {
	mu         sync.RWMutex
	leases     map[string]*Lease // interface name -> lease
	stateFile  string
	inflightMu sync.Mutex
	inflight   map[string]chan struct{} // Per-interface operation locks
}

// NewStore creates a new lease store
func NewStore(stateDir string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	stateFile := filepath.Join(stateDir, "leases.json")

	store := &Store{
		leases:    make(map[string]*Lease),
		stateFile: stateFile,
		inflight:  make(map[string]chan struct{}),
	}

	// Load existing state
	if err := store.load(); err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	return store, nil
}

// Get retrieves a lease by interface name
func (s *Store) Get(iface string) (*Lease, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lease, exists := s.leases[iface]
	return lease, exists
}

// List returns all leases
func (s *Store) List() []*Lease {
	s.mu.RLock()
	defer s.mu.RUnlock()

	leases := make([]*Lease, 0, len(s.leases))
	for _, lease := range s.leases {
		leases = append(leases, lease)
	}
	return leases
}

// Set stores a lease
func (s *Store) Set(lease *Lease) error {
	s.mu.Lock()
	s.leases[lease.Interface] = lease
	s.mu.Unlock()

	return s.save()
}

// Delete removes a lease
func (s *Store) Delete(iface string) error {
	s.mu.Lock()
	delete(s.leases, iface)
	s.mu.Unlock()

	return s.save()
}

// AcquireInflight acquires a per-interface lock for operations
func (s *Store) AcquireInflight(iface string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()

	if _, exists := s.inflight[iface]; exists {
		return false // Operation already in progress
	}
	s.inflight[iface] = make(chan struct{})
	return true
}

// ReleaseInflight releases a per-interface lock
func (s *Store) ReleaseInflight(iface string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()

	if ch, exists := s.inflight[iface]; exists {
		close(ch)
		delete(s.inflight, iface)
	}
}

// save atomically writes leases to disk
func (s *Store) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.leases, "", "  ")
	s.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("failed to marshal leases: %w", err)
	}

	// Write to temp file first
	tmpFile := s.stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpFile, s.stateFile); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// load reads leases from disk
func (s *Store) load() error {
	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // First run, no state to load
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	var leases map[string]*Lease
	if err := json.Unmarshal(data, &leases); err != nil {
		return fmt.Errorf("failed to unmarshal leases: %w", err)
	}

	s.mu.Lock()
	s.leases = leases
	s.mu.Unlock()

	return nil
}

// VerifyInterfaceExists checks if the interface exists in the kernel
func (s *Store) VerifyInterfaceExists(iface string) (bool, error) {
	_, err := netlink.LinkByName(iface)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return false, nil
		}
		return false, fmt.Errorf("failed to check interface: %w", err)
	}
	return true, nil
}

// UpdateInterfaceStatus updates the interface existence flag
func (s *Store) UpdateInterfaceStatus(iface string, exists bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if lease, ok := s.leases[iface]; ok {
		lease.InterfaceExists = exists
		if !exists {
			lease.Status = StatusStale
		}
	}

	return s.save()
}
