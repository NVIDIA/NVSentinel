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
# cleanup.sh - post-uninstall cleanup for the NVSentinel MongoDB Bitnami -> Percona migration.
# Deletes the objects that survive 'helm uninstall' and (optionally) stale fault-handling state.
#
# Usage: cleanup.sh [--dry-run] [--clear-fault-state] [--yes]
#   --dry-run            print what would be deleted, delete nothing
#   --clear-fault-state  also remove quarantine node annotations, remediation CRs,
#                        and event-labeled log-collector jobs. Use ONLY on the clean
#                        path (no restore); the default preserve path keeps this state
#   --yes                skip the interactive confirmation
#
# Exit codes: 0 = cleanup complete and verified, 1 = error or leftovers remain,
#             3 = refused (release still installed, or not confirmed)
set -euo pipefail

NS="${NVSENTINEL_NAMESPACE:-nvsentinel}"
RELEASE="${NVSENTINEL_RELEASE:-nvsentinel}"
DRY_RUN=0
CLEAR_FAULT_STATE=0
ASSUME_YES=0

for ARG in "$@"; do
  case "$ARG" in
    --dry-run) DRY_RUN=1 ;;
    --clear-fault-state) CLEAR_FAULT_STATE=1 ;;
    --yes) ASSUME_YES=1 ;;
    *) echo "unknown argument: $ARG" >&2; exit 1 ;;
  esac
done

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "DRY-RUN: $*"
  else
    echo "+ $*"
    "$@"
  fi
}

# --- guard: refuse to clean under a live release --------------------------------
# Distinguish "release not found" (safe to proceed) from helm being broken or
# pointed at the wrong context (refuse: the interlock cannot be verified).
HELM_ERR=""
if HELM_ERR="$(helm status "$RELEASE" -n "$NS" 2>&1 >/dev/null)"; then
  echo "REFUSED: helm release '$RELEASE' still exists in '$NS'." >&2
  echo "Run 'helm uninstall $RELEASE -n $NS' first. Cleanup only runs on an uninstalled release." >&2
  exit 3
elif ! printf '%s' "$HELM_ERR" | grep -qi "not found"; then
  echo "REFUSED: could not verify the release state: $HELM_ERR" >&2
  exit 3
fi

# --- guard: confirmation ---------------------------------------------------------
if [ "$DRY_RUN" -eq 0 ] && [ "$ASSUME_YES" -eq 0 ]; then
  if [ -t 0 ]; then
    echo "This permanently deletes the MongoDB PVCs (all health event data), TLS secrets,"
    echo "and runtime state in namespace '$NS'. Type YES to continue:"
    read -r ANSWER || { echo "aborted."; exit 3; }
    [ "$ANSWER" = "YES" ] || { echo "aborted."; exit 3; }
  else
    echo "REFUSED: non-interactive run without --yes." >&2
    exit 3
  fi
fi

# --- collect the target object lists ---------------------------------------------
# PVCs: Bitnami volumes carry app.kubernetes.io/name=mongodb, Percona (operator-created)
# volumes carry app.kubernetes.io/name=percona-server-mongodb. Cover both so the script
# works for the reverse migration and for cleaning up failed mixed states.
PVCS="$(kubectl get pvc -n "$NS" -l 'app.kubernetes.io/name in (mongodb, percona-server-mongodb)' -o name 2>/dev/null || true)"

# Secrets: fixed names plus per-replica server certs discovered by pattern
# (one mongo-server-cert-<n> per Bitnami replica; discovery avoids hardcoding the count).
FIXED_SECRETS="mongodb mongo-ca-secret mongo-root-ca-secret mongo-app-client-cert-secret"
SERVER_CERTS="$(kubectl get secrets -n "$NS" -o name 2>/dev/null | sed 's|^secret/||' | grep -E '^mongo-server-cert-[0-9]+$' || true)"
# Stale Percona-side objects from any earlier attempt (harmless if absent).
STALE_PERCONA="internal-mongodb-users percona-server-mongodb-users mongodb-encryption-key mongodb-keyfile mongodb-ssl mongodb-ssl-internal mongodb-ca-cert"

echo "== Deleting MongoDB PVCs =="
if [ -n "$PVCS" ]; then
  # shellcheck disable=SC2086
  run kubectl delete -n "$NS" $PVCS
else
  echo "(none found)"
fi

echo "== Deleting MongoDB secrets =="
# shellcheck disable=SC2086
run kubectl delete secret $FIXED_SECRETS $STALE_PERCONA -n "$NS" --ignore-not-found=true
if [ -n "$SERVER_CERTS" ]; then
  # shellcheck disable=SC2086
  run kubectl delete secret $SERVER_CERTS -n "$NS" --ignore-not-found=true
fi

echo "== Deleting runtime state ConfigMaps =="
run kubectl delete configmap resume-control circuit-breaker -n "$NS" --ignore-not-found=true

