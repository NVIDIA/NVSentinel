# CSP Health Monitor Configuration

## Overview

The CSP Health Monitor detects maintenance events of the cloud provider and starts automated quarantine workflows for the nodes. This document describes the main Helm configuration options.

## Module Enable/Disable

This setting enables or disables the deployment of the CSP Health Monitor in the cluster.

```yaml
global:
  cspHealthMonitor:
    enabled: true
```

## Cloud Provider Selection

The `cspName` field selects the cloud provider to monitor. Only one provider can be active at a time.

```yaml
csp-health-monitor:
  cspName: "gcp"  # Options: "gcp", "aws", "lambda", or "akamai"
```

## Global Settings

These settings apply to all cloud providers.

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

The ID of the GCP project where the GKE cluster runs. The monitor queries Cloud Logging in this project.

#### gcpServiceAccountName

The name of the GCP Service Account, without the `@project.iam.gserviceaccount.com` suffix. The chart uses this name to generate the Workload Identity annotation on the Kubernetes ServiceAccount.

#### apiPollingIntervalSeconds

How often the monitor polls the Cloud Logging API for new maintenance events. A lower value gives faster detection, but it increases the API usage.

#### logFilter

The Cloud Logging filter expression that selects maintenance events. Common filters:

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

The ID of the AWS account (a 12-digit number) where the EKS cluster runs. The chart uses it to build the IAM role ARN annotation.

#### region

The AWS region of the EKS cluster. The monitor queries the AWS Health API in this region.

#### pollingIntervalSeconds

How often the monitor polls the AWS Health API for maintenance events. A lower value gives faster detection, but it increases the API usage.

#### iamRoleName

A custom IAM role name for IRSA (IAM Roles for Service Accounts). If you set it, the ServiceAccount annotation uses this role name directly. The chart then does not build a role name from `clusterName`.

If the value is empty (the default), the chart generates the role name `{CLUSTER_NAME}-nvsentinel-health-monitor-assume-role-policy`.

> **Important (EKS)**: If your EKS cluster name has more than 19 characters, you **must** set `iamRoleName` to a custom value. An AWS IAM role name has a maximum of 64 characters. The default suffix `-nvsentinel-health-monitor-assume-role-policy` has 45 characters, so the cluster name can have only **19 characters**.

### Complete AWS Example

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  cspName: "aws"
  logLevel: info
  
  configToml:
    clusterName: "prod-eks-cluster"
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

If the cluster name is long (more than 19 characters), set `iamRoleName` explicitly:

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

The monitor polls the Lambda maintenance-events API. It creates a health event for each maintenance event that affects a node in this cluster. The monitor matches a maintenance event to a node by the instance UUID in the `entity_lrns` field of the maintenance event. Thus, each node must have `spec.providerID` in the form `lambda://<instanceID>`.

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

The base URL of the Lambda API. The default is the production endpoint, so a normal installation does not need to change it.

### workspaceId

Optional. This value limits the maintenance-events query to one workspace. Use a UUID, with or without dashes. To use the default workspace of the credential, leave it empty.

If the default workspace is not the workspace that runs this cluster, set this value. A wrong workspace does not cause an error. The API returns `200` with an empty list. The monitor then looks healthy, but it never sees a maintenance event for these nodes. A value that is not a UUID causes a failure at startup, not a `400` response on every poll.

This is more important with workload identity. With workload identity, the credential belongs to a service identity that has its role at workspace scope. The default workspace of that identity is probably not the workspace of the cluster.

### pollingIntervalSeconds

How often the monitor polls the maintenance-events API. The minimum is 30.

### lambdaApiKeySecret

The Secret that holds the Lambda API key. The chart injects the key into the pod as `LAMBDA_API_KEY`. If `cspName` is `lambda` and you do not set `lambdaWorkloadIdentity.identityLRN`, you must set `name`. If you set neither of the two, the install fails. You can use the same Secret for the Janitor Provider, which reads the key in the same way. The key must have permission to read maintenance events in the target workspace.

### lambdaWorkloadIdentity.identityLRN

The service identity that the monitor assumes, in the form `lrn:iam:identity:<id>`. If possible, use this method instead of a static key. With this method, the cluster stores no Lambda credential, and the token is short-lived and rotates automatically.

