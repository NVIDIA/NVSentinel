# Runbook: CSP Health Monitor IAM Troubleshooting

## Overview

This runbook describes IAM permission issues for the CSP Health Monitor on GCP and AWS.

## GCP Issues

### Symptom: PERMISSION_DENIED Errors

**Logs show:**

```log
Error iterating GCP log entries: rpc error: code = PermissionDenied desc = The caller does not have permission
```

### Verification Steps

1. **Check that the GCP Service Account has the required role:**

```bash
gcloud projects get-iam-policy {TARGET_PROJECT_ID} \
    --flatten="bindings[].members" \
    --filter="bindings.members:serviceAccount:{GCP_SA_NAME}@{TARGET_PROJECT_ID}.iam.gserviceaccount.com"
```

The expected output shows the custom role `projects/{TARGET_PROJECT_ID}/roles/cspHealthMonitorRole` or the predefined role `roles/logging.viewer`.

2. **Check the Workload Identity binding:**

```bash
gcloud iam service-accounts get-iam-policy \
    {GCP_SA_NAME}@{TARGET_PROJECT_ID}.iam.gserviceaccount.com
```

The expected output shows `roles/iam.workloadIdentityUser` with the member `serviceAccount:{GKE_PROJECT_ID}.svc.id.goog[nvsentinel/csp-health-monitor]`.

3. **Check the ServiceAccount annotation:**

```bash
kubectl get serviceaccount csp-health-monitor -n nvsentinel -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}'
```

Expected output: `{GCP_SA_NAME}@{TARGET_PROJECT_ID}.iam.gserviceaccount.com`

### Resolution

If the GCP Service Account does not have the role:

```bash
gcloud projects add-iam-policy-binding {TARGET_PROJECT_ID} \
    --member="serviceAccount:{GCP_SA_NAME}@{TARGET_PROJECT_ID}.iam.gserviceaccount.com" \
    --role="projects/{TARGET_PROJECT_ID}/roles/cspHealthMonitorRole"
```

If the Workload Identity binding is missing:

```bash
gcloud iam service-accounts add-iam-policy-binding \
    {GCP_SA_NAME}@{TARGET_PROJECT_ID}.iam.gserviceaccount.com \
    --role="roles/iam.workloadIdentityUser" \
    --member="serviceAccount:{GKE_PROJECT_ID}.svc.id.goog[nvsentinel/csp-health-monitor]"
```

### Test Permissions Manually

```bash
gcloud logging read "logName=\"projects/{TARGET_PROJECT_ID}/logs/cloudaudit.googleapis.com%2Fsystem_event\"" \
    --project={TARGET_PROJECT_ID} \
    --limit=1 \
    --impersonate-service-account={GCP_SA_NAME}@{TARGET_PROJECT_ID}.iam.gserviceaccount.com
```

## AWS Issues

### Symptom: AccessDeniedException Errors

**Logs show:**

```log
Error while fetching maintenance events: operation error Health: DescribeEvents, https response error StatusCode: 403, AccessDeniedException
```

### Verification Steps

1. **Check that the role has the IAM policy:**

```bash
# Use your custom role name if aws.iamRoleName is set, otherwise use the default pattern
aws iam list-attached-role-policies \
    --role-name {IAM_ROLE_NAME}
```

> **Note**: The role name is either the value of `aws.iamRoleName` (if you set it) or the default `{CLUSTER_NAME}-nvsentinel-health-monitor-assume-role-policy`.

The expected output shows `CSPHealthMonitorPolicy` as an attached policy.

2. **Check the trust policy of the IAM role:**

```bash
aws iam get-role \
    --role-name {IAM_ROLE_NAME} \
    --query 'Role.AssumeRolePolicyDocument'
```

Expected: the trust policy references the correct EKS OIDC provider and `system:serviceaccount:nvsentinel:csp-health-monitor`.

3. **Check the ServiceAccount annotation:**

```bash
kubectl get serviceaccount csp-health-monitor -n nvsentinel -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}'
```

Expected output: `arn:aws:iam::{ACCOUNT_ID}:role/{IAM_ROLE_NAME}`

### Resolution

If the role does not have the IAM policy:

```bash
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

aws iam attach-role-policy \
    --role-name {IAM_ROLE_NAME} \
    --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/CSPHealthMonitorPolicy
```

If the role ARN does not match the Helm values, make sure that `aws.iamRoleName` is correct. If you use the default pattern, make sure that `configToml.clusterName` is correct instead. Then redeploy.

### Test Permissions Manually

```bash
aws health describe-events --filter "services=EC2" --max-items 1
```

## Node Mapping Failures

### Symptom: Events Detected but Nodes Not Quarantined

**Logs show:**

```log
No Kubernetes node found matching GCP numeric instance ID
Instance ID not found in node map
```

### Verification Steps

1. **Check that the nodes have a providerID:**

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.providerID}{"\n"}{end}'
```

Expected:

- GCP: `gce://{PROJECT_ID}/{ZONE}/{INSTANCE_NAME}`
- AWS: `aws:///{AVAILABILITY_ZONE}/{INSTANCE_ID}`

2. **Check the GCP node annotations (GCP only):**

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.container\.googleapis\.com/instance_id}{"\n"}{end}'
```

3. **Check the RBAC permissions:**

```bash
kubectl auth can-i list nodes --as=system:serviceaccount:nvsentinel:csp-health-monitor
```

Expected: `yes`

### Resolution

If nodes do not have a `providerID`, the kubelet configuration can be incorrect. Check the node registration and the cloud provider integration.

If the RBAC permissions are missing, verify that the Helm chart created the ClusterRole and the ClusterRoleBinding:

```bash
kubectl get clusterrole csp-health-monitor
kubectl get clusterrolebinding csp-health-monitor
```

