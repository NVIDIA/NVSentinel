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

set -e

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m'

CLUSTER_NAME="nvsentinel-demo"
NAMESPACE="nvsentinel"

log() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[✓]${NC} $1"
}

section() {
    echo ""
    echo "=========================================="
    echo "  $1"
    echo "=========================================="
    echo ""
}

main() {
    section "NVSentinel Demo - Cluster Status"
    
    # Check if cluster exists
    if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        echo -e "${YELLOW}[WARN]${NC} Cluster '$CLUSTER_NAME' not found. Run './scripts/00-setup.sh' first."
        exit 1
    fi
    
    # Set context
    kubectl config use-context "kind-${CLUSTER_NAME}" > /dev/null 2>&1
    
    log "Cluster Nodes:"
    kubectl get nodes 
    
    echo ""
    log "Checking node scheduling status..."
    
    # Check each worker node
    for node in $(kubectl get nodes -o name | grep worker); do
        node_name=$(echo "$node" | cut -d'/' -f2)
        is_unschedulable=$(kubectl get "$node" -o jsonpath='{.spec.unschedulable}')
        
        if [ "$is_unschedulable" = "true" ]; then
            echo "  🔒 $node_name is CORDONED (SchedulingDisabled)"
        else
            echo "  ✅ $node_name is SCHEDULABLE (SchedulingEnabled)"
        fi
    done
    
    section "NVSentinel Pods"
    
    kubectl get pods -n "$NAMESPACE" -o wide
    
    echo ""
    log "Pod Status Summary:"
    
    # Count pod statuses
    local running
    local pending
    local failed
    running=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l)
    pending=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l)
    failed=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Failed --no-headers 2>/dev/null | wc -l)
    
    echo "  ✅ Running: $running"
    if [ "$pending" -gt 0 ]; then
        echo "  ⏳ Pending: $pending"
    fi
    if [ "$failed" -gt 0 ]; then
        echo "  ❌ Failed: $failed"
    fi
    
    section "Node Conditions (Health Events)"
    
    log "Checking for health-related node conditions..."
    echo ""
    
    # Check for any XID or health conditions
    local has_conditions=false
    for node in $(kubectl get nodes -o name | grep worker); do
        node_name=$(echo "$node" | cut -d'/' -f2)
        echo "Node: $node_name"
        
        # Get all conditions and capture for checking while also displaying
        node_conditions=$(kubectl get "$node" -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.message}{"\n"}{end}') || {
            echo "  ⚠️  Failed to fetch conditions for $node_name"
            continue
        }
        conditions_output=$(echo "$node_conditions" | \
            grep -v "^Ready\|^MemoryPressure\|^DiskPressure\|^PIDPressure\|^NetworkUnavailable" || true)
        
        echo "$conditions_output"
        
        # Track if we found any actual health failures (Status=True indicating a problem)
        if echo "$conditions_output" | grep -q $'\t'"True"$'\t'; then
            has_conditions=true
        fi
        
    done
    
    echo ""
    if [ "$has_conditions" = true ]; then
        echo "  ⚠️  Health issues detected (see conditions above)"
    else
        echo "  ✅ No health event conditions found (cluster is healthy)"
    fi
    
    section "Recent Events"
    
    log "Recent cluster events (last 10):"
    echo ""
    kubectl get events -A --sort-by='.lastTimestamp' | tail -10
    
    echo ""
    section "Summary"
    
    success "Cluster is ready for demo"
    echo ""
    echo "Next steps:"
    echo "  1. Run './scripts/02-inject-error.sh' to simulate a GPU failure"
    echo "  2. Run './scripts/03-verify-cordon.sh' to verify the node was cordoned"
    echo ""
}

main "$@"

