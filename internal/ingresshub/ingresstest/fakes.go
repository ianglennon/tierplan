// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresstest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Bundles is a hub.BundleSource a test fills: the current bundle of each environment and the notifier that is
// told when one changes. It stands in for ingressrecon.Reconciler.
type Bundles struct {
	mu      sync.Mutex
	by      map[string]ingressbundle.Bundle
	changes *ingress.Notifier
}

func newBundles() *Bundles {
	return &Bundles{by: map[string]ingressbundle.Bundle{}, changes: ingress.NewNotifier()}
}

// Set makes b the current bundle of envID and tells the notifier.
func (b *Bundles) Set(envID string, bundle ingressbundle.Bundle) {
	b.mu.Lock()
	b.by[envID] = bundle
	b.mu.Unlock()
	b.changes.Notify(envID)
}

// Remove drops the current bundle of envID and tells the notifier.
func (b *Bundles) Remove(envID string) {
	b.mu.Lock()
	delete(b.by, envID)
	b.mu.Unlock()
	b.changes.Notify(envID)
}

// Current is the environment's current bundle and whether it has one.
func (b *Bundles) Current(envID string) (ingressbundle.Bundle, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bundle, ok := b.by[envID]
	return bundle, ok
}

// Changes is the notifier told whenever Set or Remove runs.
func (b *Bundles) Changes() *ingress.Notifier { return b.changes }

// MakeBundle builds a valid schema 1 bundle of the given config version with one route per hostname. The
// dynamic file names the version and the hostnames, so two calls with different arguments have different IDs
// and two with the same arguments have the same one.
func MakeBundle(t testing.TB, version int64, hostnames ...string) ingressbundle.Bundle {
	t.Helper()
	routes := make([]ingressbundle.RouteEntry, 0, len(hostnames))
	body := fmt.Sprintf("# ingresstest bundle, config version %d\n", version)
	for i, h := range hostnames {
		routes = append(routes, ingressbundle.RouteEntry{
			Router: fmt.Sprintf("r%d", i), Hostname: h, PathPrefix: "/", Stack: "stack", Service: "svc", Hash: fmt.Sprintf("hash%d", i),
		})
		body += "# route " + h + "\n"
	}
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: version},
		[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: []byte(body)}}, routes)
	if err != nil {
		t.Fatalf("ingresstest: build a bundle: %v", err)
	}
	return b
}

// NodeLists is the agents.NodeLists a test fills: the swarm node IDs each environment's manager last reported,
// read at the test clock, so the list is fresh until the test advances the clock past its age limit.
type NodeLists struct {
	clock *Clock
	mu    sync.Mutex
	lists map[string]nodeList
}

type nodeList struct {
	ids []string
	at  time.Time
}

func newNodeLists(c *Clock) *NodeLists {
	return &NodeLists{clock: c, lists: map[string]nodeList{}}
}

// Set makes ids the node list of envID, read now.
func (n *NodeLists) Set(envID string, ids ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lists[envID] = nodeList{ids: ids, at: n.clock.Now()}
}

// SwarmNodeIDs implements agents.NodeLists.
func (n *NodeLists) SwarmNodeIDs(envID string) ([]string, time.Time, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, ok := n.lists[envID]
	return l.ids, l.at, ok
}

// RefreshSwarmNodeIDs implements agents.NodeLists: a refresh finds the held list unchanged.
func (n *NodeLists) RefreshSwarmNodeIDs(_ context.Context, envID string, _ time.Duration) ([]string, time.Time, bool) {
	return n.SwarmNodeIDs(envID)
}
