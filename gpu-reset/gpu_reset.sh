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
NODE_NAME="${NODE_NAME:-unknown-node}"
START_TIME=$(date +%s.%N)
BUG_REPORT_TIMESTAMP="${BUG_REPORT_TIMESTAMP:-$(date +%Y%m%d-%H%M%S)}"
# With DRIVER_ROOT=/run/nvidia/driver, this writes to that HostPath mount. The report
# survives the reset container exiting, but is lost after reboot when /run is cleared.
BUG_REPORT_DIR="${BUG_REPORT_DIR:-/var/tmp}"

log() {
  printf "(%s) %s\n" "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

nvidia_smi_helper() {
  chroot "$DRIVER_ROOT" nvidia-smi "$@"
}

prepend_driver_root() {
  if [ "$DRIVER_ROOT" = "/" ]; then
    printf "%s\n" "$1"
  else
    printf "%s%s\n" "${DRIVER_ROOT%/}" "$1"
  fi
}

nvidia_bug_report_helper() {
  chroot "$DRIVER_ROOT" nvidia-bug-report.sh "$@"
}

collect_and_upload_nvidia_bug_report() {
  local upload_url_base="${UPLOAD_URL_BASE:-}"
  local local_bug_report_dir
  local bug_report_base
  local bug_report_file
  local local_bug_report_path
  local upload_url

  local_bug_report_dir=$(prepend_driver_root "$BUG_REPORT_DIR")

  if ! mkdir -p "$local_bug_report_dir"; then
    log "WARN: Failed to create nvidia-bug-report output directory: $local_bug_report_dir"
    return 1
  fi

  bug_report_base="${BUG_REPORT_DIR%/}/nvidia-bug-report-${NODE_NAME}-${BUG_REPORT_TIMESTAMP}"
  bug_report_file="nvidia-bug-report-${NODE_NAME}-${BUG_REPORT_TIMESTAMP}.log.gz"
  local_bug_report_path="${local_bug_report_dir%/}/${bug_report_file}"

  log "INFO: Collecting nvidia-bug-report for failed GPU reset..."
  if ! nvidia_bug_report_helper --safe-mode --output-file "${bug_report_base}.log"; then
    log "WARN: nvidia-bug-report collection failed."
    return 1
  fi

  if [ ! -f "$local_bug_report_path" ]; then
    log "WARN: nvidia-bug-report completed but output was not found: $local_bug_report_path"
    return 1
  fi

  log "INFO: nvidia-bug-report collected: $local_bug_report_path"

  if [ -z "$upload_url_base" ]; then
    log "INFO: UPLOAD_URL_BASE is not configured; nvidia-bug-report retained locally at $local_bug_report_path"
    return 0
  fi

  upload_url="${upload_url_base%/}/${NODE_NAME}/${BUG_REPORT_TIMESTAMP}/${bug_report_file}"

  log "INFO: Uploading nvidia-bug-report to $upload_url"
  if curl -fsS --connect-timeout 10 --max-time 120 -X PUT --upload-file "$local_bug_report_path" "$upload_url"; then
    log "INFO: nvidia-bug-report upload complete: $bug_report_file"
  else
    log "WARN: Failed to upload nvidia-bug-report: $bug_report_file"
    return 1
  fi
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
  sed -e '/All done\./d' -e 's/\.$//' -e 's/^/  /' "$RESET_OUTPUT_FILE"
  log "INFO: GPU reset complete."
else
  RESET_STATUS=$?
  FINAL_EXIT_STATUS=$RESET_STATUS
  log "ERROR: Reset failed. See details below:"
  sed -e '/All done\./d' -e 's/\.$//' "$RESET_OUTPUT_FILE" | grep . || true
  collect_and_upload_nvidia_bug_report || log "WARN: Continuing after nvidia-bug-report collection failure."
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
    logger -t nvsentinel-gpu-reset -p daemon.err \
      "GPU reset executed: ${UUID}, success: ${SYSLOG_SUCCESS}"

  done
  IFS=$ORIGINAL_IFS
else
  log "Skipping writing reset result (success: ${SYSLOG_SUCCESS}) to syslog: WRITE_SYSLOG_EVENT is not true"
fi

exit "$FINAL_EXIT_STATUS"
