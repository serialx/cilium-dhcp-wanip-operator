//go:build linux

package network

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// SetupInterface configures network settings for a WAN interface
// This includes:
// - Removing IP from interface (avoid BGP conflicts)
// - Enabling proxy ARP
// - Adding neighbor proxy
// - Disabling rp_filter (allow asymmetric routing)
func SetupInterface(ifaceName string, ip net.IP, mac net.HardwareAddr) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get interface: %w", err)
	}

	// 1. Remove IP from interface (avoid BGP conflicts)
	// The IP should not be bound to the interface to prevent local route conflicts
	// Note: DHCP client may or may not have added the IP to the interface
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(32, 32),
		},
	}
	if err := netlink.AddrDel(link, addr); err != nil {
		// It's okay if the address wasn't there (EADDRNOTAVAIL or EINVAL)
		// Just log and continue - the important thing is that the IP is not bound
	}

	// 2. Enable proxy ARP
	proxyARPPath := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", ifaceName)
	if err := os.WriteFile(proxyARPPath, []byte("1"), 0644); err != nil {
		return fmt.Errorf("failed to enable proxy ARP: %w", err)
	}

	// 3. Add neighbor proxy
	neigh := &netlink.Neigh{
		LinkIndex: link.Attrs().Index,
		IP:        ip,
		Flags:     unix.NTF_PROXY,
	}
	if err := netlink.NeighSet(neigh); err != nil {
		return fmt.Errorf("failed to add neighbor proxy: %w", err)
	}

	// 4. Disable rp_filter (allow asymmetric routing)
	// Packets arrive on WAN interface but are routed via LAN
	rpFilterPath := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", ifaceName)
	if err := os.WriteFile(rpFilterPath, []byte("0"), 0644); err != nil {
		return fmt.Errorf("failed to disable rp_filter: %w", err)
	}

	return nil
}

// CleanupInterface removes network configuration for a WAN interface
func CleanupInterface(ifaceName string, ip net.IP) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		// Interface already gone
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("failed to get interface: %w", err)
	}

	// Remove neighbor proxy
	neigh := &netlink.Neigh{
		LinkIndex: link.Attrs().Index,
		IP:        ip,
		Flags:     unix.NTF_PROXY,
	}
	if err := netlink.NeighDel(neigh); err != nil {
		// It's okay if it wasn't there
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove neighbor proxy: %w", err)
		}
	}

	// Disable proxy ARP
	proxyARPPath := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", ifaceName)
	if err := os.WriteFile(proxyARPPath, []byte("0"), 0644); err != nil {
		// It's okay if interface is already gone
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to disable proxy ARP: %w", err)
		}
	}

	// Re-enable rp_filter
	rpFilterPath := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", ifaceName)
	if err := os.WriteFile(rpFilterPath, []byte("1"), 0644); err != nil {
		// It's okay if interface is already gone
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to re-enable rp_filter: %w", err)
		}
	}

	return nil
}

// CreateMacvlan creates a macvlan interface
func CreateMacvlan(ifaceName, parentIface string, mac net.HardwareAddr) error {
	parent, err := netlink.LinkByName(parentIface)
	if err != nil {
		return fmt.Errorf("failed to get parent interface %s: %w", parentIface, err)
	}

	// Check if interface already exists
	if _, err := netlink.LinkByName(ifaceName); err == nil {
		return fmt.Errorf("interface %s already exists", ifaceName)
	}

	// Create macvlan interface
	macvlan := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:         ifaceName,
			ParentIndex:  parent.Attrs().Index,
			HardwareAddr: mac,
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}

	if err := netlink.LinkAdd(macvlan); err != nil {
		return fmt.Errorf("failed to create macvlan interface: %w", err)
	}

	// Bring interface up
	if err := netlink.LinkSetUp(macvlan); err != nil {
		// Clean up on failure
		netlink.LinkDel(macvlan)
		return fmt.Errorf("failed to bring interface up: %w", err)
	}

	return nil
}

// DeleteMacvlan deletes a macvlan interface
func DeleteMacvlan(ifaceName string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		// Interface already gone
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("failed to get interface: %w", err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete interface: %w", err)
	}

	return nil
}
