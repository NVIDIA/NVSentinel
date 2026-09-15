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

# NPD custom plugin: nvidia-fabricmanager liveness (ADR-050).
#
# Exit codes follow the NPD custom-plugin protocol:
#   0 = healthy, 1 = unhealthy, 3 = unknown.
# Stdout becomes the condition message on state transitions.
#
# Contract (ADR-050 plugin script contracts):
# - LoadState=not-found exits healthy: presence is a separate,
#   operator-declared check (check_fm_installed.sh).
# - ActiveState=active confirms healthy and resets the failure count.
# - Transitional states (activating/deactivating/reloading) neither confirm
#   health nor count as down: the script holds its last confirmed state, so
#   startup and planned restarts never fire the condition.
# - A non-running observation (inactive/failed) reports down only after
#   FAIL_THRESHOLD consecutive probes.
# - Probe failures never clear a fault: the script holds the last confirmed
#   state (unhealthy indefinitely; healthy for PROBE_FAIL_MAX consecutive
#   failures, then unknown). Recovery always requires a confirming probe.

set -o nounset
set -o pipefail

UNIT="nvidia-fabricmanager"
STATE_DIR="/var/run/nvsentinel/npd"
STATE_FILE="${STATE_DIR}/fm-liveness.state"
FAIL_THRESHOLD=3
PROBE_FAIL_MAX=4
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
          "${fail_count}" =~ ^[0-9]+$ &&
          "${probe_fail_count}" =~ ^[0-9]+$ ]]; then
    confirmed="none"; fail_count=0; probe_fail_count=0
  fi
}

save_state_and_exit() { # $1=exit code, $2=message
  local rc="${1}" msg="${2}" tmp_file
  if mkdir -p "${STATE_DIR}" 2>/dev/null && chmod 0700 "${STATE_DIR}" 2>/dev/null &&
     tmp_file=$(mktemp "${STATE_DIR}/.fm-liveness.XXXXXX" 2>/dev/null); then
    chmod 0600 "${tmp_file}"
    {
      echo "confirmed=${confirmed}"
      echo "fail_count=${fail_count}"
      echo "probe_fail_count=${probe_fail_count}"
    } > "${tmp_file}"
    mv -f "${tmp_file}" "${STATE_FILE}"
  fi
  echo "${msg}"
  exit "${rc}"
}

# --- probe -------------------------------------------------------------------

load_state

show_output=$(timeout "${PROBE_TIMEOUT_SECONDS}" \
  systemctl show "${UNIT}" \
  --property=LoadState,ActiveState,SubState --no-pager 2>/dev/null)
rc=$?

if [[ ${rc} -ne 0 || -z "${show_output}" ]]; then
  # Probe failure: hold the last confirmed state; never clear a fault.
  probe_fail_count=$((probe_fail_count + 1))
  if [[ "${confirmed}" == "unhealthy" ]]; then
    save_state_and_exit 1 \
      "${UNIT} holding fault: probe failing (${probe_fail_count} consecutive), last confirmed not active"
  fi
  if [[ "${confirmed}" == "healthy" && ${probe_fail_count} -lt ${PROBE_FAIL_MAX} ]]; then
    save_state_and_exit 0 \
      "${UNIT} holding healthy: probe failing (${probe_fail_count} consecutive)"
  fi
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
    # reloading, ...): neither confirm health nor count as down; hold.
    if [[ "${confirmed}" == "unhealthy" ]]; then
      save_state_and_exit 1 "${UNIT} holding fault: unit in transition (state=${active_state})"
    fi
    if [[ "${confirmed}" == "healthy" ]]; then
      save_state_and_exit 0 "${UNIT} in transition (state=${active_state}); holding healthy"
    fi
    save_state_and_exit 3 "${UNIT} in transition (state=${active_state}); no confirmed state yet"
    ;;
esac
