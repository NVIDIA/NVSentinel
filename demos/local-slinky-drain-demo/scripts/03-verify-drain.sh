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

verify_node_annotation() {
    section "1. Node Annotation"
    
    local annotation=$(kubectl get node ${CLUSTER_NAME}-worker \
        -o jsonpath='{.metadata.annotations.nodeset\.slinky\.slurm\.net/node-cordon-reason}' 2>/dev/null || echo "")
    
    if [ -n "$annotation" ]; then
        echo "✅ Node annotation is set:"
        echo "   $annotation"
        return 0
    else
        echo "❌ Node annotation NOT set"
        echo "   Expected: nodeset.slinky.slurm.net/node-cordon-reason"
        return 1
    fi
}

verify_pod_conditions() {
    section "2. Pod Conditions"
    
    local pod_count=$(kubectl get pods -n "$SLINKY_NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    
    if [ "$pod_count" -eq 0 ]; then
        echo "✅ All pods have been drained (deleted)"
        echo "   No pods remaining in $SLINKY_NAMESPACE namespace"
        return 0
    fi
    
    echo "Found $pod_count pods still running:"
    echo ""
    
    local all_ready=true
    while IFS= read -r pod; do
        local conditions=$(kubectl get pod "$pod" -n "$SLINKY_NAMESPACE" -o json | \
            jq -r '.status.conditions[] | select(.type == "SlurmNodeStateDrain") | .status')
        
        if [ "$conditions" = "True" ]; then
            echo "  ✅ $pod: SlurmNodeStateDrain=True (ready for drain)"
        else
            echo "  ⧗ $pod: SlurmNodeStateDrain not set (waiting...)"
            all_ready=false
        fi
    done < <(kubectl get pods -n "$SLINKY_NAMESPACE" -o jsonpath='{.items[*].metadata.name}')
    
    if $all_ready; then
        echo ""
        echo "✅ All pods marked ready for drain"
        echo "   (pods should be deleted shortly by slinky-drainer)"
        return 0
    else
        echo ""
        echo "⧗ Some pods not yet marked for drain"
        return 1
    fi
}

verify_drain_request_status() {
    section "3. DrainRequest Status"
    
    local drain_request=$(kubectl get drainrequests -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    
    if [ -z "$drain_request" ]; then
        echo "❌ No DrainRequest found"
        return 1
    fi
    
    local status=$(kubectl get drainrequest "$drain_request" -n "$NAMESPACE" \
        -o jsonpath='{.status.conditions[?(@.type=="DrainComplete")].status}' 2>/dev/null || echo "")
    
    echo "DrainRequest: $drain_request"
    
    if [ "$status" = "True" ]; then
        echo "✅ Status: DrainComplete=True"
        echo ""
        echo "Condition details:"
        kubectl get drainrequest "$drain_request" -n "$NAMESPACE" -o json | \
            jq -r '.status.conditions[] | "  \(.type): \(.status) (\(.reason))\n    \(.message)"'
        return 0
    elif [ -n "$status" ]; then
        echo "⧗ Status: DrainComplete=$status"
        return 1
    else
        echo "⧗ Status: Not yet updated (workflow in progress)"
        return 1
    fi
}

show_controller_logs() {
    section "4. Controller Logs"
    
    echo "Slinky Drainer (last 20 lines):"
    kubectl logs -n "$NAMESPACE" deployment/slinky-drainer --tail=20 | sed 's/^/  /' || true
    
    echo ""
    echo "Mock Slurm Operator (last 20 lines):"
    kubectl logs -n "$NAMESPACE" deployment/mock-slurm-operator --tail=20 | sed 's/^/  /' || true
}

show_timeline() {
    section "5. Event Timeline"
    
    echo "Recent events on worker node:"
    kubectl get events -n "$NAMESPACE" --sort-by='.lastTimestamp' \
        --field-selector involvedObject.name=${CLUSTER_NAME}-worker \
        -o custom-columns=TIME:.lastTimestamp,REASON:.reason,MESSAGE:.message | tail -10 || true
}

show_final_status() {
    section "Drain Workflow Status"
    
    local annotation_set=false
    local pods_ready=false  
    local drain_complete=false
    
    if kubectl get node ${CLUSTER_NAME}-worker \
        -o jsonpath='{.metadata.annotations.nodeset\.slinky\.slurm\.net/node-cordon-reason}' 2>/dev/null | grep -q .; then
        annotation_set=true
    fi
    
    local pod_count=$(kubectl get pods -n "$SLINKY_NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$pod_count" -eq 0 ]; then
        pods_ready=true
    fi
    
    local drain_request=$(kubectl get drainrequests -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$drain_request" ]; then
        local status=$(kubectl get drainrequest "$drain_request" -n "$NAMESPACE" \
            -o jsonpath='{.status.conditions[?(@.type=="DrainComplete")].status}' 2>/dev/null || echo "")
        if [ "$status" = "True" ]; then
            drain_complete=true
        fi
    fi
    
    echo "Workflow Steps:"
    if $annotation_set; then
        echo "  ✅ 1. Node annotation set"
    else
        echo "  ❌ 1. Node annotation NOT set"
    fi
    
    if $pods_ready; then
        echo "  ✅ 2. Pods drained (deleted)"
    else
        echo "  ⧗ 2. Pods still present ($pod_count remaining)"
    fi
    
    if $drain_complete; then
        echo "  ✅ 3. DrainRequest marked complete"
    else
        echo "  ⧗ 3. DrainRequest not yet complete"
    fi
    
    echo ""
    if $annotation_set && $pods_ready && $drain_complete; then
        echo "🎉 SUCCESS! The drain workflow completed successfully."
        echo ""
        echo "What happened:"
        echo "  1. DrainRequest CR triggered the workflow"
        echo "  2. Slinky Drainer set node annotation with error details"
        echo "  3. Mock Slurm Operator detected annotation"
        echo "  4. Mock Slurm set SlurmNodeStateDrain=True on all pods"
        echo "  5. Slinky Drainer waited for pod conditions"
        echo "  6. Slinky Drainer deleted all pods"
        echo "  7. Slinky Drainer marked DrainComplete=True"
        echo ""
        echo "This demonstrates how NVSentinel's custom drain framework enables"
        echo "integration with external orchestrators like Slurm!"
    else
        echo "⧗ Drain workflow still in progress."
        echo ""
        echo "Wait 10-30 seconds and run this script again, or watch real-time:"
        echo "  kubectl logs -f -n $NAMESPACE deployment/slinky-drainer"
    fi
}

main() {
    local checks_passed=0
    
    verify_node_annotation && ((checks_passed++)) || true
    verify_pod_conditions && ((checks_passed++)) || true
    verify_drain_request_status && ((checks_passed++)) || true
    
    show_controller_logs
    show_timeline
    show_final_status
    
    echo ""
    echo "Checks passed: $checks_passed/3"
    
    if [ "$checks_passed" -eq 3 ]; then
        exit 0
    else
        exit 1
    fi
}

main "$@"

