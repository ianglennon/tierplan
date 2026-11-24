// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// EnsureTick is how often the ensure loop takes a pass on its own, on the injected clock. A change to the ingress
// state and a Wake take one sooner.
const EnsureTick = time.Minute

// ensureParallel bounds how many environments one round works on at once, so one unreachable swarm does not hold
// up the others.
const ensureParallel = 4

// The classes a pass over an environment ends with. They are recorded on the environment's ingress row for the status
// read and are short words, never an engine's text, which can carry a secret.
const (
	// EnsureOK is a pass that found the objects as wanted, or made them so. It is stored as the empty string, which
	// is what the row holds for "succeeded".
	EnsureOK = ""
	// EnsureUnreachable is a pass that could not read the swarm: no manager is connected or the read failed.
	EnsureUnreachable = "unreachable"
	// EnsureLocked is a pass that had something to write and found the environment's operation lock held.
	EnsureLocked = "locked"
	// EnsurePaused is an environment a restore put under observation (D-42): nothing is written until an Admin resumes.
	EnsurePaused = "paused"
	// EnsureWriteFailed is a pass in which a read or a write inside the lock failed. The next pass is the retry.
	EnsureWriteFailed = "write_failed"
	// EnsureURLUnset is a pass that found no usable ingress URL (empty, or not a wss:// address with a host): the proxy
	// would not start with it, so nothing is written and the running proxies keep the spec they have (WR-06).
	EnsureURLUnset = "url_unset"
	// EnsureEnableFailed is what a failed enable job leaves on an environment that is still off: the objects it made stay, and
	// the status reports them as a failed enable and not as an incomplete switch-off (IN-11). A successful enable, and a
	// switch-off, clear it; the ensure loop never writes it.
	EnsureEnableFailed = "enable_failed"
	// EnsureUpdateFailed is a pass that left the proxy service alone because the update this server wants was written
	// before and the swarm rolled it back (WR-07). It is tried again when the wanted spec changes or after UpdateRetryAfter.
	EnsureUpdateFailed = "update_failed"
	// EnsurePrivilegedLost is a pass over a virtual IP that found a manager no longer allowing privileged services (D-19): nothing
	// is written and the keepalived service that runs is left running. It is recorded on the virtual IP's row only.
	EnsurePrivilegedLost = "privileged_lost"
)

// UpdateRetryAfter is how long, on the injected clock, the loop leaves a proxy service alone after the swarm rolled back an
// update to the spec the loop still wants. The update is stop-first, so each try takes one node's proxy down for the
// swarm's monitor period; an hour keeps a cause that has gone (a registry that was down) from being held against the proxy
// for ever, and keeps a spec that can never start from being tried every minute.
const UpdateRetryAfter = time.Hour

// errUpdateHeld is what ensureService answers when it would write an update the swarm rolled back already.
var errUpdateHeld = errors.New("the update was rolled back and is held back")

// ensureHolder says who holds the operation lock during a pass.
const ensureHolder = "system:ingress"

// Wake asks Run for a pass soon. It never blocks, and a wake that is already waiting absorbs another.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run is the ensure loop: a round now, then one on every EnsureTick of the injected clock, on every change the ingress
// notifier reports and on every Wake, until ctx ends. Each cycle arms exactly one timer; a wake takes its round on the
// timer already armed (D-11).
func (s *Service) Run(ctx context.Context) {
	var changed <-chan struct{}
	var sub *ingress.Subscription
	if s.d.Notifier != nil {
		sub = s.d.Notifier.Subscribe()
		defer sub.Close()
		changed = sub.C()
	}
	for {
		s.round(ctx)
		timer := s.d.Clock.After(EnsureTick)
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer:
				break wait
			case <-changed:
				sub.Pending()
				s.round(ctx)
			case <-s.wake:
				s.round(ctx)
			}
		}
	}
}

// round takes one pass over every environment whose ingress is on. An environment whose ingress is off is never
// listed, so nothing of it is touched (Pitfall 15).
func (s *Service) round(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	ids, err := s.d.Store.EnabledEnvs(ctx)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not list the environments", "error", err)
		return
	}
	var g errgroup.Group
	g.SetLimit(ensureParallel)
	for _, id := range ids {
		g.Go(func() error {
			s.pass(ctx, id)
			return nil
		})
	}
	_ = g.Wait()
	if s.roundHook != nil {
		s.roundHook()
	}
}

