// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"errors"
	"slices"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
)

// ReviewModeAdopt reviews the definition rebuilt from what runs against what runs, before the stack is adopted
// (plan 08-14, D-16, D-20). It answers what the first deploy of the adopted stack would change. A difference that is
// only TierPlan's ownership labels is the change the first deploy makes by itself, so it is marked
// stacks.ServiceChange.OwnershipOnly and counted apart in Summary.OwnershipOnly; every other difference is a gap.
//
// The definition reviewed is ReviewInput.Copy: the rebuild's file, or (plan 08-15, D-12) a pasted file that has already
// passed stacks.Check. Rebuilt always carries what runs: its Pins are the image each service is converted with (the
// digest it runs, or for a pasted file the image as written when that is not what runs).
const ReviewModeAdopt = "adopt"

// AdoptCheckID is the stack ID the adopt check converts with. The stack has none yet; the placeholder is on the
// converted side only, and as a label it is the ownership label a first deploy would put on, so it is the one
// difference an unlabelled service always has.
const AdoptCheckID = "adopt-check"

// reviewAdopt fills rv with what a first deploy of the rebuilt definition in in.Rebuilt would do to the stack's
// services. It never writes, asks no registry (the images are the ones that run, D-14) and judges no policy (D-17).
func (d Deps) reviewAdopt(ctx context.Context, in ReviewInput, rv *Review) error {
	_, err := d.reviewAdoptPlan(ctx, in, rv)
	return err
}

// reviewAdoptPlan is reviewAdopt that also answers the swarm plan the review converted, which the host-access
// settings of the stack are read from. A standalone stack has none: the empty plan.
func (d Deps) reviewAdoptPlan(ctx context.Context, in ReviewInput, rv *Review) (convert.Plan, error) {
	if in.Rebuilt == nil {
		return convert.Plan{}, errors.New("stackops: an adopt review needs the rebuilt definition")
	}
	loaded, err := loadRebuilt(ctx, in.Stack.Name, in.Stack.Target, in.Copy)
	if err != nil {
		return convert.Plan{}, err
	}
	rv.Pins = make(map[string]string, len(in.Rebuilt.Pins))
	for name, ref := range in.Rebuilt.Pins {
		rv.Pins[name] = ref
	}
	if in.Stack.Target == stacks.TargetStandalone {
		return convert.Plan{}, d.reviewAdoptStandalone(ctx, in, loaded, rv)
	}

	files, vars := stacks.NewFileSet(in.Copy.Files), stacks.ObjectVars(in.Copy)
	objs, err := stacks.ResolveObjects(loaded.Project, files, vars)
	if err != nil {
		return convert.Plan{}, &RebuildError{Reason: ObjectErrorDetail(err)}
	}
	live, err := d.readLive(ctx, in.EnvID, in.Stack.Name, liveWant{
		allNetworks: true,
		allSecrets:  hasExternalObject(objs, stacks.KindSecret),
		allConfigs:  hasExternalObject(objs, stacks.KindConfig),
	})
	if err != nil {
		return convert.Plan{}, err
	}
	// Nothing is created and nothing is cleared: the rebuilt definition is converted against the objects that exist
	// (their IDs, so a service that does not change converts to the spec the swarm holds), with the placeholder ID.
	plan, err := convert.Convert(loaded.Project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: reviewRefs(in.Stack, objs, live.Objects)})
	if err != nil {
		return convert.Plan{}, &RebuildError{Reason: ConvertDetail(convert.GuardSecrets(err, stacks.NewSecretUse(in.Copy)))}
	}

	byName := make(map[string]dockerx.ServiceDetail, len(live.Services))
	for _, s := range live.Services {
		byName[s.Name] = s
	}
	// The pins are the images that run, with the platforms docker stack deploy stored for them: they belong to the
	// image, which a compose file cannot say, so the service keeps them while it runs that image (G-08-4).
	pins := make(map[string]Pin, len(in.Rebuilt.Pins))
	for name, ref := range in.Rebuilt.Pins {
		pins[name] = Pin{Service: name, Ref: ref}
	}
	carryPlatforms(pins, byName)
	desired := make(map[string]swarm.ServiceSpec, len(plan.Services))
	for _, sp := range plan.Services {
		spec := sp.Spec
		if pin, ok := pins[sp.Name]; ok {
			spec = withPin(spec, pin)
		}
		if existing, ok := byName[sp.Name]; ok {
			// The counter that makes the swarm replace tasks is carried over, as a deploy does.
			spec.TaskTemplate.ForceUpdate = liveForceUpdate(existing.Spec)
		}
		desired[sp.Name] = spec
	}
	own := OwnershipOf(in.Stack)
	running, err := liveSpecsOwned(live, func(s dockerx.ServiceDetail) bool { return own.owns(s.Stack, s.Labels) })
	if err != nil {
		return convert.Plan{}, err
	}
	masker := stacks.NewMasker(in.Copy.Vars, in.Copy.Files)
	rv.Services = stacks.DiffServices(desired, running, masker)
	stacks.MarkOwnershipOnly(rv.Services, desired, running)
	countServices(rv)
	return plan, nil
}

