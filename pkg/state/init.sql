-- SPDX-License-Identifier: Apache-2.0
--
-- Belt-and-braces guard for the transaction below. pgroll no longer installs
-- the DDL-capture event triggers, and the block after CREATE SCHEMA removes
-- them from databases initialized by an older pgroll — but that removal needs
-- ownership of the triggers, and this script runs real DDL before and after
-- it. On a database where the drop could not run, this keeps Init itself from
-- being captured.
SET LOCAL pgroll.no_inferred_migrations TO 'TRUE';

CREATE SCHEMA IF NOT EXISTS placeholder;

-- pgroll does not record DDL run outside of its own migrations. Databases
-- initialized by an older pgroll carry the capture triggers and the function
-- they call; remove them here so an upgrade cleans up after itself.
--
-- Ordering is load-bearing: the function cannot be dropped while the triggers
-- still depend on it. Dropping an event trigger requires ownership, so a
-- non-owner role gets a warning rather than a failed init — capture keeps
-- running on such a database, but SchemaHistory filters the rows it produces,
-- so they stay inert.
DO $$
BEGIN
    DROP EVENT TRIGGER IF EXISTS pg_roll_handle_ddl;
    DROP EVENT TRIGGER IF EXISTS pg_roll_handle_drop;
    DROP FUNCTION IF EXISTS placeholder.raw_migration ();
EXCEPTION
    WHEN insufficient_privilege THEN
        RAISE WARNING 'pgroll: could not remove the legacy DDL-capture event triggers (pg_roll_handle_ddl, pg_roll_handle_drop); re-run `pgroll init` as their owner or a superuser';
END
$$;

CREATE TABLE IF NOT EXISTS placeholder.migrations (
    schema NAME NOT NULL,
    name text NOT NULL,
    migration jsonb NOT NULL,
    created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    parent text,
    done boolean NOT NULL DEFAULT FALSE,
    resulting_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (schema, name),
    FOREIGN KEY (schema, parent) REFERENCES placeholder.migrations (schema, name)
);

-- Only one migration can be active at a time.
--
-- Indexed on (schema) alone, deliberately. The original form indexed
-- (schema, name, done) WHERE done = FALSE, which enforced nothing: (schema,
-- name) is already the PRIMARY KEY, so at most one row exists per pair
-- regardless, the partial index was trivially satisfied, and two *different*
-- migrations could both sit done = FALSE. Single-active then rested entirely
-- on the read-then-insert check in Roll.Start -- a TOCTOU that two concurrent
-- pgroll processes can both pass.
--
-- Same shape as only_first_migration_without_parent below, for the same
-- reason: the constraint is "one row per schema matching this predicate".
--
-- Recreated rather than CREATE ... IF NOT EXISTS, because the broken index
-- carries the same name and would otherwise survive untouched on every
-- database that already ran an older pgroll. Dropping it loses nothing: it
-- enforced no constraint the PRIMARY KEY did not already imply.
--
-- If a database really does hold two active migrations, this raises instead of
-- letting a silent unique violation abort init with nothing actionable in it.
-- That state was always broken -- GetActiveMigration returns whichever row
-- Postgres yields first -- so surfacing it is the point.
DO $$
DECLARE
    dupe_schema name;
    dupe_names text;
BEGIN
    DROP INDEX IF EXISTS placeholder.only_one_active;
    SELECT
        SCHEMA,
        string_agg(name, ', ' ORDER BY created_at)
    INTO
        dupe_schema,
        dupe_names
    FROM
        placeholder.migrations
    WHERE
        done = FALSE
    GROUP BY
        SCHEMA
    HAVING
        count(*) > 1
    LIMIT 1;
    IF dupe_schema IS NOT NULL THEN
        RAISE EXCEPTION 'schema % has more than one active migration (%), which pgroll cannot represent', dupe_schema, dupe_names
            USING HINT = 'Resolve with `pgroll rollback` (or `pgroll revert`) until one active migration remains, then re-run `pgroll init`.';
        END IF;
        CREATE UNIQUE INDEX only_one_active ON placeholder.migrations (schema)
        WHERE
            done = FALSE;
