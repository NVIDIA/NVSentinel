#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

# NPD custom plugin: nvidia-fabricmanager crash-loop (flap) detection.
#
# Exit codes follow the NPD custom-plugin protocol:
#   0 = healthy, 1 = unhealthy (flapping), 3 = unknown (could not observe).
#
# Restart observations come from systemd NRestarts deltas between
# invocations, kept in a state file on the host's /run tree (tmpfs: the
# window is boot-scoped by construction). NRestarts is not monotonic and a
# decrease is not itself a restart: `systemctl reset-failed` flushes the
# counter without restarting the process. On a decrease this script
# re-baselines and records a restart observation only when
# ExecMainStartTimestampMonotonic also changed; a pure counter flush records
# nothing. Manual restarts do not increment NRestarts and are not counted —
# the window tracks Restart= crash-loop behavior.
#
# Probe failures — including unparseable systemd output — never clear a
# fault: the script holds its last result (flapping indefinitely; healthy
# for PROBE_FAIL_MAX consecutive failures, after which the stale
# confirmation is degraded and the check reports unknown). A failed
# state-file commit never clears a fault either: a flapping result is still
# reported unhealthy; other results report unknown, because an uncommitted
# baseline would double-count restart deltas on the next invocation.
#
# State-file contract (documented in the operator guide; DaemonSet NPD must
# hostPath-mount the host's /var/run/nvsentinel/npd at the same path, or the
# boot-scoped guarantee degrades to pod-scoped):
#   baseline_nrestarts=<int>
#   baseline_exec_ts=<int>                  (ExecMainStartTimestampMonotonic)
#   observations=<epoch>:<count>[ <epoch>:<count>...]
#   last_result=<healthy|flapping|none>
#   probe_fail_count=<int>
# An unreadable or invalid state file is treated as a fresh baseline.

set -o nounset
set -o pipefail

UNIT="nvidia-fabricmanager"
STATE_DIR="/var/run/nvsentinel/npd"
STATE_FILE="${STATE_DIR}/fm-flap.state"
WINDOW_SECONDS=600
RESTART_THRESHOLD=3
PROBE_FAIL_MAX=4
# Canonical decimal: leading zeros would be read as octal by Bash arithmetic.
NUM='^(0|[1-9][0-9]*)$'
# Bound the probe below the NPD rule timeout (12s) so a wedged systemd/D-Bus
# reports as this script's deliberate unknown, not an NPD plugin timeout.
PROBE_TIMEOUT_SECONDS=8

# --- state -------------------------------------------------------------------

baseline_nrestarts=""
baseline_exec_ts=""
observations=""
last_result="none"
probe_fail_count=0

if [[ -r "${STATE_FILE}" ]]; then
  while IFS='=' read -r key value; do
    case "${key}" in
      baseline_nrestarts) baseline_nrestarts="${value}" ;;
      baseline_exec_ts) baseline_exec_ts="${value}" ;;
      observations) observations="${value}" ;;
      last_result) last_result="${value}" ;;
      probe_fail_count) probe_fail_count="${value}" ;;
    esac
  done < "${STATE_FILE}"
fi
if ! [[ "${last_result}" =~ ^(healthy|flapping|none)$ && "${probe_fail_count}" =~ ${NUM} ]]; then
  last_result="none"; probe_fail_count=0
fi
if ! [[ "${baseline_nrestarts}" =~ ${NUM} && "${baseline_exec_ts}" =~ ${NUM} ]]; then
  baseline_nrestarts=""
  observations=""
fi

# Persist state; returns non-zero (with the temp file removed) on any failure.
persist_state() {
  local tmp_file
  mkdir -p "${STATE_DIR}" 2>/dev/null || return 1
  chmod 0700 "${STATE_DIR}" 2>/dev/null || return 1
  tmp_file=$(mktemp "${STATE_DIR}/.fm-flap.XXXXXX" 2>/dev/null) || return 1
  if ! chmod 0600 "${tmp_file}" 2>/dev/null ||
     ! {
       echo "baseline_nrestarts=${baseline_nrestarts}"
       echo "baseline_exec_ts=${baseline_exec_ts}"
       echo "observations=${observations}"
       echo "last_result=${last_result}"
       echo "probe_fail_count=${probe_fail_count}"
     } > "${tmp_file}" 2>/dev/null ||
     ! mv -f "${tmp_file}" "${STATE_FILE}" 2>/dev/null; then
    rm -f "${tmp_file}" 2>/dev/null
    return 1
  fi
  return 0
}

