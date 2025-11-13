#!/bin/bash
set -e

# Install script for DHCP WAN Agent on UDM-Pro
# Usage: ./install.sh <router-address>

ROUTER_ADDR="${1:-192.168.1.1}"
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
AGENT_BINARY="${SCRIPT_DIR}/../../bin/dhcp-wan-agent"

echo "Installing DHCP WAN Agent to ${ROUTER_ADDR}..."

# Check if agent binary exists
if [ ! -f "${AGENT_BINARY}" ]; then
    echo "Error: Agent binary not found at ${AGENT_BINARY}"
    echo "Please run 'make build-agent' first"
    exit 1
fi

# 1. Copy binary to router
echo "Copying agent binary..."
scp "${AGENT_BINARY}" "root@${ROUTER_ADDR}:/usr/local/bin/dhcp-wan-agent"
ssh "root@${ROUTER_ADDR}" "chmod +x /usr/local/bin/dhcp-wan-agent"

# 2. Create state directory
echo "Creating state directory..."
ssh "root@${ROUTER_ADDR}" "mkdir -p /var/lib/dhcp-wan-agent && chmod 700 /var/lib/dhcp-wan-agent"

# 3. Copy systemd service
echo "Installing systemd service..."
scp "${SCRIPT_DIR}/dhcp-wan-agent.service" "root@${ROUTER_ADDR}:/etc/systemd/system/"

# 4. Enable and start service
echo "Starting service..."
ssh "root@${ROUTER_ADDR}" "systemctl daemon-reload"
ssh "root@${ROUTER_ADDR}" "systemctl enable --now dhcp-wan-agent"

# 5. Check status
echo ""
echo "Installation complete! Checking service status..."
ssh "root@${ROUTER_ADDR}" "systemctl status dhcp-wan-agent"

echo ""
echo "You can view logs with:"
echo "  ssh root@${ROUTER_ADDR} journalctl -u dhcp-wan-agent -f"
