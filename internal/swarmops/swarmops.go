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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/rbac/swarmspec"
	"github.com/ianglennon/bayplan/internal/redact"
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
	// Record takes the audit row of each object an apply makes and each it removes again. Nil
	// records nothing.
	Record Recorder
	// CanCreate says whether the caller may create an object of a kind (KindNetwork, KindSecret,
	// KindConfig). Nil means nobody may: an apply that needs an object made is then refused.
	CanCreate func(kind string) bool
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
	// Pinned is true when Bayplan resolved the tag and wrote the digest into the spec.
	Pinned bool
}

// ImageError is a lookup that failed in a review: the spec is still returned, unpinned.
type ImageError struct{ Ref, Host, Reason string }

// Missing lists what the spec names that the cluster does not have, by the name or ID the spec used.
type Missing struct{ Networks, Secrets, Configs []string }

// stackLabelPrefix is the namespace docker stack deploy labels services and their containers
// with (com.docker.stack.namespace, com.docker.stack.image). A spec may not introduce one.
const stackLabelPrefix = "com.docker.stack."

// stackNamespaceLabel is the label that makes a service a stack's.
const stackNamespaceLabel = "com.docker.stack.namespace"

// common is what Review and Apply read the same way.
type common struct {
	envID, serviceID string
	version          uint64
	spec             json.RawMessage
	caller           Caller
	client           *dockerx.Client
	// staged are the objects the caller made in the editor; a review has none.
	staged []StagedObject
}

// prepared is a spec after the steps Review and Apply share. Its fields are filled as far as the
// work got, so a caller that fails can still audit what it learned.
type prepared struct {
	detail   dockerx.ServiceDetail
	current  swarm.ServiceSpec
	merged   swarm.ServiceSpec
	changed  bool
	networks []dockerx.NetworkRow
	resolver swarmspec.NetworkResolver
	// have is what the environment holds of the objects the spec can name, plan what the spec needs
	// done about them.
	have existing
	plan objectPlan
}

// prepare reads the service fresh and judges the submitted spec against it: stack ownership, the
// version, the strict decode, the merge of the masked values, and the rules a spec may not break
// (name, mode, stack labels). It does not look at the registry or the policy.
func prepare(ctx context.Context, deps Deps, c common) (*prepared, error) {
	p := &prepared{}
	detail, _, err := c.client.ServiceInspect(ctx, c.serviceID)
	if err != nil {
		return p, err
	}
	p.detail = detail
	owner := deps.StackOwner
	if owner == nil {
		owner = func(labels map[string]string) (string, bool) {
			v := labels[stackNamespaceLabel]
			return v, v != ""
		}
	}
	if stack, owned := owner(detail.Labels); owned {
		return p, &StackOwnedError{Stack: stack}
	}
	if c.version != detail.Version {
		return p, dockerx.ErrServiceChanged
	}
	if err := json.Unmarshal(detail.Spec, &p.current); err != nil {
		return p, fmt.Errorf("read the service's spec: %w", err)
	}
	sub, err := decodeSpec(c.spec)
	if err != nil {
		return p, err
	}
	if p.merged, err = redact.MergeMaskedSpec(p.current, sub, c.caller.Reveal); err != nil {
		return p, err
	}
	switch {
	case p.merged.Name != p.current.Name:
		return p, ErrNameImmutable
	case modeKind(p.merged.Mode) != modeKind(p.current.Mode):
		return p, ErrModeImmutable
	case newStackLabel(p.current, p.merged):
		return p, ErrStackLabel
	}
	if err := p.planObjects(ctx, c); err != nil {
		return p, err
	}
	if p.changed, err = specsDiffer(p.current, p.merged); err != nil {
		return p, err
	}
	return p, nil
}

// planObjects reads the networks, secrets and configs of the environment (the last two only when
// the spec names any) and works out what the spec needs done about the ones it names. The merged
// spec is copied first, so giving a reference its ID changes nothing the current spec shares.
func (p *prepared) planObjects(ctx context.Context, c common) error {
	var err error
	if p.networks, err = c.client.Networks(ctx); err != nil {
		return err
	}
	p.resolver = networkResolver(p.networks)
	p.have = existing{networks: p.networks}
	if cs := p.merged.TaskTemplate.ContainerSpec; cs != nil {
		if len(cs.Secrets) > 0 {
			if p.have.secrets, err = c.client.SecretList(ctx); err != nil {
				return err
			}
		}
		if len(cs.Configs) > 0 {
			if p.have.configs, err = c.client.ConfigList(ctx); err != nil {
				return err
			}
		}
	}
	if p.merged, err = cloneSpec(p.merged); err != nil {
		return err
	}
	p.plan = planObjects(&p.merged, p.have, c.staged)
	return nil
}

