# Janitor

## Overview

The Janitor is the remediation executor of NVSentinel. It watches for the Custom Resources (RebootNode, TerminateNode, GPUReset) that the Fault Remediation module creates. For a RebootNode or TerminateNode CR, it does the corresponding cloud or hardware operation through the Janitor Provider. For a GPUReset CR, it runs a privileged Job on the node.

The Janitor Provider is a separate deployment that holds the cloud credentials and makes the API calls. The Janitor calls it over a TLS-secured gRPC connection. This connection authenticates with the configured CA bundle and a projected ServiceAccount token.

### Why Do You Need This?

The creation of a maintenance CR is only the first step. A component must then do the repair:

- **Cloud API execution**: A reboot or a termination of a VM requires authenticated calls to the cloud provider (AWS, GCP, Azure, OCI). The Janitor handles these calls, and does not expose the credentials to the main controller.
- **Credential isolation**: The Janitor Provider runs as a separate pod with tightly scoped cloud permissions. Thus, the credentials are never in the same pod as the main Janitor controller.
- **Hardware-level GPU reset**: Some GPU faults require an in-place reset with `nvidia-smi`, not a full VM reboot. The Janitor runs this reset as a privileged Kubernetes Job, and it safely pauses the GPU Operator during the reset.
- **End-to-end lifecycle management**: After the Janitor sends a reboot request, it waits for the node to return to a Ready state. Then it marks the CR complete. Thus, downstream systems see an accurate remediation status.

## How It Works

The Janitor watches the cluster for maintenance CRs and processes each one to completion.

For a RebootNode or TerminateNode CR, the Janitor does these steps:

1. Detects a new RebootNode or TerminateNode CR that the Fault Remediation module created
2. Calls the Janitor Provider over TLS-secured gRPC, and authenticates with the configured CA bundle and a projected ServiceAccount token
3. The Janitor Provider uses its scoped cloud credentials to make the applicable cloud or hardware API call
4. For a RebootNode CR, waits for the target node to return to a Ready state. For a TerminateNode CR, waits until the node is NotReady, and then deletes the Node object
5. Marks the CR as complete

For a GPUReset CR, the Janitor does these steps:

1. Detects a new GPUReset CR that the Fault Remediation module created
2. Pauses the dependent services that run on the node, for example the GPU Operator DaemonSet
3. Runs a privileged reset Job on the node
4. Resumes the paused services, and waits until the GPU Operator pods are Ready
5. Marks the CR as complete

The Janitor automatically deletes each CR 14 days after it first reconciles the CR (configurable).

In `manualMode`, the Janitor skips steps 2–3 for RebootNode and TerminateNode CRs. The Janitor monitors the CR, but it does not call the Janitor Provider. A human operator can then approve and do the action. After that, the Janitor resumes monitoring. Manual mode does not apply to GPUReset CRs: the Janitor still runs GPU resets.

## Supported Operations

### RebootNode

Issues a reboot API call to the cloud provider. The node restarts with the same identity and rejoins the cluster.

| Provider | API call |
|----------|----------|
| AWS | `RebootInstances` |
| GCP | `reset` |
| Azure | Virtual Machine restart |
| OCI | `InstanceAction RESET` |
| Akamai | `POST /linode/instances/{id}/reboot` |

### TerminateNode

Issues a terminate or stop API call to the cloud provider. The Janitor uses it for `REPLACE_VM` actions. For these actions, the node is removed, and the node lifecycle tooling of the cluster provisions a replacement.

### GPUReset

Runs a privileged Kubernetes Job on the target node. This Job does an in-place GPU reset with `nvidia-smi`. During the reset, the Janitor pauses the GPU Operator DaemonSet on the node to prevent interference. On completion, the Job writes a syslog event. The Syslog Health Monitor uses this syslog event to confirm that the reset succeeded.

When a GPUReset ends, the controller sets two conditions. `Complete` is `True` for each GPUReset that ended. `Succeeded` gives the outcome: `True` for a successful reset, and `False` with the failure reason for a failed reset.

