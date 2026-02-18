# CSP Health Monitor - IAM Requirements

## Overview

The CSP Health Monitor requires IAM permissions to monitor cloud provider maintenance events. This document provides the setup commands for GCP and AWS.

## Google Cloud Platform (GCP)

### Required IAM Permission

- `logging.logEntries.list` - Read Cloud Logging entries for maintenance events

### Setup Commands

Replace placeholders:
- `<GCP_SA_NAME>` - GCP Service Account name (e.g., `csp-health-monitor`)
- `<TARGET_PROJECT_ID>` - GCP project ID where the cluster runs
- `<GKE_PROJECT_ID>` - GCP project ID where GKE cluster is deployed
- `<NAMESPACE>` - Kubernetes namespace (default: `nvsentinel`)

```bash
# 1. Create GCP Service Account
gcloud iam service-accounts create <GCP_SA_NAME> \
    --display-name="CSP Health Monitor Service Account" \
    --project=<TARGET_PROJECT_ID>

# 2. Create custom IAM role with minimal permissions
gcloud iam roles create cspHealthMonitorRole \
    --project=<TARGET_PROJECT_ID> \
    --title="CSP Health Monitor Role" \
    --description="Minimal permissions for CSP Health Monitor" \
    --permissions="logging.logEntries.list"

# 3. Grant role to GCP Service Account
gcloud projects add-iam-policy-binding <TARGET_PROJECT_ID> \
    --member="serviceAccount:<GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com" \
    --role="projects/<TARGET_PROJECT_ID>/roles/cspHealthMonitorRole"

# 4. Enable Workload Identity binding
gcloud iam service-accounts add-iam-policy-binding \
    <GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com \
    --role="roles/iam.workloadIdentityUser" \
    --member="serviceAccount:<GKE_PROJECT_ID>.svc.id.goog[<NAMESPACE>/csp-health-monitor]"
```

### Helm Configuration

```yaml
csp-health-monitor:
  cspName: "gcp"
  configToml:
    clusterName: "my-gke-cluster"
    gcp:
      targetProjectId: "<TARGET_PROJECT_ID>"
      gcpServiceAccountName: "<GCP_SA_NAME>"
      apiPollingIntervalSeconds: 60
      logFilter: 'logName="projects/<TARGET_PROJECT_ID>/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'
```

## Amazon Web Services (AWS)

### Required IAM Permissions

- `health:DescribeEvents` - Query AWS Health API for maintenance events
- `health:DescribeAffectedEntities` - Get affected EC2 instance IDs
- `health:DescribeEventDetails` - Get event details and recommended actions

### Setup Commands

Replace placeholders:
- `<CLUSTER_NAME>` - EKS cluster name
- `<NAMESPACE>` - Kubernetes namespace (default: `nvsentinel`)

```bash
# 1. Create IAM policy
aws iam create-policy \
    --policy-name CSPHealthMonitorPolicy \
    --policy-document '{
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": [
                    "health:DescribeEvents",
                    "health:DescribeAffectedEntities",
                    "health:DescribeEventDetails"
                ],
                "Resource": "*"
            }
        ]
    }'

# 2. Get OIDC provider and Account ID
OIDC_PROVIDER=$(aws eks describe-cluster --name <CLUSTER_NAME> --query "cluster.identity.oidc.issuer" --output text | sed 's|https://||')
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

# 3. Create trust policy file
cat > trust-policy.json << EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "Federated": "arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
            },
            "Action": "sts:AssumeRoleWithWebIdentity",
            "Condition": {
                "StringEquals": {
                    "${OIDC_PROVIDER}:aud": "sts.amazonaws.com",
                    "${OIDC_PROVIDER}:sub": "system:serviceaccount:<NAMESPACE>:csp-health-monitor"
                }
            }
        }
    ]
}
EOF

# 4. Create IAM role
aws iam create-role \
    --role-name <CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy \
    --assume-role-policy-document file://trust-policy.json

# 5. Attach policy to role
aws iam attach-role-policy \
    --role-name <CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy \
    --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/CSPHealthMonitorPolicy
```

### Helm Configuration

```yaml
csp-health-monitor:
  cspName: "aws"
  configToml:
    clusterName: "<CLUSTER_NAME>"
    aws:
      accountId: "<ACCOUNT_ID>"
      region: "<AWS_REGION>"
      pollingIntervalSeconds: 60
      # Optional: override the default IAM role name
      # iamRoleName: "my-custom-nvsentinel-role"
```

> **Important (EKS)**: By default, the IAM role name is constructed as `<CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy`. AWS IAM role names have a **64-character limit**, and the default suffix is 45 characters, leaving only **19 characters** for the cluster name. If your cluster name exceeds 19 characters, set `aws.iamRoleName` to a custom role name and create the IAM role with that name instead:
>
> ```bash
> aws iam create-role \
>     --role-name my-custom-nvsentinel-role \
>     --assume-role-policy-document file://trust-policy.json
> ```
>
> Then in Helm values:
> ```yaml
> aws:
>   iamRoleName: "my-custom-nvsentinel-role"
> ```

## Additional Resources

- **Configuration Guide**: See [docs/configuration/csp-health-monitor.md](./configuration/csp-health-monitor.md) for detailed Helm configuration options
- **Troubleshooting**: See [docs/runbooks/csp-health-monitor.md](./runbooks/csp-health-monitor.md) for common issues and solutions
