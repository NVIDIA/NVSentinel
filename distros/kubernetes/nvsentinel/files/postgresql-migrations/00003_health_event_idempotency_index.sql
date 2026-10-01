-- Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
--
-- Licensed under the Apache License, Version 2.0 (the "License");
-- you may not use this file except in compliance with the License.
-- You may obtain a copy of the License at
--
--     http://www.apache.org/licenses/LICENSE-2.0
--
-- Unless required by applicable law or agreed to in writing, software
-- distributed under the License is distributed on an "AS IS" BASIS,
-- WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
-- See the License for the specific language governing permissions and
-- limitations under the License.

-- NVSentinel PostgreSQL schema version 3.
--
-- nvsentinel:no-transaction
--
-- Adds the unique partial index behind the idempotency keys that the
-- deployment platform connector writes (ADR 052). CREATE INDEX CONCURRENTLY
-- cannot run inside a transaction block, so each statement in this file
-- commits on its own. Apply it with psql -v ON_ERROR_STOP=1 so that a failed
-- statement stops the file before the version is recorded.
--
-- CONCURRENTLY keeps health event inserts running during the build. If the
-- build fails or is cancelled, PostgreSQL leaves an INVALID index behind, and
-- IF NOT EXISTS would skip it on the next run. The check below therefore fails
-- on an INVALID index. To recover, run
--   DROP INDEX CONCURRENTLY IF EXISTS healthevent_idempotency_key_unique;
-- and apply this file again. During an upgrade, an older release can still be
-- building the index itself; the check then fails too. If
-- pg_stat_progress_create_index shows that build, wait for it to finish and
-- apply this file again instead of dropping the index.
--
-- Documents that already share an idempotency key make the build fail with a
-- unique violation. List them with
--   SELECT document #>> '{healthevent,metadata,idempotencyKey}' AS key, count(*)
--   FROM health_events GROUP BY 1 HAVING count(*) > 1;
-- remove the extra rows, then recover as above.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS healthevent_idempotency_key_unique
    ON health_events ((document #>> '{healthevent,metadata,idempotencyKey}'))
    WHERE (document #>> '{healthevent,metadata,idempotencyKey}') IS NOT NULL;

DO $$
DECLARE
    index_unique BOOLEAN;
    index_valid BOOLEAN;
    index_columns SMALLINT;
BEGIN
    SELECT i.indisunique, i.indisvalid, i.indnatts
    INTO index_unique, index_valid, index_columns
    FROM pg_index i
    JOIN pg_class idx ON idx.oid = i.indexrelid
    WHERE idx.relname = 'healthevent_idempotency_key_unique'
      AND i.indrelid = to_regclass('health_events');

    IF NOT FOUND THEN
        RAISE EXCEPTION 'index healthevent_idempotency_key_unique does not exist on health_events';
    END IF;

    IF NOT index_valid THEN
        RAISE EXCEPTION 'index healthevent_idempotency_key_unique is INVALID'
            USING HINT = 'If pg_stat_progress_create_index shows a build of it, wait and apply this migration again. '
                'Otherwise run DROP INDEX CONCURRENTLY IF EXISTS healthevent_idempotency_key_unique; '
                'then apply this migration again.';
    END IF;

    IF NOT index_unique OR index_columns <> 1 THEN
        RAISE EXCEPTION 'index healthevent_idempotency_key_unique exists with a different definition'
            USING HINT = 'Run DROP INDEX CONCURRENTLY IF EXISTS healthevent_idempotency_key_unique; then apply this migration again.';
    END IF;
END
$$;

INSERT INTO nvsentinel_schema_migrations (version, description)
VALUES (3, 'health event idempotency index')
ON CONFLICT (version) DO NOTHING;
