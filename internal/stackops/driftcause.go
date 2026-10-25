// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
)

// What a drift cause says was done (D-24).
const (
	// CauseScaled is a scale from a service page (service.scaled).
	CauseScaled = "scaled"
	// CauseRolledBack is a service roll back (service.rolled_back).
	CauseRolledBack = "rolled_back"
	// CauseEdited is an edit of a service (service.updated).
	CauseEdited = "edited"
	// CauseAdopted is a difference that was already there when the stack was adopted and that its first deploy has not
	// put right yet (D-55).
	CauseAdopted = "adopted"
)

// DriftCause is what the audit log knows about why a service differs from the deployed revision: Bayplan itself changed
// its spec (a scale, a roll back, an edit), or the difference was there when the stack was adopted. Only the action,
// the actor's display name, the time and, for a scale, the replica counts are kept: no audit detail leaves with it
// (T-08-78). A restart and a force update do not change the spec, so they are neither drift nor a cause.
type DriftCause struct {
	Action string
	// Service is the service's name in the compose file; empty for a cause kept by service.
	Service string
	// By is the display name of the actor, At the time of the audit row. Both are empty for an adoption.
	By string
	At time.Time
	// From and To are the replica counts of a scale, as text. From is empty when the row did not say.
	From, To string
}

// driftCauseActions are the audit actions that change the spec of a service, so they can be why one differs from the
// deployed revision. service.force_updated is left out on purpose: a counter that forces new tasks is not drift.
var driftCauseActions = []string{audit.ActionServiceScaled, audit.ActionServiceRolledBack, audit.ActionServiceUpdated}

const (
	// driftCausePageSize and driftCauseMaxPages bound the audit rows one drift check reads: a service changed more
	// often than that since its deploy still gets its newest cause from the pages read, or none (T-08-79).
	driftCausePageSize = 100
	driftCauseMaxPages = 5
	// scaleUnchanged is the code the scale route puts on a row that asked for the replicas the service had.
	scaleUnchanged = "unchanged"
)

// DriftCauses reads, for each service in services (service ID to name), the newest spec-changing action of Bayplan's
// own on it since the given time, from the audit log and nowhere else (D-24). Only successful rows of the environment
// count, matched to the service by the row's target. The result is keyed by service ID; a service nothing is known
// about is absent. It pages a bounded number of times and stops as soon as every service has its cause.
func (d Deps) DriftCauses(ctx context.Context, st *store.Store, envID string, since time.Time, services map[string]string) (map[string]DriftCause, error) {
	out := map[string]DriftCause{}
	if st == nil || len(services) == 0 {
		return out, nil
	}
	f := audit.Filter{Actions: driftCauseActions, From: since, Scoped: true, Envs: []string{envID}}
	var cursor int64
	for range driftCauseMaxPages {
		rows, next, err := audit.Query(ctx, st, f, cursor, driftCausePageSize)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			name, ok := services[r.TargetID]
			if !ok || r.Result != string(audit.ResultSuccess) || r.EnvID != envID {
				continue
			}
			if _, have := out[r.TargetID]; have {
				continue
			}
			c, ok := causeOfRow(r, name)
			if !ok {
				continue
			}
			out[r.TargetID] = c
		}
		if next == 0 || len(out) == len(services) {
			break
		}
		cursor = next
	}
	return out, nil
}

// causeOfRow is the cause an audit row stands for, and false for a row that changed nothing.
func causeOfRow(r audit.Row, service string) (DriftCause, bool) {
	c := DriftCause{Service: service, By: r.ActorName, At: r.Time}
	switch r.Action {
	case audit.ActionServiceScaled:
		if r.Details["code"] == scaleUnchanged {
			return DriftCause{}, false
		}
		c.Action, c.From, c.To = CauseScaled, r.Details["previous"], r.Details["replicas"]
	case audit.ActionServiceRolledBack:
		c.Action = CauseRolledBack
	case audit.ActionServiceUpdated:
		c.Action = CauseEdited
	default:
		return DriftCause{}, false
	}
	return c, true
}

