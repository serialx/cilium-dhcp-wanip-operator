# Cilium DHCP WAN IP Operator

A Kubernetes operator that dynamically allocates public IP addresses from your ISP via DHCP and integrates them with Cilium's LoadBalancer IP pools. Perfect for home labs and edge deployments where you want to expose services with multiple public IPs without static IP assignments.

## Description

This operator bridges the gap between ISP-provided DHCP addresses and Kubernetes LoadBalancer services. It:

1. **Router Agent**: Runs a dedicated HTTP API agent on your router (UDM-Pro, pfSense, etc.) for reliable DHCP lease management
2. **Allocates Public IPs**: Creates macvlan interfaces and obtains DHCP leases via the agent API over SSH tunnel
3. **Updates Cilium Pools**: Automatically adds allocated IPs to CiliumLoadBalancerIPPool resources
4. **Manages Lifecycle**: Handles cleanup when IPs are released with graceful lease termination
5. **Integrates with BGP**: Works with Cilium BGP to advertise routes dynamically (no static routes needed)

Perfect for homelabs where you have limited public IPs but want proper LoadBalancer support for services like Ingress controllers, game servers, or VPN endpoints

## How It Works

```
┌─────────────┐  SSH Tunnel  ┌──────────────┐
│  Operator   │─────────────>│ Router Agent │
│   (K8s)     │   HTTP API   │  (systemd)   │
└─────────────┘              └──────────────┘
      │                             │
      │ 1. AllocateLease()          │ 2. Create macvlan
      │    via agent API            │    + DHCP lease
      │                             │
      │ 3. GetLease()               │ 4. Track renewals
      │    verify status            │    + proxy ARP
      │                             │
      │ 5. Add IP to Pool           │ 6. Unicast DHCP
      v                             v    renewal
┌─────────────┐              ┌──────────────┐
│   Cilium    │<─── BGP ────>│  WAN/ISP     │
│  IP Pool    │              │  Network     │
└─────────────┘              └──────────────┘
```

**Traffic Flow**: Internet → Router WAN (proxy ARP) → Router BGP table → K8s via LAN → Cilium LoadBalancer → Service

**v0.3.2+ uses a dedicated router agent** that replaces SSH scripts with an HTTP API for better reliability and structured error handling.

## Quick Start

### Prerequisites

**Infrastructure:**
- Kubernetes v1.16+ cluster with Cilium installed
- Cilium BGP configured and peering with your router
- Router with SSH access (UDM-Pro, pfSense, Linux-based routers)

**Development (optional - only needed if building from source):**
- Go 1.24.0+
- Docker 17.03+
- kubectl 1.11.3+

### Installation

**Option A: Quick Install (Recommended - Uses pre-built images)**

**1. Deploy the operator:**

```bash
kubectl apply -f https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/v0.3.2/dist/install.yaml
```

This will:
- Create the `cilium-dhcp-wanip-operator-system` namespace
- Install the `PublicIPClaim` CRD
- Deploy the operator controller with image `ghcr.io/serialx/cilium-dhcp-wanip-operator:v0.3.2`
- Set up necessary RBAC permissions

**2. Install the router agent (v0.3.2+):**

The router agent is a systemd service that runs on your router. Download and install:

```bash
# Download the agent binary for your router architecture
# For UDM-Pro (ARM64):
wget https://github.com/serialx/cilium-dhcp-wanip-operator/releases/download/v0.3.2/dhcp-wan-agent-linux-arm64
ssh root@192.168.1.1 "mkdir -p /data/dhcp-wan-agent/bin"
scp dhcp-wan-agent-linux-arm64 root@192.168.1.1:/data/dhcp-wan-agent/bin/dhcp-wan-agent
ssh root@192.168.1.1 "chmod +x /data/dhcp-wan-agent/bin/dhcp-wan-agent"

# Install systemd service
curl -O https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/v0.3.2/deploy/agent/dhcp-wan-agent.service
scp dhcp-wan-agent.service root@192.168.1.1:/etc/systemd/system/
ssh root@192.168.1.1 "systemctl enable --now dhcp-wan-agent"
```

Verify the agent is running:

```bash
ssh root@192.168.1.1 systemctl status dhcp-wan-agent
```

**Option B: Build and Deploy from Source**

**1. Build and install the router agent:**

```bash
# Build agent for your router architecture
GOOS=linux GOARCH=arm64 go build -o dhcp-wan-agent cmd/agent/main.go

# Deploy to router
scp dhcp-wan-agent root@192.168.1.1:/data/dhcp-wan-agent/bin/
scp deploy/agent/dhcp-wan-agent.service root@192.168.1.1:/etc/systemd/system/
ssh root@192.168.1.1 "systemctl enable --now dhcp-wan-agent"
```

**2. Create SSH secret**

```bash
ssh-keygen -t ed25519 -f ssh_id_m2m_router -N "" -C "cilium-dhcp-wanip-operator ssh key"
kubectl -n kube-system create secret generic router-ssh \
  --from-file=id_rsa=ssh_id_m2m_router
```

