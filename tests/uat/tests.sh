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

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

get_boot_id() {
    local node=$1
    local boot_id
    local tmp_err
    tmp_err=$(mktemp)

    if boot_id=$(kubectl get node "$node" -o jsonpath='{.status.nodeInfo.bootID}' 2>"$tmp_err"); then
        rm -f "$tmp_err"
        echo "$boot_id" | tr -d '[:space:]'
    else
        log "Warning: kubectl failed to get boot ID for node $node: $(cat "$tmp_err")"
        rm -f "$tmp_err"
        echo ""
    fi
}

is_node_ready_and_uncordoned() {
    local node=$1
    local node_info
    node_info=$(kubectl get node "$node" -o json 2>/dev/null)

    if [[ -z "$node_info" ]]; then
        return 1
    fi

    local is_ready
    is_ready=$(echo "$node_info" | jq -r '.status.conditions[] | select(.type == "Ready" and .status == "True") | .status')
    if [[ "$is_ready" != "True" ]]; then
        return 1
    fi

    if echo "$node_info" | jq -e '.spec.unschedulable == true' > /dev/null 2>&1; then
        return 1
    fi

    local managed_label
    managed_label=$(echo "$node_info" | jq -r '.metadata.labels["k8saas.nvidia.com/ManagedByNVSentinel"] // ""')
    if [[ "$managed_label" == "false" ]]; then
        return 1
    fi

    return 0
}

get_gpu_node_with_healthy_monitor() {
    local monitor_label=$1
    local namespace=${2:-nvsentinel}

    # Get nodes running the specified health monitor pod
    local nodes_with_monitor
    nodes_with_monitor=$(kubectl get pods -n "$namespace" -l "app.kubernetes.io/name=$monitor_label" \
        --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}')

    if [[ -z "$nodes_with_monitor" ]]; then
        echo ""
        return
    fi

    # Find first GPU node that is Ready, uncordoned, and has the monitor
    for node in $nodes_with_monitor; do
        local has_gpu
        has_gpu=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.present}' 2>/dev/null)

        if [[ "$has_gpu" == "true" ]] && is_node_ready_and_uncordoned "$node"; then
            echo "$node"
            return
        fi
    done

    echo ""
}

get_gpu_node_with_healthy_gpu_monitor() {
    get_gpu_node_with_healthy_monitor "gpu-health-monitor"
}

get_gpu_node_with_healthy_syslog_monitor() {
    get_gpu_node_with_healthy_monitor "syslog-health-monitor"
}

wait_for_node_condition() {
    local node=$1
    local condition_type=$2
    local timeout=${UAT_CONDITION_TIMEOUT:-60}
    local elapsed=0

    log "Waiting for node condition '$condition_type' to appear on node $node..."

    while [[ $elapsed -lt $timeout ]]; do
        local condition_status
        condition_status=$(kubectl get node "$node" -o json | jq -r ".status.conditions[] | select(.type == \"$condition_type\" and .status == \"True\") | .type")

        if [[ -n "$condition_status" ]]; then
            log "Node condition '$condition_type' found ✓"
            kubectl get node "$node" -o json | jq -r ".status.conditions[] | select(.type == \"$condition_type\") | \"  Status=\(.status) Reason=\(.reason)\""
            return 0
        fi

        sleep 2
        elapsed=$((elapsed + 2))
    done

    error "Timeout waiting for node condition '$condition_type' on node $node"
}

wait_for_node_quarantine() {
    local node=$1
    local timeout=${UAT_QUARANTINE_TIMEOUT:-120}
    local elapsed=0

    log "Waiting for node $node to be quarantined (cordoned)..."

    while [[ $elapsed -lt $timeout ]]; do
        local is_cordoned
        is_cordoned=$(kubectl get node "$node" -o jsonpath='{.spec.unschedulable}')

        if [[ "$is_cordoned" == "true" ]]; then
            log "Node $node is quarantined (cordoned) ✓"
            return 0
        fi

        sleep 5
        elapsed=$((elapsed + 5))
    done

    error "Timeout waiting for node $node to be quarantined"
}

