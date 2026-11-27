# Janitor Provider Configuration

## Overview

The Janitor Provider is the CSP-specific backend that does node operations (reboot, terminate) on cloud infrastructure. The Janitor connects to the Janitor Provider over gRPC with TLS and ServiceAccount token authentication. This document gives all the Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the Helm chart deploys the Janitor Provider module in the cluster.

```yaml
global:
  janitorProvider:
    enabled: true
```

### Logging

Sets the verbosity level for janitor-provider logs.

```yaml
janitor-provider:
  logLevel: info  # Options: debug, info, warn, error
```

### Replica Count

```yaml
janitor-provider:
  replicaCount: 1
```

### Resources

Defines CPU and memory resource requests and limits for the janitor-provider pod. The chart sets no defaults. Configure these values for production use.

```yaml
janitor-provider:
  resources: {}
```

## TLS

The Janitor Provider serves gRPC over TLS. cert-manager issues and renews the certificate, and is a required dependency.

```yaml
janitor-provider:
  tls:
    enabled: true
    certDir: "/etc/nvsentinel/janitor-provider/tls"
    issuerName: "janitor-selfsigned-issuer"
    secretName: ""
```

### enabled

When `true`, the gRPC server requires TLS. This value must match `janitor.config.cspProvider.tls.enabled`.

### certDir

Path inside the container at which the TLS certificate and key are mounted.

### issuerName

Name of the cert-manager Issuer that issues the gRPC server certificate.

### secretName

Name of the Kubernetes Secret that stores the issued certificate. If it is empty, the chart generates a name automatically.

## Authentication

```yaml
janitor-provider:
  auth:
    enabled: true
    audiences:
      - "nvsentinel-csp-provider"
```

### enabled

When `true`, the Janitor Provider validates the ServiceAccount token that the Janitor sends on every gRPC call.

### audiences

List of accepted token audiences. This list must include the value of `janitor.config.cspProvider.auth.audience`.

## Cloud Provider Selection

```yaml
janitor-provider:
  csp:
    provider: "kind"  # Options: kind, aws, gcp, azure, oci, nebius, lambda, akamai, generic, label
```

Only one provider is active at a time. `kind` is for development only. It simulates reboots and does not contact a cloud API.

## AWS

This provider uses IRSA (IAM Roles for Service Accounts) for authentication. The cluster must have OIDC federation configured. The IAM role must trust the Kubernetes ServiceAccount of the Janitor Provider.

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

AWS region in which the EC2 instances run, for example `us-west-2`.

### accountId

12-digit ID of the AWS account that hosts the EKS cluster.

### iamRoleName

Name of the IAM role for IRSA. The role must include the `ec2:RebootInstances` and `ec2:TerminateInstances` permissions. For the general Workload Identity pattern, see [CSP Health Monitor IAM Setup](../csp-health-monitor-iam.md). Apply the same approach to the Janitor Provider ServiceAccount.

## GCP

This provider uses Workload Identity to authenticate the Kubernetes ServiceAccount of the Janitor Provider as a GCP Service Account.

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

ID of the GCP project that contains the GKE cluster and the Compute Engine instances.

### zone

GCP zone of the instances, for example `us-central1-a`.

### serviceAccount

GCP Service Account name, without the `@project.iam.gserviceaccount.com` suffix. The SA must have the `compute.instances.reset` and `compute.instances.stop` permissions. For the Workload Identity binding pattern, see [CSP Health Monitor IAM Setup](../csp-health-monitor-iam.md).

## Azure

This provider uses Azure Workload Identity to authenticate the Kubernetes ServiceAccount of the Janitor Provider as a Managed Identity.

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

ID of the Azure subscription that contains the AKS cluster and the VM resources.

### resourceGroup

Azure resource group in which the VM Scale Set or the individual VMs are managed.

### location

Azure region of the resources, for example `eastus`.

### clientId

Client ID of the Managed Identity for Workload Identity authentication. The identity must have the `Microsoft.Compute/virtualMachines/restart/action` and `Microsoft.Compute/virtualMachines/delete/action` permissions on the relevant resource group.

## OCI

This provider uses OCI Workload Identity or a credentials file for authentication.

OCI maps all `RebootNode` resources to the `RESET` action. `RESET` powers off the instance immediately, and does not wait for the operating system. Drain the workloads before you create the resource. An immediate power-off can cause data corruption.

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

