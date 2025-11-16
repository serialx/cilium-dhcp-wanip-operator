package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"serialx.net/cilium-dhcp-wanip-operator/internal/agent/api"
	"serialx.net/cilium-dhcp-wanip-operator/internal/agent/dhcp"
	"serialx.net/cilium-dhcp-wanip-operator/internal/agent/lease"
	"serialx.net/cilium-dhcp-wanip-operator/internal/agent/network"
)

// Agent is the main DHCP WAN agent
type Agent struct {
	store          *lease.Store
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	httpServer     *http.Server
	renewalWg      sync.WaitGroup
	renewalCancels map[string]context.CancelFunc // interface -> cancel func
	renewalMu      sync.Mutex
	logger         *slog.Logger
}

// Config contains agent configuration
type Config struct {
	ListenAddr string
	StateDir   string
	Logger     *slog.Logger
}

// New creates a new agent
func New(cfg *Config) (*Agent, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	store, err := lease.NewStore(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create lease store: %w", err)
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	a := &Agent{
		store:          store,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		renewalCancels: make(map[string]context.CancelFunc),
		logger:         cfg.Logger,
	}

	// Create HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /leases", a.handleAllocateLease)
	mux.HandleFunc("GET /leases", a.handleListLeases)
	mux.HandleFunc("GET /leases/{interface}", a.handleGetLease)
	mux.HandleFunc("DELETE /leases/{interface}", a.handleReleaseLease)
	mux.HandleFunc("GET /health", a.handleHealth)

	a.httpServer = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	return a, nil
}

// Start starts the agent and loads existing leases
func (a *Agent) Start(ctx context.Context) error {
	a.logger.Info("starting DHCP WAN agent")

	// Verify and restore existing leases
	a.verifyLeases(ctx)

	// Start HTTP server
	go func() {
		a.logger.Info("starting HTTP server", "addr", a.httpServer.Addr)
		if err := a.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.logger.Error("HTTP server failed", "error", err)
		}
	}()

	return nil
}

// Shutdown gracefully shuts down the agent
func (a *Agent) Shutdown(ctx context.Context) error {
	a.logger.Info("shutting down gracefully")

	// Stop accepting new requests
	if err := a.httpServer.Shutdown(ctx); err != nil {
		a.logger.Error("HTTP server shutdown failed", "error", err)
	}

	// Cancel all renewal goroutines
	a.shutdownCancel()

	// Wait for renewal goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		a.renewalWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		a.logger.Info("all renewal goroutines stopped")
	case <-time.After(30 * time.Second):
		a.logger.Warn("shutdown timeout, some renewal goroutines may not have stopped")
	}

	a.logger.Info("shutdown complete")
	return nil
}

