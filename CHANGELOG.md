# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **`pgroll migrate`'s interrupted-run message named the wrong verb for a
  batch.** It told the operator to run `pgroll rollback`, which undoes only the
  *one* active migration — so a run that died partway through a train left the
  earlier migrations applied, unsealed and carrying queued contraction, with
  nothing said about them. The message now names both verbs, states what each
  covers, and counts the already-applied-but-uncontracted migrations so the
  choice is concrete.
- **`only_one_active` enforced nothing.** It was a unique index on
  `(schema, name, done) WHERE done = FALSE`, but `(schema, name)` is already
  the primary key — at most one row exists per pair regardless, so the partial
  index was trivially satisfied and two *different* migrations could both sit
  `done = FALSE`. It is now `UNIQUE (schema) WHERE done = FALSE`, matching the
  shape `only_first_migration_without_parent` uses. Existing state schemas are
  upgraded in place; a database that already holds two active migrations gets a
  clear error naming them rather than an opaque unique violation.
- **The `RECOVERY` pre-flight state was unreachable.** `stateInSync` compared
  `state.LatestVersion` against the live schemas, but `LatestVersion` resolves
  through `find_version_schema`, whose own `WHERE` clause requires the schema
  to exist — so it was checked against a superset of itself and was always
  true. `classifyCycle` never reached its default branch, and the unit test
  passed `stateInSync: false` straight into the pure function, keeping the
  branch green while it was dead. It now compares the history *leaf's* version
  schema, which is the condition the warning always described: history has
  advanced to a migration whose projection is not deployed, repairable with
  `pgroll materialize`. Trivially in sync when version schemas are disabled.

## [0.16.2-baselayer.20] - 2026-08-09

### Added

- **Migration targets.** A migration may declare `targets: [name, ...]`, and
  `pgroll` accepts a persistent `--target <name>` (`PGROLL_TARGET`) that
  restricts which migrations are applied. Target names are free-form strings
  compared verbatim; pgroll assigns them no meaning, so adding a new target
  costs no pgroll change and the vocabulary stays with the caller's linter.

  Without `--target` nothing is filtered — every migration applies. That is
  single-database mode, so dev, CI and per-developer instances need no change:
  the number of `--target` flags in play equals the number of databases you
  have.

  **Filtering applies to selection only; history validation always reads the
  unfiltered directory.** This is the load-bearing part. A database may hold
  history for migrations the active target does not select — an ETL host
  provisioned as a volume clone of the application database inherits the
  application's entire chain — and it keeps that history, validates cleanly,
  and simply stops receiving migrations it is not a target of. No re-baseline,
  no staggered cutover. See `docs/guides/migration-targets.md`.

  With `--target` in effect every *selection candidate* (post-baseline and not
  yet applied) must declare `targets`; an untagged candidate is a hard error
  naming the file. There is no default target and no fail-open, because a
  migration silently withheld from a database it should have reached surfaces
  later as replication quietly dropping rows. Already-applied migrations are
  never inspected, so adopting targets requires no back-stamping of history.

  `targets` is excluded from `Migration.ContentHash`: routing records which
  databases a migration reaches, not what it does, so re-routing must not clear
  a re-application tombstone.

  `--target` is honoured or refused everywhere, never silently ignored. `start`
  refuses a migration whose `targets` exclude the active one — that path takes a
  single file and never consults directory resolution, so without the guard
  `pgroll start ./migrations/04_etl.yaml --target app` applied an ETL migration
  to the application database and exited 0. `stamp` filters the directory form
  (a history row makes its migration a non-candidate permanently, so an
  unfiltered stamp is unrecoverable) and `baseline` carries the target into the
  placeholder it writes. Commands with no migration set to filter — `pull`,
  `validate`, `rollback`, `prune`, `update`, `convert` — reject the flag.

  `pgroll check` gains `--require-targets`, so an undeclared migration fails at
  review time rather than at the first targeted deploy, where it would take out
  every target's leg at once. A `depends_on` that does not cover the dependent's
  targets is now an **error**: it is the one shape where treating an excluded
  dependency as satisfied can genuinely break a database, and as a warning it
  merged green. Malformed `targets` lists are errors; target names differing
  only by case remain advisory. Base-branch ordering only compares migrations
  that share a target, since two targets releasing on independent cadences is
  the steady state this feature creates and cross-target comparison demanded a
  rebase that changes nothing.

  `migrate --to` closes its bound under `depends_on`. Locating the target in the
  full local sequence makes the bound a filename-order prefix, which — unlike
  the topologically sorted slice it replaced — is not dependency-closed.

  **This is a one-way binary lockstep.** Migration files are decoded strictly,
  so any pgroll older than this release hard-rejects a file carrying `targets`,
  and the rejection takes out the whole directory for every command. Upgrade
  every consumer before committing the first tagged migration. Parse errors for
  unknown fields now say so explicitly rather than naming a Go type.