END
$$;

-- Only first migration can exist without parent
CREATE UNIQUE INDEX IF NOT EXISTS only_first_migration_without_parent ON placeholder.migrations (schema)
WHERE
    parent IS NULL;

-- History is linear
CREATE UNIQUE INDEX IF NOT EXISTS history_is_linear ON placeholder.migrations (schema, parent);

-- Add a column to tell whether the row represents an auto-detected DDL capture or a pgroll migration
ALTER TABLE placeholder.migrations
    ADD COLUMN IF NOT EXISTS migration_type varchar(32) DEFAULT 'pgroll' CONSTRAINT migration_type_check CHECK (migration_type IN ('pgroll', 'inferred'));

-- Update the `migration_type` column to also allow a `baseline` migration type.
ALTER TABLE placeholder.migrations
    DROP CONSTRAINT migration_type_check;

ALTER TABLE placeholder.migrations
    ADD CONSTRAINT migration_type_check CHECK (migration_type IN ('pgroll', 'inferred', 'baseline'));

-- Change timestamp columns to use timestamptz
ALTER TABLE placeholder.migrations
    ALTER COLUMN created_at SET DATA TYPE timestamptz USING created_at AT TIME ZONE 'UTC',
    ALTER COLUMN updated_at SET DATA TYPE timestamptz USING updated_at AT TIME ZONE 'UTC';

-- Mark a migration as logically done while its Complete operations are
-- queued for replay during the next non-deferred Complete. Used by
-- `pgroll migrate` intermediates so destructive DDL runs after the
-- previous-production version schema has been dropped.
ALTER TABLE placeholder.migrations
    ADD COLUMN IF NOT EXISTS complete_deferred boolean NOT NULL DEFAULT FALSE;

-- Revert-window boundary. Rows with sealed=FALSE form the revertible window:
-- the most recent deployment, still physically in its expand phase (its
-- destructive DDL queued, not drained). Sealing happens when the deferred
-- queue drains — at the start of the next `pgroll migrate`, or at any
-- non-deferred Complete. Sealed rows must never be reverted: their
-- contraction has run and the prior state is not physically recoverable.
--
-- The backfill runs exactly once, when the column is first added: rows
-- completed by the pre-delayed-contraction flow were contracted at their own
-- Complete and are therefore sealed. In-flight (done=FALSE) and
-- deferred-pending rows stay unsealed.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT
            1
        FROM
            pg_attribute
        WHERE
            attrelid = 'placeholder.migrations'::regclass
            AND attname = 'sealed'
            AND NOT attisdropped) THEN
    ALTER TABLE placeholder.migrations
        ADD COLUMN sealed boolean NOT NULL DEFAULT FALSE;
    UPDATE
        placeholder.migrations
    SET
        sealed = TRUE
    WHERE
        done = TRUE
        AND complete_deferred = FALSE;
END IF;
END
$$;

-- Re-application tombstones. A sealed revert prunes the reverted forward
-- migrations from history, which makes their unchanged files look unapplied
-- again — the next deploy would silently re-run the DDL the revert just
-- undid. Each sealed revert records (name, content hash) here; `pgroll
-- migrate` refuses to re-apply a migration whose content still matches its
-- tombstone, and clears the tombstone when a changed version applies.
CREATE TABLE IF NOT EXISTS placeholder.reverted_migrations (
    schema NAME NOT NULL,
    name text NOT NULL,
    content_hash text NOT NULL,
    reverted_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (schema, name)
);

-- Table to track pgroll binary version
CREATE TABLE IF NOT EXISTS placeholder.pgroll_version (
    version text NOT NULL,
    initialized_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (version)
);

