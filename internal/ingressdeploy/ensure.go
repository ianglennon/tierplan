// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/stackops"
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
)

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
	if !record || ctx.Err() != nil {
		return
	}
	if err := s.d.Store.RecordEnsure(ctx, envID, s.d.Clock.Now(), class); err != nil {
		s.log().Warn("the ingress ensure loop could not record a pass", "environment", envID, "error", err)
	}
}

// ensure decides one environment's pass and returns its class. record is false for an environment the pass leaves
// alone altogether. The lock is taken only when there is something to write, is never waited for, and is given back
// before the class is recorded; no write is retried and nothing waits for a rollout.
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
		return "", false
	}
	if !info.ManagerConnected {
		return EnsureUnreachable, true
	}
	var l live
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		l, err = readLive(ctx, c)
		return err
	}); err != nil {
		s.log().Warn("the ingress ensure loop could not read a swarm", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	work, err := s.needsWrite(ctx, envID, state, l)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not compare a swarm with what it wants", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if !work {
		return EnsureOK, true
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: ensureHolder, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		s.log().Debug("the ingress ensure loop left an environment for the next pass: it is busy", "environment", envID, "holder", held.Kind)
		return EnsureLocked, true
	}
	defer release()
	if err := s.repair(ctx, envID, state, l); err != nil {
		s.log().Warn("the ingress ensure loop could not put the ingress objects right", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	return EnsureOK, true
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

// needsWrite is whether a pass has anything to write, judged from the swarm as read: no secret for the current token,
// no network, no service or one that differs from what this server wants, or old token secrets to take away. It
// writes nothing, so a pass over a swarm that is as wanted never takes the lock.
func (s *Service) needsWrite(ctx context.Context, envID string, state ingressstore.DeployState, l live) (bool, error) {
	st, err := s.d.Tokens.TokenStatus(ctx, envID)
	if err != nil {
		return false, err
	}
	var cur dockerx.SecretRow
	have := false
	if st.Exists {
		// A secret of that name that is not TierPlan's is refused by the step under the lock.
		cur, have = l.secret(SecretName(envID, st.Generation, st.ID))
	}
	if !have && (!st.Exists || !st.Disabled) {
		return true, nil
	}
	if _, ok := l.network(); !ok {
		return true, nil
	}
	if !have {
		// A disabled token with no secret is an admin's decision, and nothing is left to run the proxy with.
		return false, nil
	}
	if l.service == nil {
		return true, nil
	}
	changes, err := serviceChanges(s.desiredService(serviceInput{secretID: cur.ID, secretName: cur.Name, accessLog: state.AccessLogs}), l.service.Spec, l.networks)
	if err != nil {
		return false, err
	}
	if len(changes) > 0 {
		return true, nil
	}
	return serviceSettled(l.service) && len(staleSecrets(l.secrets, envID, cur.Name)) > 0, nil
}

// repair runs the shared steps one write at a time, each of which writes only on a difference, and then takes the
// token secrets of earlier generations away. The caller holds the operation lock.
func (s *Service) repair(ctx context.Context, envID string, state ingressstore.DeployState, l live) error {
	var secretID, secretName string
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) (err error) {
		secretID, secretName, err = s.ensureSecret(ctx, c, audit.SystemIngress, envID, false)
		return err
	}); err != nil {
		return err
	}
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		_, err := s.ensureNetwork(ctx, c, envID, "")
		return err
	}); err != nil {
		return err
	}
	if secretName == "" {
		// A disabled token whose secret is gone: there is nothing to run the proxy with, and the loop does not undo the disable.
		return nil
	}
	auth, err := s.authFor(ctx, envID, IngressImage(s.d.ServerVersion))
	if err != nil {
		return err
	}
	in := serviceInput{secretID: secretID, secretName: secretName, accessLog: state.AccessLogs, auth: auth}
	wrote := false
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) (err error) {
		wrote, err = s.ensureService(ctx, c, in)
		return err
	}); err != nil {
		return err
	}
	// Earlier generations' secrets go only once the service names the current one and its last update has settled, so
	// a rollback of that update still finds the secret it needs.
	if wrote || l.service == nil || !serviceSettled(l.service) {
		return nil
	}
	return s.removeStaleSecrets(ctx, envID, secretName)
}

// removeStaleSecrets removes the token secrets of this environment other than the current one. A secret the manager
// still holds a service reference to, or that is already gone, is left for the next pass.
func (s *Service) removeStaleSecrets(ctx context.Context, envID, current string) error {
	return s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		rows, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, row := range staleSecrets(rows, envID, current) {
			if err := c.SecretRemove(ctx, row.ID); err != nil && !errors.Is(err, dockerx.ErrObjectInUse) && !errors.Is(err, dockerx.ErrObjectNotFound) {
				return err
			}
		}
		return nil
	})
}

// staleSecrets are the join token secrets of the environment other than the current one: TierPlan's by label and
// prefix, and this environment's by the tag its secret names carry.
func staleSecrets(rows []dockerx.SecretRow, envID, current string) []dockerx.SecretRow {
	tag := "-" + agents.SecretTag(envID) + "-"
	var out []dockerx.SecretRow
	for _, r := range rows {
		if r.Name != current && ingress.OwnsSecret(r.Name, r.Labels) && strings.Contains(r.Name, tag) {
			out = append(out, r)
		}
	}
	return out
}

// serviceSettled is whether the proxy service has no update under way: it never had one, or the last one completed.
func serviceSettled(d *dockerx.ServiceDetail) bool {
	return d.UpdateStatus == nil || d.UpdateStatus.State == "completed"
}
