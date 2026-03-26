# Migration Dependencies and Preconditions

pgroll supports two mechanisms for ensuring migrations apply in a safe
order: **explicit dependencies** and **schema preconditions**. Both
are optional and backward compatible -- existing migrations work
without changes.

## When to use these features

For most declarative migrations (create table, add column, etc.),
pgroll's built-in validation catches ordering problems automatically.
These features are most valuable when:

- You have **raw SQL migrations** that depend on specific schema
  state (function signatures, column types, data formats).
- Multiple branches modify **overlapping parts of the schema** and
  may deploy in different orders.
- A migration does **data manipulation** (UPDATE, DELETE) that
  depends on the output of functions or column types created by
  other migrations.

## Dependencies (`depends_on`)

The `depends_on` field declares that a migration must not be applied
until the listed migrations have been applied first.

```yaml
# 20260324_build_entity_name.yaml
depends_on:
  - "20260319_create_normalize_name"
operations:
  - sql:
      up: |
        CREATE FUNCTION build_individual_entity_name(...)
        AS $$ SELECT normalize_name(...) || ... $$;
```

### How it works

```
                    depends_on
20260319_create  ──────────────> 20260324_build
normalize_name                   entity_name

20260320_add     (no dependency, can apply in any order
indexes           relative to the others)
```

When `pgroll migrate` determines which migrations to apply, it:

1. Collects all unapplied migrations (via name-set matching).
2. Builds a directed acyclic graph (DAG) from `depends_on` edges.
3. Topologically sorts the migrations, using filesystem position as
   a tiebreaker for migrations with no dependency relationship.
4. Applies them in the sorted order.

Migrations without `depends_on` preserve their filesystem order
exactly, so existing workflows are unaffected.

### Validation

pgroll validates dependencies at two points:

**At sort time** (in `UnappliedMigrations`):
- All `depends_on` targets must exist -- either already applied in
  the database or present in the unapplied set. Unknown targets
  produce an error.
- Circular dependencies are detected and rejected.

**At apply time** (in `Start`):
- All `depends_on` targets must be in the applied set. This is a
  runtime safety net in case migrations are applied individually
  rather than via `pgroll migrate`.

### Error examples

```
dependency cycle detected: migrations involved: [A, B, C]

migration "X" depends on unknown migration "Y"

migration "X" depends on unapplied migrations: [Y, Z]
```

## Preconditions (`preconditions`)

The `preconditions` field declares schema state that must hold before
a migration can be applied. If any assertion fails, the migration is
rejected with a descriptive error.

```yaml
# 20260324_build_entity_name.yaml
depends_on:
  - "20260319_create_normalize_name"
preconditions:
  - table_exists: "lien_parties"
  - column_exists:
      table: "lien_parties"
      column: "entity_type"
      type: "lien_party_entity_types"
operations:
  - sql:
      up: |
        CREATE FUNCTION build_individual_entity_name(...)
        ...
```

### Available assertions

| Assertion | Fields | Checks |
|-----------|--------|--------|
| `table_exists` | table name (string) | Table is present in schema |
| `table_not_exists` | table name (string) | Table is absent |
| `column_exists` | `table`, `column`, optional `type` | Column exists, optionally with a specific type |
| `column_not_exists` | `table`, `column` | Column is absent from table |
| `index_exists` | `table`, `index` | Named index exists on table |
| `constraint_exists` | `table`, `constraint` | Named constraint exists (any type: check, unique, FK, exclude) |

### How it works

Preconditions are validated in `Migration.Validate()`, which runs
before any DDL operations. The check uses pgroll's in-memory schema
representation (the same one used for operation validation), so it
reflects the actual database state at the time of migration.

Preconditions are checked before operation validation, so a failed
precondition produces a clear, specific error rather than a
potentially confusing operation validation failure.

### Error examples

```
precondition failed: table "users" does not exist

precondition failed: column "email" on table "users" has type "text"
but expected "varchar(255)"

precondition failed: index "users_email_idx" does not exist on
table "users"
```

## Using both together

`depends_on` and `preconditions` serve complementary purposes:

- **`depends_on`** controls **ordering** -- it ensures migration B
  runs after migration A.
- **`preconditions`** verify **state** -- they ensure the schema
  looks the way the migration expects, regardless of how it got
  there.

Use `depends_on` when you know which specific migration must run
first. Use `preconditions` when you care about the resulting state
rather than the specific migration that created it.

For raw SQL migrations that depend on specific schema state, using
both provides defense in depth:

```yaml
depends_on:
  - "20260319_create_normalize_name"
preconditions:
  - column_exists:
      table: "lien_parties"
      column: "entity_type"
      type: "lien_party_entity_types"
operations:
  - sql:
      up: |
        -- This migration assumes normalize_name() exists and
        -- entity_type is an enum, not a varchar
        ...
```

## Backward compatibility

Both fields are optional. Migrations without `depends_on` or
`preconditions` behave identically to before:

- Filesystem order is preserved for migrations with no dependency
  relationships.
- No additional validation is performed.
- No database schema changes are required -- both fields are stored
  in the existing `migration` JSONB column.

## Best practices

1. **Always use `depends_on` for raw SQL migrations that reference
   objects created by other migrations.** Declarative operations
   (create_table, add_column) are validated automatically, but raw
   SQL bypasses this.

2. **Use `preconditions` when the migration's correctness depends on
   column types or constraint existence.** This catches cases where
   a migration that changes a column type deploys out of order.

3. **Prefer `depends_on` over timestamp ordering for critical
   dependencies.** Timestamps are a convention; `depends_on` is
   enforced.

4. **Keep dependency chains short.** Long chains reduce the
   flexibility that name-set matching provides. Only declare
   dependencies that are truly necessary.
