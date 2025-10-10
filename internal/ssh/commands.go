package ssh

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// InterfaceExists checks if a network interface exists on the router
func (m *SSHConnectionManager) InterfaceExists(ifname string) (bool, error) {
	cmd := fmt.Sprintf("ip link show %s", ifname)
	output, err := m.Execute(cmd)
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("failed to check interface %s: %w (output: %s)",
			ifname, err, strings.TrimSpace(output))
	}
	return true, nil
}

// IsUdhcpcRunning checks if udhcpc daemon is running for the specified interface
func (m *SSHConnectionManager) IsUdhcpcRunning(ifname string) (bool, error) {
	// Use [u] trick to avoid matching the grep process itself
	cmd := fmt.Sprintf("ps aux | grep '[u]dhcpc.*%s'", ifname)
	output, err := m.Execute(cmd)
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("failed to inspect udhcpc processes: %w (output: %s)",
			err, strings.TrimSpace(output))
	}

	return strings.TrimSpace(output) != "", nil
}

// GetInterfaceMAC gets the MAC address of the specified interface
func (m *SSHConnectionManager) GetInterfaceMAC(ifname string) (string, error) {
	cmd := fmt.Sprintf("ip link show %s | grep 'link/ether' | awk '{print $2}'", ifname)
	output, err := m.Execute(cmd)
	if err != nil {
		return "", err
	}

	mac := strings.TrimSpace(output)
	if mac == "" {
		return "", fmt.Errorf("no MAC address found for interface %s", ifname)
	}

	return mac, nil
}

// IsProxyARPEnabled checks if proxy ARP is enabled for the specified interface
func (m *SSHConnectionManager) IsProxyARPEnabled(ifname string) (bool, error) {
	cmd := fmt.Sprintf("cat /proc/sys/net/ipv4/conf/%s/proxy_arp", ifname)
	output, err := m.Execute(cmd)
	if err != nil {
		return false, err
	}

	value := strings.TrimSpace(output)
	return value == "1", nil
}

// ListManagedInterfaces lists all interfaces matching the pattern for managed DHCP interfaces
func (m *SSHConnectionManager) ListManagedInterfaces() ([]string, error) {
	cmd := "ip link show | grep -E '^[0-9]+: (wan|eth).*\\.dhcp[0-9]+' | awk -F': ' '{print $2}' | cut -d'@' -f1"
	output, err := m.Execute(cmd)
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to list managed interfaces: %w (output: %s)",
			err, strings.TrimSpace(output))
	}

	var interfaces []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			interfaces = append(interfaces, line)
		}
	}

	return interfaces, nil
}

// GetInterfaceIP gets the IP address assigned to the specified interface
func (m *SSHConnectionManager) GetInterfaceIP(ifname string) (string, error) {
	cmd := fmt.Sprintf("ip addr show %s | grep 'inet ' | awk '{print $2}' | cut -d'/' -f1", ifname)
	output, err := m.Execute(cmd)
	if err != nil {
		return "", err
	}

	ip := strings.TrimSpace(output)
	if ip == "" {
		return "", fmt.Errorf("no IP address found for interface %s", ifname)
	}

	return ip, nil
}

// IsInterfaceUp checks if the specified interface is up
func (m *SSHConnectionManager) IsInterfaceUp(ifname string) (bool, error) {
	cmd := fmt.Sprintf("ip link show %s | grep -q 'state UP'", ifname)
	_, err := m.Execute(cmd)
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitStatus() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("failed to check interface %s state: %w", ifname, err)
	}
	return true, nil
}
