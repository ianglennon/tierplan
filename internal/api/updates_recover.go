// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/updates"
)

// RecoverStackUpdate is the seam startup recovery (updates.Recovery, plan 12-24, IMG-07, D-12) uses for an update of a stack that a
// restart interrupted. It does what the automatic rollback of a failed update does (rollbackFailedUpdate): the newest good revision
// before the update is copied as a "rollback of #N" revision and deployed with its own images, platforms and subnets, for every
// service together, in a stack job of its own that holds the environment's lock. The run ends rolled_back or rollback_failed with
// the restart as its reason, the failure is remembered and the audit rows are written, all as for any failed update.
//
// It acts as the run's own actor: the user who asked, judged as of now, or the system principal that did. It answers nil when the
// stack was put back or the update turned out to have been deployed whole (then the run is ended failed and nothing is written to
// the engine), an *updates.LockedError when the environment's lock is held, and any other error when the rollback could not be made;
// the run is ended by then, or is left to the caller.
func RecoverStackUpdate(d StacksDeps) func(ctx context.Context, run updates.Run) error {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return func(ctx context.Context, run updates.Run) error { return recoverStackUpdate(ctx, d, run) }
}

// itemRecoveryRecord is the last step of the job of a recovery, finished once the run is on record and the lock is free.
const itemRecoveryRecord = "record"

func recoverStackUpdate(ctx context.Context, d StacksDeps, run updates.Run) error {
	if d.Jobs == nil || d.Locks == nil || d.Envs == nil || d.Updates == nil || d.Updates.UpdateRuns == nil {
		return errors.New("stack updates are not wired for recovery")
	}
	env, err := d.Envs.Resolve(ctx, run.EnvID)
	if err != nil {
		return fmt.Errorf("resolve the environment of an interrupted stack update: %w", err)
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, run.EnvID, run.StackID)
	if err != nil {
		return fmt.Errorf("read the stack of an interrupted update: %w", err)
	}
	p, err := recoveryPrincipal(ctx, repo, run.Actor)
	if err != nil {
		return err
	}
	actor := p.AuditActor()
	ap := &updates.Applier{Runs: d.Updates.UpdateRuns, Failures: d.Updates.Failures}
	plan := updates.StackPlan{}
	standalone := st.Target == stacks.TargetStandalone
	for _, it := range run.Items {
		plan.Targets = append(plan.Targets, updates.StackTarget{Key: it.Service, Image: it.Image, From: it.FromDigest, To: it.ToDigest, Swarm: !standalone})
	}

	good, ok, err := repo.LatestGoodRevision(ctx, st.ID)
	switch {
	case err != nil:
		return fmt.Errorf("read the running revision of an interrupted update: %w", err)
	case !ok:
		return errors.New("the stack has no revision to put back")
	case run.RevisionSeq > 0 && good.Seq >= run.RevisionSeq:
		// The update's own revision was recorded good before the restart: the whole update was deployed. Nothing is rolled back and
		// nothing is written to the engine; the run says the restart ended it.
		wctx, cancel := doneActionAuditContext(ctx)
		defer cancel()
		endStackRun(wctx, d, ap, actor, "", st, run, plan, stackEnd{
			state: updates.RunFailed, reason: updates.ReasonServerRestarted, cause: updates.ReasonServerRestarted, revision: run.RevisionSeq,
		})
		return nil
	}
	goodPins, err := repo.RevisionPins(ctx, good.ID)
	if err != nil {
		return fmt.Errorf("read the pins of the revision to put back: %w", err)
	}
	node := ""
	if standalone {
		if node, err = d.ops().StandaloneNode(ctx, env.ID); err != nil {
			return fmt.Errorf("find the host of an interrupted stack update: %w", err)
		}
	}

	release, held, taken := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackID: st.ID, StackName: st.Name, Kind: deployModeRollback, By: p.Name, Started: time.Now().UTC(),
	})
	if !taken {
		return &updates.LockedError{Holder: held}
	}
	rb := stackRollback{
		d: d, ap: ap, p: p, env: &env, st: st, good: good, goodPins: goodPins, failed: stackops.Snapshot{Seq: run.RevisionSeq}, run: run,
		plan: plan, actor: actor, extra: deployOpts{}.auditExtras(p), standalone: standalone, node: node,
	}
	meta := map[string]string{
		metaStackID: st.ID, metaStackName: st.Name, metaMode: deployModeRollback, metaStartedBy: p.Name, metaEnvName: env.Name,
		metaRevision: itoa(good.Seq),
	}
	var rbErr error
	job, err := d.Jobs.Start(jobs.Spec{
		Kind: stackDeployJobKind, EnvID: env.ID, Owner: stackJobOwner(p), Meta: meta,
		Items: []jobs.ItemSpec{
			{ID: updates.ItemRollback, Label: "Roll back to revision #" + itoa(good.Seq), Kind: stackops.KindStep},
			{ID: updates.ItemRollbackWait, Label: "Wait for the previous version to run", Kind: stackops.KindStep},
			// Held until the run is ended and the lock is free, so the job reads finished only then, as a deploy's does.
			{ID: itemRecoveryRecord, Label: "Record the outcome", Kind: stackops.KindStep},
		},
	}, func(jctx context.Context, r *jobs.Reporter) {
		defer d.driftRecheck(env.ID, st.ID)
		defer release()
		defer r.Finish(itemRecoveryRecord, jobs.ItemDone, "", "", nil)
		r.Output("TierPlan restarted during this update.")
		rbErr = rollbackFailedUpdate(jctx, rb, r, updates.ReasonServerRestarted)
		release()
	})
	if err != nil {
		release()
		return err
	}
	d.Locks.Attach(env.ID, job.ID)
	if _, err := d.Jobs.Wait(ctx, job.ID); err != nil {
		return fmt.Errorf("wait for the rollback of an interrupted stack update: %w", err)
	}
	return rbErr
}

// recoveryPrincipal is who an interrupted update is rolled back as: the user the run kept, as a user principal, so the stack's policy
// is judged for them as of now; or the system principal it names.
func recoveryPrincipal(ctx context.Context, repo stackops.Repo, key string) (*rbac.Principal, error) {
	if key == "" {
		return nil, errors.New("an interrupted update has no actor")
	}
	if strings.HasPrefix(key, "system:") {
		p := rbac.System(key)
		if p == nil {
			return nil, fmt.Errorf("an interrupted update was started by an unknown system principal %q", key)
		}
		return p, nil
	}
	// A user is named by their username, as a signed-in principal is.
	name := key
	if u, err := repo.Store.RQ.GetUserByID(ctx, key); err == nil && u.Username != "" {
		name = u.Username
	}
	return &rbac.Principal{Type: rbac.PrincipalUser, ID: key, Name: name}, nil
}
