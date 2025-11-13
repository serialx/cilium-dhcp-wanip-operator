//go:build linux

package network

import (
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// SendGratuitousARP sends gratuitous ARP announcements
// This announces the IP-to-MAC mapping to the network
func SendGratuitousARP(ifaceName string, ip net.IP, mac net.HardwareAddr, count int) error {
	// Get interface
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get interface: %w", err)
	}

	// Create raw socket (AF_PACKET for layer 2)
	fd, err := syscall.Socket(unix.AF_PACKET, syscall.SOCK_RAW, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("failed to create raw socket: %w", err)
	}
	defer syscall.Close(fd)

	// Build gratuitous ARP packet
	packet := buildGratuitousARP(mac, ip)

	// Bind to interface
	addr := &syscall.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  iface.Index,
	}

	// Send multiple times (typical: 3)
	for i := 0; i < count; i++ {
		if err := syscall.Sendto(fd, packet, 0, addr); err != nil {
			return fmt.Errorf("failed to send gratuitous ARP (attempt %d/%d): %w", i+1, count, err)
		}
		if i < count-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}

	return nil
}

// buildGratuitousARP builds an ARP announcement packet
func buildGratuitousARP(mac net.HardwareAddr, ip net.IP) []byte {
	packet := make([]byte, 42)

	// Ethernet header (14 bytes)
	copy(packet[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // Dst: broadcast
	copy(packet[6:12], mac)                                       // Src: our MAC
	packet[12] = 0x08                                             // EtherType: ARP
	packet[13] = 0x06

	// ARP header (28 bytes)
	packet[14] = 0x00
	packet[15] = 0x01 // Hardware type: Ethernet
	packet[16] = 0x08
	packet[17] = 0x00 // Protocol type: IPv4
	packet[18] = 0x06 // Hardware size: 6
	packet[19] = 0x04 // Protocol size: 4
	packet[20] = 0x00
	packet[21] = 0x01 // Opcode: Request (gratuitous)

	copy(packet[22:28], mac)                      // Sender MAC
	copy(packet[28:32], ip.To4())                 // Sender IP
	copy(packet[32:38], []byte{0, 0, 0, 0, 0, 0}) // Target MAC: 00:00:00:00:00:00
	copy(packet[38:42], ip.To4())                 // Target IP: same as sender (gratuitous!)

	return packet
}

// htons converts host byte order to network byte order (big-endian)
func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}
