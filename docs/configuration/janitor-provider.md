# Janitor Provider Configuration

## Overview

The Janitor Provider is the CSP-specific backend that executes node operations (reboot, terminate) on cloud infrastructure. The Janitor connects to the Janitor Provider over gRPC with TLS and ServiceAccount token authentication. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the janitor-provider module is deployed in the cluster.

```yaml
global:
  janitorProvider:
    enabled: true
```

### Replica Count

```yaml
janitor-provider:
  replicaCount: 1
```

### Resources

Defines CPU and memory resource requests and limits for the janitor-provider pod. No defaults are set; configure for production use.

```yaml
janitor-provider:
  resources: {}
```

## TLS

The Janitor Provider serves gRPC over TLS. The certificate is issued and renewed by cert-manager, which is a required dependency.

```yaml
janitor-provider:
  tls:
    enabled: true
    certDir: "/etc/nvsentinel/janitor-provider/tls"
    issuerName: "janitor-selfsigned-issuer"
    secretName: ""
```

### enabled
When `true`, the gRPC server requires TLS. Must match `janitor.config.cspProvider.tls.enabled`.

### certDir
Path inside the container where the TLS certificate and key are mounted.

### issuerName
Name of the cert-manager ClusterIssuer used to issue the gRPC server certificate.

### secretName
Name of the Kubernetes Secret where the issued certificate is stored. When empty, the chart generates a name automatically.

## Authentication

```yaml
janitor-provider:
  auth:
    enabled: true
    audiences:
      - "nvsentinel-csp-provider"
```

### enabled
When `true`, the Janitor Provider validates the ServiceAccount token presented by the Janitor on every gRPC call.

### audiences
List of accepted token audiences. Must include the value set in `janitor.config.cspProvider.auth.audience`.

## Cloud Provider Selection

```yaml
janitor-provider:
  csp:
    provider: "kind"  # Options: kind, kwok, aws, gcp, azure, oci, nebius, generic
```

Only one provider is active at a time. `kind` and `kwok` are for development only; they simulate reboots without contacting any cloud API.

## AWS

Uses IRSA (IAM Roles for Service Accounts) for authentication. The cluster must have OIDC federation configured, and the IAM role must trust the Janitor Provider's Kubernetes ServiceAccount.

```yaml
janitor-provider:
  csp:
    provider: "aws"
    aws:
      region: ""
      accountId: ""
      iamRoleName: ""
```

### region
AWS region where the EC2 instances are running (e.g. `us-west-2`).

### accountId
12-digit AWS account ID hosting the EKS cluster.

### iamRoleName
Name of the IAM role used for IRSA. The role must include `ec2:RebootInstances` and `ec2:TerminateInstances` permissions. See [CSP Health Monitor IAM Setup](../csp-health-monitor-iam.md) for the general Workload Identity pattern; apply the same approach for the Janitor Provider ServiceAccount.

## GCP

Uses Workload Identity to authenticate the Janitor Provider's Kubernetes ServiceAccount as a GCP Service Account.

```yaml
janitor-provider:
  csp:
    provider: "gcp"
    gcp:
      project: ""
      zone: ""
      serviceAccount: ""
```

### project
GCP project ID where the GKE cluster and Compute Engine instances reside.

### zone
GCP zone of the instances (e.g. `us-central1-a`).

### serviceAccount
GCP Service Account name, without the `@project.iam.gserviceaccount.com` suffix. The SA must have `compute.instances.reset` and `compute.instances.stop` permissions. See [CSP Health Monitor IAM Setup](../csp-health-monitor-iam.md) for the Workload Identity binding pattern.

## Azure

Uses Azure Workload Identity to authenticate the Janitor Provider's Kubernetes ServiceAccount as a Managed Identity.

```yaml
janitor-provider:
  csp:
    provider: "azure"
    azure:
      subscriptionId: ""
      resourceGroup: ""
      location: ""
      clientId: ""
```

### subscriptionId
Azure subscription ID containing the AKS cluster and VM resources.

### resourceGroup
Azure resource group where the VM Scale Set or individual VMs are managed.

### location
Azure region of the resources (e.g. `eastus`).

### clientId
Client ID of the Managed Identity used for Workload Identity authentication. The identity must have `Microsoft.Compute/virtualMachines/restart/action` and `Microsoft.Compute/virtualMachines/delete/action` permissions on the relevant resource group.

## OCI

Uses OCI Workload Identity or a credentials file for authentication.

```yaml
janitor-provider:
  csp:
    provider: "oci"
    oci:
      region: ""
      compartment: ""
      credentialsFile: ""
      profile: "DEFAULT"
      principalId: ""
```

### region
OCI region identifier (e.g. `us-phoenix-1`).

### compartment
OCID of the OCI compartment containing the compute instances.

### credentialsFile
Path to an OCI credentials file inside the container. Leave empty to use Workload Identity (recommended for production).

### profile
Profile name within the credentials file. Defaults to `DEFAULT`. Ignored when `credentialsFile` is empty.

### principalId
OCI principal OCID used for Workload Identity. Required when `credentialsFile` is empty.

## Generic / Bare-Metal

The `generic` provider spawns a privileged Kubernetes Job on the target node that runs `chroot /host reboot` to reboot the node. Use this for bare-metal clusters, on-premises deployments, or any environment not covered by the cloud provider backends.

```yaml
janitor-provider:
  csp:
    provider: "generic"
    generic:
      rebootImage: "public.ecr.aws/docker/library/busybox:1.37.0"
      useSysrqReboot: false
      rebootJobNamespace: ""
      rebootJobTTLSeconds: 3600
      imagePullSecrets: ""
```

### rebootImage
Container image used by the reboot Job. Must include `chroot` and standard shell utilities.

### useSysrqReboot
When `false` (default), the Job runs `chroot /host reboot` to trigger a clean OS shutdown and restart.

When `true`, the Job writes `b` to `/proc/sysrq-trigger`, triggering an immediate kernel reboot via the Linux Magic SysRq interface. Use this only when `chroot /host reboot` is unavailable — for example, when the node OS has a read-only root filesystem or a custom OS image that does not expose a reboot binary at the standard path.

### rebootJobNamespace
Kubernetes namespace where the reboot Job is created. Defaults to the janitor-provider namespace when empty.

### rebootJobTTLSeconds
Time in seconds after Job completion before Kubernetes deletes the Job and its pod. Defaults to `3600`.

### imagePullSecrets
Name of an image pull secret to attach to the reboot Job, if `rebootImage` is pulled from a private registry.
