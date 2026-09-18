# ADR-057: Distribution — Native GPU Health Monitor Release

## Context

Some node-image builders require host processes instead of runtime containers. The GPU monitor needs Python and locked dependencies that must not change the host Python environment. The container and native distributions must use the same detection code.

This proposal uses a private-Python tar.gz package and tagged release integration. Upstream acceptance, release ownership, and hardware qualification remain maintainer decisions.

## Decision

Propose a GPU-monitor-only tar.gz artifact on the same tagged NVSentinel release as the existing containers and Helm chart. The archive contains private CPython, the monitor wheel, locked dependencies, configuration defaults, and a systemd unit.

## Implementation

- Keep assembly in `health-monitors/gpu-health-monitor/native/build_package.py`. Release builds require a clean checkout at the requested tag. Development builds remain available without a tag.
- Pin the standalone CPython download and digest in `.versions.yaml`, alongside the existing build-tool pins. Build the monitor wheel from the tagged source, not from a container export.
- Share the native build and CPU validation workflow between branch validation and the Release workflow. Start with Linux amd64 and host-provided DCGM 4. The build runner uses Ubuntu 24.04; this does not establish support for other operating systems.
- Make GitHub release creation depend on successful native tests, assembly, archive verification, relocated Python imports, and SBOM generation.
- Publish the archive, archive checksum, manifest, CycloneDX SBOM, and build-provenance bundle. Attest both the archive and its metadata. Refuse replacement of an existing native release asset.
- Keep node identity, credentials, configuration changes, service activation, and rollback in the consumer-owned host installer. This change does not build the three Go components or implement a host provisioning system.

## Rationale

- One source implementation keeps native and container detection behavior aligned.
- Private Python avoids dependencies on the host Python installation.
- A tar.gz artifact lets image builders control installation without requiring a new package repository.
- One build workflow prevents branch validation and tagged assembly from using different procedures.

## Consequences

### Positive

- Consumers download a versioned runtime without resolving Python dependencies on nodes.
- Release assets identify their source tag, runtime input, and installed packages.
- Existing container and Helm installation paths remain available.

### Negative

- Release owners must update and qualify bundled CPython and dependencies when security fixes are required.
- Native compatibility depends on the host OS, driver, DCGM libraries, matching bindings, and platform connector.
- CPU tests do not prove hardware fault detection or host-installer rollback.

### Mitigations

- Start with an experimental release artifact. Require the hardware checklist in the GPU monitor README before declaring production support.
- Retain checksum enforcement, isolated Python imports, immutable artifact names, and an explicit runtime prerequisite check.
- Publish a new version when inputs change. Do not replace an existing archive under the same version.

## Alternatives Considered

### Build Python dependencies in each host image

Not selected for this proposal because it duplicates upstream packaging and dependency-maintenance work in each consumer.

### Start with deb and rpm packages

Deferred because the initial consumer already owns installation and service activation. These formats can wrap the same runtime later.

### Export the container filesystem or freeze the Python application

Not selected because neither is required to run the existing monitor with private Python. Container filesystem export also couples the package to container-specific paths and dependencies.

## Notes

- Status: proposed for upstream review. Local implementation does not imply NVIDIA support or authorization to publish.
- This record adds a distribution format. It does not change the DCGM source modes in ADR-044.
- No runtime container, remediation component, or Kubernetes chart change is included.

## References

- [ADR-044: DCGM Source Modes](044-dcgm-source-modes.md)
- [Native package](../../health-monitors/gpu-health-monitor/README.md#host-native-gpu-monitor-package)
- [Release process](../../RELEASE.md)
- [Supply chain security](../../SECURITY.md)
