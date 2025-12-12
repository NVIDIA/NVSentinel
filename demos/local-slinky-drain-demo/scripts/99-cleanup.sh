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

main() {
    section "Cleaning Up Slinky Drain Demo"
    
    log "Deleting KIND cluster: $CLUSTER_NAME..."
    if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
        kind delete cluster --name "$CLUSTER_NAME"
        log "✓ Cluster deleted"
    else
        log "Cluster '$CLUSTER_NAME' not found, skipping"
    fi
    
    log "Cleaning up local Docker images..."
    docker rmi -f node-drainer:demo 2>/dev/null || true
    docker rmi -f slinky-drainer:demo 2>/dev/null || true
    docker rmi -f mock-slurm-operator:demo 2>/dev/null || true
    docker rmi -f platform-connectors:demo 2>/dev/null || true
    docker rmi -f ko.local:demo 2>/dev/null || true
    
    log "✅ Cleanup complete!"
}

main "$@"
