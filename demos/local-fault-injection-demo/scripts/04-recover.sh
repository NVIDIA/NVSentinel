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

# Step 4: the tail of Remediate — the repair lands and the node returns to
# service. A KIND node is never really rebooted, so restarting the DCGM pod is
# what clears the injected fault here. README.md explains why.

RECOVER_TIMEOUT=${RECOVER_TIMEOUT:-300}
# The DaemonSet has to create the replacement pod before it can become ready.
RESTART_TIMEOUT=${RESTART_TIMEOUT:-300}

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# Both of these check for the absence of something, so both have to prove the
# query succeeded first: an API error must not read as "the fault cleared".
has_no_gpu_condition() {
    local failing
    failing="$(kubectl get node "$NODE" -o json |
        jq -r '.status.conditions[] | select(.type | startswith("Gpu")) | select(.status == "True") | .type')" || return 1
    [[ -z "$failing" ]]
}

not_cordoned() {
    local unschedulable
    unschedulable="$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}')" || return 1
    [[ "$unschedulable" != "true" ]]
}

workload_running() {
    local ready
    ready="$(kubectl get deployment "$WORKLOAD_NAME" -n "$WORKLOAD_NAMESPACE" \
        -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    [[ "${ready:-0}" -ge 1 ]]
}

repair_gpu() {
    section "Repairing the GPU"

    local pod
    pod="$(kubectl get pods -n "$DCGM_NAMESPACE" -l app=nvidia-dcgm -o json |
        jq -r --arg node "$NODE" '.items[] | select(.spec.nodeName == $node) | .metadata.name' | head -1)"
    [[ -n "$pod" ]] || fail "No nvidia-dcgm pod on $NODE."

    log "Restarting ${DCGM_NAMESPACE}/${pod}, which drops the injected fault as a real reboot would have."

    kubectl delete pod -n "$DCGM_NAMESPACE" "$pod" --wait=true >/dev/null

    wait_for_pod_ready "$DCGM_NAMESPACE" app=nvidia-dcgm "$RESTART_TIMEOUT" ||
        fail "The replacement DCGM pod did not become ready within ${RESTART_TIMEOUT}s."

    success "DCGM is back, reporting a healthy GPU"
}

back_in_service() {
    section "Back in service"

    log "The monitor reports the check healthy again, and fault-quarantine uncordons on its own."

    wait_until "$RECOVER_TIMEOUT" "the GPU fault condition to clear" has_no_gpu_condition ||
        fail "The GPU condition on $NODE did not clear within ${RECOVER_TIMEOUT}s.
Check the monitor:  kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=gpu-health-monitor --tail=50"

    wait_until "$RECOVER_TIMEOUT" "$NODE to be uncordoned" not_cordoned ||
        fail "$NODE was not uncordoned within ${RECOVER_TIMEOUT}s.
Check fault-quarantine:  kubectl logs -n $NAMESPACE deployment/fault-quarantine --tail=50"

    wait_until "$RECOVER_TIMEOUT" "the workload to be rescheduled" workload_running ||
        fail "The workload did not come back within ${RECOVER_TIMEOUT}s.
Check the pods:  kubectl get pods -n $WORKLOAD_NAMESPACE -l app=${WORKLOAD_NAME}"

    echo
    kubectl get node "$NODE"
    echo
    kubectl get pods -n "$WORKLOAD_NAMESPACE" -l "app=${WORKLOAD_NAME}" -o wide
    echo
    success "$NODE is schedulable again, with the workload running on it"
}

main() {
    require_tools kubectl kind jq
    require_cluster
    NODE="$(gpu_node)"

    repair_gpu
    back_in_service

    next_step "./scripts/99-cleanup.sh   delete the cluster"
}

main "$@"
