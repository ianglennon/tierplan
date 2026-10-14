// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

var (
	// ErrServiceChanged means somebody else changed the service between the moment it was
	// read and the moment the update arrived: the daemon refused the version as out of
	// sequence (RESEARCH E8). The write did not happen and is never retried.
	ErrServiceChanged = errors.New("dockerx: the service was changed by someone else")
	// ErrServiceNotFound means the manager knows no such service.
	ErrServiceNotFound = errors.New("dockerx: no such service")
	// ErrNotScalable means the service is not in replicated mode: a global service runs one
	// task per node and a job runs to completion, so neither has a replica count to set.
	ErrNotScalable = errors.New("dockerx: the service has no replica count")
)

// outOfSequence is the text of the 500 a manager answers an update whose version is not the
// service's current one (RESEARCH E8).
const outOfSequence = "update out of sequence"

// ServiceUpdateStatus is the state of a service's last update or rollback.
type ServiceUpdateStatus struct {
	State, Message         string
	StartedAt, CompletedAt time.Time
}

// ServiceDetail is one service as an inspect shows it. Spec and PreviousSpec are the
// documents as the engine states them; they can hold environment values, so they are for
// the code that edits a service and never for a list.
type ServiceDetail struct {
	ID, Name string
	// Mode is replicated, global, replicated-job or global-job.
	Mode string
	// Stack is the stack the service belongs to, empty for a service made on its own.
	Stack string
	// Image is the image the spec names, as written; empty for a spec with no container spec.
	Image string
	// Version is the version a write must send back (swarm.Version.Index).
	Version              uint64
	CreatedAt, UpdatedAt time.Time
	Spec                 json.RawMessage
	// PreviousSpec is the spec before the last update, nil when the service has none.
	PreviousSpec json.RawMessage
	// UpdateStatus is nil for a service that was never updated or whose update left none.
	UpdateStatus *ServiceUpdateStatus
	// Agent is true for a Bayplan agent service.
	Agent bool
	// Labels are the service's own labels, for the rules that look at who owns it.
	Labels map[string]string
	// Replicas is the replica count of a replicated service, zero otherwise.
	Replicas uint64
	// TemplateHash identifies the spec's TaskTemplate, Runtime aside: the SpecHash of a task
	// that runs the service's current version.
	TemplateHash string
}

// ServiceInspect reads one service by ID or name, and returns the typed detail and the
// raw document. The specs are not given the engine's defaults. A service the manager does
// not know is ErrServiceNotFound.
func (c *Client) ServiceInspect(ctx context.Context, id string) (ServiceDetail, []byte, error) {
	res, err := c.cur().ServiceInspect(ctx, id, client.ServiceInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return ServiceDetail{}, nil, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), ErrServiceNotFound)
		}
		return ServiceDetail{}, nil, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), err)
	}
	d, err := serviceDetail(res.Service)
	if err != nil {
		return ServiceDetail{}, nil, fmt.Errorf("read service on %s: %w", c.t.Describe(), err)
	}
	return d, res.Raw, nil
}

// serviceDetail turns an inspected service into its detail.
func serviceDetail(s swarm.Service) (ServiceDetail, error) {
	d := ServiceDetail{
		ID: s.ID, Name: s.Spec.Name, Mode: serviceMode(s.Spec.Mode), Stack: s.Spec.Labels[stackLabel],
		Version: s.Version.Index, CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(), Agent: isAgentService(s),
		Labels: s.Spec.Labels, TemplateHash: specHash(s.Spec.TaskTemplate),
	}
	if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
		d.Image = cs.Image
	}
	if r := s.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
		d.Replicas = *r.Replicas
	}
	var err error
	if d.Spec, err = json.Marshal(s.Spec); err != nil {
		return ServiceDetail{}, err
	}
	if s.PreviousSpec != nil {
		if d.PreviousSpec, err = json.Marshal(s.PreviousSpec); err != nil {
			return ServiceDetail{}, err
		}
	}
	if u := s.UpdateStatus; u != nil {
		st := &ServiceUpdateStatus{State: string(u.State), Message: u.Message}
		if u.StartedAt != nil {
			st.StartedAt = u.StartedAt.UTC()
		}
		if u.CompletedAt != nil {
			st.CompletedAt = u.CompletedAt.UTC()
		}
		d.UpdateStatus = st
	}
	return d, nil
}

