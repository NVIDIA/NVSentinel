# CSP Health Monitor Configuration

## Overview

The CSP Health Monitor detects cloud provider maintenance events and triggers automated node quarantine workflows. This document covers all Helm configuration options.

## Module Enable/Disable

Controls whether the csp-health-monitor module is deployed in the cluster.

```yaml
global:
  cspHealthMonitor:
    enabled: true
```

## Cloud Provider Selection

The `cspName` field determines which cloud provider to monitor. Only one provider can be active at a time.

```yaml
csp-health-monitor:
  cspName: "gcp"  # Options: "gcp", "aws", or "lambda"
```

## Global Settings

Settings that apply regardless of cloud provider.

```yaml
csp-health-monitor:
  logLevel: info  # Options: debug, info, warn, error
  
  configToml:
    # Cluster identifier used in health events
    clusterName: "my-cluster"
    
    # How often the sidecar polls MongoDB for maintenance events (seconds)
    maintenanceEventPollIntervalSeconds: 60
    
    # Minutes before maintenance start time to trigger quarantine
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    
    # Minutes after maintenance ends to send healthy event
    postMaintenanceHealthyDelayMinutes: 15
    
    # Timeout for node to become ready after maintenance (minutes)
    nodeReadinessTimeoutMinutes: 60
```

## GCP Configuration

### Required Fields

```yaml
csp-health-monitor:
  cspName: "gcp"
  
  configToml:
    clusterName: "my-gke-cluster"
    
    gcp:
      # GCP project ID where the cluster runs
      targetProjectId: "my-gcp-project-id"
      
      # GCP Service Account name (without @project.iam.gserviceaccount.com)
      # Must match the GCP SA created in IAM setup
      gcpServiceAccountName: "csp-health-monitor"
      
      # How often to poll Cloud Logging API (seconds)
      apiPollingIntervalSeconds: 60
      
      # Cloud Logging filter for maintenance events
      logFilter: 'logName="projects/my-gcp-project-id/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'
```

### GCP Parameters

#### targetProjectId
GCP project ID where the GKE cluster is running. The monitor queries Cloud Logging in this project.

#### gcpServiceAccountName
Name of the GCP Service Account (without the `@project.iam.gserviceaccount.com` suffix). Used to generate the Workload Identity annotation on the Kubernetes ServiceAccount.

#### apiPollingIntervalSeconds
How frequently the monitor polls the Cloud Logging API for new maintenance events. Lower values provide faster detection but increase API usage.

#### logFilter
Cloud Logging filter expression to select maintenance events. Common filters:

```python
# Standard GCE instance maintenance
'logName="projects/{PROJECT_ID}/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'

# Include termination events
'logName="projects/{PROJECT_ID}/logs/cloudaudit.googleapis.com%2Fsystem_event" AND (protoPayload.methodName="compute.instances.upcomingMaintenance" OR protoPayload.methodName="compute.instances.terminateOnHostMaintenance")'
```

### Complete GCP Example

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  cspName: "gcp"
  logLevel: info
  
  configToml:
    clusterName: "production-gke-cluster"
    maintenanceEventPollIntervalSeconds: 60
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    postMaintenanceHealthyDelayMinutes: 15
    nodeReadinessTimeoutMinutes: 60
    
    gcp:
      targetProjectId: "my-production-project"
      gcpServiceAccountName: "csp-health-monitor"
      apiPollingIntervalSeconds: 60
      logFilter: 'logName="projects/my-production-project/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'
```

## AWS Configuration

### Required Fields

```yaml
csp-health-monitor:
  cspName: "aws"
  
  configToml:
    clusterName: "my-eks-cluster"
    
    aws:
      # AWS Account ID (12-digit number)
      accountId: "123456789012"
      
      # AWS region where the EKS cluster runs
      region: "us-east-1"
      
      # How often to poll AWS Health API (seconds)
      pollingIntervalSeconds: 60
      
      # (Optional) Custom IAM role name for IRSA
      iamRoleName: ""
```

### AWS Parameters

#### accountId
AWS account ID (12-digit number) where the EKS cluster is running. Used to construct the IAM role ARN annotation.

#### region
AWS region where the EKS cluster is deployed. The monitor queries the AWS Health API in this region.

#### pollingIntervalSeconds
How frequently the monitor polls the AWS Health API for maintenance events. Lower values provide faster detection but increase API usage.

#### iamRoleName
Custom IAM role name for IRSA (IAM Roles for Service Accounts). When set, the ServiceAccount annotation uses this role name directly instead of constructing one from `clusterName`.

If left empty (default), the role name is generated as `{CLUSTER_NAME}-nvsentinel-health-monitor-assume-role-policy`.

> **Important (EKS)**: AWS IAM role names have a maximum of 64 characters. The default suffix `-nvsentinel-health-monitor-assume-role-policy` is 45 characters, leaving only **19 characters** for the cluster name. If your EKS cluster name exceeds 19 characters, you **must** set `iamRoleName` to a custom value.

### Complete AWS Example

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  cspName: "aws"
  logLevel: info
  
  configToml:
    clusterName: "production-eks-cluster"
    maintenanceEventPollIntervalSeconds: 60
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    postMaintenanceHealthyDelayMinutes: 15
    nodeReadinessTimeoutMinutes: 60
    
    aws:
      accountId: "123456789012"
      region: "us-east-1"
      pollingIntervalSeconds: 60
```