If you set this value, the chart annotates the ServiceAccount of the CSP Health Monitor with `lambda.ai/identity-lrn`. The pod then gets a short-lived Kubernetes token. The monitor exchanges this token for a Lambda API key, and it refreshes the key before the key expires. Thus, the cluster stores no Lambda credential.

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

If you set the identity LRN, you do not have to set `lambdaApiKeySecret`, and the chart does not put `LAMBDA_API_KEY` in the pod. If you configure both, workload identity has priority, and NVSentinel does not use the static key. At startup, the monitor logs the method that it selected (`authMode`).

In Lambda, the identity must meet these conditions:

- It exists.
- It has permission to read maintenance events in the workspace that `workspaceId` names.
- It trusts the ServiceAccount of this cluster.

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

Run this script one time for each cluster, with an admin API key. `$WS` is the workspace that contains the instances of the cluster. It is the same workspace that you set as `workspaceId` above.

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

Use the printed LRN as the Helm value above.

| Built-in role | Grants | Needed for |
| --- | --- | --- |
| `maintenance-api-reader` | `support:maintenance-event:read` | Polling the maintenance-events API |

The role has workspace scope, so the identity sees only the maintenance events of `$WS`. For this reason, set `workspaceId` to the same workspace.

#### Verifying

A pod gets the identity only when Kubernetes creates the pod. Thus, only pods that Kubernetes creates *after* the annotation exists have the identity. If you added the annotation to a running installation, restart the deployment.

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

The expected output is `authMode=workload-identity`. If the output shows `api-key`, the pod did not receive an identity. In this case, verify that the annotation is on the ServiceAccount. Then verify that Kubernetes created the pod after you added the annotation.

A failure of the token exchange shows on the first poll, not at startup. The reason is that the monitor gets the Lambda API key only when it first needs the key. If the exchange endpoint rejects the token, it returns `401` with no details. This behavior is intentional: an unauthenticated caller cannot use it to find which identities exist. Thus, a missing trust, a wrong identity LRN and a disabled account all give the same result on the client. Examine the trust and the identity LRN first.

## Akamai Configuration

The monitor polls the Akamai Cloud account maintenance API (`GET /account/maintenance`). It creates a health event for each maintenance event that affects a node in this cluster. The account lists the maintenance for all of its instances. Thus, the monitor matches a maintenance event to a node by the Linode instance ID in the `entity` field of the maintenance event. The monitor ignores maintenance events for instances that are not nodes of this cluster.

Each node must have `spec.providerID` in the form `linode://<instanceID>`. The Linode cloud controller manager applies this value to LKE nodes.

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  cspName: "akamai"

  akamaiApiTokenSecret:
    name: "akamai-api-token"
    key: "LINODE_TOKEN"

  configToml:
    clusterName: "my-cluster"

    akamai:
      pollingIntervalSeconds: 60
```

### Which maintenance events quarantine a node

Each maintenance type that the API reports (`reboot`, `cold_migration`, `live_migration`, `migrate` and `power_off_on`) quarantines the node. A GPU workload cannot do a live migration, so the monitor does not treat `live_migration` as harmless. The monitor keeps the type and the maintenance policy in the metadata of the maintenance event, for reference.

If the `description` of a maintenance event is `emergency`, NVSentinel quarantines the node on the next poll. It does not wait for the trigger window. For all other maintenance events, NVSentinel quarantines the node before the `when` time of the maintenance event, within `triggerQuarantineWorkflowTimeLimitMinutes`. The notice period changes with the policy, from a few hours for a live migration to seven days for a power off and on. Set the size of that window to the notice that you need to drain a node.

The API does not report the last-updated time of a maintenance event. Instead, the monitor compares a fingerprint of the status, the type and the schedule. The monitor writes a maintenance event again only when one of these values changes. Thus, a cancellation or a reschedule still takes effect.

### Cancelled maintenance

The monitor does not rely on the API to return a `canceled` status. If the API does not list a `pending` or `scheduled` maintenance of this cluster in two consecutive polls, the monitor treats the maintenance as cancelled. The monitor then releases the node through a `CANCELLED` event. The monitor uses the maintenance event that it stored earlier as the record of why NVSentinel quarantined the node.

A poll that fails never counts as an absence. A list that has fewer entries than the total that the API reports is an error, so it never counts as an absence either. The monitor never releases a maintenance that is `started`, `in-progress` or `completed` in this way. The healthy event that releases the node has the check name `CSPMaintenance`. Thus, it clears only the maintenance fault, and any other fault on the node keeps the node quarantined.

Two rules prevent an early release of a node:

1. If the whole list is empty, the monitor waits for 10 consecutive polls. The reason is that a lasting empty list is more likely a fault between the monitor and the API than a cancellation of every maintenance.
2. The monitor does not release a node while another stored `pending`, `scheduled`, `started` or `in-progress` event of this cluster is on the same node. The reason is that the healthy event clears the maintenance check for the whole node, not for one maintenance event.

The monitor does not cancel a maintenance event that the API still lists. This is also true if the instance is no longer a node of this cluster. The maintenance event stays in the datastore until the API stops listing it. Without a datastore, the monitor cannot detect a cancellation, and it logs one warning.

### apiEndpoint

Optional. The base URL of the Akamai Cloud API, with the version. The default is `https://api.linode.com/v4`, so a normal installation does not need to change it.

