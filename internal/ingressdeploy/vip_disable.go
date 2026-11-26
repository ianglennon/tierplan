// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// JobKindVIPDisable is the kind of the job that switches an environment's virtual IP off.
const JobKindVIPDisable = "ingress_vip_disable"

// The items of the virtual IP switch-off, in the order they run (D-16, D-17).
const (
	ItemVIPRemoveService = "vip_remove_service"
	ItemVIPWaitTasks     = "vip_wait_tasks"    //nolint:gosec // G101: an item name that holds the letters "pw", not a credential
	ItemVIPRemoveSecret  = "vip_remove_secret" //nolint:gosec // G101: an item name, not a credential
	ItemVIPForget        = "vip_forget"
)

// CodeVIPRemoveFailed is the code an item of the switch-off ends with when it fails. Whatever it left in the swarm is named
// in the item's sentence, and the row stays, switched off, so the work is finished later (PROH-26-06).
const CodeVIPRemoveFailed = "vip_remove_failed"

// CodeVIPNotEnabled is the refusal of a change or a switch-off of an environment that has no virtual IP. The methods answer
// it as ErrVIPNotEnabled.
const CodeVIPNotEnabled = "vip_not_enabled"

// VIPDisableTaskDeadline is how long a switch-off waits, on the injected clock, for the keepalived tasks to end, so that
// each holder's SIGTERM has sent its priority-0 advert and dropped the address (D-16).
const VIPDisableTaskDeadline = 60 * time.Second

// vipDisableItems are the items of the switch-off in order, and vipDisableLabels their lines.
var (
	vipDisableItems  = []string{ItemVIPRemoveService, ItemVIPWaitTasks, ItemVIPRemoveSecret, ItemVIPForget}
	vipDisableLabels = map[string]string{ //nolint:gosec // G101: item names and their lines, not credentials
		ItemVIPRemoveService: "Remove the virtual IP service",
		ItemVIPWaitTasks:     "Wait for every node to release the address",
		ItemVIPRemoveSecret:  "Remove the virtual IP key",
		ItemVIPForget:        "Switch the virtual IP off",
	}
	// vipDisableMessages are the sentences of a failed item, built here and never from an engine's answer.
	vipDisableMessages = map[string]string{ //nolint:gosec // G101: item names and their sentences, not credentials
		ItemVIPRemoveService: "The keepalived service could not be removed.",
		ItemVIPWaitTasks:     "The keepalived tasks did not all stop in time, so an address may still be held.",
		ItemVIPRemoveSecret:  "The virtual IP key could not be removed from the swarm.",
		ItemVIPForget:        "The virtual IP could not be switched off.",
	}
)

// vipLeftText is what a person is told when a switch-off ended with something left in the swarm.
const vipLeftText = "Part of the virtual IP is still in the swarm. Switch it off again to finish removing it."

// VIPDisablePreview is what a switch-off of the virtual IP would remove (D-16). It holds no key.
type VIPDisablePreview struct {
	// Address is the virtual IP the environment has now.
	Address netip.Prefix
	// Service is the name of the keepalived service; empty when the swarm was read and does not hold one of TierPlan's.
	Service string
	// Tasks is how many keepalived tasks run now; zero when the swarm could not be read.
	Tasks int
	// Secrets are the names of the key secrets the swarm holds for the environment; when the swarm could not be read, the
	// name the row's key generation gives.
	Secrets []string
	// Items are the steps the switch-off job runs, as their labels.
	Items []string
}

// vipDisableLabelList is the labels of the switch-off's items in order.
func vipDisableLabelList() []string {
	out := make([]string, 0, len(vipDisableItems))
	for _, id := range vipDisableItems {
		out = append(out, vipDisableLabels[id])
	}
	return out
}

