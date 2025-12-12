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
SLINKY_NAMESPACE="slinky"

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
    kubectl get node ${CLUSTER_NAME}-worker -o json | jq -r '
        "  Name: \(.metadata.name)",
        "  Status: \(.status.conditions[] | select(.type=="Ready") | .status)",
        "  Schedulable: \(if .spec.unschedulable then "No (Cordoned)" else "Yes" end)",
        "  Annotations:",
        (.metadata.annotations | to_entries | 
         map(select(.key | startswith("nodeset.slinky"))) |
         if length > 0 then
             map("    \(.key): \(.value)") | join("\n")
         else
             "    (none related to slinky)"
         end)
    '
}

show_plugin_status() {
    section "Plugin Controllers"
    
    echo "Slinky Drainer:"
    kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=slinky-drainer \
        -o custom-columns=NAME:.metadata.name,STATUS:.status.phase,RESTARTS:.status.containerStatuses[0].restartCount,AGE:.metadata.creationTimestamp
    
    echo ""
    echo "Mock Slurm Operator:"
    kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=mock-slurm-operator \
        -o custom-columns=NAME:.metadata.name,STATUS:.status.phase,RESTARTS:.status.containerStatuses[0].restartCount,AGE:.metadata.creationTimestamp
}

show_workloads() {
    section "Slinky Workload Pods"
    
    echo "Pods in $SLINKY_NAMESPACE namespace:"
    if kubectl get pods -n "$SLINKY_NAMESPACE" --no-headers 2>/dev/null | wc -l | grep -q "^0$"; then
        echo "  (no pods found)"
    else
        kubectl get pods -n "$SLINKY_NAMESPACE" -o wide
        
        echo ""
        echo "Pod Conditions (SlurmNodeStateDrain):"
        kubectl get pods -n "$SLINKY_NAMESPACE" -o json | jq -r '
            .items[] | 
            "  \(.metadata.name): \(
                if (.status.conditions // [] | map(select(.type == "SlurmNodeStateDrain" and .status == "True")) | length) > 0 then
                    "✓ Ready for drain"
                else
                    "⧗ Not marked for drain"
                end
            )"
        '
    fi
}

show_drain_requests() {
    section "DrainRequest CRs"
    
    echo "Active DrainRequests:"
    if kubectl get drainrequests -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | grep -q "^0$"; then
        echo "  (none)"
    else
        kubectl get drainrequests -n "$NAMESPACE" -o custom-columns=\
NAME:.metadata.name,\
NODE:.spec.nodeName,\
ERROR:.spec.errorCode,\
STATUS:.status.conditions[?\(@.type==\"Complete\"\)].status,\
AGE:.metadata.creationTimestamp
    fi
}

show_summary() {
    section "Summary"
    
    local worker_schedulable=$(kubectl get node ${CLUSTER_NAME}-worker -o jsonpath='{.spec.unschedulable}')
    local workload_count=$(kubectl get pods -n "$SLINKY_NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local drain_count=$(kubectl get drainrequests -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    
    echo "Current State:"
    if [ "$worker_schedulable" = "true" ]; then
        echo "  🔒 Worker node: Cordoned (SchedulingDisabled)"
    else
        echo "  ✅ Worker node: Available (SchedulingEnabled)"
    fi
    echo "  📦 Workload pods: $workload_count in $SLINKY_NAMESPACE namespace"
    echo "  📋 DrainRequests: $drain_count active"
    echo ""
    
    if [ "$drain_count" -eq 0 ] && [ "$workload_count" -gt 0 ]; then
        echo "Ready for demo! Run './scripts/02-create-drain-request.sh' to trigger a drain."
    elif [ "$drain_count" -gt 0 ]; then
        echo "Drain in progress or completed. Check './scripts/03-verify-drain.sh' for status."
    fi
}

main() {
    show_nodes
    show_plugin_status
    show_workloads
    show_drain_requests
    show_summary
}

main "$@"