// pass is one ensure pass over one environment: it decides, writes what differs under the operation lock and records
// when it ended and with which class.
func (s *Service) pass(ctx context.Context, envID string) {
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment's state", "environment", envID, "error", err)
		return
	}
	if !state.Enabled {
		return
	}
	class, record := s.ensure(ctx, envID, state)
	if ctx.Err() != nil {
		return
	}
	if record {
		if err := s.d.Store.RecordEnsure(ctx, envID, s.d.Clock.Now(), class); err != nil {
			s.log().Warn("the ingress ensure loop could not record a pass", "environment", envID, "error", err)
		}
	}
	// The virtual IP is the second leg of the pass. A paused environment is only observed: nothing of the virtual IP is written
	// or removed until an Admin resumes it (D-42).
	if !state.Paused {
		s.ensureVIP(ctx, envID)
	}
}

// ensure decides one environment's pass and returns its class. record is false for an environment the pass leaves
// alone altogether. The plan is made lock-free; when there is something to write the lock is taken, never waited for,
// and the plan is made again under it, from the state and the swarm as they are then, before anything is written: a
// switch-off, a restore's pause or another writer may have come in between (WR-05). The lock is given back before the
// class is recorded; no write is retried and nothing waits for a rollout.
func (s *Service) ensure(ctx context.Context, envID string, state ingressstore.DeployState) (class string, record bool) {
	if state.Paused {
		return s.observePaused(ctx, envID, state)
	}
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	if !info.Swarm {
		return s.ensureStandalone(ctx, envID, state, info)
	}
	if !info.ManagerConnected {
		return EnsureUnreachable, true
	}
	p, class := s.survey(ctx, envID, state)
	if class != "" {
		return class, true
	}
	if !p.work {
		return p.class(), true
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: ensureHolder, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		s.log().Debug("the ingress ensure loop left an environment for the next pass: it is busy", "environment", envID, "holder", held.Kind)
		return EnsureLocked, true
	}
	defer release()
	// Under the lock, which an enable, a switch-off, a stack operation and an Admin's token rotation hold too: the switches
	// and the swarm are read again, and whatever changed since the plan decides what is written.
	state, err = s.d.Store.DeployState(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment's state under the lock", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if !state.Enabled {
		return "", false
	}
	if state.Paused {
		return s.observePaused(ctx, envID, state)
	}
	if p, class = s.survey(ctx, envID, state); class != "" {
		return class, true
	}
	if !p.work {
		return p.class(), true
	}
	held2, err := s.repair(ctx, envID, state, p.l)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not put the ingress objects right", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if held2 {
		return EnsureUpdateFailed, true
	}
	return EnsureOK, true
}

// ensurePlan is what one reading of the swarm says a pass has to do.
type ensurePlan struct {
	l live
	// work is true when something has to be written: an object is missing, the service differs from what this server
	// wants, or old token secrets are to go.
	work bool
	// held is true when the service differs but the update to the wanted spec was rolled back already, so it is not written.
	held bool
}

// class is the class of a pass that writes nothing more than the plan says.
func (p ensurePlan) class() string {
	if p.held {
		return EnsureUpdateFailed
	}
	return EnsureOK
}

// survey reads the swarm and makes the plan. A class that is not empty says why it could not: the ingress URL is unusable,
// the swarm cannot be read or the comparison failed; nothing has been written in any of them.
func (s *Service) survey(ctx context.Context, envID string, state ingressstore.DeployState) (ensurePlan, string) {
	if !validIngressURL(s.url()) {
		return ensurePlan{}, EnsureURLUnset
	}
	var l live
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		l, err = readLive(ctx, c)
		return err
	}); err != nil {
		s.log().Warn("the ingress ensure loop could not read a swarm", "environment", envID, "error", err)
		return ensurePlan{}, EnsureUnreachable
	}
	p, err := s.needsWrite(ctx, envID, state, l)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not compare a swarm with what it wants", "environment", envID, "error", err)
		return ensurePlan{}, EnsureWriteFailed
	}
	return p, ""
}

