#!/bin/bash
set -e

# Install script for DHCP WAN Agent on UDM-Pro
# Usage: ./install.sh <router-address>

ROUTER_ADDR="${1:-192.168.1.1}"
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
AGENT_BINARY="${SCRIPT_DIR}/../../bin/dhcp-wan-agent"
PACKAGE_ROOT="/data/dhcp-wan-agent"

echo "Installing DHCP WAN Agent to ${ROUTER_ADDR}..."

# Check if agent binary exists
if [ ! -f "${AGENT_BINARY}" ]; then
    echo "Error: Agent binary not found at ${AGENT_BINARY}"
    echo "Please run 'make build-agent' first"
    exit 1
fi

# 1. Create package directory
echo "Creating package directory..."
ssh "root@${ROUTER_ADDR}" "mkdir -p ${PACKAGE_ROOT}"

# 2. Copy package files to router
echo "Copying agent files..."
scp "${AGENT_BINARY}" "root@${ROUTER_ADDR}:${PACKAGE_ROOT}/dhcp-wan-agent"
scp "${SCRIPT_DIR}/dhcp-wan-agent.service" "root@${ROUTER_ADDR}:${PACKAGE_ROOT}/"
scp "${SCRIPT_DIR}/manage.sh" "root@${ROUTER_ADDR}:${PACKAGE_ROOT}/"
scp "${SCRIPT_DIR}/on-boot.sh" "root@${ROUTER_ADDR}:${PACKAGE_ROOT}/"

# 3. Set executable permissions
echo "Setting permissions..."
ssh "root@${ROUTER_ADDR}" "chmod +x ${PACKAGE_ROOT}/dhcp-wan-agent"
ssh "root@${ROUTER_ADDR}" "chmod +x ${PACKAGE_ROOT}/manage.sh"
ssh "root@${ROUTER_ADDR}" "chmod +x ${PACKAGE_ROOT}/on-boot.sh"

# 4. Install boot script
echo "Installing boot script..."
ssh "root@${ROUTER_ADDR}" "mkdir -p /data/on_boot.d"
ssh "root@${ROUTER_ADDR}" "ln -sf ${PACKAGE_ROOT}/on-boot.sh /data/on_boot.d/10-dhcp-wan-agent.sh"

# 5. Run installation
echo "Running installation..."
ssh "root@${ROUTER_ADDR}" "${PACKAGE_ROOT}/manage.sh install"

# 6. Start service
echo "Starting service..."
ssh "root@${ROUTER_ADDR}" "${PACKAGE_ROOT}/manage.sh start"

# 7. Check status
echo ""
echo "Installation complete! Checking service status..."
ssh "root@${ROUTER_ADDR}" "${PACKAGE_ROOT}/manage.sh status"

echo ""
echo "The agent will automatically start on router boot via /data/on_boot.d/10-dhcp-wan-agent.sh"
echo ""
echo "Useful commands:"
echo "  Status:  ssh root@${ROUTER_ADDR} '${PACKAGE_ROOT}/manage.sh status'"
echo "  Stop:    ssh root@${ROUTER_ADDR} '${PACKAGE_ROOT}/manage.sh stop'"
echo "  Start:   ssh root@${ROUTER_ADDR} '${PACKAGE_ROOT}/manage.sh start'"
echo "  Restart: ssh root@${ROUTER_ADDR} '${PACKAGE_ROOT}/manage.sh restart'"
echo "  Logs:    ssh root@${ROUTER_ADDR} journalctl -u dhcp-wan-agent -f"