// cloneSpec copies a spec through its JSON form, so the copy shares nothing with the original.
func cloneSpec(spec swarm.ServiceSpec) (swarm.ServiceSpec, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return swarm.ServiceSpec{}, fmt.Errorf("encode a spec: %w", err)
	}
	var out swarm.ServiceSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		return swarm.ServiceSpec{}, fmt.Errorf("read a spec: %w", err)
	}
	return out, nil
}

// decodeSpec decodes raw strictly: an unknown field, a wrong type or data after the object is
// refused, and the decoder's own message, which names a field and never a value, is the reason.
func decodeSpec(raw json.RawMessage) (swarm.ServiceSpec, error) {
	var spec swarm.ServiceSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&spec)
	if err == nil {
		if _, more := dec.Token(); !errors.Is(more, io.EOF) {
			err = errors.New("unexpected data after the spec")
		}
	}
	if errors.Is(err, io.EOF) {
		err = errors.New("the spec is empty")
	}
	if err != nil {
		return swarm.ServiceSpec{}, &SpecInvalidError{Message: err.Error()}
	}
	return spec, nil
}

// modeKind names the kind of a service's mode.
func modeKind(m swarm.ServiceMode) string {
	switch {
	case m.Replicated != nil:
		return "replicated"
	case m.Global != nil:
		return "global"
	case m.ReplicatedJob != nil:
		return "replicated-job"
	case m.GlobalJob != nil:
		return "global-job"
	}
	return ""
}

// stackLabels are the stack-namespace labels of a spec, on the service and on its container spec.
func stackLabels(spec swarm.ServiceSpec) map[string]string {
	out := map[string]string{}
	for k, v := range spec.Labels {
		if strings.HasPrefix(k, stackLabelPrefix) {
			out["service:"+k] = v
		}
	}
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for k, v := range cs.Labels {
			if strings.HasPrefix(k, stackLabelPrefix) {
				out["container:"+k] = v
			}
		}
	}
	return out
}

// newStackLabel says whether next carries a stack-namespace label that cur lacks.
func newStackLabel(cur, next swarm.ServiceSpec) bool {
	have := stackLabels(cur)
	for k := range stackLabels(next) {
		if _, ok := have[k]; !ok {
			return true
		}
	}
	return false
}

// specsDiffer compares two specs by what Docker would hold: an empty map, list or null are the
// same, so an editor that writes {} for a missing Labels does not make a change.
func specsDiffer(a, b swarm.ServiceSpec) (bool, error) {
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	return !reflect.DeepEqual(ca, cb), nil
}

func canonical(spec swarm.ServiceSpec) (any, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode a spec: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("read a spec: %w", err)
	}
	return prune(v), nil
}

// prune drops the null, empty-object and empty-list members of every object.
func prune(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			x = prune(x)
			if empty(x) {
				delete(t, k)
				continue
			}
			t[k] = x
		}
		return t
	case []any:
		for i := range t {
			t[i] = prune(t[i])
		}
		return t
	}
	return v
}

func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// minIDPrefix is the shortest ID prefix a network target is matched by.
const minIDPrefix = 4

// findNetwork finds the network a target names: by ID, by name or by an ID prefix, as the daemon does.
func findNetwork(rows []dockerx.NetworkRow, target string) (dockerx.NetworkRow, bool) {
	for _, n := range rows {
		if n.ID == target || n.Name == target {
			return n, true
		}
	}
	if len(target) >= minIDPrefix {
		for _, n := range rows {
			if strings.HasPrefix(n.ID, target) {
				return n, true
			}
		}
	}
	return dockerx.NetworkRow{}, false
}

// networkResolver is the policy's lookup of a network target in the engine's network list.
func networkResolver(rows []dockerx.NetworkRow) swarmspec.NetworkResolver {
	return func(target string) (name, driver string, ok bool) {
		n, found := findNetwork(rows, target)
		return n.Name, n.Driver, found
	}
}

