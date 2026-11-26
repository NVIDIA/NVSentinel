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
    local current_conditions node_json
    # Only stdout is the node JSON; warnings and errors on stderr go straight to the job log.
    if ! node_json=$(kubectl get node "$node" -o json --ignore-not-found); then
      echo "⚠ $node: get failed"
      return 1
    fi
    # A node deleted since the listing has nothing left to clean up.
    if [ -z "$node_json" ]; then
      echo "- $node: deleted before cleanup, skipping"
      return 0
    fi
    if ! current_conditions=$(echo "$node_json" | jq -c '.status.conditions // []' 2>&1); then
      echo "⚠ $node: could not parse node JSON - $current_conditions"
      return 1
    fi

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
    
    # The node was deleted between the get and the patch: nothing left to clean up.
    if echo "$patch_output" | grep -q "^Error from server (NotFound):"; then
      echo "- $node: deleted before patch, skipping"
      return 0
    fi

    # Non-retriable error
    echo "⚠ $node: patch failed - $patch_output"
    return 1
  done
}

worker_pids=()
max_parallel=5
processed=0
failed=0

for node in $nodes; do
  process_node "$node" &
  worker_pids+=("$!")
  processed=$((processed + 1))
  
  if [ "${#worker_pids[@]}" -ge "$max_parallel" ]; then
    if ! wait "${worker_pids[0]}"; then
      failed=$((failed + 1))
    fi
    worker_pids=("${worker_pids[@]:1}")
  fi
  
  if [ $((processed % 10)) -eq 0 ]; then
    echo "[$processed/$node_count nodes started]"
  fi
done

# Waiting by PID retrieves the status even if the worker has already exited.
for pid in "${worker_pids[@]}"; do
  if ! wait "$pid"; then
    failed=$((failed + 1))
  fi
done

if [ $failed -gt 0 ]; then
  echo "Cleanup failed: $failed of $node_count nodes failed"
  exit 1
fi

echo "Cleanup completed for $node_count nodes"
