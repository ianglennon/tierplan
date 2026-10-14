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
	// Replicas is the replica count of a replicated service, zero otherwise.
	Replicas uint64
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
	switch {
	case err == nil:
		return previous, true, nil
	case strings.Contains(err.Error(), outOfSequence):
		return previous, false, fmt.Errorf("scale service on %s: %w", c.t.Describe(), ErrServiceChanged)
	case cerrdefs.IsNotFound(err):
		return previous, false, fmt.Errorf("scale service on %s: %w", c.t.Describe(), ErrServiceNotFound)
	}
	return previous, false, fmt.Errorf("scale service on %s: %w", c.t.Describe(), err)
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
