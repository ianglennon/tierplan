// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// What a revert does to a service or to one of its settings (D-25).
const (
	// RevertChanged is a setting that runs other than the deployed revision has it and that the deploy sets back to
	// the revision's value.
	RevertChanged = "changed"
	// RevertCreated is a service of the deployed revision that no longer runs and that the deploy creates again.
	RevertCreated = "created"
	// RevertRemoved is a service that runs under the stack and that neither the deployed revision nor the deploy
	// declares: the deploy removes it.
	RevertRemoved = "removed"
)

// DriftRevert is one outside change a deploy would put back (D-25). Service is the service's name in the compose
// file. A changed setting has its Path, the value that runs now (Live) and the value the deploy sets (Desired), both
// as plain text and empty for a setting that one side does not have (an environment variable added by hand has no
// Desired: the deploy removes it). A created or removed service has neither. Cause is why the service differs when
// the audit log or the adoption knows, nil when it does not (D-24).
//
// Live and Desired are the values as the engine holds them until the review masks them.
type DriftRevert struct {
	Service, Change, Path string
	Live, Desired         string
	Cause                 *DriftCause
}

// RevertsDrift is the three-way set of D-25 (RESEARCH Pattern 6): with D the services of the deployed revision, W
// those the deploy (the draft, or the revision rolled back to) would send and L those that run, each as the JSON of
// its settings with the ownership labels and the noise the engine adds taken out and keyed by the service's name, a
// setting reverts drift exactly when D[p] differs from L[p] and W[p] is D[p]. A setting the deploy itself changes
// (W[p] is neither of them, or it is L[p]) is the deploy's own change and is not listed. A service D has that no longer
// runs (L lacks it) and that W keeps is created again; a service that runs (L has it) and that neither D nor W declares
// is removed. The rows come in the order of the services' names, then of their settings' paths.
//
// A side that is a service present with nothing to compare (a service that runs and has no spec to show) is the JSON
// {} or null; a service absent from a map is not there.
func RevertsDrift(deployed, desired, live map[string]json.RawMessage) []DriftRevert {
	names := map[string]struct{}{}
	for name := range deployed {
		names[name] = struct{}{}
	}
	for name := range live {
		names[name] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	var out []DriftRevert
	for _, name := range sorted {
		d, inD := deployed[name]
		l, inL := live[name]
		w, inW := desired[name]
		switch {
		case inD && inL && inW:
			out = append(out, settingReverts(name, d, w, l)...)
		case inD && !inL && inW:
			out = append(out, DriftRevert{Service: name, Change: RevertCreated})
		case !inD && inL && !inW:
			out = append(out, DriftRevert{Service: name, Change: RevertRemoved})
		}
	}
	return out
}

// settingReverts are the settings of one service that the deployed revision and the deploy have alike and that
// run otherwise.
func settingReverts(name string, deployed, desired, live json.RawMessage) []DriftRevert {
	drifted := stacks.ChangedPaths(live, deployed)
	if len(drifted) == 0 {
		return nil
	}
	ours := map[string]stacks.PathChange{}
	for _, p := range stacks.ChangedPaths(live, desired) {
		ours[p.Path] = p
	}
	var out []DriftRevert
	for _, p := range drifted {
		// The deploy leaves the setting as it runs (it took the hot-fix into the draft) or sets it to something else
		// again (its own change): neither puts the deployed revision's value back.
		if q, ok := ours[p.Path]; !ok || q.To != p.To {
			continue
		}
		out = append(out, DriftRevert{Service: name, Change: RevertChanged, Path: p.Path, Live: plainText(p.From), Desired: plainText(p.To)})
	}
	return out
}

// plainText is the text of a value given as JSON: a string without its quotes, anything else as it is written.
func plainText(jsonText string) string {
	if !strings.HasPrefix(jsonText, `"`) {
		return jsonText
	}
	var s string
	if err := json.Unmarshal([]byte(jsonText), &s); err != nil {
		return jsonText
	}
	return s
}

// noSpec is the JSON of a service that runs and has no settings to compare.
var noSpec = json.RawMessage(`{}`)

// revertsFromRows is RevertsDrift over the rows of two comparisons that both read what runs: drift is what runs
// against the deployed revision (Diff.Current is what runs, Diff.Next what the revision deployed), draft is what runs
// against what the deploy would send (Current what runs, Next what the deploy sends). Both carry unmasked settings.
// A service that neither lists is the same in all three. The cause of a drifted service is carried to its rows: a
// scale only to the row of the replica count, since it explains no other setting.
func revertsFromRows(drift []DriftService, draft []stacks.ServiceChange) []DriftRevert {
	deployed, desired, live := map[string]json.RawMessage{}, map[string]json.RawMessage{}, map[string]json.RawMessage{}
	causes := map[string]*DriftCause{}
	key := func(short, name string) string {
		if short != "" {
			return short
		}
		return name
	}
	for _, s := range drift {
		k := key(s.Short, s.Name)
		causes[k] = s.Cause
		switch s.Change {
		case DriftChanged:
			deployed[k], live[k], desired[k] = specOf(s.Diff.Next), specOf(s.Diff.Current), specOf(s.Diff.Current)
		case DriftMissing:
			deployed[k] = specOf(s.Diff.Next)
		case DriftExtra:
			// The deploy keeps a service it declares; the draft's own row says it does not.
			live[k], desired[k] = noSpec, noSpec
		}
	}
	for _, s := range draft {
		k := key(s.Short, s.Name)
		switch s.Change {
		case stacks.ChangeAdded:
			desired[k] = specOf(s.Next)
		case stacks.ChangeChanged:
			desired[k] = specOf(s.Next)
			if _, listed := causes[k]; !listed {
				// The deployed revision and what runs are the same here, so there is nothing to revert.
				live[k], deployed[k] = specOf(s.Current), specOf(s.Current)
			}
		case stacks.ChangeRemoved:
			delete(desired, k)
			if _, listed := causes[k]; !listed {
				live[k], deployed[k] = noSpec, noSpec
			}
		}
	}
	rows := RevertsDrift(deployed, desired, live)
	for i := range rows {
		rows[i].Cause = causeOfRevert(causes[rows[i].Service], rows[i])
	}
	return rows
}

// specOf is the JSON of a row's side, {} for none.
func specOf(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return noSpec
	}
	return raw
}

