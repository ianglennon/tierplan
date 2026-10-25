// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The states of a drift check (D-21).
const (
	// DriftNone is a stack whose running state matches its deployed revision.
	DriftNone = "none"
	// DriftDrifted is a stack that differs from its deployed revision.
	DriftDrifted = "drifted"
	// DriftStopped is a stack a person stopped: its services are scaled down on purpose, which is not drift.
	DriftStopped = "stopped"
	// DriftNotDeployed is a stack with no good revision to compare with.
	DriftNotDeployed = "not_deployed"
	// DriftUnknown is a stack whose deployed revision cannot be converted any more, so there is nothing to compare.
	DriftUnknown = "unknown"
)

// The changes of a drifted service: it differs, the revision has it and the engine does not, or the engine has it under
// the stack's ownership and the revision does not.
const (
	DriftChanged = "changed"
	DriftMissing = "missing"
	DriftExtra   = "extra"
)

// DriftService is one service that differs from the deployed revision. Diff is the review's row for it, masked by the
// revision's masker: Current is what runs now and Next what the revision deployed, as in a roll back review (a
// missing service has only Next, an extra one neither).
type DriftService struct {
	Name, Short, Change string
	Diff                stacks.ServiceChange
}

// DriftResult is a drift check: the state, the revision it was made against, when, the services that differ, how many
// settings differ in all, and a Fingerprint of what the engine held that changes whenever a service does.
type DriftResult struct {
	State       string
	Revision    int64
	CheckedAt   time.Time
	Services    []DriftService
	Count       int
	Fingerprint string
}

// DriftInput is a stack to check: the environment, the stack, its deployed revision (the newest good one, nil for none)
// and the images that revision ran, by swarm service name.
type DriftInput struct {
	EnvID    string
	Stack    Stack
	Deployed *Snapshot
	Pins     map[string]string
}

// Drift compares what runs with the deployed revision. The revision is prepared as a roll back to it is
// (PrepareRollback, with the pins it stored) and converted against what the swarm holds, and each service is compared
// with the one running by the review's own engine (stacks.DiffServices), minus what the swarm adds by itself and
// Bayplan's own labels (D-21, D-24). Current is what runs now and Next what the revision deployed, as a roll back review
// reads them.
//
// It only reads. A stopped stack is DriftStopped and a stack with no good revision DriftNotDeployed, neither read from
// the engine. A revision that cannot be redeployed any more is DriftUnknown. A failure to read the swarm is the error
// returned (D-23: nothing here writes to an engine).
func (d Deps) Drift(ctx context.Context, in DriftInput) (DriftResult, error) {
	res := DriftResult{CheckedAt: d.clock().Now().UTC()}
	switch {
	case in.Deployed == nil:
		res.State = DriftNotDeployed
		return res, nil
	case in.Stack.Stopped:
		res.State, res.Revision = DriftStopped, in.Deployed.Seq
		return res, nil
	case in.Stack.Target == stacks.TargetStandalone:
		// Standalone hosts follow in plan 08-20.
		res.State, res.Revision = DriftUnknown, in.Deployed.Seq
		return res, nil
	}
	res.Revision = in.Deployed.Seq
	deployed := *in.Deployed

	prep, err := d.PrepareRollback(ctx, in.Stack, deployed, in.Pins)
	if err != nil {
		var pe *PrepareError
		if errors.As(err, &pe) {
			res.State = DriftUnknown
			return res, nil
		}
		return DriftResult{}, err
	}
	project := prep.Loaded.Project
	objs, err := stacks.ResolveObjects(project, prep.Files, prep.Vars)
	if err != nil {
		res.State = DriftUnknown
		return res, nil
	}
	live, err := d.readLive(ctx, in.EnvID, in.Stack.Name, liveWant{
		allNetworks: true,
		allSecrets:  hasExternalObject(objs, stacks.KindSecret),
		allConfigs:  hasExternalObject(objs, stacks.KindConfig),
	})
	if err != nil {
		return DriftResult{}, err
	}
	// The conversion names each secret and config by the object the swarm already holds, so a service that has not
	// changed converts to the spec the swarm holds (as the review converts).
	plan, err := convert.Convert(project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: reviewRefs(in.Stack, objs, live.Objects)})
	if err != nil {
		res.State = DriftUnknown
		return res, nil
	}
	restored, err := RestoredPins(plan.Services, in.Pins)
	if err != nil {
		res.State = DriftUnknown
		return res, nil
	}
	byName := make(map[string]dockerx.ServiceDetail, len(live.Services))
	for _, s := range live.Services {
		byName[s.Name] = s
	}
	desired := make(map[string]swarm.ServiceSpec, len(plan.Services))
	for _, sp := range plan.Services {
		spec := sp.Spec
		if p, ok := restored[sp.Name]; ok && spec.TaskTemplate.ContainerSpec != nil {
			cs := *spec.TaskTemplate.ContainerSpec
			cs.Image = p.Ref
			spec.TaskTemplate.ContainerSpec = &cs
		}
		if existing, ok := byName[sp.Name]; ok {
			// The counter that makes the swarm replace tasks moves without a person changing the stack: it is
			// carried over, as a deploy does.
			spec.TaskTemplate.ForceUpdate = liveForceUpdate(existing.Spec)
		}
		desired[sp.Name] = stacks.StripOwnership(spec)
	}
	running, err := liveSpecs(live, in.Stack)
	if err != nil {
		return DriftResult{}, err
	}
	for name, spec := range running {
		running[name] = stacks.StripOwnership(spec)
	}

	masker := stacks.NewMasker(deployed.Copy.Vars, deployed.Copy.Files)
	for _, c := range stacks.DiffServices(desired, running, masker) {
		ds := DriftService{Name: c.Name, Short: c.Short, Diff: c}
		switch c.Change {
		case stacks.ChangeAdded:
			ds.Change = DriftMissing
			res.Count++
		case stacks.ChangeRemoved:
			ds.Change = DriftExtra
			res.Count++
		default:
			ds.Change = DriftChanged
			res.Count += c.Settings
		}
		res.Services = append(res.Services, ds)
	}
	res.Fingerprint = driftFingerprint(live, in.Stack, deployed.Seq)
	res.State = DriftNone
	if len(res.Services) > 0 {
		res.State = DriftDrifted
	}
	return res, nil
}

// driftFingerprint names what the engine held for the stack at a check: the revision and the version of each service
// the stack owns. A service that is changed by anyone moves its version, so a later check can tell that nothing moved.
func driftFingerprint(live liveStack, stack Stack, seq int64) string {
	own := OwnershipOf(stack)
	var parts []string
	for _, s := range live.Services {
		if own.OwnsWorkload(s.Labels) {
			parts = append(parts, s.ID+":"+strconv.FormatUint(s.Version, 10))
		}
	}
	sort.Strings(parts)
	out := "rev" + strconv.FormatInt(seq, 10)
	for _, p := range parts {
		out += "," + p
	}
	return out
}
