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
RED='\033[0;31m'
NC='\033[0m'

CLUSTER_NAME="nvsentinel-demo"
NAMESPACE="nvsentinel"
TARGET_NODE=""

log() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[✓]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[!]${NC} $1"
}

error() {
    echo -e "${RED}[✗]${NC} $1"
}

section() {
    echo ""
    echo "=========================================="
    echo "  $1"
    echo "=========================================="
    echo ""
}

check_cluster() {
    if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        error "Cluster '$CLUSTER_NAME' not found. Run './scripts/00-setup.sh' first."
        exit 1
    fi
    
    kubectl config use-context "kind-${CLUSTER_NAME}" > /dev/null 2>&1
}

find_target_node() {
    # Dynamically find the first worker node (supports clusters with 1 or more workers)
    TARGET_NODE=$(kubectl get nodes -o json | jq -r '.items[] | select(.metadata.name | contains("worker")) | .metadata.name' | head -1)
    
    if [ -z "$TARGET_NODE" ]; then
        echo -e "\n${RED}[ERROR]${NC} No worker nodes found in cluster"
        exit 1
    fi
}

verify_cordon() {
    section "Verifying Node Cordon Status"
    
    log "Checking if node $TARGET_NODE was cordoned..."
    echo ""
    echo "  (Will retry every 3 seconds, up to 20 seconds if needed...)"
    echo ""
    
    # Poll for cordon status with retries
    local max_attempts=7  # 7 attempts x 3 seconds = 21 seconds
    local attempt=1
    local is_unschedulable="false"
    
    while [ $attempt -le $max_attempts ]; do
        is_unschedulable=$(kubectl get node "$TARGET_NODE" -o jsonpath='{.spec.unschedulable}' 2>/dev/null || echo "false")
        
        if [ "$is_unschedulable" = "true" ]; then
            success "Node $TARGET_NODE is CORDONED (SchedulingDisabled)"
            if [ $attempt -gt 1 ]; then
                echo "  ⏱️  Took $((attempt * 3)) seconds to cordon"
            fi
            echo ""
            break
        fi
        
        if [ $attempt -eq 1 ]; then
            echo "  ⏳ Not cordoned yet, waiting..."
        else
            echo "  ⏳ Attempt $attempt/$max_attempts - still waiting..."
        fi
        
        if [ $attempt -lt $max_attempts ]; then
            sleep 3
        fi
        ((attempt++))
    done
    
    if [ "$is_unschedulable" = "true" ]; then
        echo "  🔒 No new pods will be scheduled on this node"
        echo "  ✅ Existing workloads continue running (safe mode)"
        echo "  🎯 NVSentinel successfully quarantined the faulty node!"
        echo ""
    else
        warn "Node $TARGET_NODE is NOT cordoned yet"
        echo ""
        echo "This could mean:"
        echo "  - The event is still being processed (wait a few seconds)"
        echo "  - Fault Quarantine rules didn't match the event"
        echo "  - There was an error in the processing pipeline"
        echo ""
        echo "Try checking the logs: kubectl logs -n $NAMESPACE deployment/fault-quarantine"
        echo ""
    fi
    
    section "Node Details"
    
    kubectl get node "$TARGET_NODE" -o wide
    
    section "Node Conditions"
    
    log "Health-related conditions on $TARGET_NODE:"
    echo ""
    
    # Show all conditions
    kubectl get node "$TARGET_NODE" -o json | \
        if command -v jq &> /dev/null; then
            jq -r '.status.conditions[] | "\(.type)\t\(.status)\t\(.reason)\t\(.message)"'
        else
            grep -A 5 '"conditions"' || echo "Unable to parse conditions without jq"
        fi
    
    section "Recent Events for Node"
    
    log "Kubernetes events related to $TARGET_NODE:"
    echo ""
    
    kubectl get events -A --field-selector involvedObject.name="$TARGET_NODE" \
        --sort-by='.lastTimestamp' | tail -20
    
    section "NVSentinel Logs"
    
    log "Recent logs from Fault Quarantine:"
    echo ""
    
    kubectl logs -n "$NAMESPACE" deployment/fault-quarantine --tail=30 2>&1 || \
        warn "Could not fetch fault-quarantine logs"
    
    section "Comparison: All Nodes"
    
    log "Node status comparison:"
    echo ""
    
    kubectl get nodes
    
    echo ""
    
    # Visual summary
    for node in $(kubectl get nodes -o name | grep worker); do
        node_name=$(echo "$node" | cut -d'/' -f2)
        is_unschedulable=$(kubectl get "$node" -o jsonpath='{.spec.unschedulable}')
        
        if [ "$is_unschedulable" = "true" ]; then
            echo "  🔒 $node_name: CORDONED (protected from new workloads)"
        else
            echo "  ✅ $node_name: ACTIVE (accepting new workloads)"
        fi
    done
    
    section "Demo Complete! 🎉"
    
    echo "You've successfully completed the NVSentinel local demo!"
    echo ""
    echo "What you learned:"
    echo "  ✅ How NVSentinel detects GPU failures"
    echo "  ✅ How events flow through the system (HTTP → gRPC → MongoDB)"
    echo "  ✅ How Fault Quarantine automatically cordons faulty nodes"
    echo "  ✅ How Kubernetes prevents new workloads on failed hardware"
    echo ""
    echo "Next steps:"
    echo "  1. Explore NVSentinel logs: kubectl logs -n $NAMESPACE -l app=platform-connectors"
    echo "  2. Check out the full NVSentinel docs: ../../README.md"
    echo "  3. When done, clean up: ./scripts/99-cleanup.sh"
    echo ""
}

main() {
    check_cluster
    find_target_node
    verify_cordon
}

main "$@"

