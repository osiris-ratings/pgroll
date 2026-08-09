# Migration targets

One migrations directory, more than one database.

## The problem

Some deployments run more than one Postgres database over what is logically one
schema. The case this was built for is an ETL database holding a subset of the
tables, logically replicated into an application database. Replication carries
**data only**, so both databases have to apply their own DDL — from the same
authored migrations, because they are the same schema.

Locally, and on each per-developer instance, there is exactly one database that
must hold everything.

## The mechanism

A migration may declare the targets it belongs to:

```yaml
targets:
  - etl
operations:
  - add_column:
      table: sos_registrations
      column: { name: status, type: text, nullable: true }
```

and `pgroll` takes a `--target`:

```console
$ pgroll migrate migrations --target app --postgres-url "$APP_URL"
$ pgroll migrate migrations --target etl --postgres-url "$ETL_URL"
```

Three rules, and that is the whole feature:

1. **No `--target` means no filtering.** Every migration applies. That *is*
   single-database mode — dev worktrees, CI, disposable instances — and it needs
   no flag, no host comparison and no special case. The number of `--target`
   flags in play equals the number of databases you have.
2. **`--target X` applies migrations whose `targets` contain `X`.** Names are
   free-form strings compared verbatim. pgroll assigns them no meaning, so
   adding a third database costs no pgroll change; deciding which names are
   legal is a job for your own linter, which is where the domain knowledge is.
3. **Validation always reads the unfiltered directory.** Only selection filters.

Rule 3 is the one that matters.

## Why validation stays unfiltered

`pgroll migrate` refuses to run when the database's history contains a migration
with no local file — that check is what stops a deleted or renamed migration
from silently diverging a database. It is deliberately one-directional: a
history row with no file is fatal, a file with no history row is merely
unapplied.

Filtering only the selection pass, and leaving validation reading the whole
directory, means a database can hold history for migrations the active target
does not select. That is not a corner case; it is the normal shape. An ETL
database provisioned as a volume clone of the application database starts life
with the application's entire migration chain in `pgroll.migrations`. Under
`--target etl` it keeps that history, validates cleanly, and simply stops
receiving application-only migrations from the first targeted deploy onward.

**No re-baseline. No routing epoch. No staggered cutover.** Adopting targets on
an existing pair of databases is a deploy, not a migration project.

## The tag requirement

With `--target` in effect, every *selection candidate* — a migration that is
post-baseline and not yet applied — must declare `targets`. An untagged
candidate is a hard error naming the file:

```
migration file "20260810_add_status.yaml" must declare `targets`
(--target "etl" is in effect); name the target(s) this migration belongs to,
for example `targets: [etl]`. There is no default target
```

There is no default and no fail-open. A migration silently withheld from a
database it should have reached does not announce itself — it surfaces weeks
later as replication quietly dropping rows.

Already-applied migrations are never inspected for tags. That is what lets an
existing database adopt targets without back-stamping its history.

The corollary is worth stating plainly, because it decides your topologies: on a
database with **empty** history every post-baseline file is a candidate, so
bootstrapping a fresh database with `--target` requires the whole directory to
be tagged, or a baseline set first. Building the ETL schema from scratch in CI —
a good idea, since replaying the target's set into an empty database and
asserting the resulting table set is the only real detector of mis-routing — is
exactly that case.

## Dependencies across targets

`depends_on` expresses ordering, not reference. A dependency the active target
does not select will never be applied to this database, so it imposes no
ordering here and is treated as satisfied by construction.

`pgroll check` warns when a dependency's targets do not cover the dependent's,
so a `depends_on` written as a semantic prerequisite is caught at authoring
time rather than relying on that leniency. If one slips through, it fails loudly
at execution — a missing relation — rather than quietly.

## Rollout: the binary lockstep is one-way

Migration files are decoded strictly. The moment a committed file carries
`targets`, **any** pgroll older than the release that understands it hard-rejects
that file — and because the whole directory is read at once, the rejection takes
out `check`, `plan`, `latest` and `migrate`, on every database, including ones
that have nothing to do with targets.

So:

1. Ship the release, and upgrade **every** consumer: CI, deploy runners,
   developer machines, and any long-lived instance image with a baked-in binary.
   Land the pin bump in one commit so there is a single "everything is on ≥ N"
   landmark.
2. Only then commit the first migration carrying `targets`. Never land the
   parser change and a tagged file in the same change.

Two things bound the damage. Strictness is **disk-only** — every state-reading
path ignores unknown fields — so an older binary reading rows written by a newer
one is fine, and a downgrade survives as long as the tagged files are not in the
directory it reads. And a version floor checked at install time (compare
`pgroll --version` against your pin and fail) is worth more than any error
message, because it works for people already running a stale binary.

Note one less obvious way the field enters a checkout: once a targeted migration
is applied, its history body carries `targets`, so `pgroll pull` will write it
into the directory.

## What this does not do

pgroll does not orchestrate multiple databases. One invocation is still one
`--postgres-url` and one schema, and the legs of a multi-database deploy differ
in ways that are structural rather than incidental — whether contraction is
inline, whether version schemas are projected. Sequencing them is the deploy
tool's job. Targets only decide *which migrations* each invocation applies.
