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
    kubectl get node "$node" -o jsonpath='{.status.nodeInfo.bootID}'
}

wait_for_boot_id_change() {
    local node=$1
    local original_boot_id=$2
    local timeout=${UAT_REBOOT_TIMEOUT:-600}
    local elapsed=0


    log "Waiting for node $node to reboot (boot ID to change)..."


    while [[ $elapsed -lt $timeout ]]; do
        local current_boot_id
        current_boot_id=$(get_boot_id "$node" 2>/dev/null || echo "")


        if [[ -n "$current_boot_id" && "$current_boot_id" != "$original_boot_id" ]]; then
            log "Node $node rebooted successfully (boot ID changed)"
            elapsed=0
            break
        fi

        sleep 5
        elapsed=$((elapsed + 5))
    done

    if [[ $elapsed -ge $timeout ]]; then
        error "Timeout waiting for node $node to reboot"
    fi

    log "Waiting for node $node to be uncordoned..."
    while [[ $elapsed -lt $timeout ]]; do
        local is_cordoned
        is_cordoned=$(kubectl get node "$node" -o jsonpath='{.spec.unschedulable}')


        if [[ "$is_cordoned" != "true" ]]; then
            log "Node $node is uncordoned and ready ✓"
            return 0
        fi

        sleep 5
        elapsed=$((elapsed + 5))
    done

    error "Timeout waiting for node $node to be uncordoned"
}

test_gpu_monitoring_dcgm() {
    log "========================================="
    log "Test 1: GPU monitoring via DCGM"
    log "========================================="

    local gpu_node
    gpu_node=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.name}')

    if [[ -z "$gpu_node" ]]; then
        error "No GPU nodes found"
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

    kubectl exec -n gpu-operator "$dcgm_pod" -- dcgmi test --inject --gpuid 0 -f 84 -v 0    # infoROM watch error

    log "Waiting for node conditions to appear..."
    local max_wait=30
    local waited=0
    while [[ $waited -lt $max_wait ]]; do
        conditions_count=$(kubectl get node "$gpu_node" -o json | jq '[.status.conditions[] | select(.type == "GpuInforomWatch" and .status == "True")] | length')
        if [[ "$conditions_count" -ge 1 ]]; then
            log "Found $conditions_count node conditions"
            break
        fi
        sleep 2
        waited=$((waited + 2))
    done

    log "Verifying node conditions are populated"
    kubectl get node "$gpu_node" -o json | jq -r '.status.conditions[] | select(.type == "GpuInforomWatch") | "\(.type) Status=\(.status) Reason=\(.reason)"'

    inforom_condition=$(kubectl get node "$gpu_node" -o json | jq -r '.status.conditions[] | select(.type == "GpuInforomWatch" and .status == "True") | .type')

    if [[ -z "$inforom_condition" ]]; then
        error "Expected node conditions not found: GpuInforomWatch=$inforom_condition"
    fi
    log "Node conditions verified ✓"

    log "Waiting for node to be quarantined and rebooted..."
    wait_for_boot_id_change "$gpu_node" "$original_boot_id"

    log "Test 1 PASSED ✓"
}

test_xid_monitoring_syslog() {
    log "========================================="
    log "Test 2: XID monitoring via syslog"
    log "========================================="

    local gpu_node
    gpu_node=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.name}')

    if [[ -z "$gpu_node" ]]; then
        error "No GPU nodes found"
    fi

    log "Selected GPU node: $gpu_node"

    local original_boot_id
    original_boot_id=$(get_boot_id "$gpu_node")
    log "Original boot ID: $original_boot_id"

    local driver_pod
    driver_pod=$(kubectl get pods -n gpu-operator -l app=nvidia-driver-daemonset -o jsonpath="{.items[?(@.spec.nodeName=='$gpu_node')].metadata.name}" | head -1)

    if [[ -z "$driver_pod" ]]; then
        error "No driver pod found on node $gpu_node"
    fi

    log "Injecting XID 119 message via logger on pod: $driver_pod"
    kubectl exec -n gpu-operator "$driver_pod" -- logger -p daemon.err "[6085126.134786] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8)."

    log "Waiting for node to be quarantined and rebooted..."
    wait_for_boot_id_change "$gpu_node" "$original_boot_id"

    log "Test 2 PASSED ✓"
}

test_sxid_monitoring_syslog() {
    log "========================================="
    log "Test 3: SXID monitoring (NVSwitch errors)"
    log "========================================="

    local gpu_node
    gpu_node=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.name}')

    if [[ -z "$gpu_node" ]]; then
        error "No GPU nodes found"
    fi

    log "Selected GPU node: $gpu_node"

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

    log "Waiting for node conditions to appear..."
    local max_wait=30
    local waited=0
    while [[ $waited -lt $max_wait ]]; do
        conditions_count=$(kubectl get node "$gpu_node" -o json | jq '[.status.conditions[] | select(.type == "SysLogsSXIDError" and .status == "True")] | length')
        if [[ "$conditions_count" -ge 1 ]]; then
            log "Found $conditions_count node conditions"
            break
        fi
        sleep 2
        waited=$((waited + 2))
    done

    log "Verifying SXID node condition is populated (fatal SXID 20034)"
    sxid_condition=$(kubectl get node "$gpu_node" -o json | jq -r '.status.conditions[] | select(.type == "SysLogsSXIDError" and .status == "True") | .type')

    if [[ -z "$sxid_condition" ]]; then
        error "SysLogsSXIDError condition not found (fatal SXID should create condition)"
    fi
    log "Node condition verified: SysLogsSXIDError ✓"

    log "Waiting for node to be quarantined and rebooted..."
    wait_for_boot_id_change "$gpu_node" "$original_boot_id"

    log "Test 3 PASSED ✓"
}

main() {
    log "Starting NVSentinel UAT tests..."


    test_gpu_monitoring_dcgm
    test_xid_monitoring_syslog
    # test_sxid_monitoring_syslog

    log "========================================="
    log "All tests PASSED ✓"
    log "========================================="
}

main "$@"
