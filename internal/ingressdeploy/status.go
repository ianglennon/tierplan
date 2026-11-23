// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// nodeActive is the status of an enrolled ingress node that has not been revoked.
const nodeActive = "active"

// Status is what an Admin reads of one environment's ingress: the switches, the objects the swarm holds, how many
// proxy nodes are expected, enrolled and ready, and the stacks that still have to be redeployed (D-10). It carries no
// token and no secret, only counts and names.
type Status struct {
	Enabled, Paused, AccessLogs bool
	// ObjectsKnown is false when the swarm could not be read (no manager is connected or the read failed): Objects is
	// then all false and means nothing.
	ObjectsKnown bool
	Objects      StatusObjects
	Nodes        StatusNodes
	LastEnsure   struct {
		// At is when the last pass over the environment ended, the zero time when none has.
		At time.Time
		// Class is the class of the pass: "" for one that succeeded, else one of the Ensure* classes.
		Class string
	}
	// PendingWipes is the number of certificate cache volumes still to be wiped on nodes that could not be reached.
	PendingWipes int
	// DisablingIncomplete is true when ingress is off while an ingress object remains in the swarm after a switch-off.
	DisablingIncomplete bool
	// EnableFailed is true when ingress is off while the objects a failed enable made remain in the swarm. A switch-off removes
	// them, and so does an enable that succeeds.
	EnableFailed bool
	// TokenDisabled is true when the join token was disabled, so no further node can enrol.
	TokenDisabled bool
	// Redeploy lists the stacks that must be redeployed to be attached to the ingress network, ordered by name.
	Redeploy []StackRef
}

// StatusObjects says which of the ingress objects the environment holds. A swarm holds a network, a service and a secret;
// a standalone host holds the proxy's container, a network and the cache volume, so the fields that are not its kind's
// stay false.
type StatusObjects struct {
	Network, Service, Secret bool
	// Container and Volume are a standalone host's proxy container and certificate cache volume.
	Container, Volume bool
}

// StatusNodes counts the proxy nodes.
type StatusNodes struct {
	// Expected is the swarm's nodes, each of which runs a proxy. Enrolled is the proxies that enrolled and were not
	// revoked. Ready is the enrolled proxies whose health is ok and whose applied bundle is the current one.
	Expected, Enrolled, Ready int
}

// Status reads the ingress state of an environment. A swarm or a standalone host that cannot be read leaves ObjectsKnown
// false and is not an error. A standalone host expects one proxy node.
func (s *Service) Status(ctx context.Context, envID string) (Status, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return Status{}, err
	}
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return Status{}, err
	}
	st := Status{Enabled: state.Enabled, Paused: state.Paused, AccessLogs: state.AccessLogs, Redeploy: []StackRef{}}
	st.LastEnsure.At, st.LastEnsure.Class = state.EnsureAt, state.EnsureError
	wipes, err := s.d.Store.PendingWipes(ctx, envID)
	if err != nil {
		return Status{}, err
	}
	st.PendingWipes = len(wipes)
	tok, err := s.d.Tokens.TokenStatus(ctx, envID)
	if err != nil {
		return Status{}, fmt.Errorf("read the join token: %w", err)
	}
	st.TokenDisabled = tok.Exists && tok.Disabled

	st.Nodes.Expected = len(info.Nodes)
	if s.d.Nodes != nil {
		rows, err := s.d.Nodes.ListNodes(ctx, envID)
		if err != nil {
			return Status{}, fmt.Errorf("list the ingress nodes: %w", err)
		}
		current, have := "", false
		if s.d.CurrentBundle != nil {
			current, have = s.d.CurrentBundle(envID)
		}
		for _, n := range rows {
			if n.Status != nodeActive {
				continue
			}
			st.Nodes.Enrolled++
			if have && n.Health == "ok" && n.AppliedBundleID == current {
				st.Nodes.Ready++
			}
		}
	}

	switch {
	case !info.Swarm:
		s.readStandaloneObjects(ctx, info, state, &st)
	case info.ManagerConnected:
		var l live
		if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
			l, err = readLive(ctx, c)
			return err
		}); err != nil {
			s.log().Warn("could not read the ingress objects for the status", "environment", envID, "error", err)
		} else {
			st.ObjectsKnown = true
			net, ok := l.network()
			st.Objects.Network = ok && ingress.OwnsNetwork(net.Name, net.Labels)
			st.Objects.Service = l.service != nil && ingress.OwnsService(l.service.Name, l.service.Labels)
			st.Objects.Secret = len(staleSecrets(l.secrets, envID, "")) > 0
			s.markLeftovers(&st, state.EnsureError, st.Objects.Network || st.Objects.Service || st.Objects.Secret)
		}
	}

	if s.d.Stacks != nil {
		refs, err := s.d.Stacks.WithRoutes(ctx, envID)
		if err != nil {
			return Status{}, fmt.Errorf("list the stacks that hold routes: %w", err)
		}
		for _, r := range refs {
			if !r.Attached {
				st.Redeploy = append(st.Redeploy, r)
			}
		}
		slices.SortFunc(st.Redeploy, func(a, b StackRef) int { return strings.Compare(a.Name, b.Name) })
	}
	return st, nil
}

// readStandaloneObjects fills the objects of a standalone host from the host's own engine, when its node is connected.
// A host that cannot be read leaves ObjectsKnown false, which is not an error: the status says it does not know.
func (s *Service) readStandaloneObjects(ctx context.Context, info EnvInfo, state ingressstore.DeployState, st *Status) {
	if len(info.Nodes) == 0 || !info.Nodes[0].Connected {
		return
	}
	l, err := s.readStandaloneLive(ctx, info)
	if err != nil {
		s.log().Warn("could not read the standalone ingress objects for the status", "environment", info.ID, "error", err)
		return
	}
	st.ObjectsKnown = true
	st.Objects.Container = l.proxy != nil
	st.Objects.Network = l.network != nil
	st.Objects.Volume = l.volume
	s.markLeftovers(st, state.EnsureError, st.Objects.Container || st.Objects.Network || st.Objects.Volume)
}

// markLeftovers says what an environment with ingress off and an ingress object still held is: the objects a failed enable
// made, or those a switch-off did not finish removing. A switch-off removes them, and so does an enable that succeeds.
func (s *Service) markLeftovers(st *Status, ensureClass string, held bool) {
	left := !st.Enabled && held
	st.EnableFailed = left && ensureClass == EnsureEnableFailed
	st.DisablingIncomplete = left && !st.EnableFailed
}
