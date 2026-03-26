# Migration Safety Design

This document describes the architecture of pgroll's migration
ordering and safety system, including how name-set matching,
dependency ordering, and preconditions work together.

## Architecture overview

```
                    pgroll migrate <dir>
                           |
                           v
               ┌───────────────────────┐
               │  UnappliedMigrations  │
               │                       │
               │  1. Collect local     │
               │     migration files   │
               │  2. Name-set match    │
               │     against DB        │
               │  3. Topological sort  │◄── depends_on
               │     (Kahn's algo)     │
               └───────────┬───────────┘
                           │
                    for each migration
                           │
                           v
               ┌───────────────────────┐
               │     Migration.Start   │
               │                       │
               │  1. Validate()        │
               │     a. Preconditions  │◄── preconditions
               │     b. Op validation  │
               │  2. Dependencies      │◄── depends_on (runtime check)
               │  3. DDL operations    │
               │  4. Backfill          │
               └───────────────────────┘
```

## Component design

### Name-set matching (`pkg/roll/unapplied.go`)

Determines WHAT migrations need to run. Compares local migration
file names against the database's applied migration set.

```
Local files:    {A, B, C, D, H}
DB applied:     {A, B, H}
                ─────────────────
Unapplied:      {C, D}         (preserving filesystem order)
```

Key properties:
- Order-independent: works regardless of DB application order
- Validates completeness: every DB migration must have a local file
- Baseline-aware: only considers migrations after the latest baseline

### Topological sort (`pkg/roll/depends.go`)

Determines the ORDER for applying unapplied migrations. Uses Kahn's
algorithm with filesystem position as tiebreaker.

```
Input (filesystem order):   [C, D, E]
Dependencies:               E depends_on D, D depends_on C
                            ─────────────────
Output:                     [C, D, E]       (deps happen to match fs order)

Input (filesystem order):   [X, Y, Z]
Dependencies:               X depends_on Y
                            ─────────────────
Output:                     [Y, X, Z]       (Y promoted before X)
```

Algorithm:
1. Build in-degree counts from `depends_on` edges among unapplied
   migrations.
2. Dependencies on already-applied migrations are considered
   satisfied (in-degree not incremented).
3. Dependencies on unknown migrations produce an error.
4. Repeatedly select the migration with in-degree 0 and lowest
   filesystem position (tiebreaker).
5. If all remaining migrations have in-degree > 0, a cycle exists.

### Dependency validation (`pkg/roll/depends.go`)

Runtime safety net in `Start()`. Even if topological sort ordered
things correctly, this check ensures dependencies are satisfied
when migrations are applied individually (outside `pgroll migrate`).

### Precondition validation (`pkg/migrations/preconditions.go`)

Validates schema STATE before a migration runs. Checked in
`Migration.Validate()` before operation-level validation.

```
Precondition: column_exists(users, email, type=text)
                    |
                    v
         schema.GetTable("users")
                    |
                    v
         table.Columns["email"]
                    |
                    v
         column.Type == "text"  →  pass or fail
```

Each assertion maps to a lookup on pgroll's in-memory
`*schema.Schema`, which is read from the database at validation
time via `state.ReadSchema()`.

## Data flow

```
Migration file (JSON/YAML)
│
├── name            → identity (filename without extension)
├── operations      → what to do
├── depends_on      → ordering constraints (Phase 1)
└── preconditions   → state assertions (Phase 2)
         │
         ▼
Database: pgroll.migrations table
┌─────────┬──────────┬───────────────┬────────┬──────┐
│ schema  │ name     │ migration     │ parent │ done │
│         │          │ (JSONB)       │        │      │
├─────────┼──────────┼───────────────┼────────┼──────┤
│ public  │ 01_init  │ {operations,  │ NULL   │ true │
│         │          │  depends_on,  │        │      │
│         │          │  preconditions│        │      │
│         │          │  ...}         │        │      │
└─────────┴──────────┴───────────────┴────────┴──────┘
```

No database schema changes are needed. `depends_on` and
`preconditions` are stored inside the existing `migration` JSONB
column. The linear parent chain (`parent` column) continues to
record actual application order.

## Interaction between features

| Feature | Controls | When checked | Failure mode |
|---------|----------|-------------|--------------|
| Name-set matching | What to apply | `UnappliedMigrations()` | Missing local file error |
| `depends_on` | Application order | Sort time + `Start()` | Cycle/unknown/unsatisfied dep error |
| `preconditions` | Schema state contract | `Validate()` | Precondition failed error |
| Op validation | Operation feasibility | `Validate()` | Table/column does not exist error |

The layers are complementary:
- `depends_on` prevents a migration from running before its
  prerequisites.
- `preconditions` verify the prerequisites actually produced the
  expected state (catches behavioral changes, type mismatches).
- Operation validation catches structural issues (missing tables,
  duplicate columns).

## Error hierarchy

```
UnappliedMigrations()
├── ErrMismatchedMigration    "migration X exists in DB but not locally"
├── ErrMismatchedMigration    "migration X depends on unknown migration Y"
└── ErrDependencyCycle        "dependency cycle detected: [A, B, C]"

Start()
├── precondition failed       "table X does not exist"
├── precondition failed       "column X.Y has type Z but expected W"
├── operation validation      "table X already exists"
└── ErrDependencyNotApplied   "migration X depends on unapplied: [Y]"
```

All failures are hard errors. pgroll never silently skips or
reorders migrations.

## Backward compatibility

Both `depends_on` and `preconditions` are optional fields. When
absent:

- Topological sort is skipped entirely (fast path: no migrations
  have `depends_on`).
- Precondition validation is a no-op (empty slice).
- All existing behavior is preserved exactly.
