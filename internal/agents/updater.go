// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package agents

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/proto/wire"
)

// updateActor is the principal an automatic update is audited under (D-13).
const updateActor = "system:agent-update"

// Update states an agent can be in, as the node list reports them.
const (
	// UpdateIdle means no update is running and none failed that is still unresolved.
	UpdateIdle = "idle"
	// UpdateUpdating means an update was sent and the agent has not yet come back
	// with the new version.
	UpdateUpdating = "updating"
	// UpdateFailed means the last update did not finish; the reason says why.
	UpdateFailed = "failed"
)

// Reasons the updater itself gives for a failed update. An agent's own refusal is
// reported with the code it sent (wire.UpdateErr*).
const (
	// ReasonNoAnswer means the agent never answered the update. Agents older than
	// the self-update message ignore it, and so does one that lost its connection.
	ReasonNoAnswer = "no_answer"
	// ReasonNoNewVersion means the agent accepted the update and never came back
	// running the new version in time.
	ReasonNoNewVersion = "no_new_version"
	// ReasonRolledBack means a standalone agent came back on its old version after it
	// accepted the update: the helper put it back because the new one did not connect.
	ReasonRolledBack = "rolled_back"
)

// Errors of updating agents by hand.
var (
	// ErrAgentOffline means the agent, or for a swarm every agent, has no live connection.
	ErrAgentOffline = errors.New("agents: the agent is not connected")
	// ErrNoManager means a swarm has no connected manager agent to send the update through.
	ErrNoManager = errors.New("agents: no manager agent of the swarm is connected")
	// ErrSelfUpdateOff means the host owner set COAMING_AGENT_SELF_UPDATE=false.
	ErrSelfUpdateOff = errors.New("agents: the host does not let the agent update itself")
	// ErrAgentUpToDate means the agent already runs this server's version.
	ErrAgentUpToDate = errors.New("agents: the agent is already up to date")
	// ErrUpdateInProgress means an update of this agent, or of this swarm's agent
	// service, is running.
	ErrUpdateInProgress = errors.New("agents: an update is already running")
)

const (
	// defaultAnswerTimeout is how long an agent has to answer an update before it is
	// taken not to understand it.
	defaultAnswerTimeout = 20 * time.Second
	// defaultStandaloneConfirm is how long a standalone agent has, after accepting an
	// update, to come back on the new version: the helper waits 90 s for the new
	// container to connect and the engine takes up to 10 s to stop the old one.
	defaultStandaloneConfirm = 150 * time.Second
	// defaultSwarmConfirm is how long a swarm has to roll its agent service over.
	// Swarm replaces one node at a time with the delay the install set.
	defaultSwarmConfirm = 15 * time.Minute
	// updateIDBytes is the random part of a request ID, which the agent echoes.
	updateIDBytes = 12
	// auditTimeout bounds one audit write made off a timer or a callback.
	auditTimeout = 10 * time.Second
)

// minSelfUpdate is the first release whose agent understands the update message:
// v0.1.0, the first release there is. An agent older than this would ignore it.
var minSelfUpdate = [3]int{0, 1, 0}

// Agent is what the updater needs to know of a connected agent.
type Agent struct {
	ID, EnvID string
	// Node is the host name the agent reported.
	Node string
	// Version is the agent version from its handshake.
	Version string
	// Role is manager, worker or standalone.
	Role string
	// SelfUpdate is whether the host owner lets the agent update itself.
	SelfUpdate bool
}

// AgentOf describes a live session for the updater.
func AgentOf(s *tunnel.Session) Agent {
	return Agent{
		ID: s.AgentID, EnvID: s.EnvID, Node: s.Facts.Hostname, Version: s.AgentVersion,
		Role: roleOf(s.Facts), SelfUpdate: s.Facts.Policy.SelfUpdate,
	}
}

// UpdateHub is the part of the tunnel hub the updater uses.
type UpdateHub interface {
	// SendUpdate writes an update to one agent's control stream.
	SendUpdate(agentID string, msg wire.Update) error
	// SendUninstall writes an uninstall to one agent's control stream.
	SendUninstall(agentID string, msg wire.Uninstall) error
	// Connected lists the agents of an environment that have a live session.
	Connected(envID string) []Agent
}

