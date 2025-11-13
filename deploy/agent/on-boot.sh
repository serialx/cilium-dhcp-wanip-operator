#!/bin/bash
set -e

DHCP_WAN_AGENT_ROOT="/data/dhcp-wan-agent"

$DHCP_WAN_AGENT_ROOT/manage.sh on-boot
