#!/bin/sh
set -e

PACKAGE_ROOT="${PACKAGE_ROOT:-"$(dirname -- "$(readlink -f -- "$0";)")"}"
export DHCP_WAN_AGENT_ROOT="${DHCP_WAN_AGENT_ROOT:-/data/dhcp-wan-agent}"

dhcp_wan_agent_status() {
  if ! command -v dhcp-wan-agent >/dev/null 2>&1; then
    echo "DHCP WAN Agent is not installed"
    exit 1
  elif systemctl is-active --quiet dhcp-wan-agent; then
    echo "DHCP WAN Agent is running"
    dhcp-wan-agent --version 2>/dev/null || echo "Version info not available"
  else
    echo "DHCP WAN Agent is not running"
  fi
}

dhcp_wan_agent_start() {
  systemctl start dhcp-wan-agent

  # Wait a few seconds for the daemon to start
  sleep 5

  if systemctl is-active --quiet dhcp-wan-agent; then
    echo "DHCP WAN Agent started successfully"
  else
    echo "DHCP WAN Agent failed to start"
    exit 1
  fi

  echo "Agent is now listening on 127.0.0.1:8692"
  echo "You can check status with: curl http://127.0.0.1:8692/health"
}

dhcp_wan_agent_stop() {
  echo "Stopping DHCP WAN Agent..."
  systemctl stop dhcp-wan-agent
}

dhcp_wan_agent_install() {
  echo "Installing DHCP WAN Agent..."

  # Create directories
  mkdir -p "${DHCP_WAN_AGENT_ROOT}/bin"
  mkdir -p /var/lib/dhcp-wan-agent
  chmod 700 /var/lib/dhcp-wan-agent

  # Copy binary to persistent storage if not already there
  if [ -f "${PACKAGE_ROOT}/dhcp-wan-agent" ] && [ ! -f "${DHCP_WAN_AGENT_ROOT}/bin/dhcp-wan-agent" ]; then
    echo "Installing binary to ${DHCP_WAN_AGENT_ROOT}/bin/..."
    cp "${PACKAGE_ROOT}/dhcp-wan-agent" "${DHCP_WAN_AGENT_ROOT}/bin/"
    chmod +x "${DHCP_WAN_AGENT_ROOT}/bin/dhcp-wan-agent"
  fi

  # Create symlink in /usr/local/bin for easy access
  if [ ! -L "/usr/local/bin/dhcp-wan-agent" ]; then
    if [ -e "/usr/local/bin/dhcp-wan-agent" ]; then
      rm -f /usr/local/bin/dhcp-wan-agent
    fi
    ln -s "${DHCP_WAN_AGENT_ROOT}/bin/dhcp-wan-agent" /usr/local/bin/dhcp-wan-agent
  fi

  # Install systemd service
  if [ ! -L "/etc/systemd/system/dhcp-wan-agent.service" ]; then
    if [ ! -e "${DHCP_WAN_AGENT_ROOT}/dhcp-wan-agent.service" ]; then
      rm -f /etc/systemd/system/dhcp-wan-agent.service
    fi

    echo "Installing systemd service..."
    ln -s "${DHCP_WAN_AGENT_ROOT}/dhcp-wan-agent.service" /etc/systemd/system/dhcp-wan-agent.service
  fi

  systemctl daemon-reload
  systemctl enable dhcp-wan-agent.service

  echo "Installation complete, run '$0 start' to start DHCP WAN Agent"
}

dhcp_wan_agent_uninstall() {
  echo "Removing DHCP WAN Agent"

  # Stop service
  systemctl stop dhcp-wan-agent || true
  systemctl disable dhcp-wan-agent || true

  # Remove systemd service
  rm -f /etc/systemd/system/dhcp-wan-agent.service || true
  systemctl daemon-reload

  # Remove symlink
  rm -f /usr/local/bin/dhcp-wan-agent || true

  echo "DHCP WAN Agent uninstalled"
  echo "Note: Data in /var/lib/dhcp-wan-agent and ${DHCP_WAN_AGENT_ROOT} has been preserved"
  echo "To completely remove, run: rm -rf /var/lib/dhcp-wan-agent ${DHCP_WAN_AGENT_ROOT}"
}

case $1 in
  "status")
    dhcp_wan_agent_status
    ;;
  "start")
    dhcp_wan_agent_start
    ;;
  "stop")
    dhcp_wan_agent_stop
    ;;
  "restart")
    dhcp_wan_agent_stop
    dhcp_wan_agent_start
    ;;
  "install")
    if systemctl is-active --quiet dhcp-wan-agent; then
      echo "DHCP WAN Agent is already installed and running"
      echo "If you wish to force a reinstall, run '$0 install!'"
      exit 0
    fi

    dhcp_wan_agent_install
    ;;
  "install!")
    dhcp_wan_agent_install
    ;;
  "uninstall")
    dhcp_wan_agent_stop
    dhcp_wan_agent_uninstall
    ;;
  "on-boot")
    if ! command -v dhcp-wan-agent >/dev/null 2>&1; then
      dhcp_wan_agent_install
    fi

    dhcp_wan_agent_start
    ;;
  *)
    echo "Usage: $0 {status|start|stop|restart|install|uninstall|on-boot}"
    exit 1
    ;;
esac
