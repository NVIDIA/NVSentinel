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
source "${SCRIPT_DIR}/common.sh"

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
VERSIONS_FILE="${REPO_ROOT}/.versions.yaml"

# Verify yq is available for parsing versions
if ! command -v yq &> /dev/null; then
    error "yq is required but not installed. Please install yq: https://github.com/mikefarah/yq"
fi

# Load versions from .versions.yaml that are exported to install-apps.sh
PROMETHEUS_OPERATOR_VERSION=$(yq eval '.cluster.prometheus_operator' "$VERSIONS_FILE")
GPU_OPERATOR_VERSION=$(yq eval '.cluster.gpu_operator' "$VERSIONS_FILE")
CERT_MANAGER_VERSION=$(yq eval '.cluster.cert_manager' "$VERSIONS_FILE")

# Validate required versions were loaded
if [[ -z "$PROMETHEUS_OPERATOR_VERSION" ]] || [[ -z "$GPU_OPERATOR_VERSION" ]] || [[ -z "$CERT_MANAGER_VERSION" ]]; then
    error "Failed to load required versions from $VERSIONS_FILE. Please verify the file exists and contains required keys."
fi

# Note: KWOK versions are loaded directly by install-apps.sh from .versions.yaml

# Configuration
CSP="${CSP:-kind}"  # Default to kind for local development
CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-uat}"
AWS_REGION="${AWS_REGION:-us-east-1}"
K8S_VERSION="${K8S_VERSION:-1.34}"
GPU_AVAILABILITY_ZONE="${GPU_AVAILABILITY_ZONE:-e}"
CPU_NODE_TYPE="${CPU_NODE_TYPE:-m7a.4xlarge}"
CPU_NODE_COUNT="${CPU_NODE_COUNT:-3}"
GPU_NODE_TYPE="${GPU_NODE_TYPE:-p5.48xlarge}"
GPU_NODE_COUNT="${GPU_NODE_COUNT:-2}"
CAPACITY_RESERVATION_ID="${CAPACITY_RESERVATION_ID:-}"

# NVSentinel configuration
NVSENTINEL_VERSION="${NVSENTINEL_VERSION:-}"
NVSENTINEL_TAG="${NVSENTINEL_TAG:-main}"

DELETE_CLUSTER_ON_EXIT="${DELETE_CLUSTER_ON_EXIT:-false}"

cleanup() {
    if [[ "$DELETE_CLUSTER_ON_EXIT" == "true" ]]; then
        log "========================================="
        log "Cleaning up cluster..."
        log "========================================="
        
        # Skip cleanup for Kind clusters (managed externally)
        if [[ "$CSP" == "kind" ]]; then
            log "Kind cluster cleanup skipped (managed externally)"
            return 0
        fi
        
        local original_dir="$PWD"
        
        export CLUSTER_NAME
        export AWS_REGION
        
        # Get the cluster deletion script for this CSP
        local delete_script
        delete_script=$(get_cluster_script "delete")
        
        cd "${SCRIPT_DIR}/${CSP}"
        "./${delete_script}" || log "WARNING: Cluster deletion failed"
        cd "$original_dir"
        
        log "Cluster cleanup completed"
    fi
}

trap cleanup EXIT

check_prerequisites() {
    log "Checking prerequisites..."
    
    if ! command -v kubectl &> /dev/null; then
        error "kubectl is not installed"
    fi
    
    if ! command -v helm &> /dev/null; then
        error "helm is not installed. Install from: https://helm.sh/docs/intro/install/"
    fi
    
    if [[ "$CSP" == "aws" ]]; then
        if ! command -v aws &> /dev/null; then
            error "aws CLI is not installed. Install from: https://aws.amazon.com/cli/"
        fi
        
        if ! command -v eksctl &> /dev/null; then
            error "eksctl is not installed. Install from: https://eksctl.io/installation/"
        fi
        
        if ! command -v envsubst &> /dev/null; then
            error "envsubst is not installed. Install gettext package."
        fi
        
        if ! aws sts get-caller-identity &> /dev/null; then
            error "AWS credentials not configured. Run 'aws configure' or set AWS environment variables."
        fi
        
        if [[ -z "$CAPACITY_RESERVATION_ID" ]]; then
            error "CAPACITY_RESERVATION_ID is required for AWS. Set it via environment variable."
        fi
    fi
    
    if [[ -z "$NVSENTINEL_VERSION" ]]; then
        error "NVSENTINEL_VERSION is required. Set it via environment variable: export NVSENTINEL_VERSION='v0.1.0'"
    fi
    
    local values_dir="${SCRIPT_DIR}/${CSP}"
    if [[ ! -f "${values_dir}/prometheus-operator-values.yaml" ]]; then
        error "Prometheus values file not found: ${values_dir}/prometheus-operator-values.yaml"
    fi
    
    if [[ ! -f "${values_dir}/gpu-operator-values.yaml" ]]; then
        error "GPU Operator values file not found: ${values_dir}/gpu-operator-values.yaml"
    fi
    
    if [[ ! -f "${values_dir}/cert-manager-values.yaml" ]]; then
        error "cert-manager values file not found: ${values_dir}/cert-manager-values.yaml"
    fi
    
    if [[ ! -f "${values_dir}/nvsentinel-values.yaml" ]]; then
        error "NVSentinel values file not found: ${values_dir}/nvsentinel-values.yaml"
    fi
    
    local nvsentinel_chart="${SCRIPT_DIR}/../../distros/kubernetes/nvsentinel"
    if [[ ! -d "$nvsentinel_chart" ]]; then
        error "NVSentinel chart not found: $nvsentinel_chart"
    fi
    
    log "Prerequisites check passed ✓"
}

