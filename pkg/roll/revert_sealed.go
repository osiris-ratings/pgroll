// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/state"
)

// SealedRevertPlan describes a revert of SEALED history: the forward
// migrations to be undone (newest first), the boundary the database returns
// to, and the synthesized inverse train.
//
// Unlike a window revert, a sealed revert is best-effort for data: the
// forward migrations' contraction has physically run, so dropped values are
// re-derived through the original up/down expressions rather than restored.
// Schema shape is restored exactly (modulo snapshot fidelity).
type SealedRevertPlan struct {
	// Targets are the sealed migrations to undo, newest first.
	Targets []string
	// Boundary is the migration that becomes the history leaf.
	Boundary string
	// BoundaryVersionSchema is its version schema name (the schema apps
	// must be repinned to).
	BoundaryVersionSchema string
	// Inverses is the synthesized inverse train, in application order.
	Inverses []*migrations.Migration
}

// pollutionMarkers identify pgroll-internal expand artifacts inside a
// stored resulting_schema; their presence means the snapshot was captured
// mid-flight and cannot anchor inverse synthesis.
var pollutionMarkers = []string{"_pgroll_new_", "_pgroll_needs_backfill", "_pgroll_del_", "_pgroll_dup_"}

// sealedRevertOptions holds options for RevertSealed.
type sealedRevertOptions struct {
	expandOnly bool
}

// SealedRevertOption is a functional option for RevertSealed.
type SealedRevertOption func(*sealedRevertOptions)

// WithExpandOnly makes RevertSealed stop after the inverse train's expand
// phase: the final inverse migration is left active (its version schema — the
// restored boundary projection — exists alongside the current one), and
// nothing is contracted or pruned. This is the zero-downtime revert split:
// apps repin to the restored schema between this call and the `pgroll
// complete` that contracts the inverses and prunes history.
func WithExpandOnly() SealedRevertOption {
	return func(o *sealedRevertOptions) { o.expandOnly = true }
}

