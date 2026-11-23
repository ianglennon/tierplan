// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ingressDeployDeps are what newIngressDeploy reads from the rest of the server.
type ingressDeployDeps struct {
	Log        *slog.Logger
	Store      *store.Store
	Audit      *audit.Writer
	Aggregator *envs.Aggregator
	Clusters   *envs.Clusters
	Envs       *envs.Service
	Ingress    *ingressstore.Store
	// Notifier wakes the ensure loop on every change to the ingress state.
	Notifier *ingress.Notifier
	Tokens   *ingresshub.Service
	// Hub closes an environment's proxy sessions when its ingress is switched off.
	Hub *ingresshub.Hub
	// Broker's agent.connected events wake the loop that wipes the certificate cache of a node that was away at the switch-off.
	Broker *envs.Broker
	Jobs   *jobs.Hub
	// Locks is the operation lock the stack routes use, so a stack operation and an ingress job exclude each other.
	Locks         *stackops.Locks
	Resolver      *registry.Resolver
	AddressSpaces *envs.AddressSpaces
	// Instance gives the ingress URL: the address of the configured agent or public URL, never a request's.
	Instance *instance.Instance
	// Pins are the pins of the server's certificate, as the agent install carries them.
	Pins func() []string
	// Stacks lists the stacks to redeploy; nil lists none.
	Stacks ingressdeploy.Stacks
	// CurrentBundle is the reconciler's current bundle ID of an environment, for the status.
	CurrentBundle func(envID string) (string, bool)
	// OwnChallenge reads the challenge answer of the server's own address; nil reads none.
	OwnChallenge func(ctx context.Context) (string, error)
	Clock        stackops.Clock
}

// newIngressDeploy builds the deployer from the server's parts. A nil Stacks leaves the deployer listing no stacks to
// redeploy.
func newIngressDeploy(d ingressDeployDeps) *ingressdeploy.Service {
	var events func(ctx context.Context) (<-chan envs.HealthEvent, func())
	if d.Broker != nil {
		events = func(ctx context.Context) (<-chan envs.HealthEvent, func()) {
			// Only what happens from now on wakes the loop; the loop's first pass reads the whole queue anyway.
			_, ch, cancel := d.Broker.Subscribe(ctx)
			return ch, cancel
		}
	}
	return ingressdeploy.New(ingressdeploy.Deps{
		Engine:        ingressEngine{agg: d.Aggregator, clusters: d.Clusters},
		Envs:          ingressEnvReader{st: d.Store, svc: d.Envs},
		Store:         d.Ingress,
		Notifier:      d.Notifier,
		Tokens:        d.Tokens,
		NodeRows:      d.Tokens,
		Kicker:        d.Hub,
		Events:        events,
		Nodes:         d.Tokens,
		Stacks:        d.Stacks,
		CurrentBundle: d.CurrentBundle,
		Jobs:          d.Jobs,
		Locks:         d.Locks,
		Audit:         d.Audit,
		Resolver:      d.Resolver,
		AddressSpaces: d.AddressSpaces,
		System:        ingressSystem(d),
		LocalSwarm:    d.Envs.LocalSwarmMembership,
		OwnChallenge:  d.OwnChallenge,
		ServerVersion: buildinfo.Get().Version,
		URL:           d.Instance.IngressEndpoint,
		Pins:          d.Pins,
		Clock:         d.Clock,
		Log:           d.Log,
	})
}

// ingressSystem is the deployer of a standalone host's proxy project: the stack pipeline's own system deploy, on the server's
// aggregator. It has no Ingress, so the project is never attached to the ingress network or given a hostname claim.
func ingressSystem(d ingressDeployDeps) stackops.Deps {
	return stackops.Deps{Aggregator: d.Aggregator, AddressSpaces: d.AddressSpaces, Resolver: d.Resolver, Log: d.Log}
}

// ingressEngine reaches a swarm's engines: the writes and the node reads through the aggregator, the cluster read
// through the cluster reader.
type ingressEngine struct {
	agg      *envs.Aggregator
	clusters *envs.Clusters
}

