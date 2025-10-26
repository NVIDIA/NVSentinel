#!/bin/bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../common.sh"

CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-uat}"
AWS_REGION="${AWS_REGION:-us-east-1}"
GPU_AVAILABILITY_ZONE="${GPU_AVAILABILITY_ZONE:-e}"

delete_gpu_nodegroup() {
    log "Deleting GPU node group..."
    
    if ! eksctl delete nodegroup \
        --cluster="$CLUSTER_NAME" \
        --name="gpu-nodes" \
        --region="$AWS_REGION" \
        --wait 2>/dev/null; then
        log "GPU node group not found or already deleted"
    else
        log "GPU node group deleted ✓"
    fi
}

delete_gpu_subnet() {
    log "Checking for GPU subnet..."
    
    local vpc_id
    vpc_id=$(aws eks describe-cluster \
        --name "$CLUSTER_NAME" \
        --region "$AWS_REGION" \
        --query 'cluster.resourcesVpcConfig.vpcId' \
        --output text 2>/dev/null || echo "")
    
    if [[ -z "$vpc_id" || "$vpc_id" == "None" ]]; then
        log "Could not get VPC ID, skipping GPU subnet deletion"
        return 0
    fi
    
    local az="${AWS_REGION}${GPU_AVAILABILITY_ZONE}"
    
    local gpu_subnet_id
    gpu_subnet_id=$(aws ec2 describe-subnets \
        --filters "Name=vpc-id,Values=$vpc_id" \
                  "Name=availability-zone,Values=$az" \
                  "Name=tag:kubernetes.io/cluster/${CLUSTER_NAME},Values=owned" \
        --query 'Subnets[0].SubnetId' \
        --output text \
        --region "$AWS_REGION" 2>/dev/null || echo "None")
    
    if [[ "$gpu_subnet_id" == "None" || -z "$gpu_subnet_id" ]]; then
        log "No GPU subnet found, skipping"
        return 0
    fi
    
    log "Deleting GPU subnet: $gpu_subnet_id"
    if ! aws ec2 delete-subnet \
        --subnet-id "$gpu_subnet_id" \
        --region "$AWS_REGION" 2>/dev/null; then
        log "WARNING: Failed to delete GPU subnet, it may have dependencies"
    else
        log "GPU subnet deleted ✓"
    fi
}

delete_cluster() {
    log "Deleting EKS cluster: $CLUSTER_NAME in region $AWS_REGION..."
    
    if ! eksctl delete cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" --wait; then
        log "WARNING: eksctl delete failed. The cluster may be partially deleted."
        log "Check AWS Console for remaining resources:"
        log "  - CloudFormation stacks: https://console.aws.amazon.com/cloudformation"
        log "  - EKS clusters: https://console.aws.amazon.com/eks"
        log "  - EC2 instances: https://console.aws.amazon.com/ec2"
        return 1
    fi
    
    log "EKS cluster deleted successfully ✓"
}

main() {
    log "Starting EKS cluster deletion for NVSentinel UAT..."
    
    delete_gpu_nodegroup
    delete_gpu_subnet
    delete_cluster
    
    
    log "EKS cluster deleted successfully ✓"
}

main "$@"
