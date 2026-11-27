// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
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
	// OwnHTTP01InUse is whether the server's own certificate really renews over HTTP-01 (acme.HTTP01InUse); nil skips the
	// HTTP-01 preflight checks.
	OwnHTTP01InUse func(ctx context.Context) (bool, error)
	// Self reads TierPlan's own container on the local engine (readSelfFacts on the server's socket); nil answers unknown.
	Self func(ctx context.Context) (ingressdeploy.SelfFacts, error)
	// SelfURLs reads the instance's own addresses and port for TierPlan's own route; nil means none are set.
	SelfURLs func() ingressdeploy.SelfURLs
	// SocketPath and SelfID are the local socket and TierPlan's own container ID, from self-detection and never from a
	// request: SelfConnect and SelfDisconnect act on that container alone, and are nil when either is not known (D-04).
	SocketPath, SelfID string
	// IngressCerts is the ingress certificate store, read for one hostname's certificate state (D-09); nil answers unknown.
	IngressCerts hostCertReader
	Clock        stackops.Clock
}

// hubLive is the hub's Live, or nil (no check) without a hub.
func hubLive(h *ingresshub.Hub) func(nodeID string) bool {
	if h == nil {
		return nil
	}
	return h.Live
}

// newIngressDeploy builds the deployer from the server's parts. A nil Stacks leaves the deployer listing no stacks to
// redeploy.
func newIngressDeploy(d ingressDeployDeps) *ingressdeploy.Service {
	return ingressdeploy.New(ingressDeployOf(d))
}

// ingressLoop is one loop the deployer runs for as long as the server does.
type ingressLoop struct {
	// Name says what the loop does, for a log line and a test.
	Name string
	Run  func(ctx context.Context)
}

// ingressDeployLoops are the loops the server starts for the deployer, each until its context ends: the ensure loop, the
// wipe of a certificate cache on a node that was away at the switch-off, and the reader of the virtual IP's moves (D-21).
func ingressDeployLoops(svc *ingressdeploy.Service) []ingressLoop {
	return []ingressLoop{
		{Name: "ensure", Run: svc.Run},
		{Name: "wipes", Run: svc.RunWipes},
		{Name: "vip_reader", Run: svc.RunVIPReader},
	}
}

// ingressDeployOf maps the server's parts to the deployer's dependencies. Its Managers is the cluster reader, whose manager
// table the virtual IP's gate and status read (D-19).
func ingressDeployOf(d ingressDeployDeps) ingressdeploy.Deps {
	var events func(ctx context.Context) (<-chan envs.HealthEvent, func())
	if d.Broker != nil {
		events = func(ctx context.Context) (<-chan envs.HealthEvent, func()) {
			// Only what happens from now on wakes the loop; the loop's first pass reads the whole queue anyway.
			_, ch, cancel := d.Broker.Subscribe(ctx)
			return ch, cancel
		}
	}
	// A nil cluster reader leaves the interface nil, not holding a nil pointer, so the deployer sees no table to read.
	var managers ingressdeploy.ManagerPolicies
	if d.Clusters != nil {
		managers = d.Clusters
	}
	connect, disconnect := selfConnector(d.SocketPath, d.SelfID)
	return ingressdeploy.Deps{
		Engine:         ingressEngine{agg: d.Aggregator, clusters: d.Clusters},
		Envs:           ingressEnvReader{st: d.Store, svc: d.Envs},
		Store:          d.Ingress,
		Notifier:       d.Notifier,
		Tokens:         d.Tokens,
		NodeRows:       d.Tokens,
		Kicker:         d.Hub,
		Events:         events,
		Nodes:          d.Tokens,
		Stacks:         d.Stacks,
		CurrentBundle:  d.CurrentBundle,
		Live:           hubLive(d.Hub),
		Jobs:           d.Jobs,
		Locks:          d.Locks,
		Audit:          d.Audit,
		Resolver:       d.Resolver,
		AddressSpaces:  d.AddressSpaces,
		System:         ingressSystem(d),
		LocalSwarm:     d.Envs.LocalSwarmMembership,
		OwnHTTP01InUse: d.OwnHTTP01InUse,
		Self:           d.Self,
		SelfURLs:       d.SelfURLs,
		SelfConnect:    connect,
		SelfDisconnect: disconnect,
		HostCertificate: hostCertificate(d.IngressCerts, func() time.Time {
			if d.Clock != nil {
				return d.Clock.Now()
			}
			return time.Now()
		}),
		ServerVersion: buildinfo.Get().Version,
		URL:           d.Instance.IngressEndpoint,
		Pins:          d.Pins,
		Clock:         d.Clock,
		Log:           d.Log,
		Managers:      managers,
	}
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

// ownHTTP01InUse is whether the server's own certificate really renews over HTTP-01, read from the ACME manager's status
// through acme.HTTP01InUse (A-04): a self-signed install whose challenge type merely defaults to HTTP-01 is not.
func ownHTTP01InUse(mgr *acme.Manager) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		st, err := mgr.Status(ctx)
		if err != nil {
			return false, err
		}
		return acme.HTTP01InUse(st), nil
	}
}

