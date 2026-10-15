// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"encoding/json"
	"errors"

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

// ApplyResult is what an apply did. It is filled as far as the work got, also when Apply returns an
// error, so the caller can audit the attempt.
type ApplyResult struct {
	ServiceID, Name string
	// Changed is true when the spec differs from the service's current one.
	Changed  bool
	Image    ImageInfo
	Findings []compose.Finding
	Warnings []string
	// Steps are the steps of the apply, in order, as far as it got. Cleanup is what removing the
	// objects of a failed apply did.
	Steps   []ApplyStep
	Cleanup CleanupReport
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
// revealing caller (*redact.PlaceholderError), a changed name or mode kind, a new stack label and a
// spec equal to the current one (ErrNothingToApply); resolves the digest of a changed tag; runs the
// host-escalation policy on the exact spec it will send (*RefusedError); refuses a network that does
// not exist (*MissingError); and sends one ServiceUpdate with the version and the credential, never
// retried. A refusal by Docker is a *RejectedError; a version taken by another writer in between is
// dockerx.ErrServiceChanged. No error and no warning carries a value the caller may not see. The
// result is filled as far as the work got.
func Apply(ctx context.Context, deps Deps, in ApplyInput) (ApplyResult, error) {
	c := common{envID: in.EnvID, serviceID: in.ServiceID, version: in.Version, spec: in.Spec, caller: in.Caller, client: in.Client}
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
	info, auth, unresolved, err := p.pinImage(ctx, deps, c)
	res.Image = info
	if err != nil {
		return res, err
	}
	if unresolved != nil {
		return res, unresolved
	}
	dec, raw, err := p.judge(ctx, deps, c)
	res.Findings = dec.Findings
	if err != nil {
		return res, err
	}
	if !dec.Allowed {
		return res, &RefusedError{Reason: dec.Reason, Message: dec.Message, Findings: dec.Findings}
	}
	if m := missingObjects(p.merged, p.networks); m.any() {
		return res, &MissingError{Missing: m}
	}
	scrub := func(s string) string {
		if in.Caller.Reveal {
			return s
		}
		return redact.ScrubServiceText(s, p.merged)
	}
	warnings, err := in.Client.ServiceUpdate(ctx, p.detail.ID, in.Version, raw, auth)
	if err != nil {
		var rej *dockerx.SpecRejectedError
		if errors.As(err, &rej) {
			return res, &RejectedError{Message: scrub(rej.Message)}
		}
		if !in.Caller.Reveal {
			return res, &scrubbedError{err: err, msg: scrub(err.Error())}
		}
		return res, err
	}
	res.Warnings = make([]string, len(warnings))
	for i, w := range warnings {
		res.Warnings[i] = scrub(w)
	}
	return res, nil
}