// HubSource adapts the tunnel hub to UpdateHub.
func HubSource(h *tunnel.Hub) UpdateHub { return hubSource{h} }

type hubSource struct{ h *tunnel.Hub }

func (s hubSource) SendUpdate(agentID string, msg wire.Update) error {
	return s.h.SendUpdate(agentID, msg)
}

func (s hubSource) SendUninstall(agentID string, msg wire.Uninstall) error {
	return s.h.SendUninstall(agentID, msg)
}

func (s hubSource) Connected(envID string) []Agent {
	var out []Agent
	for _, ss := range s.h.Sessions() {
		if ss.EnvID == envID {
			out = append(out, AgentOf(ss))
		}
	}
	return out
}

// UpdateState is what the node list shows of an agent's update.
type UpdateState struct {
	// State is UpdateIdle, UpdateUpdating or UpdateFailed.
	State string
	// Reason is why it failed, for UpdateFailed only.
	Reason string
}

// pendingUpdate is one update message that has been sent and not yet resolved. A
// swarm has one for its whole agent service, since one message rolls every node's
// agent; a standalone agent has its own.
type pendingUpdate struct {
	id, key, envID string
	via            Agent
	image          string
	target         [3]int
	targetOK       bool
	swarm          bool
	mode           string
	answered       bool
	// actor, ip and ua are who started it and from where, for the audit entry of
	// its failure.
	actor  audit.Actor
	ip, ua string
	// members are the agents whose state is updating, with the version each had when
	// the update was sent.
	members map[string]string
	timer   *time.Timer
}

// Updater keeps agents on the server's version (D-24, D-28). After a server upgrade
// it sends the frozen update message to each agent of a release build that is older
// than the server and inside the compatibility window, unless the environment's
// setting is off or the host refused self-update; admins can also update by hand.
// It tracks each update from the send until the agent reconnects with the new
// version, and records every send and every failure in the audit log.
//
// A swarm gets one message through one connected manager agent, because the agent
// updates its own service and the service rolls every node. A standalone agent is
// sent its own. A dev or edge server never updates anybody on its own (RESEARCH
// Pitfall 23). It is safe for concurrent use.
type Updater struct {
	svc     *Service
	hub     UpdateHub
	manager func(envID string) (agentID string, ok bool)
	version string
	now     func() time.Time
	log     *slog.Logger

	answerFor, standaloneFor, swarmFor time.Duration

	mu     sync.Mutex
	closed bool
	// pend is the updates in flight by key: "env:<id>" for a swarm, "agent:<id>" for a
	// standalone agent. member maps each agent whose state is updating to its update.
	pend   map[string]*pendingUpdate
	member map[string]*pendingUpdate
	// failed is why the last update of each agent failed, until it is resolved: the
	// agent runs the server's version, or an admin starts another update.
	failed map[string]string
	// tried is the image an automatic update was last started for, by key, so that
	// a release that breaks does not loop: it is not started again by itself.
	tried map[string]string
	// un is the uninstalls that were sent and not yet answered.
	un map[uninstallKey]*uninstallWait
}

var _ tunnel.UpdateObserver = (*Updater)(nil)

// NewUpdater returns an updater for a server of serverVersion. clusterManager says
// which agent runs on the leading manager of a swarm (empty or not ok when none is
// known); it must not do I/O. now defaults to time.Now.
func NewUpdater(svc *Service, hub UpdateHub, clusterManager func(envID string) (agentID string, ok bool), serverVersion string, now func() time.Time) *Updater {
	if now == nil {
		now = time.Now
	}
	if clusterManager == nil {
		clusterManager = func(string) (string, bool) { return "", false }
	}
	return &Updater{
		svc: svc, hub: hub, manager: clusterManager, version: serverVersion, now: now, log: slog.Default(),
		answerFor: defaultAnswerTimeout, standaloneFor: defaultStandaloneConfirm, swarmFor: defaultSwarmConfirm,
		pend: map[string]*pendingUpdate{}, member: map[string]*pendingUpdate{},
		failed: map[string]string{}, tried: map[string]string{}, un: map[uninstallKey]*uninstallWait{},
	}
}

