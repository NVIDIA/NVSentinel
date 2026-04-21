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

check_cluster() {
    if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
        log "❌ Cluster '$CLUSTER_NAME' not found. Run 'make setup' first."
        exit 1
    fi
    kubectl config use-context "kind-${CLUSTER_NAME}" > /dev/null 2>&1
}

deploy_memory_hog() {
    section "Deploying Memory Hog Pod"

    local worker="${CLUSTER_NAME}-worker"

    log "About to deploy a memory-hungry pod on the worker node."
    echo ""
    echo "  📍 Target node: $worker"
    echo "  🧠 Memory allocation: 300MB (stress --vm-bytes 300M)"
    echo "  📊 Memory limit: 350Mi"
    echo "  🏷️  Label: nvsentinel.nvidia.com/memory-hog=true"
    echo ""

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: memory-hog
  namespace: default
  labels:
    nvsentinel.nvidia.com/memory-hog: "true"
spec:
  nodeName: $worker
  containers:
  - name: stress
    image: polinux/stress
    command: ["stress"]
    args: ["--vm", "1", "--vm-bytes", "300M", "--vm-hang", "0"]
    resources:
      requests:
        memory: "300Mi"
      limits:
        memory: "350Mi"
  restartPolicy: Never
EOF

    log "Waiting for memory-hog pod to start..."
    kubectl wait --for=condition=ready --timeout=120s \
        pod/memory-hog -n default 2>/dev/null || {
            log "⧗ Pod may still be pulling image, continuing..."
        }

    log "✓ Memory hog pod deployed"
}

explain_flow() {
    section "What Happens Next"

    cat <<EOF
The memory hog pod is now consuming memory on the worker node.
The automated remediation flow is:

  1. 📡 Memory Pressure Monitor detects low available memory
     (polls /proc/meminfo, threshold: 400 MB)

  2. 📨 Sends health event to Platform Connectors via gRPC
     (recommendedAction: CUSTOM, customAction: RECLAIM_MEMORY)

  3. 📊 Platform Connectors store event in MongoDB

  4. 🔒 Fault Quarantine cordons the node

  5. 🔧 Fault Remediation creates a MemoryReclaim CR
     (from the RECLAIM_MEMORY action template)

  6. 🧹 Memory Reclaim Controller detects the CR
     (watches for MemoryReclaim CRs at demo.nvsentinel.nvidia.com)

  7. 🗑️  Deletes memory-hog pods on the affected node

  8. ✅ Updates CR status with MemoryReclaimed=True

This process takes 30-60 seconds depending on polling intervals.

To verify the remediation completed:
  ./scripts/03-verify-remediation.sh

To watch in real-time:
  kubectl logs -f -n $NAMESPACE -l app=memory-pressure-monitor
  kubectl logs -f -n $NAMESPACE deployment/memory-reclaim-controller

EOF
}

main() {
    check_cluster
    deploy_memory_hog
    explain_flow
}

main "$@"
