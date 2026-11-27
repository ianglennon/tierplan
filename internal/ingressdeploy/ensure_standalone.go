// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// JobKindEnsure is the kind of the job in which the ensure loop redeploys a standalone host's proxy project. It is owned
// by ensureHolder, so it is a system job and no person's.
const JobKindEnsure = "ingress_ensure"

// EnsureNotEnrolled is a pass that found the proxy running for NotEnrolledAfter without an active ingress node row: its
// URL or pin may not reach TierPlan from the host, see docs/ingress.md. Nothing is written for it while the proxy holds the
// URL and pin the server has now: recreating it would not change what it can reach. A changed URL or pin is a redeploy.
const EnsureNotEnrolled = "not_enrolled"

// EnsureNameShadowed is a pass that found a container stored on a network name that is the start of the ingress network's
// ID (T-25-47, ingress_name_shadowed): the project was taken down and is not brought up while the host holds such a
// network. Once the network is gone, the next pass makes it again with another ID.
const EnsureNameShadowed = "name_shadowed"

// NotEnrolledAfter is how long, on the injected clock, a proxy may run without enrolling before a pass says so (D-28).
const NotEnrolledAfter = 5 * time.Minute

// errNoSystem is a wiring fault: a standalone environment is on but the deployer has nothing to deploy its project with.
var errNoSystem = errors.New("ingress: no deployer of the standalone proxy's project")

// ensureStandalone is the ensure pass over a standalone host whose ingress is on (D-14). It reads the host, decides with
// planStandalone, the rule the enable uses, and when the project has to come up it takes the operation lock (never waits
// for it), decides again under it and redeploys the system project in a job. A token is issued only when the up creates or
// starts a container that must enrol (D-26). A host that holds what is wanted is neither written to nor issued a token.
// The caller has seen the environment is not paused; a restore's pause is checked again under the lock.
func (s *Service) ensureStandalone(ctx context.Context, envID string, state ingressstore.DeployState, info EnvInfo) (string, bool) {
	if len(info.Nodes) == 0 || !info.Nodes[0].Connected {
		return EnsureUnreachable, true
	}
	if !validIngressURL(s.url()) {
		return EnsureURLUnset, true
	}
	sp := s.selfPassOf(ctx, info)
	class, record := s.ensureStandalonePass(ctx, envID, state, info, sp)
	// Every pass that could read the host tells the server what the self network holds, whether or not it wrote (D-06).
	if record && ctx.Err() == nil && class != EnsureUnreachable && class != EnsurePaused {
		s.observeStandaloneSelf(ctx, sp)
	}
	return class, record
}