// validIngressURL is whether raw is an address the proxy's supervisor accepts as TIERPLAN_INGRESS_URL: wss://, a host, and no
// user name, query or fragment. The supervisor refuses anything else at start, so a spec carrying it would stop the proxy
// from coming up.
func validIngressURL(raw string) bool {
	if raw == "" || strings.Contains(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "wss") {
		return false
	}
	return u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

// observePaused is the pass over an environment a restore paused: it writes nothing and issues no token. The first
// pass that finds it paused records ingress.deploy_paused; a pass that cannot record it leaves the environment
// unrecorded, so the next one tries again.
func (s *Service) observePaused(ctx context.Context, envID string, state ingressstore.DeployState) (string, bool) {
	if state.EnsureError != EnsurePaused && s.d.Audit != nil {
		err := s.d.Audit.Record(ctx, audit.Entry{
			Actor: audit.SystemIngress, Action: audit.ActionIngressDeployPaused, TargetType: "environment", TargetID: envID,
			EnvID: envID, Result: audit.ResultSuccess,
		})
		if err != nil {
			s.log().Warn("the ingress ensure loop could not audit a paused environment", "environment", envID, "error", err)
			return "", false
		}
	}
	return EnsurePaused, true
}

// needsWrite makes the plan from the swarm as read: no secret for the current token, no network, no service or one that
// differs from what this server wants, or old token secrets to take away. It writes nothing, so a pass over a swarm that is
// as wanted never takes the lock. A service whose update to the wanted spec was rolled back is not a thing to write (held),
// though the old secrets may still be.
func (s *Service) needsWrite(ctx context.Context, envID string, state ingressstore.DeployState, l live) (ensurePlan, error) {
	p := ensurePlan{l: l}
	st, err := s.d.Tokens.TokenStatus(ctx, envID)
	if err != nil {
		return p, err
	}
	var cur dockerx.SecretRow
	have := false
	if st.Exists {
		// A secret of that name that is not TierPlan's is refused by the step under the lock.
		cur, have = l.secret(SecretName(envID, st.Generation, st.ID))
	}
	if !have && (!st.Exists || !st.Disabled) {
		p.work = true
		return p, nil
	}
	if _, ok := l.network(); !ok {
		p.work = true
		return p, nil
	}
	if !have {
		// A disabled token with no secret is an admin's decision, and nothing is left to run the proxy with.
		return p, nil
	}
	if l.service == nil {
		p.work = true
		return p, nil
	}
	desired := s.desiredService(serviceInput{secretID: cur.ID, secretName: cur.Name, accessLog: state.AccessLogs})
	changes, err := serviceChanges(desired, l.service.Spec, l.networks)
	if err != nil {
		return p, err
	}
	if len(changes) > 0 {
		if !s.updateHeld(envID, l.service, desired) {
			p.work = true
			return p, nil
		}
		p.held = true
	}
	p.work = serviceSettled(l.service) && len(staleSecrets(l.secrets, envID, cur.Name, liveSecretNames(l.service)...)) > 0
	return p, nil
}

// repair runs the shared steps one write at a time, each of which writes only on a difference, and then takes the
// token secrets of earlier generations away. The caller holds the operation lock. held is true when the service was left
// alone because its update to the wanted spec was rolled back already.
func (s *Service) repair(ctx context.Context, envID string, state ingressstore.DeployState, l live) (held bool, err error) {
	var secretID, secretName string
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) (err error) {
		secretID, secretName, err = s.ensureSecret(ctx, c, audit.SystemIngress, envID, false)
		return err
	}); err != nil {
		return false, err
	}
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		_, err := s.ensureNetwork(ctx, c, envID, "")
		return err
	}); err != nil {
		return false, err
	}
	if secretName == "" {
		// A disabled token whose secret is gone: there is nothing to run the proxy with, and the loop does not undo the disable.
		return false, nil
	}
	auth, err := s.authFor(ctx, envID, IngressImage(s.d.ServerVersion))
	if err != nil {
		return false, err
	}
	in := serviceInput{envID: envID, secretID: secretID, secretName: secretName, accessLog: state.AccessLogs, auth: auth}
	wrote := false
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) (err error) {
		wrote, err = s.ensureService(ctx, c, in)
		return err
	}); err != nil {
		if !errors.Is(err, errUpdateHeld) {
			return false, err
		}
		held = true
	}
	// Earlier generations' secrets go only once the service names the current one and its last update has settled, so
	// a rollback of that update still finds the secret it needs. A rolled-back update has settled too: the secrets the
	// service still references stay, whichever generation they are.
	if wrote || l.service == nil || !serviceSettled(l.service) {
		return held, nil
	}
	return held, s.removeStaleSecrets(ctx, envID, secretName, liveSecretNames(l.service))
}