### AWS Example with Custom IAM Role Name

For clusters with long names (>19 characters), set `iamRoleName` explicitly:

```yaml
csp-health-monitor:
  cspName: "aws"
  
  configToml:
    clusterName: "my-very-long-production-eks-cluster-name"
    
    aws:
      accountId: "123456789012"
      region: "us-east-1"
      pollingIntervalSeconds: 60
      iamRoleName: "my-custom-nvsentinel-role"
```

## Lambda Configuration

Polls the Lambda maintenance-events API and raises a health event for each event affecting a node in this cluster. Nodes are matched by the instance UUID in the event's `entity_lrns`, so they must carry `spec.providerID` in the form `lambda://<instanceID>`.

### Static API Key Example

```yaml
csp-health-monitor:
  cspName: "lambda"

  lambdaApiKeySecret:
    name: "lambda-api-key"
    key: "LAMBDA_API_KEY"

  configToml:
    clusterName: "my-cluster"

    lambda:
      apiEndpoint: "https://cloud.lambda.ai"
      workspaceId: "c4d291f47f9d436fa39f58493ce3b50d"
      pollingIntervalSeconds: 30
```

### apiEndpoint
Base URL of the Lambda API. Defaults to the production endpoint, so the happy path needs no override.

### workspaceId
Optional. Scopes the maintenance-events query to one workspace, in dashed or undashed UUID form. Leave it empty to use the default workspace for the credential.

Set it whenever that default is not the workspace running this cluster. Getting it wrong is quiet: the API answers `200` with an empty list rather than an error, so the monitor looks healthy while never seeing a maintenance event for these nodes. A value that is not a UUID fails at startup rather than returning `400` on every poll.

This matters more with workload identity, where the credential belongs to a service identity whose role is assigned at workspace scope — its default workspace is unlikely to be the cluster's.

### pollingIntervalSeconds
How often the maintenance-events API is polled. Minimum 30.

### lambdaApiKeySecret
Secret holding the Lambda API key, injected as `LAMBDA_API_KEY`. `name` is required when `cspName` is `lambda`, unless `lambdaWorkloadIdentity.identityLRN` is set; the install fails without one of the two. The same Secret can be shared with the Janitor Provider, which reads the key the same way. The key needs permission to read maintenance events in the target workspace.

### lambdaWorkloadIdentity.identityLRN

The service identity to assume, in the form `lrn:iam:identity:<id>`. Preferred over a static key: no Lambda credential is stored in the cluster, and the token is short-lived and rotated automatically.

Setting it annotates the csp-health-monitor ServiceAccount with `lambda.ai/identity-lrn`. The pod is then given a short-lived Kubernetes token, which the monitor exchanges for a Lambda API key and refreshes before it expires — so no Lambda credential is stored in the cluster.

```yaml
csp-health-monitor:
  cspName: "lambda"

  lambdaWorkloadIdentity:
    identityLRN: "lrn:iam:identity:3cd2d107c6a347eeb0ef9498820d637d"

  configToml:
    clusterName: "my-cluster"
    lambda:
      workspaceId: "c4d291f47f9d436fa39f58493ce3b50d"
```

When set, `lambdaApiKeySecret` is not required and no `LAMBDA_API_KEY` is placed in the pod. If both are configured, workload identity wins and the static key is ignored; the monitor logs which one it selected at startup (`authMode`).

On the Lambda side, the identity must exist, hold permission to read maintenance events in the workspace named by `workspaceId`, and trust this cluster's ServiceAccount.

```bash
(
  set -euo pipefail

  NAMESPACE=$(
    kubectl get deployments --all-namespaces \
      -l app.kubernetes.io/name=csp-health-monitor -o json |
      jq -er '
        .items
        | if length == 1 then .[0] else error("expected exactly one csp-health-monitor Deployment") end
        | .metadata.namespace
      '
  )
  kubectl -n "$NAMESPACE" get sa -l app.kubernetes.io/name=csp-health-monitor
)
```

#### Setting up the identity

Run once per cluster, with an admin API key. `$WS` is the workspace the cluster's instances belong to — the same one you set as `workspaceId` above.

