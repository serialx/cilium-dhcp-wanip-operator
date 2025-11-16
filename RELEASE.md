# Release Process

This document describes how to create a new release of the Cilium DHCP WAN IP Operator.

## Overview

Each release consists of:
1. **Operator Docker Image**: Multi-platform image built automatically by CI
2. **Router Agent Binaries**: Manually built for ARM64 and AMD64
3. **Install Manifest**: Generated Kubernetes YAML for easy installation

## Pre-Release Checklist

Before starting the release process, ensure:

- [ ] All desired changes are merged to `main` branch
- [ ] All tests pass: `make test`
- [ ] Code is properly formatted: `make fmt`
- [ ] No vet warnings: `make vet`
- [ ] No linter errors: `make lint`
- [ ] If API types were modified: `make manifests generate`
- [ ] Documentation is up to date (README.md, SPEC.md, etc.)

## Release Steps

### 1. Determine Version Number

Follow semantic versioning (vMAJOR.MINOR.PATCH):
- **MAJOR**: Breaking changes
- **MINOR**: New features, backwards compatible
- **PATCH**: Bug fixes, backwards compatible

Check current version:
```bash
git describe --tags
git tag -l --sort=-v:refname | head -5
```

### 2. Update Version and Generate Installer

Generate the installer manifest with the new version tag:

```bash
# Example for v0.3.4
export VERSION=v0.3.4
make build-installer IMG=ghcr.io/serialx/cilium-dhcp-wanip-operator:${VERSION}
```

This command:
- Updates `config/manager/kustomization.yaml` with the new image tag
- Generates `dist/install.yaml` with all CRDs and deployment manifests

**Note**: The generated file goes to `dist/install.yaml`, but it should be copied to `config/install.yaml` for version control:

```bash
cp dist/install.yaml config/install.yaml
```

### 3. Commit Version Bump

Commit the version changes **before** creating the tag:

```bash
git add config/install.yaml config/manager/kustomization.yaml
git commit -m "chore: update installer manifest for ${VERSION}"
```

**Why this order matters**: The tag should point to a commit that already contains the correct installer manifest. This ensures users who check out the tag get the matching installer.

### 4. Create and Push Git Tag

Create an annotated tag with release notes:

```bash
git tag -a ${VERSION} -m "Release ${VERSION} - Brief description of changes"
```

Push the commit and tag:

```bash
git push origin main
git push origin ${VERSION}
```

**Important**: Pushing the tag triggers the GitHub Actions workflow (`.github/workflows/docker.yml`) which automatically:
- Builds multi-platform Docker images (linux/arm64, amd64, s390x, ppc64le)
- Pushes to `ghcr.io/serialx/cilium-dhcp-wanip-operator:${VERSION}`

Monitor the build at: https://github.com/serialx/cilium-dhcp-wanip-operator/actions

### 5. Build Router Agent Binaries

While the CI builds the Docker image, build the agent binaries:

```bash
# Build for ARM64 (UDM-Pro, most modern routers)
GOOS=linux GOARCH=arm64 go build -o dhcp-wan-agent-linux-arm64 cmd/agent/main.go

# Build for AMD64 (x86_64 Linux routers, VMs)
GOOS=linux GOARCH=amd64 go build -o dhcp-wan-agent-linux-amd64 cmd/agent/main.go

# Optional: Build for other architectures if needed
# GOOS=linux GOARCH=386 go build -o dhcp-wan-agent-linux-i386 cmd/agent/main.go
```

Verify the binaries:
```bash
file dhcp-wan-agent-linux-*
```

### 6. Create GitHub Release

Wait for the Docker build to complete successfully, then create a GitHub release.

**Option A: Using GitHub CLI**

```bash
gh release create ${VERSION} \
  --title "${VERSION} - Release Title" \
  --notes "$(cat <<'EOF'
## What's Changed

- Feature 1: Description
- Fix 2: Description
- Improvement 3: Description

## Installation

**Operator:**
```bash
kubectl apply -f https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/${VERSION}/config/install.yaml
```

**Router Agent:**
```bash
# For ARM64 routers (UDM-Pro):
wget https://github.com/serialx/cilium-dhcp-wanip-operator/releases/download/${VERSION}/dhcp-wan-agent-linux-arm64
# For AMD64 routers:
wget https://github.com/serialx/cilium-dhcp-wanip-operator/releases/download/${VERSION}/dhcp-wan-agent-linux-amd64
```

See [README.md](https://github.com/serialx/cilium-dhcp-wanip-operator/blob/${VERSION}/README.md) for complete installation instructions.

**Full Changelog**: https://github.com/serialx/cilium-dhcp-wanip-operator/compare/v0.3.3...${VERSION}
EOF
)" \
  dhcp-wan-agent-linux-arm64 \
  dhcp-wan-agent-linux-amd64
```