-- Helper functions
-- Are we in the middle of a migration?
CREATE OR REPLACE FUNCTION placeholder.is_active_migration_period (schemaname name)
    RETURNS boolean
    AS $$
    SELECT
        EXISTS (
            SELECT
                1
            FROM
                placeholder.migrations
            WHERE
                SCHEMA = schemaname
                AND done = FALSE)
$$
LANGUAGE SQL
STABLE;

-- Get the name of the latest migration, or NULL if there is none.
-- This will be the same as the version-schema name of the migration in most
-- cases, unless the migration sets its `versionSchema` field.
CREATE OR REPLACE FUNCTION placeholder.latest_migration (schemaname name)
    RETURNS text
    SECURITY DEFINER
    SET search_path = placeholder, pg_catalog, pg_temp
    AS $$
    SELECT
        p.name
    FROM
        placeholder.migrations p
    WHERE
        NOT EXISTS (
            SELECT
                1
            FROM
                placeholder.migrations c
            WHERE
                SCHEMA = schemaname
                AND c.parent = p.name)
        AND SCHEMA = schemaname
$$
LANGUAGE SQL
STABLE;

-- Get the name of the previous migration, or NULL if there is none.
CREATE OR REPLACE FUNCTION placeholder.previous_migration (schemaname name)
    RETURNS text
    AS $$
    SELECT
        parent
    FROM
        placeholder.migrations
    WHERE
        SCHEMA = schemaname
        AND name = placeholder.latest_migration (schemaname);
$$
LANGUAGE SQL;

-- find_version_schema finds a recent version schema for a given schema name.
-- How recent is determined by the minDepth parameter: for a minDepth of 0, it
-- returns the latest version schema, for a minDepth of 1, it returns the
-- previous version schema, and so on.
-- Only version schemas that exist in the database are considered; migrations
-- without version schema (such as inferred migrations) are ignored.
CREATE OR REPLACE FUNCTION placeholder.find_version_schema (p_schema_name name, p_depth integer DEFAULT 0)
    RETURNS text
    AS $$
    WITH RECURSIVE ancestors AS (
        SELECT
            name,
            COALESCE(migration ->> 'version_schema', name) AS version_schema,
            schema,
            parent,
            0 AS depth
        FROM
            placeholder.migrations
        WHERE
            name = placeholder.latest_migration (p_schema_name)
            AND SCHEMA = p_schema_name
        UNION ALL
        SELECT
            m.name,
            COALESCE(m.migration ->> 'version_schema', m.name) AS version_schema,
            m.schema,
            m.parent,
            a.depth + 1
        FROM
            placeholder.migrations m
            JOIN ancestors a ON m.name = a.parent
                AND m.schema = a.schema
)
        SELECT
            a.version_schema
        FROM
            ancestors a
    WHERE
        EXISTS (
            SELECT
                1
            FROM
                information_schema.schemata s
            WHERE
                s.schema_name = p_schema_name || '_' || a.version_schema)
    ORDER BY
        a.depth ASC OFFSET p_depth
    LIMIT 1;
$$
LANGUAGE SQL
STABLE;

-- previous_version returns the name of the previous version schema for a given
-- schema name or NULL if there is no previous version schema.
CREATE OR REPLACE FUNCTION placeholder.previous_version (schemaname name)
    RETURNS text
    AS $$
    SELECT
        placeholder.find_version_schema (schemaname, 1);
$$
LANGUAGE SQL
STABLE;

-- latest_version returns the name of the latest version schema for a given
-- schema name or NULL if there are no version schema.
CREATE OR REPLACE FUNCTION placeholder.latest_version (schemaname name)
    RETURNS text
    AS $$
    SELECT
        placeholder.find_version_schema (schemaname, 0);
$$
LANGUAGE SQL
STABLE;

-- Get the JSON representation of the current schema
CREATE OR REPLACE FUNCTION placeholder.read_schema (schemaname text)
    RETURNS jsonb
    LANGUAGE plpgsql
    AS $$
DECLARE
    tables jsonb;
