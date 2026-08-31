#!/usr/bin/env bash
#
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
#
# verify.sh - post-install verification gates for the Percona-backed NVSentinel deployment.
# Read-only. Waits on each gate with a timeout and prints a verdict table.
# Exit codes: 0 = all gates passed, 1 = at least one gate failed/timed out.
set -euo pipefail

NS="${NVSENTINEL_NAMESPACE:-nvsentinel}"
CR_TIMEOUT="${VERIFY_CR_TIMEOUT:-600s}"
JOB_TIMEOUT="${VERIFY_JOB_TIMEOUT:-600s}"
POD_TIMEOUT="${VERIFY_POD_TIMEOUT:-300s}"
PING_RETRIES="${VERIFY_PING_RETRIES:-30}"   # x10s = up to 5 minutes

FAIL=0
ROWS=()
row() {
  ROWS+=("$1|$2|$3")
  if [ "$1" = "FAIL" ]; then FAIL=$((FAIL + 1)); fi
}

echo "Gate 1/5: PerconaServerMongoDB reconciled (timeout $CR_TIMEOUT)..."
if kubectl wait psmdb/mongodb -n "$NS" --for=jsonpath='{.status.state}'=ready --timeout="$CR_TIMEOUT" >/dev/null 2>&1; then
  row PASS "psmdb CR ready" "status.state=ready"
else
  STATE="$(kubectl get psmdb mongodb -n "$NS" -o jsonpath='{.status.state}' 2>/dev/null || true)"
  ERRMSG="$(kubectl logs -n "$NS" deploy/nvsentinel-psmdb-operator --tail 200 2>/dev/null | grep -o '"error": *"[^"]*"' | tail -1 || true)"
  row FAIL "psmdb CR ready" "state='${STATE:-unknown}'. Last operator error: ${ERRMSG:-none captured}. If it mentions 'requested storage ... less than actual', see the volume-size section of the migration runbook."
fi

echo "Gate 2/5: database initialization job (timeout $JOB_TIMEOUT)..."
mongodb_job=$(kubectl get job -n "$NS" -l app.kubernetes.io/name=create-mongodb-database \
  --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)
if [[ -n "$mongodb_job" ]] && kubectl wait "job/$mongodb_job" -n "$NS" --for=condition=complete --timeout="$JOB_TIMEOUT" >/dev/null 2>&1; then
  row PASS "create-mongodb-database" "job $mongodb_job Complete"
else
  row FAIL "create-mongodb-database" "job not complete; check: kubectl logs -n $NS -l app.kubernetes.io/name=create-mongodb-database"
fi

echo "Gate 3/5: mongod pods ready (timeout $POD_TIMEOUT)..."
if kubectl wait pod -n "$NS" -l app.kubernetes.io/component=mongod --for=condition=ready --timeout="$POD_TIMEOUT" >/dev/null 2>&1; then
  COUNT="$(kubectl get pod -n "$NS" -l app.kubernetes.io/component=mongod --no-headers 2>/dev/null | wc -l || true)"
  NODES="$(kubectl get pod -n "$NS" -l app.kubernetes.io/component=mongod -o jsonpath='{range .items[*]}{.spec.nodeName} {end}' 2>/dev/null || true)"
  row PASS "mongod pods" "$COUNT ready on: $NODES"
else
  row FAIL "mongod pods" "not all ready; check: kubectl get pods -n $NS -l app.kubernetes.io/component=mongod"
fi

echo "Gate 4/5: connection endpoint..."
URI="$(kubectl get configmap mongodb-config -n "$NS" -o jsonpath='{.data.MONGODB_URI}' 2>/dev/null || true)"
case "$URI" in
  *mongodb-rs0*) row PASS "MONGODB_URI" "points at the Percona service (mongodb-rs0)" ;;
  *mongodb-headless*) row FAIL "MONGODB_URI" "still points at the Bitnami service (mongodb-headless); backend flags were not applied" ;;
  *) row FAIL "MONGODB_URI" "unexpected or missing value: '${URI:-<none>}'" ;;
esac

echo "Gate 5/5: datastore consumers connected (up to $PING_RETRIES x10s)..."
# Every deployed datastore consumer must confirm connectivity: on the restore
# path all of them must be healthy before restored events can be processed.
CONSUMERS=""
for D in health-events-analyzer fault-quarantine node-drainer fault-remediation; do
  if kubectl get deploy "$D" -n "$NS" >/dev/null 2>&1; then
    CONSUMERS="$CONSUMERS $D"
  fi
done
if [ -z "$CONSUMERS" ]; then
  row FAIL "consumer connectivity" "no datastore consumer deployment found (need at least health-events-analyzer or fault-quarantine); cannot confirm the datastore is reachable"
else
  PENDING="$CONSUMERS"
  I=0
  while [ "$I" -lt "$PING_RETRIES" ] && [ -n "$PENDING" ]; do
    STILL=""
    for D in $PENDING; do
      # Capture logs, then grep without -q: -q under pipefail can SIGPIPE the
      # producer and turn a successful match into a failed pipeline.
      LOGTXT="$(kubectl logs -n "$NS" "deploy/$D" --tail 200 2>/dev/null || true)"
      if printf '%s\n' "$LOGTXT" | grep "Successfully pinged" >/dev/null; then
        continue
      fi
      STILL="$STILL $D"
    done
    PENDING="$STILL"
    if [ -n "$PENDING" ]; then
      sleep 10
      I=$((I + 1))
    fi
  done
  if [ -z "$PENDING" ]; then
    row PASS "consumer connectivity" "all deployed consumers pinged:$CONSUMERS (a few early restarts are normal)"
  else
    FIRST="${PENDING# }"
    FIRST="${FIRST%% *}"
    ERRLINE="$(kubectl logs -n "$NS" "deploy/$FIRST" --tail 50 --previous 2>/dev/null | grep -oE '"error":"[^"]{1,300}' | tail -1 || true)"
    if [ -z "$ERRLINE" ]; then
      ERRLINE="$(kubectl logs -n "$NS" "deploy/$FIRST" --tail 20 2>/dev/null | grep -iE 'error|fatal' | tail -1 | cut -c1-200 || true)"
    fi
    HINT="see the runbook troubleshooting table"
    case "$ERRLINE" in
      *tls*|*TLS*|*certificate*|*handshake*|*"connection closed"*) HINT="TLS failures here usually mean leftover secrets from the old backend (rerun cleanup)" ;;
      *NoPrimary*|*RSGhost*|*"server selection"*) HINT="replica set has no primary; check the psmdb resource and operator logs" ;;
    esac
    row FAIL "consumer connectivity" "never pinged:$PENDING. Last error from $FIRST: ${ERRLINE:-none}. Hint: $HINT."
  fi
fi

echo
echo "NVSentinel Percona migration verification (namespace=$NS)"
echo "----------------------------------------------------------"
printf '%-8s %-26s %s\n' "STATUS" "GATE" "DETAIL"
for R in "${ROWS[@]}"; do
  IFS='|' read -r S G D <<<"$R"
  printf '%-8s %-26s %s\n' "$S" "$G" "$D"
done
echo "----------------------------------------------------------"
if [ "$FAIL" -gt 0 ]; then
  echo "VERDICT: FAILED ($FAIL gate(s)). See the runbook troubleshooting table."
  exit 1
fi
echo "VERDICT: MIGRATED. All gates passed."
exit 0
