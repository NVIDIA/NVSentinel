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

# Step 0: build the demo cluster, install NVSentinel with custom drain enabled,
# and stand up the two plugins that carry the drain out.

# How long to wait on the multi-gigabyte image pulls (DCGM and
# gpu-health-monitor, both carrying the NVIDIA stack). A fresh KIND cluster has
# an empty containerd, so both are pulled from scratch every run.
PULL_TIMEOUT=${PULL_TIMEOUT:-900}

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

install_drain_request_crd() {
    section "Installing the DrainRequest CRD"

    log "node-drainer creates these; slinky-drainer reconciles them. Both need the type to exist first."

    local crd_file="$REPO_ROOT/plugins/slinky-drainer/config/crd/nvsentinel.nvidia.com_drainrequests.yaml"

    if [[ ! -f "$crd_file" ]]; then
        log "CRD not generated yet; running 'make generate' in plugins/slinky-drainer."
        make -C "$REPO_ROOT/plugins/slinky-drainer" generate >/dev/null
    fi

    kubectl apply -f "$crd_file" >/dev/null

    success "DrainRequest CRD installed"
}

install_nvsentinel() {
    section "Installing NVSentinel ${CHART_VERSION}"

    log "Chart:     oci://ghcr.io/nvidia/nvsentinel ${CHART_VERSION}"
    log "Values:    config/nvsentinel-values.yaml"
    log "Modules:   gpu-health-monitor, platform-connectors, fault-quarantine, node-drainer"
    log "Drain:     delegated to slinky-drainer through a DrainRequest CR"
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

create_drain_template() {
    section "Configuring the custom drain template"

    log "node-drainer renders this Go template into a DrainRequest for every node it is asked to drain."

    # Ahead of the Helm install, not after it: node-drainer mounts this ConfigMap,
    # so its pod cannot start until the ConfigMap exists, and `helm --wait` would
    # sit there until it timed out.
    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

    apply_manifest drain-template.yaml

    success "Drain template ready"
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

    success "gpu-health-monitor is reading the GPU through DCGM"
}

# True once gpu-health-monitor has proven it can talk to DCGM.
dcgm_connected() {
    [[ "$(node_condition_status "$1" GpuDcgmConnectivityFailure)" == "False" ]]
}

build_mock_slurm() {
    section "Building the mock Slurm operator"

    # slinky-drainer ships with the release, so it is pulled rather than built.
    # mock-slurm-operator is demo-only and published nowhere, so it is the one
    # thing that has to be built from this checkout.
    log "This is the only image the demo builds: it stands in for a Slurm control plane, so it ships with no release."

    (
        cd "$REPO_ROOT/plugins/mock-slurm-operator"
        KO_DOCKER_REPO=ko.local ko build --bare --local . --tags demo >/dev/null 2>&1
    ) || fail "Could not build mock-slurm-operator."

    docker tag ko.local:demo mock-slurm-operator:demo >/dev/null
    kind load docker-image mock-slurm-operator:demo --name "$CLUSTER_NAME" >/dev/null 2>&1

    success "mock-slurm-operator:demo is loaded into the cluster"
}

deploy_plugins() {
    section "Deploying the drain plugins"

    log "slinky-drainer: ${SLINKY_DRAINER_IMAGE}"
    log "mock-slurm-operator: locally built mock-slurm-operator:demo"

    local plugin container image policy
    for plugin in slinky-drainer mock-slurm-operator; do
        # The two kustomizations name their container differently, and only the
        # locally built one has to be kept from being pulled.
        if [[ "$plugin" == "slinky-drainer" ]]; then
            container=controller
            image="$SLINKY_DRAINER_IMAGE"
            policy=IfNotPresent
        else
            container=manager
            image=mock-slurm-operator:demo
            policy=Never
        fi

        kubectl apply -k "$REPO_ROOT/plugins/$plugin/config/default" >/dev/null
        kubectl set image "deployment/$plugin" "${container}=${image}" -n "$NAMESPACE" >/dev/null
        kubectl patch deployment "$plugin" -n "$NAMESPACE" --type=json \
            -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/imagePullPolicy\", \"value\": \"${policy}\"}]" >/dev/null

        if ! kubectl wait --for=condition=available --timeout=300s \
            "deployment/$plugin" -n "$NAMESPACE" >/dev/null 2>&1; then
            kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=$plugin"
            fail "${plugin} did not become available."
        fi

        success "${plugin} is running"
    done
}

deploy_workload() {
    section "Deploying the demo workload"

    log "Standing in for Slurm-managed jobs, so the custom drain has something to evict."

    kubectl create namespace "$SLINKY_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    kubectl apply -f "$CONFIG_DIR/slinky-workload.yaml" >/dev/null

    if ! kubectl rollout status "deployment/${WORKLOAD_NAME}" \
        -n "$SLINKY_NAMESPACE" --timeout=180s >/dev/null 2>&1; then
        kubectl get pods -n "$SLINKY_NAMESPACE"
        fail "The demo workload did not start."
    fi

    success "Workload is running on ${NODE}"
}

# A pod that reports Ready once can still be crash-looping: fault-quarantine,
# for example, starts cleanly and only fails a few seconds later when it first
# evaluates the circuit breaker. Re-check after a settle window and treat a
# restart as a failed setup rather than reporting success over the top of it.
verify_sustained_readiness() {
    section "Confirming the controllers stay up"

    log "Waiting ${SETTLE_SECONDS}s and re-checking, so a crash loop is caught here rather than three steps later."

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
    success "Cluster '${CLUSTER_NAME}' is running NVSentinel ${CHART_VERSION}, with ${NODE} standing in for a GPU node"

    next_step "./scripts/01-show-cluster.sh   look at it before anything is broken"
}

main() {
    require_tools docker kind kubectl helm ko go jq curl column
    require_supported_namespace

    section "Preparing the cluster"
    log "Resolving the NVSentinel version to install..."
    CHART_VERSION="$(resolve_chart_version)"

    # slinky-drainer is a released NVSentinel component, so it is pulled at the
    # same version as the chart rather than built from this checkout. Override
    # SLINKY_DRAINER_IMAGE to try a local build instead.
    SLINKY_DRAINER_IMAGE="${SLINKY_DRAINER_IMAGE:-ghcr.io/nvidia/nvsentinel/slinky-drainer:${CHART_VERSION}}"

    success "Installing NVSentinel ${CHART_VERSION}"

    create_cluster
    install_cert_manager
    install_drain_request_crd
    create_drain_template
    install_nvsentinel
    deploy_fake_dcgm
    label_gpu_nodes

    NODE="$(gpu_node)"
    await_dcgm_connectivity

    build_mock_slurm
    deploy_plugins
    deploy_workload
    verify_sustained_readiness
    print_summary
}

main "$@"
