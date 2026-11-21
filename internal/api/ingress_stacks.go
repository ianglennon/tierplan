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

// ingressStacks is the stack side of the ingress deployer (D-14, D-44): it names the stacks of an environment that the
// ingress has to do with, and whether each is on the ingress network yet.
type ingressStacks struct {
	repo   stackops.Repo
	claims *ingressstore.Store
	read   ClusterReadFunc
}

// NewIngressStacks builds the adapter the ingress deployer lists the stacks to redeploy through. repo holds the stored
// stacks, claims the hostname claims and read the swarm's services and networks.
func NewIngressStacks(repo stackops.Repo, claims *ingressstore.Store, read ClusterReadFunc) ingressdeploy.Stacks {
	return ingressStacks{repo: repo, claims: claims, read: read}
}

// WithRoutes lists the swarm stacks of the environment that hold hostname claims, and those with none whose newest good
// revision declares ingress labels (a deploy from before ingress existed), by name. Attached is false while the ingress
// network is absent and, once it is there, true when the stack has routed services and every one of them lists it.
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
	for _, st := range all {
		if st.Discovered || st.Target != stacks.TargetSwarm {
			continue
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
	if len(out) == 0 || s.read == nil {
		return out, nil
	}
	// One cluster read for the lot. A swarm that cannot be read leaves every stack unattached: each is then listed as one
	// to redeploy, which is the safe thing to say about a stack that cannot be seen.
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
		for i := range out {
			svcs, err := c.ServicesWithLabel(ctx, stackNamespaceLabel, out[i].Name)
			if err != nil {
				return err
			}
			out[i].Attached = attachedTo(svcs, *net)
		}
		return nil
	})
	return out, nil
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
