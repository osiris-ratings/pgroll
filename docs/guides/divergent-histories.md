# Handling Divergent Migration Histories

## The problem

pgroll tracks which migrations have been applied to a database in
the `pgroll.migrations` table. When you run `pgroll migrate`, it
compares this history against the local migration files to determine
which migrations still need to be applied.

In a strictly linear workflow — one branch, one environment — the
order of migrations in the database always matches the order on the
filesystem. But real-world teams often have multiple branches
deploying to different environments, and this creates **divergent
histories**: the database has migrations applied in a different
order than the filesystem implies.

### Common scenario: hotfix branches

```
main:    A → B → C → D → (merge H) → E
hotfix:       └── H ──┘
```

1. `main` has migrations `A`, `B`, and is developing `C`, `D`.
2. A critical bug requires a hotfix. A branch is created from
   `main` at `B`, adding migration `H`.
3. The hotfix is deployed to production. Production's history is
   now `[A, B, H]`.
4. Meanwhile, `C` and `D` are merged to `main` and deployed to
   staging. Staging's history is `[A, B, C, D]`.
5. The hotfix branch is merged back to `main`. The filesystem now
   has `[A, B, C, D, H]`.

At this point:

| Environment | DB History | Filesystem |
|-------------|-----------|------------|
| Production  | `[A, B, H]` | `[A, B, C, D, H]` |
| Staging     | `[A, B, C, D]` | `[A, B, C, D, H]` |

Both environments need to apply the migrations they're missing, but
neither history matches the filesystem order position-by-position.

## How pgroll handles this

pgroll uses **name-set matching** to determine which migrations are
unapplied. Instead of requiring the database history to match the
filesystem order index-by-index, it:

1. Builds a set of migration names from the database history.
2. Builds a set of migration names from local files (after any
   baseline).
3. Validates that every migration in the database has a
   corresponding local file. If a migration exists in the database
   but not locally, this indicates a deleted migration file — pgroll
   returns an error.
4. Returns all local migrations not in the database set, in
   filesystem order.

For the hotfix scenario above:

- **Production** (`[A, B, H]`): pgroll sees `C` and `D` are not in
  the applied set → returns `[C, D]` as unapplied.
- **Staging** (`[A, B, C, D]`): pgroll sees `H` is not in the
  applied set → returns `[H]` as unapplied.

Both environments converge to the same state after running
`pgroll migrate`.

## Safety guarantees

### Missing local files are caught

If the database contains a migration that has no corresponding local
file, pgroll returns an error:

```
mismatched migration: migration "05_hotfix" exists in schema history
but not in local migration files
```

This prevents accidentally running migrations against a database
whose history includes migrations from a branch you haven't merged.

### Application order is safe

pgroll's `Start()` function always sets the new migration's parent
to the latest migration in the database. This means unapplied
migrations chain correctly onto whatever the database's current
state is, regardless of their position in the filesystem relative
to already-applied migrations.

### Baselines are unaffected

Name-set matching operates only on migrations after the latest
baseline. If you use baselines, divergent history handling works
the same way — only post-baseline migrations are considered.

## Best practices

1. **Use timestamp prefixes for migration filenames.** This ensures
   filesystem order reflects intended chronological order (e.g.,
   `20240115120000_create_users.json`).

2. **Rebase feature branches before merging.** If your feature
   branch has migrations that sort before migrations already on
   `main`, rebase the migration timestamps so they sort after. This
   keeps the filesystem order clean, even though pgroll tolerates
   divergence.

3. **Don't delete migration files.** pgroll validates that every
   applied migration has a local file. Deleting a file for an
   applied migration will cause an error. Use baselines to hide
   old history instead.

4. **Merge hotfix branches promptly.** The longer a hotfix branch
   lives independently, the more divergence accumulates. Merge back
   to `main` as soon as the hotfix is verified.
