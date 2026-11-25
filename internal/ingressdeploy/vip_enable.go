// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// JobKindVIPEnable is the kind of the job that switches an environment's virtual IP on.
const JobKindVIPEnable = "ingress_vip_enable"

// The items of the virtual IP enable job, in the order they run (D-16).
const (
	ItemVIPKey     = "vip_key"
	ItemVIPSecret  = "vip_secret"
	ItemVIPService = "vip_service"
	ItemVIPSwitch  = "vip_switch"
)

// The codes an item of the virtual IP enable job ends with when it fails. Each is a stable word; nothing of an engine's
// answer rides with the key and secret steps, because it could touch the key.
const (
	CodeVIPKeyFailed     = "vip_key_failed"
	CodeVIPSecretFailed  = "vip_secret_failed"
	CodeVIPServiceFailed = "vip_service_failed"
	CodeVIPSwitchFailed  = "vip_switch_failed"
)

// vipCodeMessages are the sentences of the job's codes, built here and never from an engine's answer. A code that is not
// here is read from codeMessages (not_run).
var vipCodeMessages = map[string]string{ //nolint:gosec // G101: job codes and their sentences, not credentials
	CodeVIPKeyFailed:     "The VRRP key could not be made or sealed.",
	CodeVIPSecretFailed:  "The VRRP key could not be stored as a swarm secret.",
	CodeVIPServiceFailed: "The keepalived service could not be created or updated.",
	CodeVIPSwitchFailed:  "The virtual IP could not be switched on.",
}

// vipItemLabels are the lines of the job's steps.
var vipItemLabels = map[string]string{
	ItemVIPKey:     "Make the VRRP key",
	ItemVIPSecret:  "Store the key as a swarm secret",
	ItemVIPService: "Create the keepalived service",
	ItemVIPSwitch:  "Switch the virtual IP on",
}

// vipEnableItems are the items of the job in order.
var vipEnableItems = []string{ItemVIPKey, ItemVIPSecret, ItemVIPService, ItemVIPSwitch}

// vipItemLabelList is the labels of the job's items in order, for the preview.
func vipItemLabelList() []string {
	out := make([]string, 0, len(vipEnableItems))
	for _, id := range vipEnableItems {
		out = append(out, vipItemLabels[id])
	}
	return out
}

// errVIPManagersUnread is why the gate had no table to read when the dependency is not wired.
var errVIPManagersUnread = errors.New("ingressdeploy: the managers' privileged-services setting is not readable")

// vipFirstGeneration is the generation of the key an enable makes.
const vipFirstGeneration = 1

// PreviewVIP says what a virtual IP enable would do and what would stop it, and writes nothing: no key is made, no secret
// created. The preview always carries the fixed privilege warning (D-20).
func (s *Service) PreviewVIP(ctx context.Context, envID string, req VIPRequest) (VIPPreview, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return VIPPreview{}, err
	}
	pv, findings, err := s.vipPreflight(ctx, info, req)
	if err != nil {
		return VIPPreview{}, err
	}
	pv.Findings = findings
	return pv, nil
}

// EnableVIP switches the environment's virtual IP on: it takes the environment's operation lock, re-runs every check, and
// starts a job that makes the VRRP key, stores it as a swarm secret, creates the keepalived service and stores the row with
// its audit entry. It returns the job's ID. A held lock is a *LockedError and a refused check a *PreflightError; in each case
// nothing was written. A step that fails ends its item with a stable code and the items after it as not run; nothing is
// retried and nothing rolled back, and switching on again starts over from what the failed one left.
func (s *Service) EnableVIP(ctx context.Context, actor audit.Actor, owner, envID string, req VIPRequest) (string, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return "", err
	}
	specs := make([]jobs.ItemSpec, 0, len(vipEnableItems))
	for _, id := range vipEnableItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: vipItemLabels[id], Kind: "step"})
	}
	// pv is what the preflight under the lock decided; the job runs on it. The preflight ends before the job starts.
	var pv VIPPreview
	pre := func(ctx context.Context, _ EnvInfo, _ EnableRequest) ([]Finding, error) {
		// The environment is read again under the lock, so the checks see it as it is now.
		fresh, err := s.d.Envs.Info(ctx, envID)
		if err != nil {
			return nil, err
		}
		var findings []Finding
		pv, findings, err = s.vipPreflight(ctx, fresh, req)
		return findings, err
	}
	return s.startEnable(ctx, JobKindVIPEnable, owner, info, EnableRequest{}, pre, specs,
		func(jctx context.Context, r *jobs.Reporter, release func()) {
			s.runVIPEnable(jctx, r, release, actor, envID, pv)
		})
}

