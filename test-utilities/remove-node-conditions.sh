# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

#script to remove node conditions from all nodes in a cluster using bash script
#!/bin/bash
echo "Deleting node conditions..."
nodeConditions=("XidError" "GpuDbeMsbeProblem" "GpuHWSlowDown")
nodes=$(kubectl get nodes -o name)
for node in $nodes; do
    for condition in "${nodeConditions[@]}"; do
        conditionIndex=$(kubectl get $node -o json | jq --arg condition "$condition" '.status.conditions | map(.type == $condition) | index(true)')
        if [[ $conditionIndex != "null" ]]; then
            kubectl patch $node --type='json' -p="[{\"op\": \"remove\", \"path\": \"/status/conditions/$conditionIndex\"}]" --subresource=status
            if [[ $? -eq 0 ]]; then
                echo "Removed $condition from $node"
            else
                echo "Failed to remove $condition from $node"
            fi
        else
            echo "$condition condition not found on $node, nothing to delete."
        fi
    done
done