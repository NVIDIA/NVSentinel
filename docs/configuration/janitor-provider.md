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
    provider: "kind"  # Options: kind, kwok, aws, gcp, azure, oci, nebius, lambda, generic
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

## Lambda

Reboot maps to the Lambda power-cycle operation (a host-level power cycle, not a guest restart) and terminate maps to the Lambda terminate operation. Nodes must carry `spec.providerID` in the form `lambda://<instanceID>`.

Authenticate either with a static API key or with workload identity. Workload identity is preferred: no Lambda credential is stored in the cluster, and the token it uses is short-lived and rotated automatically.

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
Optional. Overrides the base URL of the Lambda Cloud API, passed as `LAMBDA_API_ENDPOINT`. Defaults to `https://cloud.lambda.ai` when unset. Must be https, and must not carry userinfo, a query string, or a fragment. The host is checked against a fixed allowlist built into the provider, so an unapproved host fails at startup rather than on the first remediation.

### apiKeySecretRef
Secret holding the Lambda API key, injected as `LAMBDA_API_KEY`. `name` is required when `csp.provider` is `lambda`, unless `workloadIdentity.identityLRN` is set; the install fails without one of the two. `key` defaults to `LAMBDA_API_KEY`. The same Secret can be shared with the CSP Health Monitor, which reads the key the same way. The key needs permission to power cycle and terminate instances.

### workloadIdentity.identityLRN

The service identity to assume, in the form `lrn:iam:identity:<id>`. Setting it annotates the janitor-provider ServiceAccount with `lambda.ai/identity-lrn`. The pod is then given a short-lived Kubernetes token, which the provider exchanges for a Lambda API key and refreshes before it expires — so no Lambda credential is stored in the cluster.

```yaml
janitor-provider:
  csp:
    provider: "lambda"
    lambda:
      workloadIdentity:
        identityLRN: "lrn:iam:identity:3cd2d107c6a347eeb0ef9498820d637d"
```

When set, `apiKeySecretRef` is not required and no `LAMBDA_API_KEY` is placed in the pod. If both are configured, workload identity wins and the static key is ignored; the provider logs which one it selected at startup (`authMode`).

On the Lambda side, the identity must exist, hold permission to power cycle and terminate instances, and trust this cluster's ServiceAccount.

#### Setting up the identity

Run once per cluster, with an admin API key. `$WS` is the workspace the cluster's instances belong to.

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

The printed LRN is what goes in the chart value above.

| Built-in role | Grants | Needed for |
| --- | --- | --- |
| `instance-reader` | `compute:instance:read` | Reading instance state before and after a reboot |
| `instance-power-cycle` | `compute:instance:power-cycle` | The reboot itself |
| `instance-terminate` | `compute:instance:terminate` | Terminating a node the remediation policy replaces rather than reboots |

All three are workspace-scoped, so the identity can only act on instances in `$WS`.

Drop `instance-terminate` from the loop if your remediation policy only ever reboots — the roles are additive, so granting just the two leaves terminate unauthorized.

#### Verifying

The identity is attached when the pod is created, so it only appears on pods created *after* the annotation lands. Restart the deployment if you added it to a running install.

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

Expect `authMode=workload-identity`. If it says `api-key`, the pod never received an identity — check that the annotation is on the ServiceAccount and that the pod was created after it landed.

Failures in the exchange itself surface on the first remediation, not at startup, because the token is minted lazily. When the exchange endpoint rejects the token it returns `401` with no detail by design, so an unauthenticated caller cannot probe for which identities exist; that means a missing trust, a wrong identity LRN and a disabled account all look identical from the client. Check the trust and the identity LRN first.

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
      writeSyslog: false
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

### writeSyslog
When `true`, the Job writes an attribution entry to the node's syslog via `logger` before executing the reboot command. Defaults to `false`.