// removeStaleSecrets removes the token secrets of this environment other than the current one and the ones in keep, which
// the live service still references. A secret the manager
// still holds a service reference to, or that is already gone, is left for the next pass.
func (s *Service) removeStaleSecrets(ctx context.Context, envID, current string, keep []string) error {
	return s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		rows, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, row := range staleSecrets(rows, envID, current, keep...) {
			if err := c.SecretRemove(ctx, row.ID); err != nil && !errors.Is(err, dockerx.ErrObjectInUse) && !errors.Is(err, dockerx.ErrObjectNotFound) {
				return err
			}
		}
		return nil
	})
}

// staleSecrets are the join token secrets of the environment other than the current one and the ones in keep: TierPlan's by
// label and prefix, and this environment's by the tag its secret names carry.
func staleSecrets(rows []dockerx.SecretRow, envID, current string, keep ...string) []dockerx.SecretRow {
	tag := "-" + agents.SecretTag(envID) + "-"
	var out []dockerx.SecretRow
	for _, r := range rows {
		if r.Name != current && !slices.Contains(keep, r.Name) && ingress.OwnsSecret(r.Name, r.Labels) && strings.Contains(r.Name, tag) {
			out = append(out, r)
		}
	}
	return out
}

// serviceSettled is whether the proxy service has no update under way: it never had one, the last one completed, or the
// swarm rolled it back and is done doing so.
func serviceSettled(d *dockerx.ServiceDetail) bool {
	return d.UpdateStatus == nil || d.UpdateStatus.State == "completed" || d.UpdateStatus.State == updateRollbackCompleted
}

// The update states of a service the loop reads.
const (
	updateRollbackCompleted = "rollback_completed"
	updatePaused            = "paused"
)

// liveSecretNames are the names of the secrets the live proxy service's spec references, none for a spec that cannot be read.
func liveSecretNames(d *dockerx.ServiceDetail) []string {
	if d == nil {
		return nil
	}
	var spec swarm.ServiceSpec
	if json.Unmarshal(d.Spec, &spec) != nil || spec.TaskTemplate.ContainerSpec == nil {
		return nil
	}
	var out []string
	for _, ref := range spec.TaskTemplate.ContainerSpec.Secrets {
		if ref != nil {
			out = append(out, ref.SecretName)
		}
	}
	return out
}

// updateMemo is the proxy spec the loop (or an enable) last wrote to an environment's service, and when.
type updateMemo struct {
	hash string
	at   time.Time
}

// specHash identifies a wanted proxy spec: the swarm's defaults applied and the force-update counter, which the swarm owns,
// left out.
func specHash(spec swarm.ServiceSpec) string {
	spec.TaskTemplate.ForceUpdate = 0
	b, err := json.Marshal(stacks.NormalizeSpec(spec))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// noteWritten remembers the spec just written to the environment's proxy service, so a later pass can tell a service the
// swarm rolled back from one that is simply behind.
func (s *Service) noteWritten(envID string, spec swarm.ServiceSpec) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	if s.written == nil {
		s.written = map[string]updateMemo{}
	}
	s.written[envID] = updateMemo{hash: specHash(spec), at: s.d.Clock.Now()}
}

// updateHeld is whether writing spec to the service would repeat an update the swarm rolled back: the last update ended
// rolled back (or paused), the spec is the one the loop wrote last, and UpdateRetryAfter has not passed since. The memory is
// the process's own, so a restart of the server allows one more try.
func (s *Service) updateHeld(envID string, d *dockerx.ServiceDetail, spec swarm.ServiceSpec) bool {
	if d == nil || d.UpdateStatus == nil {
		return false
	}
	if st := d.UpdateStatus.State; st != updateRollbackCompleted && st != updatePaused {
		return false
	}
	s.heldMu.Lock()
	memo, ok := s.written[envID]
	s.heldMu.Unlock()
	if !ok || memo.hash == "" || memo.hash != specHash(spec) {
		return false
	}
	return s.d.Clock.Now().Before(memo.at.Add(UpdateRetryAfter))
}

// vipHeldKey is the key the keepalived service's last written spec is remembered under: the environment's, apart from its
// proxy service's.
func vipHeldKey(envID string) string { return envID + "/keepalived" }

