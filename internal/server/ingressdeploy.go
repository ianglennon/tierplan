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
	Jobs     *jobs.Hub
	// Locks is the operation lock the stack routes use, so a stack operation and an ingress job exclude each other.
	Locks         *stackops.Locks
	Resolver      *registry.Resolver
	AddressSpaces *envs.AddressSpaces
	// Instance gives the ingress URL: the address of the configured agent or public URL, never a request's.
	Instance *instance.Instance
	// Pins are the pins of the server's certificate, as the agent install carries them.
	Pins func() []string
	// OwnChallenge reads the challenge answer of the server's own address; nil reads none.
	OwnChallenge func(ctx context.Context) (string, error)
	Clock        stackops.Clock
}

// newIngressDeploy builds the deployer from the server's parts. Stacks is left nil here: the deployer then lists no stacks
// to redeploy, until the stack side is wired.
func newIngressDeploy(d ingressDeployDeps) *ingressdeploy.Service {
	return ingressdeploy.New(ingressdeploy.Deps{
		Engine:        ingressEngine{agg: d.Aggregator, clusters: d.Clusters},
		Envs:          ingressEnvReader{st: d.Store, svc: d.Envs},
		Store:         d.Ingress,
		Notifier:      d.Notifier,
		Tokens:        d.Tokens,
		Jobs:          d.Jobs,
		Locks:         d.Locks,
		Audit:         d.Audit,
		Resolver:      d.Resolver,
		AddressSpaces: d.AddressSpaces,
		LocalCluster:  d.Envs.LocalSwarmClusterID,
		OwnChallenge:  d.OwnChallenge,
		ServerVersion: buildinfo.Get().Version,
		URL:           d.Instance.IngressEndpoint,
		Pins:          d.Pins,
		Clock:         d.Clock,
		Log:           d.Log,
	})
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
	info := ingressdeploy.EnvInfo{ID: envID, Name: row.Name, ClusterID: row.SwarmClusterID.String}
	if in.Kind != envs.KindAgent {
		// The local socket is one node, and its ID is the environment's.
		info.Swarm = in.Local.Probe.Type == dockerx.TypeSwarmManager
		info.ManagerConnected = info.Swarm
		if info.Swarm {
			info.Nodes = []ingressdeploy.NodeInfo{{AgentID: envID, Name: in.Local.Probe.Hostname, Role: "manager", Connected: true}}
		}
		return info, nil
	}
	info.Swarm = in.EnvType == "swarm"
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
	}
	return info, nil
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
