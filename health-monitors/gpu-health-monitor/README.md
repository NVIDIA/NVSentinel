# GPU Health Monitor

Health monitor for monitoring the health of GPUs


## Incident reporting

Each GPU and watch reports every distinct error code. Repeated incidents for the same code share one event with their combined messages. Each event retains its code-specific remediation action.

Suppression and debounce apply separately to each code. A healthy event clears the watch only when that GPU has no remaining reported incidents. Cache updates occur after successful delivery.

## Host-native GPU monitor package

This package runs the existing GPU monitor as a native systemd process. It does not use a runtime container. It does not install or change the operating system's Python environment.

This branch adds an experimental native artifact to the NVSentinel release workflow. It is not an existing upstream release or a production-support commitment. Maintainers must accept the proposal and qualify the target hosts before production use. See [ADR-057](../../docs/designs/057-native-gpu-monitor-release.md).

This target packages only gpu-health-monitor. It does not build or install the other NVSentinel components. The host must provide a compatible platform connector and GPU metadata separately.

### Design and ownership

The archive contains private CPython, the monitor wheel, dependencies pinned by poetry.lock, configuration defaults, and a systemd unit. Its manifest records the source revision, release tag, uncommitted-source status, build tools, Python archive checksum, lock checksum, and installed Python package versions. Tagged CI builds also produce a CycloneDX SBOM and build provenance for the archive and metadata.

The host owns the NVIDIA driver, DCGM 4 native libraries, and matching DCGM Python bindings. Keep those bindings and libraries from the same DCGM package release. The native launcher connects to the existing node-local hostengine. It does not start another hostengine.

The host installer owns node identity, package selection, configuration, activation, and rollback. It consumes this archive without resolving Python dependencies on the node. No specific host provisioning system is required.

Two packaging approaches were considered:

- A private interpreter and installed wheel preserve standard Python imports and package metadata. This is the selected proof-of-concept approach.
- Freezing the application introduces additional handling for dynamic DCGM imports and package metadata. That work is not required for this experiment.

The monitor's detection code, protobufs, event interface, Helm deployment, and DCGM source-mode behavior stay unchanged. See [ADR-044](../../docs/designs/044-dcgm-source-modes.md).

### Tagged releases

The Release workflow calls `.github/workflows/native-package.yml` with the NVSentinel release tag. The same native workflow validates relevant branch changes without publication. It initially builds Linux amd64 on Ubuntu 24.04 with the pinned private Python runtime in `.versions.yaml`. DCGM 4 and matching Python bindings remain host prerequisites. Other operating systems and arm64 are not qualified by this workflow.

The build uses a clean checkout at the exact tag. It rejects tracked and untracked edits. Stable tag `v1.2.3` produces package version `1.2.3`. Prerelease tags use Python version syntax inside the package: `v1.2.3-rc1` becomes `1.2.3rc1`, `-alpha1` becomes `a1`, and `-beta1` becomes `b1`. The manifest preserves the original tag.

Each release includes these native assets, using `1.2.3` as an example:

- `gpu-health-monitor-1.2.3-linux-amd64.tar.gz`
- `gpu-health-monitor-1.2.3-linux-amd64.tar.gz.sha256`
- `gpu-health-monitor-1.2.3-linux-amd64.manifest.json`
- `gpu-health-monitor-1.2.3-linux-amd64.cdx.json`
- `gpu-health-monitor-native-linux-amd64.sigstore.json`

