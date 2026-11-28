// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// The kinds of the jobs that switch TierPlan's own route on and off (D-17, D-11).
const (
	JobKindSelfRouteEnable  = "ingress_self_route_enable"
	JobKindSelfRouteDisable = "ingress_self_route_disable"
)

// The items of the self route jobs, in the order they run. The switch-on is ItemSelfCheck, ItemSelfClaim and ItemSelfNetwork;
// the switch-off is ItemSelfRelease and ItemSelfNetwork. ItemSelfNetwork is the same word as the preview's network item.
const (
	ItemSelfCheck   = "self_check"
	ItemSelfClaim   = "self_claim"
	ItemSelfRelease = "self_release"
)

// The codes a self route job's item ends with when it fails. A refused check ends ItemSelfCheck with the refusing item's own
// reason (such as ingress.CodeHostTaken or CodeVIPIngressOff) instead.
const (
	// CodeSelfRouteCheckFailed is a check whose reads failed, so nothing could be judged and nothing was written.
	CodeSelfRouteCheckFailed = "self_route_check_failed"
	// CodeSelfRouteClaimFailed is a claim the store could not write for a reason that is not a refusal.
	CodeSelfRouteClaimFailed = "self_route_claim_failed"
	// CodeSelfRouteReleaseFailed is a release the store could not write. The route is still on, and turning it off again
	// tries again.
	CodeSelfRouteReleaseFailed = "self_route_release_failed"
	// CodeSelfRouteHeld is a claim the store refused because the install already holds TierPlan's own route
	// (ingressstore.ErrSystemClaimHeld): another switch-on, in another environment, stored its claim after this job's check.
	CodeSelfRouteHeld = "self_route_held"
)

// SelfNetworkWait is how long a self route job waits, on the injected clock, for an ensure pass to report the self network as
// it should be after the switch. The item ends done either way: the ensure loop keeps trying.
const SelfNetworkWait = time.Minute

// selfDefaultPort is the port of TIERPLAN_LISTEN's default, for a server that does not know its own listen port.
const selfDefaultPort = 7480

// The items of the switch-on in order, and their lines.
var (
	selfEnableItems  = []string{ItemSelfCheck, ItemSelfClaim, ItemSelfNetwork}
	selfEnableLabels = map[string]string{
		ItemSelfCheck:   "Check the hostname and where TierPlan runs",
		ItemSelfClaim:   "Claim the hostname for TierPlan",
		ItemSelfNetwork: "Make the " + ingress.SelfNetworkName + " network",
	}
)

// The items of the switch-off in order, and their lines.
var (
	selfDisableItems  = []string{ItemSelfRelease, ItemSelfNetwork}
	selfDisableLabels = map[string]string{
		ItemSelfRelease: "Release the hostname",
		ItemSelfNetwork: "Remove the " + ingress.SelfNetworkName + " network, unless it is still needed",
	}
)

// selfCodeMessages are the sentences of the self route jobs' codes, built here and never from an engine's answer.
var selfCodeMessages = map[string]string{
	CodeSelfRouteCheckFailed:   "The checks could not be run, so TierPlan's own route was not switched on. Try again.",
	CodeSelfRouteClaimFailed:   "TierPlan's own route could not be stored.",
	CodeSelfRouteReleaseFailed: "TierPlan's own route could not be released, so it is still on. Turn it off again.",
	CodeSelfRouteHeld:          "TierPlan's own route is already on in another environment. Turn it off there first.",
}

// The lines a self route job writes to its output.
const (
	selfStillOffText    = "TierPlan's own route is still off."
	selfNetPendingText  = "The " + ingress.SelfNetworkName + " network isn't there yet. The ingress keeps trying, and the route is served once it is."
	selfNetCancelledLog = "the wait for TierPlan's own network ended with the job"
	selfNetLaterText    = "The ingress hasn't looked at the " + ingress.SelfNetworkName + " network since. It removes the network on a later pass when nothing needs it."
	selfNetKeptText     = "The " + ingress.SelfNetworkName + " network stays: the ingress still sends TierPlan's own HTTP-01 challenges over it."
	selfNetInUseText    = "The " + ingress.SelfNetworkName + " network stays while something is still on it. If TierPlan's stack file names it, " +
		"take those lines out and redeploy TierPlan; a later pass removes the network."
)

