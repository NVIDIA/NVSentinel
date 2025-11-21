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

log() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

section() {
    echo ""
    echo "=========================================="
    echo "  $1"
    echo "=========================================="
    echo ""
}

cleanup() {
    section "Cleaning Up Demo Environment"
    
    # Check if cluster exists
    if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        warn "Cluster '$CLUSTER_NAME' not found. Nothing to clean up."
        return
    fi
    
    log "Deleting KIND cluster: $CLUSTER_NAME"
    
    kind delete cluster --name "$CLUSTER_NAME"
    
    success "Cluster deleted successfully"
    
    # Clean up any leftover port-forwards
    log "Cleaning up any orphaned port-forwards..."
    pkill -f "kubectl port-forward.*nvsentinel" 2>/dev/null || true
    
    # Clean up temp files
    if [ -f /tmp/nvsentinel-demo-values.yaml ]; then
        rm -f /tmp/nvsentinel-demo-values.yaml
        log "Removed temporary values file"
    fi
    
    section "Cleanup Complete! ✨"
    
    echo "The demo environment has been removed."
    echo ""
    echo "Resources cleaned up:"
    echo "  ✅ KIND cluster deleted"
    echo "  ✅ All containers removed"
    echo "  ✅ Temporary files deleted"
    echo ""
    echo "To run the demo again:"
    echo "  ./scripts/00-setup.sh"
    echo ""
    
    success "All clean!"
}

main() {
    cleanup
}

main "$@"

