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
set -eou pipefail

# Performs a robust NVIDIA GPU reset workflow. This includes
# safely managing persistence mode (PM), executing the reset,
# restoring PM, and running a post-reset health check.
#
# USAGE:
#   1. Reset all GPUs (default if NVIDIA_GPU_RESETS is unset or empty):
#      ./gpu_reset.sh
#
#   2. Reset specific GPUs:
#      NVIDIA_GPU_RESETS="GPU-123,GPU-456" ./gpu_reset.sh
#
# NOTES:
#   - Requires root
#   - Requires 'nvidia-smi'

START_TIME=$(date +%s.%N)

log() {
  printf "(%s) %s\n" "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

RESET_STATUS=0
FINAL_EXIT_STATUS=0
PM_STATES_FILE=$(mktemp)
RESET_OUTPUT_FILE=$(mktemp)
HEALTH_CHECK_OUTPUT_FILE=$(mktemp)
trap 'rm -f -- "$PM_STATES_FILE" "$RESET_OUTPUT_FILE" "$HEALTH_CHECK_OUTPUT_FILE"' EXIT

log "INFO: Starting GPU reset workflow..."

#------------------
# DETERMINE TARGETS
#------------------

log "INFO: Determining target devices..."
IDS="${NVIDIA_GPU_RESETS:-}"

if [ -z "$IDS" ]; then
  IDS=$(nvidia-smi --query-gpu=uuid --format=csv,noheader | sed 's/^[[:space:]]*//' | tr '\n' ',' | sed 's/,$//')

  if [ -z "$IDS" ]; then
    log "ERROR: No specific devices provided, and no GPUs found on the node."
    exit 1
  fi

  log "INFO: No specific devices provided. All GPUs will be reset."
fi

TARGET_UUIDS="$IDS"
log "INFO: Targets:"
echo "${TARGET_UUIDS}" | tr ',' '\n' | sed 's/^/  /'

#-------------------------------------------
# PRE-RESET PERSISTENCE MODE (PM) MANAGEMENT
#-------------------------------------------

log "INFO: Running pre-reset persistence mode (PM) management..."

ORIGINAL_IFS=$IFS
IFS=','
for UUID in $TARGET_UUIDS; do
  # Trim leading/trailing whitespace from UUID
  UUID=$(echo "$UUID" | xargs)

  if [ -z "$UUID" ]; then
    continue
  fi

  if ! nvidia-smi -i "$UUID" >/dev/null 2>&1; then
    log "WARN: Device not found or inaccessible: ${UUID}"
    continue
  fi

  PM_STATUS=$(nvidia-smi -i "$UUID" --query-gpu=persistence_mode --format=csv,noheader | tr -d ' \r\n')

  # Store original status for re-enabling
  echo "${UUID}:${PM_STATUS}" >> "${PM_STATES_FILE}"

  if [ "${PM_STATUS}" = "Enabled" ]; then
    nvidia-smi -i "${UUID}" -pm 0 | grep -v "All done." | sed -e 's/\.$//' -e 's/^/  /'
  fi
done
IFS=$ORIGINAL_IFS

log "INFO: Pre-reset persistence mode (PM) management complete."

#----------------
# RESET EXECUTION
#----------------

log "INFO: Resetting GPUs..."

if nvidia-smi --gpu-reset -i "${TARGET_UUIDS}" > "$RESET_OUTPUT_FILE" 2>&1; then
  cat "$RESET_OUTPUT_FILE" | grep -v "All done." | sed -e 's/\.$//' -e 's/^/  /'
  log "INFO: GPU reset complete."
else
  RESET_STATUS=$?
  FINAL_EXIT_STATUS=$RESET_STATUS
  log "ERROR: Reset failed. See details below:"
  cat "$RESET_OUTPUT_FILE" | grep -v "All done." | sed 's/\.$//' | grep .
fi

#--------------------------------------------
# POST-RESET PERSISTENCE MODE (PM) MANAGEMENT
#--------------------------------------------

log "INFO: Running post-reset persistence mode (PM) management..."

while IFS= read -r line; do
  ID=$(echo "$line" | cut -d: -f1)
  ORIGINAL_STATUS=$(echo "$line" | cut -d: -f2)

  if [ "${ORIGINAL_STATUS}" = "Enabled" ]; then
    nvidia-smi -i "${ID}" -pm 1 | grep -v "All done." | sed -e 's/\.$//' -e 's/^/  /'
  fi
done < "${PM_STATES_FILE}"

log "INFO: Post-reset persistence mode (PM) management complete."

#------------------------
# POST-RESET HEALTH CHECK
#------------------------

if [ "$FINAL_EXIT_STATUS" -eq 0 ]; then
  log "INFO: Running post-reset health check..."

  if nvidia-smi -q -i "$TARGET_UUIDS" > "$HEALTH_CHECK_OUTPUT_FILE" 2>&1; then
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
else
  log "SUCCESS: GPU reset workflow completed in ${DURATION}s."

  # Write "GPU reset executed" message to notify syslog-health-monitor of GPU reset success
  ORIGINAL_IFS=$IFS
  IFS=','
  for UUID in $TARGET_UUIDS; do
    # Trim leading/trailing whitespace from UUID
    UUID=$(echo "$UUID" | xargs)

    if [ -z "$UUID" ]; then
      continue
    fi

    log "Writing reset success for ${UUID} to syslog"
    logger -p daemon.err "GPU reset executed: ${UUID}"

  done
  IFS=$ORIGINAL_IFS
fi

exit "$FINAL_EXIT_STATUS"
