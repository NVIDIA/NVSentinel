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

NUM_GPU_NODES=${NUM_GPU_NODES:-50}
NUM_KATA_TEST_NODES=${NUM_KATA_TEST_NODES:-5}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODE_TEMPLATE="$SCRIPT_DIR/kwok-node-template.yaml"
KATA_NODE_TEMPLATE="$SCRIPT_DIR/kwok-kata-test-node-template.yaml"

echo "Creating $NUM_GPU_NODES regular KWOK GPU nodes..."
for i in $(seq 0 $((NUM_GPU_NODES - 1))); do
    sed "s/PLACEHOLDER/$i/g" "$NODE_TEMPLATE" | kubectl apply -f - >/dev/null 2>&1 || true
done
echo "Created $NUM_GPU_NODES regular KWOK GPU nodes"

echo "Creating $NUM_KATA_TEST_NODES KWOK Kata test nodes..."
for i in $(seq 0 $((NUM_KATA_TEST_NODES - 1))); do
    sed "s/PLACEHOLDER/$i/g" "$KATA_NODE_TEMPLATE" | kubectl apply -f - >/dev/null 2>&1 || true
done
echo "Created $NUM_KATA_TEST_NODES KWOK Kata test nodes"

echo "Total KWOK nodes created: $((NUM_GPU_NODES + NUM_KATA_TEST_NODES))"
