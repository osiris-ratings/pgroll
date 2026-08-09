// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/xataio/pgroll/pkg/migrations"
)

// PlanResult is the machine-readable, execution-free description of what it
// would take to converge a target database's migration history to a local
// migrations directory. It is the JSON contract emitted by `pgroll plan`:
// callers branch on its fields (apply / revert / diverged / blocked) rather
// than reaching into pgroll's internal tables.
type PlanResult struct {
	// Schema is the target schema.
	Schema string `json:"schema"`
	// LiveSchema is the version schema apps currently resolve through (the
	// `latest schema`); nil when none exists.
	LiveSchema *string `json:"live_schema"`
	// Status is the point-in-time migration status: "No migrations",
	// "In progress", or "Complete".
	Status string `json:"status"`
	// ActiveMigration is the in-flight (done=false) history leaf; nil when no
	// migration is in progress.
	ActiveMigration *string `json:"active_migration"`
	// DBLatest is the newest migration in the database, including an active
	// (done=false) leaf; nil when history is empty.
	DBLatest *string `json:"db_latest"`
	// LocalLatest is the newest migration file on disk that the active target
	// selects; with no target, simply the newest file. Nil when the directory
	// holds no migrations.
	LocalLatest *string `json:"local_latest"`
	// Target is the deployment target this plan was computed for; empty when
	// no filtering was in effect. Reported so a plan produced with the wrong
	// --target is self-evidently so.
	Target string `json:"target,omitempty"`
	// InSync is true when the database leaf equals the local leaf and the
	// deployment is Complete (nothing to apply, revert, or contract).
	InSync bool `json:"in_sync"`
	// Diverged is true when the local and database histories fork: neither
	// leaf appears in the other's history.
	Diverged bool `json:"diverged"`

	// Apply lists the forward migrations to apply, oldest first.
	Apply ApplyView `json:"apply"`
	// Revert describes the migrations to walk back to reach the convergence
	// target.
	Revert RevertView `json:"revert"`
	// Blocked lists database migrations absent from the checkout that cannot
	// be cleanly reverted (interleaved history, or an un-synthesizable
	// inverse).
	Blocked BlockedView `json:"blocked"`
}

// ApplyView is the forward leg of a plan: the migrations to apply, in apply
// order (oldest → newest).
type ApplyView struct {
	Count      int      `json:"count"`
	Migrations []string `json:"migrations"`
}

// RevertView is the backward leg of a plan: the migrations to revert to reach
// the convergence/restore target, in revert order (newest → oldest). It is
// also the standalone `revert --dry-run` output.
type RevertView struct {
	Count int `json:"count"`
	// Migrations are the revert targets, newest first.
	Migrations []string `json:"migrations"`
	// To is the migration that becomes the history leaf after the revert; nil
	// means the database returns to empty (no version schema remains).
	To *string `json:"to"`
	// ToSchema is the version schema apps must repin to before the revert (and
	// any contraction that finishes it); nil when none remains.
	ToSchema *string `json:"to_schema"`
	// Contiguous is true when the revert set is a contiguous suffix of history.
	Contiguous bool `json:"contiguous"`
	// ContainsContracted is true when any target has already been contracted
	// (sealed): the revert proceeds by inversion (schema-exact, best-effort
	// data) rather than a lossless window rollback.
	ContainsContracted bool `json:"contains_contracted"`
	// WouldDropSchemas are the version schemas the revert (and its contraction)
	// drops — the set to guard application pins against.
	WouldDropSchemas []string `json:"would_drop_schemas"`
}

// BlockedView lists database migrations that stand in the way of convergence
// but cannot be cleanly reverted.
type BlockedView struct {
	Count int `json:"count"`
	// Reason is a fixed, machine-branchable classification — one of
	// "non-contiguous", "target not found", "inverse unavailable",
	// "window open", "no convergence target", or the catch-all "unavailable";
	// nil when nothing is blocked. Never a free-form error message.
	Reason     *string  `json:"reason"`
	Migrations []string `json:"migrations"`
}

// planOptions holds the optional bounds for Plan.
type planOptions struct {
	to string
}

// PlanOption configures Plan.
type PlanOption func(*planOptions)

// WithPlanTo overrides the convergence target: the plan reverts history down
// to (and keeping) the named migration, which must already exist in the
// database history. Revert-only — it does not bound the forward leg.
func WithPlanTo(to string) PlanOption {
	return func(o *planOptions) { o.to = to }
}

