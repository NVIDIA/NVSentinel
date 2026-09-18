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

# Step 3: detect, protect, remediate — the three stages from the project README,
# each waited on by the effect it produces rather than by the clock.
# README.md explains what each stage does and why.

DETECT_TIMEOUT=${DETECT_TIMEOUT:-180}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-180}
REMEDIATE_TIMEOUT=${REMEDIATE_TIMEOUT:-300}

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

node_state() {
    kubectl get node "$NODE" -o jsonpath="{.metadata.labels.dgxc\\.nvidia\\.com/nvsentinel-state}" 2>/dev/null
}

# True once the node has reached the named state or any state after it. Modules
# hand the node on in well under a second, so an exact match is unreliable.
reached_state() {
    local target=$1 current
    current="$(node_state)"
    [[ -n "$current" ]] || return 1

    local order=(quarantined draining drain-succeeded remediating remediation-succeeded)
    local target_index=-1 current_index=-1 i
    for i in "${!order[@]}"; do
        [[ "${order[$i]}" == "$target" ]] && target_index=$i
        [[ "${order[$i]}" == "$current" ]] && current_index=$i
    done

    [[ $current_index -ge 0 && $target_index -ge 0 && $current_index -ge $target_index ]]
}

gpu_conditions() {
    kubectl get node "$NODE" -o json |
        jq -r '.status.conditions[]
               | select(.type | startswith("Gpu"))
               | select(.status == "True")
               | "\(.type)\t\(.reason)\t\(.message)"'
}

has_gpu_condition() { [[ -n "$(gpu_conditions)" ]]; }

reboot_requests() {
    kubectl get rebootnodes.janitor.dgxc.nvidia.com -o json 2>/dev/null |
        jq -r --arg node "$NODE" '.items[] | select(.spec.nodeName == $node) | .metadata.name'
}

has_reboot_request() { [[ -n "$(reboot_requests)" ]]; }

# Note the explicit query check: a failed API call must not be read as "the
# workload is no longer there", which would let the wait succeed on an error.
workload_evicted() {
    local nodes
    nodes="$(kubectl get pods -n "$WORKLOAD_NAMESPACE" -l "app=${WORKLOAD_NAME}" \
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

protect() {
    section "Protect"

    log "fault-quarantine cordons the node, then node-drainer evicts what was running on it."

    wait_until "$DETECT_TIMEOUT" "$NODE to be cordoned" is_cordoned "$NODE" ||
        fail "$NODE was not cordoned within ${DETECT_TIMEOUT}s.
Check fault-quarantine:  kubectl logs -n $NAMESPACE deployment/fault-quarantine --tail=50"

    wait_until "$DRAIN_TIMEOUT" "the workload to be evicted" workload_evicted ||
        fail "The workload was still on $NODE after ${DRAIN_TIMEOUT}s.
Check node-drainer:  kubectl logs -n $NAMESPACE deployment/node-drainer --tail=50"

    wait_until "$DRAIN_TIMEOUT" "the drain to be reported complete" reached_state drain-succeeded ||
        warn "The node did not reach drain-succeeded; continuing anyway."

    echo
    kubectl get node "$NODE"
    echo
    kubectl get pods -n "$WORKLOAD_NAMESPACE" -l "app=${WORKLOAD_NAME}"
}

remediate() {
    section "Remediate"

    log "fault-remediation requests the repair the fault calls for, and janitor carries it out."

    wait_until "$REMEDIATE_TIMEOUT" "a RebootNode for $NODE" has_reboot_request ||
        fail "No RebootNode was created within ${REMEDIATE_TIMEOUT}s.
Check fault-remediation:  kubectl logs -n $NAMESPACE deployment/fault-remediation --tail=50"

    wait_until "$REMEDIATE_TIMEOUT" "janitor to finish the reboot" reached_state remediation-succeeded ||
        fail "The node did not reach remediation-succeeded within ${REMEDIATE_TIMEOUT}s (state: $(node_state)).
Check janitor:  kubectl logs -n $NAMESPACE deployment/janitor --tail=50"

    echo
    kubectl get rebootnodes.janitor.dgxc.nvidia.com
    echo
    log "The reboot was simulated, so the GPU still reports XID 95 and the node stays out of service."
}

main() {
    require_tools kubectl kind jq
    require_cluster
    NODE="$(gpu_node)"

    detect
    protect
    remediate

    next_step "./scripts/04-recover.sh   clear the fault and watch the node come back"
}

main "$@"
