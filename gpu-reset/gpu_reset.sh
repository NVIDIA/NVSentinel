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
#   Mode 3: Host-installed driver whose binaries are outside the default PATH (NVIDIA_VISIBLE_DEVICES=void):
#     Requires HostPath volumes:
#       - From / to /run/nvidia/driver
#       - From /sys to /run/nvidia/driver/sys
#     DRIVER_BIN_PATH is a path inside DRIVER_ROOT. For example, on GKE:
#       DRIVER_ROOT=/run/nvidia/driver DRIVER_BIN_PATH=/home/kubernetes/bin/nvidia/bin ./gpu_reset.sh
#
# NOTES:
#   - Requires root
#   - Supports x86_64 and aarch64
#   - With RESET_RUN_COUNT_FILE and RESET_MAX_RUNS set, the script counts its runs in the file and
#     writes a failed result to syslog only in run RESET_MAX_RUNS. A run after that exits without a
#     reset. Without them, every run writes its result.

set -eou pipefail

DRIVER_ROOT="${DRIVER_ROOT:-/}"
# Optional directory inside DRIVER_ROOT that holds the driver binaries, for drivers installed
# outside the default PATH.
DRIVER_BIN_PATH="${DRIVER_BIN_PATH:-}"
NODE_NAME="${NODE_NAME:-unknown-node}"
RESET_RUN_COUNT_FILE="${RESET_RUN_COUNT_FILE:-}"
RESET_MAX_RUNS="${RESET_MAX_RUNS:-}"
START_TIME=$(date +%s.%N)
BUG_REPORT_TIMESTAMP="${BUG_REPORT_TIMESTAMP:-$(date +%Y%m%d-%H%M%S)}"
# With DRIVER_ROOT=/run/nvidia/driver, this writes to that HostPath mount. The report
# survives the reset container exiting, but is lost after reboot when /run is cleared.
BUG_REPORT_DIR="${BUG_REPORT_DIR:-/var/tmp}"