OCI region identifier, for example `us-phoenix-1`.

### compartment

OCID of the OCI compartment that contains the compute instances.

### credentialsFile

Path to an OCI credentials file inside the container. To use Workload Identity (recommended for production), keep it empty.

### profile

Profile name in the credentials file. The default is `DEFAULT`. The provider ignores this value when `credentialsFile` is empty.

### principalId

OCI principal OCID for Workload Identity. This value is required when `credentialsFile` is empty.

## Nebius

This provider is for Nebius Managed Kubernetes (MK8s) clusters. It uses the official Nebius Go SDK, and authenticates with a service account key or an IAM token. The identity needs the `compute.instances.stop` and `compute.instances.start` permissions.

```yaml
janitor-provider:
  csp:
    provider: "nebius"
    nebius:
      serviceAccountKeySecret: "nebius-sa-credentials"
```

Configure exactly one credential. Prefer a service account key. IAM tokens expire and need a manual refresh. Thus, they suit testing, not production.

### serviceAccountKeySecret

Secret that holds the service account key file under the key `sa-credentials.json`. The chart mounts it at `/etc/nebius` and sets `NEBIUS_SA_KEY_FILE` for you. This is the recommended production option.

### serviceAccountKeyFile

Path to a service account key file that is already present inside the container. The file uses the JSON format that the Nebius SDK documents. The chart passes the path as `NEBIUS_SA_KEY_FILE`. If you mount the credential yourself, use this option.

### iamToken

A Nebius IAM token that you supply directly. The chart passes it as `NEBIUS_IAM_TOKEN`. To get a token, use `nebius iam get-access-token`. Use this option for testing only. The token expires, and then remediation stops working.

### iamTokenSecretRef

Secret that holds an IAM token. Use it instead of writing the token into Helm values. It keeps the token out of your values file, but the token still expires.

```yaml
janitor-provider:
  csp:
    nebius:
      iamTokenSecretRef:
        name: nebius-janitor-token
        key: token
```

## Lambda

Reboot maps to the Lambda power-cycle operation (a host-level power cycle, not a guest restart). Terminate maps to the Lambda terminate operation. Nodes must carry `spec.providerID` in the form `lambda://<instanceID>`.

Authenticate with a static API key or with workload identity. Workload identity is the preferred option. With workload identity, the cluster stores no Lambda credential, and the token that it uses is short-lived and rotates automatically.

```yaml
janitor-provider:
  csp:
    provider: "lambda"
    lambda:
      apiKeySecretRef:
        name: "lambda-api-key"
        key: "LAMBDA_API_KEY"
```

### apiEndpoint

Optional. Overrides the base URL of the Lambda Cloud API. The chart passes it as `LAMBDA_API_ENDPOINT`. If it is not set, the default is `https://cloud.lambda.ai`.

The URL must use https, and must not carry userinfo, a query string, or a fragment. The provider checks the host against a fixed allowlist built into the provider. Thus, an unapproved host fails at startup, not on the first remediation.

### apiKeySecretRef

Secret that holds the Lambda API key. The chart injects the key as `LAMBDA_API_KEY`. If `csp.provider` is `lambda`, `name` is required, unless you set `workloadIdentity.identityLRN`. The install fails without one of the two. `key` defaults to `LAMBDA_API_KEY`.

You can share the same Secret with the CSP Health Monitor, which reads the key in the same way. The key needs permission to power cycle and terminate instances.

### workloadIdentity.identityLRN

The service identity to assume, in the form `lrn:iam:identity:<id>`. If you set it, the chart annotates the janitor-provider ServiceAccount with `lambda.ai/identity-lrn`. The pod then gets a short-lived Kubernetes token. The provider exchanges this token for a Lambda API key, and refreshes the key before it expires. Thus, the cluster stores no Lambda credential.

```yaml
janitor-provider:
  csp:
    provider: "lambda"
    lambda:
      workloadIdentity:
        identityLRN: "lrn:iam:identity:3cd2d107c6a347eeb0ef9498820d637d"
```

If you set it, `apiKeySecretRef` is not required, and the chart puts no `LAMBDA_API_KEY` in the pod. If you configure both, workload identity wins, and the provider ignores the static key. At startup, the provider logs the credential that it selected (`authMode`).

