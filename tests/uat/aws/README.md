# AWS EKS UAT Testing

Scripts to create and delete an EKS cluster for NVSentinel UAT testing.

## Prerequisites

Install the following tools:

- **AWS CLI** (v2.x or later) - `aws --version`
- **eksctl** (v0.150.0 or later) - `eksctl version`
- **kubectl** (v1.28 or later) - `kubectl version --client`
- **envsubst** (gettext package) - Install: `apt-get install gettext-base` or `brew install gettext`

Configure AWS credentials with permissions for EKS, EC2, VPC, IAM, and Capacity Reservations:

```bash
aws configure
```

## Environment Variables

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `CLUSTER_NAME` | `nvsentinel-uat` | No | Name of the EKS cluster |
| `AWS_REGION` | `us-east-1` | No | AWS region to deploy the cluster |
| `K8S_VERSION` | `1.34` | No | Kubernetes version for the cluster |
| `GPU_AVAILABILITY_ZONE` | `e` | No | Availability zone suffix for GPU nodes (e.g., 'e' = us-east-1e) |
| `CPU_NODE_TYPE` | `m7a.4xlarge` | No | EC2 instance type for CPU nodes |
| `CPU_NODE_COUNT` | `3` | No | Number of CPU nodes for system services |
| `GPU_NODE_TYPE` | `p5.48xlarge` | No | EC2 instance type for GPU nodes |
| `GPU_NODE_COUNT` | `2` | No | Number of GPU nodes |
| `CAPACITY_RESERVATION_ID` | (none) | **Yes** | EC2 Capacity Reservation ID for GPU instances |

## Usage

### 1. Create Capacity Reservation

Create an EC2 Capacity Reservation for GPU instances:

```bash
aws ec2 create-capacity-reservation \
  --instance-type p5.48xlarge \
  --instance-platform Linux/UNIX \
  --availability-zone us-east-1e \
  --instance-count 2 \
  --region us-east-1
```

Note the returned Capacity Reservation ID (e.g., `cr-0123456789abcdef0`).

### 2. Create EKS Cluster

Set the required environment variable and run the creation script:

```bash
export CAPACITY_RESERVATION_ID="cr-0123456789abcdef0"

# Optional: Override defaults
export CLUSTER_NAME="nvsentinel-uat"
export AWS_REGION="us-east-1"
export GPU_AVAILABILITY_ZONE="e"

cd tests/uat/aws
./create-eks-cluster.sh
```

The cluster creation takes approximately 15-20 minutes.

### 3. Verify Cluster

```bash
kubectl get nodes -o wide
kubectl get nodes -l workload-type=system
kubectl get nodes -l workload-type=gpu
```

### 4. Delete EKS Cluster

When done with testing:

```bash
cd tests/uat/aws
./delete-eks-cluster.sh
```

The cluster deletion takes approximately 10-15 minutes.

### 5. Cancel Capacity Reservation

Manually cancel the capacity reservation to stop charges:

```bash
aws ec2 cancel-capacity-reservation \
  --capacity-reservation-id "$CAPACITY_RESERVATION_ID" \
  --region "$AWS_REGION"
```

## Cost Considerations

| Resource | Cost |
|----------|------|
| 2x p5.48xlarge (GPU nodes) | ~$196/hour |
| 3x m7a.4xlarge (CPU nodes) | ~$2.31/hour |
| **Total** | **~$198.31/hour** (~$4,759/day) |

⚠️ **Always delete the cluster and cancel capacity reservations when not in use!**

## Files

- `create-eks-cluster.sh` - Creates the EKS cluster
- `delete-eks-cluster.sh` - Deletes the EKS cluster
- `eks-cluster-config.yaml.template` - Base cluster configuration template
- `eks-gpu-nodegroup-config.yaml.template` - GPU nodegroup configuration template