**Option B: Using GitHub Web UI**

1. Go to https://github.com/serialx/cilium-dhcp-wanip-operator/releases/new
2. Select the tag: `${VERSION}`
3. Set release title: `${VERSION} - Release Title`
4. Write release notes (see template above)
5. Attach binaries:
   - `dhcp-wan-agent-linux-arm64`
   - `dhcp-wan-agent-linux-amd64`
6. Click "Publish release"

### 7. Verify Release

Verify the release is complete:

- [ ] GitHub release created: https://github.com/serialx/cilium-dhcp-wanip-operator/releases
- [ ] Docker image available: `docker pull ghcr.io/serialx/cilium-dhcp-wanip-operator:${VERSION}`
- [ ] Agent binaries attached to release
- [ ] Install manifest accessible at: `https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/${VERSION}/config/install.yaml`

Test installation:
```bash
# In a test cluster
kubectl apply -f https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/${VERSION}/config/install.yaml
kubectl -n cilium-dhcp-wanip-operator-system get pods
```

### 8. Update Documentation (if needed)

If the release includes significant changes, update:
- [ ] README.md references to the version (if showing specific version examples)
- [ ] DEPLOYMENT.md if installation process changed
- [ ] SPEC.md if architecture changed

## Troubleshooting

### Docker Build Fails

Check the GitHub Actions logs:
```bash
gh run list --workflow=docker.yml --limit 5
gh run view <run-id> --log
```

Common issues:
- **Build timeout**: Retry the workflow
- **Registry authentication**: Check GitHub token permissions
- **Platform build failure**: Check Dockerfile compatibility

### Tag Already Exists

If you need to move a tag (use carefully):
```bash
git tag -d ${VERSION}                    # Delete local tag
git push origin :refs/tags/${VERSION}    # Delete remote tag
# Then recreate the tag at the correct commit
```

### Wrong Version in Installer

If you forgot to update the installer before tagging:
```bash
# Move the tag (see above)
# Update installer
make build-installer IMG=ghcr.io/serialx/cilium-dhcp-wanip-operator:${VERSION}
cp dist/install.yaml config/install.yaml
git add config/install.yaml config/manager/kustomization.yaml
git commit --amend -m "chore: update installer manifest for ${VERSION}"
# Recreate tag
git tag -a ${VERSION} -m "Release ${VERSION} - Description"
git push origin main --force-with-lease
git push origin ${VERSION}
```

## Release Checklist Summary

```bash
# 1. Pre-release checks
make fmt vet lint test
make manifests generate  # if API changed

# 2. Set version
export VERSION=v0.3.4

# 3. Generate installer
make build-installer IMG=ghcr.io/serialx/cilium-dhcp-wanip-operator:${VERSION}
cp dist/install.yaml config/install.yaml

# 4. Commit
git add config/install.yaml config/manager/kustomization.yaml
git commit -m "chore: update installer manifest for ${VERSION}"

# 5. Tag and push
git tag -a ${VERSION} -m "Release ${VERSION} - Description"
git push origin main
git push origin ${VERSION}

# 6. Build agent
GOOS=linux GOARCH=arm64 go build -o dhcp-wan-agent-linux-arm64 cmd/agent/main.go
GOOS=linux GOARCH=amd64 go build -o dhcp-wan-agent-linux-amd64 cmd/agent/main.go

# 7. Create GitHub release (wait for CI to complete)
gh release create ${VERSION} \
  --title "${VERSION} - Title" \
  --notes "Release notes" \
  dhcp-wan-agent-linux-arm64 \
  dhcp-wan-agent-linux-amd64

# 8. Verify
docker pull ghcr.io/serialx/cilium-dhcp-wanip-operator:${VERSION}
kubectl apply -f https://raw.githubusercontent.com/serialx/cilium-dhcp-wanip-operator/${VERSION}/config/install.yaml
```

## Version History

- **v0.3.3** (2025-11-13): Agent integration complete + linting fixes
- **v0.3.2** (2025-11-13): Router agent HTTP API integration
- **v0.2.0** (2025-10-11): Automatic reboot recovery and reconciliation
- **v0.1.0**: Initial release
