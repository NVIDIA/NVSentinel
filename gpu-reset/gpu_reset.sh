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

# Performs a robust NVIDIA GPU reset workflow. This includes
# executing the reset and running a post-reset health check.
#
# USAGE:
#   Mode 1: nvidia-container-toolkit (NVIDIA_VISIBLE_DEVICES=all):
#     ./gpu_reset.sh
#     NVIDIA_GPU_RESETS="GPU-123,GPU-456" ./gpu_reset.sh
#
#   Mode 2: HostPath driver mount (NVIDIA_VISIBLE_DEVICES=void):
#     Requires HostPath volumes:
#       - From /run/nvidia/driver to /run/nvidia/driver
#       - From /sys to /run/nvidia/driver/sys
#     Examples:
#       DRIVER_ROOT=/run/nvidia/driver ./gpu_reset.sh
#       DRIVER_ROOT=/run/nvidia/driver NVIDIA_GPU_RESETS="GPU-123,GPU-456" ./gpu_reset.sh
#
# NOTES:
#   - Requires root
#   - Supports x86_64 and aarch64

set -eou pipefail

DRIVER_ROOT="${DRIVER_ROOT:-/}"
START_TIME=$(date +%s.%N)

log() {
  printf "(%s) %s\n" "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

nvidia_smi_helper() {
  chroot "$DRIVER_ROOT" nvidia-smi "$@"
}

log "INFO: Using DRIVER_ROOT=$DRIVER_ROOT"
log "INFO: Testing nvidia-smi invocation: chroot $DRIVER_ROOT nvidia-smi --version"
nvidia_smi_helper --version

RESET_STATUS=0
FINAL_EXIT_STATUS=0
RESET_OUTPUT_FILE=$(mktemp)
HEALTH_CHECK_OUTPUT_FILE=$(mktemp)
trap 'rm -f -- "$RESET_OUTPUT_FILE" "$HEALTH_CHECK_OUTPUT_FILE"' EXIT

log "INFO: Starting GPU reset workflow..."

#------------------
# DETERMINE TARGETS
#------------------

log "INFO: Determining target devices..."
IDS="${NVIDIA_GPU_RESETS:-}"

if [ -z "$IDS" ]; then
  IDS=$(nvidia_smi_helper --query-gpu=uuid --format=csv,noheader | sed 's/^[[:space:]]*//' | tr '\n' ',' | sed 's/,$//')

  if [ -z "$IDS" ]; then
    log "ERROR: No specific devices provided, and no GPUs found on the node."
    exit 1
  fi

  log "INFO: No specific devices provided. All GPUs will be reset."
fi

TARGET_UUIDS="$IDS"
log "INFO: Targets:"
echo "${TARGET_UUIDS}" | tr ',' '\n' | sed 's/^/  /'

#----------------
# RESET EXECUTION
#----------------

log "INFO: Resetting GPUs..."

if nvidia_smi_helper --gpu-reset -i "${TARGET_UUIDS}" > "$RESET_OUTPUT_FILE" 2>&1; then
  # shellcheck disable=SC2002
  cat "$RESET_OUTPUT_FILE" | grep -v "All done." | sed -e 's/\.$//' -e 's/^/  /'
  log "INFO: GPU reset complete."
else
  RESET_STATUS=$?
  FINAL_EXIT_STATUS=$RESET_STATUS
  log "ERROR: Reset failed. See details below:"
  # shellcheck disable=SC2002
  cat "$RESET_OUTPUT_FILE" | grep -v "All done." | sed 's/\.$//' | grep .
fi

#------------------------
# POST-RESET HEALTH CHECK
#------------------------

if [ "$FINAL_EXIT_STATUS" -eq 0 ]; then
  log "INFO: Running post-reset health check..."

  if nvidia_smi_helper -q -i "$TARGET_UUIDS" > "$HEALTH_CHECK_OUTPUT_FILE" 2>&1; then
    log "INFO: Post-reset health check passed."
  else
    log "ERROR: Post-reset health check failed. See details below:"
    sed 's/^/  /' "$HEALTH_CHECK_OUTPUT_FILE"
    FINAL_EXIT_STATUS=1
  fi
else
  log "WARN: Post-reset health check skipped."
fi

#------------------------
# SUMMARY
#------------------------

END_TIME=$(date +%s.%N)
DURATION_RAW=$(awk "BEGIN {print ${END_TIME} - ${START_TIME}}")
DURATION=$(printf "%.3f" "$DURATION_RAW")

if [ "$FINAL_EXIT_STATUS" -ne 0 ]; then
  log "FAILED: GPU reset workflow failed in ${DURATION}s."
  SYSLOG_SUCCESS="false"
else
  log "SUCCESS: GPU reset workflow completed in ${DURATION}s."
  SYSLOG_SUCCESS="true"
fi

if [ "${WRITE_SYSLOG_EVENT:-true}" = "true" ]; then
  ORIGINAL_IFS=$IFS
  IFS=','
  for UUID in $TARGET_UUIDS; do
    # Trim leading/trailing whitespace from UUID
    UUID=$(echo "$UUID" | xargs)

    if [ -z "$UUID" ]; then
      continue
    fi

    log "Writing reset result for ${UUID} to syslog (success: ${SYSLOG_SUCCESS})"
    logger -p daemon.err "GPU reset executed: ${UUID}, success: ${SYSLOG_SUCCESS}"

  done
  IFS=$ORIGINAL_IFS
else
  log "Skipping writing reset result (success: ${SYSLOG_SUCCESS}) to syslog: WRITE_SYSLOG_EVENT is not true"
fi

exit "$FINAL_EXIT_STATUS"