if [ "$CLEAR_FAULT_STATE" -eq 1 ]; then
  echo "== Clearing fault-handling state (annotations, CRs, jobs) =="
  run kubectl annotate nodes --all \
    quarantineHealthEvent- quarantineHealthEventAppliedTaints- \
    quarantineHealthEventAppliedLabels- quarantineHealthEventIsCordoned- \
    quarantineHealthEventCordonPreExisting- latestFaultRemediationState-
  # These CRDs are cluster scoped (the namespace flag is ignored), so scope the
  # deletion to NVSentinel-owned resources via the label fault-remediation stamps
  # on everything it creates. Unlabeled leftovers are listed, never deleted.
  MANAGED="app.kubernetes.io/managed-by=nvsentinel"
  # Janitor adds finalizers to some of these CRs and is uninstalled by now, so a
  # waiting delete would hang forever. Delete without waiting, then check below.
  for KIND in rebootnodes terminatenodes gpuresets externalremediationrequests; do
    if kubectl get "$KIND" >/dev/null 2>&1; then
      run kubectl delete "$KIND" -l "$MANAGED" --ignore-not-found=true --wait=false
      UNLABELED="$(kubectl get "$KIND" -l '!app.kubernetes.io/managed-by' --no-headers 2>/dev/null | awk '{print $1}' || true)"
      if [ -n "$UNLABELED" ]; then
        echo "WARNING: $KIND resources without the $MANAGED label were left in place;" >&2
        echo "review and delete them manually if they belong to this installation:" >&2
        for R in $UNLABELED; do echo "  $R" >&2; done
      fi
    fi
  done
  if [ "$DRY_RUN" -eq 0 ]; then
    # Give deletions a moment, then flag CRs stuck on finalizers (their controller
    # is uninstalled and will never release them).
    DEADLINE=$((SECONDS + 30))
    STUCK=""
    while [ "$SECONDS" -lt "$DEADLINE" ]; do
      STUCK=""
      for KIND in rebootnodes terminatenodes gpuresets externalremediationrequests; do
        L="$(kubectl get "$KIND" -l "$MANAGED" -o name 2>/dev/null || true)"
        if [ -n "$L" ]; then STUCK="$STUCK $L"; fi
      done
      if [ -z "$STUCK" ]; then break; fi
      sleep 3
    done
    if [ -n "$STUCK" ]; then
      echo "FAILED: these resources are stuck deleting (their finalizer owner, janitor, is uninstalled):" >&2
      for R in $STUCK; do echo "  $R" >&2; done
      echo "Remove the finalizers manually, then re-run this script, e.g.:" >&2
      echo "  kubectl patch <resource> --type=merge -p '{\"metadata\":{\"finalizers\":[]}}'" >&2
      exit 1
    fi
  fi
  # Jobs are namespaced and dgxc.nvidia.com/event-id is only stamped by
  # fault-remediation's log-collector jobs, so namespace + label is the ownership
  # boundary here. (The CRs above cannot be scoped by instance yet: fault-remediation
  # does not stamp an instance label; candidate follow-up for multi-install clusters.)
  run kubectl delete jobs -n "$NS" -l dgxc.nvidia.com/event-id --ignore-not-found=true
  echo "NOTE: cordons and NVSentinel-applied taints are left in place on purpose."
  echo "Review each previously quarantined node and return it to service yourself"
  echo "(kubectl uncordon, remove taints) once you are confident it is healthy."
else
  echo "== Keeping fault-handling state (default; pass --clear-fault-state only on the clean, no-restore path) =="
fi

# --- verification ------------------------------------------------------------------
if [ "$DRY_RUN" -eq 1 ]; then
  echo "DRY-RUN complete. Nothing was deleted."
  exit 0
fi

echo "== Verifying nothing is left =="
LEFT=""
L="$(kubectl get pvc -n "$NS" -l 'app.kubernetes.io/name in (mongodb, percona-server-mongodb)' -o name 2>/dev/null || true)"
if [ -n "$L" ]; then LEFT="$LEFT $L"; fi
for S in $FIXED_SECRETS $STALE_PERCONA $SERVER_CERTS; do
  if kubectl get secret "$S" -n "$NS" >/dev/null 2>&1; then LEFT="$LEFT secret/$S"; fi
done
for C in resume-control circuit-breaker; do
  if kubectl get configmap "$C" -n "$NS" >/dev/null 2>&1; then LEFT="$LEFT configmap/$C"; fi
done
# Catch anything matching the server-cert pattern that appeared between collect and delete.
L="$(kubectl get secrets -n "$NS" -o name 2>/dev/null | grep -E '^secret/mongo-server-cert-[0-9]+$' || true)"
if [ -n "$L" ]; then LEFT="$LEFT $L"; fi

if [ -n "$LEFT" ]; then
  echo "FAILED: leftovers remain:$LEFT" >&2
  echo "Do not install the new backend until these are gone (leftover TLS secrets cause" >&2
  echo "opaque connection-closed crash loops on the next install)." >&2
  exit 1
fi
echo "Cleanup verified: no MongoDB leftovers in '$NS'."
exit 0
