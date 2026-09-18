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

# Step 2: fill the node's memory.
#
# This is the equivalent of injecting an XID into DCGM in the fault injection
# demo: a real fault, produced at the source, that the monitor discovers on its
# own. Nothing downstream is told.

TRIGGER_TIMEOUT=${TRIGGER_TIMEOUT:-120}

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

pressure_detected() {
    [[ "$(node_condition_status "$NODE" MemoryAvailableCheck)" == "True" ]]
}

# The monitor compares the host's MemAvailable against a fixed threshold, and
# KIND shares the host's memory, so a threshold computed during setup is stale by
# the time you get here: the host frees or consumes hundreds of MB on its own and
# a 300 MB allocation then fails to cross it. Recomputing immediately before the
# hog starts makes the gap exactly MEM_MARGIN_MB.
recalibrate() {
    section "Recalibrating the threshold"

    local avail_mb threshold current
    avail_mb="$(node_available_mb)"

    if ((avail_mb <= MEM_MARGIN_MB)); then
        fail "MemAvailable on $NODE is only ${avail_mb} MB, under the ${MEM_MARGIN_MB} MB margin.
Free some memory on the host, or lower MEM_MARGIN_MB."
    fi

    threshold=$((avail_mb - MEM_MARGIN_MB))
    current="$(kubectl get daemonset memory-pressure-monitor -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MEM_THRESHOLD_MB")].value}')"

    log "MemAvailable now:     ${avail_mb} MB"
    log "Threshold from setup: ${current:-unknown} MB"
    log "Recalibrated to:      ${threshold} MB  (MemAvailable - ${MEM_MARGIN_MB} MB)"

    if [[ "$current" == "$threshold" ]]; then
        success "Threshold is already current"
        return
    fi

    kubectl set env daemonset/memory-pressure-monitor -n "$NAMESPACE" \
        MEM_THRESHOLD_MB="$threshold" >/dev/null
    kubectl rollout status daemonset/memory-pressure-monitor -n "$NAMESPACE" --timeout=180s >/dev/null ||
        fail "The monitor did not roll out with the new threshold."

    # The monitor only emits on state transitions. After the restart its first
    # reading is healthy, because the threshold was just placed below the current
    # MemAvailable, so the hog produces a clean healthy -> unhealthy edge.
    success "Threshold recalibrated to ${threshold} MB"
}

deploy_hog() {
    section "Allocating ${HOG_MEM_MB} MB on $NODE"

    log "A pod that reserves memory and holds it, ${MEM_MARGIN_MB} MB of headroom below the threshold."

    # Pods are largely immutable, so a re-run with different sizing cannot be
    # applied over the previous attempt.
    kubectl delete pod "$WORKLOAD_NAME" -n "$WORKLOAD_NAMESPACE" --ignore-not-found --wait >/dev/null

    apply_manifest memory-hog.yaml \
        "NODE=${NODE}" \
        "HOG_MEM_MB=${HOG_MEM_MB}" \
        "HOG_LIMIT_MB=$((HOG_MEM_MB + 50))"

    # A Ready timeout is not a failed trigger, so the run continues either way:
    # stress allocates before Kubernetes reports Ready, and once pressure is
    # detected the pod is deleted by the remediation this step is trying to
    # provoke. confirm_detected is the real check, so say nothing here that
    # claims more than we know.
    if kubectl wait --for=condition=ready --timeout=180s \
        "pod/$WORKLOAD_NAME" -n "$WORKLOAD_NAMESPACE" >/dev/null 2>&1; then
        success "${WORKLOAD_NAME} is holding ${HOG_MEM_MB} MB on $NODE"
    else
        log "${WORKLOAD_NAME} is not Ready yet; it may still be pulling, or already reclaimed."
        log "The node condition decides, so continuing."
    fi
}

confirm_detected() {
    section "Confirming the monitor saw it"

    # The proof is the node condition platform-connectors writes from the health
    # event: its type is the monitor's checkName, and Status=True means a fault is
    # present (NVSentinel inverts the usual sense). Checking it here means a stale
    # calibration is reported now rather than looking like a broken pipeline in
    # the next step.
    log "MemoryAvailableCheck=True on the node is the health event having landed."

    wait_until "$TRIGGER_TIMEOUT" "MemoryAvailableCheck to turn True" pressure_detected || {
        echo
        warn "The monitor never reported pressure. The host most likely freed memory between
recalibration and the hog starting, so MemAvailable never fell below the threshold.

Retry once with a bigger allocation and a wider margin:
  HOG_MEM_MB=1024 MEM_MARGIN_MB=700 ./scripts/02-trigger-pressure.sh

If that fails too, reset rather than retrying further - a half-processed event
leaves the node cordoned and the pipeline mid-flight:
  ./demo.sh cleanup && ./scripts/00-setup.sh"
        fail "No memory pressure detected within ${TRIGGER_TIMEOUT}s."
    }

    echo
    kubectl get node "$NODE" -o json |
        jq -r '.status.conditions[]
               | select(.type == "MemoryAvailableCheck")
               | "  \(.type)\t\(.status)\t\(.reason)\t\(.message)"' |
        column -t -s $'\t' | cut -c1-140
    echo
    success "The fault is on the node as a condition"

    next_step "./scripts/03-watch-remediation.sh   watch the custom remediation run"
}

main() {
    require_tools kubectl kind jq docker column
    require_cluster
    NODE="$(worker_node)"

    if is_cordoned "$NODE"; then
        warn "$NODE is already cordoned. Run ./demo.sh cleanup and start again for the full sequence."
        echo
    fi

    recalibrate
    deploy_hog
    confirm_detected
}

main "$@"
