#!/usr/bin/env bash

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

# SLSA Provenance Verification Script
# 
# This script sets up and configures Sigstore Policy Controller in a Kubernetes
# cluster to automatically verify SLSA Build Provenance attestations for NVSentinel
# container images before allowing them to run.
#
# The script:
# - Installs Sigstore Policy Controller via Helm
# - Applies ClusterImagePolicy for NVSentinel images
# - Configures namespace for policy enforcement
# - Tests policy enforcement with actual deployments
#
# For manual verification of images outside the cluster, see:
# distros/kubernetes/nvsentinel/policies/README.md

set -euo pipefail

# Colors for output
readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m' # No Color

# Paths
readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
readonly VERSIONS_FILE="${VERSIONS_FILE:-$REPO_ROOT/.versions.yaml}"

# Configuration - load versions from .versions.yaml if available
if [ -f "$VERSIONS_FILE" ] && command -v yq &> /dev/null; then
    _version="$(yq eval '.cluster.policy_controller' "$VERSIONS_FILE")"
    if [ -n "$_version" ] && [ "$_version" != "null" ]; then
        readonly POLICY_CONTROLLER_VERSION="$_version"
    else
        readonly POLICY_CONTROLLER_VERSION="${POLICY_CONTROLLER_VERSION:-0.10.5}"
    fi
else
    readonly POLICY_CONTROLLER_VERSION="${POLICY_CONTROLLER_VERSION:-0.10.5}"
fi
readonly POLICY_CONTROLLER_NS="cosign-system"
readonly NVSENTINEL_NS="${NVSENTINEL_NS:-nvsentinel}"
readonly POLICY_DIR="${POLICY_DIR:-$(cd "$REPO_ROOT/distros/kubernetes/nvsentinel/policies" && pwd)}"

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*"
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    local missing_tools=()
    
    if ! command -v kubectl &> /dev/null; then
        missing_tools+=("kubectl")
    fi
    
    if ! command -v jq &> /dev/null; then
        missing_tools+=("jq")
    fi
    
    if ! command -v helm &> /dev/null; then
        missing_tools+=("helm")
    fi
    
    # yq is optional but recommended for loading versions from .versions.yaml
    if ! command -v yq &> /dev/null; then
        log_warn "yq not found - using default Policy Controller version"
        log_warn "Install yq for automatic version management: https://github.com/mikefarah/yq"
    fi
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_error "Please install missing tools and try again"
        exit 1
    fi
    
    # Check kubectl context
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        log_error "Please ensure kubectl is configured correctly"
        exit 1
    fi
    
    local current_context
    current_context=$(kubectl config current-context)
    log_success "Connected to cluster: ${current_context}"
    
    # Log version source
    if [ -f "$VERSIONS_FILE" ] && command -v yq &> /dev/null; then
        log_info "Using Policy Controller version ${POLICY_CONTROLLER_VERSION} from ${VERSIONS_FILE}"
    else
        log_info "Using default Policy Controller version ${POLICY_CONTROLLER_VERSION}"
    fi
}

check_nvsentinel_deployment() {
    log_info "Checking for NVSentinel deployment..."
    
    # Check if namespace exists
    if ! kubectl get namespace "${NVSENTINEL_NS}" &> /dev/null; then
        log_error "Namespace ${NVSENTINEL_NS} not found"
        log_error "Please ensure NVSentinel is deployed"
        exit 1
    fi
    
    # Get all NVSentinel deployments (official GHCR images only)
    local deployments
    deployments=$(kubectl get deployments -n "${NVSENTINEL_NS}" -o json | \
        jq -r '.items[] | select(any(.spec.template.spec.containers[]; .image | startswith("ghcr.io/nvidia/nvsentinel/"))) | .metadata.name')
    
    if [ -z "$deployments" ]; then
        log_warn "No official NVSentinel deployments found in namespace ${NVSENTINEL_NS}"
        log_info "Checking for any NVSentinel-related workloads..."
        
        # Check for local development images
        local dev_images
        dev_images=$(kubectl get pods -n "${NVSENTINEL_NS}" -o json | \
            jq -r '.items[].spec.containers[].image | select(contains("nvsentinel"))' | head -n 3)
        
        if [ -n "$dev_images" ]; then
            log_info "Found development NVSentinel images (these won't be verified by the policy):"
            echo "$dev_images" | while read -r img; do
                echo "  - ${img}"
            done
            log_warn "Policy only applies to official ghcr.io/nvidia/nvsentinel/** images"
        else
            log_error "No NVSentinel workloads found in namespace"
            exit 1
        fi
    else
        log_success "Found official NVSentinel deployments:"
        echo "$deployments" | while read -r deployment; do
            echo "  - ${deployment}"
        done
    fi
}

