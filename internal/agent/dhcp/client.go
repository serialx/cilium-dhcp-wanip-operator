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
	SubnetMask   net.IPMask
	DHCPServerIP net.IP
	LeaseTime    time.Duration
	RenewalTime  time.Duration
	RebindTime   time.Duration
	Offer        *dhcpv4.DHCPv4 // DHCP OFFER packet (needed for library's Renew)
	ACK          *dhcpv4.DHCPv4 // DHCP ACK packet
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
	defer func() { _ = client.Close() }()

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

	// Get subnet mask (DHCP Option 1)
	subnetMask := lease.ACK.SubnetMask()
	if subnetMask == nil {
		// Default to /24 if not specified
		subnetMask = net.CIDRMask(24, 32)
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
		SubnetMask:   subnetMask,
		DHCPServerIP: dhcpServer,
		LeaseTime:    leaseTime,
		RenewalTime:  renewalTime,
		RebindTime:   rebindTime,
		Offer:        lease.Offer,
		ACK:          lease.ACK,
	}, nil
}

// RenewLease performs unicast DHCP RENEW using the library's Renew method
func (c *Client) RenewLease(ctx context.Context, currentIP, dhcpServerIP net.IP, previousOffer, previousAck []byte) (*LeaseInfo, error) {
	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	var renewedLease *nclient4.Lease

	// DEBUG: Log what we received
	fmt.Printf("DEBUG RenewLease: previousOffer len=%d, previousAck len=%d\n", len(previousOffer), len(previousAck))

	if len(previousOffer) > 0 && len(previousAck) > 0 {
		fmt.Printf("DEBUG: Using library's Renew() method (broadcast)\n")
		// Use default raw socket client - sends broadcast renewals (0.0.0.0 > 255.255.255.255)
		// Unicast renewals don't work reliably with this library, but broadcast works fine
		// and is valid per RFC 2131

		// Use library's Renew() with saved Offer and ACK
		offer, err := dhcpv4.FromBytes(previousOffer)
		if err != nil {
			return nil, fmt.Errorf("failed to parse previous OFFER: %w", err)
		}

		ack, err := dhcpv4.FromBytes(previousAck)
		if err != nil {
			return nil, fmt.Errorf("failed to parse previous ACK: %w", err)
		}

		lease := &nclient4.Lease{
			Offer: offer,
			ACK:   ack,
		}

		renewedLease, err = client.Renew(ctx, lease)
		if err != nil {
			return nil, fmt.Errorf("RENEW request failed: %w", err)
		}
	} else {
		fmt.Printf("DEBUG: Using fallback manual renewal (broadcast)\n")
		// Fallback: Manual renewal (for old leases without saved ACK)
		req, err := dhcpv4.NewRequestFromOffer(&dhcpv4.DHCPv4{
			ClientIPAddr: currentIP,
			ClientHWAddr: c.mac,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create RENEW request: %w", err)
		}

		req.UpdateOption(dhcpv4.OptServerIdentifier(dhcpServerIP))

		serverAddr := &net.UDPAddr{IP: dhcpServerIP, Port: 67}
		resp, err := client.SendAndRead(ctx, serverAddr, req, nil)
		if err != nil {
			return nil, fmt.Errorf("RENEW request failed: %w", err)
		}

		if resp.MessageType() != dhcpv4.MessageTypeAck {
			return nil, fmt.Errorf("expected ACK, got %s", resp.MessageType())
		}

		renewedLease = &nclient4.Lease{ACK: resp}
	}

	if renewedLease == nil || renewedLease.ACK == nil {
		return nil, fmt.Errorf("no ACK received from DHCP server")
	}

	resp := renewedLease.ACK

	// Extract updated lease information
	ip := resp.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		ip = currentIP // Server may not set YourIPAddr in RENEW ACK
	}

	subnetMask := resp.SubnetMask()
	if subnetMask == nil {
		subnetMask = net.CIDRMask(24, 32)
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
		SubnetMask:   subnetMask,
		DHCPServerIP: dhcpServer,
		LeaseTime:    leaseTime,
		RenewalTime:  renewalTime,
		RebindTime:   rebindTime,
		Offer:        renewedLease.Offer, // Save Offer for next renewal
		ACK:          resp,
	}, nil
}

// RebindLease performs DHCP lease acquisition (fallback when renewal fails)
// Since renewal failed, we do a full DORA (Discover/Offer/Request/Ack) to get a new lease
func (c *Client) RebindLease(ctx context.Context, currentIP net.IP) (*LeaseInfo, error) {
	// When rebind is needed, it typically means the original DHCP server is unreachable
	// The simplest and most reliable approach is to perform a full DHCP discovery
	// The IP is already on the interface, so Request() will work correctly

	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Perform full DORA - this is simpler and more reliable than manual REBIND
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

	// Get subnet mask
	subnetMask := lease.ACK.SubnetMask()
	if subnetMask == nil {
		subnetMask = net.CIDRMask(24, 32)
	}

	dhcpServer := lease.ACK.ServerIdentifier()
	if dhcpServer == nil || dhcpServer.IsUnspecified() {
		return nil, fmt.Errorf("no DHCP server identifier in ACK")
	}

	leaseTime := lease.ACK.IPAddressLeaseTime(time.Hour)
	renewalTime := lease.ACK.IPAddressRenewalTime(leaseTime / 2)
	rebindTime := lease.ACK.IPAddressRebindingTime(leaseTime * 7 / 8)

	return &LeaseInfo{
		IPAddress:    ip,
		SubnetMask:   subnetMask,
		DHCPServerIP: dhcpServer,
		LeaseTime:    leaseTime,
		RenewalTime:  renewalTime,
		RebindTime:   rebindTime,
		Offer:        lease.Offer,
		ACK:          lease.ACK,
	}, nil
}

// ReleaseLease sends DHCP RELEASE to server
func (c *Client) ReleaseLease(ctx context.Context, currentIP, dhcpServerIP net.IP) error {
	client, err := nclient4.New(c.iface, nclient4.WithHWAddr(c.mac))
	if err != nil {
		return fmt.Errorf("failed to create DHCP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Construct lease object from current lease data for release
	ack := &dhcpv4.DHCPv4{
		ClientIPAddr: currentIP,
		ClientHWAddr: c.mac,
	}
	ack.UpdateOption(dhcpv4.OptServerIdentifier(dhcpServerIP))

	lease := &nclient4.Lease{
		ACK: ack,
	}

	// Use library's Release method (no response expected per RFC 2131)
	if err := client.Release(lease); err != nil {
		return fmt.Errorf("failed to send RELEASE: %w", err)
	}

	return nil
}
