#!/bin/bash
set -euo pipefail

# Router-side script for allocating public IPs via DHCP
# Install to: /usr/local/bin/alloc_public_ip.sh
# Make executable: chmod +x /usr/local/bin/alloc_public_ip.sh

# These will be passed as environment variables from the operator
# WAN_PARENT, WAN_IF, WAN_MAC are provided by the PublicIPClaim
: "${WAN_PARENT:?WAN_PARENT must be set}"
: "${WAN_IF:?WAN_IF must be set}"
: "${WAN_MAC:?WAN_MAC must be set}"

# 1) Prepare macvlan (bridge mode), no primary IP needed
if ip link show "$WAN_IF" >/dev/null 2>&1; then
  # Interface exists - verify MAC matches
  CURRENT_MAC=$(ip link show "$WAN_IF" | awk '/link\/ether/{print $2}')
  if [ "$CURRENT_MAC" != "$WAN_MAC" ]; then
    echo "ERROR: Interface $WAN_IF exists with different MAC: $CURRENT_MAC (expected $WAN_MAC)"
    exit 1
  fi
  echo "Interface $WAN_IF already exists with correct MAC"
else
  # Create new interface
  ip link add link "$WAN_PARENT" name "$WAN_IF" type macvlan mode bridge
  ip link set "$WAN_IF" address "$WAN_MAC" up
  echo "Created interface $WAN_IF with MAC $WAN_MAC"
fi

# 2) Pull an address by DHCP and run daemon to maintain the lease
#    This will fork to background and handle renewals automatically
#    Note: On UDM routers, use /usr/bin/busybox-legacy/udhcpc
#    The --script can be omitted to use the default, or point to a custom script
/usr/bin/busybox-legacy/udhcpc --interface "$WAN_IF" --script /etc/udhcpc/udhcpc_ip_only --pidfile "/var/run/udhcpc.$WAN_IF.pid" || {
    echo "DHCP failed on $WAN_IF"
    exit 1
}

# Wait for DHCP to complete
sleep 2

# 3) Get the allocated IP from the interface
NEW_IP=$(ip -4 addr show dev "$WAN_IF" | awk '/inet /{print $2}' | cut -d/ -f1 | head -n1)
[ -n "$NEW_IP" ] || { echo "no-ip"; exit 1; }

# Extract CIDR suffix for cleanup
NEW_IP_CIDR=$(ip -4 addr show dev "$WAN_IF" | awk '/inet /{print $2}' | head -n1)

# 4) Remove the IP from the interface - we only need the DHCP lease, not the binding!
#    If the IP stays on the interface, the kernel creates a local route that conflicts with BGP.
#    We want BGP to handle routing to K8s, not have a local route on the WAN interface.
ip addr del "$NEW_IP_CIDR" dev "$WAN_IF" 2>/dev/null || true

# 5) Enable proxy ARP and publish the IP via neighbor proxy (so router answers ARP for NEW_IP)
sysctl -w net.ipv4.conf.$WAN_IF.proxy_arp=1 >/dev/null

# Disable reverse path filtering (required since we unbound the IP from the interface)
# Packets arrive on WAN but route to K8s via LAN - any rp_filter would cause issues
# since the interface has no IP/routes, we must disable rp_filter entirely
sysctl -w net.ipv4.conf.$WAN_IF.rp_filter=0 >/dev/null

# Clear old proxy entries if any
ip neigh show proxy dev "$WAN_IF" | awk '{print $1}' | while read -r ip; do ip neigh del proxy "$ip" dev "$WAN_IF" || true; done
ip neigh add proxy "$NEW_IP" dev "$WAN_IF" || ip neigh replace proxy "$NEW_IP" dev "$WAN_IF"

# 6) NO static route needed - Cilium BGP handles routing!
# Cilium will advertise the IP to the router via BGP, and the router will learn the route dynamically.
# The BGP-learned route will point to the K8s node(s) via the LAN interface, NOT the WAN macvlan.

# IMPORTANT: print ONLY the IP for the operator to parse
printf "%s\n" "$NEW_IP"