BEGIN
    SELECT
        json_build_object('name', schemaname, 'tables', (
                SELECT
                    COALESCE(json_object_agg(t.relname, jsonb_strip_nulls (jsonb_build_object('name', t.relname, 'oid', t.oid, 'comment', descr.description, 'columns', (
                                        SELECT
                                            json_object_agg(name, c)
                                    FROM (
                                        SELECT
                                            attr.attname AS name, CASE WHEN attr.attidentity <> '' THEN
                                                attr.attidentity
                                            ELSE
                                                NULL
                                            END AS IDENTITY, CASE WHEN attr.attgenerated = '' THEN
                                                pg_get_expr(def.adbin, def.adrelid)
                                            ELSE
                                                NULL
                                            END AS default, NOT (attr.attnotnull
                                                OR tp.typtype = 'd'
                                                AND tp.typnotnull) AS nullable, CASE WHEN 'character varying'::regtype = ANY (ARRAY[attr.atttypid, tp.typelem]) THEN
                                            REPLACE(format_type(attr.atttypid, attr.atttypmod), 'character varying', 'varchar')
                                        WHEN 'timestamp with time zone'::regtype = ANY (ARRAY[attr.atttypid, tp.typelem]) THEN
                                            REPLACE(format_type(attr.atttypid, attr.atttypmod), 'timestamp with time zone', 'timestamptz')
                                        ELSE
                                            format_type(attr.atttypid, attr.atttypmod)
                                        END AS type, descr.description AS comment, (EXISTS (
                                                SELECT
                                                    1
                                                FROM pg_constraint
                                            WHERE
                                                conrelid = attr.attrelid
                                                AND ARRAY[attr.attnum::int] @> conkey::int[]
                                                AND contype = 'u')
                                        OR EXISTS (
                                            SELECT
                                                1
                                            FROM pg_index
                                            JOIN pg_class ON pg_class.oid = pg_index.indexrelid
                                        WHERE
                                            indrelid = attr.attrelid
                                            AND indisunique
                                            AND ARRAY[attr.attnum::int] @> pg_index.indkey::int[])) AS unique, (
                                    SELECT
                                        array_agg(e.enumlabel ORDER BY e.enumsortorder)
                                    FROM pg_enum AS e
                                WHERE
                                    e.enumtypid = tp.oid) AS enumValues, CASE WHEN tp.typtype = 'b' THEN
                                    'base'
                                WHEN tp.typtype = 'c' THEN
                                    'composite'
                                WHEN tp.typtype = 'd' THEN
                                    'domain'
                                WHEN tp.typtype = 'e' THEN
                                    'enum'
                                WHEN tp.typtype = 'p' THEN
                                    'pseudo'
                                WHEN tp.typtype = 'r' THEN
                                    'range'
                                WHEN tp.typtype = 'm' THEN
                                    'multirange'
                                END AS postgresType FROM pg_attribute AS attr
                                INNER JOIN pg_type AS tp ON attr.atttypid = tp.oid
                                LEFT JOIN pg_attrdef AS def ON attr.attrelid = def.adrelid
                                    AND attr.attnum = def.adnum
                            LEFT JOIN pg_description AS descr ON attr.attrelid = descr.objoid
                                AND attr.attnum = descr.objsubid
                        WHERE
                            attr.attnum > 0
                            AND NOT attr.attisdropped
                            AND attr.attrelid = t.oid ORDER BY attr.attnum) c), 'primaryKey', (
                        SELECT
                            json_agg(pg_attribute.attname) AS primary_key_columns
                        FROM pg_index, pg_attribute
                    WHERE
                        indrelid = t.oid
                        AND nspname = schemaname
                        AND pg_attribute.attrelid = t.oid
                        AND pg_attribute.attnum = ANY (pg_index.indkey)
                        AND indisprimary), 'indexes', (
                        SELECT
                            json_object_agg(ix_details.name, json_build_object('name', ix_details.name, 'unique', ix_details.indisunique, 'exclusion', ix_details.indisexclusion, 'columns', ix_details.columns, 'predicate', ix_details.predicate, 'method', ix_details.method, 'definition', ix_details.definition))
                    FROM (
                        SELECT
                            replace(reverse(split_part(reverse(pi.indexrelid::regclass::text), '.', 1)), '"', '') AS name, pi.indisunique, pi.indisexclusion, array_agg(a.attname) AS columns, pg_get_expr(pi.indpred, t.oid) AS predicate, am.amname AS method, pg_get_indexdef(pi.indexrelid) AS definition
                        FROM pg_index pi
                        JOIN pg_attribute a ON a.attrelid = pi.indrelid
                            AND a.attnum = ANY (pi.indkey)
                        JOIN pg_class cls ON cls.oid = pi.indexrelid
                        JOIN pg_am am ON am.oid = cls.relam
                        WHERE
                            indrelid = t.oid::regclass GROUP BY pi.indexrelid, pi.indisunique, pi.indpred, am.amname) AS ix_details), 'checkConstraints', (
                SELECT
                    json_object_agg(cc_details.conname, json_build_object('name', cc_details.conname, 'columns', cc_details.columns, 'definition', cc_details.definition, 'noInherit', cc_details.connoinherit))
                FROM (
                    SELECT
                        cc_constraint.conname, array_agg(cc_attr.attname ORDER BY cc_constraint.conkey::int[]) AS columns, pg_get_constraintdef(cc_constraint.oid) AS definition, cc_constraint.connoinherit FROM pg_constraint AS cc_constraint
                    INNER JOIN pg_attribute cc_attr ON cc_attr.attrelid = cc_constraint.conrelid
                        AND cc_attr.attnum = ANY (cc_constraint.conkey)
                    WHERE
                        cc_constraint.conrelid = t.oid
                        AND cc_constraint.contype = 'c' GROUP BY cc_constraint.oid, cc_constraint.conname) AS cc_details), 'uniqueConstraints', (
                    SELECT
                        json_object_agg(uc_details.conname, json_build_object('name', uc_details.conname, 'columns', uc_details.columns))
                    FROM (
                        SELECT
                            uc_constraint.conname, array_agg(uc_attr.attname ORDER BY uc_constraint.conkey::int[]) AS columns, pg_get_constraintdef(uc_constraint.oid) AS definition FROM pg_constraint AS uc_constraint
                        INNER JOIN pg_attribute uc_attr ON uc_attr.attrelid = uc_constraint.conrelid
                            AND uc_attr.attnum = ANY (uc_constraint.conkey)
                        WHERE
                            uc_constraint.conrelid = t.oid
                            AND uc_constraint.contype = 'u' GROUP BY uc_constraint.oid, uc_constraint.conname) AS uc_details), 'excludeConstraints', (
                        SELECT
                            json_object_agg(xc_details.conname, json_build_object('name', xc_details.conname, 'columns', xc_details.columns, 'definition', xc_details.definition, 'predicate', xc_details.predicate, 'method', xc_details.method))
                        FROM (
                            SELECT
                                xc_constraint.conname, array_agg(xc_attr.attname ORDER BY xc_constraint.conkey::int[]) AS columns, pg_get_expr(pi.indpred, t.oid) AS predicate, am.amname AS method, pg_get_constraintdef(xc_constraint.oid) AS definition FROM pg_constraint AS xc_constraint
                            INNER JOIN pg_attribute xc_attr ON xc_attr.attrelid = xc_constraint.conrelid
                                AND xc_attr.attnum = ANY (xc_constraint.conkey)
                            JOIN pg_index pi ON pi.indexrelid = xc_constraint.conindid
                            JOIN pg_class cls ON cls.oid = pi.indexrelid
                            JOIN pg_am am ON am.oid = cls.relam
                            WHERE
                                xc_constraint.conrelid = t.oid
                                AND xc_constraint.contype = 'x' GROUP BY xc_constraint.oid, xc_constraint.conname, pi.indpred, pi.indexrelid, am.amname) AS xc_details), 'foreignKeys', (
                            SELECT
                                json_object_agg(fk_details.conname, json_build_object('name', fk_details.conname, 'columns', fk_details.columns, 'referencedTable', fk_details.referencedTable, 'referencedColumns', fk_details.referencedColumns, 'matchType', fk_details.matchType, 'onDelete', fk_details.onDelete, 'onUpdate', fk_details.onUpdate))
                            FROM (
                                SELECT
                                    fk_info.conname AS conname, fk_info.columns AS columns, fk_info.relname AS referencedTable, array_agg(ref_attr.attname ORDER BY ref_attr.attname) AS referencedColumns, CASE WHEN fk_info.confmatchtype = 'f' THEN
                                    'FULL'
                                WHEN fk_info.confmatchtype = 'p' THEN
                                    'PARTIAL'
                                WHEN fk_info.confmatchtype = 's' THEN
                                    'SIMPLE'
                                END AS matchType, CASE WHEN fk_info.confdeltype = 'a' THEN
                                    'NO ACTION'
                                WHEN fk_info.confdeltype = 'r' THEN
                                    'RESTRICT'
                                WHEN fk_info.confdeltype = 'c' THEN
                                    'CASCADE'
                                WHEN fk_info.confdeltype = 'd' THEN
                                    'SET DEFAULT'
                                WHEN fk_info.confdeltype = 'n' THEN
                                    'SET NULL'
                                END AS onDelete, CASE WHEN fk_info.confupdtype = 'a' THEN
                                    'NO ACTION'
                                WHEN fk_info.confupdtype = 'r' THEN
                                    'RESTRICT'
                                WHEN fk_info.confupdtype = 'c' THEN
                                    'CASCADE'
                                WHEN fk_info.confupdtype = 'd' THEN
                                    'SET DEFAULT'
                                WHEN fk_info.confupdtype = 'n' THEN
                                    'SET NULL'
                                END AS onUpdate FROM (
                                    SELECT
                                        fk_constraint.conname, fk_constraint.conrelid, fk_constraint.confrelid, fk_constraint.confkey, fk_cl.relname, fk_constraint.confmatchtype, fk_constraint.confdeltype, fk_constraint.confupdtype, array_agg(fk_attr.attname ORDER BY fk_attr.attname) AS columns FROM pg_constraint AS fk_constraint
                                    INNER JOIN pg_class fk_cl ON fk_constraint.confrelid = fk_cl.oid -- join the referenced table
                                    INNER JOIN pg_attribute fk_attr ON fk_attr.attrelid = fk_constraint.conrelid
                                        AND fk_attr.attnum = ANY (fk_constraint.conkey) -- join the columns of the referencing table
                                    WHERE
                                        fk_constraint.conrelid = t.oid
                                        AND fk_constraint.contype = 'f' GROUP BY fk_constraint.conrelid, fk_constraint.conname, fk_constraint.confrelid, fk_cl.relname, fk_constraint.confkey, fk_constraint.confmatchtype, fk_constraint.confdeltype, fk_constraint.confupdtype) AS fk_info
                                    INNER JOIN pg_attribute ref_attr ON ref_attr.attrelid = fk_info.confrelid
                                        AND ref_attr.attnum = ANY (fk_info.confkey) -- join the columns of the referenced table
                                GROUP BY fk_info.conname, fk_info.conrelid, fk_info.columns, fk_info.confrelid, fk_info.confmatchtype, fk_info.confdeltype, fk_info.confupdtype, fk_info.relname) AS fk_details)))), '{}'::json)
            FROM pg_class AS t
            INNER JOIN pg_namespace AS ns ON t.relnamespace = ns.oid
            LEFT JOIN pg_description AS descr ON t.oid = descr.objoid
                AND descr.objsubid = 0
            WHERE
                ns.nspname = schemaname
                AND t.relkind IN ('r', 'p') -- tables only (ignores views, materialized views & foreign tables)
))
    INTO
        tables;
    RETURN tables;
END;
$$;