// EnableSelfRoute switches TierPlan's own route on in envID (D-17): it takes the environment's operation lock and starts a
// job that re-runs the preview's checks (a refused item ends the job before anything is written, T-28-55), stores the system
// claim with ingress.self_route_enabled in the same transaction, wakes the ensure loop and waits, up to SelfNetworkWait, for
// the next ensure pass to report the self network. It returns the job's ID; a held lock is a *LockedError.
//
// The job ends done once the claim is stored, even when the route is not served yet: a swarm service that is not on the self
// network (self_not_attached) or a certificate still to come is the ensure loop's to finish, and the status says so. by is
// the client the switch came from, for the job's audit rows.
func (s *Service) EnableSelfRoute(ctx context.Context, actor audit.Actor, owner, envID string, req SelfRouteRequest, by Requester) (string, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return "", err
	}
	specs := make([]jobs.ItemSpec, 0, len(selfEnableItems))
	for _, id := range selfEnableItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: selfEnableLabels[id], Kind: "step"})
	}
	// The checks run inside the job, under the lock, as its first item: a refusal is the job's to tell (D-17).
	pre := func(context.Context, EnvInfo, EnableRequest) ([]Finding, error) { return nil, nil }
	return s.startEnable(ctx, JobKindSelfRouteEnable, owner, info, EnableRequest{}, pre, specs,
		func(jctx context.Context, r *jobs.Reporter, release func()) {
			s.runSelfRouteEnable(jctx, r, release, actor, by, envID, req)
		})
}

// selfRun is the state the items of one self route job share.
type selfRun struct {
	s       *Service
	actor   audit.Actor
	by      Requester
	envID   string
	action  string
	r       *jobs.Reporter
	release func()
	items   []string
}

// stop ends the run at the item at index i: the job's one failure row is written first (A-01), the lock is freed, the item
// ends with its code and sentence and the items after it as not run, or cancelled when the job was.
func (e *selfRun) stop(ctx context.Context, i int, code, msg, detail string) {
	item := e.items[i]
	e.s.auditJobFailedBy(ctx, e.actor, e.by, e.action, e.envID, item, code)
	e.release()
	if code == jobs.CodeCancelled {
		e.r.Finish(item, jobs.ItemCancelled, code, "", nil)
		e.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
		return
	}
	if detail != "" {
		e.r.Detail(item, detail)
	}
	if msg == "" {
		msg = selfCodeMessages[code]
	}
	if msg == "" {
		msg = codeMessages[code]
	}
	e.r.Finish(item, jobs.ItemFailed, code, msg, nil)
	e.skipFrom(i+1, jobs.ItemSkipped, CodeNotRun)
}

// skipFrom ends the items from index i on without running them.
func (e *selfRun) skipFrom(i int, state, code string) {
	for _, id := range e.items[i:] {
		msg := codeMessages[code]
		if code == jobs.CodeCancelled {
			msg = ""
		}
		e.r.Finish(id, state, code, msg, nil)
	}
}

func (e *selfRun) running(item string) {
	e.r.Update(item, func(it *jobs.Item) { it.State = jobs.ItemRunning })
}

func (e *selfRun) done(item string, result map[string]string) {
	e.r.Finish(item, jobs.ItemDone, "", "", result)
}

