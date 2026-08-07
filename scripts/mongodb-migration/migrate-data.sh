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
# migrate-data.sh - optional data preservation for the Bitnami -> Percona migration.
# dump:    streams a mongodump archive out of the current backend (auto-detects Bitnami
#          or Percona; always excludes ResumeTokens, which are only valid on the cluster
#          that created them).
# restore: streams the archive into the Percona mongod. ObjectIDs are preserved, so node
#          annotations and CR names that reference event IDs stay valid.
#
# Usage:
#   migrate-data.sh dump    <archive-file> [--stop-writers]
#                                               (run while the old backend is up;
#                                               --stop-writers scales the reference-writing
#                                               components to zero and waits for their pods
#                                               to terminate before dumping)
#   migrate-data.sh restore <archive-file>      (run after verify.sh passes on Percona)
#
# Exit codes: 0 = success, 1 = error,
#             3 = refused (dump: writers still active; restore: consumers not ready).
set -euo pipefail

NS="${NVSENTINEL_NAMESPACE:-nvsentinel}"
DB="${NVSENTINEL_DATABASE:-HealthEventsDatabase}"
TOKEN_COLLECTION="${NVSENTINEL_TOKEN_COLLECTION:-ResumeTokens}"

MODE="${1:-}"
ARCHIVE="${2:-}"
STOP_WRITERS=0
for ARG in "${@:3}"; do
  case "$ARG" in
    --stop-writers) STOP_WRITERS=1 ;;
    *)
      echo "usage: $0 dump|restore <archive-file> [--stop-writers]" >&2
      exit 1
      ;;
  esac
done
if [ -z "$MODE" ] || [ -z "$ARCHIVE" ]; then
  echo "usage: $0 dump|restore <archive-file> [--stop-writers]" >&2
  exit 1
fi
if [ "$STOP_WRITERS" -eq 1 ] && [ "$MODE" != "dump" ]; then
  echo "ERROR: --stop-writers applies to dump only; the restore needs the consumers running." >&2
  exit 1
fi