### Fixed

- `pgroll migrate --to` no longer silently no-ops an entire run. The target was
  located in the list the run would apply, while `MigrationExists` consults the
  whole history table, so an already-applied target read as "nothing to do"
  even when migrations before it were still pending. The target is now located
  in the full local sequence and bounds the run positionally.
- `pgroll latest schema --local` and `latest migration --local` honour
  `--target`, and raise the same missing-`targets` error the migrate path does
  rather than silently skipping an undeclared file.

## [0.16.2-baselayer.19] - 2026-07-24

### Fixed

- `pgroll migrate` is now idempotent across the expand→complete window. The
  additive train model leaves the final migration active (expand applied,
  awaiting `pgroll complete`), which is also the resting state a database
  inherits when provisioned from a snapshot captured mid-train. Re-running
  `migrate` in that state previously hard-failed as `INTERRUPTED` and told the
  operator to run `pgroll rollback` — wedging every fresh-instance deploy and
  any deploy retried after a mid-deploy failure. `migrate` now recognizes the
  fully-materialized expand phase as a new benign `EXPANDED` state and treats a
  re-run as a no-op (or, with `--complete`, finishes the contraction).
  Genuine interruptions are still caught: the expand counts as materialized
  only when the active migration's version schema is projected *and* its
  backfill has no rows still flagged (`_pgroll_needs_backfill`), so a Start
  killed mid-backfill remains `INTERRUPTED`. Adds `Roll.HasPendingBackfill`.

## [0.16.2-baselayer.18] - 2026-07-20

## [0.16.2-baselayer.17] - 2026-07-13

### Added

- `docs/guides/lifecycle-verbs.md` — a design note explaining why the
  `migrate` / `complete` / `rollback` / `revert` / `plan` verbs are each
  distinct and do not fold together (the "fold test"), following the seal
  retirement.

### Changed

- Bumped Go module dependencies (dependabot):
  - `github.com/testcontainers/testcontainers-go` 0.42.0 → 0.43.0
  - `github.com/testcontainers/testcontainers-go/modules/postgres` 0.42.0 → 0.43.0
  - `github.com/oapi-codegen/nullable` 1.1.0 → 1.2.0
  - `golang.org/x/tools` 0.45.0 → 0.48.0
  - `golang.org/x/mod` 0.37.0 → 0.38.0
  - `golang.org/x/crypto` 0.48.0 → 0.52.0 (indirect)

## [0.16.2-baselayer.16] - 2026-07-12

### Fixed