// runSelfRouteEnable runs the switch-on's items, one at a time.
func (s *Service) runSelfRouteEnable(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, by Requester, envID string, req SelfRouteRequest) {
	e := &selfRun{s: s, actor: actor, by: by, envID: envID, action: audit.ActionIngressSelfRouteEnabled, r: r, release: release, items: selfEnableItems}
	if ctx.Err() != nil {
		e.stop(ctx, 0, jobs.CodeCancelled, "", "")
		return
	}

	// The check: the preview, again, as it stands now under the lock.
	e.running(ItemSelfCheck)
	pv, sc, err := s.checkSelfRoute(ctx, envID, req)
	if err != nil {
		if ctx.Err() != nil {
			e.stop(ctx, 0, jobs.CodeCancelled, "", "")
			return
		}
		s.log().Warn("the checks of TierPlan's own route could not be run", "environment", envID, "error", err)
		r.Output(selfStillOffText)
		e.stop(ctx, 0, CodeSelfRouteCheckFailed, "", "")
		return
	}
	if pv.Refused() {
		first, detail := refusedItems(pv)
		r.Output(selfStillOffText)
		e.stop(ctx, 0, first.Reason, first.Message, detail)
		return
	}
	e.done(ItemSelfCheck, map[string]string{"host": pv.Host})

	if ctx.Err() != nil {
		e.stop(ctx, 1, jobs.CodeCancelled, "", "")
		return
	}

	// The claim, with its audit row in the same transaction.
	e.running(ItemSelfClaim)
	backend := selfBackend(sc.place)
	port := sc.urls.ListenPort
	if port == 0 {
		port = selfDefaultPort
	}
	envName := sc.info.Name
	if envName == "" {
		envName = envID
	}
	hook := func(ctx context.Context, tx *sql.Tx, c ingressstore.SystemClaim) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressSelfRouteEnabled, TargetType: "environment", TargetID: envID,
			EnvID: envID, Result: audit.ResultSuccess, IP: by.IP, UserAgent: by.UserAgent,
			Details: map[string]string{"host": c.Hostname, "environment": envName},
		})
	}
	// The observations so far: the network item waits for one made after the claim.
	seen := s.selfSeqOf(envID)
	err = s.d.Store.SetSystemClaim(ctx, ingressstore.SystemClaimInput{
		EnvID: envID, Hostname: pv.Host, DomainID: sc.domainID, Service: backend, Port: port, Actor: actor.ID,
	}, hook)
	if err != nil {
		code, msg := selfClaimError(err)
		s.log().Warn("TierPlan's own route could not be stored", "environment", envID, "code", code, "error", err)
		r.Output(selfStillOffText)
		e.stop(ctx, 1, code, msg, "")
		return
	}
	s.Wake()
	release()
	e.done(ItemSelfClaim, map[string]string{"host": pv.Host, "service": backend, "port": strconv.Itoa(port)})

	// The network: the next pass makes it, and the item says so when it reports it. The route is on whatever this finds.
	e.running(ItemSelfNetwork)
	o, ok, werr := s.waitSelfObservation(ctx, envID, seen, func(o SelfObservation) bool { return o.Exists })
	switch {
	case werr != nil:
		s.log().Info(selfNetCancelledLog, "environment", envID)
		r.Output(selfNetPendingText)
		r.Finish(ItemSelfNetwork, jobs.ItemCancelled, jobs.CodeCancelled, "", nil)
	case ok:
		e.done(ItemSelfNetwork, map[string]string{"network": ingress.SelfNetworkName, "attached": strconv.FormatBool(o.Attached)})
	default:
		r.Output(selfNetPendingText)
		e.done(ItemSelfNetwork, nil)
	}
}

// refusedItems is the first refusing item of a preview and, when more than one refuses, the sentences of all of them for the
// item's detail.
func refusedItems(pv SelfRoutePreview) (SelfRouteItem, string) {
	var first SelfRouteItem
	var msgs []string
	for _, it := range pv.Items {
		if it.State != SelfItemRefused {
			continue
		}
		if len(msgs) == 0 {
			first = it
		}
		msgs = append(msgs, it.Message)
	}
	if first.Reason == "" {
		first.Reason = CodeSelfRouteCheckFailed
	}
	if len(msgs) < 2 {
		return first, ""
	}
	return first, strings.Join(msgs, "\n")
}

// selfBackend is the name the proxy reaches TierPlan by on the self network: its swarm service, or on a standalone host the
// alias TierPlan connects itself with (D-04).
func selfBackend(place SelfPlace) string {
	if place.Kind == SelfKindSwarmService && place.ServiceName != "" {
		return place.ServiceName
	}
	return SelfBackendAlias
}

// selfClaimError is the code and sentence of a claim the store refused or could not write. A route the install already holds
// is CodeSelfRouteHeld; a refusal keeps the policy's own code and sentence; anything else is CodeSelfRouteClaimFailed, with
// none of the store's words.
func selfClaimError(err error) (code, msg string) {
	if errors.Is(err, ingressstore.ErrSystemClaimHeld) {
		return CodeSelfRouteHeld, selfCodeMessages[CodeSelfRouteHeld]
	}
	var refused *ingressstore.RefusedError
	if errors.As(err, &refused) && len(refused.Problems) > 0 {
		return refused.Problems[0].Code, refused.Problems[0].Message
	}
	var name *ingress.NameError
	if errors.As(err, &name) {
		return name.Code, name.Message
	}
	return CodeSelfRouteClaimFailed, ""
}

// selfSeqOf is how many observations of the environment's self network have been published since start.
func (s *Service) selfSeqOf(envID string) uint64 {
	s.selfMu.Lock()
	defer s.selfMu.Unlock()
	return s.selfSeq[envID]
}

