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

# NPD custom plugin: nvidia-fabricmanager unit presence (ADR-050).
#
# Install this rule only on fleets where the operator declares Fabric Manager
# required AND host-systemd-managed (NVSwitch platforms with the host unit):
# it reports unhealthy when the unit is absent (LoadState=not-found),
# distinguishing misconfiguration from "disabled on purpose". Fleets running
# FM inside the GPU Operator driver container must omit this rule — no host
# unit exists there even though FM is running.
#
# Exit codes follow the NPD custom-plugin protocol:
#   0 = healthy, 1 = unhealthy, 3 = unknown.
# Probe failures never clear a fault: the script holds its last confirmed
# state (unhealthy indefinitely; healthy for PROBE_FAIL_MAX consecutive
# failures, then unknown). Recovery always requires a confirming probe.

set -o nounset
set -o pipefail

UNIT="nvidia-fabricmanager"
STATE_DIR="/var/run/nvsentinel/npd"
STATE_FILE="${STATE_DIR}/fm-presence.state"
PROBE_FAIL_MAX=4
# Bound the probe below the NPD rule timeout (12s) so a wedged systemd/D-Bus
# reports as this script's deliberate exit, not an NPD plugin timeout.
PROBE_TIMEOUT_SECONDS=8

confirmed="none"
probe_fail_count=0

if [[ -r "${STATE_FILE}" ]]; then
  while IFS='=' read -r key value; do
    case "${key}" in
      confirmed) confirmed="${value}" ;;
      probe_fail_count) probe_fail_count="${value}" ;;
    esac
  done < "${STATE_FILE}"
fi
if ! [[ "${confirmed}" =~ ^(healthy|unhealthy|none)$ && "${probe_fail_count}" =~ ^[0-9]+$ ]]; then
  confirmed="none"; probe_fail_count=0
fi

save_state_and_exit() { # $1=exit code, $2=message
  local rc="${1}" msg="${2}" tmp_file
  if mkdir -p "${STATE_DIR}" 2>/dev/null && chmod 0700 "${STATE_DIR}" 2>/dev/null &&
     tmp_file=$(mktemp "${STATE_DIR}/.fm-presence.XXXXXX" 2>/dev/null); then
    chmod 0600 "${tmp_file}"
    {
      echo "confirmed=${confirmed}"
      echo "probe_fail_count=${probe_fail_count}"
    } > "${tmp_file}"
    mv -f "${tmp_file}" "${STATE_FILE}"
  fi
  echo "${msg}"
  exit "${rc}"
}

load_state_prop=$(timeout "${PROBE_TIMEOUT_SECONDS}" \
  systemctl show "${UNIT}" --property=LoadState --value --no-pager 2>/dev/null)
rc=$?

if [[ ${rc} -ne 0 || -z "${load_state_prop}" ]]; then
  # Probe failure: hold the last confirmed state; never clear a fault.
  probe_fail_count=$((probe_fail_count + 1))
  if [[ "${confirmed}" == "unhealthy" ]]; then
    save_state_and_exit 1 \
      "${UNIT} holding fault: probe failing (${probe_fail_count} consecutive), last confirmed not installed"
  fi
  if [[ "${confirmed}" == "healthy" && ${probe_fail_count} -lt ${PROBE_FAIL_MAX} ]]; then
    save_state_and_exit 0 \
      "${UNIT} holding healthy: probe failing (${probe_fail_count} consecutive)"
  fi
  save_state_and_exit 3 "could not observe ${UNIT}: systemctl unavailable (rc=${rc})"
fi

probe_fail_count=0

if [[ "${load_state_prop}" == "not-found" ]]; then
  confirmed="unhealthy"
  save_state_and_exit 1 "${UNIT} unit is not installed on a host that requires Fabric Manager"
fi

confirmed="healthy"
save_state_and_exit 0 "${UNIT} unit is present (LoadState=${load_state_prop})"