# Get the cluster management script name for the specified CSP
# Args: $1 - operation type ("create" or "delete")
# Returns: script name via stdout, or calls error() for unsupported CSPs
get_cluster_script() {
    local operation="$1"
    
    case "$CSP" in
        kind)
            # Kind clusters are managed externally - this function should not be called for Kind
            error "get_cluster_script() should not be called for CSP=kind (clusters are managed externally)"
            ;;
        aws)
            if [[ "$operation" == "create" ]]; then
                echo "create-eks-cluster.sh"
            else
                echo "delete-eks-cluster.sh"
            fi
            ;;
        azure|gcp|oci)
            error "CSP '$CSP' cluster $operation not yet implemented. Please manage cluster manually or use CSP=aws or CSP=kind"
            ;;
        *)
            error "Unknown CSP: $CSP. Supported values: kind, aws, azure, gcp, oci"
            ;;
    esac
}

create_cluster() {
    log "========================================="
    log "Creating ${CSP^^} cluster..."
    log "========================================="
    
    # Kind clusters are assumed to be created externally
    if [[ "$CSP" == "kind" ]]; then
        log "Using existing Kind cluster: $CLUSTER_NAME"
        
        # Verify cluster exists
        if ! kubectl cluster-info --context "kind-$CLUSTER_NAME" &> /dev/null; then
            error "Kind cluster '$CLUSTER_NAME' not found. Create it first with: kind create cluster --name $CLUSTER_NAME"
        fi
        
        # Switch to the Kind cluster context
        log "Switching to Kind cluster context..."
        if ! kubectl config use-context "kind-$CLUSTER_NAME" &> /dev/null; then
            error "Failed to switch to Kind cluster context: kind-$CLUSTER_NAME"
        fi
        
        log "Kind cluster verified and context set ✓"
        return 0
    fi
    
    # For cloud CSPs, run the cluster creation script
    export CLUSTER_NAME
    export AWS_REGION
    export K8S_VERSION
    export GPU_AVAILABILITY_ZONE
    export CPU_NODE_TYPE
    export CPU_NODE_COUNT
    export GPU_NODE_TYPE
    export GPU_NODE_COUNT
    export CAPACITY_RESERVATION_ID
    
    # Get the cluster creation script for this CSP
    local cluster_script
    cluster_script=$(get_cluster_script "create")
    
    cd "${SCRIPT_DIR}/${CSP}"
    "./${cluster_script}"
    cd "${SCRIPT_DIR}"
    
    log "Cluster created successfully ✓"
}

install_apps() {
    log "========================================="
    log "Installing applications..."
    log "========================================="
    
    export CLUSTER_NAME
    export AWS_REGION
    export CSP
    export PROMETHEUS_OPERATOR_VERSION
    export GPU_OPERATOR_VERSION
    export CERT_MANAGER_VERSION
    export NVSENTINEL_VERSION
    
    ./install-apps.sh
    
    log "Applications installed successfully ✓"
}

run_tests() {
    log "========================================="
    log "Running UAT tests..."
    log "========================================="
    
    ./tests.sh
    
    log "All tests passed ✓"
}

main() {
    log "========================================="
    log "NVSentinel UAT Test Suite"
    log "========================================="
    log "CSP: $CSP"
    log "Cluster: $CLUSTER_NAME"
    log "NVSentinel Version: $NVSENTINEL_VERSION"
    log "Delete on Exit: $DELETE_CLUSTER_ON_EXIT"
    log "========================================="
    
    check_prerequisites
    create_cluster
    install_apps
    run_tests
    
    log "========================================="
    log "UAT Test Suite Completed Successfully! ✓"
    log "========================================="
}

main "$@"