// errVIPSecretStep is what a failed repair of the key secret answers. It carries nothing of the engine's or the vault's words: an
// error that touched the key is never put in a log (PROH-26-03).
var errVIPSecretStep = errors.New("the virtual IP key secret could not be made again")

// vipPlan is what one reading of the swarm says the virtual IP's leg has to do.
type vipPlan struct {
	service *dockerx.ServiceDetail
	// secret is the key secret of the row's generation, nil when the swarm does not hold it.
	secret  *dockerx.SecretRow
	secrets []dockerx.SecretRow
	// work is true when something has to be written: the secret or the service is missing, the service differs from what this
	// server wants, or key secrets of older generations are to go.
	work bool
	// held is true when the service differs but the update to the wanted spec was rolled back already, so it is not written.
	held bool
}

// class is the class of a pass that writes nothing more than the plan says.
func (p vipPlan) class() string {
	if p.held {
		return EnsureUpdateFailed
	}
	return EnsureOK
}

// ensureVIP is the virtual IP's leg of a pass over an environment whose ingress is on and not paused. An environment with no
// virtual IP is left alone. An enabled one is kept as desired (D-18): the key secret of its generation made again from the sealed
// key when it is gone, the keepalived service created or updated when its spec differs from KeepalivedServiceSpec (the image tag
// too, so a server upgrade rolls it), and the key secrets of older generations removed once the service has settled. One whose
// switch is off, a switch-off that failed part-way, is finished through removeVIP (D-16, PROH-26-06). The gate is read on every
// pass: a manager that no longer allows privileged services ends the pass with privileged_lost, writes nothing and never removes
// what runs (D-19). The plan is made lock-free and made again under the operation lock, which is taken and never waited for.
// The class is recorded on the virtual IP's row with the time, and returned; the ingress row is not touched.
func (s *Service) ensureVIP(ctx context.Context, envID string) string {
	class, record := s.ensureVIPClass(ctx, envID)
	if record && ctx.Err() == nil {
		err := s.d.Store.RecordVIPEnsure(ctx, envID, s.d.Clock.Now(), class)
		if err != nil && !errors.Is(err, ingressstore.ErrVIPNotFound) {
			s.log().Warn("the ingress ensure loop could not record a virtual IP pass", "environment", envID, "error", err)
		}
	}
	return class
}

// ensureVIPClass decides the virtual IP's pass and returns its class. record is false when the pass leaves the row alone: there
// is none, or it was removed.
func (s *Service) ensureVIPClass(ctx context.Context, envID string) (class string, record bool) {
	cfg, err := s.d.Store.GetVIP(ctx, envID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return "", false
	}
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment's virtual IP", "environment", envID, "error", err)
		return "", false
	}
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	if !info.Swarm {
		return "", false
	}
	if !info.ManagerConnected {
		return EnsureUnreachable, true
	}
	if cfg.Enabled {
		if class := s.vipGateClass(ctx, envID); class != "" {
			return class, true
		}
		p, class := s.vipSurvey(ctx, envID, cfg)
		if class != "" {
			return class, true
		}
		if !p.work {
			return p.class(), true
		}
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: ensureHolder, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		s.log().Debug("the ingress ensure loop left a virtual IP for the next pass: the environment is busy", "environment", envID, "holder", held.Kind)
		return EnsureLocked, true
	}
	defer release()
	// Under the lock the switches and the swarm are read again, and what changed since the plan decides what is written.
	st, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment's state under the lock", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if !st.Enabled || st.Paused {
		return "", false
	}
	cfg, err = s.d.Store.GetVIP(ctx, envID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return "", false
	}
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment's virtual IP under the lock", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if !cfg.Enabled {
		// A switch-off that did not finish: removing a service is no escalation, so the gate is not asked (D-19).
		if err := s.removeVIP(ctx, audit.SystemIngress, envID, nil, nil); err != nil {
			if errors.Is(err, ErrVIPNotEnabled) {
				return "", false
			}
			s.log().Warn("the ingress ensure loop could not finish a virtual IP switch-off", "environment", envID, "error", err)
			return EnsureWriteFailed, true
		}
		return "", false
	}
	if class := s.vipGateClass(ctx, envID); class != "" {
		return class, true
	}
	p, class := s.vipSurvey(ctx, envID, cfg)
	if class != "" {
		return class, true
	}
	if !p.work {
		return p.class(), true
	}
	held2, err := s.repairVIP(ctx, envID, cfg, p)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not put the virtual IP right", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if held2 {
		return EnsureUpdateFailed, true
	}
	return EnsureOK, true
}

