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
# Run by `make helm-test` (and therefore by the helm-charts job in lint-test.yml), so it is
# exercised on every PR that runs the chart tests.
#
# Usage: node-condition-cleanup_test.sh

set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../files" && pwd)/node-condition-cleanup.sh"
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT

# Stub kubectl: three nodes, each carrying the deprecated condition. A patch fails for the nodes
# listed in FAIL_NODES (space separated); with PATCH_ERR=notfound the failure is a NotFound
# (node deleted mid-run). A get fails for the nodes in GET_FAIL_NODES, with GET_ERR=notfound for
# a NotFound (silent success under --ignore-not-found, as in kubectl) and a Forbidden error
# otherwise. With GET_WARN set, a successful get also writes a server warning to stderr. NODES
# overrides the node list.
cat > "$STUB_DIR/kubectl" <<'STUB'
#!/bin/bash
case "$1" in
get)
  if [ "$2" = "nodes" ]; then
    echo "${NODES:-node-a node-b node-c}"
  else
    for n in $GET_FAIL_NODES; do
      if [ "$3" = "$n" ]; then
        if [ "$GET_ERR" = "notfound" ]; then
          case " $* " in *" --ignore-not-found "*) exit 0 ;; esac
          echo "Error from server (NotFound): nodes \"$n\" not found" >&2
        else
          echo "Error from server (Forbidden): nodes \"$n\" is forbidden" >&2
        fi
        exit 1
      fi
    done
    if [ -n "$GET_WARN" ]; then
      echo "Warning: v1 Node is deprecated" >&2
    fi
    echo '{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Old","status":"True"}]}}'
  fi
  ;;
patch)
  for n in $FAIL_NODES; do
    if [ "$3" = "$n" ]; then
      if [ "$PATCH_ERR" = "notfound" ]; then
        echo "Error from server (NotFound): nodes \"$n\" not found" >&2
      else
        echo "Error from server (Forbidden): nodes/status \"$n\" is forbidden" >&2
      fi
      exit 1
    fi
  done
  ;;
esac
STUB
chmod +x "$STUB_DIR/kubectl"

run_cleanup() {
  PATH="$STUB_DIR:$PATH" DEPRECATED_CONDITIONS="Old" FAIL_NODES="$1" GET_FAIL_NODES="${2:-}" \
    GET_ERR="${GET_ERR:-}" GET_WARN="${GET_WARN:-}" \
    NODES="${NODES:-}" PATCH_ERR="${PATCH_ERR:-}" bash "$SCRIPT"
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

# A node deleted between the get and the patch must not fail the hook.
status=0
output=$(PATCH_ERR=notfound run_cleanup "node-b") || status=$?
if [ "$status" -ne 0 ]; then
  echo "FAIL: expected exit 0 when a node disappears before the patch, got $status"
  echo "$output"
  exit 1
fi

# A node already gone at get time is skipped too.
status=0
output=$(GET_ERR=notfound run_cleanup "" "node-b") || status=$?
if [ "$status" -ne 0 ]; then
  echo "FAIL: expected exit 0 when a node disappears before the get, got $status"
  echo "$output"
  exit 1
fi

# A get that fails for any other reason (e.g. RBAC) must fail the hook, not read as "no conditions".
status=0
output=$(run_cleanup "" "node-b") || status=$?
if [ "$status" -eq 0 ] || ! grep -q "1 of 3 nodes failed" <<< "$output"; then
  echo "FAIL: expected exit 1 and '1 of 3 nodes failed' when a get is forbidden (status $status)"
  echo "$output"
  exit 1
fi

# A warning on stderr from a successful get must not be mixed into the node JSON.
status=0
output=$(GET_WARN=1 run_cleanup "") || status=$?
if [ "$status" -ne 0 ] || [ "$(grep -c "removed 1 condition" <<< "$output")" -ne 3 ]; then
  echo "FAIL: expected all three nodes cleaned when get emits a warning (status $status)"
  echo "$output"
  exit 1
fi

# A node whose name contains "notfound" must not turn a Forbidden patch into a skip.
status=0
output=$(NODES="worker-notfound" run_cleanup "worker-notfound") || status=$?
if [ "$status" -eq 0 ] || ! grep -q "1 of 1 nodes failed" <<< "$output"; then
  echo "FAIL: expected exit 1 when patching 'worker-notfound' is forbidden (status $status)"
  echo "$output"
  exit 1
fi

echo "PASS"
