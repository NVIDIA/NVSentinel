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

# Applies the PostgreSQL schema migrations to the in-cluster Tilt database.
# Migrations already recorded in nvsentinel_schema_migrations are skipped.
#
# This script is only used in the Tilt dev environment. Production databases
# get their migrations from the operator's database release process.
set -euo pipefail

NAMESPACE="nvsentinel"
POD="nvsentinel-postgresql-0"
CONTAINER="postgresql"
# The pod's pg_hba trusts loopback connections without TLS.
CONNINFO="host=127.0.0.1 sslmode=disable user=postgresql dbname=nvsentinel"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATION_DIR="${SCRIPT_DIR}/../store-client/pkg/datastore/providers/postgresql/migrations"

psql_exec() {
    kubectl exec -i -n "${NAMESPACE}" "${POD}" -c "${CONTAINER}" -- \
        psql -v ON_ERROR_STOP=1 -X -q "${CONNINFO}" "$@"
}

kubectl wait --for=condition=Ready --timeout=300s -n "${NAMESPACE}" "pod/${POD}"

applied_version="$(psql_exec -tA -c "
    SELECT CASE
        WHEN to_regclass('nvsentinel_schema_migrations') IS NULL THEN 0
        ELSE (SELECT COALESCE(MAX(version), 0) FROM nvsentinel_schema_migrations)
    END")"
echo "Current PostgreSQL schema version: ${applied_version}"

shopt -s nullglob
migration_files=("${MIGRATION_DIR}"/[0-9][0-9][0-9][0-9][0-9]_*.sql)
if (( ${#migration_files[@]} == 0 )); then
    echo "ERROR: no PostgreSQL migrations found in ${MIGRATION_DIR}" >&2
    exit 1
fi

for migration_file in "${migration_files[@]}"; do
    filename="$(basename "${migration_file}")"
    version=$((10#${filename%%_*}))

    if (( version <= applied_version )); then
        continue
    fi

    echo "Applying ${filename}"
    psql_exec < "${migration_file}"
done

echo "PostgreSQL schema migrations are up to date"