```bash
# Stop at the first failed call: a half-applied setup prints an identityLRN
# that looks usable but has no role or no trust behind it.
set -euo pipefail

KEY=<admin-api-key>
export WS=<workspace-id>

lambda_curl() {
  printf 'Authorization: Bearer %s\n' "$KEY" | curl -sf -H @- "$@"
}

# 1. Create a service identity for the CSP Health Monitor.
SID=$(lambda_curl "https://cloud.lambda.ai/api/v1/identities" \
  -H 'Content-Type: application/json' \
  -d '{"display_name":"nvsentinel-csp-health-monitor"}' | jq -r .data.id)
[ -n "$SID" ] && [ "$SID" != "null" ] || { echo "identity was not created" >&2; exit 1; }

# 2. Add it to the workspace. A workspace-scoped role assignment needs membership.
lambda_curl "https://cloud.lambda.ai/api/v1/workspaces/$WS/memberships" \
  -H 'Content-Type: application/json' \
  -d "{\"member_type\":\"identity\",\"member_id\":\"$SID\"}"

# 3. Assign the built-in roles, scoped to that workspace.
for role in maintenance-api-reader; do
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
    -l app.kubernetes.io/name=csp-health-monitor -o json |
    jq -er '
      .items
      | if length == 1 then .[0] else error("expected exactly one csp-health-monitor Deployment") end
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
| `maintenance-api-reader` | `support:maintenance-event:read` | Polling the maintenance-events API |

The role is workspace-scoped, so the identity only sees events for `$WS` — which is why `workspaceId` should name that same workspace.

#### Verifying

The identity is attached when the pod is created, so it only appears on pods created *after* the annotation lands. Restart the deployment if you added it to a running install.

```bash
(
  set -euo pipefail

  NAMESPACE=$(
    kubectl get deployments --all-namespaces \
      -l app.kubernetes.io/name=csp-health-monitor -o json |
      jq -er '
        .items
        | if length == 1 then .[0] else error("expected exactly one csp-health-monitor Deployment") end
        | .metadata.namespace
      '
  )

  # The identity the pod received.
  kubectl -n "$NAMESPACE" get pod -l app.kubernetes.io/name=csp-health-monitor \
    -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="LAMBDA_IDENTITY_LRN")]}'

  # Which credential the monitor selected, and the workspace it polls.
  kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=csp-health-monitor | grep authMode
)
```

Expect `authMode=workload-identity`. If it says `api-key`, the pod never received an identity — check that the annotation is on the ServiceAccount and that the pod was created after it landed.

Failures in the exchange itself surface on the first poll, not at startup, because the token is minted lazily. When the exchange endpoint rejects the token it returns `401` with no detail by design, so an unauthenticated caller cannot probe for which identities exist; that means a missing trust, a wrong identity LRN and a disabled account all look identical from the client. Check the trust and the identity LRN first.

## CSP-Specific IAM Requirements

Each cloud provider handles IAM identity for the CSP Health Monitor differently:

| Provider | IAM Identity Configuration | Naming Flexibility |
|----------|---------------------------|-------------------|
| **GCP**  | `gcp.gcpServiceAccountName` — User provides any GCP Service Account name. The ServiceAccount annotation is built as `{SA_NAME}@{PROJECT}.iam.gserviceaccount.com`. | Fully flexible. No naming convention enforced. |
| **AWS (EKS)** | `aws.iamRoleName` (optional) — User provides a custom IAM role name. If omitted, the role name defaults to `{CLUSTER_NAME}-nvsentinel-health-monitor-assume-role-policy`. | Flexible when `iamRoleName` is set. The default convention imposes a **19-character cluster name limit** (AWS IAM role names max 64 chars, default suffix is 45 chars). |
| **Lambda** | Either a static API key in `lambdaApiKeySecret`, or `lambdaWorkloadIdentity.identityLRN` — the ServiceAccount is annotated with `lambda.ai/identity-lrn` and the pod receives a short-lived token instead of a stored key. | Fully flexible. The identity is named by LRN, so no naming convention is enforced. |

> **Recommendation for EKS users**: If your cluster name is longer than 19 characters, always set `aws.iamRoleName` explicitly and create the corresponding IAM role with that name. See [IAM Setup](../csp-health-monitor-iam.md) for detailed instructions.

## Advanced Configuration

### Out-of-Cluster Monitoring

For monitoring a tenant cluster from a separate management cluster:

```yaml
csp-health-monitor:
  configToml:
    # Path to kubeconfig for tenant cluster
    kubeconfigPath: "/etc/kubeconfig/tenant-cluster.yaml"
```

When `kubeconfigPath` is set, the monitor uses the specified kubeconfig to connect to the tenant cluster's Kubernetes API for node mapping. If empty, uses in-cluster config.

### Resources

Configure resource requests and limits for the main container and sidecar.

```yaml
csp-health-monitor:
  # Main container resources
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "200m"
      memory: "256Mi"
  
  # Sidecar (Quarantine Trigger Engine) resources
  quarantineTriggerEngine:
    resources:
      limits:
        cpu: "500m"
        memory: "512Mi"
      requests:
        cpu: "100m"
        memory: "128Mi"
```

### Scheduling

Configure pod placement using node selectors, tolerations, and affinity rules.

```yaml
csp-health-monitor:
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  
  tolerations:
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
  
  affinity: {}
```