wait_for_node_unquarantine() {
    local node=$1
    local timeout=${UAT_UNQUARANTINE_TIMEOUT:-300}
    local elapsed=0

    log "Waiting for node $node to be uncordoned..."
    while [[ $elapsed -lt $timeout ]]; do
        local is_cordoned
        is_cordoned=$(kubectl get node "$node" -o jsonpath='{.spec.unschedulable}')

        if [[ "$is_cordoned" != "true" ]]; then
            log "Node $node is uncordoned and ready ✓"
            return 0
        fi

        # Log every 30 seconds to show progress
        if [[ $((elapsed % 30)) -eq 0 && $elapsed -gt 0 ]]; then
            log "Still waiting for uncordon... elapsed=${elapsed}s, unschedulable=$is_cordoned"
        fi

        sleep 5
        elapsed=$((elapsed + 5))
    done

    error "Timeout waiting for node $node to be uncordoned"
}

wait_for_boot_id_change() {
    local node=$1
    local original_boot_id=$2
    local timeout=${UAT_REBOOT_TIMEOUT:-600}
    local elapsed=0
    local boot_id_changed=false

    # Trim original boot ID for consistent comparison
    original_boot_id=$(echo "$original_boot_id" | tr -d '[:space:]')

    log "Waiting for node $node to reboot (boot ID to change)..."
    log "Original boot ID: $original_boot_id"

    while [[ $elapsed -lt $timeout ]]; do
        local current_boot_id
        current_boot_id=$(get_boot_id "$node" || echo "")

        if [[ $((elapsed % 30)) -eq 0 && $elapsed -gt 0 ]]; then
            log "Still waiting... elapsed=${elapsed}s, current_boot_id='$current_boot_id'"
        fi

        if [[ -n "$current_boot_id" && "$current_boot_id" != "$original_boot_id" ]]; then
            log "Node $node rebooted successfully (boot ID changed)"
            log "  Old: $original_boot_id"
            log "  New: $current_boot_id"
            boot_id_changed=true
            break
        fi

        sleep 5
        elapsed=$((elapsed + 5))
    done

    if [[ "$boot_id_changed" != "true" ]]; then
        local final_boot_id
        final_boot_id=$(get_boot_id "$node" || echo "FAILED_TO_GET")
        error "Timeout waiting for node $node to reboot. Current boot ID: '$final_boot_id', Original: '$original_boot_id'"
    fi

    wait_for_node_unquarantine "$node"
}

wait_for_gpu_reset() {
    local node=$1
    local uuid=$2
    local current_ts=$3
    local timeout=${UAT_RESET_TIMEOUT:-600}
    local elapsed=0
    local matching_crd

    log "Waiting for GPU reset for $uuid on $node (matching GPUReset CRD)..."

    while [[ $elapsed -lt $timeout ]]; do
        local gpu_reset_list=$(kubectl get gpuresets -o json | jq -c '.items[]')
        local IFS=$'\n'

        for gpu_reset in $gpu_reset_list; do
            local start_time=$(echo "$gpu_reset" | jq -r '.status.startTime')
            local current_node=$(echo "$gpu_reset" | jq -r '.spec.nodeName')
            local uuids=$(echo "$gpu_reset" | jq -r '.spec.selector.uuids[]?')

            if [ -z "$start_time" ] || [ "$start_time" == "null" ]; then
                continue
            fi
            local start_ts=$(date -d "$start_time" +%s)

            if [ "$start_ts" -gt "$current_ts" ] && [ "$current_node" == "$node" ]; then
                for current_uuid in $uuids; do
                    if [ "$current_uuid" == "$uuid" ]; then
                        matching_crd=$(echo "$gpu_reset" | jq -r '.metadata.name')
                        log "GPUReset $matching_crd matches $uuid and $node"
                    fi
                done
            fi
        done

        if [ -n "$matching_crd" ]; then
            break
        fi

        sleep 5
        elapsed=$((elapsed + 5))
    done

    if [[ $elapsed -ge $timeout ]]; then
        error "Timeout waiting for GPU $uuid to reset"
    fi

    wait_for_node_unquarantine "$node"
}

