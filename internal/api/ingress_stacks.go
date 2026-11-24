// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// stackNamespaceLabel is the label every service of a swarm stack carries, naming the stack.
const stackNamespaceLabel = "com.docker.stack.namespace"

// ClusterReadFunc runs a read-only fn on a manager of the environment's swarm (envs.Clusters.ClusterRead).
type ClusterReadFunc func(ctx context.Context, envID string, fn func(*dockerx.Client) error) error

// HostReadFunc runs a read-only fn on the one node of a standalone environment (stackops.Deps.StandaloneNode with
// envs.Aggregator.OnNode). A host that cannot be reached answers an error and fn does not run.
type HostReadFunc func(ctx context.Context, envID string, fn func(*dockerx.Client) error) error

const (
	// composeProjectLabel names the Compose project of a container, which is the stack's name on a standalone host.
	composeProjectLabel = "com.docker.compose.project"
	// composeServiceLabel names the service of a container within its Compose project.
	composeServiceLabel = "com.docker.compose.service"
)

// ingressStacks is the stack side of the ingress deployer (D-14, D-44): it names the stacks of an environment that the
// ingress has to do with, and whether each is on the ingress network yet.
type ingressStacks struct {
	repo   stackops.Repo
	claims *ingressstore.Store
	read   ClusterReadFunc
	host   HostReadFunc
}

// NewIngressStacks builds the adapter the ingress deployer lists the stacks to redeploy through. repo holds the stored
// stacks, claims the hostname claims, read the swarm's services and networks and host a standalone host's containers.
// Either read may be nil, which leaves the stacks it would have read unattached.
func NewIngressStacks(repo stackops.Repo, claims *ingressstore.Store, read ClusterReadFunc, host HostReadFunc) ingressdeploy.Stacks {
	return ingressStacks{repo: repo, claims: claims, read: read, host: host}
}

// WithRoutes lists the swarm and standalone stacks of the environment that hold hostname claims, and those with none whose
// newest good revision declares ingress labels (a deploy from before ingress existed), by name. Attached is false while
// the ingress network is absent and, once it is there, true when the stack has routed services and every one of them
// lists it; a standalone stack's is read from the host's containers (D-16), and is false when the host cannot be read.
func (s ingressStacks) WithRoutes(ctx context.Context, envID string) ([]ingressdeploy.StackRef, error) {
	all, err := s.repo.Stacks(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the stacks of the environment: %w", err)
	}
	routes, err := s.claims.LiveRoutes(ctx, envID)
	if err != nil {
		return nil, err
	}
	holds := map[string]bool{}
	for _, r := range routes {
		if r.StackID != "" {
			holds[r.StackID] = true
		}
	}
	var out []ingressdeploy.StackRef
	onHost := map[string]bool{} // stack IDs of the standalone ones
	for _, st := range all {
		if st.Discovered || (st.Target != stacks.TargetSwarm && st.Target != stacks.TargetStandalone) {
			continue
		}
		if st.Target == stacks.TargetStandalone {
			onHost[st.ID] = true
		}
		if holds[st.ID] {
			out = append(out, ingressdeploy.StackRef{ID: st.ID, Name: st.Name, HoldsClaims: true})
			continue
		}
		snap, ok, err := s.repo.LatestGoodRevision(ctx, st.ID)
		if err != nil {
			return nil, fmt.Errorf("read the newest good revision of stack %q: %w", st.Name, err)
		}
		if ok && declaresIngress(snap.Copy) {
			out = append(out, ingressdeploy.StackRef{ID: st.ID, Name: st.Name})
		}
	}
	slices.SortFunc(out, func(a, b ingressdeploy.StackRef) int { return strings.Compare(a.Name, b.Name) })
	var swarm, standalone []int
	for i := range out {
		if onHost[out[i].ID] {
			standalone = append(standalone, i)
		} else {
			swarm = append(swarm, i)
		}
	}
	if len(swarm) > 0 && s.read != nil {
		s.attachSwarm(ctx, envID, out, swarm)
	}
	if len(standalone) > 0 && s.host != nil {
		s.attachStandalone(ctx, envID, out, standalone)
	}
	return out, nil
}

// attachSwarm sets Attached on the swarm stacks out[idx] with one cluster read. A swarm that cannot be read leaves every
// stack unattached: each is then listed as one to redeploy, which is the safe thing to say about a stack that cannot be
// seen.
func (s ingressStacks) attachSwarm(ctx context.Context, envID string, out []ingressdeploy.StackRef, idx []int) {
	_ = s.read(ctx, envID, func(c *dockerx.Client) error {
		nets, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		var net *dockerx.NetworkRow
		for i := range nets {
			if nets[i].Name == ingress.NetworkName {
				net = &nets[i]
				break
			}
		}
		if net == nil {
			return nil
		}
		for _, i := range idx {
			svcs, err := c.ServicesWithLabel(ctx, stackNamespaceLabel, out[i].Name)
			if err != nil {
				return err
			}
			out[i].Attached = attachedTo(svcs, *net)
		}
		return nil
	})
}

// attachStandalone sets Attached on the standalone stacks out[idx] with one read of the host: a stack is attached when it
// has a routed container and every routed service has a container on the ingress network. A host that cannot be read
// leaves the stacks it did not get to unattached, the safe answer.
func (s ingressStacks) attachStandalone(ctx context.Context, envID string, out []ingressdeploy.StackRef, idx []int) {
	_ = s.host(ctx, envID, func(c *dockerx.Client) error {
		for _, i := range idx {
			rows, err := c.ContainersWithLabel(ctx, composeProjectLabel, out[i].Name)
			if err != nil {
				return err
			}
			out[i].Attached = containersAttached(rows)
		}
		return nil
	})
}

// containersAttached is true when some service of a Compose project declares a route and every such service has a
// container on the ingress network, by name.
func containersAttached(rows []dockerx.ContainerRow) bool {
	routed := map[string]bool{} // routed service -> has a container on the network
	for _, r := range rows {
		if !ingress.HasLabels(r.Labels) {
			continue
		}
		svc := r.Labels[composeServiceLabel]
		on := slices.ContainsFunc(r.Networks, func(n dockerx.ContainerNet) bool { return n.Name == ingress.NetworkName })
		routed[svc] = routed[svc] || on
	}
	for _, on := range routed {
		if !on {
			return false
		}
	}
	return len(routed) > 0
}

// attachedTo is true when some service of the stack declares a route and every such service lists the network, by ID or
// by name as its spec wrote it.
func attachedTo(svcs []dockerx.ServiceDetail, net dockerx.NetworkRow) bool {
	routed := 0
	for _, d := range svcs {
		if !ingress.HasLabels(d.Labels) {
			continue
		}
		routed++
		if !slices.ContainsFunc(d.NetworkIDs, func(id string) bool { return id == net.ID || id == net.Name }) {
			return false
		}
	}
	return routed > 0
}
