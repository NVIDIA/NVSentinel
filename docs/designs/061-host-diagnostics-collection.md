# ADR-061: Fault Management — Cloud-Independent Host Diagnostics in Log Collector

Status: Proposed for upstream review (tracking issue #1955). Maintainer acceptance is pending; no implementation lands before it.

## Context

When fault-remediation takes a supported action on a node, it starts a log-collector Job on that node, in parallel with the drain, so diagnostics are captured before the node is rebooted or replaced (`docs/log-collection.md`). The Job collects:

- `nvidia-bug-report`;
- optionally, GPU Operator must-gather (cluster-scoped, off by default);
- a host report (`sos report`), but **only** inside the GCP and AWS detection branches of `log-collector/entrypoint.sh`.

On-prem and other-cloud nodes therefore get no host report. This is true even on distributions that install host diagnostic tools by default: SUSE Linux Enterprise Server 16 ships both `sos` and `supportconfig` (supportutils). SUSE support works from `supportconfig` archives for the OS, and from Rancher support bundles (`rancher/support-bundle-kit`) for Kubernetes objects, pod logs and node bundles. Neither of those is in the current artifact set.

Measured on an idle SLES 16 node (arm64, 80 cores, btrfs root):

| `supportconfig` invocation | Time |
|---|---|
| default | 598 s |
| `-k -c` (traced) | 687 s: btrfs ≈5 min, sysfs/memory ≈5 min, networking ≈2 min, zypper update checks ≈1.5 min |
| default minus `BTRFS,UP,SYSFS,SAR,SMART` | 470 s |
| `-m -k -c -q` (minimum set) | 17 s |

The log-collector's default timeout is 10 minutes.

Options considered:

1. Distro-specific branches (`if SUSE then supportconfig`), next to the cloud branches.
2. A generic, cloud-independent host-diagnostics step with tool auto-detection and a runtime profile.
3. A free-form "run this command and collect this glob" hook.

## Decision

Add an opt-in, cloud-independent host-diagnostics step to log-collector. It auto-detects `supportconfig` or `sos` and has a `minimal` (default) and a `full` profile. Separately, add an opt-in, rate-limited Rancher support bundle collection modelled on GPU Operator must-gather. Both default off; the GCP/AWS SOS behaviour is unchanged.

## Implementation

- Helm (`charts/fault-remediation`), passed to the Job as environment variables like the existing log-collector settings:
  ```yaml
  logCollector:
    hostDiagnostics:
      enabled: false
      tool: auto        # auto | supportconfig | sos
      profile: minimal  # minimal | full
      extraArgs: []
    rancherSupportBundle:
      enabled: false
      image: rancher/support-bundle-kit:<pinned>
      minInterval: 30m
  ```
- `log-collector/entrypoint.sh`: a new step after `nvidia-bug-report`, outside the cloud branches, running via the existing `chroot /host`:
  - `supportconfig`
    - minimal: `-Q -m -k -c -q -R <dir>`
    - full: `-Q -k -c -q -R <dir>`
    - artifact: `scc_*.txz` plus its `.md5`
  - `sos`: `sos report --batch --tmp-dir=<dir>` (the existing invocation without `--all-logs` for minimal), artifact `sosreport-*.tar.*`
  - It writes to a unique per-run directory, not a shared `/var/tmp`, and removes it after upload. That avoids the time-window glob the cloud branches use.
  - `extraArgs` are appended verbatim.
- If the GCP or AWS branch already produced a SOS report in the same run, host diagnostics are skipped, so a node never gets two host reports.
- Rancher support bundle: created once per `minInterval`, cluster-wide, using a dedicated ServiceAccount with read-only cluster access. The log-collector waits for the manager to finish and uploads the zip. Not rendered at all when disabled.
- Timeout: `profile: full` documents that `logCollector.timeout` must be raised (≥ 20m suggested), and the chart warns in NOTES when `full` is used with the default 10m. *Open question for maintainers:* auto-raise instead?
- Tests: entrypoint mock mode gains mock `supportconfig`/`sos`; helm-unittest covers env wiring and the defaults-off render; the defaults render byte-identically.

## Rationale

- Keeps distribution knowledge in one detection function rather than per-vendor branches (option 1), and stays meaningful on any distribution that ships `sos`.
- The `minimal` profile fits the existing 10-minute budget, measured at 17 s. A full report is still one setting away when support asks for it.
- `-k` stops `supportconfig` from loading kernel modules on the node being diagnosed. `-c` skips update-server checks that stall on air-gapped nodes.
- Rancher support bundles cover what host reports cannot (Kubernetes objects and pod logs across the cluster). Must-gather already set the precedent for optional, cluster-scoped collection.

## Consequences

### Positive
- On-prem clusters get the host report support teams ask for first, captured before remediation destroys the evidence.
- SUSE users get the exact artifacts SUSE support consumes.

### Negative
- Host reports can contain sensitive data (configuration, logs).
- The Rancher bundle adds an optional external image and cluster-wide read permissions.
- `full` profiles exceed the default timeout.

### Mitigations
- Everything is off by default. Artifacts go to the existing in-cluster file server with its existing access controls.
- `supportconfig -j` (scrub) can be passed through `extraArgs`.
- The bundle's RBAC is only rendered when enabled, and the bundle is rate-limited.
- The chart warns about timeouts for `full`.

## Alternatives Considered

### SUSE-specific branch next to the GCP/AWS branches
**Rejected** because it multiplies vendor branches in the entrypoint, and on-prem `sos` users would still get nothing.

### Free-form command + glob hook
**Rejected** as the primary interface because a raw `chroot /host <command>` setting is an unbounded privileged surface, and every user would have to know each tool's flags and output names. `extraArgs` keeps the useful flexibility.

### Running `supportconfig` with default options
**Rejected** as the default because it took 598 s on an idle node (687 s traced), which exceeds the 10-minute Job timeout under realistic load.

## Notes

- Non-goal: parsing these reports for detection. They are evidence for humans.
- The GCP/AWS SOS flags keep their current meaning.

## References

- Tracking issue: #1955

- `docs/log-collection.md`, `log-collector/entrypoint.sh`
- supportutils `supportconfig -h` (3.2.14): `-m`, `-k`, `-c`, `-q`, `-x`, `-R`
- https://github.com/rancher/support-bundle-kit (manager / standalone mode)