- `pgroll plan` / `revert --dry-run` follow-ups from review of the
  baselayer.15 planning commands:
  - `in_sync` now requires the apply, revert, and blocked legs to all be
    empty, not just leaf equality — a checkout migration older than the
    shared leaf that was never applied (a late-merged/backdated migration)
    no longer reports `in_sync: true` while `apply` lists it.
  - `plan --to <baseline>` is accepted instead of rejected as "not found in
    database history"; the baseline is a legal revert boundary, matching
    `revert --to <baseline>`.
  - `blocked.migrations` is now populated (the database migrations the
    revert could not walk back) whenever `blocked.count` is non-zero,
    instead of an empty list.
  - `blocked.reason` is always one of the documented tokens
    (`non-contiguous`, `target not found`, `inverse unavailable`,
    `window open`, `no convergence target`, or the catch-all `unavailable`)
    — a raw planner error message can no longer leak into the field.
  - `revert --dry-run` now surfaces a pending interrupted revert (it errors
    telling the operator to resume) and rejects `--to <in-flight> --expand-only`,
    matching the real command instead of previewing a plan it would refuse.
  - The apply leg is ordered by `depends_on` (topologically), matching what
    `migrate` actually runs, rather than raw filename order.
  - A restore-target lookup failure is now fatal rather than silently
    reported as an "empty database" restore; two redundant state reads per
    plan/preview were removed.

## [0.16.2-baselayer.15] - 2026-07-12

### Added

- **`pgroll plan <directory> [--json]`** — a read-only command that computes
  what it would take to converge the target database's migration history to a
  local migrations directory, without executing anything. It surfaces the
  forward migrations to apply, the migrations to revert (with the restore
  target and the version schemas a revert would drop), whether the histories
  are in sync or have diverged, and any database migrations that are absent
  from the checkout and cannot be cleanly reverted. `--json` emits the machine
  form; without it, a human-readable summary. Exit status is zero whenever a
  plan can be produced — including "nothing to do" and "blocked" — so callers
  branch on the JSON fields, not the exit code; a non-zero exit means no plan
  could be produced at all (database unreachable, pgroll uninitialized, or a
  `--to` target absent from history). `--to <name>` overrides the convergence
  target with an explicit revert boundary that must already exist in history.
  This lets deploy tooling decide apply-vs-revert (and pin-guard a revert)
  through the CLI instead of reading pgroll's internal tables.
- **`pgroll revert --dry-run [--json]`** — previews a revert (its targets, the
  restore schema, the version schemas it would drop, and whether it reaches
  contracted history) for the same bare / `--steps` / `--to` bounds the real
  command honors, and returns without changing anything. `--json` emits the
  machine form.

## [0.16.2-baselayer.14] - 2026-07-12

### Changed — BREAKING lifecycle semantics

- **Contraction moved from "next deployment" to "end of this deployment."**
  The cross-deploy seal window is retired. `pgroll migrate` is now purely
  additive: it never seals, drains, or drops anything — the previous
  deployment can no longer be contracted as a side effect of applying new
  work, so a stale schema pin can no longer wedge future deployments.
  Deploy flow: `migrate` (expand, final migration left active) → repin
  apps → `pgroll complete` (drain the batch's deferred DDL, drop old
  version schemas, seal). `migrate --complete` performs the full converge
  in one shot for environments with nothing pinned to the previous schema
  (dev/CI/disposable instances). At most two version schemas exist during
  a deploy; exactly one after.
- **`pgroll complete` is the single contraction step.** With no active
  migration it finishes whatever the deployment left pending
  (`FinishContraction`): drains a leftover deferred queue (including one
  left by a database upgraded from the delayed-contraction lifecycle),
  seals the window, and converges version schemas. It refuses an
  unfinished `migrate` batch (resume with `migrate` or abort with
  `revert`) instead of contracting mid-batch.
- **`pgroll revert` is the unified reverse gear.** While a deployment is
  in flight (not yet contracted) it walks back losslessly, exactly as
  before. Once contracted, `revert --to <name>` switches to inversion:
  synthesized inverse migrations run forward through the normal
  zero-downtime engine, then forward and inverse rows are pruned from
  history (schema-exact, data re-derived through the original up/down
  expressions — best effort). When the in-flight window sits above a
  contracted target, both legs compose under one confirmation.
  `--past-seal` is deprecated (hidden alias for one release).
- **New `revert --expand-only`** stops an inversion revert after its
  expand phase: the restored schema exists alongside the current one for
  apps to repin to, and the next `pgroll complete` contracts the inverses
  and finishes the history prune (`FinishPendingSealedRevert`).
