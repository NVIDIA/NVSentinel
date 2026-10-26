#!/bin/bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

# Sets the InternalIP that kwok gives to every fake node.
#
# The kwok chart starts kwok with --node-ip=$(POD_IP). kindnetd adds a route to the podCIDR of each
# node through the InternalIP of that node. The other kind nodes cannot reach a pod IP directly, so
# the route fails, and kindnetd restarts in a loop after it tries again for some seconds. Each restart
# can drop connections that pass through the node.
#
# This script uses the last address of the kind Docker network instead. Every kind node can reach it
# directly, and Docker gives addresses from the start of the network, so no container has it. Routes
# to the fake podCIDRs go to an address that does not answer, which is correct for fake nodes.
#
# Set KWOK_NODE_IP to use a different address. Run this script before you create the fake nodes:
# kwok sets the InternalIP only when it initializes a node.

set -euo pipefail

NAMESPACE=kube-system
DEPLOYMENT=kwok-controller
KIND_NETWORK=${KIND_EXPERIMENTAL_DOCKER_NETWORK:-kind}

last_address_in_network() {
    local subnet a b c d prefix address size

    subnet=$(docker network inspect "$KIND_NETWORK" \
        --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}' |
        grep -m1 -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$') || {
        echo "Error: no IPv4 subnet found on Docker network $KIND_NETWORK" >&2
        return 1
    }

    IFS=./ read -r a b c d prefix <<< "$subnet"
    if [ "$prefix" -gt 30 ]; then
        echo "Error: Docker network $KIND_NETWORK subnet $subnet is too small" >&2
        return 1
    fi

    size=$((1 << (32 - prefix)))
    address=$(( ((a << 24) | (b << 16) | (c << 8) | d) & ~(size - 1) & 0xFFFFFFFF ))
    address=$((address + size - 2))

    echo "$(( (address >> 24) & 255 )).$(( (address >> 16) & 255 )).$(( (address >> 8) & 255 )).$(( address & 255 ))"
}

node_ip=${KWOK_NODE_IP:-$(last_address_in_network)}

args=$(kubectl -n "$NAMESPACE" get deployment "$DEPLOYMENT" \
    -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}')
index=$(printf '%s\n' "$args" | awk '/^--node-ip=/ { print NR - 1; exit }')

if [ -z "$index" ]; then
    echo "Error: $DEPLOYMENT has no --node-ip argument; the kwok chart changed" >&2
    exit 1
fi

if printf '%s\n' "$args" | grep -qxF -- "--node-ip=$node_ip"; then
    echo "kwok already uses node IP $node_ip"
    exit 0
fi

kubectl -n "$NAMESPACE" patch deployment "$DEPLOYMENT" --type=json \
    -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args/$index\", \"value\": \"--node-ip=$node_ip\"}]"
kubectl -n "$NAMESPACE" rollout status deployment "$DEPLOYMENT" --timeout=120s

echo "kwok uses node IP $node_ip"
