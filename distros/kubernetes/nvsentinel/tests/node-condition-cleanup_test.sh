#!/bin/bash
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

# Checks the exit status of files/node-condition-cleanup.sh with a stubbed kubectl.
# Requires bash and jq.
#
# Usage: node-condition-cleanup_test.sh

set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../files" && pwd)/node-condition-cleanup.sh"
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT

# Stub kubectl: three nodes, each carrying the deprecated condition. A patch fails for the nodes
# listed in FAIL_NODES (space separated).
cat > "$STUB_DIR/kubectl" <<'STUB'
#!/bin/bash
case "$1" in
get)
  if [ "$2" = "nodes" ]; then
    echo "node-a node-b node-c"
  else
    echo '{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Old","status":"True"}]}}'
  fi
  ;;
patch)
  for n in $FAIL_NODES; do
    if [ "$3" = "$n" ]; then
      echo "Error from server (Forbidden): nodes/status \"$n\" is forbidden" >&2
      exit 1
    fi
  done
  ;;
esac
STUB
chmod +x "$STUB_DIR/kubectl"

run_cleanup() {
  PATH="$STUB_DIR:$PATH" DEPRECATED_CONDITIONS="Old" FAIL_NODES="$1" bash "$SCRIPT"
}

status=0
output=$(run_cleanup "") || status=$?
if [ "$status" -ne 0 ]; then
  echo "FAIL: expected exit 0 when every node is patched, got $status"
  echo "$output"
  exit 1
fi

status=0
output=$(run_cleanup "node-b") || status=$?
if [ "$status" -eq 0 ]; then
  echo "FAIL: expected a non-zero exit when a node patch fails"
  echo "$output"
  exit 1
fi
if ! grep -q "1 of 3 nodes failed" <<< "$output"; then
  echo "FAIL: expected the failure summary '1 of 3 nodes failed'"
  echo "$output"
  exit 1
fi

status=0
output=$(run_cleanup "node-a node-b node-c") || status=$?
if [ "$status" -eq 0 ] || ! grep -q "3 of 3 nodes failed" <<< "$output"; then
  echo "FAIL: expected exit 1 and '3 of 3 nodes failed' when every patch fails (status $status)"
  echo "$output"
  exit 1
fi

echo "PASS"