// WithTimeouts sets how long an agent has to answer an update, and how long after
// it accepted one it has to come back on the new version, for a standalone host and
// for a swarm. A value of zero or less keeps the default. Call it before use.
func (u *Updater) WithTimeouts(answer, standalone, swarm time.Duration) *Updater {
	if answer > 0 {
		u.answerFor = answer
	}
	if standalone > 0 {
		u.standaloneFor = standalone
	}
	if swarm > 0 {
		u.swarmFor = swarm
	}
	return u
}

// WithLogger sets where failures that are not audited are logged. Call it before use.
func (u *Updater) WithLogger(l *slog.Logger) *Updater {
	if l != nil {
		u.log = l
	}
	return u
}

// Close stops the timers of the updates in flight. The updates themselves are not
// cancelled: they are the agents' to finish.
func (u *Updater) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closed = true
	for _, p := range u.pend {
		if p.timer != nil {
			p.timer.Stop()
		}
	}
}

// ControlOpened implements tunnel.UpdateObserver: an agent whose control stream is
// open can be written to, which is the moment a decision about it can be acted on.
func (u *Updater) ControlOpened(s *tunnel.Session) {
	a := AgentOf(s)
	go u.OnConnected(a)
}

// UpdateResult implements tunnel.UpdateObserver.
func (u *Updater) UpdateResult(s *tunnel.Session, res wire.UpdateResult) {
	id := s.AgentID
	go u.HandleResult(id, res)
}

// ----- the rules -----

// target is the image this server wants its agents on, and its version when that is
// a release.
func (u *Updater) target() (image string, version [3]int, ok bool) {
	image = AgentImage(u.version)
	if v, vok := tunnel.ParseVersion(u.version); vok && !strings.HasSuffix(image, ":edge") {
		return image, v, true
	}
	return image, [3]int{}, false
}

// releaseServer is whether the server is a release build, the only kind that
// updates agents by itself.
func (u *Updater) releaseServer() bool {
	_, ok := tunnel.ParseVersion(u.version)
	return ok
}

// autoNeeds is whether the automatic rules would update an agent of this version: a
// release older than a release server, inside its window, new enough to understand
// the update message.
func (u *Updater) autoNeeds(version string) bool {
	if !u.releaseServer() || !tunnel.Behind(u.version, version) {
		return false
	}
	if ok, _, _ := tunnel.Compatible(u.version, version, wire.ProtoVersion); !ok {
		return false
	}
	v, ok := tunnel.ParseVersion(version)
	return ok && !versionLess(v, minSelfUpdate)
}

// NeedsUpdate is whether an admin would be offered an update for an agent of this
// version: older than a release server, or not a release at all on either side (a
// dev build cannot be compared, so the admin is trusted to know).
func (u *Updater) NeedsUpdate(version string) bool { return NeedsUpdate(u.version, version) }

