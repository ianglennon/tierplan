// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The record of every image update (IMG-06). A run is written before anything touches an engine, found
// again while it runs (the Updating and Busy states, restart recovery) and ended exactly once: a late
// writer can never overwrite a terminal state. A run holds no credential.

// RunState is where an update run is.
type RunState string

// The states of a run (D-14). Queued and running are not over; the rest are terminal.
const (
	RunQueued         RunState = "queued"
	RunRunning        RunState = "running"
	RunDone           RunState = "done"
	RunFailed         RunState = "failed"
	RunRolledBack     RunState = "rolled_back"
	RunRollbackFailed RunState = "rollback_failed"
	RunRefused        RunState = "refused"
)

// RunItem is one service or container a run updates: its image and the digest it ran and now runs.
type RunItem struct {
	Service    string `json:"service"`
	Image      string `json:"image"`
	FromDigest string `json:"from_digest"`
	ToDigest   string `json:"to_digest"`
	// WriteVersion is, for a plain service, the version of the service (the swarm's Meta.Version index) as the run read it for its
	// write, recorded before the write is made. A write made at a version moves it, so a service still at this version was not
	// written, and a run without one never reached the write. Nil for a run that has not recorded one, and for the other kinds.
	WriteVersion *uint64 `json:"write_version,omitempty"`
}

// Run is one image update.
type Run struct {
	ID, EnvID string
	// BulkID ties the runs of one bulk update together; empty for a single update.
	BulkID string
	// Kind is "service", "container" or "stack".
	Kind                          string
	Workload, WorkloadID, StackID string
	Items                         []RunItem
	State                         RunState
	Reason, Actor, JobID          string
	// RevisionSeq and RollbackSeq are the revision sequence numbers the update wrote and a rollback restored.
	RevisionSeq, RollbackSeq int64
	// OldContainer is the ID of the container a container update replaced.
	OldContainer          string
	StartedAt, FinishedAt time.Time
}

// RunStore keeps update runs in SQLite. Q runs on the write connection W, like SQLStore. Now stamps a run
// that has no StartedAt and the end of a run; the wall clock when nil.
type RunStore struct {
	Q   *sqlc.Queries
	W   *sql.DB
	Now func() time.Time
}

