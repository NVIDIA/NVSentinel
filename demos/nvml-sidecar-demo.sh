#!/bin/bash
# NVML Provider Sidecar Demo
# Demonstrates the NVML provider sidecar architecture for GPU enumeration
#
# Prerequisites:
#   - kubectl configured with GPU cluster access
#   - docker or podman for building images
#   - helm 3.x installed
#   - GPU nodes with RuntimeClass 'nvidia'
#
# Usage: ./demos/nvml-sidecar-demo.sh [kubeconfig]

set -euo pipefail

# ==============================================================================
# Configuration
# ==============================================================================

KUBECONFIG="${1:-$HOME/.kube/config-aws-gpu}"
NAMESPACE="nvsentinel"
RELEASE_NAME="nvsentinel"
CHART_PATH="deployments/helm/nvsentinel"
VALUES_FILE="deployments/helm/values-sidecar-test.yaml"
DOCKERFILE="deployments/container/Dockerfile"

# Image settings (using ttl.sh ephemeral registry - images expire after 2h)
SERVER_IMAGE="ttl.sh/device-api-server-sidecar:2h"
SIDECAR_IMAGE="ttl.sh/nvml-provider-sidecar:2h"

# ==============================================================================
# Terminal Colors (buildah-style)
# ==============================================================================

if [[ -t 1 ]]; then
    red=$(tput setaf 1)
    green=$(tput setaf 2)
    yellow=$(tput setaf 3)
    blue=$(tput setaf 4)
    magenta=$(tput setaf 5)
    cyan=$(tput setaf 6)
    white=$(tput setaf 7)
    bold=$(tput bold)
    reset=$(tput sgr0)
else
    red=""
    green=""
    yellow=""
    blue=""
    magenta=""
    cyan=""
    white=""
    bold=""
    reset=""
fi

# ==============================================================================
# Helper Functions
# ==============================================================================

banner() {
    echo ""
    echo "${bold}${blue}============================================================${reset}"
    echo "${bold}${blue}  $1${reset}"
    echo "${bold}${blue}============================================================${reset}"
    echo ""
}

step() {
    echo ""
    echo "${bold}${green}>>> $1${reset}"
    echo ""
}

info() {
    echo "${cyan}    $1${reset}"
}

warn() {
    echo "${yellow}    WARNING: $1${reset}"
}

error() {
    echo "${red}    ERROR: $1${reset}"
}

run_cmd() {
    echo "${magenta}    \$ $*${reset}"
    "$@"
}

pause() {
    echo ""
    read -r -p "${yellow}Press ENTER to continue...${reset}"
    echo ""
}

confirm() {
    echo ""
    read -r -p "${yellow}$1 [y/N] ${reset}" response
    case "$response" in
        [yY][eE][sS]|[yY]) return 0 ;;
        *) return 1 ;;
    esac
}

