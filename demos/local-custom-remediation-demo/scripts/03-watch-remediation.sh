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

# Step 3: protect, remediate, return to service — each stage waited on by the
# effect it produces rather than by the clock, and each read off an object rather
# than out of a controller's log. README.md explains what every stage does.

PROTECT_TIMEOUT=${PROTECT_TIMEOUT:-180}
REMEDIATE_TIMEOUT=${REMEDIATE_TIMEOUT:-300}
RECOVER_TIMEOUT=${RECOVER_TIMEOUT:-300}

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

has_reclaim_request() { [[ -n "$(reclaim_name)" ]]; }

reclaim_processed() { [[ -n "$(reclaimed_pods)" ]]; }

# Checks for the absence of something, so it has to prove the query succeeded
# first: a connection or authorization error must not read as "the pod is gone".
#
# --ignore-not-found is what separates the two. It exits 0 with empty output when
# the pod is genuinely absent, and non-zero only when the query itself failed.
hog_gone() {
    local name deletion_ts

    name="$(kubectl get pod "$WORKLOAD_NAME" -n "$WORKLOAD_NAMESPACE" \
        --ignore-not-found -o name)" || return 1
    [[ -n "$name" ]] || return 0

    # Still listed: the delete is graceful, so a deletionTimestamp is the API's
    # record that it was accepted. Treating a Terminating pod as "still there"
    # is just a race against kubelet.
    deletion_ts="$(kubectl get pod "$WORKLOAD_NAME" -n "$WORKLOAD_NAMESPACE" \
        --ignore-not-found -o jsonpath='{.metadata.deletionTimestamp}')" || return 1
    [[ -n "$deletion_ts" ]]
}

fault_cleared() {
    local status
    status="$(node_condition_status "$NODE" MemoryAvailableCheck)" || return 1
    [[ "$status" == "False" ]]
}

not_cordoned() {
    local unschedulable
    unschedulable="$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}')" || return 1
    [[ "$unschedulable" != "true" ]]
}

protect() {
    section "Protect"

    log "fault-quarantine cordons the node so nothing new lands on it."
    log "node-drainer then drains it - a no-op here, because this demo configures no user"
    log "namespaces, which is what leaves the hog in place for the custom controller to remove."

    wait_until "$PROTECT_TIMEOUT" "$NODE to be cordoned" is_cordoned "$NODE" ||
        fail "$NODE was not cordoned within ${PROTECT_TIMEOUT}s.
Check fault-quarantine:  kubectl logs -n $NAMESPACE deployment/fault-quarantine --tail=50"

    echo
    kubectl get node "$NODE"
}

remediate() {
    section "Remediate"

    log "fault-remediation reads recommendedAction=CUSTOM/RECLAIM_MEMORY and renders the action's"
    log "template into a MemoryReclaim CR. The third-party controller takes it from there."

    wait_until "$REMEDIATE_TIMEOUT" "fault-remediation to create a MemoryReclaim" has_reclaim_request ||
        fail "No MemoryReclaim was created within ${REMEDIATE_TIMEOUT}s.
Check fault-remediation:  kubectl logs -n $NAMESPACE deployment/fault-remediation --tail=50"

    wait_until "$REMEDIATE_TIMEOUT" "the controller to process it" reclaim_processed ||
        fail "The controller never recorded an outcome on $(reclaim_name) within ${REMEDIATE_TIMEOUT}s.
Check the controller:  kubectl logs -n $NAMESPACE deployment/memory-reclaim-controller --tail=50"

    wait_until "$REMEDIATE_TIMEOUT" "the memory hog to be deleted" hog_gone ||
        fail "$WORKLOAD_NAME was still running after ${REMEDIATE_TIMEOUT}s."

    echo
    kubectl get memoryreclaims
    echo

    local reclaimed reason
    reclaimed="$(reclaimed_pods)"
    reason="$(reclaim_condition_field reason)"

    # An absent pod is not by itself evidence that the custom remediation did
    # anything. node-drainer evicts user workloads before remediation even
    # starts, so the pod can vanish without this controller being involved; the
    # count it records on the CR is what separates the two.
    if [[ "${reclaimed:-0}" -eq 0 ]]; then
        fail "The controller processed the request but deleted 0 pods (reason=${reason}).
Something removed $WORKLOAD_NAME before it ran. Check that node-drainer's userNamespaces
is still empty in config/nvsentinel-values.yaml - otherwise the drain evicts the hog first
and the custom remediation has nothing left to do."
    fi

    [[ "$reason" == "HogPodsDeleted" ]] ||
        fail "The MemoryReclaim finished with an unexpected reason: ${reason}"

    success "memory-reclaim-controller deleted ${reclaimed} pod(s) itself (status.reclaimedPods=${reclaimed}, reason=${reason})"
}

back_in_service() {
    section "Back in service"

    log "With the hog gone, MemAvailable climbs back above the threshold. The monitor sends a"
    log "healthy event, and fault-quarantine uncordons the node on its own."

    wait_until "$RECOVER_TIMEOUT" "the memory pressure condition to clear" fault_cleared ||
        fail "MemoryAvailableCheck did not turn False within ${RECOVER_TIMEOUT}s.
Check the monitor:  kubectl logs -n $NAMESPACE -l app=memory-pressure-monitor --tail=50"

    wait_until "$RECOVER_TIMEOUT" "$NODE to be uncordoned" not_cordoned ||
        fail "$NODE was not uncordoned within ${RECOVER_TIMEOUT}s.
A tripped circuit breaker halts recovery as well as quarantine; this demo disables it.
Check fault-quarantine:  kubectl logs -n $NAMESPACE deployment/fault-quarantine --tail=50"

    echo
    kubectl get node "$NODE"
    echo
    kubectl get node "$NODE" -o json |
        jq -r '.status.conditions[]
               | select(.type == "MemoryAvailableCheck")
               | "  \(.type)\t\(.status)\t\(.reason)\t\(.message)"' |
        column -t -s $'\t' | cut -c1-140
    echo
    success "$NODE is schedulable again, repaired by a controller this chart has never heard of"
}

main() {
    require_tools kubectl kind jq column
    require_cluster
    NODE="$(worker_node)"

    protect
    remediate
    back_in_service

    next_step "./scripts/99-cleanup.sh   delete the cluster"
}

main "$@"
