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

#!/usr/bin/env bash

# This script collects diagnostic information for NVSentinel.
# Create main directory with timestamp
MAIN_DIR="nvsentinel-diagnostics-$(date +%Y%m%d-%H%M%S)"
LOG_DIR="${MAIN_DIR}/logs"
RESOURCE_DIR="${MAIN_DIR}/resources"
NODE_DIR="${MAIN_DIR}/nodes"

# Create directory structure
mkdir -p "${LOG_DIR}" "${RESOURCE_DIR}" "${NODE_DIR}"

# Function to execute a command and handle errors
execute_command() {
    if ! "$@"; then
        echo "Error executing: $*" >&2
    fi
}

# 1. Collect pod logs with individual files
echo "Exporting pod logs..."
kubectl get pods -n nvsentinel -o name 2>/dev/null | while read -r pod; do
    filename="${LOG_DIR}/${pod#pod/}.log"
    echo "===== Logs for ${pod} =====" > "${filename}"
    execute_command kubectl logs -n nvsentinel --all-containers=true "${pod}" >> "${filename}" 2>&1
done

# 2. Export namespace resources with separate files
echo "Exporting namespace resources..."
resources=(pods deployments daemonsets replicasets configmaps)
for resource in "${resources[@]}"; do
    filename="${RESOURCE_DIR}/${resource}.txt"
    execute_command kubectl describe -n nvsentinel "${resource}"  > "${filename}" 2>&1
done

# 3. Describe all cluster nodes
echo "Exporting node details..."
kubectl get nodes -o name 2>/dev/null | while read -r node; do
    filename="${NODE_DIR}/${node#node/}.txt"
    execute_command kubectl describe "${node}" > "${filename}" 2>&1
done

echo "NVSentinel Diagnostics saved to: ${MAIN_DIR}"