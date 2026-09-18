#!/usr/bin/env bash
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

# Deletes the demo cluster. Everything the demo creates lives inside it, so this
# is the whole cleanup.

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

main() {
    require_tools kind

    section "Cleaning up"

    if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
        log "Cluster '$CLUSTER_NAME' does not exist. Nothing to clean up."
        exit 0
    fi

    log "Deleting the KIND cluster '$CLUSTER_NAME'..."
    kind delete cluster --name "$CLUSTER_NAME"

    success "Cluster deleted"
    echo
    # Deliberately not `docker system prune -a --volumes`: that reaches across
    # every project on this Docker daemon and can delete images and volumes that
    # have nothing to do with the demo. Name the one image instead.
    log "The KIND node image stays cached for the next run. To reclaim that disk too:"
    log "  docker image rm kindest/node:v1.34.0"
    log "To run the demo again: ./demo.sh"
}

main "$@"
