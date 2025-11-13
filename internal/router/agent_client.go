package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// AgentClient wraps HTTP client with SSH tunnel
type AgentClient struct {
	sshClient     *ssh.Client
	httpClient    *http.Client
	localAddr     string
	localListener net.Listener
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

// LeaseRequest represents a lease allocation request
type LeaseRequest struct {
	Interface  string `json:"interface"`
	WanParent  string `json:"wanParent"`
	MACAddress string `json:"macAddress"`
}

// LeaseResponse represents a lease allocation response
type LeaseResponse struct {
	IPAddress string    `json:"ipAddress"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// LeaseStatus represents lease status
type LeaseStatus struct {
	IPAddress       string    `json:"ipAddress"`
	ExpiresAt       time.Time `json:"expiresAt"`
	RenewalCount    int       `json:"renewalCount"`
	Status          string    `json:"status"`
	InterfaceExists bool      `json:"interfaceExists"`
}

// LeaseListResponse represents lease list response
type LeaseListResponse struct {
	Leases []LeaseStatus `json:"leases"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// NewAgentClient creates SSH tunnel to router agent
func NewAgentClient(ctx context.Context, routerAddr string, sshConfig *ssh.ClientConfig) (*AgentClient, error) {
	// 1. SSH to router
	sshClient, err := ssh.Dial("tcp", routerAddr+":22", sshConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh dial failed: %w", err)
	}

	// 2. Create local listener for tunnel
	localListener, err := net.Listen("tcp", "127.0.0.1:0") // Random port
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("local listen failed: %w", err)
	}

	localAddr := localListener.Addr().String()

	// 3. Create cancellable context for tunnel lifecycle
	tunnelCtx, cancel := context.WithCancel(context.Background())

	client := &AgentClient{
		sshClient:     sshClient,
		localAddr:     localAddr,
		localListener: localListener,
		ctx:           tunnelCtx,
		cancel:        cancel,
	}

	// 4. Forward connections through SSH tunnel
	client.wg.Add(1)
	go func() {
		defer client.wg.Done()
		defer localListener.Close()

		for {
			select {
			case <-tunnelCtx.Done():
				return // Tunnel cancelled
			default:
			}

			// Set accept deadline to periodically check context
			localListener.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))

			// Accept connection on localhost
			localConn, err := localListener.Accept()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // Deadline exceeded, check context again
				}
				return // Listener closed
			}

			// Check if context is cancelled before forwarding
			if tunnelCtx.Err() != nil {
				localConn.Close()
				return
			}

			// Dial agent on router's localhost through SSH
			remoteConn, err := sshClient.Dial("tcp", "127.0.0.1:8692")
			if err != nil {
				localConn.Close()
				continue
			}

			// Bidirectional copy
			go copyConn(localConn, remoteConn)
		}
	}()

	// 5. HTTP client uses local tunnel endpoint with proper timeouts
	// Timeout budget breakdown for DHCP operations:
	//   - Dial (SSH tunnel):           5s  (local connection, should be fast)
	//   - Request write:               5s  (small JSON payload)
	//   - DHCP operation on agent:    60s  (DISCOVER/REQUEST/ACK sequence)
	//   - Response read:               5s  (small JSON response)
	//   - Total with buffer:         120s  (2 minutes)
	client.httpClient = &http.Client{
		Timeout: 120 * time.Second, // Overall request timeout
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,  // SSH tunnel connection (local)
				KeepAlive: 30 * time.Second, // Keep tunnel connections alive
			}).DialContext,
			ResponseHeaderTimeout: 90 * time.Second, // Wait for agent to complete DHCP + send headers
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          10,
			MaxIdleConnsPerHost:   2,
		},
	}

	return client, nil
}

// copyConn bidirectionally copies data between two connections
func copyConn(local, remote net.Conn) {
	defer local.Close()
	defer remote.Close()

	// Bidirectional copy with proper synchronization
	// Use WaitGroup to avoid premature connection closure causing
	// "use of closed connection" errors in the other goroutine
	var wg sync.WaitGroup
	wg.Add(2)

	// Local → Remote
	go func() {
		defer wg.Done()
		io.Copy(remote, local)
		// Signal remote that we're done writing
		if conn, ok := remote.(*net.TCPConn); ok {
			conn.CloseWrite()
		}
	}()

	// Remote → Local
	go func() {
		defer wg.Done()
		io.Copy(local, remote)
		// Signal local that we're done writing
		if conn, ok := local.(*net.TCPConn); ok {
			conn.CloseWrite()
		}
	}()

	// Wait for both directions to complete before closing
	wg.Wait()
}

// AllocateLease calls agent through tunnel to allocate a new lease
func (c *AgentClient) AllocateLease(ctx context.Context, iface, wanParent, macAddr string) (string, error) {
	body, err := json.Marshal(LeaseRequest{
		Interface:  iface,
		WanParent:  wanParent,
		MACAddress: macAddr,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("http://%s/leases", c.localAddr)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errResp ErrorResponse
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("agent returned %d: %s", resp.StatusCode, errResp.Error)
	}

	var result LeaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response failed: %w", err)
	}

	return result.IPAddress, nil
}

// ListLeases lists all active leases
func (c *AgentClient) ListLeases(ctx context.Context) ([]LeaseStatus, error) {
	url := fmt.Sprintf("http://%s/leases", c.localAddr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp ErrorResponse
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("agent returned %d: %s", resp.StatusCode, errResp.Error)
	}

	var result LeaseListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	return result.Leases, nil
}

// GetLease gets lease status for an interface
func (c *AgentClient) GetLease(ctx context.Context, iface string) (*LeaseStatus, error) {
	url := fmt.Sprintf("http://%s/leases/%s", c.localAddr, iface)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		var errResp ErrorResponse
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("agent returned %d: %s", resp.StatusCode, errResp.Error)
	}

	var status LeaseStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	return &status, nil
}

// ReleaseLease releases a lease
func (c *AgentClient) ReleaseLease(ctx context.Context, iface string) error {
	url := fmt.Sprintf("http://%s/leases/%s", c.localAddr, iface)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("agent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Already released
		return nil
	}

	if resp.StatusCode != http.StatusNoContent {
		var errResp ErrorResponse
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, errResp.Error)
	}

	return nil
}

// Close tears down SSH tunnel
func (c *AgentClient) Close() error {
	// 1. Cancel tunnel context to stop accepting new connections
	c.cancel()

	// 2. Wait for tunnel goroutine to finish
	c.wg.Wait()

	// 3. Close the local listener (already closed by goroutine)
	// c.localListener.Close()

	// 4. Close SSH connection (this will close all forwarded connections)
	return c.sshClient.Close()
}
