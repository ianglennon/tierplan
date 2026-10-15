// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package swarmops is the orchestration of a change to a swarm service that is more than one
// Docker call: review a full service spec, then apply it (RES-03, D-03). It imports no HTTP
// type and no huma; it takes the Docker client, the checker, the credential resolver and the
// caller through its input structs, so it is tested against the fake daemon with injected
// faults. Create (06-27), staged objects (06-26) and rotate (06-28) extend it.
//
// A change is judged against the service as it is when the work starts, never against what the
// caller saw: the version the caller read is compared, the masked values are merged back into
// the fresh spec, and the host-escalation check runs on the exact spec that is forwarded.
package swarmops

import (
	"context"
	"errors"

	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/registry"
)

var (
	// ErrNothingToApply means the spec equals the service's current one: nothing is written.
	ErrNothingToApply = errors.New("swarmops: the spec equals the current one")
	// ErrObjectsMissing means the spec names an object the cluster does not have. The error is
	// a *MissingError.
	ErrObjectsMissing = errors.New("swarmops: the spec names objects that do not exist")
	// ErrNameImmutable means the spec renames the service, which Docker does not allow.
	ErrNameImmutable = errors.New("swarmops: a service's name can't be changed")
	// ErrModeImmutable means the spec changes the kind of mode (replicated, global, a job).
	ErrModeImmutable = errors.New("swarmops: a service's mode can't be changed")
	// ErrStackLabel means the spec carries a stack namespace label the service did not have, which
	// would make a standalone service look like a stack's (Pitfall 9).
	ErrStackLabel = errors.New("swarmops: the stack namespace labels are reserved")
	// ErrSpecInvalid means the spec does not decode strictly into a service spec. The error is a
	// *SpecInvalidError.
	ErrSpecInvalid = errors.New("swarmops: the spec is not a service spec")
	// ErrStackOwned means a stack owns the service, so its settings change through the stack
	// (D-05). The error is a *StackOwnedError.
	ErrStackOwned = errors.New("swarmops: the service belongs to a stack")
	// ErrRefused means the host-escalation policy refuses the spec for this caller. The error is
	// a *RefusedError.
	ErrRefused = errors.New("swarmops: the spec needs Admin")
	// ErrImageUnresolved means the tag of a changed image could not be resolved to a digest. The
	// error is an *ImageUnresolvedError.
	ErrImageUnresolved = errors.New("swarmops: the image could not be pinned")
	// ErrRegistryUnresolved means a registry credential was chosen for the image and could not be
	// read, so nothing is sent.
	ErrRegistryUnresolved = errors.New("swarmops: the registry credential could not be read")
	// ErrRejected means the manager refused the spec. The error is a *RejectedError.
	ErrRejected = errors.New("swarmops: the manager rejected the spec")
)

// SpecInvalidError carries the decoder's own message, which names a field and a type and never a
// value.
type SpecInvalidError struct{ Message string }

func (e *SpecInvalidError) Error() string        { return "swarmops: the spec is not valid: " + e.Message }
func (e *SpecInvalidError) Is(target error) bool { return target == ErrSpecInvalid }

// StackOwnedError names the stack that owns the service.
type StackOwnedError struct{ Stack string }

func (e *StackOwnedError) Error() string        { return "swarmops: the service belongs to stack " + e.Stack }
func (e *StackOwnedError) Is(target error) bool { return target == ErrStackOwned }

// RefusedError is the policy's refusal: the reason, the message with one finding per line and the
// findings.
type RefusedError struct {
	Reason, Message string
	Findings        []compose.Finding
}

func (e *RefusedError) Error() string        { return "swarmops: refused: " + e.Reason }
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// MissingError lists the objects a spec names that the cluster does not have.
type MissingError struct{ Missing Missing }

func (e *MissingError) Error() string        { return "swarmops: the spec names objects that do not exist" }
func (e *MissingError) Is(target error) bool { return target == ErrObjectsMissing }

// ImageUnresolvedError is a tag that could not be looked up: the reference, the registry host and
// the classified reason (a dockerx.PullCode*).
type ImageUnresolvedError struct {
	Ref, Host, Reason string
	Err               error
}

func (e *ImageUnresolvedError) Error() string {
	return "swarmops: could not look up " + e.Ref + ": " + e.Err.Error()
}
func (e *ImageUnresolvedError) Unwrap() error        { return e.Err }
func (e *ImageUnresolvedError) Is(target error) bool { return target == ErrImageUnresolved }

// RejectedError is the manager's refusal of a spec. Message is its words, with every secret-bearing
// value of the spec replaced for a caller who may not reveal secrets.
type RejectedError struct{ Message string }

func (e *RejectedError) Error() string        { return "swarmops: rejected: " + e.Message }
func (e *RejectedError) Is(target error) bool { return target == ErrRejected }

// Caller is who asks and whether they may see masked values.
type Caller struct {
	Principal *rbac.Principal
	// Reveal is true when the caller holds secrets.reveal on the environment: the spec they edit is
	// unmasked, and nothing is masked in what they are answered.
	Reveal bool
}

// AuthResolver chooses the registry credential an image is looked up and pulled with: the
// X-Registry-Auth header and the credential it was made from. *registry.Resolver is one.
type AuthResolver interface {
	AuthFor(ctx context.Context, envID string, ref registry.Ref) (string, *registry.Candidate, error)
}

// Deps are what Review and Apply share.
type Deps struct {
	// Checker decides who may have host-escalation settings.
	Checker *rbac.Checker
	// Resolver chooses the registry credential; nil means no credential is ever sent.
	Resolver AuthResolver
	// StackOwner says which stack owns a service from its labels. Nil reads the label
	// docker stack deploy writes.
	StackOwner func(labels map[string]string) (stack string, owned bool)
}

// HostPolicy is what the manager's agent will forward: whether its owner allowed services with
// host access and whether the host is read-only. It decides nothing here; the review reports it so
// the page can warn before an apply the agent would refuse.
type HostPolicy struct {
	ManagerAllowsPrivileged bool
	ReadOnly                bool
}

// ImageInfo is how the image of the spec resolved.
type ImageInfo struct {
	// Ref is the image the spec will carry: tag@sha256:digest when the tag was resolved.
	Ref string
	// Tag and Digest are the parts of Ref; Host is its registry.
	Tag, Digest, Host string
	// Credential is the name of the stored credential the lookup used, empty for none. Never a secret.
	Credential string
	// Changed is true when the spec's image differs from the service's current one.
	Changed bool
}

// ImageError is a lookup that failed in a review: the spec is still returned, unpinned.
type ImageError struct{ Ref, Host, Reason string }

// Missing lists what the spec names that the cluster does not have, by the name or ID the spec used.
type Missing struct{ Networks, Secrets, Configs []string }
