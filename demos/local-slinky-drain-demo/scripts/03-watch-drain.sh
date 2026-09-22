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

# Step 3: detect, delegate, drain — each stage waited on by the effect it
# produces rather than by the clock, and each read off an object rather than out
# of a controller's log. README.md explains what every stage does and why.

DETECT_TIMEOUT=${DETECT_TIMEOUT:-180}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-300}

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

gpu_conditions() {
    kubectl get node "$NODE" -o json |
        jq -r '.status.conditions[]
               | select(.type | startswith("Gpu"))
               | select(.status == "True")
               | "\(.type)\t\(.reason)\t\(.message)"'
}

has_gpu_condition() { [[ -n "$(gpu_conditions)" ]]; }

has_drain_request() {
    [[ -n "$(kubectl get drainrequests -n "$NAMESPACE" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)" ]]
}

cordon_annotation() {
    kubectl get node "$NODE" \
        -o jsonpath='{.metadata.annotations.nodeset\.slinky\.slurm\.net/node-cordon-reason}' 2>/dev/null
}

node_annotated() { [[ -n "$(cordon_annotation)" ]]; }

# How many workload pods the mock scheduler has marked drainable.
pods_marked_for_drain() {
    kubectl get pods -n "$SLINKY_NAMESPACE" -o json 2>/dev/null |
        jq '[.items[].status.conditions[]?
             | select(.type == "SlurmNodeStateDrain" and .status == "True")] | length'
}

scheduler_responded() { [[ "$(pods_marked_for_drain)" -gt 0 ]]; }

# The DrainRequest's terminal reason. slinky-drainer writes DrainComplete only
# after it has deleted pods, and NoPods when it found none, so this is what
# separates "the drain remediated something" from "the request finished".
drain_reason() { drain_complete_field reason; }

drain_finished() { [[ -n "$(drain_reason)" ]]; }

# Checks for the absence of something, so it has to prove the query succeeded
# first: an API error must not read as "the workload is gone".
workload_evicted() {
    local nodes
    nodes="$(kubectl get pods -n "$SLINKY_NAMESPACE" \
        -o jsonpath='{.items[*].spec.nodeName}')" || return 1
    [[ " $nodes " != *" $NODE "* ]]
}

detect() {
    section "Detect"

    log "gpu-health-monitor polls DCGM and reports what it finds as a node condition."

    wait_until "$DETECT_TIMEOUT" "a GPU fault condition on $NODE" has_gpu_condition ||
        fail "No GPU condition appeared on $NODE within ${DETECT_TIMEOUT}s.
Check the monitor:  kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=gpu-health-monitor --tail=50"

    echo
    gpu_conditions | column -t -s $'\t' | cut -c1-140
}

delegate() {
    section "Delegate"

    log "fault-quarantine cordons the node. node-drainer then does NOT evict anything itself:"
    log "custom drain is enabled, so it writes a DrainRequest and waits for someone else to do the work."

    wait_until "$DETECT_TIMEOUT" "$NODE to be cordoned" is_cordoned "$NODE" ||
        fail "$NODE was not cordoned within ${DETECT_TIMEOUT}s.
Check fault-quarantine:  kubectl logs -n $NAMESPACE deployment/fault-quarantine --tail=50"

    wait_until "$DRAIN_TIMEOUT" "node-drainer to create a DrainRequest" has_drain_request ||
        fail "No DrainRequest was created within ${DRAIN_TIMEOUT}s.
Check node-drainer:  kubectl logs -n $NAMESPACE deployment/node-drainer --tail=50"

    echo
    kubectl get drainrequests -n "$NAMESPACE"
}

coordinate() {
    section "Coordinate"

    log "slinky-drainer annotates the node so the scheduler knows to stop using it,"
    log "then waits: it will not delete a pod until Slurm says that pod's work is finished."

    # Both of these are transient. The annotation is removed and the pods are
    # deleted as soon as the handshake completes, so a slow poll can legitimately
    # miss them; the DrainRequest reason in the next stage is the durable record.
    if wait_until 60 "the node-cordon-reason annotation" node_annotated; then
        echo
        log "Annotation set by slinky-drainer:"
        echo "       $(cordon_annotation)"
    else
        warn "Did not catch the annotation before it was cleaned up; the drain moved faster than this poll."
    fi

    if wait_until 60 "mock-slurm-operator to mark the pods drainable" scheduler_responded; then
        echo
        kubectl get pods -n "$SLINKY_NAMESPACE" -o json |
            jq -r '.items[] | "  \(.metadata.name)\tSlurmNodeStateDrain=\([.status.conditions[]? | select(.type == "SlurmNodeStateDrain") | .status] | first // "unset")"' |
            column -t -s $'\t'
    else
        warn "Did not catch the pod conditions before the pods were deleted; the drain moved faster than this poll."
    fi
}

drain() {
    section "Drain"

    log "With every pod cleared by the scheduler, slinky-drainer deletes them and closes the request."

    wait_until "$DRAIN_TIMEOUT" "the workload to be evicted" workload_evicted ||
        fail "The workload was still on $NODE after ${DRAIN_TIMEOUT}s.
Check slinky-drainer:  kubectl logs -n $NAMESPACE deployment/slinky-drainer --tail=50"

    wait_until "$DRAIN_TIMEOUT" "slinky-drainer to close the DrainRequest" drain_finished ||
        fail "The DrainRequest never reached a terminal state within ${DRAIN_TIMEOUT}s.
Check slinky-drainer:  kubectl logs -n $NAMESPACE deployment/slinky-drainer --tail=50"

    local reason
    reason="$(drain_reason)"

    echo
    kubectl get drainrequests -n "$NAMESPACE" -o json |
        jq -r '.items[0].status.conditions[]? | "  \(.type)\t\(.status)\t\(.reason)\t\(.message)"' |
        column -t -s $'\t'
    echo

    # A completed request is not by itself evidence that anything was drained:
    # slinky-drainer closes the request either way, recording NoPods when it
    # found nothing to evict. Only the reason separates the two.
    if [[ "$reason" == "NoPods" ]]; then
        fail "The DrainRequest completed with reason=NoPods, so slinky-drainer deleted nothing.
The workload was gone before the drain began. Check that 00-setup.sh created it in the $SLINKY_NAMESPACE namespace."
    fi

    [[ "$reason" == "DrainComplete" ]] ||
        fail "The DrainRequest finished with an unexpected reason: ${reason}"

    success "slinky-drainer evicted the workload and closed the request (reason=DrainComplete)"

    echo
    kubectl get pods -n "$SLINKY_NAMESPACE" 2>/dev/null | grep . ||
        log "No pods left in the $SLINKY_NAMESPACE namespace."
    echo
    log "The node is drained but still cordoned: draining is not return to service."
}

main() {
    require_tools kubectl kind jq column
    require_cluster
    NODE="$(gpu_node)"

    detect
    delegate
    coordinate
    drain

    next_step "./scripts/04-recover.sh   clear the fault and watch the node come back"
}

main "$@"
