# Release Process

This document outlines the step-by-step process for creating releases of nvsentinel.

## Prerequisites

- Repository admin access with write permissions
- Understanding of semantic versioning (e.g., v1.2.3)
- Access to GitHub Actions workflows

## Release Methods

### Method 1: Automatic Release (Recommended)

**When to use**: For standard releases from the main branch.

1. **Create and push a version tag**:
   ```bash
   git checkout main
   git pull origin main
   git tag v1.2.3
   git push origin v1.2.3
   ```

2. **Monitor automatic workflows**:
   - **Lint and Test** runs automatically for the tag
   - **Publish Containers** runs automatically on tag push
   - **Release** workflow runs automatically on tag push

3. **Verify release artifacts**:
   - Check container images in registry
   - Verify GitHub release with versions.txt
   - Confirm Helm chart publication

### Method 2: Manual Release

**When to use**: For rebuilding from existing tags or emergency releases.

#### Container Publishing

1. Navigate to **Actions** → **Publish Containers**
2. Click **"Run workflow"**
3. Enter the existing tag (e.g., `v1.2.3`)
4. Click **"Run workflow"**
5. Monitor build progress for all components

#### GitHub Release Creation

1. Navigate to **Actions** → **Release**
2. Click **"Run workflow"**
3. Enter the existing tag (e.g., `v1.2.3`)
4. Click **"Run workflow"**
5. Verify GitHub release creation and Helm chart publication

## Workflow Overview

### Automated Pipeline
```
Tag Push → Lint & Test (quality gates)
        ↓
        Publish Containers (from tag)
        ↓
        Release (from tag)
```

### Components Released

**Container Images**:
- gpu-health-monitor (dcgm3 & dcgm4 variants)
- syslog-health-monitor
- csp-health-monitor
- health-event-client
- platform-connectors
- health-events-analyzer
- fault-quarantine-module
- labeler-module
- node-drainer-module
- fault-remediation-module
- log-collector
- file-server-cleanup

**Release Artifacts**:
- GitHub release with versions.txt
- Helm chart published to ghcr.io

## Quality Gates

All releases must pass:

1. **Lint checks**: Code style, license headers, protobuf validation
2. **Unit tests**: All Go modules and health monitors
3. **Container builds**: All components must build successfully
4. **E2E tests**: Full integration testing (on PR/push)

## Troubleshooting

### Failed Automatic Release
- Check the **Lint and Test** workflow logs
- Verify container build failures in **Publish Containers**
- Use manual workflows to retry specific steps

### Manual Rebuild Required
- Use manual triggers with existing tag
- Useful for infrastructure issues or partial failures
- No need to create new tags

### Release Validation
- Verify `versions.txt` contains all expected components
- Check container registry for published images
- Test Helm chart installation:
  ```bash
  helm install nvsentinel oci://ghcr.io/[org]/nvsentinel --version v1.2.3
  ```

## Release Artifacts & Verification

### What Gets Generated (Example: v1.2.3)

**Container Images** (13 total) published to `ghcr.io/[org]/`:
- `gpu-health-monitor-dcgm3:v1.2.3`
- `gpu-health-monitor-dcgm4:v1.2.3`
- `syslog-health-monitor:v1.2.3`
- `csp-health-monitor:v1.2.3`
- `health-event-client:v1.2.3`
- `platform-connectors:v1.2.3`
- `health-events-analyzer:v1.2.3`
- `fault-quarantine-module:v1.2.3`
- `labeler-module:v1.2.3`
- `node-drainer-module:v1.2.3`
- `fault-remediation-module:v1.2.3`
- `log-collector:v1.2.3`
- `file-server-cleanup:v1.2.3`

**Helm Chart**: `ghcr.io/[org]/nvsentinel:v1.2.3`

**GitHub Release**: Contains `versions.txt` with complete artifact list and SHAs

### Where to Inspect

**Container Registry**:
- Navigate to repository **Packages** tab
- All containers appear with `v1.2.3` tag

**GitHub Release**:
- Go to **Releases** → `v1.2.3`
- Download `versions.txt` for complete manifest

**Workflow Runs**:
- **Actions** tab shows all triggered workflows
- Check logs for build details and any issues

**Verification Commands**:
```bash
# List container images
docker pull ghcr.io/[org]/syslog-health-monitor:v1.2.3

# Install Helm chart
helm install test oci://ghcr.io/[org]/nvsentinel --version v1.2.3

# View chart info
helm show chart oci://ghcr.io/[org]/nvsentinel --version v1.2.3
```

### Authentication Setup

**No custom secrets required** - workflows use GitHub's built-in authentication:
- `GITHUB_TOKEN` automatically authenticates to `ghcr.io`
- Repository permissions handle package publishing
- Ensure repository **Settings** → **Actions** has "Read and write permissions"

## Version Management

- Use semantic versioning: `vMAJOR.MINOR.PATCH`
- Pre-release versions: `v1.2.3-rc1`, `v1.2.3-beta1`
- Pre-releases are automatically marked in GitHub

## Emergency Procedures

For urgent hotfixes:
1. Create fix branch from main and apply minimal fixes
2. Create PR to merge fix to main (ensures fix is in main first)
3. After merge, create hotfix branch from the problematic release tag
4. Cherry-pick the fix commits from main to hotfix branch
5. Create new patch version tag on hotfix branch
6. Push the tag - automatic workflows will trigger

**Example scenario** (hotfix for v1.2.3):
```bash
# 1. Create fix on main first
git checkout main
git pull origin main
git checkout -b fix/critical-security-issue

# Apply minimal fix
git commit -m "fix: critical security issue"
# Create PR and merge to main

# 2. After merge, create hotfix branch from problematic release
git checkout v1.2.3
git checkout -b hotfix/v1.2.4

# 3. Cherry-pick the fix from main
git cherry-pick <commit-hash-from-main>

# Create and push new patch version tag
git tag v1.2.4
git push origin v1.2.4    # Triggers automatic workflows
```
