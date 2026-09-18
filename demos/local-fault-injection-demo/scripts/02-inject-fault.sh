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

# Step 2: break the GPU.
#
# XID 95 is an uncontained ECC error. DCGM reports it as
# DCGM_FR_UNCONTAINED_ERROR, which NVSentinel classes as fatal with a
# recommended action of RESTART_VM. See README.md.
XID_FIELD=230
XID_VALUE=95

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

main() {
    require_tools kubectl kind jq
    require_cluster
    NODE="$(gpu_node)"

    section "Injecting XID 95 into the GPU on $NODE"

    if is_cordoned "$NODE"; then
        warn "$NODE is already cordoned. Run ./demo.sh cleanup and start again for the full sequence."
        echo
    fi

    local pod
    pod="$(kubectl get pods -n "$DCGM_NAMESPACE" -l app=nvidia-dcgm -o json |
        jq -r --arg node "$NODE" '.items[] | select(.spec.nodeName == $node) | .metadata.name' | head -1)"
    [[ -n "$pod" ]] || fail "No nvidia-dcgm pod on $NODE. Run ./scripts/00-setup.sh first."

    log "Writing it into the hostengine in ${DCGM_NAMESPACE}/${pod}. Nothing downstream is told."
    echo

    kubectl exec -n "$DCGM_NAMESPACE" "$pod" -- \
        dcgmi test --inject --gpuid 0 -f "$XID_FIELD" -v "$XID_VALUE"

    echo
    success "GPU 0 on $NODE is now reporting XID 95"

    next_step "./scripts/03-watch-remediation.sh   watch NVSentinel handle it"
}

main "$@"
