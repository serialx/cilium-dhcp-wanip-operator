package ssh

import (
	"time"

	"golang.org/x/crypto/ssh"
)

// ReconnectionReason indicates why a reconnection event was triggered
type ReconnectionReason string

const (
	ReasonReboot         ReconnectionReason = "reboot"
	ReasonConnectionDrop ReconnectionReason = "connection_drop"
)

// RouterConfig contains router connection details
type RouterConfig struct {
	Address         string               // Router SSH address (e.g., "192.168.1.1:22")
	Username        string               // SSH username
	AuthMethod      ssh.AuthMethod       // Key-based or password auth
	HostKeyCallback ssh.HostKeyCallback  // Host key verification (nil = insecure)
	Timeout         time.Duration        // Connection timeout (default: 30s)
}