// causeOfRevert is the cause to name under a row: a scale explains the replica count and nothing else, any other
// action of Bayplan's the whole service. A created or removed service has none to name.
func causeOfRevert(c *DriftCause, r DriftRevert) *DriftCause {
	if c == nil || r.Change != RevertChanged {
		return nil
	}
	if c.Action == CauseScaled && !strings.HasPrefix(r.Path, replicasPath) {
		return nil
	}
	if c.Action == CauseAdopted {
		return nil
	}
	cause := *c
	return &cause
}

// maskReverts hides what the masker hides in the values of the rows.
func maskReverts(rows []DriftRevert, m stacks.Masker) {
	for i := range rows {
		rows[i].Live, rows[i].Desired = m.Mask(rows[i].Live), m.Mask(rows[i].Desired)
	}
}

// rawSwarmRows are the rows of what a deploy would do to the swarm services, unmasked and with Bayplan's ownership
// labels taken out on both sides, as drift compares them (so that the two are comparable): Current is what runs and
// Next what the deploy sends.
func rawSwarmRows(desired, running map[string]swarm.ServiceSpec) []stacks.ServiceChange {
	strip := func(in map[string]swarm.ServiceSpec) map[string]swarm.ServiceSpec {
		out := make(map[string]swarm.ServiceSpec, len(in))
		for name, spec := range in {
			out[name] = stacks.StripOwnership(spec)
		}
		return out
	}
	return stacks.DiffServices(strip(desired), strip(running), stacks.Masker{})
}

// rawStandaloneRows are the rows of what a deploy would do to the containers of a standalone host, unmasked: the
// compared services (with the image a container runs and the one the deploy pins, which the comparison leaves out,
// as drift compares them) and the services the deploy removes.
func rawStandaloneRows(cmp standaloneComparison, removed []string, pins map[string]string) []stacks.ServiceChange {
	rows := map[string]stacks.ServiceChange{}
	for _, c := range cmp.Changes {
		rows[c.Name] = c.Raw
	}
	for name, run := range imageDrift(cmp.Images, pins) {
		row, ok := rows[name]
		if !ok {
			row = stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeChanged}
		}
		row.Current, row.Next = withImage(row.Current, run.have), withImage(row.Next, run.want)
		rows[name] = row
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]stacks.ServiceChange, 0, len(names)+len(removed))
	for _, name := range names {
		out = append(out, rows[name])
	}
	for _, name := range removed {
		out = append(out, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeRemoved})
	}
	return out
}

// addRevertsDrift sets the Reverts drift part of a review (D-25): the outside changes this deploy puts back. draft
// is the deploy's side as rows (rawSwarmRows, rawStandaloneRows) and masker hides what the review hides. It reads
// what drift reads, once more, and never blocks or fails the review: when drift cannot be computed it says so
// (RevertsDriftUnavailable) and the rest of the review stays (T-08-88). A first deploy has no deployed revision and a
// restore is the drift itself, so neither has a section.
func (d Deps) addRevertsDrift(ctx context.Context, in ReviewInput, rv *Review, draft []stacks.ServiceChange, masker stacks.Masker) {
	if in.Deployed == nil || in.Previous == nil || in.Mode == ReviewModeRestore || in.Mode == ReviewModeAdopt {
		return
	}
	unavailable := func(err error) {
		if err != nil {
			d.log().Warn("could not check what a deploy would revert", "environment", in.EnvID, "stack", in.Stack.ID, "error", err)
		}
		rv.RevertsDriftUnavailable = true
	}
	if in.Pins == nil {
		unavailable(nil)
		return
	}
	stored, err := in.Pins.GoodPins(ctx, in.Stack.ID)
	if err != nil {
		unavailable(err)
		return
	}
	pins := make(map[string]string, len(stored))
	for name, p := range stored {
		pins[name] = p.Image
	}
	deployed := Snapshot{Seq: in.Deployed.Seq, CreatedAt: in.Deployed.At, Copy: *in.Previous}
	res, err := d.Drift(ctx, DriftInput{EnvID: in.EnvID, Stack: in.Stack, Deployed: &deployed, Pins: pins, Audit: in.Audit, Raw: true})
	if err != nil {
		unavailable(err)
		return
	}
	switch res.State {
	case DriftUnknown:
		unavailable(nil)
		return
	case DriftDrifted:
	default:
		return
	}
	rows := revertsFromRows(res.Services, draft)
	maskReverts(rows, masker)
	rv.RevertsDrift = rows
	rv.Summary.RevertsDrift = len(rows)
}