// vipRun is the state the steps of one virtual IP enable share. It embeds the ingress enable's run for the helpers that end
// an item.
type vipRun struct {
	enableRun
	pv VIPPreview

	// key is the VRRP key's clear bytes. They exist from the key step until the secret step has sent them, and are wiped
	// there, or when the job ends, whichever is first (PROH-26-03). They are in no item, line, log or audit detail.
	key []byte
	// sealed is the key as the vault holds it, for the row the switch step stores.
	sealed     string
	secretID   string
	secretName string
}

// runVIPEnable runs the steps, one at a time. The first failure ends its item with a code and the items after it as not
// run.
func (s *Service) runVIPEnable(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, envID string, pv VIPPreview) {
	run := &vipRun{
		enableRun: enableRun{s: s, actor: actor, envID: envID, r: r, release: release, items: vipEnableItems, image: KeepalivedImage(s.d.ServerVersion)},
		pv:        pv,
	}
	defer run.wipe()
	steps := []struct {
		item string
		do   func(context.Context) error
	}{
		{ItemVIPKey, run.makeKey},
		{ItemVIPSecret, run.makeSecret},
		{ItemVIPService, run.makeService},
		{ItemVIPSwitch, run.switchOn},
	}
	for i, st := range steps {
		if ctx.Err() != nil {
			run.stopVIP(st.item, jobs.ItemCancelled, jobs.CodeCancelled)
			run.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
			return
		}
		r.Update(st.item, func(it *jobs.Item) { it.State = jobs.ItemRunning })
		if err := st.do(ctx); err != nil {
			code, detail := CodeNotRun, ""
			var se *stepError
			if errors.As(err, &se) {
				code, detail = se.code, se.detail
			}
			s.log().Warn("a virtual IP enable step failed", "environment", envID, "step", st.item, "code", code, "error", err)
			r.Output("The virtual IP is still off. Switch it on again to start over from what was made.")
			run.stopVIP(st.item, jobs.ItemFailed, code, detail)
			run.skipFrom(i+1, jobs.ItemSkipped, CodeNotRun)
			return
		}
	}
}

// wipe zeroes the key's clear bytes. It is safe to repeat.
func (e *vipRun) wipe() {
	clear(e.key)
	e.key = nil
}

// stopVIP ends the item that failed or was cancelled, with the sentence of its code.
func (e *vipRun) stopVIP(item, state, code string, detail ...string) {
	e.release()
	if len(detail) > 0 && detail[0] != "" {
		e.r.Detail(item, detail[0])
	}
	msg := vipCodeMessages[code]
	if msg == "" {
		msg = codeMessages[code]
	}
	if code == jobs.CodeCancelled {
		msg = ""
	}
	e.r.Finish(item, state, code, msg, nil)
}

// makeKey makes the 32 random bytes of the VRRP key and seals them under the context of the row they will be stored in. The
// clear bytes stay in the run until the secret step has sent them.
func (e *vipRun) makeKey(_ context.Context) error {
	raw := make([]byte, keepalivedKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return fail(CodeVIPKeyFailed, err)
	}
	sealed, err := e.s.d.Store.SealVIPKey(e.envID, raw)
	if err != nil {
		clear(raw)
		return fail(CodeVIPKeyFailed, err)
	}
	e.key, e.sealed = raw, sealed
	e.done(ItemVIPKey, map[string]string{"generation": strconv.Itoa(vipFirstGeneration)})
	return nil
}

// makeSecret creates the swarm secret that carries the key, named after the environment and the key's generation. A secret
// of that name that is TierPlan's is the leftover of an enable that failed before the row was stored, so it is removed and
// made again with this key; one that is not TierPlan's is never touched, and the preflight has refused it already.
//
// When the leftover enable got as far as the service, that service still references the secret and the swarm refuses to
// remove it. The service is TierPlan's leftover too (the row that would make it the environment's is missing), so it is
// removed first, the step waits until its tasks have ended, and the secret is removed and made again. Only a service
// named and labelled as TierPlan's keepalived service is ever removed here.
func (e *vipRun) makeSecret(ctx context.Context) error {
	content, err := KeepalivedSecretContent(e.key)
	if err != nil {
		return fail(CodeVIPSecretFailed, err)
	}
	defer clear(content)
	name := KeepalivedSecretName(e.envID, vipFirstGeneration)
	for attempt := 0; ; attempt++ {
		inUse := false
		err = e.s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
			rows, err := c.SecretList(ctx)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Name != name {
					continue
				}
				if !ingress.OwnsKeepalivedSecret(row.Name, row.Labels) {
					return errors.New("the swarm holds a secret named " + name + " that is not TierPlan's")
				}
				switch err := c.SecretRemove(ctx, row.ID); {
				case err == nil, errors.Is(err, dockerx.ErrObjectNotFound):
				case errors.Is(err, dockerx.ErrObjectInUse):
					inUse = true
					return err
				default:
					return err
				}
			}
			e.secretID, err = c.SecretCreate(ctx, dockerx.SecretSpec{Name: name, Labels: ingress.KeepalivedLabels(), Data: content})
			return err
		})
		if err == nil || !inUse || attempt > 0 {
			break
		}
		cleared, clearErr := e.removeLeftoverService(ctx)
		if clearErr != nil {
			err = clearErr
			break
		}
		if !cleared {
			// Something that is not the keepalived service holds the secret: it is not ours to remove.
			break
		}
	}
	e.wipe()
	if err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return se
		}
		// The engine's words are not shown: an error that touched the key step is never put in a job.
		return fail(CodeVIPSecretFailed, err)
	}
	e.secretName = name
	e.done(ItemVIPSecret, map[string]string{"secret": name})
	return nil
}

