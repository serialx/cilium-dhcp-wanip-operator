# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Kubernetes operator built with Kubebuilder that manages public IP addresses obtained via DHCP from an ISP router and integrates them with Cilium's LoadBalancer IP pools. The operator enables allocating multiple public IPs by creating macvlan interfaces on a router (e.g., UDM-Pro) and using Cilium BGP for routing.

**Key Architecture:**
- **Router Script**: Creates macvlan interfaces, obtains DHCP leases, configures proxy ARP, and unbinds IPs from interfaces to avoid BGP conflicts
- **Operator**: SSH into router, run script, add allocated IPs to CiliumLoadBalancerIPPool CRDs
- **Cilium BGP**: Advertises routes from K8s cluster to router dynamically (no static routes)
- **Traffic Flow**: Internet → Router WAN (proxy ARP) → Router BGP routing table → K8s via LAN → Cilium LoadBalancer

**Critical Design Details:**
- Each public IP requires a unique MAC address (DHCP limitation)
- IPs are unbound from WAN interfaces after DHCP lease to prevent local route conflicts with BGP
- `rp_filter=0` required on WAN interfaces (asymmetric routing: packets arrive on WAN, route via LAN)
- udhcpc daemon runs in background to maintain DHCP lease renewals

## Architecture Reference

**SPEC.md is the source of truth** for this project's complete architecture, router script implementation, CRD schema, controller logic, and deployment. Always consult SPEC.md when implementing or modifying functionality.

## Development Commands

### Building and Running
```bash
# Build the operator binary
make build

# Run controller locally (not in cluster)
make run

# Build Docker image
make docker-build IMG=<registry>/cilium-dhcp-wanip-operator:tag

# Push Docker image
make docker-push IMG=<registry>/cilium-dhcp-wanip-operator:tag
```

### Code Generation
```bash
# Generate CRD manifests, RBAC, webhooks
make manifests

# Generate DeepCopy methods for API types
make generate

# Both manifests and generate (run before committing API changes)
make manifests generate
```

### Testing
```bash
# Run unit tests
make test

# Run e2e tests (creates Kind cluster automatically)
make test-e2e

# Run specific test
go test ./path/to/package -run TestName
```

### Linting and Formatting
```bash
# Format code
make fmt

# Run go vet
make vet

# Run golangci-lint
make lint

# Auto-fix linting issues
make lint-fix
```

### Deployment
```bash
# Install CRDs to cluster
make install

# Deploy operator to cluster
make deploy IMG=<registry>/cilium-dhcp-wanip-operator:tag

# Uninstall CRDs
make uninstall

# Remove operator from cluster
make undeploy

# Generate consolidated install.yaml
make build-installer IMG=<registry>/cilium-dhcp-wanip-operator:tag
```

## Kubebuilder Scaffolding

This is a Kubebuilder v4 project. Version details:
- **CLI Version**: 4.9.0
- **Layout**: go.kubebuilder.io/v4
- **Domain**: `serialx.net`
- **Minimum Kubernetes**: 1.16+
- **Kustomize**: v5.x

### Project Structure (v4)

```
.
├── cmd/
│   └── main.go                    # Main entry point
├── api/
│   └── v1/                        # API definitions (use v1 for stable, v1alpha1 for alpha)
│       └── *_types.go
├── internal/
│   └── controller/                # Controller implementations
│       └── *_controller.go
├── config/                        # Kustomize configs
│   ├── crd/                       # CRD manifests
│   ├── rbac/                      # RBAC configs
│   ├── manager/                   # Manager deployment
│   └── default/                   # Default kustomization
└── test/
    └── e2e/                       # E2E tests
```

### Scaffolding Commands

```bash
# Create new API with controller
kubebuilder create api --group <group> --version <version> --kind <Kind>

# Create defaulting and validating webhooks
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Create conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --conversion --spoke <spoke-version>
```

### Common Workflows

```bash
# After modifying *_types.go files:
make manifests generate

# Development cycle:
make install      # Install CRDs into cluster
make run          # Run controller locally

# Before committing:
make fmt vet lint test
```

After scaffolding, always run `make manifests generate` to update generated code.

## Project Status

**Current State**: Scaffolded but API/controllers not yet implemented. The operator currently has no CRDs or controllers defined.

**Next Steps**:
1. Create PublicIPClaim CRD using `kubebuilder create api`
2. Implement controller logic from SPEC.md
3. Add SSH client for router communication
4. Implement finalizers for cleanup
5. Add status tracking for WAN interfaces and MAC addresses

## Important Implementation Notes

- **SSH to Router**: Controller needs `golang.org/x/crypto/ssh` to execute scripts on router
- **Dynamic Client**: Use `k8s.io/client-go/dynamic` to interact with Cilium CRDs (support both `cilium.io/v2` and `cilium.io/v2alpha1`)
- **Secrets**: Router SSH keys stored in K8s secrets (namespace: `kube-system`, secret key: `id_rsa`)
- **MAC Generation**: Auto-generate locally-administered MACs (`02:xx:xx:xx:xx:xx`) if not specified
- **Interface Naming**: Linux interface names limited to 15 chars; sanitize claim names appropriately
- **Finalizers**: Essential for cleanup - must kill udhcpc daemon, remove proxy ARP, delete interface
- **Router Reboots**: Macvlan interfaces and DHCP daemons do NOT survive router reboots (requires manual recovery or systemd units)
