// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// JobKindVIPChange is the kind of the job that changes an environment's virtual IP.
const JobKindVIPChange = "ingress_vip_change"

// The items of the virtual IP change job, in the order they run (D-16).
const (
	ItemVIPChangeService = "vip_change_service"
	ItemVIPChangeWait    = "vip_change_wait"
	ItemVIPChangeSave    = "vip_change_save"
)

// VIPChangeWaitDeadline is how long the change job waits for every node to run the new configuration. A roll moves one node at
// a time with KeepalivedUpdateDelay between them, so three nodes take about half a minute.
const VIPChangeWaitDeadline = 10 * time.Minute

// vipChangeReadTimeout bounds one read of the service and its tasks while the job waits.
const vipChangeReadTimeout = 20 * time.Second

// vipChangeSaveTimeout bounds the save of a change whose job was cancelled while it waited.
const vipChangeSaveTimeout = 10 * time.Second

// CodeVIPChangeFailed is the code an item of the change job ends with when it fails. The words of the engine ride only with
// the service step, never with anything that touched the key.
const CodeVIPChangeFailed = "vip_change_failed"

// The strategies a change follows (A-08, A-16). A roll is one service update that moves one node at a time; a replace removes
// the service, waits for its tasks to end and creates it again; none touches no node.
const (
	VIPChangeRoll    = "roll"
	VIPChangeReplace = "replace"
	VIPChangeNone    = "none"
)

// What S3 measured decides how each kind of change is made (CONTEXT A-08, A-16). The code never reads the planning file; these
// are its two lines, copied:
//
//	S3 change-address decision: roll
//	S3 change-router-id decision: replace
//
// A router ID rolled one node at a time put the address on two nodes at once (nodes on the old and the new ID each elect a
// holder), so a router-ID change replaces the service. An address change rolls, with one holder at any time. Changing both at
// once is a replace, since it changes a router ID.
const (
	vipChangeAddressReplace = false // S3 change-address decision: roll
	vipChangeRouterReplace  = true  // S3 change-router-id decision: replace
)

// The sentences of the preview and of a failed change, built here and never from an engine's answer.
const (
	vipChangeRollText    = "The address moves. The nodes restart one at a time, so it may be briefly unanswered while it does."
	vipChangeReplaceText = "The virtual IP is unanswered for a few seconds while the keepalived service is removed and created again."
	vipChangeNoneText    = "No node is touched."
	vipChangeKeyGoneText = "The virtual IP key is not in the swarm, so the service cannot be built. Switch the virtual IP off and on again to make a new key."
	vipChangeUndoneText  = "A node could not run the new configuration, so swarm stopped the update. The new address was not saved and the previous one is put back."
	vipChangeSlowText    = "The new address is saved. Swarm carries on with the nodes that have not restarted yet."
	vipChangeLeftText    = "The change did not finish and the keepalived service may be missing or old. Change the virtual IP again to finish it."
)

var (
	vipChangeItems  = []string{ItemVIPChangeService, ItemVIPChangeWait, ItemVIPChangeSave}
	vipChangeLabels = map[string]string{
		ItemVIPChangeService: "Update the keepalived service",
		ItemVIPChangeWait:    "Wait for every node to restart",
		ItemVIPChangeSave:    "Save the new address",
	}
	vipChangeMessages = map[string]string{
		ItemVIPChangeService: "The keepalived service could not be updated.",
		ItemVIPChangeWait:    "Not every node restarted with the new address.",
		ItemVIPChangeSave:    "The new address could not be saved.",
	}
)

// VIPChangePreview is what PreviewVIPChange tells a person before they change (D-16): the enable's checks on the new request
// and what the change would do to the address.
type VIPChangePreview struct {
	VIPPreview
	// Current is the address the environment has now and CurrentRouterID its router ID.
	Current         netip.Prefix
	CurrentRouterID int
	// Strategy is VIPChangeRoll, VIPChangeReplace or VIPChangeNone; empty when the request was refused.
	Strategy string
	// Notice says what the change does to the address: that it moves and may be briefly unanswered, or, for a replace, that it
	// is unanswered for a few seconds.
	Notice string
}