// selfURLSource is the part of the instance TierPlan's own route reads: its two URLs and whether each is fixed by its
// environment variable (*instance.Instance).
type selfURLSource interface {
	PublicURL() string
	AgentURL() string
	Locked() bool
	AgentURLLocked() bool
}

// instanceSelfURLs reads the instance's own addresses, their locks and TIERPLAN_LISTEN's port, for the deployer's
// Deps.SelfURLs and the self-route holder (D-07). The URLs are read on every call, so a settings change applies at once;
// the port is fixed for the life of the process.
func instanceSelfURLs(inst selfURLSource, listen string) func() ingressdeploy.SelfURLs {
	port := listenPort(listen)
	return func() ingressdeploy.SelfURLs {
		return ingressdeploy.SelfURLs{
			PublicURL: inst.PublicURL(), AgentURL: inst.AgentURL(),
			PublicLocked: inst.Locked(), AgentLocked: inst.AgentURLLocked(),
			ListenPort: port,
		}
	}
}

// selfConnectTimeout bounds one connect or disconnect of TierPlan's own container on the local engine.
const selfConnectTimeout = 10 * time.Second

// selfConnector is the deployer's Deps.SelfConnect and Deps.SelfDisconnect on a standalone host (D-04): TierPlan's own
// container, named by self-detection and never by the caller (T-28-57), connected to the self network under the alias or
// taken off it, through the local socket as readSelfFacts reads it. Both are nil when the socket or the container is not
// known, so the deployer reports TierPlan as not attached and never tries.
func selfConnector(socketPath, selfID string) (connect func(ctx context.Context, networkID, alias string) error, disconnect func(ctx context.Context, networkID string) error) {
	if socketPath == "" || selfID == "" {
		return nil, nil
	}
	connect = func(ctx context.Context, networkID, alias string) error {
		return onLocalEngine(ctx, socketPath, func(ctx context.Context, cli *dockerx.Client) error {
			return cli.NetworkConnect(ctx, networkID, selfID, dockerx.EndpointSpec{Alias: alias})
		})
	}
	disconnect = func(ctx context.Context, networkID string) error {
		return onLocalEngine(ctx, socketPath, func(ctx context.Context, cli *dockerx.Client) error {
			return cli.NetworkDisconnect(ctx, networkID, selfID)
		})
	}
	return connect, disconnect
}

// onLocalEngine runs fn against the local engine, within selfConnectTimeout.
func onLocalEngine(ctx context.Context, socketPath string, fn func(ctx context.Context, cli *dockerx.Client) error) error {
	cli, err := dockerx.New(engine.LocalSocketTransport{Path: socketPath})
	if err != nil {
		return fmt.Errorf("ask the local engine: %w", err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(ctx, selfConnectTimeout)
	defer cancel()
	return fn(ctx, cli)
}

// hostCertReader is the part of the ingress certificate store the self route's certificate item reads (*ingresscert.Store).
type hostCertReader interface {
	LiveCert(ctx context.Context, hostname string) (ingresscert.Cert, bool, error)
	State(ctx context.Context) (ingresscert.State, error)
}

// hostCertificate is the deployer's Deps.HostCertificate (D-09): the live certificate of the hostname in the environment,
// as the Certificates tab reads it (ingresscert.StateOf), with the install-wide terms and pause. A certificate is valid when
// it may be served now; a hostname whose live certificate belongs to another environment is unknown here. A nil store is
// nil, which the preview shows as pending.
func hostCertificate(r hostCertReader, now func() time.Time) func(ctx context.Context, envID, host string) (ingressdeploy.HostCertState, error) {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, envID, host string) (ingressdeploy.HostCertState, error) {
		st, err := r.State(ctx)
		if err != nil {
			return ingressdeploy.HostCertState{}, err
		}
		out := ingressdeploy.HostCertState{TermsNeeded: st.TermsAcceptedAt.IsZero(), Paused: st.PausedReason != ""}
		c, found, err := r.LiveCert(ctx, host)
		if err != nil {
			return ingressdeploy.HostCertState{}, err
		}
		if !found || c.EnvID != envID {
			return out, nil
		}
		out.Known = true
		switch state := ingresscert.StateOf(c, now()); {
		case c.Ready && (state == ingresscert.StateValid || state == ingresscert.StateUploaded || state == ingresscert.StateRenewalFailed):
			out.Valid = true
		default:
			out.Reason = state
		}
		return out, nil
	}
}
