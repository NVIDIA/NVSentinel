#!/usr/bin/env bash
#
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

# Script to pull container logs from all Kind nodes via containerd/crictl
# Usage: ./pull_kind_logs.sh [output_dir]

OUTPUT_DIR="${1:-/tmp/kind_logs_$(date +%Y%m%d_%H%M%S)}"
mkdir -p "$OUTPUT_DIR"

echo "=== Pulling Kind node logs to: $OUTPUT_DIR ==="

# Try to find Kind nodes using multiple methods
# Method 1: Look for nvsentinel- prefixed containers (works for local dev and CI)
NODES=$(docker ps --filter "name=nvsentinel-" --format "{{.Names}}" 2>/dev/null)

# Method 2: If no nodes found, try to get cluster name from kind and find nodes
if [ -z "$NODES" ]; then
    echo "No nvsentinel- nodes found, trying to detect Kind cluster..."
    CLUSTER_NAME=$(kind get clusters 2>/dev/null | head -n1)
    if [ -n "$CLUSTER_NAME" ]; then
        echo "Found Kind cluster: $CLUSTER_NAME"
        # Kind nodes are named as: <cluster-name>-control-plane, <cluster-name>-worker, etc.
        NODES=$(docker ps --filter "name=${CLUSTER_NAME}-" --format "{{.Names}}" 2>/dev/null)
    fi
fi

# Method 3: If still no nodes, look for any kind- prefixed containers
if [ -z "$NODES" ]; then
    echo "Trying kind- prefix..."
    NODES=$(docker ps --filter "name=kind-" --format "{{.Names}}" 2>/dev/null)
fi

if [ -z "$NODES" ]; then
    echo "WARNING: No Kind nodes found. Listing all running containers for debugging:"
    docker ps --format "{{.Names}}" 2>/dev/null || true
    echo "Exiting without collecting logs."
    exit 0  # Exit gracefully in CI - don't fail the job
fi

echo "Found nodes: $NODES"
echo ""

# Components we're interested in
COMPONENTS="fault-quarantine health-events-analyzer fault-remediation node-drainer labeler janitor platform-connectors simple-health-client"

for NODE in $NODES; do
    echo "========================================"
    echo "Processing node: $NODE"
    echo "========================================"
    
    NODE_DIR="$OUTPUT_DIR/$NODE"
    mkdir -p "$NODE_DIR"
    
    # Get all containers on this node
    echo "Listing containers on $NODE..."
    docker exec "$NODE" crictl ps -a 2>/dev/null > "$NODE_DIR/all_containers.txt"
    
    # Pull logs for each component
    for COMPONENT in $COMPONENTS; do
        echo "  Looking for $COMPONENT containers..."
        
        # Get container IDs for this component (including exited ones with -a)
        CONTAINER_IDS=$(docker exec "$NODE" crictl ps -a 2>/dev/null | grep "$COMPONENT" | awk '{print $1}')
        
        if [ -n "$CONTAINER_IDS" ]; then
            for CID in $CONTAINER_IDS; do
                # Get container info
                CONTAINER_INFO=$(docker exec "$NODE" crictl ps -a 2>/dev/null | grep "$CID" | head -1)
                CONTAINER_NAME=$(echo "$CONTAINER_INFO" | awk '{print $7}')
                CONTAINER_STATE=$(echo "$CONTAINER_INFO" | awk '{print $5}')
                
                echo "    Found: $CONTAINER_NAME (ID: $CID, State: $CONTAINER_STATE)"
                
                # Create log filename with container ID
                LOG_FILE="$NODE_DIR/${COMPONENT}_${CID}.log"
                
                # Pull the logs
                docker exec "$NODE" crictl logs "$CID" > "$LOG_FILE" 2>&1
                
                # Also try to get previous logs if container was restarted
                docker exec "$NODE" crictl logs --previous "$CID" > "${LOG_FILE%.log}_previous.log" 2>/dev/null
                
                # Check if previous log is empty or has error, remove it
                if [ ! -s "${LOG_FILE%.log}_previous.log" ] || grep -q "error getting previous" "${LOG_FILE%.log}_previous.log" 2>/dev/null; then
                    rm -f "${LOG_FILE%.log}_previous.log"
                fi
                
                echo "      -> Saved to: $LOG_FILE ($(wc -l < "$LOG_FILE" 2>/dev/null || echo 0) lines)"
            done
        fi
    done
    
    # Also pull kubelet logs
    echo "  Pulling kubelet logs..."
    docker exec "$NODE" journalctl -u kubelet --no-pager -n 1000 > "$NODE_DIR/kubelet.log" 2>/dev/null
    echo "    -> Saved kubelet.log"
    
    # Pull containerd logs
    echo "  Pulling containerd logs..."
    docker exec "$NODE" journalctl -u containerd --no-pager -n 500 > "$NODE_DIR/containerd.log" 2>/dev/null
    echo "    -> Saved containerd.log"
    
    echo ""
done

echo "========================================"
echo "Log collection complete!"
echo "Output directory: $OUTPUT_DIR"
echo ""
echo "Summary:"
find "$OUTPUT_DIR" -name "*.log" -type f | while read -r f; do
    lines=$(wc -l < "$f" 2>/dev/null || echo 0)
    echo "  $f: $lines lines"
done
echo "========================================"