GPUReset supports both GPU Operator modes: device plugin and DRA. Refer to [GPU Operator modes](configuration/janitor.md#gpu-operator-modes).

GPU clients outside the GPU Operator, in any namespace, can also hold the GPUs open. The controller also stops them before the reset. Refer to [GPU clients](configuration/janitor.md#gpu-clients).

## Before You Enable It: Apply the CRDs

The CRD manifests of the Janitor ship inside the `janitor` subchart (`nvsentinel/charts/janitor/crds/` in the OCI chart). The manifests include these CRDs:

- Three CRDs for the maintenance custom resources: `rebootnodes`, `terminatenodes` and `gpuresets`, in the `janitor.dgxc.nvidia.com` group
- `externalremediationrequests`, in the `nvsentinel.dgxc.nvidia.com` group

Helm does not apply the CRDs of a subchart during `helm upgrade`. Thus, on an existing installation, the CRDs are absent, or at an older schema, until you apply them yourself. Before you enable the module, check the CRDs:

```bash
kubectl get crd rebootnodes.janitor.dgxc.nvidia.com terminatenodes.janitor.dgxc.nvidia.com gpuresets.janitor.dgxc.nvidia.com externalremediationrequests.nvsentinel.dgxc.nvidia.com
```

Apply the CRDs from the chart, whatever the result of the check. `NotFound` means that the kinds are absent. But an existing CRD can still have an older schema. Helm installs the CRDs of a subchart once, at install, and never upgrades them:

```bash
NVSENTINEL_VERSION="<the version you are deploying>"
PULL_DIR="$(mktemp -d)"

helm pull oci://ghcr.io/nvidia/nvsentinel --version "$NVSENTINEL_VERSION" --untar --untardir "$PULL_DIR"
kubectl apply -f "$PULL_DIR/nvsentinel/charts/janitor/crds/"
```

Pull the chart at the version that you deploy, not the installed version. The controller of that version writes `status.managedServices`. The API server prunes the fields that an older CRD schema does not know. Then the Janitor loses the record of the stopped services, which it reads when it restores these services.

If you enable the module without the CRDs, these problems occur:

- The Janitor controller crash-loops on `no matches for kind "GPUReset"`.
- Fault Remediation cannot create its maintenance CRs (`no matches for kind "RebootNode"`).
- Remediation requests retry forever and never reach an executor.

The same pre-flight check for lifecycle-manager is in [Requesting node maintenance](tutorials/requesting-node-maintenance.md).

## Configuration

Configure the Janitor and Janitor Provider through Helm values:

```yaml
global:
  janitor:
    enabled: true
  janitorProvider:
    enabled: true

janitor:
  config:
    manualMode: false     # Manual mode - if true, controllers won't send actual reboot/terminate signals
    nodes:
      exclusions:         # Label selectors; matching nodes are refused new Janitor CRs
        - matchLabels:
            node-role.kubernetes.io/control-plane: ""
        - matchLabels:
            kubernetes.io/hostname: critical-infra-node-1
  ttl:
    enabled: true
    defaultTTL: "336h"  # 14 days

janitor-provider:
  csp:
    provider: aws         # Cloud provider: aws | gcp | azure | oci | nebius | lambda | akamai | generic | label | kind
```

For the full Helm reference, see [Janitor configuration](configuration/janitor.md) and [Janitor Provider configuration](configuration/janitor-provider.md).

## Cloud Provider Support

| Provider | RebootNode | TerminateNode | GPUReset |
|----------|-----------|---------------|----------|
| `aws` | Yes | Yes | Yes |
| `gcp` | Yes | Yes | Yes |
| `azure` | Yes | Yes | Yes |
| `oci` | Yes | Yes | Yes |
| `nebius` | Yes | Yes | Yes |
| `akamai` | Yes | Yes | Yes |
| `lambda` | Yes | Yes | Yes |
| `label` | Yes (node label) | Yes (node label) | Yes |
| `generic` | Yes (privileged Job) | — | Yes (privileged Job) |
| `kind` | Simulated | Simulated | Simulated |

`generic` is for bare-metal environments that have no cloud provider API. With this provider, all supported operations run as privileged Kubernetes Jobs on the node. `kind` is a development-only provider. It simulates operations and does not make real API calls.