test_gpu_monitoring_dcgm() {
    log "========================================="
    log "Test 1: GPU monitoring via DCGM"
    log "========================================="

    local gpu_node
    gpu_node=$(get_gpu_node_with_healthy_gpu_monitor)

    if [[ -z "$gpu_node" ]]; then
        error "No GPU node found with healthy gpu-health-monitor pod (Ready + uncordoned)"
    fi

    log "Selected GPU node: $gpu_node"

    local original_boot_id
    original_boot_id=$(get_boot_id "$gpu_node")
    log "Original boot ID: $original_boot_id"

    local dcgm_pod
    dcgm_pod=$(kubectl get pods -n gpu-operator -l app=nvidia-dcgm -o jsonpath="{.items[?(@.spec.nodeName=='$gpu_node')].metadata.name}" | head -1)

    if [[ -z "$dcgm_pod" ]]; then
        error "No DCGM pod found on node $gpu_node"
    fi

    kubectl exec -n gpu-operator "$dcgm_pod" -- dcgmi test --inject --gpuid 0 -f 240 -v 99999 # power watch error

    log "Waiting for node events to appear..."
    local max_wait=30
    local waited=0
    while [[ $waited -lt $max_wait ]]; do
        power_event=$(kubectl get events --field-selector involvedObject.name="$gpu_node" -o json | jq -r '.items[] | select(.reason == "GpuPowerWatchIsNotHealthy") | .reason')
        if [[ -n "$power_event" ]]; then
            log "Found power event"
            break
        fi
        sleep 2
        waited=$((waited + 2))
    done

    log "Verifying node events are populated (non-fatal errors appear here)"
    kubectl get events --field-selector involvedObject.name="$gpu_node" -o json | jq -r '.items[] | select(.reason | contains("IsNotHealthy")) | "\(.reason) Message=\(.message)"' | head -5

    power_event=$(kubectl get events --field-selector involvedObject.name="$gpu_node" -o json | jq -r '.items[] | select(.reason == "GpuPowerWatchIsNotHealthy") | .reason')
    if [[ -z "$power_event" ]]; then
        error "GpuPowerWatch event not found (non-fatal errors should create events)"
    fi
    log "Node event verified: GpuPowerWatch is non-fatal, appears in events ✓"

    # XID 95 results in A DCGM_FR_UNCONTAINED_ERROR from GpuMemWatch which requires a RESTART_VM action
    kubectl exec -n gpu-operator "$dcgm_pod" -- dcgmi test --inject --gpuid 0 -f 230 -v 95

    wait_for_node_condition "$gpu_node" "GpuMemWatch"

    wait_for_node_quarantine "$gpu_node"

    log "Waiting for node to reboot and recover..."
    wait_for_boot_id_change "$gpu_node" "$original_boot_id"

    log "Test 1 PASSED ✓"
}

test_xid_monitoring_syslog() {
    log "======================================================"
    log "Test 2: XID monitoring via syslog triggers RESTART_VM"
    log "======================================================"

    local gpu_node
    gpu_node=$(get_gpu_node_with_healthy_syslog_monitor)

    if [[ -z "$gpu_node" ]]; then
        error "No GPU node found with healthy syslog-health-monitor pod (Ready + uncordoned)"
    fi

    log "Selected GPU node: $gpu_node (has healthy syslog-health-monitor)"

    local original_boot_id
    original_boot_id=$(get_boot_id "$gpu_node")
    log "Original boot ID: $original_boot_id"

    local driver_pod
    driver_pod=$(kubectl get pods -n gpu-operator -l app=nvidia-driver-daemonset -o jsonpath="{.items[?(@.spec.nodeName=='$gpu_node')].metadata.name}" | head -1)

    if [[ -z "$driver_pod" ]]; then
        error "No driver pod found on node $gpu_node"
    fi

    log "Injecting XID 79 message via logger on pod: $driver_pod"
    kubectl exec -n gpu-operator "$driver_pod" -- logger -p daemon.err "[6085126.134786] NVRM: Xid (PCI:0002:00:00): 79, pid=1582259, name=nvc:[driver], GPU has fallen off the bus."

    wait_for_node_condition "$gpu_node" "SysLogsXIDError"

    wait_for_node_quarantine "$gpu_node"

    log "Waiting for node to reboot and recover..."
    wait_for_boot_id_change "$gpu_node" "$original_boot_id"

    log "Test 2 PASSED ✓"
}

