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

# Step 1: the baseline the later steps are read against — a healthy node, a
# monitor watching it, and a controller waiting for something to repair.

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

main() {
    require_tools kubectl kind jq docker
    require_cluster
    NODE="$(worker_node)"

    section "Before the fault"

    kubectl get nodes
    echo

    log "The two demo components:"
    kubectl get daemonset memory-pressure-monitor -n "$NAMESPACE"
    kubectl get deployment memory-reclaim-controller -n "$NAMESPACE"
    echo

    local threshold
    threshold="$(kubectl get daemonset memory-pressure-monitor -n "$NAMESPACE" \
        -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MEM_THRESHOLD_MB")].value}')"

    log "MemAvailable on ${NODE}: $(node_available_mb) MB"
    log "Monitor threshold:       ${threshold} MB"
    echo

    log "No MemoryReclaim CR exists yet; fault-remediation creates one when a fault arrives."
    kubectl get memoryreclaims 2>/dev/null || true
    echo

    # False is the only value that means "the monitor looked and found nothing
    # wrong". Absent means it has not published a reading yet, which is not a
    # healthy baseline and must not be reported as one.
    local condition
    condition="$(node_condition_status "$NODE" MemoryAvailableCheck)"

    if is_cordoned "$NODE"; then
        warn "$NODE is already cordoned. Run ./scripts/99-cleanup.sh and start again for a clean demo."
    elif [[ "$condition" == "True" ]]; then
        warn "$NODE is already reporting memory pressure (MemoryAvailableCheck=True)."
    elif [[ "$condition" == "False" ]]; then
        success "$NODE is healthy and schedulable, with memory to spare"
    elif [[ -z "$condition" ]]; then
        warn "$NODE has no MemoryAvailableCheck condition yet, so the monitor has not published a
reading. Give it a poll interval and re-run; if it stays absent, check the monitor:
  kubectl logs -n $NAMESPACE -l app=memory-pressure-monitor --tail=50"
    else
        warn "$NODE reports MemoryAvailableCheck=${condition}, which is neither True nor False."
    fi

    next_step "./scripts/02-trigger-pressure.sh   fill the node's memory"
}

main "$@"