// Plan computes the plan to converge the target database's migration history
// to the local migrations directory `dir`. It executes nothing: every field
// is derived from state pgroll already exposes, and the revert leg reuses the
// same planners `revert` runs before it acts.
//
// Plan returns an error only when a plan cannot be produced at all — the
// database is unreachable, or an explicit WithPlanTo target is absent from
// history. A convergence that is impossible for structural reasons (forked or
// interleaved history, an un-invertible sealed segment) is reported in-band
// via the Blocked field, not as an error.
func (m *Roll) Plan(ctx context.Context, dir fs.FS, opts ...PlanOption) (*PlanResult, error) {
	var o planOptions
	for _, opt := range opts {
		opt(&o)
	}

	res := &PlanResult{
		Schema:  m.schema,
		Apply:   ApplyView{Migrations: []string{}},
		Revert:  RevertView{Migrations: []string{}, WouldDropSchemas: []string{}, Contiguous: true},
		Blocked: BlockedView{Migrations: []string{}},
	}

	// Point state: status, live schema, active leaf, db leaf.
	status, err := m.Status(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading status: %w", err)
	}
	res.Status = string(status.Status)
	if status.Version != "" {
		live := VersionedSchemaName(m.schema, status.Version)
		res.LiveSchema = &live
	}

	dbLatest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading latest migration: %w", err)
	}
	if dbLatest != nil {
		res.DBLatest = dbLatest
		// The leaf is the in-flight migration exactly when the deployment is
		// in progress — Status already resolved that from the active period,
		// so no second state query is needed.
		if status.Status == InProgressMigrationStatus {
			leaf := *dbLatest
			res.ActiveMigration = &leaf
		}
	}

	// Database history (ascending, post-baseline) and its version schemas.
	history, err := m.state.SchemaHistory(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading schema history: %w", err)
	}
	dbNames := make([]string, len(history))
	dbSet := make(map[string]struct{}, len(history))
	versionSchemaByName := make(map[string]string, len(history))
	for i, h := range history {
		dbNames[i] = h.Migration.Name
		dbSet[h.Migration.Name] = struct{}{}
		versionSchemaByName[h.Migration.Name] = rawVersionSchemaName(h.Migration)
	}

	baseline, err := m.state.LatestBaseline(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}
	baselineName := ""
	if baseline != nil {
		baselineName = baseline.Name
	}

	// Local migrations (ascending file order, post-baseline).
	//
	// Note carefully which of the two sets each consumer below takes. localSet
	// answers "does this database migration still exist on disk?" and MUST be
	// the unfiltered set: under a --target, a database legitimately holds
	// history for migrations this target does not select, and treating those
	// as absent makes the convergence search below walk past them and emit a
	// revert leg covering the whole inherited history. Only the forward leg is
	// filtered, because only it describes what `migrate` would apply.
	local, err := resolveLocalSet(dir, baselineName, dbSet, m.target)
	if err != nil {
		return nil, fmt.Errorf("reading local migrations: %w", err)
	}
	localRaws := local.All
	localNames := make([]string, len(localRaws))
	localSet := make(map[string]struct{}, len(localRaws))
	for i, raw := range localRaws {
		localNames[i] = raw.Name
		localSet[raw.Name] = struct{}{}
	}
	if len(local.Selected) > 0 {
		leaf := local.Selected[len(local.Selected)-1].Name
		res.LocalLatest = &leaf
	}
	res.Target = m.target

	// Divergence is a pure function of the two leaves (in_sync is resolved at
	// the end, once the apply/revert/blocked legs are known — leaf equality
	// alone is not enough: a checkout migration older than the shared leaf can
	// still be unapplied).
	if res.DBLatest != nil && res.LocalLatest != nil {
		_, dbLeafLocal := localSet[*res.DBLatest]
		_, localLeafDB := dbSet[*res.LocalLatest]
		res.Diverged = !dbLeafLocal && !localLeafDB
	}

	// Explicit target: revert-only. The target must already be applied — a
	// migration in history, or the baseline (a legal revert boundary the
	// sealed planner accepts).
	if o.to != "" {
		_, inHistory := dbSet[o.to]
		if !inHistory && o.to != baselineName {
			return nil, fmt.Errorf("--to target %q not found in database history", o.to)
		}
		suffix := dbNames
		for i, n := range dbNames {
			if n == o.to {
				suffix = dbNames[i+1:]
				break
			}
		}
		m.fillRevertLeg(ctx, res, o.to, suffix, versionSchemaByName)
		res.InSync = inSync(res, status.Status, m.target != "")
		return res, nil
	}

	// Forward leg: local migrations not yet in the database, in the order
	// `migrate` would apply them (depends_on topological order, filename order
	// as the tiebreaker) so the plan matches what migrate actually runs.
	// This is the one leg that filters: it must match what `migrate` would do,
	// or the plan stops being a truthful preview of it.
	unapplied := make([]*migrations.RawMigration, 0, len(local.Selected))
	for _, raw := range local.Selected {
		if _, ok := dbSet[raw.Name]; !ok {
			unapplied = append(unapplied, raw)
		}
	}
	sorted, err := TopoSortMigrations(unapplied, dbSet, local.Excluded)
	if err != nil {
		return nil, fmt.Errorf("ordering unapplied migrations: %w", err)
	}
	for _, raw := range sorted {
		res.Apply.Migrations = append(res.Apply.Migrations, raw.Name)
	}
	res.Apply.Count = len(res.Apply.Migrations)

	// Convergence target: the newest database migration (walking down from the
	// leaf) that still exists locally. Everything above it is absent locally
	// and forms the revert suffix. When no database migration exists locally,
	// the convergence point is the baseline (revert everything since it).
	to := ""
	toFound := false
	toIdx := -1
	for i := len(dbNames) - 1; i >= 0; i-- {
		if _, ok := localSet[dbNames[i]]; ok {
			to = dbNames[i]
			toFound = true
			toIdx = i
			break
		}
	}

	var revertSuffix []string
	if toFound {
		revertSuffix = dbNames[toIdx+1:]
	} else {
		revertSuffix = dbNames
	}

	// Interleaved history: database migrations absent locally that sit at or
	// below the convergence point. They cannot be reverted without also
	// unwinding in-checkout migrations above them — reported as blocked.
	suffixSet := make(map[string]struct{}, len(revertSuffix))
	for _, n := range revertSuffix {
		suffixSet[n] = struct{}{}
	}
	interleaved := make([]string, 0)
	for _, n := range dbNames {
		if _, local := localSet[n]; local {
			continue
		}
		if _, inSuffix := suffixSet[n]; inSuffix {
			continue
		}
		interleaved = append(interleaved, n)
	}

	// Revert leg. Nothing to revert when the suffix is empty (pure apply or
	// in-sync).
	if len(revertSuffix) > 0 {
		switch {
		case toFound:
			m.fillRevertLeg(ctx, res, to, revertSuffix, versionSchemaByName)
		case baselineName != "":
			// No shared migration: converge onto the baseline.
			m.fillRevertLeg(ctx, res, baselineName, revertSuffix, versionSchemaByName)
		default:
			// No shared migration and no baseline to fall back on: there is no
			// safe convergence target to revert toward.
			reason := "no convergence target"
			res.Blocked = BlockedView{Count: len(revertSuffix), Reason: &reason, Migrations: append([]string{}, revertSuffix...)}
			res.Revert.Contiguous = false
		}
	}

	// Merge the interleaved set into blocked, unless the revert leg already
	// failed for a more specific reason.
	if len(interleaved) > 0 && res.Blocked.Count == 0 {
		reason := "non-contiguous"
		res.Blocked = BlockedView{Count: len(interleaved), Reason: &reason, Migrations: interleaved}
	}

	res.InSync = inSync(res, status.Status, m.target != "")
	return res, nil
}

