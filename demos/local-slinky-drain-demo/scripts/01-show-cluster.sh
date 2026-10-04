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

# Step 1: the baseline the later steps are read against — a healthy node, the
# workload on it, and both drain plugins waiting for something to do.

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# Names the GPU health checks currently reporting a fault, if any. NVSentinel
# keeps one node condition per check, False while the check passes.
failing_checks() {
    kubectl get node "$NODE" -o json |
        jq -r '[.status.conditions[]
                | select(.type | startswith("Gpu"))
                | select(.status == "True")
                | .type] | join(", ")'
}

main() {
    require_tools kubectl kind jq
    require_cluster
    NODE="$(gpu_node)"

    section "Before the fault"

    kubectl get nodes
    echo
    kubectl get pods -n "$SLINKY_NAMESPACE" -o wide
    echo

    log "The two controllers that will carry out the drain:"
    kubectl get deployments -n "$NAMESPACE" slinky-drainer mock-slurm-operator
    echo

    log "No DrainRequest exists yet; node-drainer creates one when a fault arrives."
    kubectl get drainrequests -n "$NAMESPACE" 2>/dev/null || true
    echo

    local failing
    failing="$(failing_checks)"

    if is_cordoned "$NODE"; then
        warn "$NODE is already cordoned. Run ./scripts/99-cleanup.sh and start again for a clean demo."
    elif [[ -n "$failing" ]]; then
        warn "$NODE is already reporting GPU faults: ${failing}"
    else
        success "$NODE is healthy and schedulable, with the workload running on it"
    fi

    next_step "./scripts/02-inject-fault.sh   break the GPU"
}

main "$@"