// ServiceScale sets the replica count of one replicated service. It reads the service
// once, and when the count is already replicas it answers changed false and writes
// nothing. Otherwise it sets the count on the spec it read and sends that spec back once,
// with the version it read and encodedAuth (a base64url X-Registry-Auth value, empty for
// none) so the swarm can resolve the image. Nothing else in the spec changes.
//
// A service that is not replicated is ErrNotScalable. A version the manager no longer
// holds is ErrServiceChanged, and a service removed in between is ErrServiceNotFound.
// The write is never retried: whoever asked for it asks again (P2 D-42).
func (c *Client) ServiceScale(ctx context.Context, id string, replicas uint64, encodedAuth string) (previous uint64, changed bool, err error) {
	cli := c.cur()
	res, err := cli.ServiceInspect(ctx, id, client.ServiceInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return 0, false, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), ErrServiceNotFound)
		}
		return 0, false, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), err)
	}
	svc := res.Service
	if svc.Spec.Mode.Replicated == nil {
		return 0, false, fmt.Errorf("scale service on %s: %w", c.t.Describe(), ErrNotScalable)
	}
	if r := svc.Spec.Mode.Replicated.Replicas; r != nil {
		previous = *r
	}
	if svc.Spec.Mode.Replicated.Replicas != nil && previous == replicas {
		return previous, false, nil
	}
	spec := svc.Spec
	spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &replicas}
	_, err = cli.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{
		Version:             svc.Version,
		Spec:                spec,
		EncodedRegistryAuth: encodedAuth,
		QueryRegistry:       false,
	})
	if err != nil {
		return previous, false, c.serviceWriteError("scale", err)
	}
	return previous, true, nil
}

// serviceWriteError turns the engine's refusal of a service update into this package's
// errors: out of sequence is ErrServiceChanged, a service that went away ErrServiceNotFound.
func (c *Client) serviceWriteError(op string, err error) error {
	switch {
	case strings.Contains(err.Error(), outOfSequence):
		return fmt.Errorf("%s service on %s: %w", op, c.t.Describe(), ErrServiceChanged)
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%s service on %s: %w", op, c.t.Describe(), ErrServiceNotFound)
	}
	return fmt.Errorf("%s service on %s: %w", op, c.t.Describe(), err)
}

// inspectForWrite reads one service for a write that must be judged against its version.
func (c *Client) inspectForWrite(ctx context.Context, cli *client.Client, id string) (swarm.Service, error) {
	res, err := cli.ServiceInspect(ctx, id, client.ServiceInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return swarm.Service{}, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), ErrServiceNotFound)
		}
		return swarm.Service{}, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), err)
	}
	return res.Service, nil
}

// NetworkName is a swarm network's ID and name, enough to say which network a service's spec
// names.
type NetworkName struct{ ID, Name string }