check_prereqs() {
    local missing=()

    command -v kubectl &>/dev/null || missing+=("kubectl")
    command -v helm &>/dev/null || missing+=("helm")
    command -v docker &>/dev/null || missing+=("docker")

    if [[ ${#missing[@]} -gt 0 ]]; then
        error "Missing prerequisites: ${missing[*]}"
        exit 1
    fi

    # Check for buildx (required for cross-platform builds)
    if ! docker buildx version &>/dev/null; then
        warn "docker buildx not available - cross-platform builds may fail"
        warn "Run: docker buildx create --use --name multiarch"
    else
        info "Docker buildx: $(docker buildx version | head -1)"
    fi
}

# ==============================================================================
# Demo Sections
# ==============================================================================

show_intro() {
    clear
    banner "NVML Provider Sidecar Architecture Demo"

    echo "${white}This demo showcases the new sidecar-based NVML provider for NVSentinel.${reset}"
    echo ""
    echo "${white}Architecture:${reset}"
    echo "${cyan}  ┌─────────────────────────────────────────────────────────┐${reset}"
    echo "${cyan}  │                     Pod                                 │${reset}"
    echo "${cyan}  │  ┌──────────────────┐    ┌──────────────────┐          │${reset}"
    echo "${cyan}  │  │ device-api-server│    │  nvml-provider   │          │${reset}"
    echo "${cyan}  │  │   (pure Go)      │◄───│  (CGO + NVML)    │          │${reset}"
    echo "${cyan}  │  │   Port 8080      │gRPC│  Port 9001       │          │${reset}"
    echo "${cyan}  │  │                  │    │  RuntimeClass:   │          │${reset}"
    echo "${cyan}  │  │  No NVML deps    │    │    nvidia        │          │${reset}"
    echo "${cyan}  │  └──────────────────┘    └──────────────────┘          │${reset}"
    echo "${cyan}  └─────────────────────────────────────────────────────────┘${reset}"
    echo ""
    echo "${white}Benefits:${reset}"
    echo "${green}  ✓ Separation of concerns (API server vs NVML access)${reset}"
    echo "${green}  ✓ Independent scaling and updates${reset}"
    echo "${green}  ✓ Better testability (mock providers)${reset}"
    echo "${green}  ✓ Crash isolation (NVML crashes don't kill API server)${reset}"
    echo ""

    pause
}

show_cluster_info() {
    banner "Step 1: Verify Cluster Connectivity"

    info "Using kubeconfig: ${KUBECONFIG}"
    echo ""

    step "Check cluster connection"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" cluster-info

    pause

    step "List GPU nodes (with node-type=gpu label)"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" get nodes -l node-type=gpu -o wide

    pause

    step "Verify nvidia RuntimeClass exists"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" get runtimeclass nvidia -o yaml || {
        warn "RuntimeClass 'nvidia' not found. GPU access may not work."
    }

    pause
}

check_image_exists() {
    local image="$1"
    # Try to inspect the manifest - if it exists, the image is available
    docker buildx imagetools inspect "${image}" &>/dev/null 2>&1
}

build_images() {
    banner "Step 2: Build Container Images"

    info "Building images for ephemeral registry (ttl.sh - 2 hour expiry)"
    info "Using unified multi-target Dockerfile at ${DOCKERFILE}"
    info "Target platform: linux/amd64 (cross-compile for x86 clusters)"
    echo ""

    # Ensure buildx is available for cross-platform builds
    if ! docker buildx version &>/dev/null; then
        error "docker buildx is required for cross-platform builds"
        error "Install Docker Desktop or run: docker buildx create --use"
        exit 1
    fi

    # Check if images already exist
    local need_server=true
    local need_sidecar=true

    if check_image_exists "${SERVER_IMAGE}"; then
        info "Image ${SERVER_IMAGE} already exists"
        if ! confirm "Rebuild device-api-server image?"; then
            need_server=false
        fi
    fi

    if check_image_exists "${SIDECAR_IMAGE}"; then
        info "Image ${SIDECAR_IMAGE} already exists"
        if ! confirm "Rebuild nvml-provider image?"; then
            need_sidecar=false
        fi
    fi

    if [[ "${need_server}" == "true" ]]; then
        step "Build and push device-api-server image (CGO_ENABLED=0)"
        info "This is a pure Go binary with no NVML dependencies"
        info "Building for linux/amd64 and pushing directly..."
        run_cmd docker buildx build \
            --platform linux/amd64 \
            --target device-api-server \
            -t "${SERVER_IMAGE}" \
            -f "${DOCKERFILE}" \
            --push \
            .
        pause
    else
        info "Skipping device-api-server build"
    fi

    if [[ "${need_sidecar}" == "true" ]]; then
        step "Build and push nvml-provider sidecar image (CGO_ENABLED=1)"
        info "This requires glibc runtime for NVML library binding"
        info "Building for linux/amd64 and pushing directly..."
        run_cmd docker buildx build \
            --platform linux/amd64 \
            --target nvml-provider \
            -t "${SIDECAR_IMAGE}" \
            -f "${DOCKERFILE}" \
            --push \
            .
        pause
    else
        info "Skipping nvml-provider build"
    fi
}

show_values_file() {
    banner "Step 3: Review Helm Values"

    info "The sidecar architecture is enabled via Helm values"
    echo ""

    step "Key configuration in ${VALUES_FILE}:"
    echo ""
    echo "${cyan}# Disable built-in NVML provider${reset}"
    echo "${white}nvml:${reset}"
    echo "${white}  enabled: false${reset}"
    echo ""
    echo "${cyan}# Enable NVML Provider sidecar${reset}"
    echo "${white}nvmlProvider:${reset}"
    echo "${white}  enabled: true${reset}"
    echo "${white}  image:${reset}"
    echo "${white}    repository: ttl.sh/nvml-provider-sidecar${reset}"
    echo "${white}    tag: \"2h\"${reset}"
    echo "${white}  serverAddress: \"localhost:9001\"${reset}"
    echo "${white}  runtimeClassName: nvidia${reset}"
    echo ""

    if [[ -f "${VALUES_FILE}" ]]; then
        step "Full values file:"
        run_cmd cat "${VALUES_FILE}"
    fi

    pause
}

deploy_sidecar() {
    banner "Step 4: Deploy with Sidecar Architecture"

    step "Create namespace if not exists"
    echo "${magenta}    \$ kubectl create namespace ${NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -${reset}"
    kubectl --kubeconfig="${KUBECONFIG}" create namespace "${NAMESPACE}" --dry-run=client -o yaml | \
        kubectl --kubeconfig="${KUBECONFIG}" apply -f -

    pause

    # Check if release already exists
    if helm status "${RELEASE_NAME}" --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" &>/dev/null; then
        info "Release '${RELEASE_NAME}' already exists"
        step "Upgrading existing release..."
        run_cmd helm upgrade "${RELEASE_NAME}" "${CHART_PATH}" \
            --kubeconfig="${KUBECONFIG}" \
            --namespace "${NAMESPACE}" \
            -f "${VALUES_FILE}"

        step "Restarting pods to pick up changes..."
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" rollout restart daemonset "${RELEASE_NAME}"
    else
        step "Installing new release..."
        run_cmd helm install "${RELEASE_NAME}" "${CHART_PATH}" \
            --kubeconfig="${KUBECONFIG}" \
            --namespace "${NAMESPACE}" \
            -f "${VALUES_FILE}"
    fi

    pause

    step "Waiting for pods to be ready (timeout 2m)..."
    if ! kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" rollout status daemonset "${RELEASE_NAME}" --timeout=2m; then
        warn "Rollout not complete within timeout. Checking status..."
    fi

    step "Current pod status"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=nvsentinel -o wide

    pause

    step "Verify both containers are running in each pod"
    info "Each pod should have 2/2 containers ready"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=nvsentinel \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.containerStatuses[*]}{.name}:{.ready}{" "}{end}{"\n"}{end}'

    pause
}

verify_gpu_registration() {
    banner "Step 5: Verify GPU Registration"

    step "Wait for pods to be ready"
    info "Waiting up to 60 seconds for pods to start..."
    if ! kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" wait --for=condition=ready pod -l app.kubernetes.io/name=nvsentinel --timeout=60s 2>/dev/null; then
        warn "Pods may not be ready yet. Checking status..."
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=nvsentinel -o wide
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" describe pods -l app.kubernetes.io/name=nvsentinel | tail -30
        error "Pods not ready. Check the output above for issues."
        return 1
    fi

    step "Get a pod name for testing"
    POD=$(kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=nvsentinel -o jsonpath='{.items[0].metadata.name}')
    if [[ -z "${POD}" ]]; then
        error "No pods found. DaemonSet may not be scheduling on any nodes."
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get daemonset
        return 1
    fi
    info "Using pod: ${POD}"

    pause

    step "Check device-api-server logs for provider connection"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" logs "${POD}" -c nvsentinel --tail=20 || true

    pause

    step "Check nvml-provider sidecar logs"
    run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" logs "${POD}" -c nvml-provider --tail=20 || true

    pause

    pause
}

demonstrate_crash_recovery() {
    banner "Step 6: Demonstrate Crash Recovery"

    info "The sidecar architecture provides crash isolation."
    info "If the NVML provider crashes, the API server continues running"
    info "and will reconnect when the provider restarts."
    echo ""

    step "Get current pod"
    POD=$(kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=nvsentinel -o jsonpath='{.items[0].metadata.name}')
    info "Using pod: ${POD}"

    pause

    if confirm "Kill the nvml-provider container to demonstrate crash recovery?"; then
        step "Killing nvml-provider container..."
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" exec "${POD}" -c nvml-provider -- kill 1 || true

        info "Waiting for container restart..."
        sleep 5

        step "Check pod status (should show restart count)"
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pod "${POD}" -o wide

        step "Verify API server continued running"
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" logs "${POD}" -c nvsentinel --tail=10 || true

        step "Verify provider reconnected"
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" logs "${POD}" -c nvml-provider --tail=10 || true
    else
        info "Skipping crash recovery demonstration"
    fi

    pause
}

show_metrics() {
    banner "Step 7: View Provider Metrics"

    step "Get pod for port-forward"
    POD=$(kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=nvsentinel -o jsonpath='{.items[0].metadata.name}')

    step "Fetch metrics from the API server"
    info "Key metrics to look for:"
    info "  - device_api_provider_connected: Whether provider is connected"
    info "  - device_api_gpus_total: Number of GPUs registered"
    info "  - device_api_provider_heartbeat_*: Heartbeat latency"
    echo ""

    run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" exec "${POD}" -c nvsentinel -- \
        wget -qO- http://localhost:8081/metrics 2>/dev/null | grep -E "^device_api_" | sort || {
        run_cmd kubectl --kubeconfig="${KUBECONFIG}" -n "${NAMESPACE}" exec "${POD}" -c nvsentinel -- \
            curl -s http://localhost:8081/metrics 2>/dev/null | grep -E "^device_api_" | sort || true
    }

    pause
}

