# DHCP WAN Agent Deployment

This directory contains deployment files for the DHCP WAN Agent that runs on the router.

## Overview

The DHCP WAN Agent replaces SSH-based shell scripts with a Go service that:
- ✅ Handles DHCP operations via raw sockets (unicast renewals without broadcast)
- ✅ Manages macvlan interfaces automatically
- ✅ Provides HTTP API on localhost:8080 (accessible only via SSH tunnel)
- ✅ Auto-renews leases with REBIND fallback
- ✅ Persists lease state across agent restarts
- ✅ Verifies leases after router reboots

## Building

```bash
# Build agent for UDM-Pro (ARM64)
make build-agent

# Build for local testing (current architecture)
make build-agent-local
```

The binary will be created at `bin/dhcp-wan-agent`.

## Installation

### Automated Installation

```bash
# Install to default router (192.168.1.1)
make install-agent

# Install to custom router address
make install-agent ROUTER_ADDR=192.168.10.1
```

### Manual Installation

1. Copy binary to router:
```bash
scp bin/dhcp-wan-agent root@192.168.1.1:/usr/local/bin/
ssh root@192.168.1.1 chmod +x /usr/local/bin/dhcp-wan-agent
```

2. Create state directory:
```bash
ssh root@192.168.1.1 "mkdir -p /var/lib/dhcp-wan-agent && chmod 700 /var/lib/dhcp-wan-agent"
```

3. Install systemd service:
```bash
scp dhcp-wan-agent.service root@192.168.1.1:/etc/systemd/system/
ssh root@192.168.1.1 "systemctl daemon-reload && systemctl enable --now dhcp-wan-agent"
```

4. Verify installation:
```bash
ssh root@192.168.1.1 "systemctl status dhcp-wan-agent"
```

## Configuration

The agent accepts the following command-line flags:

- `--listen`: HTTP listen address (default: `127.0.0.1:8080`)
- `--state-dir`: State directory for lease persistence (default: `/var/lib/dhcp-wan-agent`)
- `--log-level`: Log level - debug, info, warn, error (default: `info`)

## API Endpoints

The agent exposes a REST API on localhost:8080 (accessible only via SSH tunnel):

### POST /leases
Allocate a new DHCP lease
```json
{
  "interface": "wan-001",
  "wanParent": "eth9",
  "macAddress": "02:aa:bb:cc:dd:01"
}
```

### GET /leases
List all active leases

### GET /leases/{interface}
Get status of a specific lease

### DELETE /leases/{interface}
Release a lease and clean up

### GET /health
Health check endpoint

## Monitoring

View logs:
```bash
ssh root@192.168.1.1 journalctl -u dhcp-wan-agent -f
```

Check service status:
```bash
ssh root@192.168.1.1 systemctl status dhcp-wan-agent
```

## Troubleshooting

### Agent fails to start
Check logs for errors:
```bash
ssh root@192.168.1.1 journalctl -u dhcp-wan-agent -n 50
```

### Leases marked as "stale" after reboot
This is expected if the router rebooted. The operator will automatically recreate leases.

### DHCP renewal failures
Check connectivity to DHCP server and verify interface configuration:
```bash
ssh root@192.168.1.1 ip link show
```

## Security

- Agent binds to `127.0.0.1:8080` only (not accessible from network)
- All API calls go through SSH tunnel created by operator
- Uses existing SSH key authentication
- No additional credentials required

## Uninstallation

```bash
ssh root@192.168.1.1 "systemctl stop dhcp-wan-agent && systemctl disable dhcp-wan-agent"
ssh root@192.168.1.1 "rm -f /etc/systemd/system/dhcp-wan-agent.service /usr/local/bin/dhcp-wan-agent"
ssh root@192.168.1.1 "rm -rf /var/lib/dhcp-wan-agent"
```
