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

# Display help information
show_help() {
    echo "Usage: $0 [options]"
    echo
    echo "This script collects diagnostic information for NVSentinel."
    echo
    echo "Options:"
    echo "  --help, -h                   Show this help message and exit"
    echo "  --all, -a                    Collect information from all GPU nodes"
    echo "  --nodelist node1 node2...    Space-separated list of specific node names to collect information from"
    echo
    echo "Examples:"
    echo "  $0 --all                     # Collect from all GPU nodes"
    echo "  $0 --nodelist worker1 worker2 # Collect from specific nodes 'worker1' and 'worker2'"
    echo
}

# Parse command line arguments
ALL_NODES=false
NODE_LIST=()
PARSING_NODELIST=false

# If no arguments provided, show help and exit
if [ $# -eq 0 ]; then
    show_help
    exit 0
fi

while [ $# -gt 0 ]; do
    case "$1" in
        --help|-h)
            show_help
            exit 0
            ;;
        --all|-a)
            ALL_NODES=true
            PARSING_NODELIST=false
            shift
            ;;
        --nodelist)
            PARSING_NODELIST=true
            shift
            ;;
        -*)
            echo "Error: Unknown option: $1" >&2
            show_help
            exit 1
            ;;
        *)
            if [ "$PARSING_NODELIST" = true ]; then
                NODE_LIST+=("$1")
            else
                echo "Error: Unknown argument: $1" >&2
                show_help
                exit 1
            fi
            shift
            ;;
    esac
done