func (e ingressEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	return e.agg.WriteOnManager(ctx, envID, fn)
}

func (e ingressEngine) WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	return e.agg.WriteOnNode(ctx, envID, nodeID, fn)
}

func (e ingressEngine) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	return e.clusters.ClusterRead(ctx, envID, fn)
}

func (e ingressEngine) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	return e.agg.OnNode(ctx, envID, nodeID, fn)
}

// ingressEnvReader reads the facts of an environment the deployer decides on: whether it is a swarm, its cluster, whether a
// manager is connected and which nodes it has.
type ingressEnvReader struct {
	st  *store.Store
	svc *envs.Service
}

// Info describes the environment. An environment that does not exist is envs.ErrNotFound.
func (r ingressEnvReader) Info(ctx context.Context, envID string) (ingressdeploy.EnvInfo, error) {
	row, err := r.st.RQ.GetEnvironment(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return ingressdeploy.EnvInfo{}, envs.ErrNotFound
	}
	if err != nil {
		return ingressdeploy.EnvInfo{}, err
	}
	in, err := r.svc.Input(ctx, envID, time.Now())
	if err != nil {
		return ingressdeploy.EnvInfo{}, err
	}
	return envInfoFrom(envID, row, in), nil
}

// envInfoFrom maps an environment row and its rollup input to what the deployer decides on. A standalone environment is
// one node: for the local socket its AgentID is the environment's own ID, which stackops.Deps.StandaloneNode routes
// writes by, and its reachability is the local probe's; for an agent host it is the host's one agent.
func envInfoFrom(envID string, row sqlc.Environment, in envs.RollupInput) ingressdeploy.EnvInfo {
	info := ingressdeploy.EnvInfo{ID: envID, Name: row.Name, ClusterID: row.SwarmClusterID.String}
	if in.Kind != envs.KindAgent {
		// The local socket is one node, and its ID is the environment's.
		probe := in.Local.Probe
		switch probe.Type {
		case dockerx.TypeSwarmManager:
			info.Swarm = true
			info.ManagerConnected = true
			info.Nodes = []ingressdeploy.NodeInfo{{SwarmNodeID: probe.SwarmNodeID, AgentID: envID, Name: probe.Hostname, Role: "manager", Connected: true}}
			return info
		case dockerx.TypeSwarmWorker:
			// A worker is a swarm node, as envs.Clusters.IsSwarm has it: its stacks are swarm stacks, so it is not a standalone
			// host for ingress, and the swarm path refuses it for want of a manager (WR-A4).
			info.Swarm = true
			info.Nodes = []ingressdeploy.NodeInfo{{SwarmNodeID: probe.SwarmNodeID, AgentID: envID, Name: probe.Hostname, Role: "worker", Connected: in.Local.State == envs.StateOnline}}
			return info
		}
		info.Local = true
		info.EngineID = probe.EngineID
		// An engine that has not been probed yet could be a swarm node, so it is not a connected standalone host.
		info.Nodes = []ingressdeploy.NodeInfo{{AgentID: envID, Name: probe.Hostname, Connected: probe.Type != "" && in.Local.State == envs.StateOnline}}
		return info
	}
	info.Swarm = in.EnvType == "swarm"
	if !info.Swarm {
		info.EngineID = row.EngineID.String
	}
	for _, ag := range in.Agents {
		if ag.Revoked {
			continue
		}
		info.Nodes = append(info.Nodes, ingressdeploy.NodeInfo{
			SwarmNodeID: ag.SwarmNodeID, AgentID: ag.ID, Name: ag.Node, Role: ag.Role, Connected: ag.Connected,
		})
		if ag.Role == "manager" && ag.Connected {
			info.ManagerConnected = true
		}
		if !info.Swarm {
			break // a standalone host has the one agent
		}
	}
	return info
}

// ownChallenge is the validation method the server's own certificate is ordered with now, read from the ACME manager.
func ownChallenge(mgr *acme.Manager) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		st, err := mgr.Status(ctx)
		if err != nil {
			return "", err
		}
		return st.ChallengeType, nil
	}
}
