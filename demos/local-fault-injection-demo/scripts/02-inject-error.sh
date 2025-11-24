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
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
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

check_node_exists() {
    if ! kubectl get node "$TARGET_NODE" &> /dev/null; then
        error "Node '$TARGET_NODE' not found in cluster"
    fi
}

inject_gpu_error() {
    section "Injecting GPU Error"
    
    log "Simulating GPU failure on node: $TARGET_NODE"
    echo ""
    echo "  💥 Simulating GPU hardware fault (corrupt InfoROM)"
    echo "  📍 Target node: $TARGET_NODE"
    echo "  ⚠️  This is a FATAL error that requires node quarantine"
    echo ""
    
    # Find the DCGM pod on the target node
    local dcgm_pod
    dcgm_pod=$(kubectl get pods -n gpu-operator \
        -l app=nvidia-dcgm \
        -o json | jq -r ".items[] | select(.spec.nodeName==\"$TARGET_NODE\") | .metadata.name")
    
    if [ -z "$dcgm_pod" ]; then
        error "Could not find nvidia-dcgm pod on node $TARGET_NODE"
    fi
    
    log "Found DCGM pod: $dcgm_pod"
    log "Injecting GPU fault via DCGM..."
    
    # Inject field 84 (Inforom Valid) = 0 to simulate a GPU hardware fault
    kubectl exec -n gpu-operator "$dcgm_pod" -- dcgmi test --inject --gpuid 0 -f 84 -v 0
    
    success "GPU fault injected into fake GPU!"
    
    echo ""
    log "DCGM will report corrupt InfoROM on GPU 0"
    
    section "What Happens Next?"
    
    echo "The health event is now flowing through NVSentinel:"
    echo ""
    echo "  1. 🔍 GPU Health Monitor detects the GPU error from DCGM"
    echo "  2. 📡 Sends health event to Platform Connectors via gRPC"
    echo "  3. 📊 Platform Connectors store event in MongoDB"
    echo "  4. 👀 Fault Quarantine watches MongoDB change stream"
    echo "  5. 📋 Evaluates CEL rules: isFatal=true → cordon node"
    echo "  6. 🔒 Node will be cordoned AUTOMATICALLY"
    echo ""
    
    success "GPU error injection complete!"
    echo ""
    echo "Next step: Run './scripts/03-verify-cordon.sh' to verify the node was cordoned"
    echo "  (The script will poll for cordon status with retries)"
    echo ""
}

main() {
    section "GPU Error Injection"
    
    check_cluster
    find_target_node
    check_node_exists
    inject_gpu_error
}

main "$@"

