// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// ReviewModeRollback reviews a roll back to a revision: the revision's own converted stack, with the digests it
// stored, against what is running (D-02).
const ReviewModeRollback = "rollback"

// CodeRevisionNotDeployable is the code of the blocking problem for a revision that cannot be redeployed as it
// stands: it no longer loads, converts or has an image stored for each service.
const CodeRevisionNotDeployable = "revision_not_deployable"

// noteRevision is the note of an image row of a roll back: the digest is the revision's own.
const noteRevision = "revision"

// reviewRollback reads what rolling back to in.Target would do. The revision is prepared as the roll back
// prepares it (PrepareRollback), converted against what the swarm holds the way a deploy review converts, and
// each service is compared with the one running with the image the revision stored. It asks no registry, looks
// no tag up and runs no lint: a roll back restores exactly what the revision ran (STK-07, T-08-12). It never
// writes: not to an engine, and not to the store.
//
// A revision that cannot be prepared is one blocking problem (revision_not_deployable) and nothing else. The
// rest follows Review: the name clash, the host-access policy judged on the revision's own copy (T-08-09), the
// network, secret and config changes, and the service diff.
func (d Deps) reviewRollback(ctx context.Context, in ReviewInput, rv *Review) (err error) {
	if in.Target == nil {
		return errors.New("stackops: a roll back review needs the revision to restore")
	}
	target := *in.Target
	if in.Stack.Target == stacks.TargetStandalone {
		// Rolling a standalone stack back is a later plan's: its engine is not here yet.
		return ErrTargetEngineMissing
	}
	// Every text leaves masked: the secrets of the revision and of the one that runs, whichever step added it.
	masker := rollbackMasker(target, in.Previous)
	defer func() {
		if err == nil {
			stacks.MaskProblems(rv.Problems, masker)
			rv.Problems = rv.Problems.Sorted()
		}
	}()

	prep, err := d.PrepareRollback(ctx, in.Stack, target, in.TargetPins)
	if err != nil {
		var pe *PrepareError
		if !errors.As(err, &pe) {
			return err
		}
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeRevisionNotDeployable, Message: notDeployableMessage(target.Seq, pe),
			File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
		return nil
	}
	project := prep.Loaded.Project
	objs, err := stacks.ResolveObjects(project, prep.Files, prep.Vars)
	if err != nil {
		rv.Problems = append(rv.Problems, objectErrorProblem(err))
		return nil
	}
	live, err := d.readLive(ctx, in.EnvID, in.Stack.Name, liveWant{
		allNetworks: true,
		allSecrets:  hasExternalObject(objs, stacks.KindSecret),
		allConfigs:  hasExternalObject(objs, stacks.KindConfig),
	})
	if err != nil {
		return err
	}
	if clash(in.Stack.ID, in.Stack.Name, live.Networks, live.Services, live.Objects) {
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeNameClash, Message: NameClashMessage(in.Stack.Name), File: stacks.ComposeFile,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	// PrepareRollback converts before anything exists, with names and no IDs. The comparison converts again with
	// the ID of each object the swarm already holds, so a service that does not change converts to the spec the
	// swarm holds (as the deploy review does).
	plan, err := convert.Convert(project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: reviewRefs(in.Stack, objs, live.Objects)})
	if err != nil {
		rv.Problems = append(rv.Problems, ConvertProblem(convert.GuardSecrets(err, stacks.NewSecretUse(target.Copy))))
		return nil
	}
	pol, err := d.Policy(ctx, in.Caller, in.EnvID, target.Copy, plan)
	if err != nil {
		return err
	}
	rv.Problems = append(rv.Problems, pol.Problems...)
	if pol.Kinds != nil {
		rv.NeedsAdmin = pol.Kinds
	}

	restored, err := RestoredPins(plan.Services, in.TargetPins)
	if err != nil {
		return err // PrepareRollback checked these: the plan is the same, so this is not expected
	}
	byName := make(map[string]dockerx.ServiceDetail, len(live.Services))
	for _, s := range live.Services {
		byName[s.Name] = s
	}
	rv.Pins = make(map[string]string, len(restored))
	for name, p := range restored {
		rv.Pins[name] = p.Ref
	}
	rv.Images = rollbackImageRows(plan.Services, restored, byName)
	rv.Summary.ImagesPinned = len(rv.Images)

	desired := make(map[string]swarm.ServiceSpec, len(plan.Services))
	for _, sp := range plan.Services {
		spec := sp.Spec
		if p, ok := restored[sp.Name]; ok && spec.TaskTemplate.ContainerSpec != nil {
			cs := *spec.TaskTemplate.ContainerSpec
			cs.Image = p.Ref
			spec.TaskTemplate.ContainerSpec = &cs
		}
		if existing, ok := byName[sp.Name]; ok {
			// The counter that makes the swarm replace tasks is carried over, as a deploy does.
			spec.TaskTemplate.ForceUpdate = liveForceUpdate(existing.Spec)
		}
		desired[sp.Name] = spec
	}
	running, err := liveSpecs(live, in.Stack)
	if err != nil {
		return err
	}
	rv.Services = stacks.DiffServices(desired, running, masker)
	for _, c := range rv.Services {
		bump(&rv.Summary.Services, c.Change)
	}

	var problems stacks.Problems
	rv.Objects, problems = resourceChanges(in.Stack, plan, objs, live)
	rv.Problems = append(rv.Problems, placeProblems(prep.Files, problems)...)
	for _, o := range rv.Objects {
		switch o.Kind {
		case "network":
			bump(&rv.Summary.Networks, o.Change)
		case string(stacks.KindSecret):
			bump(&rv.Summary.Secrets, o.Change)
		case string(stacks.KindConfig):
			bump(&rv.Summary.Configs, o.Change)
		}
	}

	rv.YAML = d.rollbackYAMLChange(in, prep, restored)
	return nil
}