// SwarmNetworkNames lists the swarm-scoped networks of the cluster this client talks to, by
// name then ID, so a service's network targets (an ID or a name, as the spec wrote it) can be
// shown by name. It is read-only.
func (c *Client) SwarmNetworkNames(ctx context.Context) ([]NetworkName, error) {
	ns, err := c.cur().NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list networks on %s: %w", c.t.Describe(), err)
	}
	out := make([]NetworkName, 0, len(ns.Items))
	for _, n := range ns.Items {
		if n.Scope == ScopeSwarm {
			out = append(out, NetworkName{ID: n.ID, Name: n.Name})
		}
	}
	slices.SortFunc(out, func(a, b NetworkName) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

var (
	// ErrDigestOnly means the image names a digest and no tag, so there is no tag to move and
	// nothing to re-pin: the reference already is the exact image.
	ErrDigestOnly = errors.New("dockerx: the image is pinned by digest only")
	// ErrNoPreviousSpec means the service has never been updated, so it has no earlier spec to
	// roll back to.
	ErrNoPreviousSpec = errors.New("dockerx: the service has no previous spec")
)

// RollbackRefusedError is the manager refusing a rollback with a 400: the previous spec names
// something that no longer exists (a secret, a config or a network), or is otherwise not
// acceptable. Message is the manager's own words.
type RollbackRefusedError struct{ Message string }

func (e *RollbackRefusedError) Error() string { return "rollback refused: " + e.Message }

// ResolvedImage is an image reference pinned to the digest its tag points at now.
type ResolvedImage struct {
	// Ref is the pinned reference, tag@sha256:digest, as docker writes it.
	Ref string
	// Tag is the tag that was resolved, Digest what it pointed at.
	Tag, Digest string
	// Platforms are os/arch of each platform the image is built for, the way a swarm wants
	// them (attestation entries dropped, the architecture of arm blanked, duplicates dropped).
	Platforms []string
}

// ResolveImage resolves the tag of image to the digest its registry holds for it now, through
// the manager's engine with the credential in encodedAuth (empty is anonymous). A digest the
// reference already carries is dropped first, since the tag is what is being asked about; a
// reference with no tag resolves latest. A reference with a digest and no tag is
// ErrDigestOnly. A registry that cannot answer is an error PullFailure classifies; this never
// falls back to the tag unpinned.
func (c *Client) ResolveImage(ctx context.Context, image, encodedAuth string) (ResolvedImage, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return ResolvedImage{}, &pullError{ref: image, where: c.t.Describe(), err: cerrdefs.ErrInvalidArgument.WithMessage(err.Error())}
	}
	tag := "latest"
	t, tagged := named.(reference.Tagged)
	if tagged {
		tag = t.Tag()
	}
	if _, digested := named.(reference.Digested); digested && !tagged {
		return ResolvedImage{}, fmt.Errorf("resolve %s on %s: %w", image, c.t.Describe(), ErrDigestOnly)
	}
	byTag, err := reference.WithTag(reference.TrimNamed(named), tag)
	if err != nil {
		return ResolvedImage{}, &pullError{ref: image, where: c.t.Describe(), err: cerrdefs.ErrInvalidArgument.WithMessage(err.Error())}
	}
	res, err := c.DistributionInspect(ctx, byTag.String(), encodedAuth)
	if err != nil {
		return ResolvedImage{}, err
	}
	pinned, err := reference.ParseNormalizedNamed(byTag.String() + "@" + res.Digest)
	if err != nil {
		return ResolvedImage{}, &pullError{ref: byTag.String(), where: c.t.Describe(), err: fmt.Errorf("the registry answered the digest %q: %w", res.Digest, err)}
	}
	return ResolvedImage{
		Ref: reference.FamiliarString(pinned), Tag: tag, Digest: res.Digest, Platforms: swarmPlatforms(res.Platforms),
	}, nil
}