func (m Missing) any() bool { return len(m.Networks)+len(m.Secrets)+len(m.Configs) > 0 }

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// imageOf is the image a spec names, empty for a spec with no container spec.
func imageOf(spec swarm.ServiceSpec) string {
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		return cs.Image
	}
	return ""
}

// credentialFor chooses the registry credential for the image: the X-Registry-Auth header and the
// name of the credential, empty for none. An image that is not a reference goes without one, as the
// engine resolves it itself; a credential that was chosen and cannot be read is ErrRegistryUnresolved.
func credentialFor(ctx context.Context, deps Deps, envID, image string) (header, name string, err error) {
	if deps.Resolver == nil || image == "" {
		return "", "", nil
	}
	ref, perr := registry.ParseRef(image)
	if perr != nil {
		return "", "", nil //nolint:nilerr // not a reference: no credential applies
	}
	header, cand, err := deps.Resolver.AuthFor(ctx, envID, ref)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrRegistryUnresolved, err)
	}
	if cand != nil {
		name = cand.Name
	}
	return header, name, nil
}

// pinImage works out how the spec's image resolves. A changed tag with no digest is resolved to the
// digest it points at now and written into the spec as tag@sha256 (D-08); an image that carries a
// digest, or did not change, is kept as it is. A lookup that fails leaves the spec unpinned and is
// returned as an *ImageUnresolvedError beside the info; the header is the credential to send.
func (p *prepared) pinImage(ctx context.Context, deps Deps, c common) (info ImageInfo, auth string, unresolved *ImageUnresolvedError, err error) {
	image := imageOf(p.merged)
	info = ImageInfo{Ref: image, Changed: image != imageOf(p.current)}
	ref, perr := registry.ParseRef(image)
	if perr == nil {
		info.Tag, info.Digest, info.Host = ref.Tag, ref.Digest, ref.Domain
	}
	if auth, info.Credential, err = credentialFor(ctx, deps, c.envID, image); err != nil {
		return info, "", nil, err
	}
	if !info.Changed || perr != nil || ref.Digest != "" {
		return info, auth, nil, nil
	}
	res, rerr := c.client.ResolveImage(ctx, image, auth)
	if rerr != nil {
		return info, auth, &ImageUnresolvedError{Ref: image, Host: ref.Domain, Reason: dockerx.PullFailure(rerr), Err: rerr}, nil
	}
	cs := p.merged.TaskTemplate.ContainerSpec
	cs.Image = res.Ref
	info.Ref, info.Tag, info.Digest, info.Pinned = res.Ref, res.Tag, res.Digest, true
	// The platforms belong to the image. They follow it unless the caller set their own.
	if len(res.Platforms) > 0 && reflect.DeepEqual(platformsOf(p.merged), platformsOf(p.current)) {
		pl := swarm.Placement{}
		if p.merged.TaskTemplate.Placement != nil {
			pl = *p.merged.TaskTemplate.Placement
		}
		pl.Platforms = nil
		for _, plat := range res.Platforms {
			osName, arch, _ := strings.Cut(plat, "/")
			pl.Platforms = append(pl.Platforms, swarm.Platform{OS: osName, Architecture: arch})
		}
		p.merged.TaskTemplate.Placement = &pl
	}
	return info, auth, nil, nil
}

func platformsOf(spec swarm.ServiceSpec) []swarm.Platform {
	if pl := spec.TaskTemplate.Placement; pl != nil {
		return pl.Platforms
	}
	return nil
}

// judge runs the host-escalation policy on the spec exactly as it would be forwarded.
func (p *prepared) judge(ctx context.Context, deps Deps, c common) (rbac.ServiceDecision, []byte, error) {
	raw, err := json.Marshal(p.merged)
	if err != nil {
		return rbac.ServiceDecision{}, nil, fmt.Errorf("encode the spec: %w", err)
	}
	if deps.Checker == nil {
		return rbac.ServiceDecision{}, nil, errors.New("swarmops: no checker")
	}
	dec, err := deps.Checker.CheckServiceSpec(ctx, c.caller.Principal, c.envID, raw, p.resolver)
	return dec, raw, err
}
