# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