// vipGateClass reads the managers' opt-in and asks the gate. It is empty when every manager allows privileged services,
// EnsurePrivilegedLost when one does not, and EnsureUnreachable when the table could not be read, which says nothing about any
// manager.
func (s *Service) vipGateClass(ctx context.Context, envID string) string {
	rows, err := s.vipManagerRows(ctx, envID)
	if ok, _ := EvaluateManagerGate(rows); ok {
		return ""
	}
	if err != nil {
		return EnsureUnreachable
	}
	return EnsurePrivilegedLost
}

// desiredKeepalived is the keepalived service this server wants for the row, naming the key secret by its name and ID.
func (s *Service) desiredKeepalived(cfg ingressstore.VIPConfig, secretID, secretName string) swarm.ServiceSpec {
	spec := KeepalivedServiceSpec(KeepalivedSpecInput{
		Image: KeepalivedImage(s.d.ServerVersion), Address: cfg.Prefix(), RouterID: cfg.RouterID, SecretName: secretName,
	})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = secretID
	return spec
}

// vipSurvey reads the swarm and makes the plan. A class that is not empty says why it could not: the swarm cannot be read, the
// comparison failed, or the swarm holds a service or a secret of the name that is not TierPlan's, which is never touched.
func (s *Service) vipSurvey(ctx context.Context, envID string, cfg ingressstore.VIPConfig) (vipPlan, string) {
	var p vipPlan
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var err error
		if p.secrets, err = c.SecretList(ctx); err != nil {
			return err
		}
		svc, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case err == nil:
			p.service = &svc
		case errors.Is(err, dockerx.ErrServiceNotFound):
		default:
			return err
		}
		return nil
	}); err != nil {
		s.log().Warn("the ingress ensure loop could not read a swarm for its virtual IP", "environment", envID, "error", err)
		return vipPlan{}, EnsureUnreachable
	}
	if p.service != nil && !ingress.OwnsKeepalivedService(p.service.Name, p.service.Labels) {
		return vipPlan{}, EnsureWriteFailed
	}
	name := KeepalivedSecretName(envID, cfg.KeyGeneration)
	for _, row := range p.secrets {
		if row.Name != name {
			continue
		}
		if !ingress.OwnsKeepalivedSecret(row.Name, row.Labels) {
			return vipPlan{}, EnsureWriteFailed
		}
		p.secret = &row
	}
	if p.secret == nil || p.service == nil {
		p.work = true
		return p, ""
	}
	var nets []dockerx.NetworkRow
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		nets, err = c.Networks(ctx)
		return err
	}); err != nil {
		s.log().Warn("the ingress ensure loop could not read a swarm's networks for its virtual IP", "environment", envID, "error", err)
		return vipPlan{}, EnsureUnreachable
	}
	desired := s.desiredKeepalived(cfg, p.secret.ID, name)
	changes, err := serviceChanges(desired, p.service.Spec, nets)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not compare a swarm with the virtual IP it wants", "environment", envID, "error", err)
		return vipPlan{}, EnsureWriteFailed
	}
	if len(changes) > 0 {
		if !s.updateHeld(vipHeldKey(envID), p.service, desired) {
			p.work = true
			return p, ""
		}
		p.held = true
	}
	p.work = serviceSettled(p.service) && len(staleVIPSecrets(p.secrets, envID, name, liveSecretNames(p.service)...)) > 0
	return p, ""
}

// staleVIPSecrets are the key secrets of the environment other than the current one and the ones in keep, which the live service
// still references.
func staleVIPSecrets(rows []dockerx.SecretRow, envID, current string, keep ...string) []dockerx.SecretRow {
	var out []dockerx.SecretRow
	for _, row := range vipSecretsOf(rows, envID) {
		if row.Name != current && !slices.Contains(keep, row.Name) {
			out = append(out, row)
		}
	}
	return out
}