// ensureStandalonePass is the pass over a reachable standalone host whose ingress URL is usable. sp is what the pass knows
// of the self network; the self network step runs under the lock before the proxy's project is built (D-04).
func (s *Service) ensureStandalonePass(ctx context.Context, envID string, state ingressstore.DeployState, info EnvInfo, sp *selfPass) (string, bool) {
	l, plan, err := s.standaloneSurvey(ctx, info, state, sp)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read a standalone host", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	shadow, err := s.standaloneShadow(ctx, info, l)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read a standalone host's containers", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	if !plan.Up && len(shadow) == 0 && !s.standaloneSelfWork(sp, l) {
		return s.standaloneQuietClass(ctx, envID, l), true
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: ensureHolder, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		s.log().Debug("the ingress ensure loop left a standalone host for the next pass: it is busy", "environment", envID, "holder", held.Kind)
		return EnsureLocked, true
	}
	defer release()
	// Under the lock, which an enable, a switch-off, a stack operation and an Admin's token rotation hold too: the switches
	// and the host are read again, and whatever changed since decides what is written.
	state, err = s.d.Store.DeployState(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read an environment's state under the lock", "environment", envID, "error", err)
		return EnsureWriteFailed, true
	}
	if !state.Enabled {
		s.forgetOutstanding(envID)
		return "", false
	}
	if state.Paused {
		return s.observePaused(ctx, envID, state)
	}
	if l, plan, err = s.standaloneSurvey(ctx, info, state, sp); err != nil {
		s.log().Warn("the ingress ensure loop could not read a standalone host under the lock", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	// Every pass checks the network the host holds and every write is checked once done (T-25-47): a container stored on
	// a name that is the start of the ingress network's ID would join it at the next boot, so the proxy is never left up
	// on such a network, nor on one whose containers could not be read.
	if shadow, err = s.standaloneShadow(ctx, info, l); err != nil {
		s.log().Warn("the ingress ensure loop could not read a standalone host's containers under the lock", "environment", envID, "error", err)
		return EnsureUnreachable, true
	}
	if len(shadow) > 0 {
		return s.downShadowed(ctx, envID, info, shadow), true
	}
	if s.standaloneSelfWork(sp, l) {
		if err := s.d.Engine.WriteOnNode(ctx, envID, info.Nodes[0].AgentID, func(c *dockerx.Client) (err error) {
			sp.net, err = s.ensureStandaloneSelfNetwork(ctx, c, sp, l)
			return err
		}); err != nil {
			s.log().Warn("the ingress ensure loop could not put TierPlan's own network right on a standalone host", "environment", envID, "error", err)
			return EnsureWriteFailed, true
		}
	} else {
		sp.net = selfNet{attach: sp.standaloneAttach(l), foreign: l.selfForeign}
		if l.self != nil {
			sp.net.id = l.self.ID
		}
	}
	if !plan.Up {
		s.forgetOutstanding(envID)
		return s.standaloneQuietClass(ctx, envID, l), true
	}
	if !plan.Token {
		s.forgetOutstanding(envID)
	}
	s.log().Info("the ingress ensure loop is putting a standalone proxy right", "environment", envID, "reason", plan.Reason, "token", plan.Token)
	if err := s.redeployStandalone(ctx, envID, info, state, l, plan, sp.net.attach); err != nil {
		s.log().Warn("the ingress ensure loop could not put a standalone proxy right", "environment", envID, "reason", plan.Reason, "error", err)
		// A failed up may still have made the network and started the proxy on it: a hit or an unreadable host takes it down.
		refs, rerr := s.shadowRefs(ctx, info)
		if rerr != nil {
			s.downSystem(ctx, envID, info)
		} else if len(refs) > 0 {
			return s.downShadowed(ctx, envID, info, refs), true
		}
		return EnsureWriteFailed, true
	}
	// The up may have made the network again, with a new ID, even when the survey saw one.
	refs, err := s.shadowRefs(ctx, info)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read a standalone host's containers after an up, and took the proxy down", "environment", envID, "error", err)
		s.downSystem(ctx, envID, info)
		return EnsureUnreachable, true
	}
	if len(refs) > 0 {
		return s.downShadowed(ctx, envID, info, refs), true
	}
	return EnsureOK, true
}

// standaloneShadow lists the containers stored on a name that is the start of the ingress network's ID, when the survey
// found the network; without one there is no ID to shadow.
func (s *Service) standaloneShadow(ctx context.Context, info EnvInfo, l standaloneLive) ([]dockerx.ShadowRef, error) {
	if l.network == nil {
		return nil, nil
	}
	return s.shadowRefs(ctx, info)
}

// shadowRefs reads the host's containers stored on a name that is the start of the ingress network's ID.
func (s *Service) shadowRefs(ctx context.Context, info EnvInfo) ([]dockerx.ShadowRef, error) {
	var refs []dockerx.ShadowRef
	err := s.d.Engine.OnNode(ctx, info.ID, info.Nodes[0].AgentID, func(c *dockerx.Client) (e error) {
		refs, e = c.ContainersShadowingIngress(ctx)
		return e
	})
	return refs, err
}

// downShadowed takes the system project down because of refs and says so. Compose leaves a network another container
// still uses; the next pass finds it again and keeps the proxy down until it is gone.
func (s *Service) downShadowed(ctx context.Context, envID string, info EnvInfo, refs []dockerx.ShadowRef) string {
	s.log().Warn("the ingress ensure loop keeps a standalone proxy down: a container is stored on a network name that is the start of the ingress network's ID",
		"environment", envID, "containers", shadowDetail(refs))
	s.downSystem(ctx, envID, info)
	return EnsureNameShadowed
}

// downSystem runs the system project's down and logs a failure; the next pass reads the host again.
func (s *Service) downSystem(ctx context.Context, envID string, info EnvInfo) {
	if s.d.System == nil {
		return
	}
	if err := s.d.System.DownSystem(ctx, envID, info.Nodes[0].AgentID); err != nil {
		s.log().Warn("the ingress ensure loop could not take a standalone proxy down", "environment", envID, "error", err)
	}
}

// standaloneSurvey reads the host and decides what it needs from what it holds and what the server knows. A proxy that is
// not on the self network as sp wants it (D-04) is brought up again, with the attachment wanted.
func (s *Service) standaloneSurvey(ctx context.Context, info EnvInfo, state ingressstore.DeployState, sp *selfPass) (standaloneLive, standalonePlan, error) {
	l, err := s.readStandaloneLive(ctx, info)
	if err != nil {
		return l, standalonePlan{}, err
	}
	tok, err := s.d.Tokens.TokenStatus(ctx, info.ID)
	if err != nil {
		return l, standalonePlan{}, err
	}
	enrolled, err := s.enrolled(ctx, info.ID, l)
	if err != nil {
		return l, standalonePlan{}, err
	}
	plan := planStandalone(l, IngressImage(s.d.ServerVersion), s.url(), strings.Join(s.pins(), ","), state.AccessLogs, enrolled, tok)
	if !plan.Up && l.proxy != nil && l.proxyOnSelf != sp.standaloneAttach(l) {
		plan = standalonePlan{Up: true, Token: !enrolled, Reason: "self_network"}
	}
	return l, plan, nil
}

// standaloneQuietClass is the class of a pass that has nothing to write: EnsureNotEnrolled when the proxy has run for
// NotEnrolledAfter and no active node row says it enrolled, else EnsureOK. A deployer that cannot list the nodes cannot
// tell, and says nothing.
func (s *Service) standaloneQuietClass(ctx context.Context, envID string, l standaloneLive) string {
	if !l.running() || l.StartedAt.IsZero() || s.d.Clock.Now().Sub(l.StartedAt) < NotEnrolledAfter || s.d.Nodes == nil {
		return EnsureOK
	}
	active, err := s.hasActiveNode(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not list the proxy nodes of a standalone host", "environment", envID, "error", err)
		return EnsureOK
	}
	if active {
		return EnsureOK
	}
	return EnsureNotEnrolled
}

// redeployStandalone issues the token the plan asks for, then brings the system project up in a job and waits for it.
// The caller holds the operation lock, which the job is named the holder of for as long as it runs. The engine's words
// stay in the job; the error says only that the deploy did not succeed.
func (s *Service) redeployStandalone(ctx context.Context, envID string, info EnvInfo, state ingressstore.DeployState, l standaloneLive, plan standalonePlan, selfNetwork bool) error {
	if s.d.System == nil {
		return errNoSystem
	}
	var tok *StandaloneToken
	if plan.Token {
		var err error
		if tok, err = s.ensureToken(ctx, envID); err != nil {
			return err
		}
	}
	subnet := ""
	if l.network != nil && len(l.network.Subnets) > 0 {
		subnet = l.network.Subnets[0]
	}
	project := StandaloneProject(StandaloneInput{
		Image: IngressImage(s.d.ServerVersion), URL: s.url(), Pin: strings.Join(s.pins(), ","), AccessLog: state.AccessLogs,
		Subnet: subnet, Token: tok, SelfNetwork: selfNetwork,
	})
	in := stackops.SystemDeployInput{
		EnvID: envID, NodeID: info.Nodes[0].AgentID, Project: project, Network: standaloneNetworkKey,
		Prefix: StandaloneNetworkPrefix, NoWait: true,
	}
	attached := make(chan struct{})
	results := make(chan stackops.DeployResult, 1)
	job, err := s.d.Jobs.Start(jobs.Spec{Kind: JobKindEnsure, EnvID: envID, Owner: ensureHolder, Items: stackops.SystemItemSpecs()},
		func(jctx context.Context, r *jobs.Reporter) {
			select {
			case <-attached:
			case <-jctx.Done():
			}
			results <- s.d.System.DeploySystem(jctx, in, r)
		})
	if err != nil {
		return err
	}
	s.d.Locks.Attach(envID, job.ID)
	close(attached)
	// The lock is held until the job has ended, whether or not the pass is asked to stop meanwhile.
	stopped := ctx.Done()
	var res stackops.DeployResult
	for got := false; !got; {
		select {
		case res = <-results:
			got = true
		case <-stopped:
			_ = s.d.Jobs.Cancel(job.ID)
			stopped = nil
		}
	}
	if _, err := s.d.Jobs.Wait(context.WithoutCancel(ctx), job.ID); err != nil {
		return err
	}
	if !res.OK() {
		return errSystemFailed
	}
	s.forgetOutstanding(envID)
	return nil
}

// ensureToken is the join token a redeploy that must enrol is given. A token the ensure loop made earlier for a redeploy
// that has not succeeded is given again while it is still the environment's current, enabled token, so a fault that
// stops the deploy before it creates anything (a pull that fails, a host that is read-only, a name that is taken) does
// not supersede a token and write a rotation audit row on every pass (WR-A2). Otherwise a token is issued (D-26) and kept
// until a redeploy succeeds. The caller holds the operation lock.
func (s *Service) ensureToken(ctx context.Context, envID string) (*StandaloneToken, error) {
	s.outstandingMu.Lock()
	held, ok := s.outstanding[envID]
	s.outstandingMu.Unlock()
	if ok {
		cur, err := s.d.Tokens.TokenStatus(ctx, envID)
		if err != nil {
			return nil, err
		}
		if cur.Exists && !cur.Disabled && cur.ID == held.ID {
			return &held, nil
		}
		s.forgetOutstanding(envID)
	}
	issued, err := s.d.Tokens.IssueToken(ctx, audit.SystemIngress, envID, "", auditUserAgent)
	if err != nil {
		return nil, err
	}
	tok := StandaloneToken{ID: issued.ID, Value: issued.Token}
	s.outstandingMu.Lock()
	if s.outstanding == nil {
		s.outstanding = map[string]StandaloneToken{}
	}
	s.outstanding[envID] = tok
	s.outstandingMu.Unlock()
	return &tok, nil
}

// forgetOutstanding drops the token kept for an environment's failed redeploy.
func (s *Service) forgetOutstanding(envID string) {
	s.outstandingMu.Lock()
	delete(s.outstanding, envID)
	s.outstandingMu.Unlock()
}