On the Lambda side, the identity must meet these requirements:

- It must exist.
- It must hold permission to power cycle and terminate instances.
- It must trust the ServiceAccount of this cluster.

#### Setting up the identity

Run this script one time for each cluster, with an admin API key. `$WS` is the workspace that the instances of the cluster belong to.

```bash
# Stop at the first failed call: a half-applied setup prints an identityLRN
# that looks usable but has no role or no trust behind it.
set -euo pipefail

KEY=<admin-api-key>
export WS=<workspace-id>

lambda_curl() {
  printf 'Authorization: Bearer %s\n' "$KEY" | curl -sf -H @- "$@"
}

# 1. Create a service identity for the Janitor Provider.
SID=$(lambda_curl "https://cloud.lambda.ai/api/v1/identities" \
  -H 'Content-Type: application/json' \
  -d '{"display_name":"nvsentinel-janitor-provider"}' | jq -r .data.id)
[ -n "$SID" ] && [ "$SID" != "null" ] || { echo "identity was not created" >&2; exit 1; }

# 2. Add it to the workspace. A workspace-scoped role assignment needs membership.
lambda_curl "https://cloud.lambda.ai/api/v1/workspaces/$WS/memberships" \
  -H 'Content-Type: application/json' \
  -d "{\"member_type\":\"identity\",\"member_id\":\"$SID\"}"

# 3. Assign the built-in roles, scoped to that workspace.
for role in instance-reader instance-power-cycle instance-terminate; do
  ROLE=$(lambda_curl "https://cloud.lambda.ai/api/v1/roles" \
    | jq -r --arg n "$role" '.data[] | select(.name==$n) | .id')
  [ -n "$ROLE" ] && [ "$ROLE" != "null" ] || { echo "role $role not found" >&2; exit 1; }
  lambda_curl "https://cloud.lambda.ai/api/v1/identities/$SID/role-assignments" \
    -H 'Content-Type: application/json' \
    -d "{\"role_id\":\"$ROLE\",\"scope\":{\"type\":\"workspace\",\"workspace_id\":\"$WS\"}}"
done

# 4. Trust this cluster's ServiceAccount to assume the identity. Registering the
#    issuer is an idempotent upsert keyed on the issuer URL, so re-running is safe.
SUBJECT=$(
  kubectl get deployments --all-namespaces \
    -l app.kubernetes.io/name=janitor-provider -o json |
    jq -er '
      .items
      | if length == 1 then .[0] else error("expected exactly one janitor-provider Deployment") end
      | "system:serviceaccount:\(.metadata.namespace):\(.spec.template.spec.serviceAccountName)"
    '
)
ISS=$(kubectl get --raw /.well-known/openid-configuration |
  jq -er '.issuer | select(type == "string" and length > 0)') ||
  { echo "OIDC discovery returned no valid issuer" >&2; exit 1; }
kubectl get --raw /openid/v1/jwks > /tmp/jwks.json
lambda_curl "https://cloud.lambda.ai/api/v1/oidc-providers" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg iss "$ISS" --arg lrn "lrn:iam:identity:$SID" \
        --arg subject "$SUBJECT" --slurpfile jwks /tmp/jwks.json \
        '{issuer_url:$iss, jwks:$jwks[0],
          trusts:[{identity_lrn:$lrn, subject:$subject}]}')"

echo "identityLRN: lrn:iam:identity:$SID"
```

Use the printed LRN as the Helm value above.

| Built-in role | Grants | Needed for |
| --- | --- | --- |
| `instance-reader` | `compute:instance:read` | Reading the instance state before and after a reboot |
| `instance-power-cycle` | `compute:instance:power-cycle` | The reboot itself |
| `instance-terminate` | `compute:instance:terminate` | Terminating a node that the remediation policy replaces instead of rebooting it |

All three roles are workspace-scoped. Thus, the identity can act only on instances in `$WS`.

If your remediation policy only reboots and never terminates, remove `instance-terminate` from the loop. The roles are additive. Thus, if you grant only the other two roles, terminate stays unauthorized.

#### Verifying

A pod gets the identity only when Kubernetes creates the pod. Thus, only pods that Kubernetes creates *after* the annotation lands have the identity. If you added the annotation to a running install, restart the deployment.

