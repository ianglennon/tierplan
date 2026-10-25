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

// ReviewModeRestore reviews putting back the deployed revision (D-23): the same review as a roll back, of the
// stack's own deployed revision, so that what it lists is what changed outside the stack.
const ReviewModeRestore = "restore"

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
	// Every text leaves masked: the secrets of the revision and of the one that runs, whichever step added it.
	masker := rollbackMasker(target, in.Previous)
	defer func() {
		if err == nil {
			stacks.MaskProblems(rv.Problems, masker)
			rv.Problems = rv.Problems.Sorted()
		}
	}()

	if in.Stack.Target == stacks.TargetStandalone {
		return d.reviewRollbackStandalone(ctx, in, rv)
	}

	prep, err := d.PrepareRollback(ctx, in.Stack, target, in.TargetPins)
	if err != nil {
		return notDeployable(rv, target.Seq, err)
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
	if own := OwnershipOf(in.Stack); clash(own, live.Networks, live.Services, live.Objects) {
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
	d.addRevertsDrift(ctx, in, rv, rawSwarmRows(desired, running), masker)
	return nil
}

// notDeployable puts the one blocking problem of a revision that cannot be redeployed in rv and ends the review
// without an error. An error that is not a *PrepareError is the review's own failure and is returned.
func notDeployable(rv *Review, seq int64, err error) error {
	var pe *PrepareError
	if !errors.As(err, &pe) {
		return err
	}
	rv.Problems = append(rv.Problems, stacks.Problem{
		Severity: stacks.SeverityError, Code: CodeRevisionNotDeployable, Message: notDeployableMessage(seq, pe),
		File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
	})
	return nil
}

// reviewRollbackStandalone is reviewRollback for a standalone host (STK-08): the revision is prepared with its
// stored images (PrepareRollbackStandalone), prepared as the deploy prepares a project, and compared with the
// host's containers by config hash and field diff, as reviewStandalone does for a draft. It asks no registry,
// looks no tag up and never writes. A network or volume a later revision added and this one does not declare
// is not listed: a standalone deploy leaves them on the host (D-52).
func (d Deps) reviewRollbackStandalone(ctx context.Context, in ReviewInput, rv *Review) error {
	target := *in.Target
	prep, err := d.PrepareRollbackStandalone(ctx, in.Stack, target, in.TargetPins)
	if err != nil {
		return notDeployable(rv, target.Seq, err)
	}
	project := prep.Loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, prep.Files, prep.Vars)
	if err != nil {
		rv.Problems = append(rv.Problems, objectErrorProblem(err))
		return nil
	}
	node, err := d.StandaloneNode(ctx, in.EnvID)
	if err != nil {
		return err
	}
	live, err := d.readStandalone(ctx, in.EnvID, node, in.Stack.Name)
	if err != nil {
		return err
	}
	if foreignLive(live, OwnershipOf(in.Stack)) {
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeNameClash, Message: NameClashMessage(in.Stack.Name), File: stacks.ComposeFile,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	// The compose rules judge the revision's own file; there is no converted swarm spec on a standalone host.
	pol, err := d.Policy(ctx, in.Caller, in.EnvID, target.Copy, convert.Plan{})
	if err != nil {
		return err
	}
	rv.Problems = append(rv.Problems, pol.Problems...)
	if pol.Kinds != nil {
		rv.NeedsAdmin = pol.Kinds
	}

	restored, err := RestoredStandalonePins(project, in.TargetPins)
	if err != nil {
		return err // PrepareRollbackStandalone checked these: the project is the same, so this is not expected
	}
	items := standaloneItems(project)
	pinned := PinOutcome{Pins: restored}
	rv.Pins = pinned.Refs()
	rv.Images = rollbackStandaloneImageRows(items, restored, standaloneCurrent(live.Containers))
	rv.Summary.ImagesPinned = len(rv.Images)
	// The rendering is made before the project is prepared: preparing adds the labels and the pinned images
	// a deploy sends, which are not part of what the revision stored.
	rv.YAML = d.rollbackYAMLChange(in, prep, restored)

	if err := stacks.PrepareStandalone(project, in.Stack.ID, standalonePins(items, pinned), objs); err != nil {
		rv.Problems = append(rv.Problems, prepareProblem(err))
		return nil
	}
	diff, err := diffStandalone(project, live.Containers)
	if err != nil {
		return err
	}
	// The comparison hides the secrets of the revision to restore and of the one that runs.
	cin := in
	cin.Copy = target.Copy
	cmp, err := d.compareOnNode(ctx, cin, node, project, live.Containers, diff)
	if err != nil {
		return err
	}
	for _, c := range cmp.Changes {
		rv.Services = append(rv.Services, c.Row)
		bump(&rv.Summary.Services, c.Row.Change)
	}
	// What a recreate carries with it follows the changed services; it is not counted as changed.
	rv.Services = append(rv.Services, cmp.Carried...)
	for _, name := range diff.Removed {
		rv.Services = append(rv.Services, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeRemoved})
		rv.Summary.Services.Removed++
	}
	for _, name := range missingNetworks(project, live.Networks) {
		rv.Objects = append(rv.Objects, ObjectChange{Change: stacks.ChangeAdded, Kind: "network", Name: name})
		rv.Summary.Networks.Added++
	}
	for _, name := range missingVolumes(project, live.Volumes) {
		rv.Objects = append(rv.Objects, ObjectChange{Change: stacks.ChangeAdded, Kind: "volume", Name: name})
		rv.Summary.Volumes.Added++
	}
	d.addRevertsDrift(ctx, in, rv, rawStandaloneRows(cmp, diff.Removed, standalonePins(items, pinned)), rollbackMasker(target, in.Previous))
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