// removeLeftoverService removes the keepalived service an earlier enable left behind, and waits until its tasks have ended
// so every node has dropped the address before the new key is used. It reports false, having removed nothing, when there is no
// such service, and refuses one that is not TierPlan's.
func (e *vipRun) removeLeftoverService(ctx context.Context) (bool, error) {
	s := e.s
	removed := false
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case errors.Is(err, dockerx.ErrServiceNotFound):
			return nil
		case err != nil:
			return err
		case !ingress.OwnsKeepalivedService(cur.Name, cur.Labels):
			return nil
		}
		if err := c.ServiceRemove(ctx, cur.ID); err != nil && !errors.Is(err, dockerx.ErrServiceNotFound) {
			return err
		}
		removed = true
		return nil
	})
	if err != nil || !removed {
		return false, err
	}
	e.r.Output("Removed the keepalived service an earlier switch-on left behind.")
	left, err := s.waitKeepalivedGone(ctx, e.envID, VIPDisableTaskDeadline, nil)
	if err != nil {
		return false, err
	}
	if left > 0 {
		return false, fail(CodeVIPSecretFailed, errors.New("keepalived tasks remain"),
			strconv.Itoa(left)+" task(s) of the earlier keepalived service were still running "+VIPDisableTaskDeadline.String()+" after it was removed.")
	}
	return true, nil
}

// makeService creates the global keepalived service, or, when one that is TierPlan's is there already (the leftover of a
// failed enable), updates it to the spec.
func (e *vipRun) makeService(ctx context.Context) error {
	s := e.s
	auth, err := s.authFor(ctx, e.envID, e.image)
	if err != nil {
		return fail(CodeVIPServiceFailed, err)
	}
	spec := KeepalivedServiceSpec(KeepalivedSpecInput{
		Image: e.image, Address: e.pv.Address, RouterID: e.pv.RouterID, SecretName: e.secretName,
	})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = e.secretID
	raw, err := json.Marshal(spec)
	if err != nil {
		return fail(CodeVIPServiceFailed, err)
	}
	err = s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case err == nil:
			if !ingress.OwnsKeepalivedService(cur.Name, cur.Labels) {
				return &stepError{code: CodeVIPServiceFailed, err: errors.New("the swarm holds a service named " + cur.Name + " that is not TierPlan's"),
					detail: "A service named " + cur.Name + " already exists and is not TierPlan's. Remove or rename it, then switch the virtual IP on again."}
			}
			_, err = c.ServiceUpdate(ctx, cur.ID, cur.Version, raw, auth)
			return err
		case errors.Is(err, dockerx.ErrServiceNotFound):
			_, _, err = c.ServiceCreate(ctx, raw, auth)
			return err
		default:
			return err
		}
	})
	if err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return se
		}
		return fail(CodeVIPServiceFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemVIPService, map[string]string{"image": e.image})
	return nil
}

// switchOn stores the virtual IP's row and writes the audit entry in the same transaction, so the row never exists without
// its audit entry nor the entry without the row. The key is in the row only as the vault sealed it.
func (e *vipRun) switchOn(ctx context.Context) error {
	s := e.s
	pfx := e.pv.Address
	cfg := ingressstore.VIPConfig{
		EnvID: e.envID, Address: pfx.Addr(), PrefixLen: pfx.Bits(), RouterID: e.pv.RouterID, RouterDerived: e.pv.RouterDerived,
		KeySealed: e.sealed, KeyGeneration: vipFirstGeneration, Enabled: true,
	}
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: e.actor, Action: audit.ActionIngressVIPEnabled, TargetType: "environment", TargetID: e.envID,
			EnvID: e.envID, Result: audit.ResultSuccess,
			Details: map[string]string{
				"address": pfx.String(), "router_id": strconv.Itoa(e.pv.RouterID), "generation": strconv.Itoa(vipFirstGeneration),
			},
		})
	}
	if err := s.d.Store.PutVIPWithHook(ctx, cfg, hook); err != nil {
		return fail(CodeVIPSwitchFailed, err)
	}
	e.release()
	e.done(ItemVIPSwitch, nil)
	return nil
}