**3. Install CRDs**

```bash
make install
```

**4. Deploy operator**

```bash
export IMG=<your-registry>/cilium-dhcp-wanip-operator:latest

# Option A: Multi-arch build and push (recommended - supports AMD64, ARM64, s390x, ppc64le)
make docker-buildx IMG=$IMG

# Option B: Single-arch build (faster, builds for your host platform)
make docker-build docker-push IMG=$IMG

# Deploy to cluster
make deploy IMG=$IMG
```

**Multi-arch build options:**

```bash
# Build for specific platforms only
make docker-buildx IMG=$IMG PLATFORMS=linux/amd64,linux/arm64

# Default platforms: linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
```

**5. Create a Cilium IP Pool**

```bash
kubectl apply -f config/samples/cilium-ippool-example.yaml
```

**6. Create a PublicIPClaim**

```yaml
apiVersion: network.serialx.net/v1alpha1
kind: PublicIPClaim
metadata:
  name: ip-wan-001
spec:
  poolName: public-pool
  router:
    host: 192.168.1.1
    user: root
    sshSecretRef: router-ssh
    command: /data/cilium-dhcp-wanip-operator/alloc_public_ip.sh
    wanParent: eth9  # Your router's WAN interface
```

```bash
kubectl apply -f config/samples/network_v1alpha1_publicipclaim.yaml
```

**7. Verify**

```bash
kubectl get publicipclaims
# NAME         POOL          IP               PHASE   AGE
# ip-wan-001   public-pool   203.0.113.45     Ready   1m
```

📚 **See [DEPLOYMENT.md](DEPLOYMENT.md) for detailed deployment instructions**

## Key Features

**v0.3.2+ Router Agent Integration:**
- ✅ **HTTP API Agent**: Dedicated systemd service on router with structured JSON responses
- ✅ **Automatic DHCP Renewal**: Agent tracks and renews leases automatically via unicast (no broadcast storms!)
- ✅ **Graceful Cleanup**: Proper lease release via agent API on deletion
- ✅ **Renewal Tracking**: Monitor DHCP renewal count in claim status
- ✅ **SSH Tunnel Security**: All agent communication over SSH tunnel

**Core Features:**
- ✅ **Automatic IP Allocation**: Creates macvlan interfaces and obtains DHCP leases
- ✅ **Cilium Integration**: Updates CiliumLoadBalancerIPPool with allocated IPs
- ✅ **BGP-Ready**: Works with Cilium BGP for dynamic route advertisement
- ✅ **Proxy ARP**: Configures router to answer ARP for allocated IPs
- ✅ **Auto-Cleanup**: Finalizers ensure proper cleanup on deletion
- ✅ **MAC Generation**: Auto-generates unique MAC addresses for each claim
- ✅ **API Version Detection**: Supports both Cilium v2 and v2alpha1 APIs
- ✅ **Status Tracking**: Full status reporting with phase, IP, interface, and MAC
- ✅ **Automatic Reboot Recovery**: Detects router reboots and automatically restores configuration
- ✅ **SSH Connection Pooling**: Efficient connection management with automatic reconnection
- ✅ **Periodic Verification**: Validates router state every 60 minutes via agent API
- ✅ **Event-Driven Reconciliation**: Reacts immediately to connection drops and router state changes

## Examples

### Basic Claim

```yaml
apiVersion: network.serialx.net/v1alpha1
kind: PublicIPClaim
metadata:
  name: ingress-ip
spec:
  poolName: public-pool
  router:
    host: 192.168.1.1
    user: root
    sshSecretRef: router-ssh
    wanParent: eth9
```

### Claim with Custom Interface and MAC

```yaml
apiVersion: network.serialx.net/v1alpha1
kind: PublicIPClaim
metadata:
  name: game-server-ip
spec:
  poolName: game-pool
  router:
    host: 192.168.1.1
    port: 22
    user: admin
    sshSecretRef: router-ssh
    command: /usr/local/bin/alloc_public_ip.sh
    wanParent: eth9
    wanInterface: wan-game
    macAddress: "02:aa:bb:cc:dd:01"
```

## Architecture Details

See [SPEC.md](SPEC.md) for complete architecture documentation including:
- Router agent HTTP API design (v0.3.2+)
- Router script implementation (proxy ARP + Cilium BGP) - legacy
- CRD schema and validation
- Controller reconciliation logic
- Finalizer cleanup process
- Networking details (rp_filter, BGP routing, etc.)

See [ROUTER_AGENT_DESIGN.md](ROUTER_AGENT_DESIGN.md) for router agent implementation details (v0.3.2+)

## Development

**Run locally:**

```bash
make run
```

**Build binary:**

```bash
make build
```

**Build Docker images:**

```bash
# Single-arch (for your host platform)
make docker-build IMG=<your-image>

# Multi-arch (cross-platform)
make docker-buildx IMG=<your-image>

# Multi-arch with custom platforms
make docker-buildx IMG=<your-image> PLATFORMS=linux/amd64,linux/arm64
```