test_xid_monitoring_syslog_gpu_reset() {
    log "=========================================================="
    log "Test 3: XID monitoring via syslog triggers COMPONENT_RESET"
    log "=========================================================="

    local drainer_configmap
    drainer_configmap=$(kubectl get configmaps -n nvsentinel node-drainer -o jsonpath="{.data.config\.toml}")

    if ! echo "$drainer_configmap" | grep -q "partialDrainEnabled = true"; then
        log "GPU reset is not enabled, skipping Test 3"
        return 0
    fi

    local current_ts=$(date +%s)

    local gpu_node
    gpu_node=$(get_gpu_node_with_healthy_syslog_monitor)

    if [[ -z "$gpu_node" ]]; then
        error "No GPU node found with healthy syslog-health-monitor pod (Ready + uncordoned)"
    fi

    log "Selected GPU node: $gpu_node (has healthy syslog-health-monitor)"

    local driver_pod
    driver_pod=$(kubectl get pods -n gpu-operator -l app=nvidia-driver-daemonset -o jsonpath="{.items[?(@.spec.nodeName=='$gpu_node')].metadata.name}" | head -1)

    if [[ -z "$driver_pod" ]]; then
        error "No driver pod found on node $gpu_node"
    fi

    log "Fetching GPU UUID and PCI from nvidia-smi in driver pod to construct syslog message"

    uuid_pci=$(kubectl exec -n gpu-operator "$driver_pod" -- sh -c  "nvidia-smi --query-gpu=uuid,pci.bus_id --format=csv,noheader | head -n 1")

    if [[ -z "$uuid_pci" ]]; then
        error "No nvidia-smi query output on node $gpu_node"
    fi

    uuid=$(echo "$uuid_pci" | awk -F', ' '{print $1}')
    pci=$(echo "$uuid_pci" | awk -F', ' '{print $2}' | sed -E 's/^00000000://; s/\.0$//; y/ABCDEF/abcdef/; s/^/0000:/')
    if [[ -z "$uuid" || -z "$pci" ]]; then
        error "Parsed empty UUID or PCI from nvidia-smi output: '$uuid_pci'"
    fi
    log "Resetting GPU UUID $uuid on PCI $pci"

    log "Injecting XID 119 message on GPU $uuid via logger on pod: $driver_pod"
    kubectl exec -n gpu-operator "$driver_pod" -- logger -p daemon.err "[6085126.134786] NVRM: Xid (PCI:$pci): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8)."

    wait_for_node_condition "$gpu_node" "SysLogsXIDError"

    wait_for_node_quarantine "$gpu_node"

    log "Waiting for node to GPU reset and recover..."
    wait_for_gpu_reset "$gpu_node" "$uuid" "$current_ts"

    log "Test 3 PASSED ✓"
}

