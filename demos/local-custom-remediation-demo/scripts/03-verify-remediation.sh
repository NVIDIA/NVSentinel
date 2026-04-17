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

log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

poll_with_timeout() {
    local description="$1"
    local check_cmd="$2"
    local timeout="${3:-120}"
    local interval="${4:-5}"
    local elapsed=0

    echo "  ⏳ Waiting for: $description (timeout: ${timeout}s)..."

    while [ $elapsed -lt $timeout ]; do
        if eval "$check_cmd" > /dev/null 2>&1; then
            echo "  ✅ $description"
            return 0
        fi
        sleep "$interval"
        ((elapsed += interval))
        echo "  ⏳ Still waiting... (${elapsed}s/${timeout}s)"
    done

    echo "  ❌ Timed out waiting for: $description"
    return 1
}

verify_node_cordoned() {
    section "1. Node Cordon Status"

    local worker="${CLUSTER_NAME}-worker"
    local is_unschedulable=$(kubectl get node "$worker" -o jsonpath='{.spec.unschedulable}' 2>/dev/null || echo "")

    if [ "$is_unschedulable" = "true" ]; then
        echo "  ✅ Node $worker is CORDONED (SchedulingDisabled)"
        return 0
    fi

    poll_with_timeout "Node $worker cordoned" \
        "[ \"\$(kubectl get node $worker -o jsonpath='{.spec.unschedulable}')\" = 'true' ]" \
        60 5

    return $?
}

verify_maintenance_cr() {
    section "2. Maintenance CR (MemoryReclaim)"

    local existing=$(kubectl get memoryreclaims --no-headers 2>/dev/null | grep "memory-reclaim" || true)

    if [ -n "$existing" ]; then
        echo "  ✅ MemoryReclaim CR found:"
        kubectl get memoryreclaims 2>/dev/null | grep "memory-reclaim" | sed 's/^/      /'
        return 0
    fi

    poll_with_timeout "MemoryReclaim CR created (memory-reclaim-*)" \
        "kubectl get memoryreclaims --no-headers 2>/dev/null | grep -q memory-reclaim" \
        120 5

    if [ $? -eq 0 ]; then
        kubectl get memoryreclaims 2>/dev/null | grep "memory-reclaim" | sed 's/^/      /'
        return 0
    fi

    return 1
}

verify_memory_hog_deleted() {
    section "3. Memory Hog Pod Cleanup"

    local pod_exists=$(kubectl get pod memory-hog -n default --no-headers 2>/dev/null || true)

    if [ -z "$pod_exists" ]; then
        echo "  ✅ Memory hog pod has been deleted"
        return 0
    fi

    poll_with_timeout "Memory hog pod deleted" \
        "! kubectl get pod memory-hog -n default --no-headers 2>/dev/null | grep -q memory-hog" \
        120 5

    return $?
}

verify_cr_completed() {
    section "4. CR Completion Status"

    local cr_name=$(kubectl get memoryreclaims --no-headers 2>/dev/null | grep "memory-reclaim" | awk '{print $1}' | head -1)

    if [ -z "$cr_name" ]; then
        echo "  ⧗ No MemoryReclaim CR found yet"
        return 1
    fi

    local status=$(kubectl get memoryreclaim "$cr_name" \
        -o jsonpath='{.status.conditions[?(@.type=="MemoryReclaimed")].status}' 2>/dev/null || echo "")

    if [ "$status" = "True" ]; then
        echo "  ✅ MemoryReclaim $cr_name: MemoryReclaimed=True"
        echo ""
        echo "  Condition details:"
        kubectl get memoryreclaim "$cr_name" -o json | \
            jq -r '.status.conditions[] | "    \(.type): \(.status) (\(.reason))\n      \(.message)"'
        return 0
    fi

    poll_with_timeout "MemoryReclaim $cr_name MemoryReclaimed=True" \
        "[ \"\$(kubectl get memoryreclaim $cr_name -o jsonpath='{.status.conditions[?(@.type==\"MemoryReclaimed\")].status}')\" = 'True' ]" \
        120 5

    if [ $? -eq 0 ]; then
        echo ""
        echo "  Condition details:"
        kubectl get memoryreclaim "$cr_name" -o json | \
            jq -r '.status.conditions[] | "    \(.type): \(.status) (\(.reason))\n      \(.message)"'
        return 0
    fi

    return 1
}

