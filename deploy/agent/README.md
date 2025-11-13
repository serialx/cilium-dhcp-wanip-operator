# DHCP WAN Agent Deployment

This directory contains deployment files for the DHCP WAN Agent that runs on the router.

## Overview

The DHCP WAN Agent replaces SSH-based shell scripts with a Go service that:
- ✅ Handles DHCP operations via raw sockets (unicast renewals without broadcast)
- ✅ Manages macvlan interfaces automatically
- ✅ Provides HTTP API on localhost:8692 (accessible only via SSH tunnel)
- ✅ Auto-renews leases with REBIND fallback
- ✅ Persists lease state across agent restarts
- ✅ Verifies leases after router reboots
- ✅ Survives router reboots via UDM Boot Script

## Architecture

The agent follows the UDM Boot Script pattern (similar to tailscale-udm):
- **Package directory**: `/data/dhcp-wan-agent/` - persistent storage across reboots
- **Boot script**: `/data/on_boot.d/10-dhcp-wan-agent.sh` - automatically starts agent after reboot
- **Management script**: `/data/dhcp-wan-agent/manage.sh` - handles install/start/stop/status
- **Systemd service**: Managed by `manage.sh`, ensures agent runs as a daemon

## Building

```bash
# Build agent for UDM-Pro (ARM64)
make build-agent

# Build for local testing (current architecture)
make build-agent-local
```

The binary will be created at `bin/dhcp-wan-agent`.

## Installation

### Automated Installation (Recommended)

```bash
# Install to default router (192.168.1.1)
make install-agent

# Install to custom router address
make install-agent ROUTER_ADDR=192.168.10.1
```

This will:
1. Copy all files to `/data/dhcp-wan-agent/`
2. Install boot script to `/data/on_boot.d/10-dhcp-wan-agent.sh`
3. Create systemd service
4. Start the agent

### Manual Installation

1. Build the agent:
```bash
make build-agent
```

2. Run the install script:
```bash
./deploy/agent/install.sh 192.168.1.1
```

The agent will automatically start on router boot.

## Configuration

The agent accepts the following command-line flags:

- `--listen`: HTTP listen address (default: `127.0.0.1:8692`)
- `--state-dir`: State directory for lease persistence (default: `/var/lib/dhcp-wan-agent`)
- `--log-level`: Log level - debug, info, warn, error (default: `info`)

## API Endpoints

The agent exposes a REST API on localhost:8692 (accessible only via SSH tunnel):

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

## Management

The agent includes a management script at `/data/dhcp-wan-agent/manage.sh`:

```bash
# Check status
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh status

# Start agent
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh start

# Stop agent
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh stop

# Restart agent
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh restart

# Reinstall (if needed)
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh install!
```

## Monitoring

View logs:
```bash
ssh root@192.168.1.1 journalctl -u dhcp-wan-agent -f
```

Check service status:
```bash
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh status
# or
ssh root@192.168.1.1 systemctl status dhcp-wan-agent
```

## Troubleshooting

### Agent fails to start
Check logs for errors:
```bash
ssh root@192.168.1.1 journalctl -u dhcp-wan-agent -n 50
```

### Leases marked as "stale" after reboot
The agent now survives router reboots via the UDM boot script at `/data/on_boot.d/10-dhcp-wan-agent.sh`.
If you see stale leases, verify the boot script is installed and the agent is running after reboot.

Check after reboot:
```bash
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh status
```

### DHCP renewal failures
Check connectivity to DHCP server and verify interface configuration:
```bash
ssh root@192.168.1.1 ip link show
```

## Security

- Agent binds to `127.0.0.1:8692` only (not accessible from network)
- All API calls go through SSH tunnel created by operator
- Uses existing SSH key authentication
- No additional credentials required

## Uninstallation

```bash
# Uninstall agent (preserves data)
ssh root@192.168.1.1 /data/dhcp-wan-agent/manage.sh uninstall

# Remove boot script
ssh root@192.168.1.1 rm -f /data/on_boot.d/10-dhcp-wan-agent.sh

# Optional: Remove all data
ssh root@192.168.1.1 rm -rf /data/dhcp-wan-agent /var/lib/dhcp-wan-agent
```
