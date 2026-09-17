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

# NPD custom plugin: GPU-support service liveness, parameterized (ADR-050).
#
# One NPD permanent rule per configured service, each bound to its own
# condition type. The reference Fabric Manager liveness rule invokes this
# script too:
#
#   { "type": "permanent", "condition": "FabricManagerDown", ...
#     "path": "/etc/npd-plugins/check_gpu_service.sh",
#     "args": ["nvidia-fabricmanager"] }
#
# Exit codes follow the NPD custom-plugin protocol:
#   0 = healthy, 1 = unhealthy, 3 = unknown.
# Stdout becomes the condition message on state transitions.
#
# Contract (ADR-050 plugin script contracts):
# - LoadState=not-found exits healthy: the liveness check skips absent
#   units; presence is a separate, operator-declared check
#   (check_fm_installed.sh).
# - ActiveState=active confirms healthy and resets the failure count.
# - Transitional states (activating/deactivating/reloading) neither confirm
#   health nor count as down: the script holds its last confirmed state and
#   resets the consecutive-failure count, so a service starting up or in a
#   planned restart never fires the condition.
# - A non-running observation (inactive/failed) reports down only after
#   FAIL_THRESHOLD consecutive probes; anything else resets the count.
# - Probe failures never clear a fault: the script holds the last confirmed
#   state (unhealthy indefinitely; healthy for PROBE_FAIL_MAX consecutive
#   failures, after which the healthy confirmation is considered stale and
#   the check reports unknown until a probe confirms either state).
# - A failed state-file commit never clears a fault either: an unhealthy
#   result is still reported unhealthy; other results report unknown, since
#   their debounce or hold accounting could not be persisted.

set -o nounset
set -o pipefail

if [[ $# -ne 1 || -z "${1}" ]]; then
  echo "usage: check_gpu_service.sh <systemd-unit>"
  exit 3
fi
UNIT="${1}"
STATE_DIR="/var/run/nvsentinel/npd"
STATE_FILE="${STATE_DIR}/svc-${UNIT}.state"
FAIL_THRESHOLD=3
PROBE_FAIL_MAX=4
# Canonical decimal: leading zeros would be read as octal by Bash arithmetic.
NUM='^(0|[1-9][0-9]*)$'
# Bound the probe below the NPD rule timeout (12s) so a wedged systemd/D-Bus
# reports as this script's deliberate exit, not an NPD plugin timeout.
PROBE_TIMEOUT_SECONDS=8

# --- state helpers -----------------------------------------------------------

confirmed="none"
fail_count=0
probe_fail_count=0

load_state() {
  [[ -r "${STATE_FILE}" ]] || return 0
  local key value
  while IFS='=' read -r key value; do
    case "${key}" in
      confirmed) confirmed="${value}" ;;
      fail_count) fail_count="${value}" ;;
      probe_fail_count) probe_fail_count="${value}" ;;
    esac
  done < "${STATE_FILE}"
  # Invalid content means a fresh baseline: never hold phantom state.
  if ! [[ "${confirmed}" =~ ^(healthy|unhealthy|none)$ &&
          "${fail_count}" =~ ${NUM} &&
          "${probe_fail_count}" =~ ${NUM} ]]; then
    confirmed="none"; fail_count=0; probe_fail_count=0
  fi
}