// NeedsUpdate is Updater.NeedsUpdate for a server of serverVersion.
func NeedsUpdate(serverVersion, agentVersion string) bool {
	_, sok := tunnel.ParseVersion(serverVersion)
	_, aok := tunnel.ParseVersion(agentVersion)
	if sok && aok {
		return tunnel.Behind(serverVersion, agentVersion)
	}
	return agentVersion != ""
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// reached is whether an agent of version has the version p was sent to bring it to.
// For a target that is not a release (edge) any reconnect after the agent accepted
// the update counts, since the agent's version cannot be compared with it.
func (p *pendingUpdate) reached(version string) bool {
	if !p.targetOK {
		return p.answered
	}
	v, ok := tunnel.ParseVersion(version)
	return ok && !versionLess(v, p.target)
}

// ----- observing agents -----

// OnConnected is told that an agent has a control stream open: it resolves an update
// the agent was part of, and then applies the automatic rules to its environment.
func (u *Updater) OnConnected(a Agent) {
	u.observe(a)
	u.autoUpdate(a.EnvID)
}

// observe settles what an agent's (re)connection says about the update it was in.
func (u *Updater) observe(a Agent) {
	var job func()
	u.mu.Lock()
	if p := u.member[a.ID]; p != nil {
		switch {
		case p.reached(a.Version):
			delete(p.members, a.ID)
			delete(u.member, a.ID)
			delete(u.failed, a.ID)
			if len(p.members) == 0 {
				u.finishLocked(p)
			}
		case !p.swarm && p.answered:
			// The old agent came back after it accepted: the helper rolled it back.
			job = u.failLocked(p, ReasonRolledBack)
		}
	} else if _, failed := u.failed[a.ID]; failed {
		// A failed update that turns out to have worked, or an update by hand.
		if _, v, ok := u.target(); ok {
			if av, aok := tunnel.ParseVersion(a.Version); aok && !versionLess(av, v) {
				delete(u.failed, a.ID)
			}
		}
	}
	u.mu.Unlock()
	if job != nil {
		job()
	}
}

// finishLocked ends an update that succeeded. u.mu is held.
func (u *Updater) finishLocked(p *pendingUpdate) {
	if p.timer != nil {
		p.timer.Stop()
	}
	if u.pend[p.key] == p {
		delete(u.pend, p.key)
	}
}

// failLocked ends an update that did not finish and returns the audit write, which
// the caller runs once u.mu is released. u.mu is held.
func (u *Updater) failLocked(p *pendingUpdate, reason string) func() {
	if p.timer != nil {
		p.timer.Stop()
	}
	if u.pend[p.key] == p {
		delete(u.pend, p.key)
	}
	for id := range p.members {
		delete(u.member, id)
		u.failed[id] = reason
	}
	p.members = map[string]string{}
	entry := audit.Entry{
		Actor: p.actor, Action: audit.ActionAgentUpdateFailed, TargetType: targetAgent, TargetID: p.via.ID,
		EnvID: p.envID, Result: audit.ResultFailure, Reason: reason, IP: p.ip, UserAgent: p.ua,
		Details: map[string]string{"node": p.via.Node, "from": p.via.Version, "image": p.image, "mode": p.mode},
	}
	return func() { u.record(entry) }
}

// ----- results and timers -----

// HandleResult takes an agent's answer to an update or an uninstall. An answer is
// matched to the update (or uninstall) in flight by the agent that sent it and the id
// it echoes; any other is ignored, so an agent cannot affect an update that is not its
// own.
func (u *Updater) HandleResult(agentID string, res wire.UpdateResult) {
	if u.deliverUninstall(agentID, res) {
		return
	}
	var job func()
	u.mu.Lock()
	var p *pendingUpdate
	for _, c := range u.pend {
		if c.via.ID == agentID && c.id == res.ID {
			p = c
			break
		}
	}
	switch {
	case p == nil:
	case res.OK:
		p.answered = true
		u.armLocked(p, u.confirmFor(p), ReasonNoNewVersion)
	default:
		job = u.failLocked(p, updateErrorCode(res.Error))
	}
	u.mu.Unlock()
	if job != nil {
		job()
	}
}

func (u *Updater) confirmFor(p *pendingUpdate) time.Duration {
	if p.swarm {
		return u.swarmFor
	}
	return u.standaloneFor
}

// armLocked (re)starts p's timer: when it runs and p is still the update in flight,
// the update fails with reason. u.mu is held.
func (u *Updater) armLocked(p *pendingUpdate, d time.Duration, reason string) {
	if p.timer != nil {
		p.timer.Stop()
	}
	if u.closed {
		return
	}
	p.timer = time.AfterFunc(d, func() { u.expire(p, reason) })
}

func (u *Updater) expire(p *pendingUpdate, reason string) {
	u.mu.Lock()
	if u.pend[p.key] != p || (reason == ReasonNoAnswer && p.answered) {
		u.mu.Unlock()
		return
	}
	job := u.failLocked(p, reason)
	u.mu.Unlock()
	job()
}

// updateErrorCode keeps an agent's error code when it looks like one, and otherwise
// says update_failed: the text is the agent's own and ends up in the audit log.
func updateErrorCode(s string) string {
	if s == "" || len(s) > 64 {
		return wire.UpdateErrFailed
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return wire.UpdateErrFailed
		}
	}
	return s
}

// State is what the node list shows of an agent's update.
func (u *Updater) State(agentID string) UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.member[agentID] != nil {
		return UpdateState{State: UpdateUpdating}
	}
	if reason, ok := u.failed[agentID]; ok {
		return UpdateState{State: UpdateFailed, Reason: reason}
	}
	return UpdateState{State: UpdateIdle}
}

// ----- starting updates -----

