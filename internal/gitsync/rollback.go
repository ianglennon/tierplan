// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// RolledBack is told that a person rolled a stack back and the revision of the roll back exists (D-22). A stack made from
// a repository whose automatic deploys are on would otherwise deploy the repository head over the restored version at the
// next check, so the roll back and the pause are one action: auto-deploy is turned off with the pause reason rollback
// and the number of the revision restored, the automation that ran it is forgotten, the state notes the event for the
// bell and the audit log gets git.autodeploy_paused. The stack's repository, ref, folder and compose files are kept
// (D-28); the head then shows as pending once the roll back has deployed (Deployed). Only a person turning auto-deploy
// on again resumes it.
//
// A stack whose automatic deploys are off already, or that is not from a repository, changes nothing, except that a
// stack paused for an earlier roll back is told the number of the newest one. A failure is logged and never fails the
// roll back that is under way.
func (s *Service) RolledBack(ctx context.Context, st stackops.Stack, rev stackops.Snapshot, actor audit.Actor) {
	sg, err := s.repo.StackGit(ctx, st.ID)
	if errors.Is(err, stackops.ErrNoGit) {
		return
	}
	if err != nil {
		s.log.Warn("could not read the git settings for a roll back", "stack", st.ID, "error", err)
		return
	}
	restored := rev.RestoredFrom
	switch {
	case sg.AutoDeploy:
		// On (running, or paused for its owner): the roll back pauses it.
	case sg.AutoDeployPausedReason == stackops.PausedRollback:
		// Off after an earlier roll back: the alert names the newest one, and nothing else changes.
		if sg.AutoDeployPausedSeq == restored {
			return
		}
		if err := s.repo.SetAutoDeploy(ctx, st.ID, false, stackops.PausedRollback, restored, nil); err != nil {
			s.log.Warn("could not note the revision a stack was rolled back to", "stack", st.ID, "error", err)
		}
		return
	default:
		return
	}

	err = s.repo.SetAutoDeploy(ctx, st.ID, false, stackops.PausedRollback, restored, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		if s.access == nil {
			return nil
		}
		return s.access.DeregisterOwnedTx(ctx, tx, q, KindGitAutoDeploy, st.ID)
	})
	if err != nil {
		s.log.Warn("could not pause the automatic deploys of a stack that was rolled back", "stack", st.ID, "error", err)
		return
	}
	s.setEvent(ctx, st.ID, stackops.GitEventPaused, "Rolled back to #"+strconv.FormatInt(restored, 10))
	if s.audit == nil {
		return
	}
	err = s.audit.Record(ctx, audit.Entry{
		Actor: actor, Action: audit.ActionGitAutoDeployPaused, TargetType: "stack", TargetID: st.ID, EnvID: st.EnvID,
		Result: audit.ResultSuccess,
		Details: map[string]string{
			"stack": st.Name, "reason": stackops.PausedRollback, "revision": strconv.FormatInt(restored, 10),
		},
	})
	if err != nil {
		s.log.Error("could not audit the pause of a stack's automatic deploys", "stack", st.ID, "error", err)
	}
}