// repairVIP makes the key secret again when it is gone, writes the keepalived service when it is missing or differs, and takes the
// key secrets of earlier generations away once the service names the current one and has settled. The caller holds the
// operation lock and has asked the gate. held is true when the service was left alone because its update to the wanted spec was
// rolled back already.
func (s *Service) repairVIP(ctx context.Context, envID string, cfg ingressstore.VIPConfig, p vipPlan) (held bool, err error) {
	name := KeepalivedSecretName(envID, cfg.KeyGeneration)
	secretID := ""
	if p.secret != nil {
		secretID = p.secret.ID
	} else if secretID, err = s.makeVIPSecret(ctx, envID, cfg, name); err != nil {
		return false, err
	}
	image := KeepalivedImage(s.d.ServerVersion)
	auth, err := s.authFor(ctx, envID, image)
	if err != nil {
		return false, err
	}
	spec := s.desiredKeepalived(cfg, secretID, name)
	wrote := false
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) (err error) {
		wrote, err = s.ensureKeepalived(ctx, c, envID, spec, auth)
		return err
	}); err != nil {
		if !errors.Is(err, errUpdateHeld) {
			return false, err
		}
		held = true
	}
	// Earlier generations' secrets go only once the service names the current one and its last update has settled, so a rollback
	// of that update still finds the secret it needs.
	if wrote || p.service == nil || !serviceSettled(p.service) {
		return held, nil
	}
	return held, s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		rows, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, row := range staleVIPSecrets(rows, envID, name, liveSecretNames(p.service)...) {
			if err := c.SecretRemove(ctx, row.ID); err != nil && !errors.Is(err, dockerx.ErrObjectInUse) && !errors.Is(err, dockerx.ErrObjectNotFound) {
				return err
			}
		}
		return nil
	})
}

// makeVIPSecret creates the swarm secret of the row's generation from the sealed key and returns its ID. The key is opened here
// and nowhere else in the loop, held in locals, wiped as soon as the secret is sent and in no log: a failure answers the fixed
// errVIPSecretStep (PROH-26-03).
func (s *Service) makeVIPSecret(ctx context.Context, envID string, cfg ingressstore.VIPConfig, name string) (string, error) {
	raw, err := s.d.Store.OpenVIPKey(envID, cfg.KeySealed)
	if err != nil {
		return "", errVIPSecretStep
	}
	defer clear(raw)
	content, err := KeepalivedSecretContent(raw)
	if err != nil {
		return "", errVIPSecretStep
	}
	defer clear(content)
	id := ""
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) (err error) {
		id, err = c.SecretCreate(ctx, dockerx.SecretSpec{Name: name, Labels: ingress.KeepalivedLabels(), Data: content})
		return err
	}); err != nil {
		return "", errVIPSecretStep
	}
	return id, nil
}

// ensureKeepalived creates the keepalived service, or, when one that is TierPlan's exists, updates it at the version read, carrying
// its force-update counter over, and only when the live spec differs from spec. It reports whether it wrote. c is a manager's
// client, inside one write. Nothing waits for the update to converge.
func (s *Service) ensureKeepalived(ctx context.Context, c *dockerx.Client, envID string, spec swarm.ServiceSpec, auth string) (bool, error) {
	cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
	switch {
	case err == nil:
		if !ingress.OwnsKeepalivedService(cur.Name, cur.Labels) {
			return false, errors.New("the swarm holds a service named " + cur.Name + " that is not TierPlan's")
		}
		spec.TaskTemplate.ForceUpdate = liveForceUpdate(cur.Spec)
		nets, err := c.Networks(ctx)
		if err != nil {
			return false, err
		}
		changes, err := serviceChanges(spec, cur.Spec, nets)
		if err != nil {
			return false, err
		}
		if len(changes) == 0 {
			return false, nil
		}
		if s.updateHeld(vipHeldKey(envID), &cur, spec) {
			return false, errUpdateHeld
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			return false, err
		}
		if _, err = c.ServiceUpdate(ctx, cur.ID, cur.Version, raw, auth); err != nil {
			return false, err
		}
		s.noteWritten(vipHeldKey(envID), spec)
		return true, nil
	case errors.Is(err, dockerx.ErrServiceNotFound):
		raw, err := json.Marshal(spec)
		if err != nil {
			return false, err
		}
		if _, _, err = c.ServiceCreate(ctx, raw, auth); err != nil {
			return false, err
		}
		s.noteWritten(vipHeldKey(envID), spec)
		return true, nil
	default:
		return false, err
	}
}