// PreviewVIPDisable says what switching the virtual IP off would remove and writes nothing. An environment with no virtual IP
// is ErrVIPNotEnabled. A switch-off needs no manager gate: removing a service is not an escalation (D-19).
func (s *Service) PreviewVIPDisable(ctx context.Context, envID string) (VIPDisablePreview, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return VIPDisablePreview{}, err
	}
	cfg, err := s.d.Store.GetVIP(ctx, envID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return VIPDisablePreview{}, ErrVIPNotEnabled
	}
	if err != nil {
		return VIPDisablePreview{}, err
	}
	pv := VIPDisablePreview{
		Address: cfg.Prefix(), Service: ingress.ServiceNameKeepalived, Items: vipDisableLabelList(),
		Secrets: []string{KeepalivedSecretName(envID, cfg.KeyGeneration)},
	}
	if !info.ManagerConnected {
		return pv, nil
	}
	var (
		svc     *dockerx.ServiceDetail
		secrets []dockerx.SecretRow
		tasks   int
	)
	err = s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case err == nil:
			svc = &cur
		case errors.Is(err, dockerx.ErrServiceNotFound):
		default:
			return err
		}
		if secrets, err = c.SecretList(ctx); err != nil {
			return err
		}
		tasks, err = componentTasksLeft(ctx, c, ingress.ComponentKeepalived)
		return err
	})
	if err != nil {
		s.log().Warn("could not read the swarm for a virtual IP switch-off preview", "environment", envID, "error", err)
		return pv, nil
	}
	if svc == nil || !ingress.OwnsKeepalivedService(svc.Name, svc.Labels) {
		pv.Service = ""
	}
	pv.Tasks = tasks
	pv.Secrets = vipSecretNames(secrets, envID)
	return pv, nil
}

// DisableVIP switches the environment's virtual IP off: it takes the environment's operation lock, checks that there is a
// virtual IP, and starts a job that removes the keepalived service, waits for every holder to release the address, removes the
// key secret and deletes the row with its audit entry. It returns the job's ID. A held lock is a *LockedError and an environment
// with no virtual IP is ErrVIPNotEnabled; in each case nothing was written. A failed step ends its item with vip_remove_failed
// and the later ones as not run, and leaves the row switched off, so calling DisableVIP again, or the ensure loop, finishes it.
func (s *Service) DisableVIP(ctx context.Context, actor audit.Actor, owner, envID string) (string, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return "", err
	}
	specs := make([]jobs.ItemSpec, 0, len(vipDisableItems))
	for _, id := range vipDisableItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: vipDisableLabels[id], Kind: "step"})
	}
	pre := func(ctx context.Context, _ EnvInfo, _ EnableRequest) ([]Finding, error) {
		if _, err := s.d.Store.GetVIP(ctx, envID); errors.Is(err, ingressstore.ErrVIPNotFound) {
			return nil, ErrVIPNotEnabled
		} else if err != nil {
			return nil, err
		}
		return nil, nil
	}
	return s.startEnable(ctx, JobKindVIPDisable, owner, info, EnableRequest{}, pre, specs,
		func(jctx context.Context, r *jobs.Reporter, release func()) {
			// A step that fails or is cancelled is audited as it ends its item (A-01); the ensure loop's removal is not.
			onFail := func(item, code string) {
				s.auditJobFailed(jctx, actor, audit.ActionIngressVIPDisabled, envID, item, code)
			}
			if err := s.removeVIPAudited(jctx, actor, envID, r, release, onFail); err != nil {
				s.log().Warn("a virtual IP switch-off did not finish", "environment", envID, "error", err)
				if r != nil && jctx.Err() == nil {
					r.Output(vipLeftText)
				}
			}
		})
}

// componentTasksLeft counts the swarm's tasks of a component that have not ended. A task keeps the labels its service had, so
// the component label finds the tasks of a service that is already removed.
func componentTasksLeft(ctx context.Context, c *dockerx.Client, component string) (int, error) {
	rows, err := c.Tasks(ctx, dockerx.TaskFilter{Labels: []string{ingress.LabelComponent + "=" + component}})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range rows {
		if !taskEnded(t.State) {
			n++
		}
	}
	return n, nil
}

// vipSecretNames are the names of the key secrets of TierPlan's the swarm holds for the environment, sorted: every
// generation, since a name ends with the environment's tag.
func vipSecretNames(rows []dockerx.SecretRow, envID string) []string {
	out := []string{}
	for _, row := range vipSecretsOf(rows, envID) {
		out = append(out, row.Name)
	}
	slices.Sort(out)
	return out
}

// vipSecretsOf are the secrets that are the environment's virtual IP key: TierPlan's keepalived label, the reserved prefix and
// the environment's tag at the end of the name. A secret of another environment is never one of them.
func vipSecretsOf(rows []dockerx.SecretRow, envID string) []dockerx.SecretRow {
	suffix := "-" + agents.SecretTag(envID)
	var out []dockerx.SecretRow
	for _, row := range rows {
		if ingress.OwnsKeepalivedSecret(row.Name, row.Labels) && strings.HasSuffix(row.Name, suffix) {
			out = append(out, row)
		}
	}
	return out
}