The manifest also remains inside the archive. The SBOM describes the extracted runtime, not the source checkout. CI verifies its package identity, CPython version, and coverage of every installed Python distribution. The import probe must leave the extracted payload unchanged. The provenance bundle covers the archive, checksum, manifest, and SBOM. See [verification instructions](../../SECURITY.md#native-gpu-monitor-artifacts).

The Release workflow requires the native tests, full package build, checksum verification, relocated private-Python imports, and SBOM generation to succeed. These CPU checks do not prove systemd startup or GPU fault reporting. The hardware checklist below remains a separate qualification requirement.

For a manual Release run, select the requested tag in both the workflow-ref selector and the tag input. Running the workflow from main for another tag is rejected. This keeps the provenance source ref consistent with the package source. Tags created before this workflow exists cannot use this native release path.

Native release assets cannot be replaced. Re-running a completed publication fails instead of overwriting its files. If publication stops after uploading only some files, maintainers must inspect the partial release and resolve it explicitly. A changed runtime or dependency requires a new release version.

NVSentinel release owners must maintain the private Python runtime and its dependencies. Review the SBOM, licenses, and known vulnerabilities when these inputs change. The host installer must verify provenance and pin the archive digest before activation. It must not run pip to resolve dependencies during node provisioning.

### Build

Use Linux on the target architecture, Python 3.11.8 or newer, Poetry, and poetry-plugin-export. Use the repository's tool versions from .versions.yaml. A Linux build container is permitted; it is not part of the installed runtime.

Obtain an approved python-build-standalone install_only or install_only_stripped archive for the target architecture. Pin its SHA-256 from a trusted release manifest. The build deliberately requires both inputs; it does not select or download a moving Python version.

~~~sh
make -C health-monitors/gpu-health-monitor native-package \
  NATIVE_PYTHON_ARCHIVE=/path/to/pinned-cpython-linux-archive.tar.gz \
  NATIVE_PYTHON_SHA256="$APPROVED_PYTHON_SHA256" \
  NATIVE_PACKAGE_VERSION=1.22.0+native.1
~~~

The build reads poetry.lock without changing it, exports hashed runtime requirements, and installs binary wheels into private Python. It builds the monitor wheel in a temporary source tree. It does not relax dependency constraints or edit generated protobuf code.

For a local reproduction of a tagged build, use the runtime URL and SHA-256 from that tag's `.versions.yaml`. Check out the tag in a clean repository, then replace `NATIVE_PACKAGE_VERSION` with `NATIVE_RELEASE_TAG=v1.2.3`. Do not set both. CI obtains these inputs automatically. A development build can include local edits; a release build cannot.

The output is a tar.gz archive and a matching .sha256 file under dist/native. Without a version argument, the package uses the project version plus the source revision. Use a new package version whenever any package input changes. Existing artifact names cannot be overwritten.

The build stages the complete archive before publication. The final archive appears only after its checksum file is present. Failed writes do not leave a partial archive. Concurrent builds cannot overwrite each other. A forced kill or power loss can leave a checksum without an archive; inspect that output before retrying.

The archive uses normalized timestamps and ownership. Full byte-for-byte reproducibility is not guaranteed: Python build tools can add path-dependent metadata. CI pins the direct build tools, but does not yet lock their complete transitive dependency tree.

Build once per target architecture. Test the resulting native libraries on each supported OS. A portable interpreter does not remove glibc, DCGM, driver, or CPU compatibility requirements.

Use a case-sensitive Linux filesystem for the build output and staging tree. CPython includes case-sensitive terminfo names. On Docker Desktop, build in a Linux volume or tmpfs, then copy only the finished archive to the host. A case-insensitive host bind mount cannot safely hold that runtime tree.

An offline build can use PIP_NO_INDEX=1 and PIP_FIND_LINKS pointing at a prepared wheel cache. Dependency hashes from poetry.lock are still required.

Set package-source and certificate options through PIP_INDEX_URL, PIP_CERT, or other pip network environment variables. The build ignores pip configuration files and removes install-path overrides. This prevents PIP_TARGET, PIP_PREFIX, PIP_ROOT, or PIP_USER from moving dependencies outside the private runtime.

### Runtime

Install the archive under a versioned directory such as:

~~~text
/opt/nvsentinel/gpu-health-monitor/releases/1.22.0+native.1/
~~~

The current symlink selects the active version. The packaged executable resolves its own location and runs private Python with isolated imports. Ambient PYTHONPATH, user site-packages, and the current directory cannot replace its Python dependencies.

Before activation, run:

~~~sh
/opt/nvsentinel/gpu-health-monitor/current/bin/gpu_health_monitor --check-runtime
~~~

This validates library loading, bindings, and Python imports. It does not prove hostengine connectivity or GPU health.

At startup, the launcher emits a native_runtime_validated JSON record to standard error. It identifies the interpreter path, Python version, package version, process ID, and DCGM binding path. systemd normally records it in the journal.

The service reads these host-owned files:

- /etc/nvsentinel/gpu-health-monitor/environment: DCGM location, metrics port, metadata path, and processing strategy.
- /etc/nvsentinel/gpu-health-monitor/node.env: NODE_NAME for this node. Generate it during node provisioning, not image baking.
- /etc/nvsentinel/gpu-health-monitor/config.ini: monitor configuration, including the actual host socket path.

The DCGM error mapping is package-owned under current/etc/dcgmerrorsmapping.csv. Upgrades and rollback select the matching mapping with the package.

The default metrics port is 2114. The default connector socket is /run/nvsentinel/nvsentinel.sock. The host must supply metadata at /var/lib/nvsentinel/gpu_metadata.json. Missing metadata reduces enrichment and metadata-dependent checks.

State remains under /var/lib/nvsentinel/gpu-health-monitor across package upgrades. The host installer must preserve configuration and old package directories.

The systemd unit uses the native launcher's watchdog. The launcher sends readiness after /healthz responds and sends keep-alive messages only while that endpoint is healthy. A stale polling loop eventually causes systemd to restart the process. DCGM disconnection alone does not cause watchdog restarts while the polling loop remains responsive.

The service currently runs as root, with filesystem restrictions. Review privileges and the monitor's metrics listener exposure before production use. Service readiness means process liveness, not successful GPU discovery or downstream event delivery.

### Detection without NVSentinel remediation

Leave NVSentinel remediation components disabled. Do not choose STORE_ONLY merely to disable remediation if the consumer needs Kubernetes node conditions: STORE_ONLY events do not produce those conditions.

### Validation

~~~sh
make -C health-monitors/gpu-health-monitor native-test
~~~

CPU-only tests cover archive safety, checksums, stable archive output, publication failures, concurrent builds, isolated installation, and watchdog behavior. Consumers must validate their host installer's activation and rollback behavior separately.

After a trusted local build, check the complete archive on Linux. Run this command from the GPU monitor module directory:

~~~sh
python3 native/check_package.py dist/native/gpu-health-monitor-1.2.3-linux-amd64.tar.gz \
  --extract-dir dist/native-verified --release-tag v1.2.3
~~~

This executes the packaged Python, checks generated protobuf imports and dependency isolation, and writes a manifest sidecar. Use a new extraction directory. This is a build test, not an installer or an authenticity check for untrusted downloads.

Before production rollout, run these checks with your host installer on approved disposable GPU nodes:

1. Start with no system Python dependencies installed for this monitor.
2. Confirm runtime checks, hostengine connection, GPU discovery, and expected metadata.
3. Confirm healthy events reach the platform connector without false faults.
4. Inject an approved synthetic DCGM fault and confirm the expected unhealthy event and downstream node condition.
5. Clear the fault and confirm recovery.
6. Restart DCGM and the platform connector separately; confirm reconnection and resumed reporting.
7. Block the polling loop in a test environment; confirm watchdog restart and clean shutdown.
8. Install a new package version; confirm configuration and state remain intact.
9. Test a failed upgrade; confirm the previous package resumes.
10. Confirm the intended external consumer receives the detection signal while NVSentinel remediation remains disabled.

CPU-only package tests do not validate GPU hardware, NVIDIA driver behavior, or external consumers. Publishing an experimental artifact does not complete these checks or establish production support.