save_state_and_exit() { # $1=exit code, $2=message
  local rc="${1}" msg="${2}"
  if persist_state; then
    echo "${msg}"
    exit "${rc}"
  fi
  # A failed commit must never clear a fault; other results become unknown —
  # an uncommitted baseline would double-count deltas next invocation.
  if [[ "${rc}" == "1" ]]; then
    echo "${msg} (state persistence failed)"
    exit 1
  fi
  echo "could not persist flap state for ${UNIT} under ${STATE_DIR}"
  exit 3
}

# Probe failure or unparseable output: hold the last result.
hold_last_result_and_exit() { # $1=diagnostic message for the unknown case
  local diag="${1}"
  probe_fail_count=$((probe_fail_count + 1))
  if [[ "${last_result}" == "flapping" ]]; then
    save_state_and_exit 1 \
      "${UNIT} holding fault: probe failing (${probe_fail_count} consecutive), last confirmed flapping"
  fi
  if [[ "${last_result}" == "healthy" && ${probe_fail_count} -lt ${PROBE_FAIL_MAX} ]]; then
    save_state_and_exit 0 \
      "${UNIT} holding healthy: probe failing (${probe_fail_count} consecutive)"
  fi
  # The healthy confirmation is stale after the hold expires: degrade it.
  last_result="none"
  save_state_and_exit 3 "${diag}"
}

# --- probe -------------------------------------------------------------------

show_output=$(timeout "${PROBE_TIMEOUT_SECONDS}" \
  systemctl show "${UNIT}" \
  --property=LoadState,NRestarts,ExecMainStartTimestampMonotonic \
  --no-pager 2>/dev/null)
rc=$?
if [[ ${rc} -ne 0 || -z "${show_output}" ]]; then
  hold_last_result_and_exit "could not observe ${UNIT}: systemctl unavailable (rc=${rc})"
fi

load_state=""
n_restarts=""
exec_ts=""
while IFS='=' read -r key value; do
  case "${key}" in
    LoadState) load_state="${value}" ;;
    NRestarts) n_restarts="${value}" ;;
    ExecMainStartTimestampMonotonic) exec_ts="${value}" ;;
  esac
done <<< "${show_output}"

if [[ "${load_state}" == "not-found" ]]; then
  echo "${UNIT} is not present on this host; flap check not applicable"
  exit 0
fi

if ! [[ "${n_restarts}" =~ ${NUM} && "${exec_ts}" =~ ${NUM} ]]; then
  hold_last_result_and_exit \
    "could not observe ${UNIT}: unparseable NRestarts/ExecMainStartTimestamp"
fi

probe_fail_count=0
now=$(date +%s)

if [[ -n "${baseline_nrestarts}" ]]; then
  delta=$((n_restarts - baseline_nrestarts))
  if [[ ${delta} -gt 0 ]]; then
    observations="${observations:+${observations} }${now}:${delta}"
  elif [[ ${delta} -lt 0 && "${exec_ts}" != "${baseline_exec_ts}" ]]; then
    # Counter flushed and the main process also restarted across the flush;
    # the true count is unknowable, so record the one provable restart.
    observations="${observations:+${observations} }${now}:1"
  fi
fi

# Prune observations that fell out of the sliding window and total the rest.
window_start=$((now - WINDOW_SECONDS))
kept=""
total=0
for entry in ${observations}; do
  ts="${entry%%:*}"
  count="${entry##*:}"
  if [[ "${ts}" =~ ${NUM} && "${count}" =~ ${NUM} && ${ts} -ge ${window_start} ]]; then
    kept="${kept:+${kept} }${entry}"
    total=$((total + count))
  fi
done

baseline_nrestarts="${n_restarts}"
baseline_exec_ts="${exec_ts}"
observations="${kept}"
if [[ ${total} -ge ${RESTART_THRESHOLD} ]]; then
  last_result="flapping"
  save_state_and_exit 1 \
    "${UNIT} restarted ${total} times in the last ${WINDOW_SECONDS}s (threshold ${RESTART_THRESHOLD})"
fi
last_result="healthy"
save_state_and_exit 0 \
  "${UNIT} restart rate is normal (${total} in the last ${WINDOW_SECONDS}s)"
