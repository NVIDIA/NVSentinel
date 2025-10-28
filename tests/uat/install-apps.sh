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

# Detect platform architecture
ARCH=$(uname -m)
log "Detected architecture: $ARCH"

# Load versions from .versions.yaml
KWOK_VERSION=$(yq eval '.testing_tools.kwok' "$VERSIONS_FILE")
KWOK_CHART_VERSION=$(yq eval '.testing_tools.kwok_chart' "$VERSIONS_FILE")
HELM_VERSION=$(yq eval '.testing_tools.helm' "$VERSIONS_FILE")
PROMETHEUS_OPERATOR_VERSION=$(yq eval '.cluster.prometheus_operator' "$VERSIONS_FILE")
GPU_OPERATOR_VERSION=$(yq eval '.cluster.gpu_operator' "$VERSIONS_FILE")
CERT_MANAGER_VERSION=$(yq eval '.cluster.cert_manager' "$VERSIONS_FILE")

# Configuration
CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-uat}"
AWS_REGION="${AWS_REGION:-us-east-1}"
CSP="${CSP:-kind}"  # Default to kind for local development
NVSENTINEL_VERSION="${NVSENTINEL_VERSION:-}"
FAKE_GPU_NODE_COUNT="${FAKE_GPU_NODE_COUNT:-10}"

VALUES_DIR="${SCRIPT_DIR}/${CSP}"

PROMETHEUS_VALUES="${VALUES_DIR}/prometheus-operator-values.yaml"
GPU_OPERATOR_VALUES="${VALUES_DIR}/gpu-operator-values.yaml"
CERT_MANAGER_VALUES="${VALUES_DIR}/cert-manager-values.yaml"
NVSENTINEL_VALUES="${VALUES_DIR}/nvsentinel-values.yaml"
NVSENTINEL_CHART="${REPO_ROOT}/distros/kubernetes/nvsentinel"

# ARM64-specific values file (if needed)
NVSENTINEL_ARM64_VALUES="${REPO_ROOT}/distros/kubernetes/nvsentinel/values-tilt-arm64.yaml"

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
        --wait \
        --timeout 10m; then
        error "Failed to install cert-manager"
    fi
    
    log "Waiting for cert-manager webhook to be ready..."
    if ! kubectl wait --for=condition=available --timeout=5m \
        -n cert-manager deployment/cert-manager-webhook; then
        log "WARNING: cert-manager webhook did not become ready in time"
        log "Checking webhook pod status:"
        kubectl get pods -n cert-manager -l app.kubernetes.io/component=webhook
        kubectl describe pods -n cert-manager -l app.kubernetes.io/component=webhook | tail -30
    fi
    
    log "cert-manager installed successfully ✓"
}

install_kwok() {
    log "Installing KWOK (app version: $KWOK_VERSION, chart version: $KWOK_CHART_VERSION)..."
    
    helm repo add sigs-kwok https://kwok.sigs.k8s.io/charts/
    helm repo update
    
    if ! helm upgrade --install kwok sigs-kwok/kwok \
        --namespace kube-system \
        --version "$KWOK_CHART_VERSION" \
        --set hostNetwork=true \
        --wait \
        --timeout=5m; then
        error "Failed to install KWOK"
    fi
    
    log "KWOK controller installed successfully ✓"
    
    if ! helm upgrade --install kwok-stage-fast sigs-kwok/stage-fast \
        --namespace kube-system \
        --version "$KWOK_CHART_VERSION" \
        --wait \
        --timeout=5m; then
        error "Failed to install KWOK stage-fast"
    fi
    
    log "KWOK stage-fast installed successfully ✓"
}

