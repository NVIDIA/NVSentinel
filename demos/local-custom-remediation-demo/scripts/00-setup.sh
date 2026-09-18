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

# Step 0: build the demo cluster, install NVSentinel with a custom remediation
# action, and stand up the monitor that reports the fault and the controller
# that repairs it.

# How long the controllers must stay up before setup calls itself successful.
SETTLE_SECONDS=${SETTLE_SECONDS:-30}

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

    log "NVSentinel needs cert-manager for MongoDB mTLS."
    log "Installing the latest release."

    helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
    helm repo update jetstack >/dev/null 2>&1 || true

    # Quiet: installing the CRDs makes client-go log hundreds of "unrecognized
    # format" lines for OpenAPI int32/int64 fields. On failure the pods are
    # printed instead, which says more than the log would have.
    if ! helm upgrade --install cert-manager jetstack/cert-manager \
        --namespace cert-manager \
        --create-namespace \
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
    log "Modules:   platform-connectors, fault-quarantine, node-drainer, fault-remediation"
    log "Action:    RECLAIM_MEMORY, a custom action mapped to a MemoryReclaim CR"
    log "Datastore: Percona Server for MongoDB, single member"
    echo
    log "A few minutes: the operator issues certificates and initialises the replica set before MongoDB accepts writes."

    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

    # The chart pins its own matching image tag, so nothing overrides it here:
    # setting one and not the other is how a chart ends up rendering flags the
    # image does not have.
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

install_memoryreclaim_crd() {
    section "Installing the MemoryReclaim CRD"

    log "fault-remediation creates these from the RECLAIM_MEMORY template; the demo controller reconciles them."

    apply_manifest memoryreclaim-crd.yaml
    apply_manifest fault-remediation-rbac.yaml

    success "MemoryReclaim CRD installed and fault-remediation granted access to it"
}

build_images() {
    section "Building the demo components"

    log "A health monitor that reports memory pressure, and a controller that acts on it."

    local component
    for component in memory-pressure-monitor memory-reclaim-controller; do
        log "Building ${component}..."
        docker build -q -t "${component}:demo" \
            -f "$DEMO_DIR/${component}/Dockerfile" "$REPO_ROOT" >/dev/null ||
            fail "Could not build ${component}."
        kind load docker-image "${component}:demo" --name "$CLUSTER_NAME" >/dev/null 2>&1
    done

    success "Both images are loaded into the cluster"
}

deploy_monitor() {
    section "Deploying the memory pressure monitor"

    local avail_mb threshold
    avail_mb="$(node_available_mb)"
    threshold=$((avail_mb - MEM_MARGIN_MB))

    log "MemAvailable on ${NODE} is ${avail_mb} MB, so the threshold starts at ${threshold} MB."
    log "KIND shares the host's memory, so this drifts. Step 2 recalibrates it before it triggers."

    apply_manifest memory-pressure-monitor.yaml "MEM_THRESHOLD_MB=${threshold}"

    wait_for_pod_ready "$NAMESPACE" app=memory-pressure-monitor 180 ||
        fail "The memory pressure monitor did not become ready."

    success "Monitor is polling /proc/meminfo on ${NODE}"
}

deploy_controller() {
    section "Deploying the memory reclaim controller"

    log "This is the third-party half: it watches MemoryReclaim CRs and deletes the offending pods."

    apply_manifest memory-reclaim-controller.yaml "WORKLOAD_NAMESPACE=${WORKLOAD_NAMESPACE}"

    if ! kubectl wait --for=condition=available --timeout=180s \
        deployment/memory-reclaim-controller -n "$NAMESPACE" >/dev/null 2>&1; then
        kubectl get pods -n "$NAMESPACE" -l app=memory-reclaim-controller
        fail "The memory reclaim controller did not become available."
    fi

    # It runs on the control plane on purpose: it is the thing that repairs the
    # worker, so it must not be evicted by the drain it is reacting to.
    success "Controller is watching MemoryReclaim CRs from the control plane"
}

