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

# Runs a command again when it fails, for CI calls that fail on a transient network error.
#
# Usage: scripts/retry.sh <command> [args...]
#
# RETRY_ATTEMPTS sets the number of attempts, from 1 to 10 (default 3). RETRY_DELAY sets the wait in seconds
# before the second attempt (default 10). The wait doubles after each failed attempt.
# The script exits with the status of the last attempt.

set -uo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

attempts="${RETRY_ATTEMPTS:-3}"
delay="${RETRY_DELAY:-10}"

# The delay doubles after each attempt, so the attempt limit keeps it far from an integer overflow.
if ! [[ "${attempts}" =~ ^([1-9]|10)$ && "${delay}" =~ ^[0-9]{1,4}$ ]]; then
  echo "retry: RETRY_ATTEMPTS must be from 1 to 10 and RETRY_DELAY from 0 to 9999" >&2
  exit 2
fi

for ((attempt = 1; ; attempt++)); do
  "$@"
  status=$?

  if [ "${status}" -eq 0 ]; then
    exit 0
  fi

  if [ "${attempt}" -ge "${attempts}" ]; then
    echo "retry: '$*' failed with status ${status} after ${attempt} attempts" >&2
    exit "${status}"
  fi

  echo "::warning::retry: '$*' failed with status ${status} (attempt ${attempt}/${attempts}). Trying again in ${delay}s." >&2
  sleep "${delay}"
  delay=$((delay * 2))
done