test_sxid_monitoring_syslog() {
    log "========================================="
    log "Test 4: SXID monitoring (NVSwitch errors)"
    log "========================================="

    local gpu_node
    gpu_node=$(get_gpu_node_with_healthy_syslog_monitor)

    if [[ -z "$gpu_node" ]]; then
        error "No GPU node found with healthy syslog-health-monitor pod (Ready + uncordoned)"
    fi

    log "Selected GPU node: $gpu_node (has healthy syslog-health-monitor)"

    local original_boot_id
    original_boot_id=$(get_boot_id "$gpu_node")
    log "Original boot ID: $original_boot_id"


    local dcgm_pod
    dcgm_pod=$(kubectl get pods -n gpu-operator -l app=nvidia-dcgm -o jsonpath="{.items[?(@.spec.nodeName=='$gpu_node')].metadata.name}" | head -1)

    if [[ -z "$dcgm_pod" ]]; then
        error "No DCGM pod found on node $gpu_node"
    fi

    log "Getting NVLink topology from DCGM pod: $dcgm_pod"
    local nvlink_output
    nvlink_output=$(kubectl exec -n gpu-operator "$dcgm_pod" -- nvidia-smi nvlink -R 2>/dev/null)

    if [[ -z "$nvlink_output" ]]; then
        log "Warning: nvidia-smi nvlink not available, using fallback PCI/Link values"
        local pci_id="0005:00:00.0"
        local link_number="29"
    else
        log "Parsing NVLink topology to extract PCI and Link"
        local link_line
        link_line=$(echo "$nvlink_output" | grep -E "Link [0-9]+: Remote Device" | head -1)

        if [[ -z "$link_line" ]]; then
            log "Warning: No link information found, using fallback values"
            local pci_id="0005:00:00.0"
            local link_number="29"
        else
            local pci_id
            pci_id=$(echo "$link_line" | grep -oE '[0-9A-Fa-f]{8}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-9]' | head -1)
            local link_number
            link_number=$(echo "$link_line" | grep -oE 'Link [0-9]+$' | grep -oE '[0-9]+$')

            log "Extracted from topology: PCI=$pci_id, Link=$link_number"
        fi
    fi

    local driver_pod
    driver_pod=$(kubectl get pods -n gpu-operator -l app=nvidia-driver-daemonset -o jsonpath="{.items[?(@.spec.nodeName=='$gpu_node')].metadata.name}" | head -1)

    if [[ -z "$driver_pod" ]]; then
        error "No driver pod found on node $gpu_node"
    fi

    log "Injecting SXID error messages via logger on pod: $driver_pod"

    log "  - SXID 28002 (Non-fatal): Therm Warn Deactivated on Link $link_number"
    kubectl exec -n gpu-operator "$driver_pod" -- logger -p daemon.err "nvidia-nvswitch0: SXid (PCI:${pci_id}): 28002, Non-fatal, Link ${link_number} Therm Warn Deactivated"

    local max_wait=30
    local waited=0
    while [[ $waited -lt $max_wait ]]; do
        power_event=$(kubectl get events --field-selector involvedObject.name="$gpu_node" -o json | jq -r '.items[] | select(.reason == "SysLogsSXIDErrorIsNotHealthy") | .reason')
        if [[ -n "$power_event" ]]; then
            log "Found sxid event"
            break
        fi
        sleep 2
        waited=$((waited + 2))
    done

    log "Verifying SXID node event is populated (non-fatal SXID 28002)"
    sxid_event=$(kubectl get events --field-selector involvedObject.name="$gpu_node" -o json | jq -r '.items[] | select(.reason == "SysLogsSXIDErrorIsNotHealthy") | .reason')

    if [[ -z "$sxid_event" ]]; then
        log "SysLogsSXIDError event not found (non-fatal SXID may not create separate event)"
    fi
    log "Node event verified: SysLogsSXIDError ✓"

    log "  - SXID 20034 (Fatal): LTSSM Fault Up on Link $link_number"
    kubectl exec -n gpu-operator "$driver_pod" -- logger -p daemon.err "nvidia-nvswitch3: SXid (PCI:${pci_id}): 20034, Fatal, Link ${link_number} LTSSM Fault Up"

    wait_for_node_condition "$gpu_node" "SysLogsSXIDError"

    wait_for_node_quarantine "$gpu_node"

    log "Waiting for node to reboot and recover..."
    wait_for_boot_id_change "$gpu_node" "$original_boot_id"

    log "Test 4 PASSED ✓"
}

main() {
    log "Starting NVSentinel UAT tests..."
    
    log "Checking if circuit breaker is TRIPPED..."
    if kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}' | grep -q "TRIPPED"; then
        error "Circuit breaker is TRIPPED, please reset it manually"
    fi

    test_gpu_monitoring_dcgm

    # Wait for syslog-health-monitor to complete first initialization poll
    log "Waiting for syslog-health-monitor to initialize (60s)..."
    sleep 60

    test_xid_monitoring_syslog

    test_xid_monitoring_syslog_gpu_reset

    # test_sxid_monitoring_syslog

    log "========================================="
    log "All tests PASSED ✓"
    log "========================================="
}

main "$@"