case "$MODE" in
dump)
  if [ "$STOP_WRITERS" -eq 1 ]; then
    # Automation of the manual pre-dump step: scale the reference writers to
    # zero and wait for their pods to be deleted (a pod in its termination
    # grace period can still write). The fail-closed guard below still runs
    # afterwards and stays the source of truth.
    for D in fault-quarantine node-drainer fault-remediation; do
      if kubectl get deploy "$D" -n "$NS" >/dev/null 2>&1; then
        echo "Stopping writer $D (scale to 0, wait for pod deletion)..."
        kubectl scale deploy "$D" -n "$NS" --replicas=0
        kubectl wait --for=delete pod -l "app.kubernetes.io/name=$D" -n "$NS" --timeout=120s
      fi
    done
  fi
  # Guard: writers that create references to events (annotations, remediation CRs)
  # must be stopped before the dump. An event written after the dump is absent
  # from the archive, and a reference created for it would dangle after restore.
  # This check fails closed: a deployment must be provably absent or provably at
  # zero (desired and remaining replicas), and any query error aborts the dump.
  ACTIVE_WRITERS=""
  for D in fault-quarantine node-drainer fault-remediation; do
    STATE=""
    if ! STATE="$(kubectl get deploy "$D" -n "$NS" -o jsonpath='{.spec.replicas}/{.status.replicas}' 2>&1)"; then
      case "$STATE" in
        *NotFound*) continue ;;  # deployment not part of this installation
        *)
          echo "ERROR: could not determine the state of deployment '$D': $STATE" >&2
          echo "Refusing to dump while writer quiescence cannot be proved." >&2
          exit 1
          ;;
      esac
    fi
    DESIRED="${STATE%%/*}"
    REMAINING="${STATE##*/}"
    if [ "${DESIRED:-0}" != "0" ]; then
      ACTIVE_WRITERS="$ACTIVE_WRITERS $D"
      continue
    elif [ -n "$REMAINING" ] && [ "$REMAINING" != "0" ]; then
      ACTIVE_WRITERS="$ACTIVE_WRITERS $D"
      continue
    fi
    # Deployment status drops terminating pods immediately, but a pod in its
    # termination grace period can still write references. Count real pods.
    if ! PODS="$(kubectl get pods -n "$NS" -l "app.kubernetes.io/name=$D" --no-headers 2>&1)"; then
      echo "ERROR: could not list pods for '$D': $PODS" >&2
      echo "Refusing to dump while writer quiescence cannot be proved." >&2
      exit 1
    fi
    if [ -n "$PODS" ]; then
      ACTIVE_WRITERS="$ACTIVE_WRITERS $D(terminating)"
    fi
  done
  if [ -n "$ACTIVE_WRITERS" ]; then
    if [ "${DUMP_WITHOUT_FAULT_STATE_GUARANTEES:-0}" = "1" ]; then
      echo "WARNING: dumping with active writers:$ACTIVE_WRITERS" >&2
      echo "This archive is NOT suitable for the fault-state-preserving restore path:" >&2
      echo "references created after the dump may point at documents it does not contain." >&2
    else
      echo "REFUSED: these components are still running and can create references to" >&2
      echo "events written after the dump:$ACTIVE_WRITERS" >&2
      echo "Re-run with --stop-writers to have this script scale them to zero and wait" >&2
      echo "for their pods to terminate, or do it yourself:" >&2
      for D in $ACTIVE_WRITERS; do
        echo "  kubectl scale deploy ${D%%(*} -n $NS --replicas=0" >&2
        echo "  kubectl wait --for=delete pod -l app.kubernetes.io/name=${D%%(*} -n $NS --timeout=120s" >&2
      done
      echo "(Set DUMP_WITHOUT_FAULT_STATE_GUARANTEES=1 only for a plain backup that" >&2
      echo "will not be used to preserve fault state.)" >&2
      exit 3
    fi
  fi

  # Detect the source backend: Bitnami (statefulset/mongodb) or Percona (psmdb/mongodb).
  # Server certificates on both sides are issued for pod/service FQDNs, never localhost.
  # Credentials are passed to the pod over stdin, keeping them out of the local command
  # line and the API-server exec audit record (inside the pod, the tools still receive
  # them as arguments), and keeping shell-significant characters intact.
  # The dump is written to a temp file and renamed on success, so a failed re-run never
  # destroys a previous good archive.
  TMP_ARCHIVE="$ARCHIVE.tmp"
  if kubectl get statefulset mongodb -n "$NS" >/dev/null 2>&1; then
    PASSWORD="$(kubectl get secret mongodb -n "$NS" -o jsonpath='{.data.mongodb-root-password}' | base64 -d || true)"
    if [ -z "$PASSWORD" ]; then
      echo "ERROR: could not read the Bitnami root password (secret 'mongodb')." >&2
      exit 1
    fi
    RC=0
    DUMP_HOST="mongodb-0.mongodb-headless.$NS.svc.cluster.local"
    echo "Dumping $DB (excluding $TOKEN_COLLECTION) from Bitnami mongodb-0..."
    printf '%s\n' "$PASSWORD" | kubectl exec -i -n "$NS" mongodb-0 -c mongodb -- bash -c \
      "IFS= read -r MPW; mongodump --host '$DUMP_HOST' --db '$DB' --excludeCollection '$TOKEN_COLLECTION' \
        --username root --password \"\$MPW\" --authenticationDatabase admin \
        --ssl --sslCAFile certs/mongodb-ca-cert --sslPEMKeyFile certs/mongodb.pem \
        --archive --quiet" > "$TMP_ARCHIVE" || RC=$?
  elif kubectl get psmdb mongodb -n "$NS" >/dev/null 2>&1; then
    PU="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_BACKUP_USER}' | base64 -d || true)"
    PP="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_BACKUP_PASSWORD}' | base64 -d || true)"
    if [ -z "$PU" ] || [ -z "$PP" ]; then
      # Fall back to the database admin user if the backup user is absent.
      PU="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_USER}' | base64 -d || true)"
      PP="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d || true)"
    fi
    if [ -z "$PU" ] || [ -z "$PP" ]; then
      echo "ERROR: could not read Percona credentials (secret 'internal-mongodb-users')." >&2
      exit 1
    fi
    RC=0
    DUMP_HOST="mongodb-rs0-0.mongodb-rs0.$NS.svc.cluster.local"
    echo "Dumping $DB (excluding $TOKEN_COLLECTION) from Percona mongodb-rs0-0..."
    printf '%s\n%s\n' "$PU" "$PP" | kubectl exec -i -n "$NS" mongodb-rs0-0 -c mongod -- sh -c \
      "IFS= read -r MUSER; IFS= read -r MPW; \
       cat /etc/mongodb-ssl-internal/tls.crt /etc/mongodb-ssl-internal/tls.key > /tmp/dump.pem; \
       mongodump --host '$DUMP_HOST' --db '$DB' --excludeCollection '$TOKEN_COLLECTION' \
        --username \"\$MUSER\" --password \"\$MPW\" --authenticationDatabase admin \
        --ssl --sslCAFile /etc/mongodb-ssl-internal/ca.crt --sslPEMKeyFile /tmp/dump.pem \
        --archive --quiet" > "$TMP_ARCHIVE" || RC=$?
  else
    echo "ERROR: no MongoDB backend found in '$NS' (neither statefulset/mongodb nor psmdb/mongodb)." >&2
    exit 1
  fi
  if [ "$RC" -ne 0 ] || [ ! -s "$TMP_ARCHIVE" ]; then
    echo "ERROR: dump failed (rc=$RC, archive size $(wc -c < "$TMP_ARCHIVE" 2>/dev/null || echo 0) bytes)." >&2
    rm -f "$TMP_ARCHIVE"
    exit 1
  fi
  mv "$TMP_ARCHIVE" "$ARCHIVE"
  echo "Dump complete: $ARCHIVE ($(wc -c < "$ARCHIVE") bytes)."
  echo "Safe to proceed with the migration. Restore AFTER verify.sh passes on Percona."
  ;;
