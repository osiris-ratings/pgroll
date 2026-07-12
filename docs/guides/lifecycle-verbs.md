# Lifecycle verbs: why they don't fold together

pgroll's migration lifecycle is expressed through five commands: `migrate`,
`complete`, `rollback`, `revert`, and `plan`. Several of them *look* like they
overlap — reverting is "migrating backward," and aborting an in-progress
migration is "reverting one step" — which invites the recurring question: can we
collapse them into fewer verbs?

This fork has already answered a version of that question once. The `seal` verb
was retired because the inversion engine made it redundant (see the CHANGELOG,
`[0.16.2-baselayer.14]`). That success is tempting to repeat, so this document
records **why the remaining verbs are each distinct** and gives a reusable test
for when a fold is actually justified — so the question doesn't have to be
re-litigated every few months.

The short version: **the current cut is right.** The apparent overlaps are
degenerate-case coincidences, not duplication.

## The lifecycle at a glance

```
                 migration history  (a linked list of revisions)

                 baseline ── A ── B ── C
                                       ▲ leaf
                                       │
     migrate <dir>                     │   forward, ADDITIVE
       apply the files on disk ────────┤   (expand only; the final migration is
       appends new revisions           │    left active unless --complete)
                                       │
     complete                          │   COMMIT / point of no return
       contract the active migration ──┤   (drain deferred, drop old schemas, seal)
                                       │
     rollback                          │   ABORT the in-progress migration
       undo the active (done=false) ───┤   (upstream triad: start │ complete │ rollback)
       leaf, delete its row            │
                                       │
     revert [--to X | --steps N]       │   REVERSE across the chain, back to an
       walk backward to an earlier ────┘   earlier revision (lossless while
       revision                            in-flight; inversion once contracted)

     plan <dir> [--to X]              READ-ONLY: {apply leg, revert leg, blocked}
       describe convergence to a       computed against one target — executes nothing
       target; execute nothing
```

## The five verbs

