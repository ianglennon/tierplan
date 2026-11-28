// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// Engine is the way to a swarm's engines. envs.Aggregator (the writes and the node reads) and
// envs.Clusters (the cluster read) satisfy it together through a small adapter the server builds; a test
// satisfies it with one fake engine. Every write is one attempt and is never retried.
type Engine interface {
	// WriteOnManager runs fn once on the manager that answers for the environment, for a call that changes
	// something at the level of the cluster.
	WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error
	// WriteOnNode runs fn once on one node, named by its agent ID, for a call that changes something there.
	WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
	// ClusterRead runs a read-only fn on a manager, and may repeat it on another after a lost tunnel.
	ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error
	// OnNode runs a read-only fn on one node, named by its agent ID.
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// NodeInfo is one node of an environment as the deployer needs to see it.
type NodeInfo struct {
	// SwarmNodeID is the node's ID in the swarm; AgentID names the connection to it, "" when it has none.
	SwarmNodeID, AgentID, Name string
	// Role is "manager" or "worker".
	Role      string
	Connected bool
}

// EnvInfo is what the deployer needs to know of an environment before it touches it.
type EnvInfo struct {
	ID, Name string
	// Swarm is true for a swarm environment; ingress is available on any other only as a standalone host (Phase 25).
	// A standalone environment has exactly one node: its AgentID is the ID writes are routed by (the environment's own ID
	// for the local socket, the agent's ID otherwise) and its Connected says whether the host can be reached.
	Swarm bool
	// Local is true for TierPlan's own local-socket environment, so the host is TierPlan's own (D-04).
	Local bool
	// EngineID is the standalone host's engine ID, "" when it is not known.
	EngineID string
	// ClusterID is the swarm cluster the environment's agents report, "" when none has.
	ClusterID string
	// ManagerConnected is true when a manager node's agent is connected, so the cluster can be written to.
	ManagerConnected bool
	Nodes            []NodeInfo
}

// Environments reads the facts of an environment.
type Environments interface {
	// Info describes the environment. An environment that does not exist is an error.
	Info(ctx context.Context, envID string) (EnvInfo, error)
}

// StackRef is a stack of the environment that the ingress has to do with.
type StackRef struct {
	ID, Name string
	// HoldsClaims is true when the stack holds hostname claims.
	HoldsClaims bool
	// Attached is true when the stack's deployed services are on the ingress network already.
	Attached bool
}

// Stacks lists the stacks of an environment that hold routes. A nil Stacks lists none, so a preview and
// the preflight work before the stack side is wired.
type Stacks interface {
	// WithRoutes lists the environment's stacks that hold hostname claims.
	WithRoutes(ctx context.Context, envID string) ([]StackRef, error)
}

// Tokens is the ingress join token service: ingresshub.Service has these methods.
type Tokens interface {
	IssueToken(ctx context.Context, actor audit.Actor, envID, ip, ua string) (ingresshub.Token, error)
	DisableToken(ctx context.Context, actor audit.Actor, envID, ip, ua string) error
	TokenStatus(ctx context.Context, envID string) (ingresshub.TokenStatus, error)
}

var _ Tokens = (*ingresshub.Service)(nil)

// SystemDeployer deploys and removes the standalone host's proxy project: stackops.Deps has these methods.
type SystemDeployer interface {
	DeploySystem(ctx context.Context, in stackops.SystemDeployInput, r *jobs.Reporter) stackops.DeployResult
	DownSystem(ctx context.Context, envID, nodeID string) error
}

var _ SystemDeployer = stackops.Deps{}

// Kicker closes the live ingress sessions of an environment: *ingresshub.Hub has this method. Disable calls it once ingress
// is switched off and before the proxy service is removed, so no proxy is still being served.
type Kicker interface {
	KickEnv(envID, code string)
}

var _ Kicker = (*ingresshub.Hub)(nil)

// NodeRows removes the active ingress node rows of an environment, auditing each; revoked rows stay: *ingresshub.Service has this method.
type NodeRows interface {
	RemoveEnvNodes(ctx context.Context, actor audit.Actor, envID string) ([]ingresshub.Node, error)
}

var _ NodeRows = (*ingresshub.Service)(nil)

// NodeLister lists the ingress nodes of an environment: ingresshub.Service has this method.
type NodeLister interface {
	ListNodes(ctx context.Context, envID string) ([]ingresshub.Node, error)
}

var _ NodeLister = (*ingresshub.Service)(nil)

// Addressing hands out the subnet of the ingress network: envs.AddressSpaces satisfies it.
type Addressing interface {
	// LockAllocation takes the environment's allocation lock and returns the function that gives it back.
	LockAllocation(envID string) (unlock func())
	// AllocationInputs reads the pools and the occupied blocks a free subnet is chosen from.
	AllocationInputs(ctx context.Context, envID string, fresh bool) (envs.AllocationInputs, error)
}

var _ Addressing = (*envs.AddressSpaces)(nil)

// Registry chooses the registry credential an image is sent with: *registry.Resolver satisfies it. The
// header it returns is a credential and goes only into the service write.
type Registry interface {
	AuthFor(ctx context.Context, envID string, ref registry.Ref) (string, *registry.Candidate, error)
}

var _ Registry = (*registry.Resolver)(nil)

// ManagerPolicies reads where the privileged-services opt-in stands on every manager of a swarm: *envs.Clusters has this
// method. A nil one means the table is unread, so the gate refuses and the status says it could not read it.
type ManagerPolicies interface {
	ManagerPolicies(ctx context.Context, envID string) ([]envs.ManagerPolicyRow, error)
}

var _ ManagerPolicies = (*envs.Clusters)(nil)

// Deps are what the deployer reads and writes. A nil Stacks lists no stacks; every other field is required
// by the operation that uses it.
type Deps struct {
	Engine Engine
	Envs   Environments
	Stacks Stacks
	Store  *ingressstore.Store
	Tokens Tokens
	// Nodes lists the enrolled proxy nodes, for the status; nil reads none.
	Nodes NodeLister
	// Managers reads the managers' privileged-services opt-in for the virtual IP's status and gate; nil leaves the table
	// unread.
	Managers ManagerPolicies
	// CurrentBundle is the environment's current ingress bundle ID and whether it has one, for the status; nil
	// reads none, so no node counts as ready.
	CurrentBundle func(envID string) (string, bool)
	// Live says whether a proxy node has a live session now (ingresshub.Hub.Live). A node that is not live is not ready,
	// whatever health it last reported. Nil does not check.
	Live func(nodeID string) bool
	// Kicker closes an environment's proxy sessions when ingress is switched off; nil closes none.
	Kicker Kicker
	// NodeRows removes the environment's proxy node rows at the end of a switch-off; nil removes none.
	NodeRows NodeRows
	// Events delivers the health stream's live events (the agent.connected events wake the wipe loop) and the function that
	// ends the subscription; nil leaves the wipe loop to its tick.
	Events func(ctx context.Context) (<-chan envs.HealthEvent, func())
	Jobs   *jobs.Hub
	Locks  stackops.OperationLock
	Audit  *audit.Writer
	// Notifier is the ingress store's notifier: the ensure loop takes a pass whenever it reports a change. Nil
	// leaves the loop to its tick and to Wake.
	Notifier *ingress.Notifier
	// System deploys and removes the standalone proxy's compose project. It is required on a standalone environment; the
	// server gives it a stackops.Deps with the aggregator, the address spaces and the registry resolver.
	System SystemDeployer
	// Resolver chooses the registry credential of the proxy image; nil sends none. Give it only a non-nil
	// *registry.Resolver, since a nil one inside the interface is not nil.
	Resolver Registry
	// AddressSpaces chooses the ingress network's subnet.
	AddressSpaces Addressing
	// AllocationWait is how the read-back of the new network waits on the swarm.
	AllocationWait dockerx.AllocationWait
	// SelfReleaseLockWait bounds how long the switch-off of TierPlan's own route waits for the environment's operation lock
	// before it releases the route without it (D-11, WR-04). Zero is DefaultSelfReleaseLockWait.
	SelfReleaseLockWait time.Duration
	// LocalSwarm is the swarm membership of the server's own engine (envs.Service.LocalSwarmMembership): the cluster for a
	// manager, the node for a manager or a worker, and whether it can be told at all. The preflight compares it with the
	// environment's and says so when it cannot tell.
	LocalSwarm func(ctx context.Context) (envs.SwarmMembership, error)
	// OwnHTTP01InUse is whether TierPlan's own certificate really renews over HTTP-01 (acme.HTTP01InUse), for the HTTP-01
	// preflight checks (A-04): only an active Let's Encrypt configuration using HTTP-01 counts, never a self-signed install
	// whose challenge merely reads HTTP-01 by default. Nil skips the checks; an error makes them warn.
	OwnHTTP01InUse func(ctx context.Context) (bool, error)
	// Self reads TierPlan's own container on the local engine: its swarm service, stack and networks (D-01, D-03). Nil answers
	// unknown, so SelfLocation refuses rather than guesses.
	Self func(ctx context.Context) (SelfFacts, error)
	// SelfURLs reads the instance's own addresses, their environment locks and TIERPLAN_LISTEN's port, for the route's
	// backend (D-07); nil means none are set.
	SelfURLs func() SelfURLs
	// SelfConnect connects TierPlan's own container to the self network on the local engine, under the given alias, and
	// SelfDisconnect takes it off again. They are for a standalone host only (D-04): on a swarm TierPlan's service joins the
	// network through its own stack file and is never updated by TierPlan (D-03). Nil means TierPlan cannot connect itself
	// (no local socket), so the standalone pass reports TierPlan as not attached and never tries.
	SelfConnect    func(ctx context.Context, networkID, alias string) error
	SelfDisconnect func(ctx context.Context, networkID string) error
	// HostCertificate is the ingress certificate manager's view of one hostname of an environment, for the self route's
	// preview (D-09). Nil means unknown, which the preview shows as a warning and never as a valid certificate.
	HostCertificate func(ctx context.Context, envID, host string) (HostCertState, error)
	// ServerVersion decides the proxy image's tag (IngressImage).
	ServerVersion string
	// URL is the full address of the ingress channel the proxy dials, wss://host:port/ingress/connect.
	URL func() string
	// Pins are the pins of the server's certificate, empty when the certificate is one the system trusts.
	Pins  func() []string
	Clock stackops.Clock
	Log   *slog.Logger
}

// HostCertState is what the ingress certificate manager knows of one hostname's certificate (Deps.HostCertificate).
type HostCertState struct {
	// Valid is a certificate for the hostname that is in force now.
	Valid bool
	// Known is false when the manager holds nothing about the hostname yet: no certificate has been ordered.
	Known bool
	// TermsNeeded is the CA's terms not accepted, and Paused certificate work paused, for the environment: no certificate
	// is ordered while either holds.
	TermsNeeded, Paused bool
	// Reason is the manager's short code for a certificate that is not valid, "" for none.
	Reason string
}

// Service deploys the objects of an environment's ingress.
type Service struct {
	d Deps
	// wake asks the ensure loop for a pass soon; it holds at most one.
	wake chan struct{}
	// roundHook, when set, is called after every round of the ensure loop. Only a test sets it (export_test.go).
	roundHook func()
	// wipeHook, when set, is called after every pass of the wipe loop. Only a test sets it (export_test.go).
	wipeHook func()
	// written is, by environment, the proxy spec last written to its service, so a rolled-back update is not written again
	// every pass (WR-07); heldMu guards it.
	heldMu  sync.Mutex
	written map[string]updateMemo
	// outstanding is, by environment, the join token the ensure loop made for a standalone redeploy that has not
	// succeeded yet, so the next pass gives it again rather than superseding it for as long as the fault stands (WR-A2).
	// It holds a clear text, in memory only; outstandingMu guards it.
	outstandingMu sync.Mutex
	outstanding   map[string]StandaloneToken
	// vip is what the virtual IP reader knows of every environment's keepalived helpers, from their logs (D-21).
	vip *vipState
	// vipHook, when set, is called after every round of the virtual IP reader. Only a test sets it (export_test.go).
	vipHook func()
	// vipNodeTimeout and vipPassTimeout bound one node's read and one environment's pass of the virtual IP reader. A test
	// shortens them (export_test.go).
	vipNodeTimeout, vipPassTimeout time.Duration
	// selfObs is, by environment, the last SelfObservation of its self network, and selfCb the function each one is handed
	// to (OnSelfObserved); selfMu guards both.
	selfMu  sync.Mutex
	selfObs map[string]SelfObservation
	selfCb  func(SelfObservation)
	// selfSeq counts, by environment, the observations published since start, and selfTick is closed by the next one (and
	// then made again by a waiter), so a switch job can wait for an observation newer than its write; selfMu guards both.
	selfSeq  map[string]uint64
	selfTick chan struct{}
	// selfTickets is, by environment, the tickets handed out to reads of the self network (a counter that starts at 1, taken
	// before the read), and selfPublished the ticket of the observation now in selfObs; selfMu guards both. A pass publishes
	// after it has released the operation lock, so without this order a read that began before another pass's publish (a
	// switch-off's or a loop pass's removal) could put back a stale Exists:true over it.
	selfTickets, selfPublished map[string]uint64
	// selfWaitBound, when set, replaces the bound of a self route job's wait for the next observation: it is called once as
	// the wait starts and its channel ends the wait. Only a test sets it (export_test.go).
	selfWaitBound func() <-chan time.Time
}

// New builds the deployer.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = stackops.RealClock()
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		d: d, wake: make(chan struct{}, 1), vip: newVIPState(),
		vipNodeTimeout: vipNodeReadTimeout, vipPassTimeout: vipPassReadTimeout,
	}
}

func (s *Service) log() *slog.Logger { return s.d.Log }

// url and pins read the proxy's connection facts, empty when the server has not set them.
func (s *Service) url() string {
	if s.d.URL == nil {
		return ""
	}
	return s.d.URL()
}

func (s *Service) pins() []string {
	if s.d.Pins == nil {
		return nil
	}
	return s.d.Pins()
}