// waitKeepalivedGone polls on the injected clock until no keepalived task runs, until the deadline has passed, until ctx ends
// or until reads failed disableReadFailures times in a row. It reports how many ran at the end, 0 when none. poll, when set, is
// called after every read that found tasks and once more when none are left, so the reader sees the last lines of the holders
// before their containers go. It writes nothing of its own.
func (s *Service) waitKeepalivedGone(ctx context.Context, envID string, deadline time.Duration, poll func(context.Context)) (int, error) {
	clock := s.d.Clock
	end := clock.Now().Add(deadline)
	failures := 0
	for {
		var left int
		err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
			left, err = componentTasksLeft(ctx, c, ingress.ComponentKeepalived)
			return err
		})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if failures++; failures >= disableReadFailures {
				return 0, err
			}
		case left == 0:
			if poll != nil {
				poll(ctx)
			}
			return 0, nil
		default:
			failures = 0
			if poll != nil {
				poll(ctx)
			}
			if !clock.Now().Before(end) {
				return left, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-clock.After(stackops.PollInterval):
		}
	}
}

// vipRemoval is the report of one switch-off to the job it runs in. r is nil when no job is watching (the ensure loop), and
// an item the job does not have is ignored by the reporter, so the ingress switch-off's job takes the same calls.
type vipRemoval struct {
	r       *jobs.Reporter
	release func()
	// onFail, when set, is told the item and the code of a step that failed or was cancelled, before the item ends.
	onFail func(item, code string)
}

// record tells onFail of a step that failed or was cancelled.
func (v *vipRemoval) record(item, code string) {
	if v.onFail != nil {
		v.onFail(item, code)
	}
}

// freeLock frees the environment's operation lock before the item that ends the job is finished, so a reader that sees the
// job done finds the lock free.
func (v *vipRemoval) freeLock() {
	if v.release != nil {
		v.release()
	}
}