- Schema drops are now strict everywhere: a version-schema drop blocked by
  another backend fails loudly, naming the blocking sessions, instead of
  deferring the drop to a later deployment. Orphaned version schemas can
  no longer arise; the orphan bookkeeping (`OrphanedVersionSchemas`,
  `ReapVersionSchemasExcept`) is removed.

### Added

- Constraint-family inversion coverage: `create_constraint` (unique,
  check, foreign key) inverts to a constraint drop; `drop_constraint` /
  `drop_multicolumn_constraint` invert to the constraint's re-creation
  from the pre-state snapshot; `alter_column` adding a check/unique/
  foreign-key constraint inverts to its drop. Primary-key and exclusion
  constraints remain refused.

## [0.16.2-baselayer.13] - 2026-06-19

## [0.16.2-baselayer.12] - 2026-06-19

## [0.16.2-baselayer.11] - 2026-06-17

## [0.16.2-baselayer.10] - 2026-06-15

### Fixed

- Recreated columns now keep a deterministic physical order (ENG-6193).
  Adding a constraint over existing columns — e.g. a `unique`
  `create_constraint` — makes pgroll duplicate those columns. The order the
  duplicate columns were `ADD`ed fixes their final `attnum` (completion only
  drops the originals and renames the duplicates, which never changes
  `attnum`), but that add order came from ranging a Go map, so it was
  randomized per process: the same migration could seal as `(name,
  person_id)` on one deploy and `(person_id, name)` on another. The
  duplicator now emits the duplicate `ADD COLUMN`s in the operation's
  declared column order, so every application — per-migration replay and
  deferred train+seal alike — converges on the same column order.

## [0.16.2-baselayer.9] - 2026-06-12

### Fixed

- Concurrent index builds now honor the lock-retry budget (ENG-6174). A
  failed build leaves an INVALID index that `CREATE ... IF NOT EXISTS`
  silently no-ops against, which defeated the retry layer and capped
  effective retries at ~6 seconds regardless of the configured budget.
  Invalid leftovers are healed before and between attempts, and the
  attempt loop is deadline-driven.
- During a concurrent index build the session `lock_timeout` is raised to
  the retry budget (and restored afterwards): CIC's locks block no
  application traffic, so the aggressive timeout that protects
  strong-lock DDL only caused create/drop churn here.

### Added

- When a concurrent index build fails, the error now lists the oldest
  snapshot-holding sessions from `pg_stat_activity` (pid, source, state,
  transaction age, query) — the transactions the build must out-wait,
  regardless of which tables they touch.

## [0.16.2-baselayer.7] - 2026-06-11

## [0.16.2-baselayer.6] - 2026-06-11

## [0.16.2-baselayer.5] - 2026-06-10

