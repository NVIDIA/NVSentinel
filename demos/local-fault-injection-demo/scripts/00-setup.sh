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

# Step 0: build the demo cluster and install NVSentinel on it.

# How long to wait on the two multi-gigabyte image pulls (DCGM and
# gpu-health-monitor, both carrying the NVIDIA stack). A fresh KIND cluster has
# an empty containerd, so both are pulled from scratch every run.
PULL_TIMEOUT=${PULL_TIMEOUT:-900}

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

create_cluster() {
    section "Creating the KIND cluster"

    if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
        warn "Cluster '$CLUSTER_NAME' already exists; deleting it so this run starts clean."
        kind delete cluster --name "$CLUSTER_NAME"
    fi

    kind create cluster --name "$CLUSTER_NAME" --config "$CONFIG_DIR/kind-cluster.yaml"
    kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null
    kubectl wait --for=condition=ready nodes --all --timeout=180s >/dev/null

    success "Cluster '$CLUSTER_NAME' is up"
    kubectl get nodes
}

install_cert_manager() {
    section "Installing cert-manager"

       log "NVSentinel needs cert-manager for MongoDB mTLS and the janitor webhook."

    helm repo add jetstack https://charts.jetstack.io --force-update >/dev/null

    local version_args=()
    if [[ -n "${CERT_MANAGER_VERSION:-}" ]]; then
        version_args=(--version "$CERT_MANAGER_VERSION")
        log "Version ${CERT_MANAGER_VERSION}, pinned via CERT_MANAGER_VERSION."
    else
        log "Installing the latest release."
    fi

    # Quiet: installing the CRDs makes client-go log hundreds of "unrecognized
    # format" lines for OpenAPI int32/int64 fields. On failure the pods are
    # printed instead, which says more than the log would have.
    if ! helm upgrade --install cert-manager jetstack/cert-manager \
        --namespace cert-manager \
        --create-namespace \
        "${version_args[@]}" \
        --values "$CONFIG_DIR/cert-manager-values.yaml" \
        --wait --timeout 5m >/dev/null 2>&1; then
        kubectl get pods -n cert-manager
        fail "cert-manager did not install. Its pods are above."
    fi

    success "cert-manager is ready"
}

install_nvsentinel() {
    section "Installing NVSentinel ${CHART_VERSION}"

    log "Chart:     oci://ghcr.io/nvidia/nvsentinel ${CHART_VERSION}"
    log "Values:    config/nvsentinel-values.yaml"
    log "Modules:   gpu-health-monitor, platform-connectors, fault-quarantine, node-drainer, fault-remediation, janitor, janitor-provider"
    log "Datastore: Percona Server for MongoDB, single member"
    echo
    log "A few minutes: the operator issues certificates and initialises the replica set before MongoDB accepts writes."

    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

    if ! helm upgrade --install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
        --version "$CHART_VERSION" \
        --namespace "$NAMESPACE" \
        --values "$CONFIG_DIR/nvsentinel-values.yaml" \
        --wait --timeout 15m >/dev/null 2>&1; then
        kubectl get pods -n "$NAMESPACE"
        fail "The NVSentinel install did not complete. The pods above show how far it got."
    fi

    success "NVSentinel installed"
}

deploy_fake_dcgm() {
    section "Deploying the fake DCGM hostengine"

    log "No GPU here, so DCGM is backed by NVML injection: same protocol, same port, same code path in the monitor."

    kubectl create namespace "$DCGM_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

    # Generated from the repository's single source of truth for the injected
    # GPU, the same file the Tilt development environment mounts.
    kubectl create configmap nvidia-dcgm-gpu-spec \
        --namespace "$DCGM_NAMESPACE" \
        --from-file=gpu-spec.yaml="$REPO_ROOT/tilt/dcgm-fake/gpu-spec.yaml" \
        --dry-run=client -o yaml | kubectl apply -f - >/dev/null

    kubectl apply -f "$CONFIG_DIR/fake-dcgm.yaml" >/dev/null

    log "The image carries the NVIDIA stack and runs to a few GB, so the first pull is slow."

    if ! wait_for_pod_ready "$DCGM_NAMESPACE" app=nvidia-dcgm "$PULL_TIMEOUT"; then
        kubectl get pods -n "$DCGM_NAMESPACE"
        fail "The fake DCGM pod did not become ready within ${PULL_TIMEOUT}s. If it is still pulling, raise PULL_TIMEOUT and re-run."
    fi

    success "Fake DCGM is listening on port 5555"
}