**Run tests:**

```bash
make test
```

**Generate manifests:**

```bash
make manifests generate
```

### Releasing New Versions

📚 **See [RELEASE.md](RELEASE.md) for the complete release process documentation.**

Quick overview for releasing a new version:

**1. Build and push the Docker image**

The GitHub Actions workflow automatically builds and pushes images when you create a tag:

```bash
git tag -a v0.3.2 -m "Release v0.3.2 - Description of changes"
git push origin v0.3.2
```

This will trigger the CI to build multi-platform images and push to `ghcr.io/serialx/cilium-dhcp-wanip-operator:v0.3.2`

**2. Build and release the router agent**

Build agent binaries for multiple architectures:

```bash
# Build for common platforms
GOOS=linux GOARCH=arm64 go build -o dhcp-wan-agent-linux-arm64 cmd/agent/main.go
GOOS=linux GOARCH=amd64 go build -o dhcp-wan-agent-linux-amd64 cmd/agent/main.go
```

**3. Generate the installer manifest**

After the CI completes, update the installer manifest with the new image:

```bash
make build-installer IMG=ghcr.io/serialx/cilium-dhcp-wanip-operator:v0.3.2
```

This updates `dist/install.yaml` with the new image tag.

**4. Commit and push the installer**

```bash
git add dist/install.yaml config/manager/kustomization.yaml
git commit -m "chore: update installer manifest for v0.3.2"
git push origin main
```

**5. Create a GitHub Release**

```bash
gh release create v0.3.2 \
  --title "v0.3.2 - Router Agent HTTP API Integration" \
  --notes "Release notes here" \
  dhcp-wan-agent-linux-arm64 \
  dhcp-wan-agent-linux-amd64
```

Or create it manually in the GitHub UI and attach the agent binaries.

**Users can then install the new version:**

```bash
kubectl apply -f https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/v0.3.2/dist/install.yaml
```

## Uninstall

**If installed via Quick Install (Option A):**

```bash
# Delete all claims first to ensure proper cleanup
kubectl delete publicipclaims --all

# Remove the operator
kubectl delete -f https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/v0.3.2/dist/install.yaml

# Stop and remove the router agent
ssh root@192.168.1.1 "systemctl disable --now dhcp-wan-agent"
ssh root@192.168.1.1 "rm -f /etc/systemd/system/dhcp-wan-agent.service"
```

**If installed from source (Option B):**

```bash
# Delete all claims
kubectl delete publicipclaims --all

# Undeploy operator
make undeploy

# Remove CRDs
make uninstall
```

## Troubleshooting

**Check operator logs:**

```bash
kubectl -n cilium-dhcp-wanip-operator-system logs deployment/cilium-dhcp-wanip-operator-controller-manager
```

**Check claim status:**

```bash
kubectl describe publicipclaim <name>
```

**Common issues:**
- SSH authentication fails → Check SSH key in secret
- DHCP fails → Verify `wanParent` interface name
- IP not added to pool → Check RBAC permissions for Cilium resources

## Resilience & Recovery

### Automatic Reboot Recovery (v0.2.0+)

The operator automatically detects and recovers from router reboots with **no manual intervention required**:

**How It Works**:
- **SSH Connection Monitoring**: Maintains persistent SSH connections to routers with keep-alive checks every 30 seconds
- **Reboot Detection**: Detects router reboots by monitoring uptime changes (~40 second worst-case detection time)
- **Automatic Restoration**: Immediately reapplies all configuration (interfaces, DHCP clients, proxy ARP) when reboot detected
- **Periodic Verification**: Validates router state every 60 minutes as a safety net to catch any configuration drift
- **Connection Pooling**: Multiple claims share a single SSH connection per router for efficiency

**What This Means**:
- ✅ Router reboots are automatically handled
- ✅ Services recover within ~40 seconds of router reboot
- ✅ No manual intervention needed
- ✅ Configuration drift is automatically corrected
- ✅ Connection drops trigger immediate reconciliation

**Observability**:
```bash
# Check claim status to see last verification time
kubectl get publicipclaim my-claim -o yaml

# Status fields show:
# - lastVerified: timestamp of last successful verification
# - routerUptime: current router uptime in seconds
# - configurationVerified: whether config has been verified
# - lastReconciliationReason: why last reconciliation occurred
#   (router_reboot, interface_missing, periodic, etc.)
```

**Events**:
The operator emits Kubernetes events for key actions:
```bash
kubectl describe publicipclaim my-claim

# Events you may see:
# - RouterRebooted: Router reboot detected, reapplying configuration
# - ConfigurationApplied: Configuration applied successfully
# - ConfigurationDrift: Interface missing, reapplying configuration
```

## Contributing

Contributions are welcome! Please:
1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests: `make test`
5. Submit a pull request

Run `make help` for all available make targets.

More information: [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