# If neither --all nor specific nodes were provided, default to all nodes
if [ "$ALL_NODES" = false ] && [ ${#NODE_LIST[@]} -eq 0 ]; then
    ALL_NODES=true
fi

# This script collects diagnostic information for NVSentinel.
# Create main directory with timestamp
MAIN_DIR="nvsentinel-diagnostics-$(date +%Y%m%d-%H%M%S)"
LOG_DIR="${MAIN_DIR}/logs"
RESOURCE_DIR="${MAIN_DIR}/resources"
NODE_DIR="${MAIN_DIR}/nodes"
DMESG_DIR="${MAIN_DIR}/dmesg"
NVIDIA_SMI_DIR="${MAIN_DIR}/nvidia-smi-output"

# Create directory structure
mkdir -p "${LOG_DIR}" "${RESOURCE_DIR}" "${NODE_DIR}" "${DMESG_DIR}" "${NVIDIA_SMI_DIR}"

# Function to execute a command and handle errors
execute_command() {
    if ! "$@"; then
        echo "Error executing: $*" >&2
    fi
}

# Function to get list of nodes to process
get_nodes_to_process() {
    if [ "$ALL_NODES" = true ]; then
        kubectl get nodes -l nvidia.com/gpu.present=true -o name 2>/dev/null
    else
        for node in "${NODE_LIST[@]}"; do
            echo "node/${node}"
        done
    fi
}

# Function to display progress
display_progress() {
    local current=$1
    local total=$2
    local resource=$3
    local node=$4
    local percentage=$((current * 100 / total))
    
    printf "\r[%3d%%] Processing %d/%d: %s on %s                  " "$percentage" "$current" "$total" "$resource" "$node"
    if [ "$current" -eq "$total" ]; then
        printf "\r[100%%] Completed %s collection                                              \n" "$resource"
    fi
}

# Count total number of nodes to process for progress tracking
TOTAL_NODES=$(get_nodes_to_process | wc -l | tr -d ' ')
echo "Found ${TOTAL_NODES} nodes to process"

# 1. Collect pod logs with individual files
printf "\n=== [1/5] Exporting pod logs... ===\n"

# Only collect logs from pods on the specified nodes
if [ "$ALL_NODES" = true ]; then
    # If --all is specified, get all pods in nvsentinel namespace
    PODS=$(kubectl get pods -n nvsentinel -o name 2>/dev/null)
else
    # If specific nodes are specified, only get pods running on those nodes
    PODS=""
    for node in "${NODE_LIST[@]}"; do
        echo "Finding pods on node: $node"
        node_pods=$(kubectl get pods -n nvsentinel -o wide --field-selector spec.nodeName="$node" | grep -v "^NAME" | awk '{print "pod/"$1}')
        if [ -n "$node_pods" ]; then
            if [ -n "$PODS" ]; then
                PODS="${PODS}"$'\n'"${node_pods}"
            else
                PODS="${node_pods}"
            fi
        fi
    done
fi

TOTAL_PODS=$(echo "$PODS" | grep -v '^$' | wc -l | tr -d ' ')
CURRENT_POD=0

if [ "$TOTAL_PODS" -eq 0 ]; then
    echo "No pods found in nvsentinel namespace on the specified nodes"
else
    echo "Found $TOTAL_PODS pods to process"
    echo "$PODS" | grep -v '^$' | while read -r pod; do
        CURRENT_POD=$((CURRENT_POD + 1))
        pod_name=${pod#pod/}
        display_progress "$CURRENT_POD" "$TOTAL_PODS" "pod logs" "$pod_name"
        
        filename="${LOG_DIR}/${pod_name}.log"
        echo "===== Logs for ${pod} =====" > "${filename}"
        execute_command kubectl logs -n nvsentinel "${pod}" >> "${filename}" 2>&1
    done
fi

# 2. Export namespace resources with separate files
printf "\n=== [2/5] Exporting namespace resources... ===\n"
resources=(pods deployments daemonsets replicasets configmaps)
TOTAL_RESOURCES=${#resources[@]}
CURRENT_RESOURCE=0

for resource in "${resources[@]}"; do
    CURRENT_RESOURCE=$((CURRENT_RESOURCE + 1))
    display_progress "$CURRENT_RESOURCE" "$TOTAL_RESOURCES" "resource" "$resource"
    
    filename="${RESOURCE_DIR}/${resource}.txt"
    
    if [ "$resource" = "pods" ] && [ "$ALL_NODES" = false ]; then
        # For pods, if specific nodes are specified, only get resources on those nodes
        echo "===== ${resource} in nvsentinel namespace =====" > "${filename}"
        for node in "${NODE_LIST[@]}"; do
            printf "\n===== Pods on node: ${node} =====\n" >> "${filename}"
            # First get the pod names on this node
            node_pods=$(kubectl get pods -n nvsentinel -o name --field-selector spec.nodeName="$node" 2>/dev/null)
            if [ -n "$node_pods" ]; then
                # Then describe each pod
                echo "$node_pods" | while read -r pod; do
                    execute_command kubectl describe -n nvsentinel "$pod" >> "${filename}" 2>&1
                done
            else
                echo "No pods found on node ${node}" >> "${filename}"
            fi
        done
    else
        # For other resources or when --all is specified, get all resources in the namespace
        execute_command kubectl describe -n nvsentinel "${resource}" > "${filename}" 2>&1
    fi
done

# 3. Describe all cluster nodes or specific nodes
printf "\n=== [3/5] Exporting node details... ===\n"
NODES=$(get_nodes_to_process)
CURRENT_NODE=0

echo "$NODES" | grep -v '^$' | while read -r node; do
    CURRENT_NODE=$((CURRENT_NODE + 1))
    node_name=${node#node/}
    display_progress "$CURRENT_NODE" "$TOTAL_NODES" "node details" "$node_name"
    
    filename="${NODE_DIR}/${node_name}.txt"
    execute_command kubectl describe "${node}" > "${filename}" 2>&1
done

# 4. Collect dmesg logs from specified GPU nodes
printf "\n=== [4/5] Collecting dmesg logs from GPU nodes... ===\n"
CURRENT_NODE=0

get_nodes_to_process | grep -v '^$' | while read -r node; do
    CURRENT_NODE=$((CURRENT_NODE + 1))
    node_name=${node#node/}
    display_progress "$CURRENT_NODE" "$TOTAL_NODES" "dmesg logs" "$node_name"
    
    filename="${DMESG_DIR}/${node_name}_dmesg.log"
    
    # Find device-plugin pod on the node to get its image
    device_plugin_pod=$(kubectl get pods -n gpu-operator -o wide --field-selector spec.nodeName="${node_name}" | grep -i device-plugin | awk '{print $1}' | head -1)
    
    # Get the device plugin image
    device_plugin_image=$(kubectl get pod -n gpu-operator "$device_plugin_pod" -o jsonpath='{.spec.containers[0].image}')
    echo "Using device plugin image: $device_plugin_image for dmesg collection"
    
    # Use the device plugin image to collect dmesg
    execute_command kubectl exec -n gpu-operator "${device_plugin_pod}" -- chroot /host sh -c "dmesg --time-format=ctime" > "${filename}" 2>&1
done

# 5. Collect GPU information from specified GPU nodes
printf "\n=== [5/5] Collecting GPU information from GPU nodes... ===\n"
CURRENT_NODE=0

get_nodes_to_process | grep -v '^$' | while read -r node; do
    CURRENT_NODE=$((CURRENT_NODE + 1))
    node_name=${node#node/}
    display_progress "$CURRENT_NODE" "$TOTAL_NODES" "GPU info" "$node_name"
    
    filename="${NVIDIA_SMI_DIR}/nvidia-smi-${node_name}"
    
    # Find device-plugin pod on the node (primary choice)
    device_plugin_pod=$(kubectl get pods -n gpu-operator -o wide --field-selector spec.nodeName="${node_name}" | grep -i device-plugin | awk '{print $1}' | head -1)
    
    if [ -n "$device_plugin_pod" ]; then
        echo "Found nvidia-device-plugin pod: ${device_plugin_pod} on node: ${node_name}" >> "${filename}"
        # Collect GPU information
        execute_command kubectl exec -n gpu-operator "${device_plugin_pod}" -- nvidia-smi >> "${filename}" 2>&1
    else
        echo "No nvidia-device-plugin pod found on node: ${node_name}" >> "${filename}"
        echo "No suitable pod found on node: ${node_name} to collect GPU information." >> "${filename}"
    fi
done

# Create a summary file
printf "\n=== Creating summary report... ===\n"
SUMMARY_FILE="${MAIN_DIR}/summary.txt"

{
    printf "===== NVSentinel Diagnostics Summary =====\n"
    printf "Generated on: %s\n" "$(date)"
    printf "\n"
    printf "Nodes processed:\n"
    if [ "$ALL_NODES" = true ]; then
        printf "- All GPU nodes in the cluster\n"
    else
        for node in "${NODE_LIST[@]}"; do
            printf "- %s\n" "${node}"
        done
    fi
    printf "\n"
    printf "Collection statistics:\n"
    printf "- Pods logs collected: %d\n" "${TOTAL_PODS}"
    printf "- Resources collected: %d\n" "${TOTAL_RESOURCES}"
    printf "- Nodes processed: %d\n" "${TOTAL_NODES}"
    printf "\n"
    printf "Directories:\n"
    printf "- Pod logs: %s\n" "${LOG_DIR}"
    printf "- Resources: %s\n" "${RESOURCE_DIR}"
    printf "- Node details: %s\n" "${NODE_DIR}"
    printf "- Dmesg logs: %s\n" "${DMESG_DIR}"
    printf "- GPU information: %s\n" "${NVIDIA_SMI_DIR}"
} > "${SUMMARY_FILE}"

printf "\n=== Collection Complete ===\n"
echo "NVSentinel Diagnostics saved to: ${MAIN_DIR}"
echo "Summary report available at: ${SUMMARY_FILE}"

# Create a tarball of the diagnostics directory
printf "\n=== Creating tarball archive... ===\n"
TAR_FILE="${MAIN_DIR}.tar.gz"
tar -czf "${TAR_FILE}" "${MAIN_DIR}" 2>/dev/null
if [ $? -eq 0 ]; then
    echo "Diagnostics archive created: ${TAR_FILE}"
    # Calculate file size in a human-readable format
    if command -v du >/dev/null 2>&1; then
        ARCHIVE_SIZE=$(du -h "${TAR_FILE}" | awk '{print $1}')
        echo "Archive size: ${ARCHIVE_SIZE}"
    fi
else
    echo "Failed to create diagnostics archive"
fi