```bash
(
  set -euo pipefail

  NAMESPACE=$(
    kubectl get deployments --all-namespaces \
      -l app.kubernetes.io/name=janitor-provider -o json |
      jq -er '
        .items
        | if length == 1 then .[0] else error("expected exactly one janitor-provider Deployment") end
        | .metadata.namespace
      '
  )

  # The identity the pod received.
  kubectl -n "$NAMESPACE" get pod -l app.kubernetes.io/name=janitor-provider \
    -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="LAMBDA_IDENTITY_LRN")]}'

  # Which credential the provider selected.
  kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=janitor-provider | grep authMode
)
```

Expect `authMode=workload-identity`. If it shows `api-key`, the pod never received an identity. Check that the annotation is on the ServiceAccount. Also check that Kubernetes created the pod after the annotation landed.

Failures in the exchange itself appear on the first remediation, not at startup. The reason is that the provider mints the token only when it first needs it. When the exchange endpoint rejects the token, it returns `401` with no detail. This behavior is intentional, so that an unauthenticated caller cannot probe for the identities that exist. Thus, a missing trust, a wrong identity LRN and a disabled account all look identical from the client. Check the trust and the identity LRN first.

## Akamai

Reboot maps to the Linode reboot operation. Terminate does one of these actions:

- If the instance belongs to an LKE cluster, terminate recycles the node through LKE. The recycle deletes the node and provisions a replacement, so the node pool keeps its size.
- Otherwise, terminate deletes the instance directly.

Nodes must carry `spec.providerID` in the form `linode://<instanceID>`. The Linode cloud controller manager sets this value on LKE nodes.

**Warning:** Before you let the Janitor terminate a node, move any data that you need off the node. A terminate replaces the node, so you lose everything on the disk of the node. This includes `hostPath` and `emptyDir` volumes and local PersistentVolumes.

Authenticate with a static API token. The provider reports a reboot as complete only when both of these conditions are true:

- The instance reports `running`.
- The `bootID` of the kubelet is different from the one that the provider recorded before the reboot.

The `bootID` check is the important one. The instance can still show `running` for some time after the API accepts the request.

```yaml
global:
  janitor:
    enabled: true
  janitorProvider:
    enabled: true

janitor-provider:
  csp:
    provider: "akamai"
    akamai:
      apiTokenSecretRef:
        name: "akamai-api-token"
        key: "LINODE_TOKEN"
```

### apiEndpoint

Optional. Overrides the base URL of the Akamai Cloud API. The chart passes it as `AKAMAI_API_ENDPOINT`. If it is not set, the default is `https://api.linode.com/v4`.

The URL must use https, and must not carry userinfo, a query string, or a fragment. The host must be `api.linode.com`, and the path must be `/v4`. Thus, any other host or path fails at startup, not on the first remediation.

### apiTokenSecretRef

Secret that holds the Akamai Cloud API token. The chart injects the token as `LINODE_TOKEN`, the variable that the Linode CLI and SDKs read. If `csp.provider` is `akamai`, `name` is required, and the install fails without it. `key` defaults to `LINODE_TOKEN`.

The token needs these scopes:

- The `linodes:read_write` scope, to reboot and delete instances.
- The `lke:read_write` scope, to recycle LKE nodes.

A recycle also reads the instance. Thus, `lke:read_write` alone is not enough: the token also needs a `linodes` scope, and `linodes:read_write` covers it.

A restricted identity that Akamai limits by permission needs these permissions:

- `view_linode`. The provider reads the instance before every reboot, delete and recycle. It reads the instance again while it waits for a reboot to finish.
- `reboot_linode`, for reboots.
- `delete_linode`, to delete an instance that is not an LKE node.
- The `lkecluster_admin` role, to recycle LKE nodes.

The pod reads the token from its environment when it starts. Thus, after you rotate the Secret, restart the janitor-provider pods, for example with `kubectl rollout restart deployment/janitor-provider -n nvsentinel`.

The CSP Health Monitor reads the same variable, but it only lists maintenance events. Thus, give the CSP Health Monitor its own read-only token. Do not share this token with it.

## Generic / Bare-Metal

The `generic` provider creates a privileged Kubernetes Job on the target node. The Job runs `chroot /host reboot` to reboot the node. Use this provider for bare-metal clusters, on-premises deployments, or any environment that the cloud provider backends do not cover.