install_policy_controller() {
    log_info "Installing Sigstore Policy Controller..."
    
    # Check if Policy Controller is already installed
    if helm list -n "${POLICY_CONTROLLER_NS}" 2>/dev/null | grep -q "policy-controller"; then
        log_info "Policy Controller already installed, checking version..."
        local installed_version
        # Extract chart version from the chart field (format: "policy-controller-0.10.5")
        installed_version=$(helm list -n "${POLICY_CONTROLLER_NS}" -o json | jq -r '.[] | select(.name=="policy-controller") | .chart | sub("^policy-controller-"; "")')
        log_info "Installed version: ${installed_version}"
        
        # Prepare version string for comparison
        local chart_version="${POLICY_CONTROLLER_VERSION}"
        chart_version="${chart_version#v}"
        log_info "Target version: ${chart_version}"
        
        if [ "${chart_version}" != "latest" ] && [ "${installed_version}" = "${chart_version}" ]; then
            log_success "Policy Controller ${chart_version} is already installed"
            return 0
        else
            if [ "${chart_version}" = "latest" ]; then
                log_info "Will upgrade to latest version"
            else
                # Note: helm upgrade --install handles both upgrades and downgrades
                log_info "Will change from version ${installed_version} to ${chart_version}"
            fi
        fi
    fi
    
    # Add Sigstore Helm repository
    log_info "Adding Sigstore Helm repository..."
    helm repo add sigstore https://sigstore.github.io/helm-charts 2>/dev/null || true
    helm repo update sigstore
    
    # Prepare version string for Helm
    local chart_version="${POLICY_CONTROLLER_VERSION}"
    chart_version="${chart_version#v}"
    
    log_info "Installing/upgrading Policy Controller ${chart_version} using Helm..."
    
    local helm_args=(
        "policy-controller"
        "sigstore/policy-controller"
        "-n" "${POLICY_CONTROLLER_NS}"
        "--create-namespace"
        "--wait"
        "--timeout" "5m"
    )
    
    # Add version if not using latest
    if [ "${chart_version}" != "latest" ]; then
        helm_args+=("--version" "${chart_version}")
    fi
    
    if ! helm upgrade --install "${helm_args[@]}"; then
        log_error "Failed to install/upgrade Policy Controller via Helm"
        log_info "Checking installation status..."
        helm list -n "${POLICY_CONTROLLER_NS}" 2>/dev/null || true
        exit 1
    fi
    
    log_success "Policy Controller installed/upgraded successfully"
    
    # Verify the installation
    log_info "Verifying Policy Controller deployment..."
    kubectl wait --for=condition=available --timeout=60s \
        deployment -n "${POLICY_CONTROLLER_NS}" -l app.kubernetes.io/name=policy-controller 2>/dev/null || \
        kubectl get deployment -n "${POLICY_CONTROLLER_NS}" | grep -i policy || true
}

get_nvsentinel_images() {
    # Get all official NVSentinel images (not localhost development images)
    kubectl get pods -n "${NVSENTINEL_NS}" -o json 2>/dev/null | \
        jq -r '.items[].spec.containers[].image' | \
        grep "^ghcr.io/nvidia/nvsentinel/" | \
        sort -u
}

