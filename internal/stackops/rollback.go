// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"fmt"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The stages of PrepareRollback, so the caller words the failure of each in its own way. The load
// stage is the check: a revision the check refuses carries a *stacks.NotDeployableError.
const (
	PrepareLoad    = "load"
	PrepareObjects = "objects"
	PrepareConvert = "convert"
	PreparePins    = "pins"
)

// PrepareError is a revision that cannot be redeployed as it stands: the stage that refused it and why.
type PrepareError struct {
	Stage string
	Err   error
	// Masker is the masker of the revision that was refused. A route that answers the refusal masks
	// what it says with it, for the converter quotes the values it refuses and a value may be a secret
	// variable's. A caller that has the revision's copy at hand may use its own.
	Masker stacks.Masker
}

// Error is the refusal's text with the cause masked by the refused revision's masker, so a log line that carries
// the error never quotes a secret variable (review WR-04). A zero Masker masks nothing.
func (e *PrepareError) Error() string {
	return fmt.Sprintf("stackops: prepare a roll back (%s): %s", e.Stage, e.Masker.Mask(fmt.Sprint(e.Err)))
}
func (e *PrepareError) Unwrap() error { return e.Err }

// RollbackPlan is a revision made ready to be redeployed: its compose project loaded, its secrets and
// configs resolved, its services converted, and the images it ran.
type RollbackPlan struct {
	Loaded *stacks.Loaded
	Files  stacks.FileSet
	Vars   map[string]string
	Plan   convert.Plan
	// Pins are the images the revision ran, by swarm service name, exactly as it stored them.
	Pins map[string]string
}

// PrepareRollback checks, loads and converts the revision to restore, and checks that it knows an image for every
// service. It sends nothing to any engine and asks no registry: a roll back redeploys exactly what the
// revision ran, so the digests it stored are the ones that go out (STK-07, T-07-93). Nothing is stored,
// so a revision that cannot be redeployed is refused before the stack gains a revision of its own.
//
// The convert stage's refusal is guarded (convert.GuardSecrets): a field fed by a secret variable is the fixed
// sentence, so the roll back route (rollbackPrepareProblem), the start route (CheckStart, planStart) and the
// PrepareError text the start job logs carry no part of the value (07-42, D-02).
//
// The project comes from stacks.Deployable, the pipeline the review runs, so a revision that used
// env_file or kept build beside image converts as it did when it was deployed.
func (d Deps) PrepareRollback(ctx context.Context, st Stack, target Snapshot, pins map[string]string) (RollbackPlan, error) {
	// The masker is built only for a refusal: the revision is judged on every roll back and start.
	refuse := func(stage string, err error) error {
		return &PrepareError{Stage: stage, Err: err, Masker: stacks.NewMasker(target.Copy.Vars, target.Copy.Files)}
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: target.Copy})
	if err != nil {
		return RollbackPlan{}, refuse(PrepareLoad, err)
	}
	files, vars := stacks.NewFileSet(target.Copy.Files), make(map[string]string, len(target.Copy.Vars))
	for _, v := range target.Copy.Vars {
		vars[v.Name] = v.Value
	}
	objs, err := stacks.ResolveObjects(loaded.Project, files, vars)
	if err != nil {
		return RollbackPlan{}, refuse(PrepareObjects, err)
	}
	plan, err := convert.Convert(loaded.Project, convert.Options{StackName: st.Name, StackID: st.ID, Objects: PlannedObjects(st.Name, objs)})
	if err != nil {
		return RollbackPlan{}, refuse(PrepareConvert, convert.GuardSecrets(err, stacks.NewSecretUse(target.Copy)))
	}
	if _, err := RestoredPins(plan.Services, pins); err != nil {
		return RollbackPlan{}, refuse(PreparePins, err)
	}
	return RollbackPlan{Loaded: loaded, Files: files, Vars: vars, Plan: plan, Pins: pins}, nil
}

// RestoredPins are the images a roll back sends: for each service with an image, the reference the
// revision stored for it, unchanged. A service the revision stored nothing for is a *PinsInvalidError
// (reason missing): a roll back never looks an image up, so it has nothing to send for it.
func RestoredPins(services []convert.ServicePlan, stored map[string]string) (map[string]Pin, error) {
	out := make(map[string]Pin, len(services))
	for _, sp := range services {
		text := sp.ImageAsWritten
		if text == "" {
			continue // a service with no image has nothing to pin
		}
		ref := stored[sp.Name]
		if ref == "" {
			return nil, &PinsInvalidError{Service: sp.Name, Reason: "missing"}
		}
		out[sp.Name] = Pin{Service: sp.Name, AsWritten: text, Ref: ref, Action: stacks.PinKeep}
	}
	return out, nil
}
