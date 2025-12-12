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

log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

section() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  $*"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
}

inject_health_event() {
    section "Injecting GPU Health Event"
    
    log "Simulating GPU XID 79 error via simple-health-client..."
    log "This will trigger the FULL E2E workflow!"
    echo ""
    
    local client_pod=$(kubectl get pods -n "$NAMESPACE" -l app=simple-health-client -o jsonpath='{.items[0].metadata.name}')
    
    if [ -z "$client_pod" ]; then
        log "ERROR: simple-health-client pod not found"
        log "Make sure you ran the base setup first (00-setup.sh)"
        exit 1
    fi
    
    log "Using client pod: $client_pod"
    
    # Start port-forward in background
    kubectl port-forward -n "$NAMESPACE" "$client_pod" 8080:8080 >/dev/null 2>&1 &
    local pf_pid=$!
    
    # Wait for port-forward to be ready
    sleep 2
    
    # Inject health event via HTTP API
    log "Injecting health event..."
    local response=$(curl -s -X POST http://localhost:8080/health-event \
      -H 'Content-Type: application/json' \
      -d '{
        "version": 1,
        "agent": "gpu-health-monitor",
        "componentClass": "GPU",
        "checkName": "GpuXidError",
        "isFatal": true,
        "isHealthy": false,
        "message": "GPU XID 79 - GPU has fallen off the bus",
        "recommendedAction": 15,
        "errorCode": ["79"],
        "entitiesImpacted": [
          {
            "entityType": "GPU",
            "entityValue": "0"
          }
        ],
        "nodeName": "'"${CLUSTER_NAME}-worker"'"
      }')
    
    # Kill port-forward
    kill $pf_pid 2>/dev/null || true
    
    if echo "$response" | grep -q "success"; then
        log "✓ Health event injected successfully"
    else
        log "⚠ Unexpected response: $response"
    fi
}

watch_event_flow() {
    section "Watching Event Flow Through System"
    
    log "The health event will now flow through:"
    echo "  1. Platform Connectors (receives event via gRPC)"
    echo "  2. MongoDB (stores event)"
    echo "  3. Node-Drainer (watches change stream, evaluates policy)"
    echo "  4. DrainRequest CR (automatically created by node-drainer)"
    echo "  5. Slinky Drainer (processes DrainRequest)"
    echo "  6. Mock Slurm (sets pod conditions)"
    echo "  7. Slinky Drainer (deletes pods, marks complete)"
    echo ""
    
    log "Waiting 5 seconds for event to propagate..."
    sleep 5
    
    log "Checking MongoDB for the event..."
    local mongodb_pod=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=mongodb -o jsonpath='{.items[0].metadata.name}')
    
    if [ -n "$mongodb_pod" ]; then
        local event_count=$(kubectl exec -n "$NAMESPACE" "$mongodb_pod" -- mongosh --quiet --eval "db.getSiblingDB('nvsentinel').healthevents.countDocuments({nodeName: '${CLUSTER_NAME}-worker'})" 2>/dev/null || echo "0")
        if [ "$event_count" -gt 0 ]; then
            echo "  ✓ Event stored in MongoDB (count: $event_count)"
        else
            echo "  ⧗ Event not yet in MongoDB (may take a few seconds)"
        fi
    fi
    
    
    echo ""
    log "Waiting for DrainRequest CR to be created..."
    local timeout=30
    local elapsed=0
    
    while [ $elapsed -lt $timeout ]; do
        local dr_count=$(kubectl get drainrequests -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l)
        if [ "$dr_count" -gt 0 ]; then
            log "✓ DrainRequest CR created automatically by node-drainer!"
            kubectl get drainrequests -n "$NAMESPACE"
            break
        fi
        sleep 2
        ((elapsed+=2))
    done
    
    if [ $elapsed -ge $timeout ]; then
        log "⧗ DrainRequest not created yet (policy may not have matched)"
        log "   Check node-drainer logs for details"
    fi
}

show_next_steps() {
    section "What Happens Next"
    
    cat <<EOF
The AUTOMATIC E2E workflow is now in progress:

✅ Phase 1: Event Detection (DONE)
   - Health event injected via simple-health-client
   - Event stored in MongoDB

⧗ Phase 2: Policy Evaluation (IN PROGRESS)
   - Node-drainer watches MongoDB change stream
   - Evaluates "gpu-critical-errors" policy
   - Creates DrainRequest CR automatically

⧗ Phase 3: Custom Drain (WILL START)
   - Slinky Drainer detects DrainRequest
   - Sets node annotation
   - Waits for Slurm acknowledgment
   - Deletes pods
   - Marks CR complete

This process takes 20-40 seconds depending on polling intervals.

To verify the drain completed:
  ./scripts/03-verify-drain.sh

To watch in real-time:
  # Node-drainer (watches events, creates DrainRequests)
  kubectl logs -f -n $NAMESPACE deployment/node-drainer

  # Slinky Drainer (processes DrainRequests)
  kubectl logs -f -n $NAMESPACE deployment/slinky-drainer

  # Pods being drained
  watch -n 2 kubectl get pods -n slinky

EOF
}

main() {
    inject_health_event
    watch_event_flow
    show_next_steps
}

main "$@"

