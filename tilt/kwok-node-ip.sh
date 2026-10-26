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

# Prints the InternalIP that kwok gives to every fake node. The Tiltfile sets it as POD_IP, which
# the kwok chart passes to kwok as --node-ip.
#
# kindnetd adds a route to the podCIDR of each node through the InternalIP of that node. The other
# kind nodes cannot reach the kwok pod IP directly, so the route fails, and kindnetd restarts in a
# loop after it tries again for some seconds. Each restart can drop connections that pass through
# the node.
#
# This script prints the last address of the kind Docker network instead. Every kind node can reach
# it directly, and Docker gives addresses from the start of the network, so no container has it.
# Routes to the fake podCIDRs go to an address that does not answer, which is correct for fake nodes.
#
# Set KWOK_NODE_IP to use a different address.

set -euo pipefail

KIND_NETWORK=${KIND_EXPERIMENTAL_DOCKER_NETWORK:-kind}

if [ -n "${KWOK_NODE_IP:-}" ]; then
    echo "$KWOK_NODE_IP"
    exit 0
fi

subnet=$(docker network inspect "$KIND_NETWORK" \
    --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}' |
    grep -m1 -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$') || {
    echo "Error: no IPv4 subnet found on Docker network $KIND_NETWORK" >&2
    exit 1
}

IFS=./ read -r a b c d prefix <<< "$subnet"
if [ "$prefix" -gt 30 ]; then
    echo "Error: Docker network $KIND_NETWORK subnet $subnet is too small" >&2
    exit 1
fi

size=$((1 << (32 - prefix)))
address=$(( ((a << 24) | (b << 16) | (c << 8) | d) & ~(size - 1) & 0xFFFFFFFF ))
address=$((address + size - 2))
node_ip="$(( (address >> 24) & 255 )).$(( (address >> 16) & 255 )).$(( (address >> 8) & 255 )).$(( address & 255 ))"

# A custom network can use this address for its gateway or for a container with a fixed IP.
used=$(docker network inspect "$KIND_NETWORK" \
    --format '{{range .IPAM.Config}}{{.Gateway}}{{"\n"}}{{end}}{{range .Containers}}{{.IPv4Address}}{{"\n"}}{{end}}' |
    cut -d/ -f1)
if grep -qxF "$node_ip" <<< "$used"; then
    echo "Error: $node_ip is in use on Docker network $KIND_NETWORK; set KWOK_NODE_IP to an unused address" >&2
    exit 1
fi

echo "$node_ip"