// swarmPlatforms turns os/arch[/variant] platforms into the os/arch ones a swarm schedules by,
// as the docker client does for a service it creates: the architecture of arm is blanked
// (a node reports armv7l, never arm), the variant is dropped and duplicates are dropped
// (moby/moby#2294).
func swarmPlatforms(platforms []string) []string {
	var out []string
	for _, p := range platforms {
		parts := strings.Split(p, "/")
		if len(parts) < 2 {
			continue
		}
		arch := parts[1]
		if strings.EqualFold(arch, "arm") {
			arch = ""
		}
		if s := parts[0] + "/" + arch; !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// ForceUpdateOptions are the choices of ServiceForceUpdate.
type ForceUpdateOptions struct {
	// Version is the service version the caller read; a different current one is
	// ErrServiceChanged.
	Version uint64
	// Image, when not empty, replaces the image of the container spec (a pinned reference).
	Image string
	// Platforms, when not empty, become the placement platforms (os/arch, as ResolveImage
	// answers them).
	Platforms []string
	// EncodedAuth is a base64url X-Registry-Auth value, empty for none.
	EncodedAuth string
}

// ServiceForceUpdate bumps ForceUpdate on one service, so every task is replaced again, and
// when o.Image is set pins the container spec to it. It reads the service once and refuses
// ErrServiceChanged, without writing, when its version is not o.Version; otherwise it sends
// the spec it read, changed in those two fields only, once, with that version and
// QueryRegistry off. It takes no spec from the caller and is never retried.
func (c *Client) ServiceForceUpdate(ctx context.Context, id string, o ForceUpdateOptions) error {
	cli := c.cur()
	svc, err := c.inspectForWrite(ctx, cli, id)
	if err != nil {
		return err
	}
	if svc.Version.Index != o.Version {
		return fmt.Errorf("force update service on %s: %w", c.t.Describe(), ErrServiceChanged)
	}
	spec := svc.Spec
	spec.TaskTemplate.ForceUpdate++
	if o.Image != "" {
		if spec.TaskTemplate.ContainerSpec == nil {
			return fmt.Errorf("force update service on %s: the service has no container spec to pin an image on", c.t.Describe())
		}
		cs := *spec.TaskTemplate.ContainerSpec
		cs.Image = o.Image
		spec.TaskTemplate.ContainerSpec = &cs
	}
	if len(o.Platforms) > 0 {
		var pl swarm.Placement
		if spec.TaskTemplate.Placement != nil {
			pl = *spec.TaskTemplate.Placement
		}
		pl.Platforms = nil
		for _, p := range o.Platforms {
			osName, arch, _ := strings.Cut(p, "/")
			pl.Platforms = append(pl.Platforms, swarm.Platform{OS: osName, Architecture: arch})
		}
		spec.TaskTemplate.Placement = &pl
	}
	if _, err := cli.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{
		Version: svc.Version, Spec: spec, EncodedRegistryAuth: o.EncodedAuth, QueryRegistry: false,
	}); err != nil {
		return c.serviceWriteError("force update", err)
	}
	return nil
}

// ServiceRollback rolls one service back to its previous spec, once. It reads the service,
// refuses ErrServiceChanged when its version is not version and ErrNoPreviousSpec when it
// has no earlier spec, and otherwise sends the current spec with rollback=previous (the
// manager swaps the two). encodedAuth is the credential for the previous spec's image; when
// it is empty the update asks the manager to take the credential from the previous spec.
func (c *Client) ServiceRollback(ctx context.Context, id string, version uint64, encodedAuth string) error {
	cli := c.cur()
	svc, err := c.inspectForWrite(ctx, cli, id)
	if err != nil {
		return err
	}
	if svc.Version.Index != version {
		return fmt.Errorf("roll back service on %s: %w", c.t.Describe(), ErrServiceChanged)
	}
	if svc.PreviousSpec == nil {
		return fmt.Errorf("roll back service on %s: %w", c.t.Describe(), ErrNoPreviousSpec)
	}
	o := client.ServiceUpdateOptions{
		Version: svc.Version, Spec: svc.Spec, EncodedRegistryAuth: encodedAuth, Rollback: "previous", QueryRegistry: false,
	}
	if encodedAuth == "" {
		o.RegistryAuthFrom = swarm.RegistryAuthFromPreviousSpec
	}
	if _, err := cli.ServiceUpdate(ctx, svc.ID, o); err != nil {
		if cerrdefs.IsInvalidArgument(err) {
			if strings.Contains(err.Error(), "previous spec") {
				return fmt.Errorf("roll back service on %s: %w", c.t.Describe(), ErrNoPreviousSpec)
			}
			return fmt.Errorf("roll back service on %s: %w", c.t.Describe(), &RollbackRefusedError{Message: err.Error()})
		}
		return c.serviceWriteError("roll back", err)
	}
	return nil
}

// ServiceRemove removes the service id once and never retries: a remove whose answer was lost
// may have happened. The swarm stops its tasks; its images, volumes, networks, secrets and
// configs stay. A service the manager does not know is ErrServiceNotFound. The caller refuses
// a service a stack owns and Bayplan's own agent service before this is called.
func (c *Client) ServiceRemove(ctx context.Context, id string) error {
	if _, err := c.cur().ServiceRemove(ctx, id, client.ServiceRemoveOptions{}); err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove service on %s: %w", c.t.Describe(), ErrServiceNotFound)
		}
		return fmt.Errorf("remove service on %s: %w", c.t.Describe(), err)
	}
	return nil
}