### Added
- **Inversion engine (Phase 1)**: `pgroll revert --to <name> --past-seal` reverts SEALED history — deployments whose contraction has already drained — by synthesizing inverse migrations and running them forward through the normal expand/contract engine (the revert is itself zero-downtime), then pruning both the forward migrations and their inverses from history so the boundary becomes the leaf, exactly as if the segment had never been applied. Schema shape is restored exactly; data is re-derived through the original up/down expressions (best-effort by construction — destroyed independent data is not recoverable, and the CLI says so before confirming). Per-operation inverses live behind a new `Invertible` interface: renames swap; `create_table`/`create_index` drop; `add_column` drops with the original `up` as the re-derivation expression; raw SQL runs its `down` as a drain-deferred counter-statement (so destructive inverses execute after the sealed train's version schemas are reaped, like forward destructive DDL); `drop_index`/`drop_column` reconstruct from the boundary snapshot. Multi-operation and multi-migration segments invert against virtually-replayed per-operation pre-states from the clean boundary upward, so intermediate rows' polluted snapshots are never consulted. Crash recovery: a partially-applied inverse train is unsealed by construction and is walked back out with the standard window revert before retrying; a fully-applied-but-unpruned train (leaf is a sealed `revert_*` row, tagged via the new `revert_of` field) is finished by completing the prune. `alter_column` inverts its type/nullability/default/comment sub-operations by restoring the prior values from the boundary snapshot with the data expressions swapped; `drop_table` inverts to a full typed re-creation (columns, primary key, checks, uniques, foreign keys) plus raw-SQL re-creation of non-constraint indexes from their stored definitions — the table returns EMPTY, which is the honest meaning of best-effort for a drained table drop. Operations without inverses (the constraint family: `create_constraint`, `drop_constraint`, `drop_multicolumn_constraint`, `set_*`, and `alter_column`'s constraint-adding sub-operations) refuse with the operation named.
- `pgroll revert --steps N` / `--to <name>` — bounded reverts. `--steps` walks back at most N migrations (newest first); `--to` reverts everything newer than the named migration, which becomes the history leaf (naming the newest sealed migration reverts the whole window; sealed targets deeper than that are refused with the deepest reachable boundary named). After a bounded revert the new leaf may be a train intermediate that never projected a version schema — one is materialized for it from the deferred-replayed schema, so still-queued expand-state artifacts project under their virtual names exactly as the leaf's own Start would have projected them. `roll.RevertPlan` exposes the bounded plan for preview; bounds are mutually exclusive and validated before any work happens.
- `pgroll revert` — rolls back every migration applied since the last seal point (the previous `pgroll migrate` batch), restoring the database's schema, data, and migration history to the pre-deployment state. The revert is *lossless*: under delayed contraction (below), the deployment's destructive DDL is still queued rather than executed, so reverted migrations are physically in their expand phase — old columns alive, dual-write triggers running. Per-row recipes: in-progress and deferred rows use the standard rollback recipe (parent snapshot + virtual replay); inline-completed additive rows roll back against the fresh physical schema so operation Rollbacks resolve post-complete names (e.g. `add_column` drops the final column rather than its long-gone temp name). The walk always operates on the current history leaf, so an interrupted revert is simply re-run. `roll.RevertTargets` previews the window; the CLI prints the plan, the version schema apps must be repinned to first, and requires confirmation (`--yes` to skip).
- Reversibility by construction: migrations must be revertible or explicitly marked `irreversible: true` (new migration-level field). Non-`onComplete` raw SQL and `drop_column` operations must declare a `down` expression. Enforced by `pgroll check` (hard error) and at `start`/`migrate`/`validate` time via the new `WithRequireReversible()` roll option, which the CLI always sets; the Go library API keeps upstream behavior. `pgroll check` also now flags unparseable operations as errors.
- `sealed` column on `pgroll.migrations` — the revert-window boundary. Rows are stamped sealed when their contraction drains: by the seal step at the start of the next `pgroll migrate`, by `pgroll complete`, or (for baselines, stamps, and inferred DDL captures) at insert, since they have no expand state to revert to. A guarded one-time migration backfills `sealed=true` for previously completed rows so pre-existing history is never considered revertible.

### Changed
- **Delayed contraction**: `pgroll migrate --complete` now defers the final migration's contraction too (previously: intermediates deferred, final drained everything). The whole train ships in its expand phase and stays losslessly revertible for a full release cycle; the previous deployment's version schema is never dropped at completion, so a revert always has a live schema for apps to repin to. The queued contraction drains at the start of the *next* `pgroll migrate` (new `Roll.SealDeferredCompletes` — the point of no return, surfaced in the pre-flight as `Seals: N deferred completion(s)`) or on demand via `pgroll complete` with no migration in progress. The seal refreshes the train-final's `resulting_schema` after draining (the deferred-complete snapshot is captured mid-flight with temp artifacts present) and recreates the live version schema from the post-drain state — the same brief self-healing projection window the pre-delayed-contraction `--complete` drain had, shifted one deployment later. Crash-safe by ordering: the last drained row's deferred flag clears only after the live projection and boundary snapshot are restored, and a crashed seal leaves a sealed-but-queued row that `pgroll revert` refuses while a re-run of the seal finishes the drain.
- `create_constraint` stays inline-classified in batched migrates (chained duplicators on the same columns — e.g. add unique, add check, add FK, drop check — rely on each layer's Complete having physically run before the next layer's Start duplicates the column), and gains a `RollbackCompleted` implementation so `pgroll revert` can walk back an inline-completed constraint creation by dropping the constraint. New optional `CompletedRollbackable` interface for operations whose Complete restructures user-facing objects.
- `examples/40_create_enum_type.yaml` gained a `down` expression (`DROP TYPE fruit_size`) to comply with reversibility-by-construction.

## [0.16.2-baselayer.4] - 2026-06-09

### Added
- New `pgroll prune` command: removes named migrations from pgroll's history (`pgroll.migrations`) and drops their version schemas, rewiring the parent chain across the gaps so history stays linear. No user-table DDL is executed — the physical effects of completed migrations are *not* reverted. Its purpose is history reconciliation when applied migrations no longer exist on disk: the canonical case is a branch tested against a shared database and then abandoned, whose completed rows otherwise block `pgroll migrate` with "remote migration does not match local migration" (completed rows previously had no removal path short of hand-editing `pgroll.migrations`). Refuses while a migration is in progress (complete it or `pgroll rollback` first) and refuses to prune baseline rows. Version schemas are dropped before the rows so an interrupted prune is safely re-runnable. Names are passed via repeatable `--name` flags; `--yes` skips the confirmation. New `state.Prune` performs the chain surgery in one transaction using a temp-table copy/rewire/swap that satisfies the table's parent FK and linear-history unique indexes; `roll.Prune`/`roll.PruneTargets` add validation, listing, and version-schema cleanup.

## [0.16.2-baselayer.3] - 2026-05-28

### Fixed
- An interrupted `complete` can now be safely re-run instead of permanently wedging the migration. `complete` applies its DDL non-transactionally and flips a migration's `done` flag only as the final step, so an interruption (SIGINT, pod eviction, `lock_timeout`, dropped connection) in the window between the DDL committing and the flag flipping left a migration whose physical schema was fully migrated but was still recorded as in-progress. Re-running `complete` replayed the same actions from the top, and the non-idempotent ones errored against the already-migrated schema — wedging the migration with no forward path (`pgroll rollback` would also fail, since it tried to drop the now-renamed temp column). The trigger in practice was an `add_column`: its first complete action renames `_pgroll_new_<col>_<scope>` to the final name, and on re-run that `ALTER TABLE ... RENAME COLUMN` failed with `column "_pgroll_new_<col>_<scope>" does not exist` because `ALTER TABLE IF EXISTS` guards only the *table*, not the column. The `Complete`-path actions that lack a native idempotency guard — `renameColumnAction`, `renameConstraintAction`, `addConstraintUsingUniqueIndexAction`, and `addPrimaryKeyAction` — now probe the catalog (see `pkg/migrations/catalog.go`) and no-op when their work has already been applied: a rename whose source is gone and target is present, an `ADD CONSTRAINT`/`ADD PRIMARY KEY` whose object already exists. The guards add *only* the already-applied no-op; every other case (including a genuinely missing source, which still errors) falls through to the original SQL unchanged, and `renameTableAction` was already idempotent (its `IF EXISTS` guards the source table) so it is untouched. Added action-level regression tests for each guarded action plus an end-to-end `TestCompleteIsReRunnableAfterInterruptedRename` that reproduces the incident.

## [0.16.2-baselayer.1] - 2026-05-13

### Changed
- Merged upstream `xataio/pgroll` v0.16.2 (single commit: Go toolchain bump to 1.26.3). `go.mod` and `dev/go.mod` now declare `go 1.26.3`, CI `golangci-lint` bumped to v2.12.2, and `prek.toml` lockstep-bumped to v2.12.2 so local lint stays byte-aligned with CI. The new staticcheck `SA1019` analyzer also flagged `pq.ErrorCode` (deprecated in `lib/pq` v1.10+) and we now use the `pqerror.Code` type from `github.com/lib/pq/pqerror` — the subpackage that exists in `lib/pq` v1.12.2 (our pinned version), contrary to older nolint rationale comments that have been removed.

## [0.16.1-baselayer.11] - 2026-05-12

### Fixed
- `lock_timeout` retries during Complete-phase view re-projection no longer poison the connection. `ensureView` used to send `BEGIN; DROP VIEW; CREATE VIEW; ALTER VIEW SET DEFAULT…; COMMIT` as a single multi-statement string via `ExecContext`. When `lock_timeout` (SQLSTATE 55P03) fired on the `DROP VIEW`, Postgres aborted the *implicit* transaction started by the literal `BEGIN`, but Go's `*sql.DB` had no idea — the pooled connection was returned in "transaction aborted" state. The retry path re-sent the same string; the leading `BEGIN` became a notice ("there is already a transaction in progress"), the next statement returned `25P02`, and since `25P02` isn't `55P03` the retry loop treated it as terminal. Net result: the 5-minute retry budget produced exactly one real attempt, which is unrecoverable under continuous app-pod read load on the new version views. `ensureView` now uses `WithRetryableTransaction` with separate `tx.ExecContext` calls per statement, so each retry opens a fresh `*sql.Tx`, lets Go's pool roll back cleanly on failure, and the configured budget actually runs to completion. Added regression test `TestCompleteRetriesViewProjectionOnLockTimeout`.

## [0.16.1-baselayer.10] - 2026-05-10

### Fixed
- NOT NULL constraint names from `add_column` and `set_not_null` no longer fossilize the temp `_pgroll_new_<col>_<scope>` token in `pg_constraint` on Postgres 17+. PostgreSQL 17 promoted NOT NULL from a column attribute to a real named constraint; before this fix, pgroll's inline `ADD COLUMN ... NOT NULL` and bare `ALTER COLUMN ... SET NOT NULL` statements ran against the in-flight temp column name, so PG auto-derived names like `<table>__pgroll_new_<col>_<hash>_not_null` that didn't follow the column rename at Complete and surfaced explicitly in `pg_dump` output. `ColumnSQLWriter` now accepts an explicit `NotNullConstraintName`, `NewAddColumnAction` threads the canonical `<table>_<col>_not_null` name down to the `ADD COLUMN` SQL, and `setNotNullAction` looks up the auto-generated constraint after `SET NOT NULL` and renames it to the canonical form (no-op on PG <17 where the catalog row doesn't exist). The canonical name matches Postgres' default auto-name shape, so `pg_dump` suppresses the explicit `CONSTRAINT` clause and downstream `osiris.sql`-style dumps come out clean.

## [0.16.1-baselayer.9] - 2026-05-10

### Added
- `depends_on` — migrations may declare a list of migration names that must be applied before them, creating a DAG that `UnappliedMigrations` topologically sorts (Kahn's algorithm with filesystem order as the tiebreaker). Restores ordering guarantees for non-commutative migrations without forcing a strict positional history.
- `preconditions` — migrations may declare schema-state assertions that are validated before the migration runs. Eight assertion variants: `table_exists`, `table_not_exists`, `column_exists` (with optional type), `column_not_exists`, `index_exists`, `constraint_exists`, `function_exists` (with optional signature and SHA-256 body hash), and `type_exists` (with optional values hash for enums). Schema-level assertions run inside `Migration.Validate`; DB-level (`function_exists`, `type_exists`) run inside `Roll.Validate` against `pg_proc` / `pg_type`. Catches the "raw SQL silently runs against the wrong schema" class of bug — e.g. an `OpRawSQL` whose output depends on `normalize_name()` body, or a migration that assumes an enum has a specific set of values.
- `pgroll check <directory>` — filesystem-only validation (no DB connection required). Catches YAML/JSON syntax errors, missing/empty `operations`, schema names that exceed Postgres' 63-char identifier limit, `depends_on` targets that don't exist in the migration set, dependency cycles, and (advisory) raw-SQL operations without preconditions. With `--base origin/main`, also flags new migrations whose filenames sort before the base branch's latest migration — surfacing renames needed for the new name-set matching to remain ergonomic.

### Changed
- `UnappliedMigrations` matches by **migration name set** instead of strict linear filesystem order. A migration is considered unapplied iff its name is missing from the schema history. This unblocks divergent histories — hotfix branches, out-of-order merges — without renaming/shuffling files to keep a linear timeline. Migrations applied to the database that have no corresponding local file still produce a hard `ErrMismatchedMigration` (so accidental local deletions are still caught).
- `prek.toml` lint hook switched from upstream `golangci-lint` to `golangci-lint-full --config=.golangci.yml --timeout=30m`. The former passes `--new-from-rev HEAD --fix` and silently skipped a real `gosec` issue that CI flagged; the latter mirrors the CI lint job byte-for-byte.
- `prek.toml` `task-format` replaced with `task-format-clean` (runs `task format` and fails if it produced a diff). Local commits now reproduce the CI `format` check exactly instead of silently auto-fixing and letting drift through.
- `prek.toml` `task-generate-clean` now also fires when `cmd/*.go` changes — `cli-definition.json` is regenerated from the cobra command tree, so adding a new subcommand can stale that file even when `schema.json` and `tools/build-cli-definition.go` are untouched.

## [0.16.1-baselayer.8] - 2026-05-10

### Added
- `pgroll stamp <path>` — alembic-style state stamping that records migrations as already-applied without executing DDL. Use after loading a SQL dump (or recovering from missing/corrupt state) so pgroll's migrations table matches the live tables. The mode is implicit in the path: a single file stamps that one migration; a directory walks every file in lex order and chains through the latest (or `--up-to <name>`). `--type pgroll|baseline|inferred` (default `pgroll`); `--materialize` also creates the `<schema>_<version>` view layer over the leaf so apps have a schema to connect to. Idempotent — already-recorded names are skipped silently. Refuses during an active migration period.

### Changed
- `task format` (Taskfile.yml) now invokes `prettier` and `pgformatter` via Docker, matching the Makefile path CI runs. Brew-installed `pg_format` produced different `SELECT … INTO` formatting and silently reverted CI-required formatting in pre-commit. Contributors and CI now produce byte-identical output for embedded SQL.
- `prek.toml`: exclude `pkg/state/init.sql` from `end-of-file-fixer`. `backplane/pgformatter:latest` emits a trailing blank line on init.sql that the fixer would strip and the next pgformatter run would re-add — they were thrashing.

## [0.16.1-baselayer.7] - 2026-05-09

## [0.16.1-baselayer.6] - 2026-05-09

## [0.16.1-baselayer.5] - 2026-05-08

### Added
- Debian package distribution: GoReleaser now builds `linux/amd64` and `linux/arm64` `.deb` artifacts and attaches them to the GitHub release alongside the existing tarballs.
- `scripts/install-debian.sh` bootstrap script for first-time install on a Debian/Ubuntu VM (authenticates to the private release with a GitHub PAT).
- `pgroll-update` helper shipped inside the `.deb` (installed at `/usr/local/bin/pgroll-update`) for in-place upgrades — no more `scp`.
- Bash and zsh completions installed by the `.deb`.
- `task release:test:deb` — local end-to-end validation of the package via Docker.

## [0.16.1-baselayer.4] - 2026-05-07

## [0.16.1-baselayer.3] - 2026-05-07

## [0.16.1-baselayer.2] - 2026-05-07

## [0.16.1-baselayer.1] - 2026-05-05

### Added
- Private GoReleaser + Homebrew release workflow for Baselayer fork
- Taskfile with release, dry-run, and homebrew test tasks
- Use name-set matching for unapplied migration detection
- Deferred schema cleanup to avoid downtime when applying multiple migrations