// handleAllocateLease handles POST /leases
func (a *Agent) handleAllocateLease(w http.ResponseWriter, r *http.Request) {
	var req api.LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	// Validate request
	if req.Interface == "" || req.WanParent == "" || req.MACAddress == "" {
		a.sendError(w, http.StatusBadRequest, "interface, wanParent, and macAddress are required")
		return
	}

	mac, err := net.ParseMAC(req.MACAddress)
	if err != nil {
		a.sendError(w, http.StatusBadRequest, fmt.Sprintf("invalid MAC address: %v", err))
		return
	}

	// Acquire per-interface lock
	if !a.store.AcquireInflight(req.Interface) {
		a.sendError(w, http.StatusConflict, fmt.Sprintf("operation already in progress for interface %s", req.Interface))
		return
	}
	defer a.store.ReleaseInflight(req.Interface)

	// Check if lease already exists
	if _, exists := a.store.Get(req.Interface); exists {
		a.sendError(w, http.StatusConflict, fmt.Sprintf("interface %s already has active lease", req.Interface))
		return
	}

	// Allocate lease
	leaseInfo, err := a.allocateLease(r.Context(), req.Interface, req.WanParent, mac)
	if err != nil {
		a.logger.Error("failed to allocate lease",
			"interface", req.Interface,
			"error", err)
		a.sendError(w, http.StatusServiceUnavailable, fmt.Sprintf("failed to allocate lease: %v", err))
		return
	}

	resp := api.LeaseResponse{
		IPAddress: leaseInfo.IPAddress.String(),
		ExpiresAt: leaseInfo.ExpiresAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListLeases handles GET /leases
func (a *Agent) handleListLeases(w http.ResponseWriter, r *http.Request) {
	leases := a.store.List()

	statuses := make([]api.LeaseStatus, 0, len(leases))
	for _, l := range leases {
		statuses = append(statuses, api.LeaseStatus{
			IPAddress:       l.IPAddress.String(),
			ExpiresAt:       l.ExpiresAt,
			RenewalCount:    l.RenewalCount,
			Status:          l.Status,
			InterfaceExists: l.InterfaceExists,
		})
	}

	resp := api.LeaseListResponse{
		Leases: statuses,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleGetLease handles GET /leases/{interface}
func (a *Agent) handleGetLease(w http.ResponseWriter, r *http.Request) {
	iface := r.PathValue("interface")

	l, exists := a.store.Get(iface)
	if !exists {
		a.sendError(w, http.StatusNotFound, fmt.Sprintf("lease not found: %s", iface))
		return
	}

	status := api.LeaseStatus{
		IPAddress:       l.IPAddress.String(),
		ExpiresAt:       l.ExpiresAt,
		RenewalCount:    l.RenewalCount,
		Status:          l.Status,
		InterfaceExists: l.InterfaceExists,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleReleaseLease handles DELETE /leases/{interface}
func (a *Agent) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	iface := r.PathValue("interface")

	// Acquire per-interface lock
	if !a.store.AcquireInflight(iface) {
		a.sendError(w, http.StatusConflict, fmt.Sprintf("operation already in progress for interface %s", iface))
		return
	}
	defer a.store.ReleaseInflight(iface)

	l, exists := a.store.Get(iface)
	if !exists {
		a.sendError(w, http.StatusNotFound, fmt.Sprintf("lease not found: %s", iface))
		return
	}

	// Release lease
	if err := a.releaseLease(r.Context(), l); err != nil {
		a.logger.Error("failed to release lease",
			"interface", iface,
			"error", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to release lease: %v", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleHealth handles GET /health
func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	select {
	case <-a.shutdownCtx.Done():
		w.WriteHeader(http.StatusServiceUnavailable)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// sendError sends an error response
func (a *Agent) sendError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: message})
}

// allocateLease performs the full lease allocation process
func (a *Agent) allocateLease(ctx context.Context, iface, wanParent string, mac net.HardwareAddr) (*lease.Lease, error) {
	// 1. Create macvlan interface
	a.logger.Info("creating macvlan interface",
		"interface", iface,
		"parent", wanParent,
		"mac", mac.String())

	if err := network.CreateMacvlan(iface, wanParent, mac); err != nil {
		return nil, fmt.Errorf("failed to create macvlan: %w", err)
	}

	// Clean up on failure
	var success bool
	defer func() {
		if !success {
			_ = network.DeleteMacvlan(iface)
		}
	}()

	// 2. Acquire DHCP lease
	a.logger.Info("acquiring DHCP lease", "interface", iface)
	dhcpClient := dhcp.NewClient(iface, mac)

	dhcpCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	leaseInfo, err := dhcpClient.AcquireLease(dhcpCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire DHCP lease: %w", err)
	}

	a.logger.Info("DHCP lease acquired",
		"interface", iface,
		"ip", leaseInfo.IPAddress.String(),
		"leaseTime", leaseInfo.LeaseTime)

	// 3. Send gratuitous ARP
	a.logger.Info("sending gratuitous ARP", "interface", iface, "ip", leaseInfo.IPAddress.String())
	if err := network.SendGratuitousARP(iface, leaseInfo.IPAddress, mac, 3); err != nil {
		return nil, fmt.Errorf("gratuitous ARP failed: %w", err)
	}

	// 4. Configure network settings
	a.logger.Info("configuring network settings", "interface", iface)
	if err := network.SetupInterface(iface, leaseInfo.IPAddress, leaseInfo.SubnetMask); err != nil {
		return nil, fmt.Errorf("network setup failed: %w", err)
	}

	// 5. Create and store lease
	var offerBytes, ackBytes []byte
	if leaseInfo.Offer != nil {
		offerBytes = leaseInfo.Offer.ToBytes()
	}
	if leaseInfo.ACK != nil {
		ackBytes = leaseInfo.ACK.ToBytes()
	}

	l := &lease.Lease{
		Interface:       iface,
		WanParent:       wanParent,
		IPAddress:       leaseInfo.IPAddress,
		DHCPServerIP:    leaseInfo.DHCPServerIP,
		MACAddress:      mac,
		ExpiresAt:       time.Now().Add(leaseInfo.LeaseTime),
		LeaseTime:       leaseInfo.LeaseTime,
		RenewalCount:    0,
		Status:          lease.StatusActive,
		InterfaceExists: true,
		CreatedAt:       time.Now(),
		DHCPOffer:       offerBytes,
		DHCPAck:         ackBytes,
	}

	if err := a.store.Set(l); err != nil {
		return nil, fmt.Errorf("failed to save lease: %w", err)
	}

	// 6. Start renewal loop
	a.startRenewalLoop(l)

	success = true
	return l, nil
}

// releaseLease releases a lease and cleans up
func (a *Agent) releaseLease(ctx context.Context, l *lease.Lease) error {
	a.logger.Info("releasing lease",
		"interface", l.Interface,
		"ip", l.IPAddress.String())

	// Stop renewal goroutine first
	a.stopRenewalLoop(l.Interface)

	// Send DHCP RELEASE
	dhcpClient := dhcp.NewClient(l.Interface, l.MACAddress)
	releaseCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := dhcpClient.ReleaseLease(releaseCtx, l.IPAddress, l.DHCPServerIP); err != nil {
		a.logger.Warn("failed to send DHCP RELEASE", "error", err)
		// Continue cleanup even if RELEASE fails
	}

	// Clean up network configuration
	if err := network.CleanupInterface(l.Interface, l.IPAddress); err != nil {
		a.logger.Warn("failed to cleanup network configuration", "error", err)
	}

	// Delete macvlan interface
	if err := network.DeleteMacvlan(l.Interface); err != nil {
		a.logger.Warn("failed to delete macvlan interface", "error", err)
	}

	// Remove from store
	if err := a.store.Delete(l.Interface); err != nil {
		return fmt.Errorf("failed to delete lease from store: %w", err)
	}

	return nil
}

// verifyLeases verifies and restores leases after agent restart
func (a *Agent) verifyLeases(ctx context.Context) {
	leases := a.store.List()

	for _, l := range leases {
		a.logger.Info("verifying lease", "interface", l.Interface, "ip", l.IPAddress.String())

		// Check if interface exists
		exists, err := a.store.VerifyInterfaceExists(l.Interface)
		if err != nil {
			a.logger.Error("failed to check interface", "interface", l.Interface, "error", err)
			continue
		}

		if !exists {
			// Interface missing (router rebooted)
			a.logger.Warn("interface missing after reboot",
				"interface", l.Interface,
				"ip", l.IPAddress.String())
			l.Status = lease.StatusStale
			l.InterfaceExists = false
			_ = a.store.Set(l)
			continue
		}

		l.InterfaceExists = true

		// Try to renew lease to verify it's still valid
		dhcpClient := dhcp.NewClient(l.Interface, l.MACAddress)
		renewCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		leaseInfo, err := dhcpClient.RenewLease(renewCtx, l.IPAddress, l.DHCPServerIP, l.DHCPOffer, l.DHCPAck)
		cancel()

		if err != nil {
			a.logger.Warn("lease renewal failed after reboot, marking stale",
				"interface", l.Interface,
				"ip", l.IPAddress.String(),
				"error", err)
			l.Status = lease.StatusStale
			_ = a.store.Set(l)
			continue
		}

		// Lease verified and renewed successfully
		l.Status = lease.StatusActive
		l.ExpiresAt = time.Now().Add(leaseInfo.LeaseTime)
		l.LeaseTime = leaseInfo.LeaseTime
		l.RenewalCount++

		// Update saved Offer and ACK for next renewal
		if leaseInfo.Offer != nil {
			l.DHCPOffer = leaseInfo.Offer.ToBytes()
		}
		if leaseInfo.ACK != nil {
			l.DHCPAck = leaseInfo.ACK.ToBytes()
		}

		_ = a.store.Set(l)

		a.logger.Info("lease verified and renewed successfully",
			"interface", l.Interface,
			"ip", l.IPAddress.String(),
			"expiresAt", l.ExpiresAt)

		// Restart renewal loop
		a.startRenewalLoop(l)
	}
}

// stopRenewalLoop stops the renewal goroutine for a lease
func (a *Agent) stopRenewalLoop(iface string) {
	a.renewalMu.Lock()
	defer a.renewalMu.Unlock()

	if cancel, exists := a.renewalCancels[iface]; exists {
		a.logger.Info("cancelling renewal goroutine", "interface", iface)
		cancel()
		delete(a.renewalCancels, iface)
	}
}

// startRenewalLoop starts background renewal for a lease
func (a *Agent) startRenewalLoop(l *lease.Lease) {
	// Create per-lease context for cancellation
	renewalCtx, cancel := context.WithCancel(a.shutdownCtx)

	// Store cancel function
	a.renewalMu.Lock()
	a.renewalCancels[l.Interface] = cancel
	a.renewalMu.Unlock()

	a.renewalWg.Add(1)
	go func() {
		defer a.renewalWg.Done()
		defer func() {
			// Clean up cancel function when goroutine exits
			a.renewalMu.Lock()
			delete(a.renewalCancels, l.Interface)
			a.renewalMu.Unlock()
		}()

		// Use timer instead of ticker for dynamic intervals
		nextRenewal := l.LeaseTime / 2
		timer := time.NewTimer(nextRenewal)
		defer timer.Stop()

		backoff := time.Minute
		maxBackoff := 15 * time.Minute

		a.logger.Info("starting renewal loop",
			"interface", l.Interface,
			"nextRenewal", nextRenewal)

		for {
			select {
			case <-timer.C:
				// Try unicast RENEW first
				renewCtx, cancel := context.WithTimeout(renewalCtx, 30*time.Second)
				err := a.renewLease(renewCtx, l)
				cancel()

				if err != nil {
					a.logger.Warn("RENEW failed, trying REBIND",
						"interface", l.Interface,
						"error", err)

					// Fallback to broadcast REBIND
					rebindCtx, rebindCancel := context.WithTimeout(renewalCtx, 30*time.Second)
					err := a.rebindLease(rebindCtx, l)
					rebindCancel()

					if err != nil {
						a.logger.Error("REBIND failed, will retry with backoff",
							"interface", l.Interface,
							"error", err,
							"backoff", backoff)

						// Mark as stale
						l.Status = lease.StatusStale
						_ = a.store.Set(l)

						// Exponential backoff
						timer.Reset(backoff)
						backoff = backoff * 2
						if backoff > maxBackoff {
							backoff = maxBackoff
						}
						continue
					}
				}

				// Reset backoff and timer on success
				backoff = time.Minute
				timer.Reset(l.LeaseTime / 2)
				a.logger.Info("lease renewed",
					"interface", l.Interface,
					"expiresAt", l.ExpiresAt)

			case <-renewalCtx.Done():
				a.logger.Info("stopping renewal goroutine", "interface", l.Interface)
				return
			}
		}
	}()
}

// renewLease performs unicast RENEW
func (a *Agent) renewLease(ctx context.Context, l *lease.Lease) error {
	dhcpClient := dhcp.NewClient(l.Interface, l.MACAddress)
	leaseInfo, err := dhcpClient.RenewLease(ctx, l.IPAddress, l.DHCPServerIP, l.DHCPOffer, l.DHCPAck)
	if err != nil {
		return err
	}

	// Update lease with renewed information
	l.ExpiresAt = time.Now().Add(leaseInfo.LeaseTime)
	l.LeaseTime = leaseInfo.LeaseTime
	l.RenewalCount++
	l.Status = lease.StatusActive

	// Update saved Offer and ACK for next renewal
	if leaseInfo.Offer != nil {
		l.DHCPOffer = leaseInfo.Offer.ToBytes()
	}
	if leaseInfo.ACK != nil {
		l.DHCPAck = leaseInfo.ACK.ToBytes()
	}

	return a.store.Set(l)
}

// rebindLease performs broadcast REBIND
func (a *Agent) rebindLease(ctx context.Context, l *lease.Lease) error {
	dhcpClient := dhcp.NewClient(l.Interface, l.MACAddress)
	leaseInfo, err := dhcpClient.RebindLease(ctx, l.IPAddress)
	if err != nil {
		return err
	}

	// Update lease
	l.ExpiresAt = time.Now().Add(leaseInfo.LeaseTime)
	l.LeaseTime = leaseInfo.LeaseTime
	l.DHCPServerIP = leaseInfo.DHCPServerIP // May have changed
	l.RenewalCount++
	l.Status = lease.StatusActive

	// Update saved Offer and ACK for next renewal
	if leaseInfo.Offer != nil {
		l.DHCPOffer = leaseInfo.Offer.ToBytes()
	}
	if leaseInfo.ACK != nil {
		l.DHCPAck = leaseInfo.ACK.ToBytes()
	}

	return a.store.Set(l)
}
