// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/redact"
)

// ApplyInput is a spec to apply, as ReviewInput.
type ApplyInput struct {
	EnvID     string
	Client    *dockerx.Client
	ServiceID string
	Version   uint64
	Spec      json.RawMessage
	Caller    Caller
	// Objects are the objects the spec names that do not exist yet, with the values to make them
	// from. Those the spec does not name are ignored.
	Objects []StagedObject
}

// Outcome is what a write that may make objects and then sends a service (an apply, a create) did, as
// far as it got.
type Outcome struct {
	Image    ImageInfo
	Findings []compose.Finding
	Warnings []string
	// Steps are the steps of the write, in order, as far as it got. Cleanup is what removing the
	// objects of a failed write did.
	Steps   []ApplyStep
	Cleanup CleanupReport
}

// ApplyResult is what an apply did. It is filled as far as the work got, also when Apply returns an
// error, so the caller can audit the attempt.
type ApplyResult struct {
	ServiceID, Name string
	// Changed is true when the spec differs from the service's current one.
	Changed bool
	Outcome
}

// scrubbedError is an error whose text has been scrubbed of the values of a spec the caller may not
// see. It keeps the original for errors.Is and errors.As, which only compare sentinels and types.
type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// Apply sends the spec once. In order it: inspects the service fresh and refuses a version that is
// not the one sent (dockerx.ErrServiceChanged), a stack's service (*StackOwnedError), a spec that
// does not decode (*SpecInvalidError), a hidden value with nothing behind it or a placeholder from a
// revealing caller (*redact.PlaceholderError), a changed name or mode kind and a new stack label;
// refuses any caller a secret reference whose SecretName is not the name of its SecretID
// (*SpecInvalidError) and a caller without secrets.reveal a secret reference the service does not
// have (*RevealRequiredError), both by the engine's secret IDs and before any lookup or write; refuses a spec equal to the current one
// (ErrNothingToApply); resolves the digest of a changed tag; runs the host-escalation policy on the
// exact spec it will send (*RefusedError); refuses a network, secret or config that does not exist and
// was not staged (*MissingError) and a caller who may not create the kinds the staged ones are
// (*ObjectPermissionError), all before anything is written; creates the
// staged objects in dependency order (networks, secrets, configs), fills their IDs into the spec and
// judges it again; and sends one ServiceUpdate with the version and the credential, never retried.
// When anything after the first creation fails, every object this apply made is removed again in
// reverse order and the result says which were and which could not be. A refusal by Docker is a
// *RejectedError; a version taken by another writer in between is dockerx.ErrServiceChanged. No error
// and no warning carries a value the caller may not see. The result is filled as far as the work got.
func Apply(ctx context.Context, deps Deps, in ApplyInput) (ApplyResult, error) {
	c := common{envID: in.EnvID, serviceID: in.ServiceID, version: in.Version, spec: in.Spec, caller: in.Caller, client: in.Client, staged: in.Objects}
	var res ApplyResult
	p, err := prepare(ctx, deps, c)
	res.ServiceID, res.Name = p.detail.ID, p.detail.Name
	if err != nil {
		return res, err
	}
	res.Changed = p.changed
	if !p.changed {
		return res, ErrNothingToApply
	}
	err = p.commit(ctx, deps, c, &res.Outcome, false, func(raw []byte, auth string) ([]string, error) {
		return in.Client.ServiceUpdate(ctx, p.detail.ID, in.Version, raw, auth)
	})
	return res, err
}