// inSync reports whether the database already matches the checkout: the
// deployment is contracted (Complete) and there is nothing to apply, revert,
// or unblock. Leaf equality alone is insufficient — a checkout migration
// older than the shared leaf can still be unapplied.
func inSync(res *PlanResult, status MigrationStatus, targeted bool) bool {
	if status != CompleteMigrationStatus ||
		res.Apply.Count != 0 || res.Revert.Count != 0 || res.Blocked.Count != 0 {
		return false
	}
	if targeted {
		// Under a --target the database leaf may be a migration this target
		// does not select — on a host that inherited another target's history
		// it usually is — so leaf equality is not a convergence signal and
		// would report a fully converged database as permanently out of sync.
		// The three empty legs already establish that this target's slice of
		// the directory is fully applied and nothing extraneous is present.
		return true
	}
	return res.DBLatest != nil && res.LocalLatest != nil && *res.DBLatest == *res.LocalLatest
}

// fillRevertLeg computes the composed (window + sealed) revert down to `to`
// and writes it into res.Revert, or classifies a planner refusal into
// res.Blocked. Errors are reported in-band: Plan never fails because a revert
// is structurally impossible. `candidates` are the database migrations the
// revert was meant to walk back — reported as the blocked set when the
// planner refuses, so Blocked.Migrations is never empty with a non-zero count.
func (m *Roll) fillRevertLeg(ctx context.Context, res *PlanResult, to string, candidates []string, versionSchemaByName map[string]string) {
	view, err := m.previewRevertTo(ctx, to, versionSchemaByName)
	if err != nil {
		reason := classifyBlockedReason(err)
		blocked := append([]string{}, candidates...)
		count := len(blocked)
		if count == 0 {
			count = 1
		}
		res.Blocked = BlockedView{Count: count, Reason: &reason, Migrations: blocked}
		res.Revert.Contiguous = false
		return
	}
	res.Revert = *view
}

