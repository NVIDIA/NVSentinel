# Release Process

This document outlines the release process for NVSentinel.

## Prerequisites

- Repository admin access with write permissions
- Understanding of semantic versioning (vMAJOR.MINOR.PATCH)
- Access to GitHub Actions workflows

## Release Methods

### Method 1: Automatic Release (Recommended)

For standard releases from the main branch.

**Steps**:
1. **Create and push a version tag**:
   ```bash
   git checkout main
   git pull origin main
   git tag v1.2.3
   git push origin v1.2.3
   ```

2. **Automatic workflows trigger**:
   - Lint and Test workflow validates code quality
   - Publish Containers workflow builds and publishes all images
   - Release workflow creates GitHub release and publishes Helm chart

3. **Verify artifacts**:
   - Container images in GitHub Container Registry
   - GitHub release with `versions.txt`
   - Helm chart at `oci://ghcr.io/nvidia/nvsentinel`

### Method 2: Manual Release

For rebuilding from existing tags or emergency releases.

**Container Publishing**:
1. Navigate to **Actions** → **Publish Containers**
2. Click **Run workflow** → Enter tag (e.g., `v1.2.3`) → **Run workflow**
3. Monitor build progress

**GitHub Release**:
1. Navigate to **Actions** → **Release**
2. Click **Run workflow** → Enter tag (e.g., `v1.2.3`) → **Run workflow**
3. Verify release creation

## Workflow Pipeline

```mermaid
graph LR
    A[Tag Push] --> B[Lint & Test<br/>quality gates]
    B --> C[Publish Containers<br/>all components]
    C --> D[Release<br/>GitHub release + Helm chart]
    
    style A fill:#e1f5ff
    style B fill:#fff4e1
    style C fill:#e8f5e9
    style D fill:#f3e5f5
```

## Released Components

**Container Images** are published under `ghcr.io/nvidia/nvsentinel/`. This document deliberately does not list them — a static inventory drifts. There are two sources of truth:

- **For a released version**: the `versions.txt` asset on that [GitHub release](https://github.com/NVIDIA/NVSentinel/releases), which pins every image and tag actually published.
- **For what a release will contain**: [`scripts/build-image-list.sh`](scripts/build-image-list.sh), which generates `versions.txt`. Adding a component means adding it there.

**Artifacts**:
- GitHub release with `versions.txt`
- Helm chart at `oci://ghcr.io/nvidia/nvsentinel`

## Quality Gates

All releases must pass:
- **Lint checks**: Code style, license headers, protobuf validation
- **Unit tests**: All Go modules and Python packages
- **Container builds**: All component images must build successfully
- **E2E tests**: Integration testing (on PR/push)

## Troubleshooting

**Failed Automatic Release**:
- Check **Lint and Test** workflow logs
- Review **Publish Containers** for build failures
- Use manual workflows to retry specific steps

**Manual Rebuild**:
- Use manual triggers with existing tag
- No need to create new tags for rebuilds

**Release Validation**:
```bash
# Verify versions.txt contains all components
# Check container registry for images
# Test Helm chart installation
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel --version v1.2.3
```

## Release Artifacts

### Generated Artifacts (Example: v1.2.3)

**Container Images** published to `ghcr.io/nvidia/nvsentinel/`:
- Most component images are tagged with the release tag, e.g. `ghcr.io/nvidia/nvsentinel/fault-quarantine:v1.2.3`
- `gpu-health-monitor` is the exception: it ships one image per DCGM major version, tagged `v1.2.3-dcgm-3.x` and `v1.2.3-dcgm-4.x`
- See the release's `versions.txt` for the exact set

**Helm Chart**: `oci://ghcr.io/nvidia/nvsentinel:v1.2.3`

**GitHub Release**: Includes `versions.txt` with complete artifact list and SHAs

### Verification

**View in GitHub**:
- **Packages** tab: All containers with version tag
- **Releases** tab: Release with `versions.txt`
- **Actions** tab: Workflow run logs

**Commands**:
```bash
# Pull container image
docker pull ghcr.io/nvidia/nvsentinel/syslog-health-monitor:v1.2.3

# Install Helm chart
helm install test oci://ghcr.io/nvidia/nvsentinel --version v1.2.3

# View chart metadata
helm show chart oci://ghcr.io/nvidia/nvsentinel --version v1.2.3
```

### Authentication

No custom secrets required:
- `GITHUB_TOKEN` automatically authenticates to `ghcr.io`
- Ensure repository **Settings** → **Actions** has "Read and write permissions"

## Version Management

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1`, `v1.2.3-beta1` (automatically marked in GitHub)

## Emergency Hotfix Procedure

For urgent fixes:

1. **Fix in main first**:
   ```bash
   git checkout main
   git checkout -b fix/critical-issue
   # Apply fix and create PR to main
   ```

2. **Create hotfix branch from release tag**:
   ```bash
   git checkout v1.2.3
   git checkout -b hotfix/v1.2.4
   ```

3. **Cherry-pick and release**:
   ```bash
   git cherry-pick <commit-hash-from-main>
   git tag v1.2.4
   git push origin v1.2.4  # Triggers automatic workflows
   ```