label_gpu_nodes() {
    section "Labelling the worker as a GPU node"

    # In a real cluster the GPU Operator and NVSentinel's own labeler write
    # these. Here they are asserted by hand, which is why the labeler is turned
    # off in config/nvsentinel-values.yaml — it would strip them back off.
    log "Applying what a GPU Operator install leaves behind: gpu.present, driver.installed, dcgm.version."

    local node
    for node in $(kubectl get nodes -o name \
        -l '!node-role.kubernetes.io/control-plane'); do
        kubectl label "$node" \
            nvidia.com/gpu.present=true \
            nvsentinel.dgxc.nvidia.com/driver.installed=true \
            nvsentinel.dgxc.nvidia.com/kata.enabled=false \
            nvsentinel.dgxc.nvidia.com/dcgm.version=4.x \
            --overwrite >/dev/null
        log "Labelled ${node#node/}"
    done

    log "gpu-health-monitor selects on the dcgm.version label, so its pod starts now."

    if ! kubectl rollout status daemonset/gpu-health-monitor-dcgm-4.x \
        -n "$NAMESPACE" --timeout="${PULL_TIMEOUT}s" >/dev/null 2>&1; then
        kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=gpu-health-monitor
        fail "gpu-health-monitor did not become ready within ${PULL_TIMEOUT}s. If it is still pulling, raise PULL_TIMEOUT and re-run."
    fi

    success "gpu-health-monitor is running on the GPU node"
}

await_dcgm_connectivity() {
    section "Confirming the monitor can read the GPU"

    # A running gpu-health-monitor pod proves nothing about whether it can reach
    # the hostengine. The proof is GpuDcgmConnectivityFailure: platform-connectors
    # only writes that condition once the monitor has completed a poll, and it
    # reads False while the connection is healthy. Gating setup on it means a
    # fault injected in the next step is seen rather than silently dropped.
    log "A running pod proves nothing. GpuDcgmConnectivityFailure=False appears only after a successful poll."

    wait_until 300 "gpu-health-monitor to report a healthy DCGM connection" \
        dcgm_connected "$NODE" ||
        fail "gpu-health-monitor never reported a healthy DCGM connection.
Condition: $(node_condition_status "$NODE" GpuDcgmConnectivityFailure | grep . || echo absent)
Check the monitor:  kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=gpu-health-monitor --tail=50"

    echo
    kubectl get node "$NODE" -o json |
        jq -r '.status.conditions[]
               | select(.type == "GpuDcgmConnectivityFailure")
               | "  \(.type)\t\(.status)\t\(.reason)\t\(.message)"' | column -t -s $'\t'
    echo
    success "gpu-health-monitor is reading the GPU through DCGM"
}

# True once gpu-health-monitor has proven it can talk to DCGM.
dcgm_connected() {
    [[ "$(node_condition_status "$1" GpuDcgmConnectivityFailure)" == "False" ]]
}

deploy_workload() {
    section "Deploying the demo workload"

    log "Standing in for a training job, so the drain has something to evict."

    kubectl apply -f "$CONFIG_DIR/demo-workload.yaml" >/dev/null

    if ! kubectl rollout status "deployment/${WORKLOAD_NAME}" \
        -n "$WORKLOAD_NAMESPACE" --timeout=180s >/dev/null 2>&1; then
        kubectl get pods -n "$WORKLOAD_NAMESPACE" -l "app=${WORKLOAD_NAME}"
        fail "The demo workload did not start."
    fi

    success "Workload is running on ${NODE}"
}

print_summary() {
    success "Cluster '${CLUSTER_NAME}' is running NVSentinel ${CHART_VERSION}, with ${NODE} standing in for a GPU node"

    next_step "./scripts/01-show-cluster.sh   look at it before anything is broken"
}

main() {
    require_tools docker kind kubectl helm jq curl

    section "Preparing the cluster"
    log "Resolving the NVSentinel version to install..."
    CHART_VERSION="$(resolve_chart_version)"
    success "Installing NVSentinel ${CHART_VERSION}"

    create_cluster
    install_cert_manager
    install_nvsentinel
    deploy_fake_dcgm
    label_gpu_nodes

    NODE="$(gpu_node)"
    await_dcgm_connectivity

    deploy_workload
    print_summary
}

main "$@"
