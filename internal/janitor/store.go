// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The record of the janitor's runs (JAN-10, D-16): a run is a row written before it removes
// anything, and each item it attempted is a row written as it goes.

// The states of a run.
const (
	// StateWaiting: a scheduled run is waiting for the environment's operation lock.
	StateWaiting = "waiting"
	// StateRunning: the run holds the environment and is working.
	StateRunning = "running"
	// StateCompleted: the run ended and every item it attempted ended as it should.
	StateCompleted = "completed"
	// StatePartial: the run ended but an item errored, a node was skipped, or it was cut short.
	StatePartial = "partial"
	// StateSkipped: the run did not run because no node reported.
	StateSkipped = "skipped"
	// StateFailed: the run could not do its work.
	StateFailed = "failed"
)

// The reasons a run gives for not completing, besides ”.
const (
	// ReasonRunFailed: the run could not read the environment or its policy, or could not start.
	ReasonRunFailed = "failed"
	// ReasonServerRestarted: the server stopped while the run was waiting or working, so it was ended at
	// the next start.
	ReasonServerRestarted = "server_restarted"
	// ReasonNoNodeReporting: no node of the environment reported, so there was nothing to judge.
	ReasonNoNodeReporting = "no_node_reporting"
)

// Outcome is how the attempt to remove one item ended.
type Outcome string

// The outcomes of an item.
const (
	// OutcomeRemoved: the object was removed.
	OutcomeRemoved Outcome = "removed"
	// OutcomeRefusedInUse: the engine said the object is in use, so it was left alone.
	OutcomeRefusedInUse Outcome = "refused_in_use"
	// OutcomeSkippedProtected: the object was no longer stale (code no_longer_stale) or was already gone (code gone), so
	// nothing was removed.
	OutcomeSkippedProtected Outcome = "skipped_protected"
	// OutcomeSkippedNodeOffline: its node was not connected, so nothing was sent.
	OutcomeSkippedNodeOffline Outcome = "skipped_node_offline"
	// OutcomeError: the removal failed.
	OutcomeError Outcome = "error"
)

// Counts are what a run counted by how its items ended.
type Counts struct {
	RemovedContainers, RemovedImages, RemovedNetworks int
	// Kept are the selected items left alone: in use, kept by a label or no longer stale.
	Kept int
	// Failed are the items whose removal errored; SkippedOffline the ones not sent because their node was offline.
	Failed, SkippedOffline int
}

// Run is one run of the janitor as it is on record.
type Run struct {
	ID, EnvID string
	// Trigger is TriggerSchedule or TriggerManual; Actor who started it.
	Trigger, Actor string
	// State is one of the State constants; Reason why it did not complete, '' when it did.
	State, Reason string
	JobID         string
	StartedAt     time.Time
	// FinishedAt is the zero time while the run is not over.
	FinishedAt time.Time
	Counts     Counts
	// EstBytes is the bytes the removals freed that were known, -1 when no size was known.
	EstBytes int64
}

// RunItem is one item a run attempted.
type RunItem struct {
	// Seq is 1 for the first item the run attempted, counting up.
	Seq  int
	Type Kind
	// NodeID and Node are the node the object is on; Ref is the engine's ID of it and Name its name.
	NodeID, Node, Ref, Name string
	// Reason is why the planner selected it.
	Reason  Reason
	Outcome Outcome
	// Code is a stable word for why the attempt did not end as a clean removal, '' for one that did.
	Code string
	// Size is the bytes removing the object frees, -1 when not known.
	Size int64
	// HasEvidence is true when the run saved the crash evidence of the container before removing it.
	// It is only read from the record, never written with the item.
	HasEvidence bool
}

// ErrRunNotFound means there is no such run in the environment.
var ErrRunNotFound = errors.New("janitor: no such run")

// ErrEvidenceNotFound means the item has no crash evidence: it is not a container that exited with
// a code other than 0, or there is no such item or run in the environment.
var ErrEvidenceNotFound = errors.New("janitor: no evidence for this item")

