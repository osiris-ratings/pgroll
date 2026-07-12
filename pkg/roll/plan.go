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
	// LocalLatest is the newest migration file on disk; nil when the directory
	// holds no migrations.
	LocalLatest *string `json:"local_latest"`
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
	// Reason is a short machine-branchable classification, e.g.
	// "non-contiguous", "target not found", "inverse unavailable"; nil when
	// nothing is blocked.
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

	active, err := m.state.IsActiveMigrationPeriod(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading active migration period: %w", err)
	}
	dbLatest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("reading latest migration: %w", err)
	}
	if dbLatest != nil {
		res.DBLatest = dbLatest
		if active {
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

	// Local migration names (ascending, post-baseline).
	localNames, err := localMigrationNames(dir, baselineName)
	if err != nil {
		return nil, fmt.Errorf("reading local migrations: %w", err)
	}
	localSet := make(map[string]struct{}, len(localNames))
	for _, n := range localNames {
		localSet[n] = struct{}{}
	}
	if len(localNames) > 0 {
		leaf := localNames[len(localNames)-1]
		res.LocalLatest = &leaf
	}

	// Divergence and sync are pure functions of the two leaves.
	if res.DBLatest != nil && res.LocalLatest != nil {
		_, dbLeafLocal := localSet[*res.DBLatest]
		_, localLeafDB := dbSet[*res.LocalLatest]
		res.Diverged = !dbLeafLocal && !localLeafDB
		res.InSync = *res.DBLatest == *res.LocalLatest && status.Status == CompleteMigrationStatus
	}

	// Explicit target: revert-only, and the target must already be applied.
	if o.to != "" {
		if _, ok := dbSet[o.to]; !ok {
			return nil, fmt.Errorf("--to target %q not found in database history", o.to)
		}
		m.fillRevertLeg(ctx, res, o.to, versionSchemaByName)
		return res, nil
	}

	// Forward leg: local migrations not yet in the database, file order.
	for _, n := range localNames {
		if _, ok := dbSet[n]; !ok {
			res.Apply.Migrations = append(res.Apply.Migrations, n)
		}
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
			m.fillRevertLeg(ctx, res, to, versionSchemaByName)
		case baselineName != "":
			// No shared migration: converge onto the baseline.
			m.fillRevertLeg(ctx, res, baselineName, versionSchemaByName)
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

	return res, nil
}

// fillRevertLeg computes the composed (window + sealed) revert down to `to`
// and writes it into res.Revert, or classifies a planner refusal into
// res.Blocked. Errors are reported in-band: Plan never fails because a revert
// is structurally impossible.
func (m *Roll) fillRevertLeg(ctx context.Context, res *PlanResult, to string, versionSchemaByName map[string]string) {
	view, err := m.previewRevertTo(ctx, to, versionSchemaByName)
	if err != nil {
		reason := classifyBlockedReason(err)
		res.Blocked = BlockedView{Count: 1, Reason: &reason, Migrations: []string{}}
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

	versionSchemaByName, err := m.versionSchemasByName(ctx)
	if err != nil {
		return nil, err
	}

	if o.to != "" {
		return m.previewRevertTo(ctx, o.to, versionSchemaByName)
	}

	// Bare or --steps: the in-flight window only.
	plan, err := m.RevertPlan(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return m.windowView(ctx, plan, nil), nil
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
		return m.windowView(ctx, plan, &boundary), nil
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
func (m *Roll) windowView(ctx context.Context, plan []RevertTarget, boundary *string) *RevertView {
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
	if restoreName != "" {
		view.To = &restoreName
		if mig, err := m.state.GetMigration(ctx, m.schema, restoreName); err == nil {
			s := VersionedSchemaName(m.schema, mig.VersionSchemaName())
			view.ToSchema = &s
		}
	}
	return view
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

// localMigrationNames returns the names of the migrations in `dir` that come
// after the baseline, in filename (lexicographic) order. An empty directory
// yields an empty slice rather than an error.
func localMigrationNames(dir fs.FS, baselineName string) ([]string, error) {
	files, err := migrations.CollectFilesFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading migration files: %w", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		raw, err := migrations.ReadRawMigration(dir, f)
		if err != nil {
			return nil, fmt.Errorf("reading migration file %q: %w", f, err)
		}
		if raw.Name > baselineName {
			names = append(names, raw.Name)
		}
	}
	return names, nil
}

// classifyBlockedReason maps a revert-planner refusal to a short,
// machine-branchable reason for the Blocked field.
func classifyBlockedReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return "target not found"
	case strings.Contains(msg, "irreversible"),
		strings.Contains(msg, "cannot be inverted"),
		strings.Contains(msg, "captured from DDL"),
		strings.Contains(msg, "no operations"):
		return "inverse unavailable"
	case strings.Contains(msg, "not contiguous"),
		strings.Contains(msg, "not an ancestor"),
		strings.Contains(msg, "advanced past the revert window"):
		return "non-contiguous"
	default:
		return msg
	}
}