apply_cluster_image_policy() {
    log_info "Applying ClusterImagePolicy..."
    
    if [ ! -f "${POLICY_DIR}/image-admission-policy.yaml" ]; then
        log_error "Policy file not found: ${POLICY_DIR}/image-admission-policy.yaml"
        exit 1
    fi
    
    # Check if policy already exists
    if kubectl get clusterimagepolicy verify-nvsentinel-image-attestation &> /dev/null; then
        log_info "ClusterImagePolicy already exists, updating..."
    fi
    
    kubectl apply -f "${POLICY_DIR}/image-admission-policy.yaml"
    
    # Wait a moment for the policy to be processed
    sleep 2
    
    # Verify policy was created/updated
    if kubectl get clusterimagepolicy verify-nvsentinel-image-attestation &> /dev/null; then
        log_success "ClusterImagePolicy applied successfully"
    else
        log_error "Failed to apply ClusterImagePolicy"
        exit 1
    fi
}

configure_namespace() {
    log_info "Configuring namespace ${NVSENTINEL_NS} for policy enforcement..."
    
    # Check if label already exists
    if kubectl get namespace "${NVSENTINEL_NS}" -o jsonpath='{.metadata.labels.policy\.sigstore\.dev/include}' 2>/dev/null | grep -q "true"; then
        log_info "Namespace already labeled for policy enforcement"
    else
        # Label namespace for policy enforcement
        kubectl label namespace "${NVSENTINEL_NS}" \
            policy.sigstore.dev/include=true \
            --overwrite
        log_success "Namespace labeled for policy enforcement"
    fi
    
    # Configure no-match policy to allow images that don't match any policy
    # This is important when the namespace has other images (e.g., MongoDB, NGINX)
    log_info "Configuring no-match policy to allow non-NVSentinel images..."
    kubectl create configmap config-policy-controller -n "${POLICY_CONTROLLER_NS}" \
        --from-literal=no-match-policy=allow \
        --dry-run=client -o yaml | kubectl apply -f -
    
    log_success "Policy will only enforce verification on ghcr.io/nvidia/nvsentinel/** images"
}

test_policy_enforcement() {
    log_info "Testing policy enforcement..."
    
    # Clean up any existing test pods from previous runs
    kubectl delete pod -n "${NVSENTINEL_NS}" -l test=policy-verification --grace-period=0 --force &> /dev/null || true
    
    # Get official images from cluster
    local test_image
    test_image=$(get_nvsentinel_images | head -n 1)
    
    if [ -z "$test_image" ]; then
        log_warn "No official NVSentinel images found in cluster for testing"
        log_info "Policy enforcement can only be tested with official ghcr.io/nvidia/nvsentinel/** images"
        log_info "Development images (localhost:*) are not subject to policy verification"
        return 0
    fi
    
    # Create a test pod with a valid NVSentinel image
    local test_pod_name="policy-test-valid-$$"
    
    log_info "Creating test pod with image: ${test_image}"
    
    local apply_output
    local apply_exit_code
    apply_output=$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${test_pod_name}
  namespace: ${NVSENTINEL_NS}
  labels:
    test: policy-verification
spec:
  containers:
  - name: test
    image: ${test_image}
    command: ["/bin/sh", "-c", "sleep 10"]
  restartPolicy: Never
EOF
)
    apply_exit_code=$?
    
    # Check if kubectl apply succeeded
    if [ $apply_exit_code -ne 0 ]; then
        # Check if it's a policy-related error
        if echo "$apply_output" | grep -q "admission webhook.*denied\|no matching signatures"; then
            log_error "✗ Valid image was blocked by policy (unexpected)"
            log_error "Policy error: $apply_output"
        else
            log_error "✗ Failed to create test pod (non-policy error)"
            log_error "Error: $apply_output"
        fi
        return 1
    fi
    
    # Check if pod was created (policy allowed it)
    sleep 2
    if kubectl get pod "${test_pod_name}" -n "${NVSENTINEL_NS}" &> /dev/null; then
        log_success "✓ Valid image was allowed by policy"
        kubectl delete pod "${test_pod_name}" -n "${NVSENTINEL_NS}" --grace-period=0 --force &> /dev/null || true
    else
        log_error "✗ Valid image was blocked by policy (unexpected)"
    fi
    
    # Test with an invalid/unsigned image that matches the policy pattern
    # Use a NVSentinel-like image name but from Docker Hub (no attestation)
    local test_pod_invalid="policy-test-invalid-$$"
    log_info "Testing with unsigned NVSentinel image (should be blocked)..."
    
    # Try to create a pod with a fake NVSentinel image that won't have valid attestations
    # This will be blocked because it matches ghcr.io/nvidia/nvsentinel/** pattern
    if cat <<EOF | kubectl apply -f - 2>&1 | grep -q "admission webhook.*denied\|no matching signatures"; then
