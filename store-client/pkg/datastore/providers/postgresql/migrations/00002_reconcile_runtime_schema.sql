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

-- NVSentinel PostgreSQL schema version 2.
--
-- Reconciles DDL that was historically applied by the Go datastore but was
-- absent from the canonical SQL schema.

BEGIN;

ALTER TABLE health_events
    ADD COLUMN IF NOT EXISTS quarantine_finish_timestamp TIMESTAMPTZ;
ALTER TABLE health_events
    ADD COLUMN IF NOT EXISTS drain_finish_timestamp TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_changelog_resume
    ON datastore_changelog(table_name, changed_at, id)
    WHERE processed = FALSE;

CREATE OR REPLACE FUNCTION log_table_changes()
RETURNS TRIGGER AS $$
DECLARE
    changelog_id BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        INSERT INTO datastore_changelog (table_name, record_id, operation, old_values)
        VALUES (TG_TABLE_NAME, OLD.id, TG_OP, to_jsonb(OLD))
        RETURNING id INTO changelog_id;

        PERFORM pg_notify(
            'nvsentinel_changes',
            json_build_object(
                'id', changelog_id,
                'table', TG_TABLE_NAME,
                'operation', TG_OP
            )::text
        );
        RETURN OLD;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO datastore_changelog (
            table_name,
            record_id,
            operation,
            old_values,
            new_values
        )
        VALUES (TG_TABLE_NAME, NEW.id, TG_OP, to_jsonb(OLD), to_jsonb(NEW))
        RETURNING id INTO changelog_id;

        PERFORM pg_notify(
            'nvsentinel_changes',
            json_build_object(
                'id', changelog_id,
                'table', TG_TABLE_NAME,
                'operation', TG_OP
            )::text
        );
        RETURN NEW;
    ELSIF TG_OP = 'INSERT' THEN
        INSERT INTO datastore_changelog (table_name, record_id, operation, new_values)
        VALUES (TG_TABLE_NAME, NEW.id, TG_OP, to_jsonb(NEW))
        RETURNING id INTO changelog_id;

        PERFORM pg_notify(
            'nvsentinel_changes',
            json_build_object(
                'id', changelog_id,
                'table', TG_TABLE_NAME,
                'operation', TG_OP
            )::text
        );
        RETURN NEW;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS maintenance_events_changes ON maintenance_events;
CREATE TRIGGER maintenance_events_changes
    AFTER INSERT OR UPDATE OR DELETE ON maintenance_events
    FOR EACH ROW EXECUTE FUNCTION log_table_changes();

DROP TRIGGER IF EXISTS health_events_changes ON health_events;
CREATE TRIGGER health_events_changes
    AFTER INSERT OR UPDATE OR DELETE ON health_events
    FOR EACH ROW EXECUTE FUNCTION log_table_changes();

INSERT INTO nvsentinel_schema_migrations (version, description)
VALUES (2, 'reconcile historical Go runtime DDL')
ON CONFLICT (version) DO NOTHING;

COMMIT;