cleanup() {
    banner "Cleanup"

    if confirm "Remove the sidecar deployment and restore default?"; then
        step "Uninstalling Helm release..."
        run_cmd helm uninstall "${RELEASE_NAME}" \
            --kubeconfig="${KUBECONFIG}" \
            --namespace "${NAMESPACE}" || true

        info "Cleanup complete!"
    else
        info "Skipping cleanup. Release '${RELEASE_NAME}' left in namespace '${NAMESPACE}'"
    fi
}

show_summary() {
    banner "Demo Complete!"

    echo "${white}What we demonstrated:${reset}"
    echo "${green}  ✓ Built separate images for API server and NVML provider${reset}"
    echo "${green}  ✓ Deployed as sidecar architecture via Helm${reset}"
    echo "${green}  ✓ Verified GPU registration through the sidecar${reset}"
    echo "${green}  ✓ Showed crash isolation and recovery${reset}"
    echo "${green}  ✓ Explored provider metrics${reset}"
    echo ""
    echo "${white}Key files:${reset}"
    echo "${cyan}  - Dockerfile.nvml-provider  # Sidecar container build${reset}"
    echo "${cyan}  - values-sidecar-test.yaml  # Helm values for sidecar mode${reset}"
    echo "${cyan}  - charts/device-api-server/ # Helm chart with sidecar support${reset}"
    echo ""
    echo "${white}Learn more:${reset}"
    echo "${cyan}  - docs/design/nvml-containerization-decision.md${reset}"
    echo "${cyan}  - docs/design/nvml-container-architecture-exploration.md${reset}"
    echo ""
}

# ==============================================================================
# Main
# ==============================================================================

main() {
    export KUBECONFIG

    show_intro
    check_prereqs
    show_cluster_info

    if confirm "Build and push container images?"; then
        build_images
    else
        info "Skipping image build. Using existing images at ttl.sh"
    fi

    show_values_file

    if confirm "Deploy the sidecar architecture to the cluster?"; then
        deploy_sidecar
        verify_gpu_registration
        demonstrate_crash_recovery
        show_metrics
        cleanup
    else
        info "Skipping deployment"
    fi

    show_summary
}

# Run main if script is executed (not sourced)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
