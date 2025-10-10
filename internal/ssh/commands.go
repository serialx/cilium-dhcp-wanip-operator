package ssh

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var ifaceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`)

func validateInterfaceName(name string) error {
	if name == "" {
		return errors.New("interface name must not be empty")
	}
	if !ifaceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid interface name %q", name)
	}
	return nil
}

// GetRouterUptime returns the router uptime as a duration.
func (m *SSHConnectionManager) GetRouterUptime(ctx context.Context) (time.Duration, error) {
	output, err := m.RunCommand(ctx, "cat /proc/uptime")
	if err != nil {
		return 0, err
	}
	uptime, err := parseUptime(output)
	if err != nil {
		return 0, err
	}
	return time.Duration(uptime * float64(time.Second)), nil
}

// InterfaceExists checks whether the interface exists on the router.
func (m *SSHConnectionManager) InterfaceExists(ctx context.Context, iface string) (bool, error) {
	if err := validateInterfaceName(iface); err != nil {
		return false, err
	}
	cmd := fmt.Sprintf("if [ -d /sys/class/net/%s ]; then echo true; else echo false; fi", iface)
	out, err := m.RunCommand(ctx, cmd)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "true"), nil
}

// IsUdhcpcRunning determines if udhcpc process is running.
func (m *SSHConnectionManager) IsUdhcpcRunning(ctx context.Context) (bool, error) {
	cmd := "if pgrep -x udhcpc >/dev/null 2>&1; then echo true; else echo false; fi"
	out, err := m.RunCommand(ctx, cmd)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "true"), nil
}

// GetInterfaceMAC reads the MAC address for a given interface.
func (m *SSHConnectionManager) GetInterfaceMAC(ctx context.Context, iface string) (string, error) {
	if err := validateInterfaceName(iface); err != nil {
		return "", err
	}
	cmd := fmt.Sprintf("cat /sys/class/net/%s/address", iface)
	out, err := m.RunCommand(ctx, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsProxyARPEnabled checks if proxy ARP is enabled on the interface.
func (m *SSHConnectionManager) IsProxyARPEnabled(ctx context.Context, iface string) (bool, error) {
	if err := validateInterfaceName(iface); err != nil {
		return false, err
	}
	cmd := fmt.Sprintf("cat /proc/sys/net/ipv4/conf/%s/proxy_arp", iface)
	out, err := m.RunCommand(ctx, cmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// ListManagedInterfaces lists all network interfaces.
func (m *SSHConnectionManager) ListManagedInterfaces(ctx context.Context) ([]string, error) {
	out, err := m.RunCommand(ctx, "ls /sys/class/net")
	if err != nil {
		return nil, err
	}
	entries := strings.Fields(string(out))
	result := make([]string, 0, len(entries))
	for _, iface := range entries {
		if iface == "" {
			continue
		}
		if ifaceNamePattern.MatchString(iface) {
			result = append(result, iface)
		}
	}
	return result, nil
}