// rollbackMasker hides the secrets of the revision to restore and, when there is one, of the revision that runs.
func rollbackMasker(target Snapshot, previous *stacks.WorkingCopy) stacks.Masker {
	vars := append([]stacks.Var(nil), target.Copy.Vars...)
	files := append([]stacks.File(nil), target.Copy.Files...)
	if previous != nil {
		vars, files = append(vars, previous.Vars...), append(files, previous.Files...)
	}
	return stacks.NewMasker(vars, files)
}

// notDeployableMessage is the sentence for a revision that cannot be redeployed. It names the revision and what
// stopped it; the cause itself is not quoted, for the converter quotes the values it refuses and one may be a
// secret variable's (the caller masks the sentence as well).
func notDeployableMessage(seq int64, pe *PrepareError) string {
	reason := "it can't be read or checked any more"
	switch pe.Stage {
	case PrepareObjects:
		reason = ObjectErrorDetail(pe.Err)
	case PrepareConvert:
		reason = ConvertDetail(pe.Err) // already guarded by PrepareRollback
	case PreparePins:
		var invalid *PinsInvalidError
		if errors.As(pe.Err, &invalid) {
			reason = "it has no recorded image for " + invalid.Service
		}
	}
	return "Bayplan can't restore revision #" + strconv.FormatInt(seq, 10) + ": " + reason + ". Nothing was changed."
}

// rollbackImageRows is one row for each service with an image, ordered by the service's name in the compose
// file. Each is Pinned with the revision's own digest, the note says so, and no row is New or needs a lookup.
func rollbackImageRows(services []convert.ServicePlan, restored map[string]Pin, live map[string]dockerx.ServiceDetail) []ImageRow {
	var rows []ImageRow
	for _, sp := range services {
		p, ok := restored[sp.Name]
		if !ok {
			continue // a service with no image
		}
		row := ImageRow{Service: sp.Short, Image: sp.ImageAsWritten, Digest: digestOf(p.Ref), State: ImagePinned, Note: noteRevision}
		if det, ok := live[sp.Name]; ok {
			row.Current = digestOf(det.Image)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Service < rows[j].Service })
	return rows
}

// rollbackYAMLChange is the revision's rendering against the rendering stored with the revision that runs. The
// revision's own stored rendering is used when it has one; an older revision with none is rendered now with the
// digests it stored.
func (d Deps) rollbackYAMLChange(in ReviewInput, prep RollbackPlan, restored map[string]Pin) TextDiff {
	next := in.TargetRendering
	if len(next) == 0 {
		pins := make(map[string]stacks.LivePin, len(restored))
		for name, p := range restored {
			pins[name] = stacks.LivePin{AsWritten: p.AsWritten, Image: p.Ref}
		}
		out, err := stacks.Render(stacks.RenderInput{
			Loaded: prep.Loaded, Pins: pins, Masker: stacks.NewMasker(in.Target.Copy.Vars, in.Target.Copy.Files),
		})
		if err != nil {
			d.log().Warn("could not render a revision for its roll back review", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
			return TextDiff{}
		}
		next = out
	}
	return RevisionYAMLDiff(in.Rendering, next)
}