The URL must use https. It must not contain userinfo, a query string or a fragment. The host must be `api.linode.com`, and the path must be `/v4`. Any other host or path causes a failure at startup, not on the first poll.

### pollingIntervalSeconds

How often the monitor polls the maintenance API. The minimum is 30.

### akamaiApiTokenSecret

The Secret that holds the Akamai Cloud API token. The chart injects the token as `LINODE_TOKEN`, which is the variable that the Linode CLI and SDKs read. If `cspName` is `akamai`, you must set `name`. Without it, the install fails. Before the install, create the Secret in the release namespace, because the pod cannot start without it. `key` defaults to `LINODE_TOKEN`.

The monitor only lists maintenance events. Thus, give it a read-only token with the `account:read_only` scope. A restricted identity that Akamai limits by permission must have the `list_maintenances` permission. The legacy `read_only` grant also gives access to the endpoint, so a read-only token does not need an additional role. Do not share the token of the Janitor Provider with the monitor, because that token can reboot and delete instances.

The pod reads the token from its environment when it starts. Thus, after you rotate the Secret, restart the monitor pods. For example, use `kubectl rollout restart deployment/csp-health-monitor -n nvsentinel`.

## CSP-Specific IAM Requirements

Each cloud provider uses a different method for the IAM identity of the CSP Health Monitor:

| Provider | IAM Identity Configuration | Naming Flexibility |
|----------|---------------------------|-------------------|
| **GCP**  | `gcp.gcpServiceAccountName` — you supply any GCP Service Account name. The chart builds the ServiceAccount annotation as `{SA_NAME}@{PROJECT}.iam.gserviceaccount.com`. | Fully flexible. NVSentinel enforces no naming convention. |
| **AWS (EKS)** | `aws.iamRoleName` (optional) — you supply a custom IAM role name. If you omit it, the role name defaults to `{CLUSTER_NAME}-nvsentinel-health-monitor-assume-role-policy`. | Flexible if you set `iamRoleName`. The default convention gives a **19-character cluster name limit**. An AWS IAM role name has a maximum of 64 characters, and the default suffix has 45 characters. |
| **Lambda** | A static API key in `lambdaApiKeySecret`, or `lambdaWorkloadIdentity.identityLRN`. With the identity LRN, the chart annotates the ServiceAccount with `lambda.ai/identity-lrn`. The pod then gets a short-lived token instead of a stored key. | Fully flexible. An LRN names the identity, so NVSentinel enforces no naming convention. |
| **Akamai** | A static API token in `akamaiApiTokenSecret`, injected as `LINODE_TOKEN`. There is no workload identity, so the cluster stores the token. | Not applicable. Use a read-only token. |

> **Recommendation for EKS**: If your cluster name has more than 19 characters, always set `aws.iamRoleName` explicitly. Then create the IAM role with that name. For detailed instructions, see [IAM Setup](../csp-health-monitor-iam.md).

## Advanced Configuration

### Out-of-Cluster Monitoring

To monitor a tenant cluster from a separate management cluster, use this configuration:

```yaml
csp-health-monitor:
  configToml:
    # Path to kubeconfig for tenant cluster
    kubeconfigPath: "/etc/kubeconfig/tenant-cluster.yaml"
```

If you set `kubeconfigPath`, the monitor uses this kubeconfig to connect to the Kubernetes API of the tenant cluster for node mapping. If the value is empty, the monitor uses the in-cluster configuration.

### Resources

Configure the resource requests and limits for the main container and the sidecar.

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

Configure the pod placement with node selectors, tolerations, and affinity rules.

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
