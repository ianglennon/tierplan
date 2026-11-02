// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package swarmops

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/redact"
)

var (
	// ErrImageRequired means a new service names no image: unlike an edit there is no current one to
	// keep, and TierPlan pins the image it creates the service with (D-04, D-08).
	ErrImageRequired = errors.New("swarmops: a new service needs an image")
	// ErrNameInvalid means the new service's name is not one Docker accepts: 1 to 63 letters, numbers,
	// dot, dash and underscore.
	ErrNameInvalid = errors.New("swarmops: the service name is not valid")
)

// NameTakenError is a new service's name that the swarm has already. It is dockerx.ErrServiceNameTaken
// through errors.Is, whether TierPlan saw the name in the list or the manager refused the create.
type NameTakenError struct{ Name string }

func (e *NameTakenError) Error() string {
	return "swarmops: a service named " + e.Name + " already exists"
}
func (e *NameTakenError) Is(target error) bool { return target == dockerx.ErrServiceNameTaken }

// serviceNameRe is the shape of a service name. Docker holds a name to its own, stricter rules and
// says so in words when one is broken (spec_rejected).
var serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)

// CreateInput is a new service to review or create: the spec the caller wrote (a create has no masked
// values to merge, so a hidden placeholder is refused), who they are, and for Create the objects the
// spec names that do not exist yet. Client reaches a manager; ReviewCreate only reads.
type CreateInput struct {
	EnvID  string
	Client *dockerx.Client
	Spec   json.RawMessage
	Caller Caller
	// Objects are the objects the spec names that do not exist yet, with the values to make them
	// from, as for an apply. A review ignores them.
	Objects []StagedObject
	// HostPolicy is what the manager's agent will forward; a review reports it.
	HostPolicy HostPolicy
}

// CreateResult is what a create did. It is filled as far as the work got, also when Create returns
// an error, so the caller can audit the attempt.
type CreateResult struct {
	// ServiceID is the ID the swarm gave the service, empty until it was made. Name is the name asked
	// for.
	ServiceID, Name string
	Outcome
}

// prepareCreate judges a new service's spec the way prepare judges an edit, without a service to
// read: the strict decode, a placeholder (nothing stands behind it), the name, the image, the stack
// labels, a name the swarm has, and the objects the spec names. The name is set on the result as soon
// as it is known, so a caller that fails can still audit what it was asked for.
func prepareCreate(ctx context.Context, in CreateInput, staged []StagedObject) (*prepared, common, error) {
	c := common{envID: in.EnvID, spec: in.Spec, caller: in.Caller, client: in.Client, staged: staged}
	p := &prepared{changed: true}
	sub, err := decodeSpec(in.Spec)
	if err != nil {
		return p, c, err
	}
	p.detail.Name = sub.Name
	// Nothing is current, so a hidden placeholder stands for nothing, whoever sends it.
	if p.merged, err = redact.MergeMaskedSpec(swarm.ServiceSpec{}, sub, false); err != nil {
		return p, c, err
	}
	switch {
	case !serviceNameRe.MatchString(p.merged.Name):
		return p, c, ErrNameInvalid
	case strings.TrimSpace(imageOf(p.merged)) == "":
		return p, c, ErrImageRequired
	case newStackLabel(swarm.ServiceSpec{}, p.merged):
		return p, c, ErrStackLabel
	}
	p.merged.TaskTemplate.ContainerSpec.Image = strings.TrimSpace(imageOf(p.merged))
	taken, err := nameTaken(ctx, in.Client, p.merged.Name)
	if err != nil {
		return p, c, err
	}
	if taken {
		return p, c, &NameTakenError{Name: p.merged.Name}
	}
	if err := p.planObjects(ctx, c); err != nil {
		return p, c, err
	}
	return p, c, nil
}

// nameTaken says whether a service of that name exists. It reads the list of services rather than
// inspecting the name, which Docker also matches against ID prefixes.
func nameTaken(ctx context.Context, cli *dockerx.Client, name string) (bool, error) {
	refs, err := cli.ServiceRefs(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range refs {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ReviewCreate judges a new service without changing anything: it resolves the image, runs the
// host-escalation policy on the exact spec a create would send and lists the networks, secrets and
// configs the spec names that do not exist. A refusal by the policy is not an error: it is Allowed false
// with the findings. Everything else a spec can be refused for is returned as an error (see Create).
func ReviewCreate(ctx context.Context, deps Deps, in CreateInput) (ReviewResult, error) {
	p, c, err := prepareCreate(ctx, in, nil)
	if err != nil {
		return ReviewResult{}, err
	}
	return p.review(ctx, deps, c, in.HostPolicy)
}

// Create makes a new service once. In order it refuses a spec that does not decode (*SpecInvalidError),
// a hidden placeholder (*redact.PlaceholderError), a bad name (ErrNameInvalid), a missing image
// (ErrImageRequired), a stack label (ErrStackLabel), a name the swarm has (dockerx.ErrServiceNameTaken)
// and, for a caller without secrets.reveal, any secret at all (*RevealRequiredError);
// then it does what an apply does from the image on (see commit): the image is resolved to a digest and a
// failed lookup is an *ImageUnresolvedError and nothing is made; the host-escalation policy runs on the
// whole spec (*RefusedError); objects that are neither there nor staged are a *MissingError and a kind the
// caller may not create an *ObjectPermissionError, before anything is written; the staged objects are made
// first and removed again if the service cannot be created; one ServiceCreate is sent with the credential
// and QueryRegistry off, never retried. No error and no warning carries a value the caller may not see.
// The result is filled as far as the work got.
func Create(ctx context.Context, deps Deps, in CreateInput) (CreateResult, error) {
	var res CreateResult
	p, c, err := prepareCreate(ctx, in, in.Objects)
	res.Name = p.detail.Name
	if err != nil {
		return res, err
	}
	err = p.commit(ctx, deps, c, &res.Outcome, true, func(raw []byte, auth string) ([]string, error) {
		id, warnings, err := in.Client.ServiceCreate(ctx, raw, auth)
		res.ServiceID = id
		return warnings, err
	})
	var taken *NameTakenError
	if errors.Is(err, dockerx.ErrServiceNameTaken) && !errors.As(err, &taken) {
		// The manager refused the name: another writer made it between the list and the create.
		err = &NameTakenError{Name: res.Name}
	}
	return res, err
}