// PlanRevertSealed computes the plan for reverting sealed history down to
// (but not including) the named boundary migration.
func (m *Roll) PlanRevertSealed(ctx context.Context, to string) (*SealedRevertPlan, error) {
	if _, err := m.state.GetActiveMigration(ctx, m.schema); err == nil {
		return nil, fmt.Errorf("a migration is in progress; complete or roll it back before reverting sealed history")
	}

	unsealed, err := m.state.UnsealedMigrations(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	if len(unsealed) > 0 {
		return nil, fmt.Errorf("the revert window is open (%d unsealed migration(s)); revert it first with a plain revert", len(unsealed))
	}

	return m.planSealedSegment(ctx, to, "")
}

// PlanRevertSealedBelowWindow plans the sealed leg of a composed revert while
// the revert window is still open: the segment spans (to, through], where
// `through` is the window's lower boundary — the newest SEALED row the leg
// will revert. Used to preview and validate the sealed leg before the window
// leg executes; the actual execution re-plans via RevertSealed once the
// window has been walked back.
func (m *Roll) PlanRevertSealedBelowWindow(ctx context.Context, to, through string) (*SealedRevertPlan, error) {
	if through == "" {
		return nil, fmt.Errorf("the sealed leg of a composed revert needs the window's lower boundary")
	}
	return m.planSealedSegment(ctx, to, through)
}

// planSealedSegment computes the inverse plan for the sealed history segment
// (to, through]; an empty `through` means the segment extends to the history
// leaf.
func (m *Roll) planSealedSegment(ctx context.Context, to, through string) (*SealedRevertPlan, error) {
	history, err := m.state.SchemaHistory(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("unable to read schema history: %w", err)
	}

	// Locate the boundary. SchemaHistory is post-baseline ascending; the
	// baseline itself is also a legal boundary (revert everything since).
	boundaryIdx := -1
	for i, h := range history {
		if h.Migration.Name == to {
			boundaryIdx = i
			break
		}
	}
	if boundaryIdx == -1 {
		baseline, err := m.state.LatestBaseline(ctx, m.schema)
		if err != nil {
			return nil, err
		}
		if baseline == nil || baseline.Name != to {
			return nil, fmt.Errorf("migration %q not found in history", to)
		}
	}

	segment := history[boundaryIdx+1:]
	if through != "" {
		throughIdx := -1
		for i, h := range history {
			if h.Migration.Name == through {
				throughIdx = i
				break
			}
		}
		if throughIdx == -1 {
			return nil, fmt.Errorf("migration %q not found in history", through)
		}
		if throughIdx <= boundaryIdx {
			return nil, fmt.Errorf("migration %q is not above the boundary %q", through, to)
		}
		segment = history[boundaryIdx+1 : throughIdx+1]
	}
	if len(segment) == 0 {
		return nil, nil // already at the boundary
	}

	// Parse the forward migrations and refuse the shapes inversion cannot
	// honestly cover.
	forward := make([]*migrations.Migration, 0, len(segment))
	targets := make([]string, 0, len(segment))
	for _, h := range segment {
		raw := h.Migration
		mig, err := migrations.ParseMigration(&raw)
		if err != nil {
			return nil, fmt.Errorf("unable to parse migration %q: %w", raw.Name, err)
		}
		if mig.Irreversible {
			return nil, fmt.Errorf("migration %q is marked irreversible and cannot be inverted", mig.Name)
		}
		if len(mig.Operations) == 0 {
			return nil, fmt.Errorf("migration %q has no operations (inferred or stamped DDL?) and cannot be inverted", mig.Name)
		}
		forward = append(forward, mig)
		targets = append(targets, mig.Name)
	}

	// The boundary snapshot anchors all inverse synthesis; it must be a
	// clean post-contraction state, not a mid-flight capture.
	boundarySchema, err := m.state.SchemaAfterMigration(ctx, m.schema, to)
	if err != nil {
		return nil, fmt.Errorf("unable to read boundary snapshot for %q: %w", to, err)
	}
	rawSnapshot, err := json.Marshal(boundarySchema)
	if err != nil {
		return nil, err
	}
	for _, marker := range pollutionMarkers {
		if strings.Contains(string(rawSnapshot), marker) {
			return nil, fmt.Errorf(
				"migration %q is not a clean train boundary (its snapshot contains in-flight pgroll artifacts); "+
					"choose a deployment-final migration", to,
			)
		}
	}

	inverses, err := migrations.InvertSegment(ctx, forward, boundarySchema)
	if err != nil {
		return nil, err
	}

	boundaryMig, err := m.state.GetMigration(ctx, m.schema, to)
	if err != nil {
		return nil, err
	}

	// Reverse targets to newest-first for display symmetry with RevertPlan.
	for i, j := 0, len(targets)-1; i < j; i, j = i+1, j-1 {
		targets[i], targets[j] = targets[j], targets[i]
	}

	return &SealedRevertPlan{
		Targets:               targets,
		Boundary:              to,
		BoundaryVersionSchema: boundaryMig.VersionSchemaName(),
		Inverses:              inverses,
	}, nil
}

// PendingSealedRevertResume reports whether the database is in one of the
// intermediate states an interrupted sealed revert leaves behind, returning
// a short operator-facing description of what resuming will do, or "" when
// there is nothing to resume. The CLI checks this BEFORE planning a fresh
// revert: PlanRevertSealed refuses exactly these states (open window, active
// migration), so without the check the in-engine resume in RevertSealed
// would be unreachable from the command line.
func (m *Roll) PendingSealedRevertResume(ctx context.Context) (string, error) {
	// A partially-applied inverse train: unsealed rows that are all
	// engine-synthesized RevertOf rows.
	unsealed, err := m.state.UnsealedMigrations(ctx, m.schema)
	if err != nil {
		return "", err
	}
	if len(unsealed) > 0 {
		for _, r := range unsealed {
			if r.Migration.RevertOf == "" {
				return "", nil
			}
		}
		return fmt.Sprintf(
			"an interrupted sealed revert left %d partially-applied inverse migration(s); "+
				"they will be rolled back losslessly and the revert re-run from the start",
			len(unsealed),
		), nil
	}

	// A fully-applied inverse train that was never pruned: the history leaf
	// is a (sealed) inverse row.
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil || latest == nil {
		return "", err
	}
	leaf, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return "", err
	}
	if leaf.RevertOf != "" {
		return fmt.Sprintf(
			"a previous sealed revert applied its inverse migrations (leaf %q) but was "+
				"interrupted before pruning history; the prune will be completed", leaf.Name,
		), nil
	}
	return "", nil
}