// vipChangeStrategy is how a change is made, by what it changes.
func vipChangeStrategy(addressChanged, routerChanged bool) string {
	switch {
	case !addressChanged && !routerChanged:
		return VIPChangeNone
	case (routerChanged && vipChangeRouterReplace) || (addressChanged && vipChangeAddressReplace):
		return VIPChangeReplace
	default:
		return VIPChangeRoll
	}
}

// vipChangeNotice is the sentence of a strategy.
func vipChangeNotice(strategy string) string {
	switch strategy {
	case VIPChangeReplace:
		return vipChangeReplaceText
	case VIPChangeRoll:
		return vipChangeRollText
	default:
		return vipChangeNoneText
	}
}

// vipChangeLabelList is the labels of the change's items in order.
func vipChangeLabelList() []string {
	out := make([]string, 0, len(vipChangeItems))
	for _, id := range vipChangeItems {
		out = append(out, vipChangeLabels[id])
	}
	return out
}

// vipChangeChecks runs the enable's checks on the request for the virtual IP the environment has and decides the strategy. It
// writes nothing. An environment with no virtual IP is ErrVIPNotEnabled.
func (s *Service) vipChangeChecks(ctx context.Context, info EnvInfo, req VIPRequest) (VIPChangePreview, ingressstore.VIPConfig, error) {
	pv, findings, err := s.vipChecks(ctx, info, req, true)
	if err != nil {
		return VIPChangePreview{}, ingressstore.VIPConfig{}, err
	}
	cur, err := s.d.Store.GetVIP(ctx, info.ID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return VIPChangePreview{}, ingressstore.VIPConfig{}, ErrVIPNotEnabled
	}
	if err != nil {
		return VIPChangePreview{}, ingressstore.VIPConfig{}, err
	}
	pv.Findings = findings
	pv.Items = vipChangeLabelList()
	out := VIPChangePreview{VIPPreview: pv, Current: cur.Prefix(), CurrentRouterID: cur.RouterID}
	if !hasErrors(findings) && pv.Address.IsValid() && pv.RouterID != 0 {
		out.Strategy = vipChangeStrategy(pv.Address != cur.Prefix(), pv.RouterID != cur.RouterID)
		out.Notice = vipChangeNotice(out.Strategy)
	}
	return out, cur, nil
}

// PreviewVIPChange says what a change of the virtual IP would do and what would stop it, and writes nothing. The checks are the
// enable's, on the new request. A request is the whole configuration: no router ID derives it from the cluster ID again.
func (s *Service) PreviewVIPChange(ctx context.Context, envID string, req VIPRequest) (VIPChangePreview, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return VIPChangePreview{}, err
	}
	pv, _, err := s.vipChangeChecks(ctx, info, req)
	return pv, err
}

// ChangeVIP changes the environment's virtual IP: it takes the environment's operation lock, re-runs every check of the
// enable on the new request (the address, the router ID, the collisions and the manager gate), and starts a job that updates
// the keepalived service, waits until every node runs it and stores the new address with its audit entry. The key and its secret are kept: a change never
// rotates them (D-03). It returns the job's ID. A held lock is a *LockedError, a refused check a *PreflightError and an
// environment with no virtual IP is ErrVIPNotEnabled; in each case nothing was written. A request that changes nothing is a job
// that writes nothing. A step that fails ends its item with vip_change_failed and the ones after it as not run; nothing is
// rolled back, and changing again starts over from what the failed one left. The wait is the exception: once the swarm took the
// new configuration it carries on by itself, so a wait that was too long, could not read the swarm or was cancelled still saves
// the new address. Only an update the swarm stopped or rolled back is not saved: the row keeps the previous address, and the
// ensure loop puts it back on the nodes.
func (s *Service) ChangeVIP(ctx context.Context, actor audit.Actor, owner, envID string, req VIPRequest) (string, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return "", err
	}
	specs := make([]jobs.ItemSpec, 0, len(vipChangeItems))
	for _, id := range vipChangeItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: vipChangeLabels[id], Kind: "step"})
	}
	// pv and cur are what the preflight under the lock decided; the job runs on them.
	var (
		pv  VIPChangePreview
		cur ingressstore.VIPConfig
	)
	pre := func(ctx context.Context, _ EnvInfo, _ EnableRequest) ([]Finding, error) {
		// The environment is read again under the lock, so the checks see it as it is now.
		fresh, err := s.d.Envs.Info(ctx, envID)
		if err != nil {
			return nil, err
		}
		pv, cur, err = s.vipChangeChecks(ctx, fresh, req)
		return pv.Findings, err
	}
	return s.startEnable(ctx, JobKindVIPChange, owner, info, EnableRequest{}, pre, specs,
		func(jctx context.Context, r *jobs.Reporter, release func()) {
			s.runVIPChange(jctx, r, release, actor, envID, pv, cur)
		})
}

