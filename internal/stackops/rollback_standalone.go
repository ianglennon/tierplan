// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"sort"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
)

// PrepareRollbackStandalone checks, loads and resolves the revision of a standalone stack to restore, and checks that
// it knows an image for every service. It is PrepareRollback for a host that runs the Compose SDK: nothing is
// converted to swarm services (the Plan of the result is empty) and nothing is sent to an engine or a registry.
// A roll back redeploys exactly what the revision ran, so the references it stored are the ones that go out
// (STK-08, T-08-18). Nothing is stored, so a revision that cannot be redeployed is refused before the stack gains
// a revision of its own.
//
// Pins are keyed by the service's name in the compose file, as a standalone deploy stores them.
func (d Deps) PrepareRollbackStandalone(ctx context.Context, st Stack, target Snapshot, pins map[string]string) (RollbackPlan, error) {
	// The masker is built only for a refusal: the revision is judged on every roll back.
	refuse := func(stage string, err error) error {
		return &PrepareError{Stage: stage, Err: err, Masker: stacks.NewMasker(target.Copy.Vars, target.Copy.Files)}
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: stacks.TargetStandalone, Copy: target.Copy})
	if err != nil {
		return RollbackPlan{}, refuse(PrepareLoad, err)
	}
	files, vars := stacks.NewFileSet(target.Copy.Files), stacks.ObjectVars(target.Copy)
	if _, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, loaded.Project, files, vars); err != nil {
		return RollbackPlan{}, refuse(PrepareObjects, err)
	}
	if _, err := RestoredStandalonePins(loaded.Project, pins); err != nil {
		return RollbackPlan{}, refuse(PreparePins, err)
	}
	return RollbackPlan{Loaded: loaded, Files: files, Vars: vars, Plan: convert.Plan{}, Pins: pins}, nil
}

// RestoredStandalonePins are the images a standalone roll back sends: for each service of the project with an
// image, the reference the revision stored for it, unchanged and keyed by the service's name in the compose
// file. A service the revision stored nothing for is a *PinsInvalidError (reason missing): a roll back never
// looks an image up, so it has nothing to send for it. The pin of a reviewed deploy is not used because it
// refuses a digest that is not the one a lookup would choose now, and the point of a roll back is an older one.
func RestoredStandalonePins(p *types.Project, stored map[string]string) (map[string]Pin, error) {
	items := standaloneItems(p)
	out := make(map[string]Pin, len(items))
	for _, it := range items {
		ref := stored[it.Name]
		if ref == "" {
			return nil, &PinsInvalidError{Service: it.Name, Reason: "missing"}
		}
		out[it.Name] = Pin{Service: it.Name, AsWritten: it.AsWritten, Ref: ref, Action: stacks.PinKeep}
	}
	return out, nil
}

// rollbackStandaloneImageRows is one row for each service with an image, ordered by the service's name. Each is
// Pinned with the revision's own reference, the note says so, and no row is New or needs a lookup. current is the
// image each service's container runs now.
func rollbackStandaloneImageRows(items []pinItem, restored map[string]Pin, current map[string]string) []ImageRow {
	rows := make([]ImageRow, 0, len(items))
	for _, it := range items {
		p, ok := restored[it.Name]
		if !ok {
			continue
		}
		row := ImageRow{Service: it.Short, Image: it.AsWritten, Digest: digestOf(p.Ref), State: ImagePinned, Note: noteRevision}
		if ref, ok := current[it.Name]; ok {
			row.Current = digestOf(ref)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Service < rows[j].Service })
	return rows
}