// replicasPath is where a change of the replica count shows in a service's spec.
const replicasPath = "Mode.Replicated.Replicas"

// changePaths are the paths of the settings of a changed service that differ, with the ones that differ in a value the
// masker hides: the same paths the adoption recorded as its gaps (adopt.go gapPaths), and no value.
func changePaths(c stacks.ServiceChange) []string {
	var paths []string
	for _, p := range stacks.ChangedPaths(c.Current, c.Next) {
		paths = append(paths, p.Path)
	}
	paths = append(paths, c.HiddenChanges...)
	slices.Sort(paths)
	return slices.Compact(paths)
}

// attachCauses names why each drifted service differs, and sums them up on the result. ids maps a swarm service name
// to its ID (nil for a standalone host, which has no service actions). A failure to read the audit log leaves the
// causes out: a drift check is not worth failing for them.
func (d Deps) attachCauses(ctx context.Context, in DriftInput, res *DriftResult, ids map[string]string) {
	if res.State != DriftDrifted {
		return
	}
	// Only a changed service can have been scaled, rolled back or edited: a missing one is not there, an extra one
	// was made by somebody.
	wanted := map[string]string{}
	for _, s := range res.Services {
		if id, ok := ids[s.Name]; ok && s.Change == DriftChanged {
			wanted[id] = s.Name
		}
	}
	var found map[string]DriftCause
	if len(wanted) > 0 && in.Audit != nil {
		var since time.Time
		if in.Deployed != nil {
			since = in.Deployed.CreatedAt
		}
		var err error
		if found, err = d.DriftCauses(ctx, in.Audit, in.EnvID, since, wanted); err != nil {
			d.log().Warn("the drift check could not read the audit log for causes", "environment", in.EnvID, "stack", in.Stack.ID, "error", err)
			found = nil
		}
	}

	adopted := in.Stack.Adoption.Pending
	var bayplan []DriftCause
	var firstAdopted *DriftCause
	for i := range res.Services {
		s := &res.Services[i]
		short := s.Short
		if short == "" {
			short = s.Name
		}
		if c, ok := found[ids[s.Name]]; ok && explains(c, *s) {
			c.Service = short
			s.Cause = &c
			bayplan = append(bayplan, c)
			continue
		}
		if adopted && inGaps(in.Stack.Adoption.Gaps, *s) {
			s.Cause = &DriftCause{Action: CauseAdopted, Service: short}
			if firstAdopted == nil {
				firstAdopted = s.Cause
			}
		}
	}
	res.Causes = len(bayplan)
	switch {
	case len(bayplan) > 0:
		latest := bayplan[0]
		for _, c := range bayplan[1:] {
			if c.At.After(latest.At) {
				latest = c
			}
		}
		res.Latest = &latest
	case firstAdopted != nil:
		c := *firstAdopted
		res.Latest = &c
	}
}

// explains says whether an action of Bayplan's can be why the service differs: a scale only when the replica count
// is one of the settings that differ, so a service whose image was changed from outside after a scale is not blamed
// on the scale.
func explains(c DriftCause, s DriftService) bool {
	if c.Action != CauseScaled {
		return true
	}
	paths := changePaths(s.Diff)
	if len(paths) == 0 {
		return true
	}
	return slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(p, replicasPath) })
}

// inGaps says whether a drifted service differs from the rebuilt definition in a place the adoption recorded: a
// changed service when one of the paths that differ was recorded, a missing or extra one when the service was.
func inGaps(gaps map[string][]string, s DriftService) bool {
	paths, ok := gaps[s.Name]
	if !ok {
		paths, ok = gaps[s.Short]
	}
	if !ok {
		return false
	}
	if s.Change != DriftChanged {
		return true
	}
	return slices.ContainsFunc(changePaths(s.Diff), func(p string) bool { return slices.Contains(paths, p) })
}