// reviewAdoptStandalone is reviewAdoptPlan for a Compose project on a standalone host: the rebuilt project is
// prepared as a deploy prepares it, with the images that run, and compared with the host's containers by config hash
// and field diff (compareStandalone). A service whose containers carry another config hash than the rebuilt service
// has now, with no field that differs, differs in what TierPlan's own labels change in the hash, and nothing the
// person wrote: it is ownership-only.
func (d Deps) reviewAdoptStandalone(ctx context.Context, in ReviewInput, loaded *stacks.Loaded, rv *Review) error {
	project := loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, stacks.NewFileSet(in.Copy.Files), stacks.ObjectVars(in.Copy))
	if err != nil {
		return &RebuildError{Reason: ObjectErrorDetail(err)}
	}
	node, err := d.StandaloneNode(ctx, in.EnvID)
	if err != nil {
		return err
	}
	live, err := d.readStandalone(ctx, in.EnvID, node, in.Stack.Name)
	if err != nil {
		return err
	}
	if err := stacks.PrepareStandalone(project, in.Stack.ID, in.Rebuilt.Pins, objs); err != nil {
		return &RebuildError{Reason: prepareProblem(err).Message}
	}
	diff, err := diffStandalone(project, live.Containers)
	if err != nil {
		return err
	}
	cmp, err := d.compareOnNode(ctx, in, node, project, live.Containers, diff)
	if err != nil {
		return err
	}
	// What a recreate carries with it follows the service that causes it: a gap when that service is one, and a
	// recreation for the ownership labels when it is not.
	classifyOwnershipOnly(cmp, rv)
	for _, name := range diff.Removed {
		rv.Services = append(rv.Services, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeRemoved})
	}
	countServices(rv)
	return nil
}

// countServices counts the services of rv: the ownership-only ones apart, the rest by change.
func countServices(rv *Review) {
	for _, c := range rv.Services {
		if c.OwnershipOnly {
			rv.Summary.OwnershipOnly++
			continue
		}
		bump(&rv.Summary.Services, c.Change)
	}
}

// loadRebuilt loads the compose file of a rebuilt definition the way every deploy loads one (stacks.Deployable). A
// file that does not load is a *RebuildError naming the first problem.
func loadRebuilt(ctx context.Context, name string, target stacks.Target, copy stacks.WorkingCopy) (*stacks.Loaded, error) {
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: name, Target: target, Copy: copy})
	if err != nil {
		var refused *stacks.NotDeployableError
		if errors.As(err, &refused) && len(refused.Problems) > 0 {
			return nil, &RebuildError{Reason: refused.Problems[0].Message}
		}
		return nil, &RebuildError{Reason: err.Error()}
	}
	return loaded, nil
}

// adoptNames returns the compose names of the ownership-only services of a review, in order, once each.
func adoptNames(rows []stacks.ServiceChange) []string {
	var out []string
	for _, c := range rows {
		if c.OwnershipOnly && !slices.Contains(out, c.Short) {
			out = append(out, c.Short)
		}
	}
	return out
}