log() {
  printf "(%s) %s\n" "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

# The reset Job runs this script again after a failed run. A failed result in syslog makes the node
# reboot, but a later run can still reset the GPU. So only the last run writes a failed result.
# RUN stays 0 when the runs are not counted, and every run then writes its result.
RUN=0

count_run() {
  local previous=0

  if [ -z "$RESET_RUN_COUNT_FILE" ] || [ -z "$RESET_MAX_RUNS" ]; then
    return 0
  fi

  if ! [[ "$RESET_MAX_RUNS" =~ ^[1-9][0-9]*$ ]]; then
    log "WARN: RESET_MAX_RUNS must be a positive integer, got '${RESET_MAX_RUNS}'. Every run writes its result."
    return 0
  fi

  if [ -f "$RESET_RUN_COUNT_FILE" ]; then
    previous=$(cat "$RESET_RUN_COUNT_FILE" 2>/dev/null || true)

    if ! [[ "$previous" =~ ^[0-9]+$ ]]; then
      log "WARN: Run count file ${RESET_RUN_COUNT_FILE} holds '${previous}'. Every run writes its result."
      return 0
    fi
  fi

  if ! printf "%s\n" "$((previous + 1))" > "$RESET_RUN_COUNT_FILE"; then
    log "WARN: Failed to write run count file ${RESET_RUN_COUNT_FILE}. Every run writes its result."
    return 0
  fi

  RUN=$((previous + 1))
}

count_run

if [ "$RUN" -gt 0 ]; then
  log "INFO: Run ${RUN} of ${RESET_MAX_RUNS}."

  # With restartPolicy OnFailure, the Job fails when the container restart count reaches the backoff
  # limit, which can be after this run has started. The last run has already written the result.
  if [ "$RUN" -gt "$RESET_MAX_RUNS" ]; then
    log "INFO: The reset Job has no runs left. Exiting without a reset."
    exit 1
  fi
fi

TARGET_UUIDS="${NVIDIA_GPU_RESETS:-}"
RESET_OUTPUT_FILE=$(mktemp)
HEALTH_CHECK_OUTPUT_FILE=$(mktemp)
RESULT_WRITTEN=false
EXIT_STATUS=0
# SYSLOG_SUCCESS is the outcome of the reset and the health check. It is empty until the summary.
SYSLOG_SUCCESS=""

# write_result writes the reset result of each target GPU to syslog.
write_result() {
  local success="$1"
  local uuid
  local original_ifs=$IFS

  IFS=','
  for uuid in $TARGET_UUIDS; do
    # Trim leading/trailing whitespace from UUID
    uuid=$(echo "$uuid" | xargs)

    if [ -z "$uuid" ]; then
      continue
    fi

    log "Writing reset result for ${uuid} to syslog (success: ${success})"
    logger -t nvsentinel-gpu-reset -p daemon.err \
      "GPU reset executed: ${uuid}, success: ${success}"
  done
  IFS=$original_ifs

  RESULT_WRITTEN=true
}

# The last run must report a failed reset also when it exits early, for example when the nvidia-smi version
# check fails under set -e. Without the report, the node is not rebooted. An earlier run does not report,
# because the reset Job runs the script again. After a successful reset, a failure to write the result must
# not report a failed reset.
trap 'EXIT_STATUS=$?
rm -f -- "$RESET_OUTPUT_FILE" "$HEALTH_CHECK_OUTPUT_FILE"
if [ "$EXIT_STATUS" -ne 0 ] && [ "$RESULT_WRITTEN" = "false" ] && [ "$SYSLOG_SUCCESS" != "true" ] && [ "$RUN" -gt 0 ] &&
  [ "$RUN" -eq "$RESET_MAX_RUNS" ] && [ "${WRITE_SYSLOG_EVENT:-true}" = "true" ] && [ -n "$TARGET_UUIDS" ]; then
  log "ERROR: The last run exited with status ${EXIT_STATUS} before it wrote the reset result."
  write_result false
fi' EXIT

# chroot looks up the command with PATH after it changes root, so DRIVER_BIN_PATH is resolved
# inside DRIVER_ROOT.
run_in_driver_root() {
  local search_path="$PATH"

  if [ -n "$DRIVER_BIN_PATH" ]; then
    search_path="${DRIVER_BIN_PATH}:${search_path}"
  fi

  PATH="$search_path" chroot "$DRIVER_ROOT" "$@"
}

nvidia_smi_helper() {
  run_in_driver_root nvidia-smi "$@"
}

prepend_driver_root() {
  if [ "$DRIVER_ROOT" = "/" ]; then
    printf "%s\n" "$1"
  else
    printf "%s%s\n" "${DRIVER_ROOT%/}" "$1"
  fi
}

nvidia_bug_report_helper() {
  run_in_driver_root nvidia-bug-report.sh "$@"
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

log "INFO: Using DRIVER_ROOT=$DRIVER_ROOT DRIVER_BIN_PATH=$DRIVER_BIN_PATH"
log "INFO: Testing nvidia-smi invocation: chroot $DRIVER_ROOT nvidia-smi --version"
nvidia_smi_helper --version

RESET_STATUS=0
FINAL_EXIT_STATUS=0

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

# Set the outcome before anything else can fail, so that the EXIT trap does not report a failed reset after
# a successful one.
if [ "$FINAL_EXIT_STATUS" -ne 0 ]; then
  SYSLOG_SUCCESS="false"
else
  SYSLOG_SUCCESS="true"
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
fi

if [ "$SYSLOG_SUCCESS" = "false" ] && [ "$RUN" -gt 0 ] && [ "$RUN" -lt "$RESET_MAX_RUNS" ]; then
  log "Skipping writing the failed reset result to syslog: the reset Job runs the script again (run ${RUN} of ${RESET_MAX_RUNS})"
elif [ "${WRITE_SYSLOG_EVENT:-true}" = "true" ]; then
  write_result "$SYSLOG_SUCCESS"
else
  log "Skipping writing reset result (success: ${SYSLOG_SUCCESS}) to syslog: WRITE_SYSLOG_EVENT is not true"
fi

# The bug report upload can take up to 2 minutes. Collect the report after the result is written, so that the
# deadline of the reset Job cannot stop the last run before it reports a failed reset.
if [ "$RESET_STATUS" -ne 0 ]; then
  collect_and_upload_nvidia_bug_report || log "WARN: Continuing after nvidia-bug-report collection failure."
fi

exit "$FINAL_EXIT_STATUS"
