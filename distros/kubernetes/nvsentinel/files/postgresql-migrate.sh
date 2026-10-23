#!/bin/sh
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

# Applies the PostgreSQL schema migrations in filename order. Migrations already
# recorded in nvsentinel_schema_migrations are skipped. Run by the chart's
# PostgreSQL setup Job; POSIX sh, so it works in any image that ships psql.
#
# Connection settings come from the datastore ConfigMap (DATASTORE_*). The
# optional MIGRATION_USERNAME / MIGRATION_PASSWORD (setupJob.adminSecret)
# select a separate DDL role. A client certificate mounted at
# /client-certs-original is copied with the private permissions libpq requires.
set -eu

MIGRATION_DIR="${MIGRATION_DIR:-/migrations}"

export PGHOST="${DATASTORE_HOST:?DATASTORE_HOST is required}"
export PGPORT="${DATASTORE_PORT:-5432}"
export PGDATABASE="${DATASTORE_DATABASE:?DATASTORE_DATABASE is required}"
export PGUSER="${MIGRATION_USERNAME:-${DATASTORE_USERNAME:?DATASTORE_USERNAME is required}}"
export PGSSLMODE="${DATASTORE_SSLMODE:-require}"
export PGCONNECT_TIMEOUT=10

if [ -n "${MIGRATION_PASSWORD:-}" ]; then
    export PGPASSWORD="${MIGRATION_PASSWORD}"
elif [ -n "${DATASTORE_PASSWORD:-}" ]; then
    export PGPASSWORD="${DATASTORE_PASSWORD}"
fi

if [ -f /client-certs-original/tls.crt ] && [ -f /client-certs-original/tls.key ]; then
    cert_dir="$(mktemp -d)"
    cp /client-certs-original/tls.crt /client-certs-original/tls.key "${cert_dir}/"
    chmod 600 "${cert_dir}/tls.key"
    export PGSSLCERT="${cert_dir}/tls.crt"
    export PGSSLKEY="${cert_dir}/tls.key"
    if [ -f /client-certs-original/ca.crt ]; then
        export PGSSLROOTCERT=/client-certs-original/ca.crt
    fi
elif [ -n "${DATASTORE_SSLROOTCERT:-}" ] && [ -f "${DATASTORE_SSLROOTCERT}" ]; then
    export PGSSLROOTCERT="${DATASTORE_SSLROOTCERT}"
fi

echo "Applying PostgreSQL migrations to ${PGHOST}:${PGPORT}/${PGDATABASE} as ${PGUSER} (sslmode=${PGSSLMODE})"

until psql -X -q -tA -c "SELECT 1" >/dev/null; do
    echo "Waiting for PostgreSQL to accept connections..."
    sleep 5
done

# Two queries: PostgreSQL resolves table names at parse time, so a single CASE
# over to_regclass still fails on a database without the version table.
applied_version=0
version_table="$(psql -X -q -tA -v ON_ERROR_STOP=1 -c "SELECT to_regclass('nvsentinel_schema_migrations') IS NOT NULL")"
if [ "${version_table}" = "t" ]; then
    applied_version="$(psql -X -q -tA -v ON_ERROR_STOP=1 -c "SELECT COALESCE(MAX(version), 0) FROM nvsentinel_schema_migrations")"
fi
echo "Current PostgreSQL schema version: ${applied_version}"

found=0
for migration_file in "${MIGRATION_DIR}"/[0-9][0-9][0-9][0-9][0-9]_*.sql; do
    [ -f "${migration_file}" ] || continue
    found=1

    filename="$(basename "${migration_file}")"
    # The glob guarantees five digits; the leading 1 keeps sh arithmetic from
    # reading a zero-padded prefix as octal.
    version=$((1${filename%%_*} - 100000))

    if [ "${version}" -le "${applied_version}" ]; then
        continue
    fi

    echo "Applying ${filename}"
    psql -X -q -v ON_ERROR_STOP=1 -f "${migration_file}"
done

if [ "${found}" -eq 0 ]; then
    echo "ERROR: no PostgreSQL migrations found in ${MIGRATION_DIR}" >&2
    exit 1
fi

echo "PostgreSQL schema migrations are up to date"
