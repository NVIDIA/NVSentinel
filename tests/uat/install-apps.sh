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

CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-uat}"
AWS_REGION="${AWS_REGION:-us-east-1}"
CSP="${CSP:-aws}"

PROMETHEUS_OPERATOR_VERSION="${PROMETHEUS_OPERATOR_VERSION:-78.5.0}"
GPU_OPERATOR_VERSION="${GPU_OPERATOR_VERSION:-v25.10.0}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-1.19.1}"
NVSENTINEL_VERSION="${NVSENTINEL_VERSION:-}"

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
VALUES_DIR="${SCRIPT_DIR}/${CSP}"

PROMETHEUS_VALUES="${VALUES_DIR}/prometheus-operator-values.yaml"
GPU_OPERATOR_VALUES="${VALUES_DIR}/gpu-operator-values.yaml"
CERT_MANAGER_VALUES="${VALUES_DIR}/cert-manager-values.yaml"
NVSENTINEL_VALUES="${VALUES_DIR}/nvsentinel-values.yaml"
NVSENTINEL_CHART="${REPO_ROOT}/distros/kubernetes/nvsentinel"

install_prometheus_operator() {
    log "Installing Prometheus Operator (version $PROMETHEUS_OPERATOR_VERSION)..."
    
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
    helm repo update
    
    if ! helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
        --namespace monitoring \
        --create-namespace \
        --values "$PROMETHEUS_VALUES" \
        --version "$PROMETHEUS_OPERATOR_VERSION" \
        --wait; then
        error "Failed to install Prometheus Operator"
    fi
    
    log "Prometheus Operator installed successfully ✓"
}

install_cert_manager() {
    log "Installing cert-manager (version $CERT_MANAGER_VERSION)..."
    
    helm repo add jetstack https://charts.jetstack.io
    helm repo update
    
    if ! helm upgrade --install cert-manager jetstack/cert-manager \
        --namespace cert-manager \
        --create-namespace \
        --values "$CERT_MANAGER_VALUES" \
        --version "$CERT_MANAGER_VERSION" \
        --wait; then
        error "Failed to install cert-manager"
    fi
    
    log "cert-manager installed successfully ✓"
}

install_gpu_operator() {
    log "Installing NVIDIA GPU Operator (version $GPU_OPERATOR_VERSION)..."
    
    helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
    helm repo update
    
    if ! helm upgrade --install gpu-operator nvidia/gpu-operator \
        --namespace gpu-operator \
        --create-namespace \
        --values "$GPU_OPERATOR_VALUES" \
        --version "$GPU_OPERATOR_VERSION" \
        --wait; then
        error "Failed to install GPU Operator"
    fi
    
    log "GPU Operator installed successfully ✓"
}

wait_for_gpu_operator() {
    log "Waiting for GPU drivers to be installed..."
    
    if ! kubectl wait --for=condition=Ready \
        clusterpolicy/cluster-policy \
        -n gpu-operator \
        --timeout=15m; then
        error "GPU Operator ClusterPolicy did not become ready"
    fi
    
    log "GPU Operator ClusterPolicy is ready ✓"
}

install_nvsentinel() {
    log "Installing NVSentinel (version: $NVSENTINEL_VERSION)..."
    
    local extra_set_args=()
    if [[ "$CSP" == "aws" ]]; then
        local aws_account_id
        aws_account_id=$(aws sts get-caller-identity --query Account --output text)
        
        local janitor_role_name="${CLUSTER_NAME}-janitor"
        
        extra_set_args+=(
            "--set" "janitor.csp.aws.region=$AWS_REGION"
            "--set" "janitor.csp.aws.accountId=$aws_account_id"
            "--set" "janitor.csp.aws.iamRoleName=$janitor_role_name"
        )
    fi
    
    if ! helm upgrade --install nvsentinel "$NVSENTINEL_CHART" \
        --namespace nvsentinel \
        --create-namespace \
        --values "$NVSENTINEL_VALUES" \
        --set global.image.tag="$NVSENTINEL_VERSION" \
        "${extra_set_args[@]}" \
        --timeout 20m \
        --wait; then
        error "Failed to install NVSentinel"
    fi
    
    log "NVSentinel installed successfully ✓"
}

main() {
    log "Starting application installation for NVSentinel UAT testing..."
    
    install_prometheus_operator
    install_cert_manager
    install_gpu_operator
    wait_for_gpu_operator
    install_nvsentinel
    
    log "All applications installed successfully ✓"
}

main "$@"