restore)
  if [ ! -s "$ARCHIVE" ]; then
    echo "ERROR: archive '$ARCHIVE' missing or empty." >&2
    exit 1
  fi
  # Guard: unlike the dump, the restore needs the consumers UP, not down.
  # fault-quarantine and health-events-analyzer see restored events only through
  # live change streams (they have no cold-start replay), so a consumer that is
  # down during the restore never processes what it inserts. Concurrent live
  # writes are safe: restored documents keep their original ObjectIDs and new
  # events get fresh ones. This enforces the runbook ordering: verify.sh passes,
  # then restore.
  NOT_READY=""
  for D in health-events-analyzer fault-quarantine node-drainer fault-remediation; do
    READY=""
    if ! READY="$(kubectl get deploy "$D" -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>&1)"; then
      case "$READY" in
        *NotFound*) continue ;;  # deployment not part of this installation
        *)
          echo "ERROR: could not determine the state of deployment '$D': $READY" >&2
          echo "Refusing to restore while consumer readiness cannot be proved." >&2
          exit 1
          ;;
      esac
    fi
    if [ -z "$READY" ] || [ "$READY" = "0" ]; then
      NOT_READY="$NOT_READY $D"
    fi
  done
  if [ -n "$NOT_READY" ]; then
    if [ "${RESTORE_WITHOUT_LIVE_CONSUMERS:-0}" = "1" ]; then
      echo "WARNING: restoring while these consumers are not ready:$NOT_READY" >&2
      echo "Change-stream consumers that are down never process the events inserted now." >&2
    else
      echo "REFUSED: these datastore consumers are not ready:$NOT_READY" >&2
      echo "fault-quarantine and health-events-analyzer receive restored events only" >&2
      echo "through live change streams, so they must be running and connected before" >&2
      echo "the restore. Run verify.sh and restore once all of its gates pass." >&2
      echo "(Set RESTORE_WITHOUT_LIVE_CONSUMERS=1 only for a plain data restore whose" >&2
      echo "immediate processing does not matter.)" >&2
      exit 3
    fi
  fi
  # Percona: databaseAdmin credentials from internal-mongodb-users, internal TLS certs.
  PU="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_USER}' | base64 -d || true)"
  PP="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d || true)"
  if [ -z "$PU" ] || [ -z "$PP" ]; then
    echo "ERROR: could not read Percona credentials (secret 'internal-mongodb-users')." >&2
    exit 1
  fi
  # The internal certificate is issued for the pod/service FQDNs, not localhost.
  # Credentials travel over stdin ahead of the archive bytes (kept off the local
  # command line and out of the exec audit record): the two 'read' calls consume
  # the credential lines and mongorestore reads the remainder of the stream.
  RC=0
  RESTORE_HOST="mongodb-rs0-0.mongodb-rs0.$NS.svc.cluster.local"
  echo "Restoring $DB into mongodb-rs0-0 (ObjectIDs preserved)..."
  { printf '%s\n%s\n' "$PU" "$PP"; cat "$ARCHIVE"; } | \
    kubectl exec -i -n "$NS" mongodb-rs0-0 -c mongod -- sh -c \
    "IFS= read -r MUSER; IFS= read -r MPW; \
     cat /etc/mongodb-ssl-internal/tls.crt /etc/mongodb-ssl-internal/tls.key > /tmp/restore.pem; \
     mongorestore --host '$RESTORE_HOST' --nsInclude '$DB.*' \
      --username \"\$MUSER\" --password \"\$MPW\" --authenticationDatabase admin \
      --ssl --sslCAFile /etc/mongodb-ssl-internal/ca.crt --sslPEMKeyFile /tmp/restore.pem \
      --archive --quiet" || RC=$?
  if [ "$RC" -ne 0 ]; then
    echo "ERROR: restore failed (rc=$RC)." >&2
    exit 1
  fi
  echo "Restore complete. fault-quarantine and health-events-analyzer are processing the"
  echo "restored events through their change streams. Now restart the datastore consumers"
  echo "(kubectl rollout restart on the consumer deployments) so node-drainer and"
  echo "fault-remediation also re-query for unfinished events on startup, and expect the"
  echo "event exporter to re-export restored events to its sink (duplicates downstream)."
  ;;
*)
  echo "usage: $0 dump|restore <archive-file>" >&2
  exit 1
  ;;
esac