Each verb is tagged with its lineage — **upstream** (inherited from
[xataio/pgroll](https://github.com/xataio/pgroll)) or **fork** (added on
`baselayer`) — because lineage is load-bearing when weighing a fold (see
[The fold test](#the-fold-test)).

- **`migrate <dir> [--to X] [--complete]`** — *(upstream verb, fork-hardened.)*
  Forward and **purely additive**. It applies the migration files on disk in the
  expand phase and leaves the final migration active unless `--complete` is given
  (`cmd/migrate.go`). On this fork it never drops, drains, or seals anything, so a
  stale version-schema pin can never wedge a future deploy — it fails the current
  deploy loudly and retryably instead (CHANGELOG `[0.16.2-baselayer.14]`).

- **`complete`** — *(upstream verb; the fork moved contraction here.)* The single
  contraction step and point of no return: it drains deferred completes, drops the
  old version schemas, and stamps history rows `sealed` (`Roll.FinishContraction`,
  `pkg/roll/contract.go`).

- **`rollback`** — *(upstream foundation.)* Aborts the **in-progress**
  (`done=false`) migration: it undoes the expand phase and deletes the active leaf
  row (`Roll.Rollback` → `rollbackExpandPhase`, `pkg/roll/execute.go`). It is the
  third leg of the upstream `start` │ `complete` │ `rollback` authoring triad — the
  way you back out of a migration you just `start`ed but haven't committed.

- **`revert [--to X | --steps N] [--expand-only]`** — *(fork-only.)* The reverse
  gear across the **whole chain**. While a deployment is in flight it walks the
  migrations back losslessly (`Roll.Revert`, `pkg/roll/revert.go`); once a
  deployment has been contracted it switches to inversion — synthesized inverse
  migrations run *forward* through the normal engine and are then pruned from
  history (`Roll.RevertSealed`, `pkg/roll/revert_sealed.go`). Schema shape is
  restored exactly; contracted-history data is best-effort.

- **`plan <dir> [--to X] [--json]`** — *(fork-only.)* The read-only convergence
  brain. It describes what it would take to bring a database to a target — an
  `apply` leg, a `revert` leg, and a `blocked` set — and executes nothing
  (`Roll.Plan`, `pkg/roll/plan.go`).

## Why the overlaps aren't duplication

### `rollback` vs `revert`

These two are mechanically identical in **exactly one** case: when a single
in-flight migration is the only unsealed row, `rollback` and `revert --steps 1`
do the same thing — both route through `rollbackExpandPhase` against the same
parent snapshot and delete the same leaf row.

That is a coincidence of the degenerate case, not a shared concept:

- **Different operands.** `rollback`'s operand is *implicit*: "the migration in
  progress." `revert`'s operand is *explicit*: a target revision (`--to`) or a
  step count (`--steps`).
- **Different scope.** `revert` is a strict superset. It also walks `done=true`
  unsealed leaves (a completed-but-not-yet-contracted deploy) and reaches into
  *contracted* history via inversion — neither of which `rollback` can touch
  (`rollback` calls `GetActiveMigration`, which errors when nothing is in
  progress).
- **Different mental model.** `rollback` is "abort the thing I'm in the middle
  of," paired with `start`/`complete`. `revert` is "travel backward along the
  chain to an earlier point."

Analogy: `cd ..` and `cd /the/parent` land in the same directory when you're one
level deep. They're still different commands with different meanings.

### `revert` vs `migrate`

"Reverting is just migrating backward" is the most tempting fold, and the one
with the strongest reasons *against*:

- **Opposite safety envelopes.** `migrate` is additive and non-destructive by
  construction — that property is the whole point of the `.14` retirement, and it
  is what guarantees `migrate` can't wedge or destroy a deploy. `revert` is
  unavoidably destructive: it drops version schemas, deletes or prunes history
  rows, and for contracted history is lossy. Folding `revert` into `migrate`
  reintroduces destruction into the one verb designed never to have it.
- **Different inputs.** Forward `migrate` runs the migration **files on disk**.
  `revert` runs **nothing from disk** — it undoes remembered expand DDL, or
  synthesizes inverses from stored history snapshots. A "bidirectional migrate"
  wouldn't be one engine going two directions; it would be two engines behind one
  name.
- **Losslessness depends on it staying separate.** The in-flight window revert is
  lossless *precisely because* it is an undo, not a re-migration — the destructive
  DDL was never run, so old columns and dual-write triggers are still alive.
  Forcing everything through "migrate backward" (inversion) would push cases that
  have a lossless undo available onto the best-effort path — a data-safety
  regression.

## The fold test

Collapse verb A into verb B only when **both** hold:

1. **A is a strict, always-equivalent special case of B** — not just equivalent in
   a degenerate case.
2. **A carries no distinct lineage or mental model** whose loss would cost more
   than the duplication it removes.

Scored against this:

| Candidate | Strict special case? | No distinct lineage/model? | Verdict |
|---|---|---|---|
| `seal` → (inversion) | yes — pure plumbing, redundant once inversion existed | yes — fork-internal, no user-facing concept | **folded** |
| `rollback` → `revert` | no — equivalent only in the single-in-flight case | no — upstream triad, distinct operand & model | **kept** |
| `revert` → `migrate` | no — opposite effects on history and data | no — opposite safety envelope | **kept** |

`seal` passed both tests, so retiring it removed complexity for free. `rollback`
fails both: it is upstream foundation (removing it would mean carrying a
suppression diff against `cmd/rollback.go`, `pkg/roll/execute.go`, and the
upstream docs, re-resolved on every rebase) for no functional gain, since
`revert --steps 1` already exists alongside it. `revert`↔`migrate` fails hardest.

## Where "converge to revision N" is genuinely true: `plan`

The intuition behind these fold questions is real — a database and a checkout
*do* converge toward a single target revision, symmetrically, whether that means
applying forward or reverting backward. But that symmetry lives in **planning**,
not **execution**.

`plan` already expresses it end-to-end: for a given target it emits an `apply`
leg and a `revert` leg (carrying `to_schema`, `would_drop_schemas`,
`contains_contracted`) plus a `blocked` set, computed together and executing
nothing (`Roll.Plan`, `pkg/roll/plan.go`). A deploy tool reads one `plan` and
decides direction from it.

Execution stays two verbs because the *mechanisms* (files-on-disk forward vs
remembered-DDL / synthesized-inverse backward) and the *safety profiles*
(non-destructive vs destructive/lossy) genuinely differ. Unifying the plan while
keeping the execution verbs distinct is the correct factoring, not a compromise.

## See also

- [`docs/cli/rollback.mdx`](../cli/rollback.mdx) — the upstream `rollback` reference.
- [Migration safety design](./migration-safety-design.md) — ordering, name-set
  matching, and preconditions.
- [Divergent histories](./divergent-histories.md) — how convergence handles a
  database and checkout that have forked.
- CHANGELOG `[0.16.2-baselayer.14]` (the seal retirement) and `[0.16.2-baselayer.15]`
  (the `plan` / `revert --dry-run` planning surface).