show_controller_logs() {
    section "5. Controller Logs"

    echo "Memory Pressure Monitor (last 15 lines):"
    kubectl logs -n "$NAMESPACE" -l app=memory-pressure-monitor --tail=15 2>/dev/null | sed 's/^/  /' || \
        echo "  (no logs available)"

    echo ""
    echo "Memory Reclaim Controller (last 15 lines):"
    kubectl logs -n "$NAMESPACE" deployment/memory-reclaim-controller --tail=15 2>/dev/null | sed 's/^/  /' || \
        echo "  (no logs available)"

    echo ""
    echo "Fault Remediation (last 15 lines):"
    kubectl logs -n "$NAMESPACE" deployment/fault-remediation --tail=15 2>/dev/null | sed 's/^/  /' || \
        echo "  (no logs available)"
}

show_final_status() {
    section "Remediation Workflow Status"

    local node_cordoned=false
    local cr_exists=false
    local hog_deleted=false
    local cr_completed=false

    local worker="${CLUSTER_NAME}-worker"
    local is_unschedulable=$(kubectl get node "$worker" -o jsonpath='{.spec.unschedulable}' 2>/dev/null || echo "")
    [ "$is_unschedulable" = "true" ] && node_cordoned=true

    local cr_name=$(kubectl get memoryreclaims --no-headers 2>/dev/null | grep "memory-reclaim" | awk '{print $1}' | head -1)
    [ -n "$cr_name" ] && cr_exists=true

    local pod_exists=$(kubectl get pod memory-hog -n default --no-headers 2>/dev/null || true)
    [ -z "$pod_exists" ] && hog_deleted=true

    if [ -n "$cr_name" ]; then
        local status=$(kubectl get memoryreclaim "$cr_name" \
            -o jsonpath='{.status.conditions[?(@.type=="MemoryReclaimed")].status}' 2>/dev/null || echo "")
        [ "$status" = "True" ] && cr_completed=true
    fi

    echo "Workflow Steps:"
    $node_cordoned && echo "  ✅ 1. Node cordoned (SchedulingDisabled)" || echo "  ⧗ 1. Node NOT cordoned yet"
    $cr_exists && echo "  ✅ 2. MemoryReclaim CR created" || echo "  ⧗ 2. MemoryReclaim CR NOT created yet"
    $hog_deleted && echo "  ✅ 3. Memory hog pod deleted" || echo "  ⧗ 3. Memory hog pod still running"
    $cr_completed && echo "  ✅ 4. CR status: MemoryReclaimed=True" || echo "  ⧗ 4. CR status NOT complete yet"

    echo ""
    if $node_cordoned && $cr_exists && $hog_deleted && $cr_completed; then
        echo "🎉 SUCCESS! The custom remediation workflow completed successfully."
        echo ""
        echo "What happened:"
        echo "  1. Memory pressure monitor detected low available memory"
        echo "  2. Sent CUSTOM health event with RECLAIM_MEMORY action"
        echo "  3. Fault quarantine cordoned the node"
        echo "  4. Fault remediation created a MemoryReclaim CR (custom CRD)"
        echo "  5. Memory reclaim controller watched for MemoryReclaim CRs"
        echo "  6. Controller deleted memory-hog pods on the affected node"
        echo "  7. Controller set MemoryReclaimed=True on the CR"
        echo ""
        echo "This demonstrates how NVSentinel's custom remediation framework"
        echo "enables third-party CRDs and controllers for domain-specific remediation!"
    else
        echo "⧗ Remediation workflow still in progress."
        echo ""
        echo "Wait 10-30 seconds and run this script again, or watch real-time:"
        echo "  kubectl logs -f -n $NAMESPACE -l app=memory-pressure-monitor"
        echo "  kubectl logs -f -n $NAMESPACE deployment/memory-reclaim-controller"
    fi
}

main() {
    local checks_passed=0

    verify_node_cordoned && ((checks_passed++)) || true
    verify_maintenance_cr && ((checks_passed++)) || true
    verify_memory_hog_deleted && ((checks_passed++)) || true
    verify_cr_completed && ((checks_passed++)) || true

    show_controller_logs
    show_final_status

    echo ""
    echo "Checks passed: $checks_passed/4"

    if [ "$checks_passed" -eq 4 ]; then
        exit 0
    else
        exit 1
    fi
}

main "$@"
