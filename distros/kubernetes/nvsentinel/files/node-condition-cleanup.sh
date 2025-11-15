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

if [ -z "$DEPRECATED_CONDITIONS" ]; then
  echo "No deprecated conditions configured"
  exit 0
fi

IFS=',' read -ra conditions <<< "$DEPRECATED_CONDITIONS"
echo "Node Condition Cleanup: Removing ${conditions[*]}"

nodes=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
node_count=$(echo "$nodes" | wc -w | tr -d ' ')
echo "Processing $node_count nodes (up to 5 in parallel)..."

process_node() {
  local node=$1
  local -r MAX_RETRIES=3
  local -r BASE_DELAY=0.1
  local retry=0
  
  while true; do
    # Fetch current conditions (refetch on each retry to resolve conflicts)
    local current_conditions
    current_conditions=$(kubectl get node "$node" -o json 2>/dev/null | jq -c '.status.conditions // []' 2>/dev/null || echo "[]")
    
    if [ "$current_conditions" = "[]" ]; then
      if [ $retry -eq 0 ]; then
        echo "- $node: no conditions found"
      fi
      return 0
    fi
    
    # Check if any deprecated conditions exist and filter them out
    local filtered_conditions="$current_conditions"
    local found_conditions=()
    
    for condition in "${conditions[@]}"; do
      # Check if this condition exists using jq -e (exit code based)
      if echo "$current_conditions" | jq -e --arg type "$condition" 'any(.[]; .type == $type)' >/dev/null 2>&1; then
        found_conditions+=("$condition")
        filtered_conditions=$(echo "$filtered_conditions" | jq --arg type "$condition" '[.[] | select(.type != $type)]' 2>/dev/null || echo "$filtered_conditions")
      fi
    done
    
    if [ ${#found_conditions[@]} -eq 0 ]; then
      return 0
    fi
    
    # Attempt to patch the node with the freshly computed filtered conditions
    local patch_output
    if patch_output=$(kubectl patch node "$node" --type=json \
      -p="[{\"op\":\"replace\",\"path\":\"/status/conditions\",\"value\":$filtered_conditions}]" \
      --subresource=status 2>&1); then
      echo "✓ $node: removed ${#found_conditions[@]} condition(s): ${found_conditions[*]}"
      return 0
    fi
    
    # Check if this is a conflict error that warrants retry
    if echo "$patch_output" | grep -qi "conflict"; then
      retry=$((retry + 1))
      
      if [ $retry -lt $MAX_RETRIES ]; then
        local delay=$(awk "BEGIN {print $BASE_DELAY * (2 ^ ($retry - 1))}")
        sleep "$delay"
        continue
      fi
      
      echo "⚠ $node: conflict persists after $MAX_RETRIES attempts - $patch_output"
      return 1
    fi
    
    # Non-retriable error
    echo "⚠ $node: patch failed - $patch_output"
    return 1
  done
}

active_jobs=0
max_parallel=5
processed=0

for node in $nodes; do
  process_node "$node" &
  active_jobs=$((active_jobs + 1))
  processed=$((processed + 1))
  
  if [ $active_jobs -ge $max_parallel ]; then
    wait -n 2>/dev/null || true
    active_jobs=$((active_jobs - 1))
  fi
  
  if [ $((processed % 10)) -eq 0 ]; then
    echo "[$processed/$node_count nodes started]"
  fi
done

wait
echo "Cleanup completed for $node_count nodes"