# A pod that reports Ready once can still be crash-looping: fault-quarantine,
# for example, starts cleanly and only fails a few seconds later when it first
# evaluates the circuit breaker. Re-check after a settle window and treat a
# restart as a failed setup rather than reporting success over the top of it.
verify_sustained_readiness() {
    section "Confirming the controllers stay up"

    log "Waiting ${SETTLE_SECONDS}s and re-checking, so a crash loop is caught here rather than two steps later."

    local before after unready
    before="$(restart_counts)"
    sleep "$SETTLE_SECONDS"
    after="$(restart_counts)"

    # Completed Job pods (the MongoDB bootstrap job) sit at Ready=False forever,
    # so match on phase too and only treat a still-running pod as unready. A
    # Failed pod is not excused - that is a genuine problem worth stopping for.
    unready="$(kubectl get pods -n "$NAMESPACE" -o json |
        jq -r '.items[]
               | select(.status.phase != "Succeeded")
               | select([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length == 0)
               | .metadata.name')"

    if [[ -n "$unready" ]]; then
        kubectl get pods -n "$NAMESPACE"
        fail "These pods are not Ready: $(echo "$unready" | tr '\n' ' ')"
    fi

    local problems
    problems="$(restart_problems "$before" "$after")"

    if [[ -n "$problems" ]]; then
        kubectl get pods -n "$NAMESPACE"
        fail "Something restarted during the ${SETTLE_SECONDS}s settle window:
${problems}"
    fi

    success "Every pod is Ready and has stayed up for ${SETTLE_SECONDS}s"
}

# Snapshot of every pod's identity and its containers' restart counts.
#
# Keyed by UID rather than by pod name: a replacement pod can reuse the name, and
# comparing by name alone would read a brand-new container as the old one having
# restarted.
restart_counts() {
    kubectl get pods -n "$NAMESPACE" -o json |
        jq -S '[.items[] | {
                   uid: .metadata.uid,
                   name: .metadata.name,
                   containers: [(.status.containerStatuses // [])[] | {name, restartCount}]
               }]'
}

# Prints one line per problem found between two snapshots, and nothing when the
# cluster merely churned.
#
# Only two things count: a container that restarted while the demo watched, and a
# pod replaced under the same name. Pods appearing or disappearing do not - the
# MongoDB bootstrap Job and the Percona operator both retire pods while the
# cluster settles, and comparing whole snapshots would fail a healthy setup for
# that alone.
restart_problems() {
    jq -rn --argjson before "$1" --argjson after "$2" '
        ($before | INDEX(.uid)) as $b |
        [
          ( $after[] | . as $pod
            | select($b[$pod.uid] != null)
            | $pod.containers[] | . as $c
            | ($b[$pod.uid].containers | map(select(.name == $c.name)) | first) as $was
            | select($was != null and $c.restartCount > $was.restartCount)
            | "\($pod.name)/\($c.name) restarted \($c.restartCount - $was.restartCount) time(s)" ),
          ( $after[] | . as $pod
            | select($b[$pod.uid] == null)
            | select([$before[] | select(.name == $pod.name)] | length > 0)
            | "\($pod.name) was replaced" )
        ] | .[]
    '
}

print_summary() {
    success "Cluster '${CLUSTER_NAME}' is running NVSentinel ${CHART_VERSION} with a custom RECLAIM_MEMORY action"

    next_step "./scripts/01-show-cluster.sh   look at it before anything is broken"
}

main() {
    require_tools docker kind kubectl helm go jq curl column

    section "Preparing the cluster"
    log "Resolving the NVSentinel version to install..."
    CHART_VERSION="$(resolve_chart_version)"
    success "Installing NVSentinel ${CHART_VERSION}"

    create_cluster
    NODE="$(worker_node)"

    install_cert_manager
    install_nvsentinel
    install_memoryreclaim_crd
    build_images
    deploy_monitor
    deploy_controller
    verify_sustained_readiness
    print_summary
}

main "$@"