// RunStore keeps the record of the runs.
type RunStore interface {
	// CreateRun puts a run on record. It is called before the run removes anything.
	CreateRun(ctx context.Context, run Run) error
	// SetState moves a run that is not over to a state and gives it its job. It changes nothing
	// of a run that has ended.
	SetState(ctx context.Context, runID, state, jobID string) error
	// AddItems records a batch of items in one transaction.
	AddItems(ctx context.Context, runID string, items []RunItem) error
	// FinishRun records the end of the run: its state, reason, finish time, counts and bytes.
	FinishRun(ctx context.Context, run Run) error
	// GetRun answers a run of an environment and its items in the order they were attempted.
	// A run of another environment is ErrRunNotFound.
	GetRun(ctx context.Context, envID, runID string) (Run, []RunItem, error)
	// ListRuns answers the runs of an environment, newest first by start time and then by ID, without
	// their items, at most limit of them.
	ListRuns(ctx context.Context, envID string, limit int) ([]Run, error)
	// CountRuns answers how many runs an environment has on record.
	CountRuns(ctx context.Context, envID string) (int, error)
	// PruneRuns deletes the runs of an environment the retention does not keep, with their items and
	// evidence, in one statement, and answers how many it deleted. A waiting or running run is never
	// deleted.
	PruneRuns(ctx context.Context, envID string, keep Retention, now time.Time) (int, error)
	// PruneAll is PruneRuns for every environment.
	PruneAll(ctx context.Context, keep Retention, now time.Time) (int, error)
	// MarkInterruptedRuns ends every run still waiting or running as failed with the reason
	// ReasonServerRestarted, finished at now, and answers them. Their items are kept and counted. It is
	// for the start of the server, before any run can begin.
	MarkInterruptedRuns(ctx context.Context, now time.Time) ([]Run, error)
	// SaveEvidence records the crash evidence of the item seq of a run, in a transaction of its own.
	// It is called before the container is removed, which is not removed if it fails.
	SaveEvidence(ctx context.Context, runID string, seq int, ev Evidence) error
	// GetEvidence answers the evidence of an item of a run of an environment, or ErrEvidenceNotFound.
	GetEvidence(ctx context.Context, envID, runID string, seq int) (Evidence, error)
}

// SQLStore is the RunStore in SQLite.
type SQLStore struct {
	St *store.Store
}

var _ RunStore = (*SQLStore)(nil)

// CreateRun implements RunStore.
func (s *SQLStore) CreateRun(ctx context.Context, run Run) error {
	err := s.St.Q.InsertJanitorRun(ctx, sqlc.InsertJanitorRunParams{
		ID: run.ID, EnvID: run.EnvID, Trigger: run.Trigger, Actor: run.Actor, State: run.State,
		Reason: run.Reason, JobID: run.JobID, StartedAt: run.StartedAt.UTC().UnixNano(),
	})
	if err != nil {
		return fmt.Errorf("record the janitor run: %w", err)
	}
	return nil
}

// SetState implements RunStore.
func (s *SQLStore) SetState(ctx context.Context, runID, state, jobID string) error {
	if _, err := s.St.Q.SetJanitorRunState(ctx, sqlc.SetJanitorRunStateParams{State: state, JobID: jobID, ID: runID}); err != nil {
		return fmt.Errorf("set the state of the janitor run: %w", err)
	}
	return nil
}

// reasonDetail is the part of a Reason that the code does not carry, as it is stored.
type reasonDetail struct {
	Service          string `json:"service,omitempty"`
	Slot             string `json:"slot,omitempty"`
	Project          string `json:"project,omitempty"`
	Stack            string `json:"stack,omitempty"`
	Node             string `json:"node,omitempty"`
	Rank             int    `json:"rank,omitempty"`
	AgeSeconds       int64  `json:"ageSeconds,omitempty"`
	RemainingSeconds int64  `json:"remainingSeconds,omitempty"`
}

