# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
