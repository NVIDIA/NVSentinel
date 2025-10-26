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
K8S_VERSION="${K8S_VERSION:-1.34}"
GPU_AVAILABILITY_ZONE="${GPU_AVAILABILITY_ZONE:-e}"

CPU_NODE_TYPE="${CPU_NODE_TYPE:-m7a.4xlarge}"
CPU_NODE_COUNT="${CPU_NODE_COUNT:-3}"

GPU_NODE_TYPE="${GPU_NODE_TYPE:-p5.48xlarge}"
GPU_NODE_COUNT="${GPU_NODE_COUNT:-2}"

CAPACITY_RESERVATION_ID="${CAPACITY_RESERVATION_ID:-}"

BASE_TEMPLATE_FILE="${SCRIPT_DIR}/eks-cluster-config.yaml.template"
GPU_TEMPLATE_FILE="${SCRIPT_DIR}/eks-gpu-nodegroup-config.yaml.template"
CONFIG_FILE="/tmp/eks-cluster-config-${CLUSTER_NAME}.yaml"

create_base_cluster() {
    log "Creating base EKS cluster (CPU nodes only)..."
    log "This will take approximately 15-20 minutes..."
    
    export CLUSTER_NAME
    export AWS_REGION
    export K8S_VERSION
    export CPU_NODE_TYPE
    export CPU_NODE_COUNT
    
    envsubst < "$BASE_TEMPLATE_FILE" > "$CONFIG_FILE"
    
    if ! eksctl create cluster -f "$CONFIG_FILE" --install-nvidia-plugin=false; then
        error "Failed to create base EKS cluster"
    fi
    
    log "Base EKS cluster created successfully ✓"
}

get_vpc_id() {
    log "Getting VPC ID..." >&2
    
    local vpc_id
    vpc_id=$(aws eks describe-cluster \
        --name "$CLUSTER_NAME" \
        --region "$AWS_REGION" \
        --query 'cluster.resourcesVpcConfig.vpcId' \
        --output text)
    
    if [[ -z "$vpc_id" || "$vpc_id" == "None" ]]; then
        error "Could not get VPC ID for cluster"
    fi
    
    log "VPC ID: $vpc_id" >&2
    echo "$vpc_id"
}

create_gpu_subnet() {
    local vpc_id="$1"
    local az="${AWS_REGION}${GPU_AVAILABILITY_ZONE}"
    
    log "Checking for existing subnet in $az..." >&2
    
    local existing_subnet
    existing_subnet=$(aws ec2 describe-subnets \
        --filters "Name=vpc-id,Values=$vpc_id" \
                  "Name=availability-zone,Values=$az" \
                  "Name=tag:kubernetes.io/cluster/${CLUSTER_NAME},Values=owned" \
        --query 'Subnets[0].SubnetId' \
        --output text \
        --region "$AWS_REGION" 2>/dev/null || echo "None")
    
    if [[ "$existing_subnet" != "None" && -n "$existing_subnet" ]]; then
        log "Using existing subnet: $existing_subnet" >&2
        echo "$existing_subnet"
        return 0
    fi
    
    log "Creating private subnet in $az..." >&2
    
    local subnet_cidr="192.168.128.0/19"
    log "Using CIDR: $subnet_cidr" >&2
    
    local subnet_id
    subnet_id=$(aws ec2 create-subnet \
        --vpc-id "$vpc_id" \
        --cidr-block "$subnet_cidr" \
        --availability-zone "$az" \
        --tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=${CLUSTER_NAME}-private-${az}},{Key=kubernetes.io/role/internal-elb,Value=1},{Key=kubernetes.io/cluster/${CLUSTER_NAME},Value=owned}]" \
        --region "$AWS_REGION" \
        --query 'Subnet.SubnetId' \
        --output text)
    
    if [[ -z "$subnet_id" ]]; then
        error "Failed to create subnet"
    fi
    
    log "Created subnet: $subnet_id" >&2
    
    local route_table_id
    route_table_id=$(aws ec2 describe-route-tables \
        --filters "Name=vpc-id,Values=$vpc_id" \
                  "Name=tag:Name,Values=*rivate*" \
        --query 'RouteTables[0].RouteTableId' \
        --output text \
        --region "$AWS_REGION" 2>/dev/null || echo "None")
    
    if [[ "$route_table_id" != "None" && -n "$route_table_id" ]]; then
        log "Associating subnet with route table: $route_table_id" >&2
        aws ec2 associate-route-table \
            --subnet-id "$subnet_id" \
            --route-table-id "$route_table_id" \
            --region "$AWS_REGION" &> /dev/null || log "Warning: Could not associate route table" >&2
    fi
    
    log "Subnet created successfully ✓" >&2
    echo "$subnet_id"
}

generate_cluster_config() {
    local gpu_subnet_id="$1"
    
    log "Generating cluster configuration with GPU nodes..."
    
    export CLUSTER_NAME
    export AWS_REGION
    export K8S_VERSION
    export GPU_AVAILABILITY_ZONE
    export CPU_NODE_TYPE
    export CPU_NODE_COUNT
    export GPU_NODE_TYPE
    export GPU_NODE_COUNT
    export CAPACITY_RESERVATION_ID
    export GPU_SUBNET_ID="$gpu_subnet_id"
    
    envsubst < "$GPU_TEMPLATE_FILE" > "$CONFIG_FILE"
    
    log "Cluster configuration generated: $CONFIG_FILE"
}

add_gpu_nodegroup() {
    log "Adding GPU node group..."
    
    if ! eksctl create nodegroup -f "$CONFIG_FILE" --install-nvidia-plugin=false; then
        error "Failed to add GPU node group"
    fi
    
    log "GPU node group added successfully ✓"
}

main() {
    log "Starting EKS cluster creation for NVSentinel UAT testing..."
    
    create_base_cluster
    
    local vpc_id
    vpc_id=$(get_vpc_id)
    
    local gpu_subnet_id
    gpu_subnet_id=$(create_gpu_subnet "$vpc_id")
    
    generate_cluster_config "$gpu_subnet_id"
    add_gpu_nodegroup
    
    log "EKS cluster created successfully ✓"
}

main "$@"
