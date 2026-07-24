# Janitor

## Overview

The Janitor is NVSentinel's remediation executor. It watches for Custom Resources (RebootNode, TerminateNode, GPUReset) created by the Fault Remediation module and executes the corresponding cloud or hardware operation through the Janitor Provider — a separate deployment that holds cloud credentials and performs the actual API calls over a TLS-secured gRPC connection authenticated with the configured CA bundle and a projected ServiceAccount token.

Think of it as a maintenance crew dispatcher — when Fault Remediation posts a work order (the CR), the Janitor picks it up, coordinates with the crew (the Janitor Provider), and sees the job through until the node is back and healthy.

### Why Do You Need This?

Creating a maintenance CR is only the first step — something has to carry out the repair:

- **Cloud API execution**: Rebooting or terminating a VM requires authenticated calls to the cloud provider (AWS, GCP, Azure, OCI); the Janitor handles this without exposing credentials to the main controller
- **Credential isolation**: The Janitor Provider runs as a separate pod with tightly scoped cloud permissions, so credentials never live alongside the main Janitor controller
- **Hardware-level GPU reset**: Some GPU faults require an in-place reset via `nvidia-smi`, not a full VM reboot; the Janitor orchestrates this as a privileged Kubernetes Job while safely pausing the GPU Operator
- **End-to-end lifecycle management**: After issuing a cloud action, the Janitor waits for the node to return to a Ready state before marking the CR complete, ensuring downstream systems see accurate remediation status

## How It Works

The Janitor watches the cluster for maintenance CRs and drives each one to completion:

1. Detects a new RebootNode, TerminateNode, or GPUReset CR created by the Fault Remediation module
2. Pauses dependent services (e.g. the GPU Operator DaemonSet) if the operation requires it
3. Calls the Janitor Provider over TLS-secured gRPC, authenticating with the configured CA bundle and a projected ServiceAccount token
4. The Janitor Provider uses its scoped cloud credentials to issue the appropriate cloud or hardware API call
5. Waits for the target node to return to a Ready state
6. Resumes any paused services
7. Marks the CR as complete; completed CRs are automatically deleted after 14 days (configurable)

In `manualMode`, steps 3–4 are skipped. The Janitor creates and monitors the CR but does not call the Janitor Provider, allowing a human operator to approve and execute the action before the Janitor resumes monitoring.

## Supported Operations

### RebootNode

Issues a reboot API call to the cloud provider. The node restarts with the same identity and rejoins the cluster.

| Provider | API call |
|----------|----------|
| AWS | `RebootInstances` |
| GCP | `reset` |
| Azure | Virtual Machine restart |
| OCI | `InstanceAction RESET` |

### TerminateNode

Issues a terminate or stop API call to the cloud provider. Used for `REPLACE_VM` actions where the node is removed and a replacement is provisioned by the cluster's node lifecycle tooling.

### GPUReset

Runs a privileged Kubernetes Job on the target node that performs an in-place GPU reset via `nvidia-smi`. The GPU Operator DaemonSet is paused on the node during the reset to prevent interference. On completion, the Job writes a syslog event so the Syslog Health Monitor can confirm the reset succeeded.

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
    manualMode: false     # Set to true to skip Provider calls; requires human approval
    nodes:
      exclusions:         # Nodes the Janitor will never act on
        - control-plane-node-1
        - critical-infra-node-1
  ttl:
    enabled: true
    defaultTTL: "336h"   # Auto-delete completed CRs after this duration (default: 14 days)

janitor-provider:
  csp:
    provider: aws         # Cloud provider: aws | gcp | azure | oci | nebius | generic | kind | kwok
```

See [Janitor configuration](configuration/janitor.md) and [Janitor Provider configuration](configuration/janitor-provider.md) for the full Helm reference.

## Cloud Provider Support

| Provider | RebootNode | TerminateNode | GPUReset |
|----------|-----------|---------------|----------|
| `aws` | Yes | Yes | Yes |
| `gcp` | Yes | Yes | Yes |
| `azure` | Yes | Yes | Yes |
| `oci` | Yes | Yes | Yes |
| `nebius` | Yes | Yes | Yes |
| `generic` | — | — | Yes (privileged Job) |
| `kind` / `kwok` | Simulated | Simulated | Simulated |

`generic` targets bare-metal environments where no cloud provider API is available; all operations are executed as privileged Kubernetes Jobs on the node. `kind` and `kwok` are development-only providers that simulate operations without performing real API calls.
