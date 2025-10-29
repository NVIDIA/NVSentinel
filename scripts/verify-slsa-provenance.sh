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
# cluster to verify SLSA Build Provenance attestations for NVSentinel container
# images.
#
# CURRENT STATUS: Policy runs in WARN mode due to bundle format v0.3 incompatibility
# - Attestations are created by GitHub Actions in Sigstore bundle format v0.3
# - Policy Controller 0.10.5 cannot read bundle format v0.3 yet (only v0.1/v0.2)
#   Issue: https://github.com/sigstore/policy-controller/issues/1895
# - All images are allowed to deploy, but validation warnings are logged
# - Policy will be switched to enforce mode when v0.3 support is added
#
# The script:
# - Installs Sigstore Policy Controller via Helm
# - Applies ClusterImagePolicy for NVSentinel images (in warn mode)
# - Configures namespace for policy enforcement
# - Tests policy configuration with actual deployments
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
    log_info "Testing policy configuration..."
    
    # Clean up any existing test pods from previous runs
    kubectl delete pod -n "${NVSENTINEL_NS}" -l test=policy-verification --grace-period=0 --force &> /dev/null || true
    
    # Get official images from cluster
    local test_image
    test_image=$(get_nvsentinel_images | head -n 1)
    
    if [ -z "$test_image" ]; then
        log_warn "No official NVSentinel images found in cluster for testing"
        log_info "Policy validation can only be tested with official ghcr.io/nvidia/nvsentinel/** images"
        log_info "Development images (localhost:*) are not subject to policy verification"
        return 0
    fi
    
    # Create a test pod with a valid NVSentinel image
    local test_pod_name="policy-test-valid-$$"
    
    log_info "Creating test pod with image: ${test_image}"
    log_warn "Note: Policy is in WARN mode - all images are allowed but validation warnings are logged"
    
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
    
    # In warn mode, images should always be allowed
    if [ $apply_exit_code -ne 0 ]; then
        log_error "✗ Failed to create test pod"
        log_error "Error: $apply_output"
        return 1
    fi
    
    # Check if pod was created
    sleep 2
    if kubectl get pod "${test_pod_name}" -n "${NVSENTINEL_NS}" &> /dev/null; then
        log_success "✓ Image deployment succeeded (warn mode)"
        
        # Check for policy warnings in pod events
        log_info "Checking for policy validation warnings..."
        local events
        events=$(kubectl get events -n "${NVSENTINEL_NS}" --field-selector involvedObject.name="${test_pod_name}" -o json 2>/dev/null | \
            jq -r '.items[] | select(.reason | contains("Policy") or contains("policy")) | .message' 2>/dev/null || echo "")
        
        if [ -n "$events" ]; then
            log_info "Policy warnings detected:"
            echo "$events" | sed 's/^/  /'
        else
            log_info "No policy warnings found in pod events (check Policy Controller logs for details)"
        fi
        
        kubectl delete pod "${test_pod_name}" -n "${NVSENTINEL_NS}" --grace-period=0 --force &> /dev/null || true
    else
        log_error "✗ Test pod was not created (unexpected)"
    fi
    
    # Test that non-NVSentinel images are also allowed (and not subject to policy)
    local test_pod_other="policy-test-other-$$"
    log_info "Testing with non-NVSentinel image (should be allowed and not validated)..."
    
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
        log_error "✗ Non-NVSentinel image was not created (unexpected)"
    fi
    
    # Clean up
    kubectl delete pod -n "${NVSENTINEL_NS}" -l test=policy-verification --grace-period=0 --force &> /dev/null || true
    
    log_info ""
    log_info "To view policy validation warnings, check Policy Controller logs:"
    log_info "  kubectl logs -n ${POLICY_CONTROLLER_NS} -l app.kubernetes.io/name=policy-controller --tail=50"
}

show_summary() {
    log_info "Verification Summary"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    log_success "✓ Sigstore Policy Controller installed and running"
    log_success "✓ ClusterImagePolicy applied for NVSentinel images"
    log_success "✓ Namespace ${NVSENTINEL_NS} configured for policy enforcement"
    
    # Check if we tested with actual images
    local images
    images=$(get_nvsentinel_images)
    if [ -n "$images" ]; then
        log_success "✓ Policy configuration tested successfully"
    else
        log_info "ℹ Policy ready (verification requires official images)"
    fi
    
    echo ""
    log_warn "⚠  Policy is in WARN mode (bundle format v0.3 incompatibility)"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    log_info "The policy is currently running in WARN mode. All NVSentinel images"
    log_info "will be allowed to deploy, but validation warnings will be logged."
    echo ""
    log_info "Why warn mode?"
    log_info "• Attestations are created by GitHub Actions in Sigstore bundle format v0.3"
    log_info "• Policy Controller 0.10.5 cannot read bundle format v0.3 yet"
    log_info "• Attestations exist and are valid (verified manually with cosign)"
    log_info "• Policy will be switched to enforce mode when v0.3 support is added"
    echo ""
    log_info "Policy scope: ghcr.io/nvidia/nvsentinel/**"
    log_info "Development images (localhost:*) are excluded from verification"
    echo ""
    log_info "Next steps:"
    echo ""
    log_info "View policy configuration:"
    echo "  kubectl describe clusterimagepolicy verify-nvsentinel-image-attestation"
    echo ""
    log_info "View Policy Controller logs for validation warnings:"
    echo "  kubectl logs -n ${POLICY_CONTROLLER_NS} -l app.kubernetes.io/name=policy-controller -f"
    echo ""
    log_info "Manual verification outside the cluster:"
    echo "  See distros/kubernetes/nvsentinel/policies/README.md"
    echo ""
    log_info "Track Policy Controller v0.3 support:"
    echo "  https://github.com/sigstore/policy-controller/issues"
    echo ""
    log_info "Disable policy enforcement on this namespace:"
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