// RevertSealed executes a sealed-history revert: the synthesized inverse
// train runs forward through the normal expand/contract engine (so the
// revert is itself zero-downtime), is completed and drained, and then both
// the forward migrations and their inverses are pruned from history,
// leaving the boundary as the leaf — exactly as if the segment had never
// been applied. Schema-exact; data best-effort.
//
// Under WithExpandOnly the run stops after the final inverse's Start: the
// restored boundary projection exists alongside the current schema, apps
// repin to it, and a subsequent `pgroll complete` contracts the inverses and
// finishes the prune (see FinishPendingSealedRevert).
//
// Crash recovery:
//   - interrupted while the inverse train was applying: the partial inverse
//     rows are unsealed (still in their expand phase), so they are walked
//     back out with the standard window revert and the run restarts cleanly;
//   - interrupted after the final inverse completed but before the prune:
//     the leaf is a sealed inverse row, and the prune is finished.
func (m *Roll) RevertSealed(ctx context.Context, to string, cfg *backfill.Config, opts ...SealedRevertOption) (*SealedRevertPlan, error) {
	var o sealedRevertOptions
	for _, opt := range opts {
		opt(&o)
	}
	// Resume: a partial inverse train from an interrupted run is unsealed
	// (and only ever contains engine-synthesized RevertOf rows) — undo it
	// and start over.
	unsealed, err := m.state.UnsealedMigrations(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	if len(unsealed) > 0 {
		allRevert := true
		for _, r := range unsealed {
			if r.Migration.RevertOf == "" {
				allRevert = false
				break
			}
		}
		if allRevert {
			m.logger.Info("undoing a partially-applied inverse train from an interrupted sealed revert", "count", len(unsealed))
			if _, err := m.Revert(ctx); err != nil {
				return nil, fmt.Errorf("unable to undo the partial inverse train: %w", err)
			}
		}
	}

	// Resume: a fully-completed inverse train that was never pruned.
	if done, err := m.finishSealedRevert(ctx, to); err != nil {
		return nil, err
	} else if done != nil {
		return done, nil
	}

	plan, err := m.PlanRevertSealed(ctx, to)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, nil
	}

	if cfg == nil {
		cfg = backfill.NewConfig()
	}

	// Apply the inverse train the way `pgroll migrate --complete` applies a
	// forward one — except the final Complete is NOT deferred: a revert is
	// not a deployment to keep revertible, it must finish (drain its own
	// deferred queue) so the prune below leaves no queued work behind.
	for i, inv := range plan.Inverses {
		final := i == len(plan.Inverses)-1
		if final {
			inv.VersionSchema = plan.BoundaryVersionSchema
			if err := m.Start(ctx, inv, cfg); err != nil {
				return nil, fmt.Errorf("unable to start inverse migration %q: %w", inv.Name, err)
			}
			if o.expandOnly {
				m.logger.Info("expand-only revert: final inverse left active; repin apps to the "+
					"restored schema, then run `pgroll complete` to contract and finish the revert",
					"inverse", inv.Name,
					"schema", VersionedSchemaName(m.schema, plan.BoundaryVersionSchema))
				return plan, nil
			}
			if err := m.Complete(ctx); err != nil {
				return nil, fmt.Errorf("unable to complete inverse migration %q: %w", inv.Name, err)
			}
			continue
		}
		if err := m.Start(ctx, inv, cfg, WithoutVersionSchema()); err != nil {
			return nil, fmt.Errorf("unable to start inverse migration %q: %w", inv.Name, err)
		}
		completeOpt := WithSkipSchemaDrop()
		if inv.CompleteMustBeDeferred() {
			completeOpt = WithDeferComplete()
		}
		if err := m.Complete(ctx, completeOpt); err != nil {
			return nil, fmt.Errorf("unable to complete inverse migration %q: %w", inv.Name, err)
		}
	}

	if err := m.pruneSealedRevert(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// FinishPendingSealedRevert finishes a sealed revert whose inverse train has
// fully applied but was never pruned — the history leaf is a sealed inverse
// (RevertOf) row. This is the second half of a split (`revert --expand-only`)
// revert, and the resume path for a revert interrupted between its final
// Complete and its prune. The boundary is derived from history: the newest
// row that is neither an inverse nor a target of the inverses above it (or
// the baseline when the whole post-baseline history is being unwound).
// Returns the finished plan, or nil when the leaf is not an inverse row.
func (m *Roll) FinishPendingSealedRevert(ctx context.Context) (*SealedRevertPlan, error) {
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil || latest == nil {
		return nil, err
	}
	leaf, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return nil, err
	}
	if leaf.RevertOf == "" {
		return nil, nil
	}

	history, err := m.state.SchemaHistory(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	reverted := make(map[string]bool)
	boundary := ""
	for i := len(history) - 1; i >= 0; i-- {
		raw := history[i].Migration
		mig, err := migrations.ParseMigration(&raw)
		if err != nil {
			return nil, fmt.Errorf("unable to parse migration %q: %w", raw.Name, err)
		}
		if mig.RevertOf != "" {
			reverted[mig.RevertOf] = true
			continue
		}
		if reverted[mig.Name] {
			continue
		}
		boundary = mig.Name
		break
	}
	if boundary == "" {
		baseline, err := m.state.LatestBaseline(ctx, m.schema)
		if err != nil {
			return nil, err
		}
		if baseline == nil {
			return nil, fmt.Errorf("cannot finish the pending revert: no boundary found beneath the inverse train")
		}
		boundary = baseline.Name
	}
	return m.finishSealedRevert(ctx, boundary)
}

// finishSealedRevert completes an interrupted run whose inverse train fully
// applied but was never pruned: the history leaf is a sealed inverse row.
// Returns the reconstructed plan when it finished such a run, nil when
// there is nothing to resume.
func (m *Roll) finishSealedRevert(ctx context.Context, to string) (*SealedRevertPlan, error) {
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil || latest == nil {
		return nil, err
	}
	leaf, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return nil, err
	}
	if leaf.RevertOf == "" {
		return nil, nil
	}

	history, err := m.state.SchemaHistory(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	plan := &SealedRevertPlan{Boundary: to}
	for i := len(history) - 1; i >= 0; i-- {
		raw := history[i].Migration
		if raw.Name == to {
			break
		}
		mig, err := migrations.ParseMigration(&raw)
		if err != nil {
			return nil, fmt.Errorf("unable to parse migration %q: %w", raw.Name, err)
		}
		if mig.RevertOf != "" {
			plan.Inverses = append(plan.Inverses, mig)
			plan.Targets = append(plan.Targets, mig.RevertOf)
		}
	}
	if len(plan.Inverses) == 0 {
		return nil, nil
	}

	if boundaryMig, err := m.state.GetMigration(ctx, m.schema, to); err == nil {
		plan.BoundaryVersionSchema = boundaryMig.VersionSchemaName()
	}

	m.logger.Info("finishing an interrupted sealed revert", "inverses", len(plan.Inverses))
	if err := m.pruneSealedRevert(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// pruneSealedRevert removes the forward migrations and their inverses from
// history in one transaction, leaving the boundary as the leaf, and ensures
// its version schema exists. Before pruning it records re-application
// tombstones for the forward migrations: pruning makes their unchanged
// files look unapplied again, and without a tombstone the next convergent
// deploy would silently re-run the DDL this revert just undid. Recording
// before the prune is crash-safe — a tombstone for a still-applied
// migration is inert.
func (m *Roll) pruneSealedRevert(ctx context.Context, plan *SealedRevertPlan) error {
	tombstones := make([]state.Tombstone, 0, len(plan.Targets))
	for _, name := range plan.Targets {
		mig, err := m.state.GetMigration(ctx, m.schema, name)
		if err != nil {
			return fmt.Errorf("unable to load reverted migration %q for its tombstone: %w", name, err)
		}
		hash, err := mig.ContentHash()
		if err != nil {
			return err
		}
		tombstones = append(tombstones, state.Tombstone{Name: name, ContentHash: hash})
	}
	if err := m.state.RecordRevertedMigrations(ctx, m.schema, tombstones); err != nil {
		return err
	}

	names := make([]string, 0, 2*len(plan.Inverses))
	names = append(names, plan.Targets...)
	for _, inv := range plan.Inverses {
		names = append(names, inv.Name)
	}
	if err := m.state.Prune(ctx, m.schema, names); err != nil {
		return fmt.Errorf("unable to prune reverted history: %w", err)
	}
	if err := m.ensureLeafVersionSchema(ctx); err != nil {
		return fmt.Errorf("unable to materialize the boundary's version schema: %w", err)
	}
	return nil
}