func (s RunStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Insert records a run. The caller gives its ID, so the job that carries it can name it before it starts.
func (s RunStore) Insert(ctx context.Context, r Run) error {
	if r.ID == "" {
		return errors.New("record an image update run: it has no ID")
	}
	items := r.Items
	if items == nil {
		items = []RunItem{}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("encode the items of an image update run: %w", err)
	}
	started := r.StartedAt
	if started.IsZero() {
		started = s.now()
	}
	var finished int64
	if !r.FinishedAt.IsZero() {
		finished = r.FinishedAt.UnixNano()
	}
	if err := s.Q.InsertImageUpdateRun(ctx, sqlc.InsertImageUpdateRunParams{
		ID: r.ID, EnvID: r.EnvID, BulkID: r.BulkID, Kind: r.Kind, Workload: r.Workload, WorkloadID: r.WorkloadID,
		StackID: r.StackID, Items: string(raw), State: string(r.State), Reason: r.Reason, Actor: r.Actor,
		JobID: r.JobID, RevisionSeq: r.RevisionSeq, RollbackSeq: r.RollbackSeq, OldContainer: r.OldContainer,
		StartedAt: started.UnixNano(), FinishedAt: finished,
	}); err != nil {
		return fmt.Errorf("record an image update run: %w", err)
	}
	return nil
}

// Begin starts a run that waited in a bulk update (plan 12-30): it is running from now, with the items it has now (the images and
// digests the registry gave when its turn came) and the revision a stack's update wrote. It reports whether it did. A run that is
// not queued any more (it ended, or was begun already) is left as it is and the answer is false.
func (s RunStore) Begin(ctx context.Context, r Run) (bool, error) {
	items := r.Items
	if items == nil {
		items = []RunItem{}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return false, fmt.Errorf("encode the items of an image update run: %w", err)
	}
	started := r.StartedAt
	if started.IsZero() {
		started = s.now()
	}
	n, err := s.Q.BeginQueuedImageUpdateRun(ctx, sqlc.BeginQueuedImageUpdateRunParams{
		WorkloadID: r.WorkloadID, Items: string(raw), RevisionSeq: r.RevisionSeq, StartedAt: started.UnixNano(), ID: r.ID,
	})
	if err != nil {
		return false, fmt.Errorf("begin a queued image update run: %w", err)
	}
	return n > 0, nil
}

// SetItems replaces the items of a run that is not over and reports whether it did. A run that has ended is left as it is.
func (s RunStore) SetItems(ctx context.Context, id string, items []RunItem) (bool, error) {
	raw, err := json.Marshal(items)
	if err != nil {
		return false, fmt.Errorf("encode the items of an image update run: %w", err)
	}
	n, err := s.Q.SetImageUpdateRunItems(ctx, sqlc.SetImageUpdateRunItemsParams{Items: string(raw), ID: id})
	if err != nil {
		return false, fmt.Errorf("record the items of an image update run: %w", err)
	}
	return n > 0, nil
}

// SetState moves a run that is not over to st and reports whether it did. A run that has ended is left
// as it is and the answer is false.
func (s RunStore) SetState(ctx context.Context, id string, st RunState) (bool, error) {
	n, err := s.Q.SetImageUpdateRunState(ctx, sqlc.SetImageUpdateRunStateParams{State: string(st), ID: id})
	if err != nil {
		return false, fmt.Errorf("set the state of an image update run: %w", err)
	}
	return n > 0, nil
}

// SetOldContainer records the ID of the container a container update replaces, before it is stopped, and
// reports whether it did. A run that has ended is left as it is.
func (s RunStore) SetOldContainer(ctx context.Context, id, containerID string) (bool, error) {
	n, err := s.Q.SetImageUpdateRunOldContainer(ctx, sqlc.SetImageUpdateRunOldContainerParams{OldContainer: containerID, ID: id})
	if err != nil {
		return false, fmt.Errorf("record the old container of an image update run: %w", err)
	}
	return n > 0, nil
}

// Finish ends a run in st with the reason and the revisions it wrote, and reports whether it did. It
// writes once: a run that has already ended is left as it is and the answer is false. A revision of 0 leaves the one the run has, so an
// end that does not know it cannot erase the revision a stack update wrote.
func (s RunStore) Finish(ctx context.Context, id string, st RunState, reason string, revisionSeq, rollbackSeq int64) (bool, error) {
	n, err := s.Q.FinishImageUpdateRun(ctx, sqlc.FinishImageUpdateRunParams{
		State: string(st), Reason: reason, FinishedAt: s.now().UnixNano(), RevisionSeq: revisionSeq, RollbackSeq: rollbackSeq, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("finish an image update run: %w", err)
	}
	return n > 0, nil
}

// Running returns the runs of one environment that are queued or running, oldest first.
func (s RunStore) Running(ctx context.Context, envID string) ([]Run, error) {
	rows, err := s.Q.RunningImageUpdateRuns(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the image update runs that are running: %w", err)
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		run, err := runFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// Unfinished returns the runs of every environment that are queued or running and have not ended, oldest first. After a restart
// each of them belongs to a process that is gone (startup recovery, plan 12-24).
func (s RunStore) Unfinished(ctx context.Context) ([]Run, error) {
	rows, err := s.Q.UnfinishedImageUpdateRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the image update runs that did not end: %w", err)
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		run, err := runFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// RevisionSeqs returns, for one stack, the sequence numbers of the revisions that image updates made, as a set. History
// labels those revisions "Image update"; a revision of the older Update images deploy is not among them.
func (s RunStore) RevisionSeqs(ctx context.Context, stackID string) (map[int64]bool, error) {
	seqs, err := s.Q.ImageUpdateRevisionSeqs(ctx, stackID)
	if err != nil {
		return nil, fmt.Errorf("read the revisions image updates made: %w", err)
	}
	out := make(map[int64]bool, len(seqs))
	for _, q := range seqs {
		out[q] = true
	}
	return out, nil
}

// Get returns one run by ID; sql.ErrNoRows (wrapped) when there is none.
func (s RunStore) Get(ctx context.Context, id string) (Run, error) {
	r, err := s.Q.GetImageUpdateRun(ctx, id)
	if err != nil {
		return Run{}, fmt.Errorf("read an image update run: %w", err)
	}
	return runFromRow(r)
}

func runFromRow(r sqlc.ImageUpdateRun) (Run, error) {
	var items []RunItem
	if err := json.Unmarshal([]byte(r.Items), &items); err != nil {
		return Run{}, fmt.Errorf("decode the items of image update run %s: %w", r.ID, err)
	}
	return Run{
		ID: r.ID, EnvID: r.EnvID, BulkID: r.BulkID, Kind: r.Kind, Workload: r.Workload, WorkloadID: r.WorkloadID,
		StackID: r.StackID, Items: items, State: RunState(r.State), Reason: r.Reason, Actor: r.Actor, JobID: r.JobID,
		RevisionSeq: r.RevisionSeq, RollbackSeq: r.RollbackSeq, OldContainer: r.OldContainer,
		StartedAt: fromNanos(r.StartedAt), FinishedAt: fromNanos(r.FinishedAt),
	}, nil
}