create_fake_gpu_nodes() {
    log "Creating $FAKE_GPU_NODE_COUNT fake GPU nodes..."
    
    local node_template="${VALUES_DIR}/kwok-node-template.yaml"
    
    if [[ ! -f "$node_template" ]]; then
        error "KWOK node template not found at $node_template"
    fi
    
    for i in $(seq 1 "$FAKE_GPU_NODE_COUNT"); do
        if ! sed "s/kwok-node-PLACEHOLDER/kwok-gpu-node-${i}/g" "$node_template" | kubectl apply -f -; then
            error "Failed to create fake GPU node kwok-gpu-node-${i}"
        fi
    done
    
    log "Fake GPU nodes created successfully ✓"
    kubectl get nodes -l type=kwok
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

install_fake_gpu_stack() {
    log "Installing fake GPU driver and DCGM for Kind..."
    
    kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f -
    
    if ! kubectl apply -f "${VALUES_DIR}/nvidia-driver-daemonset.yaml"; then
        error "Failed to install fake GPU driver"
    fi
    
    if ! kubectl apply -f "${VALUES_DIR}/nvidia-dcgm-daemonset.yaml"; then
        error "Failed to install fake DCGM"
    fi
    
    log "Fake GPU stack installed successfully ✓"
}

wait_for_fake_gpu_stack() {
    log "Waiting for fake GPU daemonsets to be ready..."
    
    if ! kubectl rollout status daemonset/nvidia-driver-daemonset \
        -n gpu-operator \
        --timeout=5m; then
        error "Fake GPU driver daemonset did not become ready"
    fi
    
    if ! kubectl rollout status daemonset/nvidia-dcgm \
        -n gpu-operator \
        --timeout=5m; then
        error "Fake DCGM daemonset did not become ready"
    fi
    
    log "Fake GPU stack is ready ✓"
}

install_nvsentinel() {
    log "Installing NVSentinel (version: $NVSENTINEL_VERSION)..."
    
    # Validate required variables
    if [[ -z "$NVSENTINEL_CHART" ]]; then
        error "NVSENTINEL_CHART is not set"
    fi
    
    if [[ ! -d "$NVSENTINEL_CHART" ]]; then
        error "NVSentinel chart directory not found: $NVSENTINEL_CHART"
    fi
    
    if [[ -z "$NVSENTINEL_VALUES" ]]; then
        error "NVSENTINEL_VALUES is not set"
    fi
    
    if [[ ! -f "$NVSENTINEL_VALUES" ]]; then
        error "NVSentinel values file not found: $NVSENTINEL_VALUES"
    fi
    
    log "Using chart: $NVSENTINEL_CHART"
    log "Using values: $NVSENTINEL_VALUES"
    
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
    
    # Build helm command with proper array handling
    local helm_args=(
        "upgrade" "--install" "nvsentinel" "$NVSENTINEL_CHART"
        "--namespace" "nvsentinel"
        "--create-namespace"
        "--values" "$NVSENTINEL_VALUES"
    )
    
    # Add ARM64-specific values if on ARM architecture
    if [[ "$ARCH" == "arm64" ]] || [[ "$ARCH" == "aarch64" ]]; then
        if [[ -f "$NVSENTINEL_ARM64_VALUES" ]]; then
            log "Using ARM64-specific values: $NVSENTINEL_ARM64_VALUES"
            helm_args+=("--values" "$NVSENTINEL_ARM64_VALUES")
        else
            log "WARNING: ARM64 architecture detected but ARM64 values file not found: $NVSENTINEL_ARM64_VALUES"
        fi
    fi
    
    helm_args+=("--set" "global.image.tag=$NVSENTINEL_VERSION")
    
    # Add CSP-specific args if any
    if [[ ${#extra_set_args[@]} -gt 0 ]]; then
        helm_args+=("${extra_set_args[@]}")
    fi
    
    helm_args+=("--timeout" "20m" "--wait")
    
    if ! helm "${helm_args[@]}"; then
        error "Failed to install NVSentinel"
    fi
    
    log "NVSentinel installed successfully ✓"
}

main() {
    log "Starting application installation for NVSentinel UAT testing..."
    log "Architecture: $ARCH"
    log "Target CSP: $CSP (override with CSP=<aws|azure|gcp|kind|oci>)"
    log "Versions from .versions.yaml:"
    log "  - KWOK: $KWOK_VERSION (chart: $KWOK_CHART_VERSION)"
    log "  - Prometheus Operator: $PROMETHEUS_OPERATOR_VERSION"
    log "  - GPU Operator: $GPU_OPERATOR_VERSION"
    log "  - cert-manager: $CERT_MANAGER_VERSION"
    
    if [[ "$ARCH" == "arm64" ]] || [[ "$ARCH" == "aarch64" ]]; then
        log "ARM64 architecture detected - using compatible image overrides for MongoDB"
    fi
    
    install_prometheus_operator
    install_cert_manager
    
    if [[ "$CSP" == "kind" ]]; then
        install_kwok
        create_fake_gpu_nodes
        install_fake_gpu_stack
        wait_for_fake_gpu_stack
    else
        install_gpu_operator
        wait_for_gpu_operator
    fi
    
    install_nvsentinel
    
    log "All applications installed successfully ✓"
}

main "$@"
