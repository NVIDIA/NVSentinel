#!/bin/bash
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

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-demo}"
NAMESPACE="nvsentinel"

section() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  $*"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
}

show_nodes() {
    section "Cluster Nodes"

    echo "Node Status:"
    kubectl get nodes -o wide

    echo ""
    echo "Worker Node Details:"
    local worker="${CLUSTER_NAME}-worker"
    kubectl get node "$worker" -o json | jq -r '
        "  Name: \(.metadata.name)",
        "  Status: \(.status.conditions[] | select(.type=="Ready") | .status)",
        "  Schedulable: \(if .spec.unschedulable then "No (Cordoned)" else "Yes" end)"
    '

    echo ""
    echo "Available Memory on Worker:"
    local mem=$(kubectl get node "$worker" -o json | jq -r '.status.allocatable.memory')
    echo "  $mem"
}

show_nvsentinel_pods() {
    section "NVSentinel Pods"

    kubectl get pods -n "$NAMESPACE" -o wide
}

show_monitor_pods() {
    section "Memory Pressure Monitor"

    echo "DaemonSet:"
    kubectl get daemonset memory-pressure-monitor -n "$NAMESPACE" \
        -o custom-columns=NAME:.metadata.name,DESIRED:.status.desiredNumberScheduled,READY:.status.numberReady,NODE_SELECTOR:.spec.template.spec.affinity 2>/dev/null || \
        echo "  (not deployed)"

    echo ""
    echo "Pods:"
    kubectl get pods -n "$NAMESPACE" -l app=memory-pressure-monitor \
        -o custom-columns=NAME:.metadata.name,STATUS:.status.phase,NODE:.spec.nodeName,AGE:.metadata.creationTimestamp 2>/dev/null || \
        echo "  (none)"
}

show_controller() {
    section "Memory Reclaim Controller"

    echo "Deployment:"
    kubectl get deployment memory-reclaim-controller -n "$NAMESPACE" \
        -o custom-columns=NAME:.metadata.name,READY:.status.readyReplicas,AVAILABLE:.status.availableReplicas 2>/dev/null || \
        echo "  (not deployed)"

    echo ""
    echo "Pods:"
    kubectl get pods -n "$NAMESPACE" -l app=memory-reclaim-controller \
        -o custom-columns=NAME:.metadata.name,STATUS:.status.phase,RESTARTS:.status.containerStatuses[0].restartCount 2>/dev/null || \
        echo "  (none)"
}

show_summary() {
    section "Summary"

    local worker="${CLUSTER_NAME}-worker"
    local worker_schedulable=$(kubectl get node "$worker" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)
    local monitor_count=$(kubectl get pods -n "$NAMESPACE" -l app=memory-pressure-monitor --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local controller_ready=$(kubectl get deployment memory-reclaim-controller -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    local reclaim_count=$(kubectl get memoryreclaims --no-headers 2>/dev/null | wc -l | tr -d ' ')

    echo "Current State:"
    if [ "$worker_schedulable" = "true" ]; then
        echo "  🔒 Worker node: Cordoned (SchedulingDisabled)"
    else
        echo "  ✅ Worker node: Available (SchedulingEnabled)"
    fi
    echo "  📡 Memory monitors running: $monitor_count"
    echo "  🔧 Reclaim controller ready: ${controller_ready:-0}"
    echo "  📋 MemoryReclaim CRs: $reclaim_count"
    echo ""

    if [ "$reboot_count" -eq 0 ]; then
        echo "Ready for demo! Run './scripts/02-trigger-memory-pressure.sh' to trigger."
    elif [ "$reboot_count" -gt 0 ]; then
        echo "Remediation in progress. Check './scripts/03-verify-remediation.sh' for status."
    fi
}

main() {
    show_nodes
    show_nvsentinel_pods
    show_monitor_pods
    show_controller
    show_summary
}

main "$@"
