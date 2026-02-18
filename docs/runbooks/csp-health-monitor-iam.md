# Runbook: CSP Health Monitor IAM Troubleshooting

## Overview

This runbook covers IAM permission issues for the CSP Health Monitor on GCP and AWS.

## GCP Issues

### Symptom: PERMISSION_DENIED Errors

**Logs show:**
```log
Error iterating GCP log entries: rpc error: code = PermissionDenied desc = The caller does not have permission
```

### Verification Steps

1. **Check GCP Service Account has required role:**

```bash
gcloud projects get-iam-policy <TARGET_PROJECT_ID> \
    --flatten="bindings[].members" \
    --filter="bindings.members:serviceAccount:<GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com"
```

Expected output should show the custom role `projects/<TARGET_PROJECT_ID>/roles/cspHealthMonitorRole` or predefined role `roles/logging.viewer`.

2. **Check Workload Identity binding:**

```bash
gcloud iam service-accounts get-iam-policy \
    <GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com
```

Expected output should show `roles/iam.workloadIdentityUser` with member `serviceAccount:<GKE_PROJECT_ID>.svc.id.goog[nvsentinel/csp-health-monitor]`.

3. **Check ServiceAccount annotation:**

```bash
kubectl get serviceaccount csp-health-monitor -n nvsentinel -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}'
```

Expected output: `<GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com`

### Resolution

If the GCP Service Account is missing the role:

```bash
gcloud projects add-iam-policy-binding <TARGET_PROJECT_ID> \
    --member="serviceAccount:<GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com" \
    --role="projects/<TARGET_PROJECT_ID>/roles/cspHealthMonitorRole"
```

If Workload Identity binding is missing:

```bash
gcloud iam service-accounts add-iam-policy-binding \
    <GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com \
    --role="roles/iam.workloadIdentityUser" \
    --member="serviceAccount:<GKE_PROJECT_ID>.svc.id.goog[nvsentinel/csp-health-monitor]"
```

### Test Permissions Manually

```bash
gcloud logging read "logName=\"projects/<PROJECT_ID>/logs/cloudaudit.googleapis.com%2Fsystem_event\"" \
    --project=<PROJECT_ID> \
    --limit=1 \
    --impersonate-service-account=<GCP_SA_NAME>@<PROJECT_ID>.iam.gserviceaccount.com
```

## AWS Issues

### Symptom: AccessDeniedException Errors

**Logs show:**
```log
Error while fetching maintenance events: operation error Health: DescribeEvents, https response error StatusCode: 403, AccessDeniedException
```

### Verification Steps

1. **Check IAM policy is attached to role:**

```bash
# Use your custom role name if aws.iamRoleName is set, otherwise use the default pattern
aws iam list-attached-role-policies \
    --role-name <IAM_ROLE_NAME>
```

> **Note**: The role name is either the value of `aws.iamRoleName` (if set) or the default `<CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy`.

Expected output should show `CSPHealthMonitorPolicy` attached.

2. **Check IAM role trust policy:**

```bash
aws iam get-role \
    --role-name <IAM_ROLE_NAME> \
    --query 'Role.AssumeRolePolicyDocument'
```

Expected: Trust policy should reference the correct EKS OIDC provider and `system:serviceaccount:nvsentinel:csp-health-monitor`.

3. **Check ServiceAccount annotation:**

```bash
kubectl get serviceaccount csp-health-monitor -n nvsentinel -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}'
```

Expected output: `arn:aws:iam::<ACCOUNT_ID>:role/<IAM_ROLE_NAME>`

### Resolution

If IAM policy is not attached:

```bash
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

aws iam attach-role-policy \
    --role-name <IAM_ROLE_NAME> \
    --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/CSPHealthMonitorPolicy
```

If the role ARN doesn't match Helm values, ensure `aws.iamRoleName` (or `configToml.clusterName` if using the default pattern) is correct, and redeploy.

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

1. **Check nodes have providerID set:**

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.providerID}{"\n"}{end}'
```

Expected:
- GCP: `gce://<project-id>/<zone>/<instance-name>`
- AWS: `aws:///<availability-zone>/<instance-id>`

2. **Check GCP node annotations (GCP only):**

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.container\.googleapis\.com/instance_id}{"\n"}{end}'
```

3. **Check RBAC permissions:**

```bash
kubectl auth can-i list nodes --as=system:serviceaccount:nvsentinel:csp-health-monitor
```

Expected: `yes`

### Resolution

If nodes missing `providerID`, the kubelet configuration may be incorrect. Check node registration and cloud provider integration.

If RBAC is missing, verify the ClusterRole and ClusterRoleBinding were created by the Helm chart:

```bash
kubectl get clusterrole csp-health-monitor
kubectl get clusterrolebinding csp-health-monitor
```