# Persist state; returns non-zero (with the temp file removed) on any failure.
persist_state() {
  local tmp_file
  mkdir -p "${STATE_DIR}" 2>/dev/null || return 1
  chmod 0700 "${STATE_DIR}" 2>/dev/null || return 1
  tmp_file=$(mktemp "${STATE_DIR}/.svc-${UNIT}.XXXXXX" 2>/dev/null) || return 1
  if ! chmod 0600 "${tmp_file}" 2>/dev/null ||
     ! {
       echo "confirmed=${confirmed}"
       echo "fail_count=${fail_count}"
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
  # A failed commit must never clear a fault; other results become unknown
  # because their debounce/hold accounting was not persisted.
  if [[ "${rc}" == "1" ]]; then
    echo "${msg} (state persistence failed)"
    exit 1
  fi
  echo "could not persist state for ${UNIT} under ${STATE_DIR}"
  exit 3
}

# --- probe -------------------------------------------------------------------

load_state

show_output=$(timeout "${PROBE_TIMEOUT_SECONDS}" \
  systemctl show "${UNIT}" \
  --property=LoadState,ActiveState,SubState --no-pager 2>/dev/null)
rc=$?

if [[ ${rc} -ne 0 || -z "${show_output}" ]]; then
  # Probe failure: hold the last confirmed state; never clear a fault. The
  # consecutive-failure count only tracks confirmed non-running probes.
  probe_fail_count=$((probe_fail_count + 1))
  fail_count=0
  if [[ "${confirmed}" == "unhealthy" ]]; then
    save_state_and_exit 1 \
      "${UNIT} holding fault: probe failing (${probe_fail_count} consecutive), last confirmed not active"
  fi
  if [[ "${confirmed}" == "healthy" && ${probe_fail_count} -lt ${PROBE_FAIL_MAX} ]]; then
    save_state_and_exit 0 \
      "${UNIT} holding healthy: probe failing (${probe_fail_count} consecutive)"
  fi
  # The healthy confirmation is stale after the hold expires: degrade so a
  # later observation cannot resurrect it.
  confirmed="none"
  save_state_and_exit 3 "could not observe ${UNIT}: systemctl unavailable (rc=${rc})"
fi

probe_fail_count=0

load_state_prop=""
active_state=""
sub_state=""
while IFS='=' read -r key value; do
  case "${key}" in
    LoadState) load_state_prop="${value}" ;;
    ActiveState) active_state="${value}" ;;
    SubState) sub_state="${value}" ;;
  esac
done <<< "${show_output}"

if [[ "${load_state_prop}" == "not-found" ]]; then
  confirmed="none"; fail_count=0
  save_state_and_exit 0 "${UNIT} is not present on this host; liveness check not applicable"
fi

case "${active_state}" in
  active)
    confirmed="healthy"; fail_count=0
    save_state_and_exit 0 "${UNIT} is active"
    ;;
  inactive|failed)
    if [[ "${confirmed}" == "unhealthy" ]]; then
      save_state_and_exit 1 "${UNIT} is not active (state=${active_state}, sub-state=${sub_state:-unknown})"
    fi
    fail_count=$((fail_count + 1))
    if [[ ${fail_count} -ge ${FAIL_THRESHOLD} ]]; then
      confirmed="unhealthy"
      save_state_and_exit 1 \
        "${UNIT} is not active (state=${active_state}, sub-state=${sub_state:-unknown}; ${fail_count} consecutive probes)"
    fi
    if [[ "${confirmed}" == "healthy" ]]; then
      save_state_and_exit 0 \
        "${UNIT} not active (${fail_count}/${FAIL_THRESHOLD} consecutive); holding healthy pending threshold"
    fi
    save_state_and_exit 3 \
      "${UNIT} not active (${fail_count}/${FAIL_THRESHOLD} consecutive); no confirmed state yet"
    ;;
  *)
    # Transitional or unrecognized states (activating, deactivating,
    # reloading, ...): neither confirm health nor count as down; hold, and
    # reset the consecutive-failure count — it tracks consecutive
    # non-running observations only.
    fail_count=0
    if [[ "${confirmed}" == "unhealthy" ]]; then
      save_state_and_exit 1 "${UNIT} holding fault: unit in transition (state=${active_state})"
    fi
    if [[ "${confirmed}" == "healthy" ]]; then
      save_state_and_exit 0 "${UNIT} in transition (state=${active_state}); holding healthy"
    fi
    save_state_and_exit 3 "${UNIT} in transition (state=${active_state}); no confirmed state yet"
    ;;
esac