// commit is the part of a write that an apply and a create share, from the image on. In order it:
// resolves the digest of a changed tag and sends nothing unpinned; runs the host-escalation policy on
// the exact spec it will send (*RefusedError); refuses a network, secret or config that does not exist
// and was not staged (*MissingError) and a caller who may not create the kinds the staged ones are
// (*ObjectPermissionError), all before anything is written; creates the staged objects in dependency
// order (networks, secrets, configs), fills their IDs into the spec and judges it again; and calls
// send once with the bytes and the credential, never retried. When anything after the first creation
// fails, every object this write made is removed again in reverse order and out says which were and
// which could not be. A refusal by Docker is a *RejectedError. No error and no warning carries a value
// the caller may not see. out is filled as far as the work got. creating names the service step's
// failure create_failed rather than update_failed.
func (p *prepared) commit(ctx context.Context, deps Deps, c common, out *Outcome, creating bool, send func(raw []byte, auth string) ([]string, error)) error {
	out.Steps = plannedSteps(p)
	info, auth, unresolved, err := p.pinImage(ctx, deps, c)
	out.Image = info
	if info.Pinned {
		out.Steps[0].State = StepDone
	}
	if err != nil {
		return err
	}
	if unresolved != nil {
		out.Steps[0].State, out.Steps[0].Code = StepFailed, stepCode(unresolved)
		return unresolved
	}
	dec, raw, err := p.judge(ctx, deps, c)
	out.Findings = dec.Findings
	if err != nil {
		return err
	}
	if !dec.Allowed {
		return &RefusedError{Reason: dec.Reason, Message: dec.Message, Findings: dec.Findings}
	}
	if p.plan.missing.any() {
		return &MissingError{Missing: p.plan.missing}
	}
	if denied := deps.refusedKinds(p.plan.create); len(denied) > 0 {
		return &ObjectPermissionError{Kinds: denied}
	}
	scrub := func(s string) string {
		if c.caller.Reveal {
			return s
		}
		return redact.ScrubServiceText(s, p.merged)
	}

	var created []CreatedObject
	// fail ends the write with err: whatever this write made is removed again.
	fail := func(err error) error {
		if len(created) > 0 {
			out.Cleanup = cleanupObjects(ctx, c.client, created, deps.Record)
		}
		return err
	}
	if len(p.plan.create) > 0 {
		var cerr error
		created, cerr = createObjects(ctx, c.client, p.plan.create, deps.Record)
		out.Steps = out.markObjects(created, cerr)
		if cerr != nil {
			return fail(cerr)
		}
		if raw, err = p.adopt(ctx, deps, c, created); err != nil {
			return fail(err)
		}
	}

	warnings, err := send(raw, auth)
	last := len(out.Steps) - 1
	if err != nil {
		var rej *dockerx.SpecRejectedError
		switch {
		case errors.As(err, &rej):
			err = &RejectedError{Message: scrub(rej.Message)}
		case !c.caller.Reveal:
			err = &scrubbedError{err: err, msg: scrub(err.Error())}
		}
		out.Steps[last].State, out.Steps[last].Code = StepFailed, stepCode(err)
		if creating && out.Steps[last].Code == codeUpdateFailed {
			out.Steps[last].Code = codeCreateFailed
		}
		return fail(err)
	}
	out.Steps[last].State = StepDone
	out.Warnings = make([]string, len(warnings))
	for i, w := range warnings {
		out.Warnings[i] = scrub(w)
	}
	return nil
}

// refusedKinds lists, in creation order, the kinds of the objects to create that the caller may not
// create. Without a CanCreate nobody may.
func (d Deps) refusedKinds(objects []StagedObject) []string {
	var denied []string
	for _, kind := range kinds(objects) {
		if d.CanCreate == nil || !d.CanCreate(kind) {
			denied = append(denied, kind)
		}
	}
	return denied
}

// plannedSteps are the steps an apply will take, none reached yet: the image, each object to
// create in creation order, the service.
func plannedSteps(p *prepared) []ApplyStep {
	steps := []ApplyStep{{Step: StepImage, State: StepSkipped}}
	for _, o := range p.plan.create {
		steps = append(steps, ApplyStep{Step: StepObject, Kind: o.Kind, Name: o.Name, State: StepSkipped})
	}
	return append(steps, ApplyStep{Step: StepService, Name: p.detail.Name, State: StepSkipped})
}

// markObjects marks the object steps: those made are done, the one that failed (err) is failed with
// its code, the rest stay skipped.
func (r Outcome) markObjects(made []CreatedObject, err error) []ApplyStep {
	steps := slices.Clone(r.Steps)
	n := 0
	for i := range steps {
		if steps[i].Step != StepObject {
			continue
		}
		switch {
		case n < len(made):
			steps[i].State = StepDone
		case n == len(made) && err != nil:
			steps[i].State, steps[i].Code = StepFailed, stepCode(err)
		}
		n++
	}
	return steps
}

// adopt writes the IDs of the objects just made into the spec's references to them, adds the
// networks to what the policy knows and judges the spec again, so the check still runs on exactly
// the bytes that are sent. It returns those bytes.
func (p *prepared) adopt(ctx context.Context, deps Deps, c common, made []CreatedObject) ([]byte, error) {
	ids := map[string]string{}
	for _, o := range made {
		ids[objectKey(o.Kind, o.Name)] = o.ID
		if o.Kind == KindNetwork {
			p.networks = append(p.networks, dockerx.NetworkRow{ID: o.ID, Name: o.Name, Driver: dockerx.DriverOverlay, Scope: dockerx.ScopeSwarm, Attachable: true})
		}
	}
	p.resolver = networkResolver(p.networks)
	for _, f := range p.plan.fills {
		if id, ok := ids[f.key]; ok {
			f.setID(id)
		}
	}
	dec, raw, err := p.judge(ctx, deps, c)
	if err != nil {
		return nil, err
	}
	if !dec.Allowed {
		return nil, &RefusedError{Reason: dec.Reason, Message: dec.Message, Findings: dec.Findings}
	}
	return raw, nil
}