func (v *vipRemoval) running(item string) {
	if v.r != nil {
		v.r.Update(item, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	}
}

func (v *vipRemoval) done(item string, result map[string]string) {
	if v.r != nil {
		v.r.Finish(item, jobs.ItemDone, "", "", result)
	}
}

// failed ends the item with vip_remove_failed and the sentence of the item, and the engine's own words when detail is set.
func (v *vipRemoval) failed(item, detail string) {
	v.record(item, CodeVIPRemoveFailed)
	if v.r == nil {
		return
	}
	if detail != "" {
		v.r.Detail(item, detail)
	}
	v.r.Finish(item, jobs.ItemFailed, CodeVIPRemoveFailed, vipDisableMessages[item], nil)
}

// skip ends the items from index i on without running them, or cancelled when the job was.
func (v *vipRemoval) skip(from int, state, code string) {
	if v.r == nil {
		return
	}
	for _, id := range vipDisableItems[from:] {
		msg := codeMessages[code]
		if code == jobs.CodeCancelled {
			msg = ""
		}
		v.r.Finish(id, state, code, msg, nil)
	}
}

// removeVIP switches the environment's virtual IP off and leaves nothing behind: it marks the row switched off and the
// reader as disabling, removes the keepalived service, waits (VIPDisableTaskDeadline) until no keepalived task runs so every
// holder has dropped the address, removes every key secret of the environment, and deletes the row with ingress.vip_disabled
// in one transaction. It is the one removal the VIP switch-off, the ingress switch-off and the ensure loop share (D-17).
//
// r reports each step as the items of the VIP switch-off; it may be nil. release frees the operation lock the caller holds
// before the job's last item ends; it may be nil. A step that fails ends its item with vip_remove_failed and leaves the row with
// enabled 0, which a later call finishes. A wait that ran out is not the end: the secret is still tried, and the row is kept.
// An environment with no virtual IP is ErrVIPNotEnabled. The caller holds the environment's operation lock.
func (s *Service) removeVIP(ctx context.Context, actor audit.Actor, envID string, r *jobs.Reporter, release func()) error {
	return s.removeVIPAudited(ctx, actor, envID, r, release, nil)
}

// removeVIPAudited is removeVIP that also tells onFail, when set, of each step that fails or is cancelled, just before the
// step's item ends, so the switch-off's own job can write the audit row of a failed run.
func (s *Service) removeVIPAudited(ctx context.Context, actor audit.Actor, envID string, r *jobs.Reporter, release func(), onFail func(item, code string)) error {
	v := &vipRemoval{r: r, release: release, onFail: onFail}
	cfg, err := s.d.Store.GetVIP(ctx, envID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return ErrVIPNotEnabled
	}
	if err != nil {
		v.freeLock()
		v.failed(ItemVIPRemoveService, "")
		v.skip(1, jobs.ItemSkipped, CodeNotRun)
		return err
	}
	if err := s.d.Store.SetVIPEnabled(ctx, envID, false); err != nil {
		v.freeLock()
		v.failed(ItemVIPRemoveService, "")
		v.skip(1, jobs.ItemSkipped, CodeNotRun)
		return err
	}
	// From here a holder that stops is the switch-off's doing, and the reader keeps reading until the row is gone.
	s.markVIPDisabling(envID, true)
	defer s.markVIPDisabling(envID, false)

	// stop ends the run at the item that failed or was cancelled.
	stop := func(i int, err error, detail string) error {
		v.freeLock()
		if ctx.Err() != nil {
			v.record(vipDisableItems[i], jobs.CodeCancelled)
			if r != nil {
				r.Finish(vipDisableItems[i], jobs.ItemCancelled, jobs.CodeCancelled, "", nil)
			}
			v.skip(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
			return ctx.Err()
		}
		v.failed(vipDisableItems[i], detail)
		v.skip(i+1, jobs.ItemSkipped, CodeNotRun)
		return err
	}

	v.running(ItemVIPRemoveService)
	removed := false
	err = s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case errors.Is(err, dockerx.ErrServiceNotFound):
			return nil
		case err != nil:
			return err
		case !ingress.OwnsKeepalivedService(cur.Name, cur.Labels):
			if r != nil {
				r.Output("Left the service " + cur.Name + " in place: it is not TierPlan's.")
			}
			return nil
		}
		if err := c.ServiceRemove(ctx, cur.ID); err != nil && !errors.Is(err, dockerx.ErrServiceNotFound) {
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		return stop(0, err, dockerx.DaemonText(err))
	}
	v.done(ItemVIPRemoveService, map[string]string{"removed": strconv.FormatBool(removed)})

	// The wait does not end the run when it runs out: the key secret is still tried, and the row is kept.
	v.running(ItemVIPWaitTasks)
	var waitErr error
	left, err := s.waitKeepalivedGone(ctx, envID, VIPDisableTaskDeadline, func(ctx context.Context) { s.readVIPOnce(ctx, envID) })
	switch {
	case err != nil && ctx.Err() != nil:
		return stop(1, err, "")
	case err != nil:
		waitErr = err
		v.failed(ItemVIPWaitTasks, "The swarm's tasks could not be read, so it is not known that every node released the address.")
	case left > 0:
		waitErr = errors.New("keepalived tasks remain")
		v.failed(ItemVIPWaitTasks, strconv.Itoa(left)+" keepalived task(s) were still running after "+VIPDisableTaskDeadline.String()+".")
	default:
		v.done(ItemVIPWaitTasks, nil)
	}

	v.running(ItemVIPRemoveSecret)
	gone := 0
	err = s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		rows, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, row := range vipSecretsOf(rows, envID) {
			switch err := c.SecretRemove(ctx, row.ID); {
			case err == nil:
				gone++
			case errors.Is(err, dockerx.ErrObjectNotFound):
			default:
				return err
			}
		}
		return nil
	})
	if err != nil {
		// The engine's words are not shown: an error that touched the key is never put in a job.
		if waitErr != nil {
			err = errors.Join(waitErr, err)
		}
		return stop(2, err, "")
	}
	v.done(ItemVIPRemoveSecret, map[string]string{"removed": strconv.Itoa(gone)})
	if waitErr != nil {
		// A holder may still hold the address: the row stays, switched off, for the next try.
		v.freeLock()
		v.skip(3, jobs.ItemSkipped, CodeNotRun)
		return waitErr
	}

	v.running(ItemVIPForget)
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressVIPDisabled, TargetType: "environment", TargetID: envID,
			EnvID: envID, Result: audit.ResultSuccess,
			Details: map[string]string{
				"address": cfg.Prefix().String(), "router_id": strconv.Itoa(cfg.RouterID), "generation": strconv.Itoa(cfg.KeyGeneration),
			},
		})
	}
	if _, err := s.d.Store.DeleteVIPWithHook(ctx, envID, hook); err != nil {
		return stop(3, err, "")
	}
	v.freeLock()
	v.done(ItemVIPForget, nil)
	return nil
}
