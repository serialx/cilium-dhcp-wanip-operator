//go:build linux

package dhcp

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

// LeaseInfo contains information about a DHCP lease
type LeaseInfo struct {
	IPAddress    net.IP
	DHCPServerIP net.IP
	LeaseTime    time.Duration
	RenewalTime  time.Duration
	RebindTime   time.Duration
}

// Client wraps DHCP operations
type Client struct {
	iface string
	mac   net.HardwareAddr
}

// NewClient creates a new DHCP client for an interface
func NewClient(iface string, mac net.HardwareAddr) *Client {
	return &Client{
		iface: iface,
		mac:   mac,
	}
}

// AcquireLease performs DHCP DISCOVER/REQUEST to obtain a lease
func (c *Client) AcquireLease(ctx context.Context) (*LeaseInfo, error) {
	// Create DHCP client with raw sockets
	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer client.Close()

	// Request lease with timeout
	lease, err := client.Request(ctx)
	if err != nil {
		return nil, fmt.Errorf("DHCP request failed: %w", err)
	}

	if lease.ACK == nil {
		return nil, fmt.Errorf("no ACK received from DHCP server")
	}

	// Extract lease information
	ip := lease.ACK.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("no IP address in DHCP ACK")
	}

	// Get DHCP server IP (required for unicast renewals)
	dhcpServer := lease.ACK.ServerIdentifier()
	if dhcpServer == nil || dhcpServer.IsUnspecified() {
		return nil, fmt.Errorf("no DHCP server identifier in ACK")
	}

	// Get lease time (default to 1 hour if not specified)
	leaseTime := lease.ACK.IPAddressLeaseTime(time.Hour)

	// Get renewal and rebind times (defaults to 50% and 87.5% of lease time)
	renewalTime := lease.ACK.IPAddressRenewalTime(leaseTime / 2)
	rebindTime := lease.ACK.IPAddressRebindingTime(leaseTime * 7 / 8)

	return &LeaseInfo{
		IPAddress:    ip,
		DHCPServerIP: dhcpServer,
		LeaseTime:    leaseTime,
		RenewalTime:  renewalTime,
		RebindTime:   rebindTime,
	}, nil
}

// RenewLease performs unicast DHCP RENEW
func (c *Client) RenewLease(ctx context.Context, currentIP, dhcpServerIP net.IP) (*LeaseInfo, error) {
	// Create DHCP client with raw sockets
	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer client.Close()

	// Create RENEW request
	// CRITICAL: Use raw sockets to send with source IP without binding
	req, err := dhcpv4.NewRequestFromOffer(&dhcpv4.DHCPv4{
		ClientIPAddr: currentIP,
		ClientHWAddr: c.mac,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create RENEW request: %w", err)
	}

	// Set message type to REQUEST (RENEW)
	req.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))

	// Send unicast to DHCP server
	serverAddr := &net.UDPAddr{IP: dhcpServerIP, Port: 67}
	resp, err := client.SendAndRead(ctx, serverAddr, req, nil)
	if err != nil {
		return nil, fmt.Errorf("RENEW request failed: %w", err)
	}

	// Verify ACK
	if resp.MessageType() != dhcpv4.MessageTypeAck {
		return nil, fmt.Errorf("expected ACK, got %s", resp.MessageType())
	}

	// Extract updated lease information
	ip := resp.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		ip = currentIP // Server may not set YourIPAddr in RENEW ACK
	}

	dhcpServer := resp.ServerIdentifier()
	if dhcpServer == nil || dhcpServer.IsUnspecified() {
		dhcpServer = dhcpServerIP // Use existing server IP
	}

	leaseTime := resp.IPAddressLeaseTime(time.Hour)
	renewalTime := resp.IPAddressRenewalTime(leaseTime / 2)
	rebindTime := resp.IPAddressRebindingTime(leaseTime * 7 / 8)

	return &LeaseInfo{
		IPAddress:    ip,
		DHCPServerIP: dhcpServer,
		LeaseTime:    leaseTime,
		RenewalTime:  renewalTime,
		RebindTime:   rebindTime,
	}, nil
}

// RebindLease performs broadcast DHCP REBIND
func (c *Client) RebindLease(ctx context.Context, currentIP net.IP) (*LeaseInfo, error) {
	// Create DHCP client with raw sockets
	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer client.Close()

	// Create REBIND request (broadcast)
	req, err := dhcpv4.NewRequestFromOffer(&dhcpv4.DHCPv4{
		ClientIPAddr: currentIP,
		ClientHWAddr: c.mac,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create REBIND request: %w", err)
	}

	// Set message type to REQUEST (REBIND)
	req.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))

	// Send broadcast
	broadcastAddr := &net.UDPAddr{IP: net.IPv4bcast, Port: 67}
	resp, err := client.SendAndRead(ctx, broadcastAddr, req, nil)
	if err != nil {
		return nil, fmt.Errorf("REBIND request failed: %w", err)
	}

	// Verify ACK
	if resp.MessageType() != dhcpv4.MessageTypeAck {
		return nil, fmt.Errorf("expected ACK, got %s", resp.MessageType())
	}

	// Extract lease information
	ip := resp.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		ip = currentIP
	}

	dhcpServer := resp.ServerIdentifier()
	if dhcpServer == nil || dhcpServer.IsUnspecified() {
		return nil, fmt.Errorf("no DHCP server identifier in REBIND ACK")
	}

	leaseTime := resp.IPAddressLeaseTime(time.Hour)
	renewalTime := resp.IPAddressRenewalTime(leaseTime / 2)
	rebindTime := resp.IPAddressRebindingTime(leaseTime * 7 / 8)

	return &LeaseInfo{
		IPAddress:    ip,
		DHCPServerIP: dhcpServer,
		LeaseTime:    leaseTime,
		RenewalTime:  renewalTime,
		RebindTime:   rebindTime,
	}, nil
}

// ReleaseLease sends DHCP RELEASE to server
func (c *Client) ReleaseLease(ctx context.Context, currentIP, dhcpServerIP net.IP) error {
	// Create DHCP client with raw sockets
	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer client.Close()

	// Create RELEASE message
	release, err := dhcpv4.NewReleaseFromACK(&dhcpv4.DHCPv4{
		ClientIPAddr: currentIP,
		ClientHWAddr: c.mac,
		ServerIPAddr: dhcpServerIP,
	})
	if err != nil {
		return fmt.Errorf("failed to create RELEASE message: %w", err)
	}

	// Send RELEASE (no response expected)
	serverAddr := &net.UDPAddr{IP: dhcpServerIP, Port: 67}
	if _, err := client.SendAndRead(ctx, serverAddr, release, nil); err != nil {
		return fmt.Errorf("failed to send RELEASE: %w", err)
	}

	return nil
}