// vipChangeRun is the state the steps of one change share.
type vipChangeRun struct {
	s       *Service
	actor   audit.Actor
	envID   string
	r       *jobs.Reporter
	release func()
	pv      VIPChangePreview
	cur     ingressstore.VIPConfig
	image   string
	// serviceID is the keepalived service the service step wrote and prior the update status it had before; empty when no
	// node is touched.
	serviceID string
	prior     *dockerx.ServiceUpdateStatus
}

// runVIPChange runs the steps, one at a time. The first failure ends its item with a code and the items after it as not run.
// A cancel stops it up to the moment the swarm took the new configuration; after that it only ends the wait, and the address
// is saved, so the row never says less than what the nodes are told to run.
func (s *Service) runVIPChange(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, envID string, pv VIPChangePreview, cur ingressstore.VIPConfig) {
	run := &vipChangeRun{s: s, actor: actor, envID: envID, r: r, release: release, pv: pv, cur: cur, image: KeepalivedImage(s.d.ServerVersion)}
	steps := []struct {
		item string
		do   func(context.Context) error
	}{
		{ItemVIPChangeService, run.service},
		{ItemVIPChangeWait, run.wait},
		{ItemVIPChangeSave, run.save},
	}
	for i, st := range steps {
		taken := run.serviceID != "" // an earlier step ended well, so the swarm took the new configuration
		if ctx.Err() != nil && !taken {
			run.stop(st.item, jobs.ItemCancelled, jobs.CodeCancelled, "")
			run.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
			return
		}
		r.Update(st.item, func(it *jobs.Item) { it.State = jobs.ItemRunning })
		if err := st.do(ctx); err != nil {
			if ctx.Err() != nil && !taken {
				run.stop(st.item, jobs.ItemCancelled, jobs.CodeCancelled, "")
				run.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
				return
			}
			detail := ""
			var se *stepError
			if errors.As(err, &se) {
				detail = se.detail
			}
			s.log().Warn("a virtual IP change step failed", "environment", envID, "step", st.item, "error", err)
			if !errors.Is(err, errVIPChangeUndone) {
				r.Output(vipChangeLeftText)
			}
			run.stop(st.item, jobs.ItemFailed, CodeVIPChangeFailed, detail)
			run.skipFrom(i+1, jobs.ItemSkipped, CodeNotRun)
			return
		}
	}
}

// stop ends the item that failed or was cancelled, with the sentence of its code.
func (e *vipChangeRun) stop(item, state, code, detail string) {
	e.release()
	if detail != "" {
		e.r.Detail(item, detail)
	}
	msg := ""
	switch code {
	case CodeVIPChangeFailed:
		msg = vipChangeMessages[item]
	case jobs.CodeCancelled:
	default:
		msg = codeMessages[code]
	}
	e.r.Finish(item, state, code, msg, nil)
}

// skipFrom ends the items from index i on without running them.
func (e *vipChangeRun) skipFrom(i int, state, code string) {
	for _, id := range vipChangeItems[i:] {
		msg := codeMessages[code]
		if code == jobs.CodeCancelled {
			msg = ""
		}
		e.r.Finish(id, state, code, msg, nil)
	}
}

// done ends an item that went well.
func (e *vipChangeRun) done(item string, result map[string]string) {
	e.r.Finish(item, jobs.ItemDone, "", "", result)
}