// AddItems implements RunStore. The batch is one transaction on the single writer.
func (s *SQLStore) AddItems(ctx context.Context, runID string, items []RunItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.St.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record the janitor run's items: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.St.Q.WithTx(tx)
	for _, it := range items {
		detail, err := json.Marshal(reasonDetail{
			Service: it.Reason.Service, Slot: it.Reason.Slot, Project: it.Reason.Project, Stack: it.Reason.Stack,
			Node: it.Reason.Node, Rank: it.Reason.Rank,
			AgeSeconds: int64(it.Reason.Age.Seconds()), RemainingSeconds: int64(it.Reason.Remaining.Seconds()),
		})
		if err != nil {
			return fmt.Errorf("encode the reason of a janitor run item: %w", err)
		}
		if err := q.InsertJanitorRunItem(ctx, sqlc.InsertJanitorRunItemParams{
			RunID: runID, Seq: int64(it.Seq), Type: string(it.Type), NodeID: it.NodeID, NodeName: it.Node,
			Ref: it.Ref, Name: it.Name, Reason: string(it.Reason.Code), ReasonDetail: string(detail),
			Outcome: string(it.Outcome), Code: it.Code, EstBytes: it.Size,
		}); err != nil {
			return fmt.Errorf("record a janitor run item: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record the janitor run's items: %w", err)
	}
	return nil
}

// FinishRun implements RunStore.
func (s *SQLStore) FinishRun(ctx context.Context, run Run) error {
	n, err := s.St.Q.FinishJanitorRun(ctx, sqlc.FinishJanitorRunParams{
		State: run.State, Reason: run.Reason, FinishedAt: run.FinishedAt.UTC().UnixNano(),
		RemovedContainers: int64(run.Counts.RemovedContainers), RemovedImages: int64(run.Counts.RemovedImages),
		RemovedNetworks: int64(run.Counts.RemovedNetworks), Kept: int64(run.Counts.Kept),
		Failed: int64(run.Counts.Failed), SkippedOffline: int64(run.Counts.SkippedOffline),
		EstBytes: run.EstBytes, ID: run.ID,
	})
	if err != nil {
		return fmt.Errorf("finish the janitor run: %w", err)
	}
	if n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// GetRun implements RunStore.
func (s *SQLStore) GetRun(ctx context.Context, envID, runID string) (Run, []RunItem, error) {
	row, err := s.St.RQ.GetJanitorRun(ctx, sqlc.GetJanitorRunParams{EnvID: envID, ID: runID})
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, nil, ErrRunNotFound
	}
	if err != nil {
		return Run{}, nil, fmt.Errorf("read the janitor run: %w", err)
	}
	rows, err := s.St.RQ.ListJanitorRunItems(ctx, runID)
	if err != nil {
		return Run{}, nil, fmt.Errorf("read the janitor run's items: %w", err)
	}
	withEvidence, err := s.St.RQ.ListJanitorEvidenceSeqs(ctx, runID)
	if err != nil {
		return Run{}, nil, fmt.Errorf("read the janitor run's evidence: %w", err)
	}
	has := make(map[int64]bool, len(withEvidence))
	for _, seq := range withEvidence {
		has[seq] = true
	}
	run := runOf(row)
	items := make([]RunItem, 0, len(rows))
	for _, r := range rows {
		var d reasonDetail
		if err := json.Unmarshal([]byte(r.ReasonDetail), &d); err != nil {
			return Run{}, nil, fmt.Errorf("decode the reason of a janitor run item: %w", err)
		}
		items = append(items, RunItem{
			Seq: int(r.Seq), Type: Kind(r.Type), NodeID: r.NodeID, Node: r.NodeName, Ref: r.Ref, Name: r.Name,
			Reason: Reason{
				Code: ReasonCode(r.Reason), Service: d.Service, Slot: d.Slot, Project: d.Project, Stack: d.Stack,
				Node: d.Node, Rank: d.Rank,
				Age:       time.Duration(d.AgeSeconds) * time.Second,
				Remaining: time.Duration(d.RemainingSeconds) * time.Second,
			},
			Outcome: Outcome(r.Outcome), Code: r.Code, Size: r.EstBytes, HasEvidence: has[r.Seq],
		})
	}
	return run, items, nil
}

// runOf is a run as the record has it.
func runOf(row sqlc.JanitorRun) Run {
	run := Run{
		ID: row.ID, EnvID: row.EnvID, Trigger: row.Trigger, Actor: row.Actor, State: row.State, Reason: row.Reason,
		JobID: row.JobID, StartedAt: time.Unix(0, row.StartedAt).UTC(), EstBytes: row.EstBytes,
		Counts: Counts{
			RemovedContainers: int(row.RemovedContainers), RemovedImages: int(row.RemovedImages),
			RemovedNetworks: int(row.RemovedNetworks), Kept: int(row.Kept), Failed: int(row.Failed),
			SkippedOffline: int(row.SkippedOffline),
		},
	}
	if row.FinishedAt != 0 {
		run.FinishedAt = time.Unix(0, row.FinishedAt).UTC()
	}
	return run
}

// ListRuns implements RunStore.
func (s *SQLStore) ListRuns(ctx context.Context, envID string, limit int) ([]Run, error) {
	rows, err := s.St.RQ.ListJanitorRuns(ctx, sqlc.ListJanitorRunsParams{EnvID: envID, Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list the janitor runs: %w", err)
	}
	runs := make([]Run, 0, len(rows))
	for _, r := range rows {
		runs = append(runs, runOf(r))
	}
	return runs, nil
}

// CountRuns implements RunStore.
func (s *SQLStore) CountRuns(ctx context.Context, envID string) (int, error) {
	n, err := s.St.RQ.CountJanitorRuns(ctx, envID)
	if err != nil {
		return 0, fmt.Errorf("count the janitor runs: %w", err)
	}
	return int(n), nil
}

// PruneRuns implements RunStore.
func (s *SQLStore) PruneRuns(ctx context.Context, envID string, keep Retention, now time.Time) (int, error) {
	if err := keep.valid(); err != nil {
		return 0, err
	}
	n, err := s.St.Q.PruneJanitorRuns(ctx, sqlc.PruneJanitorRunsParams{EnvID: envID, Cutoff: keep.cutoff(now), Keep: int64(keep.Runs)})
	if err != nil {
		return 0, fmt.Errorf("prune the janitor runs: %w", err)
	}
	return int(n), nil
}

// PruneAll implements RunStore.
func (s *SQLStore) PruneAll(ctx context.Context, keep Retention, now time.Time) (int, error) {
	if err := keep.valid(); err != nil {
		return 0, err
	}
	n, err := s.St.Q.PruneAllJanitorRuns(ctx, sqlc.PruneAllJanitorRunsParams{Cutoff: keep.cutoff(now), Keep: int64(keep.Runs)})
	if err != nil {
		return 0, fmt.Errorf("prune the janitor runs: %w", err)
	}
	return int(n), nil
}

// MarkInterruptedRuns implements RunStore.
func (s *SQLStore) MarkInterruptedRuns(ctx context.Context, now time.Time) ([]Run, error) {
	rows, err := s.St.Q.MarkJanitorRunsInterrupted(ctx, now.UTC().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("end the janitor runs that were working when the server stopped: %w", err)
	}
	runs := make([]Run, 0, len(rows))
	for _, r := range rows {
		runs = append(runs, runOf(r))
	}
	return runs, nil
}

// SaveEvidence implements RunStore.
func (s *SQLStore) SaveEvidence(ctx context.Context, runID string, seq int, ev Evidence) error {
	err := s.St.Q.InsertJanitorEvidence(ctx, sqlc.InsertJanitorEvidenceParams{
		RunID: runID, Seq: int64(seq), ExitCode: int64(ev.ExitCode), Error: ev.Error, OomKilled: boolInt(ev.OOMKilled),
		StartedAt: nanos(ev.StartedAt), FinishedAt: nanos(ev.FinishedAt), Image: ev.Image, ImageID: ev.ImageID,
		ImageDigest: ev.ImageDigest, Node: ev.Node, LogText: strings.Join(ev.Lines, "\n"), LogLines: int64(len(ev.Lines)),
		LogState: ev.State,
	})
	if err != nil {
		return fmt.Errorf("save the crash evidence of a janitor run item: %w", err)
	}
	return nil
}

// GetEvidence implements RunStore.
func (s *SQLStore) GetEvidence(ctx context.Context, envID, runID string, seq int) (Evidence, error) {
	row, err := s.St.RQ.GetJanitorEvidence(ctx, sqlc.GetJanitorEvidenceParams{EnvID: envID, RunID: runID, Seq: int64(seq)})
	if errors.Is(err, sql.ErrNoRows) {
		return Evidence{}, ErrEvidenceNotFound
	}
	if err != nil {
		return Evidence{}, fmt.Errorf("read the crash evidence of a janitor run item: %w", err)
	}
	ev := Evidence{
		ExitCode: int(row.ExitCode), Error: row.Error, OOMKilled: row.OomKilled != 0,
		StartedAt: timeOf(row.StartedAt), FinishedAt: timeOf(row.FinishedAt),
		Image: row.Image, ImageID: row.ImageID, ImageDigest: row.ImageDigest, Node: row.Node, State: row.LogState,
	}
	if row.LogText != "" {
		ev.Lines = strings.Split(row.LogText, "\n")
	}
	return ev, nil
}

// boolInt is the 0 or 1 SQLite keeps a bool as.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// nanos is a time as UTC unix nanoseconds, 0 for the zero time.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

// timeOf is the time of UTC unix nanoseconds, the zero time for 0.
func timeOf(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