// waitSelfObservation waits for an observation of the environment's self network published after the after'th that want
// accepts, up to SelfNetworkWait on the injected clock. ok is false when the wait ran out first, with the last observation;
// the error is ctx's when the job ended. It writes nothing.
func (s *Service) waitSelfObservation(ctx context.Context, envID string, after uint64, want func(SelfObservation) bool) (SelfObservation, bool, error) {
	var bound <-chan time.Time
	if s.selfWaitBound != nil {
		bound = s.selfWaitBound()
	} else {
		bound = s.d.Clock.After(SelfNetworkWait)
	}
	for {
		s.selfMu.Lock()
		o, seq := s.selfObs[envID], s.selfSeq[envID]
		if s.selfTick == nil {
			s.selfTick = make(chan struct{})
		}
		tick := s.selfTick
		s.selfMu.Unlock()
		if seq > after && want(o) {
			return o, true, nil
		}
		select {
		case <-ctx.Done():
			return o, false, ctx.Err()
		case <-bound:
			return o, false, nil
		case <-tick:
		}
	}
}

// DefaultSelfReleaseLockWait is how long the switch-off of TierPlan's own route waits for the environment's operation lock
// when Deps.SelfReleaseLockWait is zero.
const DefaultSelfReleaseLockWait = 30 * time.Second

// selfReleaseLockPoll is how often the switch-off asks for the operation lock while it waits.
const selfReleaseLockPoll = 100 * time.Millisecond

// DisableSelfRoute switches TierPlan's own route off in envID (D-11): it starts a job that waits for the environment's
// operation lock, releases the claim with ingress.self_route_disabled in the same transaction (removeSelfRoute), wakes the
// ensure loop and reports from its next pass whether the self network was removed or kept. No check of the route, the
// engine, the certificate or where TierPlan runs stops it (T-28-53), and nor does a stack operation that holds the
// environment (WR-04): the job waits for the lock up to Deps.SelfReleaseLockWait, then releases without it, since the release
// is one transaction of its own and the ensure loop reads the claim again under the lock. The only refusal is no route on in
// envID (ErrSelfRouteNotEnabled), before a job starts. by is the client the switch came from, for the job's audit rows.
func (s *Service) DisableSelfRoute(ctx context.Context, actor audit.Actor, owner, envID string, by Requester) (string, error) {
	specs := make([]jobs.ItemSpec, 0, len(selfDisableItems))
	for _, id := range selfDisableItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: selfDisableLabels[id], Kind: "step"})
	}
	// The environment is not read: nothing of it is needed to release the claim, and a read that fails must not stop it.
	claim, held, err := s.d.Store.SystemClaim(ctx)
	if err != nil {
		return "", err
	}
	if !held || claim.EnvID != envID {
		return "", ErrSelfRouteNotEnabled
	}
	job, err := s.d.Jobs.Start(jobs.Spec{Kind: JobKindSelfRouteDisable, EnvID: envID, Owner: owner, Items: specs},
		func(jctx context.Context, r *jobs.Reporter) {
			release := s.selfReleaseLock(jctx, owner, envID, r.JobID())
			defer release()
			s.runSelfRouteDisable(jctx, r, release, actor, by, envID)
		})
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// selfReleaseLock waits for the environment's operation lock for the switch-off's job, up to Deps.SelfReleaseLockWait, and
// returns the function that frees it. When the lock is still held at the bound, or the job is cancelled, it returns a release
// that frees nothing: the switch-off goes on without the lock and the holder keeps it.
func (s *Service) selfReleaseLock(ctx context.Context, owner, envID, jobID string) func() {
	bound := s.d.SelfReleaseLockWait
	if bound <= 0 {
		bound = DefaultSelfReleaseLockWait
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	poll := time.NewTicker(selfReleaseLockPoll)
	defer poll.Stop()
	for {
		release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
			Kind: stackops.HolderKindIngress, By: owner, Workload: "ingress", Started: s.d.Clock.Now(),
		})
		if ok {
			s.d.Locks.Attach(envID, jobID)
			return release
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			s.log().Info("TierPlan's own route is released without the environment's operation lock: another operation held it past the wait",
				"environment", envID, "holder", held.Kind, "stack", held.StackName, "waited", bound)
			return func() {}
		case <-ctx.Done():
			return func() {}
		}
	}
}