// serviceSpec builds the keepalived service for the new address and router ID with the ID of the key secret of the row's
// generation, which the same swarm holds already. A secret that is gone cannot be made again here: the key's clear bytes exist
// only while an enable runs.
func (e *vipChangeRun) serviceSpec(ctx context.Context, c *dockerx.Client) (json.RawMessage, error) {
	name := KeepalivedSecretName(e.envID, e.cur.KeyGeneration)
	rows, err := c.SecretList(ctx)
	if err != nil {
		return nil, err
	}
	secretID := ""
	for _, row := range rows {
		if row.Name == name && ingress.OwnsKeepalivedSecret(row.Name, row.Labels) {
			secretID = row.ID
		}
	}
	if secretID == "" {
		return nil, &stepError{code: CodeVIPChangeFailed, err: errors.New("the key secret " + name + " is not in the swarm"), detail: vipChangeKeyGoneText}
	}
	spec := KeepalivedServiceSpec(KeepalivedSpecInput{Image: e.image, Address: e.pv.Address, RouterID: e.pv.RouterID, SecretName: name})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = secretID
	return json.Marshal(spec)
}

// vipNotOursDetail is the detail of a keepalived service name that another service holds.
func vipNotOursDetail(name string) string {
	return "A service named " + name + " already exists and is not TierPlan's. Remove or rename it, then change the virtual IP again."
}

// service makes the keepalived service follow the new request, by the strategy the preview chose. A change that moves no node
// leaves it alone.
func (e *vipChangeRun) service(ctx context.Context) error {
	if e.pv.Strategy == VIPChangeNone {
		e.done(ItemVIPChangeService, map[string]string{"strategy": VIPChangeNone})
		return nil
	}
	s := e.s
	auth, err := s.authFor(ctx, e.envID, e.image)
	if err != nil {
		return fail(CodeVIPChangeFailed, err)
	}
	if e.pv.Strategy == VIPChangeReplace {
		err = e.replace(ctx, auth)
	} else {
		err = e.roll(ctx, auth)
	}
	if err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return se
		}
		return fail(CodeVIPChangeFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemVIPChangeService, map[string]string{"strategy": e.pv.Strategy})
	return nil
}

// roll updates the service in one write: swarm moves one node at a time, stopping the old task first, with the update delay S3
// chose (the spec carries them). A service that is not there is created.
func (e *vipChangeRun) roll(ctx context.Context, auth string) error {
	return e.s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		raw, err := e.serviceSpec(ctx, c)
		if err != nil {
			return err
		}
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case err == nil:
			if !ingress.OwnsKeepalivedService(cur.Name, cur.Labels) {
				return &stepError{code: CodeVIPChangeFailed, err: errors.New("the swarm holds a service named " + cur.Name + " that is not TierPlan's"), detail: vipNotOursDetail(cur.Name)}
			}
			e.serviceID, e.prior = cur.ID, cur.UpdateStatus
			_, err = c.ServiceUpdate(ctx, cur.ID, cur.Version, raw, auth)
			return err
		case errors.Is(err, dockerx.ErrServiceNotFound):
			e.serviceID, _, err = c.ServiceCreate(ctx, raw, auth)
			return err
		default:
			return err
		}
	})
}

// replace removes the service, waits for its tasks to end, so every holder has dropped the address, and creates it again with
// the new configuration. The nodes never run two router IDs at once, and the address is unanswered for a few seconds.
func (e *vipChangeRun) replace(ctx context.Context, auth string) error {
	s := e.s
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case errors.Is(err, dockerx.ErrServiceNotFound):
			return nil
		case err != nil:
			return err
		case !ingress.OwnsKeepalivedService(cur.Name, cur.Labels):
			return &stepError{code: CodeVIPChangeFailed, err: errors.New("the swarm holds a service named " + cur.Name + " that is not TierPlan's"), detail: vipNotOursDetail(cur.Name)}
		}
		if err := c.ServiceRemove(ctx, cur.ID); err != nil && !errors.Is(err, dockerx.ErrServiceNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	left, err := s.waitKeepalivedGone(ctx, e.envID, VIPDisableTaskDeadline, nil)
	if err != nil {
		return err
	}
	if left > 0 {
		return fail(CodeVIPChangeFailed, errors.New("keepalived tasks remain"),
			strconv.Itoa(left)+" keepalived task(s) were still running "+VIPDisableTaskDeadline.String()+" after the service was removed, so it was not created again.")
	}
	return s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		raw, err := e.serviceSpec(ctx, c)
		if err != nil {
			return err
		}
		e.serviceID, _, err = c.ServiceCreate(ctx, raw, auth)
		return err
	})
}