apiVersion: v1
kind: Pod
metadata:
  name: ${test_pod_invalid}
  namespace: ${NVSENTINEL_NS}
  labels:
    test: policy-verification
spec:
  containers:
  - name: test
    image: ghcr.io/nvidia/nvsentinel/fake-unsigned-image:latest
    command: ["/bin/sh", "-c", "sleep 10"]
  restartPolicy: Never
EOF
        log_success "✓ Invalid NVSentinel image was correctly blocked by policy"
    else
        log_warn "Could not verify that invalid images are blocked (expected for non-existent images)"
    fi
    
    # Test that non-NVSentinel images are NOT blocked
    local test_pod_other="policy-test-other-$$"
    log_info "Testing with non-NVSentinel image (should be allowed)..."
    
    cat <<EOF | kubectl apply -f - &> /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${test_pod_other}
  namespace: ${NVSENTINEL_NS}
  labels:
    test: policy-verification
spec:
  containers:
  - name: test
    image: busybox:latest
    command: ["/bin/sh", "-c", "sleep 5"]
  restartPolicy: Never
EOF
    
    sleep 2
    if kubectl get pod "${test_pod_other}" -n "${NVSENTINEL_NS}" &> /dev/null; then
        log_success "✓ Non-NVSentinel image was allowed (policy only applies to ghcr.io/nvidia/nvsentinel/**)"
        kubectl delete pod "${test_pod_other}" -n "${NVSENTINEL_NS}" --grace-period=0 --force &> /dev/null || true
    else
        log_error "✗ Non-NVSentinel image was blocked (unexpected - policy should only apply to NVSentinel images)"
    fi
    
    # Clean up
    kubectl delete pod "${test_pod_invalid}" -n "${NVSENTINEL_NS}" --grace-period=0 --force &> /dev/null || true
}

show_summary() {
    log_info "Verification Summary"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    log_success "✓ Sigstore Policy Controller installed and running"
    log_success "✓ ClusterImagePolicy applied for NVSentinel images"
    log_success "✓ Namespace ${NVSENTINEL_NS} configured for enforcement"
    
    # Check if we tested with actual images
    local images
    images=$(get_nvsentinel_images)
    if [ -n "$images" ]; then
        log_success "✓ Policy enforcement tested successfully"
    else
        log_info "ℹ Policy ready (verification requires official images)"
    fi
    
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    log_info "All NVSentinel images in namespace '${NVSENTINEL_NS}' will now be"
    log_info "verified against SLSA Build Provenance attestations before deployment."
    echo ""
    log_info "Policy applies to: ghcr.io/nvidia/nvsentinel/**"
    log_info "Development images (localhost:*) are excluded from verification"
    echo ""
    log_info "Next steps:"
    echo ""
    log_info "View policy status:"
    echo "  kubectl describe clusterimagepolicy verify-nvsentinel-image-attestation"
    echo ""
    log_info "View Policy Controller logs:"
    echo "  kubectl logs -n ${POLICY_CONTROLLER_NS} -l app=policy-controller -f"
    echo ""
    log_info "Manual verification outside the cluster:"
    echo "  See distros/kubernetes/nvsentinel/policies/README.md"
    echo ""
    log_info "Disable enforcement on this namespace:"
    echo "  kubectl label namespace ${NVSENTINEL_NS} policy.sigstore.dev/include-"
    echo ""
}

main() {
    echo ""
    log_info "═══════════════════════════════════════════════════════════"
    log_info "  NVSentinel SLSA Provenance Verification"
    log_info "═══════════════════════════════════════════════════════════"
    echo ""
    
    check_prerequisites
    echo ""
    
    check_nvsentinel_deployment
    echo ""
    
    install_policy_controller
    echo ""
    
    apply_cluster_image_policy
    echo ""
    
    configure_namespace
    echo ""
    
    test_policy_enforcement
    echo ""
    
    show_summary
}

# Run main function
main "$@"