// runSelfRouteDisable runs the switch-off's items: the release, then the network's report, which never fails the job.
func (s *Service) runSelfRouteDisable(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, by Requester, envID string) {
	e := &selfRun{s: s, actor: actor, by: by, envID: envID, action: audit.ActionIngressSelfRouteDisabled, r: r, release: release, items: selfDisableItems}
	if ctx.Err() != nil {
		e.stop(ctx, 0, jobs.CodeCancelled, "", "")
		return
	}
	seen := s.selfSeqOf(envID)
	// Only this job audits a failed release; the row is written, and the lock freed, before the item ends (A-01).
	onFail := func(item, code string) {
		s.auditJobFailedBy(ctx, actor, by, audit.ActionIngressSelfRouteDisabled, envID, item, code)
		release()
	}
	err := s.removeSelfRoute(ctx, actor, by, envID, r, onFail)
	switch {
	case errors.Is(err, ErrSelfRouteNotEnabled):
		// Released in between by another way (the ingress switch-off): nothing is left to release.
	case err != nil:
		s.log().Warn("TierPlan's own route could not be released", "environment", envID, "error", err)
		release()
		if ctx.Err() != nil {
			e.skipFrom(1, jobs.ItemCancelled, jobs.CodeCancelled)
			return
		}
		e.skipFrom(1, jobs.ItemSkipped, CodeNotRun)
		return
	}
	release()

	e.running(ItemSelfNetwork)
	o, ok, werr := s.waitSelfObservation(ctx, envID, seen, func(SelfObservation) bool { return true })
	switch {
	case werr != nil:
		s.log().Info(selfNetCancelledLog, "environment", envID)
		r.Output(selfNetLaterText)
		r.Finish(ItemSelfNetwork, jobs.ItemCancelled, jobs.CodeCancelled, "", nil)
	case !ok:
		r.Output(selfNetLaterText)
		e.done(ItemSelfNetwork, nil)
	case !o.Exists:
		e.done(ItemSelfNetwork, map[string]string{"network": "removed"})
	case o.KeptInUse:
		r.Output(selfNetInUseText)
		e.done(ItemSelfNetwork, map[string]string{"network": "kept_in_use"})
	default:
		r.Output(selfNetKeptText)
		e.done(ItemSelfNetwork, map[string]string{"network": "kept"})
	}
}

// removeSelfRoute releases TierPlan's own route in envID: the claim and its route go with ingress.self_route_disabled in one
// transaction, and the ensure loop is woken to take the self network away when nothing else wants it. It runs no check at all
// (D-11), so no state of the route, the engine, the certificate or the environment stops it. It is the one release the
// switch-off job and the ingress switch-off (28-31) share.
//
// r reports the release as ItemSelfRelease; it may be nil, and a job without that item ignores it. onFail, when set, is told
// the item and the code of a release that failed or was cancelled, before the item ends, so only the switch-off job audits a
// failure. With no route on in envID it returns ErrSelfRouteNotEnabled, with the item done {removed: false}.
func (s *Service) removeSelfRoute(ctx context.Context, actor audit.Actor, by Requester, envID string, r *jobs.Reporter, onFail func(item, code string)) error {
	if r != nil {
		r.Update(ItemSelfRelease, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	}
	envName := envID
	if info, err := s.d.Envs.Info(ctx, envID); err == nil && info.Name != "" {
		envName = info.Name
	}
	hook := func(ctx context.Context, tx *sql.Tx, c ingressstore.SystemClaim) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressSelfRouteDisabled, TargetType: "environment", TargetID: envID,
			EnvID: envID, Result: audit.ResultSuccess, IP: by.IP, UserAgent: by.UserAgent,
			Details: map[string]string{"host": c.Hostname, "environment": envName},
		})
	}
	c, ok, err := s.d.Store.ClearSystemClaim(ctx, envID, actor.ID, time.Time{}, hook)
	if err != nil {
		code, state := CodeSelfRouteReleaseFailed, jobs.ItemFailed
		if ctx.Err() != nil {
			code, state = jobs.CodeCancelled, jobs.ItemCancelled
		}
		if onFail != nil {
			onFail(ItemSelfRelease, code)
		}
		if r != nil {
			msg := selfCodeMessages[code]
			r.Finish(ItemSelfRelease, state, code, msg, nil)
		}
		return fmt.Errorf("ingressdeploy: release TierPlan's own route: %w", err)
	}
	if !ok {
		if r != nil {
			r.Finish(ItemSelfRelease, jobs.ItemDone, "", "", map[string]string{"removed": "false"})
		}
		return ErrSelfRouteNotEnabled
	}
	s.Wake()
	if r != nil {
		r.Finish(ItemSelfRelease, jobs.ItemDone, "", "", map[string]string{"host": c.Hostname, "removed": "true"})
	}
	return nil
}