When the provider creates the Job, it records the boot ID of the node. It passes this boot ID to the container in the `PRE_REBOOT_BOOT_ID` environment variable.

Before any other action, the container compares this value with `/proc/sys/kernel/random/boot_id`. If the values are different, the node has already rebooted. Then the container exits without a reboot and without a syslog entry. This check prevents a second reboot when the kubelet starts the same pod again after the node boots. If the container cannot read either boot ID, it exits with an error and does not reboot the node.

The generic provider also bounds when the Job may start a reboot. Janitor sends the attempt's start time plus its reboot timeout as an absolute deadline. The provider rejects an expired request and sets the Job's active deadline. If the boot ID still matches, the container checks the absolute deadline before logging or rebooting. A reboot that already started can finish independently of the readiness timer.

Keep the clocks on the nodes that run Janitor, the Janitor Provider, and the reboot Jobs synchronized to a common time source. The container compares `REBOOT_DEADLINE_UNIX_SECONDS` with the target node's wall clock. A clock that lags Janitor's can allow a late reboot if the pod is still runnable. A clock that is ahead can reject a reboot before the attempt expires. The Job's active deadline limits its runtime from Job start. The container check relies on synchronized clocks to enforce Janitor's absolute cutoff.

For callers that omit the deadline, the generic provider uses 30 minutes from receipt. Upgrade both Janitor and janitor-provider to apply the configured Janitor timeout end to end.

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
      writeSyslog: false
      rebootJobPriorityClassName: ""
```

### rebootImage

Container image for the reboot Job. The image must include `sh`, `cat`, `date` and `chroot`. Every reboot method runs through `sh -c`, because the boot ID check needs a shell. If the image has no `sh`, the container does not start, and the node does not reboot.

### useSysrqReboot

When `false` (default), the Job runs `chroot /host reboot` to trigger a clean OS shutdown and restart.

When `true`, the Job writes `b` to `/proc/sysrq-trigger`. This triggers an immediate kernel reboot through the Linux Magic SysRq interface. If `chroot /host reboot` is not available, use this setting. For example, the reboot command is not available in these cases:

- The node OS has a read-only root filesystem.
- The node has a custom OS image that does not expose a reboot binary at the standard path.

In other cases, do not use this setting.

### rebootJobNamespace

Kubernetes namespace in which the provider creates the reboot Job. If it is empty, the default is the janitor-provider namespace.

### rebootJobTTLSeconds

Time in seconds after the Job completes, before Kubernetes deletes the Job and its pod. The default is `3600`.

### imagePullSecrets

Name of an image pull secret to attach to the reboot Job. If the cluster pulls `rebootImage` from a private registry, set this value.

### writeSyslog

When `true`, the Job writes an attribution entry to the syslog of the node with `logger`, before it runs the reboot command. The default is `false`.

### rebootJobPriorityClassName

Name of the PriorityClass that the provider sets on the pod of the reboot Job. The default is empty, which sets no priority class.

The kubelet rejects a pod without a critical priority on a node that reports `DiskPressure`. Then the reboot never runs, and the attempt fails. To let the kubelet admit the pod on such a node, set this value to `system-node-critical` or `system-cluster-critical`. Only these two built-in classes are critical: a PriorityClass that you create cannot have a value that high.

The class must exist in the cluster. Some clusters use a ResourceQuota to limit the namespaces that can use the `system-` classes. If the class does not exist or the namespace cannot use it, the Job creates no pod. Then the reboot attempt ends when the reboot timeout expires.

## Label provider

This provider requests reboot and terminate through labels on the Node. An external controller that runs on the Kubernetes control plane can watch those labels and do the required action. After the reboot completes, that controller **MUST** remove the reboot label. The provider considers the node ready when the controller has removed the reboot label and the boot ID of the node has changed.

```yaml
janitor-provider:
  csp:
    provider: "label"
    label:
      rebootKey: "nke.nvidia.com/reboot=requested-by-nvsentinel"
      terminateKey: "nke.nvidia.com/terminate=requested-by-nvsentinel"
```

### rebootKey

Node label spec that requests a reboot. It uses the `key=value` form, so reboot and terminate can use different values. The default is `nke.nvidia.com/reboot=requested-by-nvsentinel`.

### terminateKey

Node label spec that requests termination, in `key=value` form. The default is `nke.nvidia.com/terminate=requested-by-nvsentinel`.