// newUpdateID is a random request id, which the agent echoes in its answer.
func newUpdateID() string {
	b := make([]byte, updateIDBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// start sends an update to via and tracks it. members are the agents whose state
// becomes updating: via itself for a standalone host, every agent of the swarm that
// is behind for a swarm. It returns ErrUpdateInProgress when one is already running
// under the same key, and an error wrapping ErrAgentOffline when the message could
// not be written, in which case nothing is left behind.
func (u *Updater) start(actor audit.Actor, ip, ua, mode string, via Agent, members []Agent, swarm bool) error {
	image, tv, tok := u.target()
	key := "agent:" + via.ID
	if swarm {
		key = "env:" + via.EnvID
	}
	p := &pendingUpdate{
		id: newUpdateID(), key: key, envID: via.EnvID, via: via, image: image, target: tv, targetOK: tok,
		swarm: swarm, mode: mode, actor: actor, ip: ip, ua: ua, members: map[string]string{},
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return ErrAgentOffline
	}
	if u.pend[key] != nil {
		u.mu.Unlock()
		return ErrUpdateInProgress
	}
	for _, m := range members {
		p.members[m.ID] = m.Version
		u.member[m.ID] = p
		delete(u.failed, m.ID)
	}
	u.pend[key] = p
	u.armLocked(p, u.answerFor, ReasonNoAnswer)
	u.mu.Unlock()

	if err := u.hub.SendUpdate(via.ID, wire.Update{T: wire.TypeUpdate, ID: p.id, Image: image}); err != nil {
		u.mu.Lock()
		if u.pend[key] == p {
			if p.timer != nil {
				p.timer.Stop()
			}
			delete(u.pend, key)
			for id := range p.members {
				delete(u.member, id)
			}
		}
		u.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrAgentOffline, err)
	}
	details := map[string]string{"node": via.Node, "from": via.Version, "image": image, "mode": mode}
	if swarm {
		details["agents"] = fmt.Sprint(len(members))
	}
	u.record(audit.Entry{
		Actor: actor, Action: audit.ActionAgentUpdated, TargetType: targetAgent, TargetID: via.ID,
		EnvID: via.EnvID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua, Details: details,
	})
	return nil
}

// record writes one audit entry. A failure to write is logged and nothing more: an
// update that has been sent cannot be taken back because its record was not kept.
func (u *Updater) record(e audit.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	if err := u.svc.aud.Record(ctx, e); err != nil {
		u.log.Warn("could not record an agent update", "action", e.Action, "agent_id", e.TargetID, "error", err)
	}
}

// pickManager chooses the connected manager agent a swarm's update goes through:
// the leader's when it is connected, else the connected manager with the lowest ID,
// so the choice is stable. A worker is never chosen (it answers not_a_manager).
func (u *Updater) pickManager(envID string, connected []Agent) (Agent, bool) {
	if id, ok := u.manager(envID); ok {
		for _, a := range connected {
			if a.ID == id && a.Role == "manager" {
				return a, true
			}
		}
	}
	var best *Agent
	for i := range connected {
		a := &connected[i]
		if a.Role == "manager" && (best == nil || a.ID < best.ID) {
			best = a
		}
	}
	if best == nil {
		return Agent{}, false
	}
	return *best, true
}

func isSwarm(connected []Agent) bool {
	for _, a := range connected {
		if a.Role != "standalone" {
			return true
		}
	}
	return false
}

// ----- the automatic pass -----

// Sweep applies the automatic rules to the connected agents of an environment. An
// agent that connects is handled by OnConnected; Sweep is for a change that makes
// agents eligible without one, such as an admin switching auto-update on.
func (u *Updater) Sweep(envID string) { u.autoUpdate(envID) }

// claim records that an automatic update to image is being started under key, and
// reports false when one already was: an automatic update is tried once per image.
func (u *Updater) claim(key, image string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tried[key] == image || u.pend[key] != nil {
		return false
	}
	u.tried[key] = image
	return true
}

func (u *Updater) unclaim(key string) {
	u.mu.Lock()
	delete(u.tried, key)
	u.mu.Unlock()
}

// autoUpdate sends what the automatic rules call for to an environment's connected
// agents. It reads the environment's setting from the database, so a switch an admin
// flipped applies at the next connection.
func (u *Updater) autoUpdate(envID string) {
	if !u.releaseServer() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	on, err := u.svc.AutoUpdate(ctx, envID)
	if err != nil {
		if !errors.Is(err, ErrEnvironmentNotFound) {
			u.log.Warn("could not read an environment's auto-update setting", "environment_id", envID, "error", err)
		}
		return
	}
	if !on {
		return
	}
	connected := u.hub.Connected(envID)
	var behind []Agent
	for _, a := range connected {
		if u.autoNeeds(a.Version) {
			behind = append(behind, a)
		}
	}
	if len(behind) == 0 {
		return
	}
	actor := rbac.System(updateActor).AuditActor()
	image, _, _ := u.target()
	if isSwarm(connected) {
		via, ok := u.pickManager(envID, connected)
		if !ok || !via.SelfUpdate {
			return
		}
		key := "env:" + envID
		if !u.claim(key, image) {
			return
		}
		if err := u.start(actor, "", "", "auto", via, behind, true); err != nil {
			u.unclaim(key)
			u.logSkipped(envID, via.ID, err)
		}
		return
	}
	for _, a := range behind {
		if !a.SelfUpdate {
			continue
		}
		key := "agent:" + a.ID
		if !u.claim(key, image) {
			continue
		}
		if err := u.start(actor, "", "", "auto", a, []Agent{a}, false); err != nil {
			u.unclaim(key)
			u.logSkipped(envID, a.ID, err)
		}
	}
}

func (u *Updater) logSkipped(envID, agentID string, err error) {
	if errors.Is(err, ErrUpdateInProgress) {
		return
	}
	u.log.Info("an automatic agent update was not sent; it is tried again when an agent connects",
		"environment_id", envID, "agent_id", agentID, "error", err)
}

// ----- updates by hand -----

// UpdateAgent updates one agent, on an admin's request. In a swarm the agent updates
// its own service, which rolls every node, so a request for any agent of a swarm
// sends one update through a connected manager and every agent that is behind shows
// updating. It works whatever the environment's auto-update setting is. It returns
// ErrAgentNotFound, ErrAlreadyRevoked, ErrAgentOffline, ErrAgentUpToDate,
// ErrSelfUpdateOff, ErrNoManager or ErrUpdateInProgress when it will not.
func (u *Updater) UpdateAgent(ctx context.Context, actor audit.Actor, envID, agentID, ip, ua string) error {
	if err := requireAgentEnvironment(ctx, u.svc.st.RQ, envID); err != nil {
		return err
	}
	row, err := agentOf(ctx, u.svc.st.RQ, envID, agentID)
	if err != nil {
		return err
	}
	if row.Status != "active" {
		return ErrAlreadyRevoked
	}
	connected := u.hub.Connected(envID)
	var a *Agent
	for i := range connected {
		if connected[i].ID == agentID {
			a = &connected[i]
		}
	}
	if a == nil {
		return ErrAgentOffline
	}
	if !u.NeedsUpdate(a.Version) {
		return ErrAgentUpToDate
	}
	if !isSwarm(connected) {
		if !a.SelfUpdate {
			return ErrSelfUpdateOff
		}
		return u.start(actor, ip, ua, "manual", *a, []Agent{*a}, false)
	}
	via, ok := u.pickManager(envID, connected)
	if !ok {
		return ErrNoManager
	}
	if !via.SelfUpdate {
		return ErrSelfUpdateOff
	}
	return u.start(actor, ip, ua, "manual", via, u.behind(connected), true)
}

// behind are the connected agents an admin's update would bring up to date.
func (u *Updater) behind(connected []Agent) []Agent {
	var out []Agent
	for _, a := range connected {
		if u.NeedsUpdate(a.Version) {
			out = append(out, a)
		}
	}
	return out
}

// UpdateAll updates every connected agent of an environment that is behind, and
// returns how many updates it started: one for a swarm, one per agent for standalone
// hosts. An agent whose host refuses self-update, one that is up to date and one that
// is already updating are skipped. It returns ErrAgentOffline when the environment
// has agents and none is connected, and ErrNoManager when a swarm has no connected
// manager.
func (u *Updater) UpdateAll(ctx context.Context, actor audit.Actor, envID, ip, ua string) (int, error) {
	if err := requireAgentEnvironment(ctx, u.svc.st.RQ, envID); err != nil {
		return 0, err
	}
	connected := u.hub.Connected(envID)
	if len(connected) == 0 {
		rows, err := u.svc.st.RQ.ListAgentsByEnv(ctx, envID)
		if err != nil {
			return 0, fmt.Errorf("list the agents: %w", err)
		}
		for _, r := range rows {
			if r.Status == "active" {
				return 0, ErrAgentOffline
			}
		}
		return 0, nil
	}
	behind := u.behind(connected)
	if len(behind) == 0 {
		return 0, nil
	}
	if isSwarm(connected) {
		via, ok := u.pickManager(envID, connected)
		if !ok {
			return 0, ErrNoManager
		}
		if !via.SelfUpdate {
			return 0, nil
		}
		if err := u.start(actor, ip, ua, "manual", via, behind, true); err != nil {
			if errors.Is(err, ErrUpdateInProgress) {
				return 0, nil
			}
			return 0, err
		}
		return 1, nil
	}
	sort.Slice(behind, func(i, j int) bool { return behind[i].ID < behind[j].ID })
	started := 0
	for _, a := range behind {
		if !a.SelfUpdate {
			continue
		}
		switch err := u.start(actor, ip, ua, "manual", a, []Agent{a}, false); {
		case err == nil:
			started++
		case errors.Is(err, ErrUpdateInProgress), errors.Is(err, ErrAgentOffline):
			// Already updating, or gone since the list was read: not this admin's concern.
		default:
			return started, err
		}
	}
	return started, nil
}

// ----- the environment setting -----

// AutoUpdate is whether Coaming updates the agents of an environment itself. It
// reports ErrEnvironmentNotFound for an environment that is not an agent
// environment. It is on unless an admin switched it off.
func (s *Service) AutoUpdate(ctx context.Context, envID string) (bool, error) {
	row, err := s.st.RQ.GetEnvironment(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrEnvironmentNotFound
	}
	if err != nil {
		return false, fmt.Errorf("get the environment: %w", err)
	}
	if row.Kind != KindAgent {
		return false, ErrEnvironmentNotFound
	}
	return row.AutoUpdate != 0, nil
}

// SetAutoUpdate switches automatic agent updates on or off for an agent
// environment, and audits environment.auto_update_changed when the setting changed.
// Setting what is already set changes and records nothing.
func (s *Service) SetAutoUpdate(ctx context.Context, actor audit.Actor, envID string, on bool, ip, ua string) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := q.GetEnvironment(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && row.Kind != KindAgent {
		return ErrEnvironmentNotFound
	}
	if err != nil {
		return fmt.Errorf("get the environment: %w", err)
	}
	if (row.AutoUpdate != 0) == on {
		return nil
	}
	v := int64(0)
	if on {
		v = 1
	}
	if _, err := q.SetEnvironmentAutoUpdate(ctx, sqlc.SetEnvironmentAutoUpdateParams{AutoUpdate: v, UpdatedAt: s.now().UTC().UnixNano(), ID: envID}); err != nil {
		return fmt.Errorf("set auto-update: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentAutoUpdateChanged, TargetType: "environment", TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"from": onOff(!on), "to": onOff(on)},
	}); err != nil {
		return fmt.Errorf("audit auto-update changed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// ManualUpdateNeeded is whether the node list should give an admin the command that
// updates an agent by hand: the agent is behind a release server, and Coaming cannot
// do it for the admin. That is when the agent is outside the server's version window,
// when it is too old to understand the update message, and when it is connected and
// its host owner turned self-update off. Versions that are not releases are never
// worth a command: a dev build cannot be compared.
func ManualUpdateNeeded(serverVersion, agentVersion string, connected, selfUpdate bool) bool {
	if _, ok := tunnel.ParseVersion(serverVersion); !ok {
		return false
	}
	v, ok := tunnel.ParseVersion(agentVersion)
	if !ok {
		return false
	}
	if inWindow, _, _ := tunnel.Compatible(serverVersion, agentVersion, wire.ProtoVersion); !inWindow {
		return true
	}
	if !tunnel.Behind(serverVersion, agentVersion) {
		return false
	}
	return versionLess(v, minSelfUpdate) || connected && !selfUpdate
}