// errVIPChangeUndone is an update the swarm stopped or rolled back: the one end of the wait that is a step failure.
var errVIPChangeUndone = errors.New("the swarm stopped the keepalived service's update")

// wait waits until every node the swarm wants keepalived on runs the new configuration, so the job is not done, and the new
// address not saved, while nodes still serve the old one (UAT-26 issue 3). It writes nothing to the swarm. It fails only when
// the swarm stopped or rolled back the update; any other end leaves a swarm that carries on by itself, so it is reported on
// its own item and the save follows, also when the job was cancelled.
func (e *vipChangeRun) wait(ctx context.Context) error {
	if e.serviceID == "" {
		e.done(ItemVIPChangeWait, nil)
		return nil
	}
	s := e.s
	var last stackops.Convergence
	err := stackops.WaitConverged(ctx, s.d.Clock, VIPChangeWaitDeadline, func(ctx context.Context) (stackops.Convergence, []dockerx.TaskRow, error) {
		rctx, cancel := context.WithTimeout(ctx, vipChangeReadTimeout)
		defer cancel()
		var (
			d    dockerx.ServiceDetail
			rows []dockerx.TaskRow
		)
		if err := s.d.Engine.ClusterRead(rctx, e.envID, func(c *dockerx.Client) (err error) {
			if d, _, err = c.ServiceInspect(rctx, e.serviceID); err != nil {
				return err
			}
			rows, err = c.Tasks(rctx, dockerx.TaskFilter{ServiceIDs: []string{d.ID}})
			return err
		}); err != nil {
			return stackops.Convergence{}, nil, err
		}
		return stackops.Progress(d, rows).IgnoringUpdate(e.prior), rows, nil
	}, nil, func(c stackops.Convergence, _ []dockerx.TaskRow) { last = c })
	nodes := map[string]string{"running": strconv.Itoa(last.Running), "wanted": strconv.Itoa(last.Want)}
	switch {
	case err == nil:
		e.done(ItemVIPChangeWait, nodes)
		return nil
	case errors.Is(err, stackops.ErrUpdateStopped):
		return fail(CodeVIPChangeFailed, errVIPChangeUndone, vipChangeUndoneText)
	}
	s.log().Warn("a virtual IP change saved its address before every node had restarted", "environment", e.envID, "error", err)
	e.r.Output(vipChangeSlowText)
	if ctx.Err() != nil {
		e.r.Finish(ItemVIPChangeWait, jobs.ItemCancelled, jobs.CodeCancelled, "", nodes)
		return nil
	}
	e.r.Finish(ItemVIPChangeWait, jobs.ItemFailed, CodeVIPChangeFailed, vipChangeMessages[ItemVIPChangeWait], nodes)
	return nil
}

// save stores the new address with the audit entry in the same transaction, so the row never changes without its entry nor the
// entry without the change. A request that changes nothing stores nothing.
func (e *vipChangeRun) save(ctx context.Context) error {
	s := e.s
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), vipChangeSaveTimeout)
		defer cancel()
	}
	pfx := e.pv.Address
	if pfx == e.cur.Prefix() && e.pv.RouterID == e.cur.RouterID && e.pv.RouterDerived == e.cur.RouterDerived {
		e.release()
		e.done(ItemVIPChangeSave, map[string]string{"changed": "false"})
		return nil
	}
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: e.actor, Action: audit.ActionIngressVIPChanged, TargetType: "environment", TargetID: e.envID,
			EnvID: e.envID, Result: audit.ResultSuccess,
			Details: map[string]string{"address": pfx.String(), "router_id": strconv.Itoa(e.pv.RouterID)},
		})
	}
	if err := s.d.Store.UpdateVIPAddressWithHook(ctx, e.envID, pfx.Addr(), pfx.Bits(), e.pv.RouterID, e.pv.RouterDerived, hook); err != nil {
		return fail(CodeVIPChangeFailed, err)
	}
	e.release()
	e.done(ItemVIPChangeSave, map[string]string{"changed": "true"})
	return nil
}