// PreviewRevert produces the RevertView a Revert with the same options would
// carry out, without executing anything. It mirrors the command's bound
// selection: bare / --steps walk the in-flight window; --to composes the
// window and sealed legs down to the named target exactly as `revert --to`
// would.
func (m *Roll) PreviewRevert(ctx context.Context, opts ...RevertOption) (*RevertView, error) {
	var o revertOptions
	for _, opt := range opts {
		opt(&o)
	}

	if o.to != "" {
		// nil map: composedRevertView reads the version schemas lazily, only
		// if it actually reaches the sealed leg — a bare window revert to `to`
		// never touches history here.
		return m.previewRevertTo(ctx, o.to, nil)
	}

	// Bare or --steps: the in-flight window only.
	plan, err := m.RevertPlan(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return m.windowView(ctx, plan, nil)
}

// previewRevertTo previews a revert down to (and keeping) `to`, composing the
// lossless window leg with an inversion leg when `to` has been contracted —
// exactly as `revert --to` composes them, but plan-only.
func (m *Roll) previewRevertTo(ctx context.Context, to string, versionSchemaByName map[string]string) (*RevertView, error) {
	plan, err := m.RevertPlan(ctx, WithRevertTo(to))
	switch {
	case err == nil:
		// Pure lossless window revert; `to` is kept as the leaf.
		boundary := to
		return m.windowView(ctx, plan, &boundary)
	case errors.Is(err, ErrRevertTargetSealed):
		return m.composedRevertView(ctx, to, versionSchemaByName)
	default:
		return nil, err
	}
}

// windowView builds a RevertView from a lossless window revert plan. boundary
// is the migration kept as the leaf; nil means the database returns to the
// oldest target's parent (or empty when that parent is nil), matching a bare
// or --steps revert.
func (m *Roll) windowView(ctx context.Context, plan []RevertTarget, boundary *string) (*RevertView, error) {
	view := &RevertView{
		Migrations:       make([]string, 0, len(plan)),
		WouldDropSchemas: make([]string, 0, len(plan)),
		Contiguous:       true,
	}
	for _, t := range plan {
		view.Migrations = append(view.Migrations, t.Name)
		view.WouldDropSchemas = append(view.WouldDropSchemas, VersionedSchemaName(m.schema, t.VersionSchema))
	}
	view.Count = len(view.Migrations)

	// Resolve the restore target: an explicit boundary, or the oldest target's
	// parent for a bare/--steps revert.
	restoreName := ""
	if boundary != nil {
		restoreName = *boundary
	} else if len(plan) > 0 {
		if p := plan[len(plan)-1].Parent; p != nil {
			restoreName = *p
		}
	}
	// A nil restore target means the database returns to empty; ToSchema stays
	// nil. When a target exists, resolving its version schema must succeed —
	// mirroring runWindowRevert, a lookup failure is fatal rather than a
	// silent (and misleading) "empty database" restore.
	if restoreName != "" {
		view.To = &restoreName
		mig, err := m.state.GetMigration(ctx, m.schema, restoreName)
		if err != nil {
			return nil, fmt.Errorf("unable to resolve the restore target %q: %w", restoreName, err)
		}
		s := VersionedSchemaName(m.schema, mig.VersionSchemaName())
		view.ToSchema = &s
	}
	return view, nil
}

// composedRevertView builds a RevertView for a revert that reaches contracted
// (sealed) history: the still-open window leg (if any) reverts first,
// losslessly, then the sealed segment down to `to` is undone by inversion.
func (m *Roll) composedRevertView(ctx context.Context, to string, versionSchemaByName map[string]string) (*RevertView, error) {
	windowTargets, err := m.RevertTargets(ctx)
	if err != nil {
		return nil, err
	}

	var sealed *SealedRevertPlan
	if len(windowTargets) > 0 {
		oldest := windowTargets[len(windowTargets)-1]
		if oldest.Parent == nil {
			return nil, fmt.Errorf("migration %q not found beneath the in-flight window", to)
		}
		sealed, err = m.PlanRevertSealedBelowWindow(ctx, to, *oldest.Parent)
	} else {
		sealed, err = m.PlanRevertSealed(ctx, to)
	}
	if err != nil {
		return nil, err
	}

	view := &RevertView{
		Migrations:         make([]string, 0, len(windowTargets)),
		WouldDropSchemas:   make([]string, 0, len(windowTargets)),
		Contiguous:         true,
		ContainsContracted: true,
	}
	for _, t := range windowTargets {
		view.Migrations = append(view.Migrations, t.Name)
		view.WouldDropSchemas = append(view.WouldDropSchemas, VersionedSchemaName(m.schema, t.VersionSchema))
	}
	if sealed != nil {
		// The sealed targets' version schemas are not carried on the plan;
		// resolve them from history. Built lazily so callers that never reach
		// the sealed leg (a bare window revert to `to`) pay no history read.
		if versionSchemaByName == nil {
			versionSchemaByName, err = m.versionSchemasByName(ctx)
			if err != nil {
				return nil, err
			}
		}
		for _, name := range sealed.Targets {
			view.Migrations = append(view.Migrations, name)
			vs := name
			if v, ok := versionSchemaByName[name]; ok && v != "" {
				vs = v
			}
			view.WouldDropSchemas = append(view.WouldDropSchemas, VersionedSchemaName(m.schema, vs))
		}
		leaf := sealed.Boundary
		view.To = &leaf
		s := VersionedSchemaName(m.schema, sealed.BoundaryVersionSchema)
		view.ToSchema = &s
	} else {
		leaf := to
		view.To = &leaf
	}
	view.Count = len(view.Migrations)
	return view, nil
}

// versionSchemasByName maps every post-baseline history migration name to its
// version schema name.
func (m *Roll) versionSchemasByName(ctx context.Context) (map[string]string, error) {
	history, err := m.state.SchemaHistory(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading schema history: %w", err)
	}
	out := make(map[string]string, len(history))
	for _, h := range history {
		out[h.Migration.Name] = rawVersionSchemaName(h.Migration)
	}
	return out, nil
}

// rawVersionSchemaName mirrors Migration.VersionSchemaName for a RawMigration
// history entry: the explicit version schema, or the migration name.
func rawVersionSchemaName(raw migrations.RawMigration) string {
	if raw.VersionSchema != "" {
		return raw.VersionSchema
	}
	return raw.Name
}

// classifyBlockedReason maps a revert-planner refusal to one of the fixed,
// machine-branchable reason tokens documented on BlockedView.Reason. It never
// returns the raw error text: an unrecognized refusal falls through to the
// stable catch-all "unavailable" so a consumer switching on the reason never
// has to parse a free-form sentence.
func classifyBlockedReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return "target not found"
	case strings.Contains(msg, "irreversible"),
		strings.Contains(msg, "cannot be inverted"),
		strings.Contains(msg, "captured from DDL"),
		strings.Contains(msg, "no operations"),
		strings.Contains(msg, "not a clean train boundary"):
		return "inverse unavailable"
	case strings.Contains(msg, "not contiguous"),
		strings.Contains(msg, "not an ancestor"),
		strings.Contains(msg, "advanced past the revert window"):
		return "non-contiguous"
	case strings.Contains(msg, "in progress"),
		strings.Contains(msg, "window is open"),
		strings.Contains(msg, "not sealed"):
		return "window open"
	default:
		return "unavailable"
	}
}
