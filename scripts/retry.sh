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
# The script runs the command up to 3 times. It waits 10 s before the second attempt and 20 s before
# the third. It exits with the status of the last attempt.

set -uo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

attempts=3
delay=10

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
