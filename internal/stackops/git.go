// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// What started the deploy of a revision that no person started.
const (
	SyncPoll    = "poll"
	SyncWebhook = "webhook"
)

// ErrNoComposeFiles means a git stack was given no compose file.
var ErrNoComposeFiles = errors.New("stackops: a git stack needs at least one compose file")

// GitSource is where the content of a git stack came from: the repository (by ID, with the name it had), the ref
// and its kind, the folder and the ordered compose files. A snapshot stores it with the commit so that a revision
// restores without the mirror, whatever became of the repository or its branch (D-08).
type GitSource struct {
	RepoID       string   `json:"repoId"`
	RepoName     string   `json:"repoName"`
	RefKind      string   `json:"refKind"`
	Ref          string   `json:"ref"`
	Folder       string   `json:"folder"`
	ComposeFiles []string `json:"composeFiles"`
}

// GitRevision is what a snapshot of a git stack was made from: the commit, the tree hash of the stack's folder at
// that commit, and the source. The zero value is a snapshot of a stack that is not from git.
type GitRevision struct {
	Commit string
	Tree   string
	Source GitSource
}

func (g GitRevision) isZero() bool {
	return g.Commit == "" && g.Tree == "" && g.Source.RepoID == ""
}

// gitRevisionOf reads the git columns of a snapshot row. A source that is not valid JSON reads as none and is
// logged: a revision must stay readable, and it then restores from its files.
func gitRevisionOf(row sqlc.StackSnapshot) GitRevision {
	return parseGitRevision(row.GitCommit, row.GitTree, row.GitSource, row.ID)
}

// parseGitRevision is gitRevisionOf for the three columns alone; label names the snapshot in the log.
func parseGitRevision(commit, tree, source, label string) GitRevision {
	out := GitRevision{Commit: commit, Tree: tree}
	if source == "" || source == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(source), &out.Source); err != nil {
		slog.Default().Warn("could not read the git source of a stack snapshot", "snapshot", label, "error", err)
		out.Source = GitSource{}
	}
	return out
}

// StackGit is the git settings of a stack (the stack_git row). The sealed webhook secret is not here.
type StackGit struct {
	StackID      string
	RepoID       string
	RefKind      string
	Ref          string
	Folder       string
	ComposeFiles []string
	PollEnabled  bool
	// PollIntervalSeconds is how often the repository is checked, 60 to 86400.
	PollIntervalSeconds    int
	AutoDeploy             bool
	AutoDeployPausedReason string
	AutoDeployPausedSeq    int64
	WebhookFlavour         string
	WebhookPlacement       string
	WebhookTokenHash       string
	WebhookSecretLast4     string
	WebhookCreatedBy       string
	WebhookLastAt          *time.Time
	WebhookLastResult      string
	PendingConvert         bool
	CreatedAt, UpdatedAt   time.Time
}

// Status of a git stack's check.
const (
	GitStatusUnknown = "unknown"
	GitStatusInSync  = "in_sync"
	GitStatusBehind  = "behind"
	GitStatusError   = "error"
)

// GitState is the answer of the last check of a git stack against its repository. It is derived: a stack that was
// never checked, or restored from a backup, is GitStatusUnknown.
type GitState struct {
	StackID             string
	DeployedCommit      string
	DeployedTree        string
	HeadCommit          string
	HeadTree            string
	Behind              int
	Status              string
	ReasonCode          string
	ReasonDetail        string
	CheckedAt           *time.Time
	ErrorSince          *time.Time
	ConsecutiveFailures int
	PendingCommit       string
	NextCheckAt         *time.Time
}

// CreateGitStackInput is a stack to be made from a repository folder. Copy holds the files the import read, with
// ComposeFiles set; Rev says which commit and tree they are.
type CreateGitStackInput struct {
	EnvID, Name string
	Target      stacks.Target
	Actor       string
	Copy        stacks.WorkingCopy
	Rev         GitRevision
}

// CreateGitStack stores a stack of source kind git (its source ref is the repository's ID), its first draft with the
// repository's files, the git revision fields on that draft, a stack_git row (polling every 5 minutes, auto-deploy
// off, no webhook) and a stack_git_state row with status unknown, in one transaction. Nothing is deployed. A name the
// environment already has is ErrNameTaken and nothing is stored.
func (r Repo) CreateGitStack(ctx context.Context, in CreateGitStackInput) (Stack, Snapshot, error) {
	copy := in.Copy
	if len(copy.ComposeFiles) == 0 {
		copy.ComposeFiles = in.Rev.Source.ComposeFiles
	}
	if len(copy.ComposeFiles) == 0 {
		return Stack{}, Snapshot{}, ErrNoComposeFiles
	}
	rev := in.Rev
	rev.Source.ComposeFiles = slices.Clone(copy.ComposeFiles)
	rawSource, err := json.Marshal(rev.Source)
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("encode the git source: %w", err)
	}
	rawCompose, err := json.Marshal(copy.ComposeFiles)
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("encode the compose files: %w", err)
	}
	stackID, err := uuid.NewV7()
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("new stack id: %w", err)
	}
	snapID, err := uuid.NewV7()
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("new snapshot id: %w", err)
	}
	now := r.now()
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	err = q.InsertStack(ctx, sqlc.InsertStackParams{
		ID: stackID.String(), EnvID: in.EnvID, Name: in.Name, Target: string(in.Target),
		SourceKind: string(stacks.SourceGit), SourceRef: rev.Source.RepoID, CreatedBy: in.Actor, CreatedAt: now.UnixNano(),
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Stack{}, Snapshot{}, ErrNameTaken
		}
		return Stack{}, Snapshot{}, fmt.Errorf("insert stack: %w", err)
	}
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: snapID.String(), StackID: stackID.String(), Kind: KindDraft, Version: 1, Outcome: OutcomePending,
		Pins: "{}", CreatedBy: in.Actor, CreatedAt: now.UnixNano(), Origin: OriginDeploy,
	}); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("insert draft: %w", err)
	}
	if err := r.writeContent(ctx, q, snapID.String(), copy); err != nil {
		return Stack{}, Snapshot{}, err
	}
	if _, err := q.SetSnapshotGit(ctx, sqlc.SetSnapshotGitParams{
		GitCommit: rev.Commit, GitTree: rev.Tree, GitSource: string(rawSource), ID: snapID.String(),
	}); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("store the git revision: %w", err)
	}
	if err := q.InsertStackGit(ctx, sqlc.InsertStackGitParams{
		StackID: stackID.String(), RepoID: rev.Source.RepoID, RefKind: rev.Source.RefKind, Ref: rev.Source.Ref,
		Folder: rev.Source.Folder, ComposeFiles: string(rawCompose), CreatedAt: now.UnixNano(), UpdatedAt: now.UnixNano(),
	}); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("insert the git settings: %w", err)
	}
	if err := q.InsertStackGitState(ctx, stackID.String()); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("insert the git state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("commit: %w", err)
	}
	st := Stack{
		ID: stackID.String(), EnvID: in.EnvID, Name: in.Name, Target: in.Target,
		Source: stacks.Source{Kind: stacks.SourceGit, Ref: rev.Source.RepoID}, CreatedBy: in.Actor, CreatedAt: now,
	}
	snap := Snapshot{
		ID: snapID.String(), StackID: st.ID, Kind: KindDraft, Version: 1, Outcome: OutcomePending,
		CreatedBy: in.Actor, CreatedAt: now, Git: rev, Copy: normalise(copy), IsDraft: true,
	}
	return st, snap, nil
}

// StackGit reads the git settings of a stack, or ErrNoGit when it has none (a stack that is not from git).
func (r Repo) StackGit(ctx context.Context, stackID string) (StackGit, error) {
	row, err := r.Store.RQ.GetStackGit(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return StackGit{}, ErrNoGit
	}
	if err != nil {
		return StackGit{}, fmt.Errorf("read the git settings: %w", err)
	}
	return stackGitOf(row), nil
}

// StackGitsOfEnv reads the git settings of every git stack of an environment, by stack name.
func (r Repo) StackGitsOfEnv(ctx context.Context, envID string) ([]StackGit, error) {
	rows, err := r.Store.RQ.ListStackGitsByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the git settings: %w", err)
	}
	out := make([]StackGit, len(rows))
	for i, row := range rows {
		out[i] = stackGitOf(row)
	}
	return out, nil
}

// GitState reads the last check of a git stack. A stack with no state row (restored from a backup, which does not
// carry them) reads as GitStatusUnknown; a stack that is not from git is ErrNoGit.
func (r Repo) GitState(ctx context.Context, stackID string) (GitState, error) {
	row, err := r.Store.RQ.GetStackGitState(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, gerr := r.StackGit(ctx, stackID); gerr != nil {
			return GitState{}, gerr
		}
		return GitState{StackID: stackID, Status: GitStatusUnknown}, nil
	}
	if err != nil {
		return GitState{}, fmt.Errorf("read the git state: %w", err)
	}
	return gitStateOf(row), nil
}

// GitStates reads the last check of every git stack of an environment, by stack ID. A git stack that was never
// checked, or whose state a restore did not carry, has no entry: it reads as GitStatusUnknown. It is one query.
func (r Repo) GitStates(ctx context.Context, envID string) (map[string]GitState, error) {
	rows, err := r.Store.RQ.ListStackGitStatesByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the git states: %w", err)
	}
	out := make(map[string]GitState, len(rows))
	for _, row := range rows {
		out[row.StackID] = gitStateOf(sqlc.StackGitState(row))
	}
	return out, nil
}

// WriteGitState stores the answer of a check of a git stack: all of its columns, and the row itself if a restore left
// none. A write that only moves the time of the check (the same answer as the one stored, looked at again) changes
// the one column. Only gitsync's state writer calls it, so that what is new in an answer is judged in one place.
func (r Repo) WriteGitState(ctx context.Context, s GitState) error {
	if err := r.Store.Q.UpsertStackGitState(ctx, sqlc.UpsertStackGitStateParams{
		StackID: s.StackID, DeployedCommit: s.DeployedCommit, DeployedTree: s.DeployedTree, HeadCommit: s.HeadCommit,
		HeadTree: s.HeadTree, Behind: int64(s.Behind), Status: s.Status, ReasonCode: s.ReasonCode, ReasonDetail: s.ReasonDetail,
		CheckedAt: nullUnixNano(s.CheckedAt), ErrorSince: nullUnixNano(s.ErrorSince), ConsecutiveFailures: int64(s.ConsecutiveFailures),
		PendingCommit: s.PendingCommit, NextCheckAt: nullUnixNano(s.NextCheckAt),
	}); err != nil {
		return fmt.Errorf("write the git state: %w", err)
	}
	return nil
}

// TouchGitChecked records that a check looked at the stack again and found what it already held.
func (r Repo) TouchGitChecked(ctx context.Context, stackID string, at time.Time) error {
	n, err := r.Store.Q.SetStackGitChecked(ctx, sqlc.SetStackGitCheckedParams{CheckedAt: nullUnixNano(&at), StackID: stackID})
	if err != nil {
		return fmt.Errorf("touch the git state: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeployedGit is where the stack's newest good revision came from: its commit, the tree hash of its folder and its
// source (the repository, ref and folder it was made with). The deployed commit and tree of a check come from here and
// never from the stack's current settings, which a review may have changed since (RESEARCH Pitfall 8). It reports
// false when the stack has no good revision, or the newest one was not made from a repository.
func (r Repo) DeployedGit(ctx context.Context, stackID string) (GitRevision, bool, error) {
	row, err := r.Store.RQ.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return GitRevision{}, false, nil
	}
	if err != nil {
		return GitRevision{}, false, fmt.Errorf("read the deployed revision: %w", err)
	}
	rev := gitRevisionOf(row)
	if rev.isZero() {
		return GitRevision{}, false, nil
	}
	return rev, true, nil
}

// PolledStack is a git stack the background poller looks at: polling is on and it follows a branch or a tag.
type PolledStack struct {
	StackID, EnvID, RepoID string
	// RefKind and Ref are what the stack follows; the poller compares the repository's advertised head with the last one seen.
	RefKind, Ref string
	// Interval is how often the stack is checked.
	Interval time.Duration
	// NextCheckAt is when the poller looks next; nil for a stack the poller has not scheduled yet (new, or restored
	// from a backup).
	NextCheckAt *time.Time
	// CheckedAt is when a check last looked at the stack; nil if it never did.
	CheckedAt *time.Time
	// ConsecutiveFailures is the run of failed checks, 0 when the last one was good.
	ConsecutiveFailures int
}

// PolledStacks lists the git stacks to poll, by stack ID: polling on, and not a fixed commit (which cannot move, D-12).
func (r Repo) PolledStacks(ctx context.Context) ([]PolledStack, error) {
	rows, err := r.Store.RQ.ListPolledStackGits(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the polled git stacks: %w", err)
	}
	out := make([]PolledStack, len(rows))
	for i, row := range rows {
		out[i] = PolledStack{
			StackID: row.StackID, EnvID: row.EnvID, RepoID: row.RepoID, RefKind: row.RefKind, Ref: row.Ref,
			Interval: time.Duration(row.PollIntervalS) * time.Second, NextCheckAt: unixNanoPtr(row.NextCheckAt),
			CheckedAt: unixNanoPtr(row.CheckedAt), ConsecutiveFailures: int(row.ConsecutiveFailures.Int64),
		}
	}
	return out, nil
}

// SetNextCheck records when the poller looks at the stack next. It touches no other column of the stack's state.
func (r Repo) SetNextCheck(ctx context.Context, stackID string, at time.Time) error {
	if err := r.Store.Q.SetStackGitNextCheck(ctx, sqlc.SetStackGitNextCheckParams{StackID: stackID, NextCheckAt: nullUnixNano(&at)}); err != nil {
		return fmt.Errorf("schedule the next git check: %w", err)
	}
	return nil
}

// Bounds of the polling interval a person may set (D-14): a minute to a day.
const (
	MinPollInterval = time.Minute
	MaxPollInterval = 24 * time.Hour
)

// SetPolling turns a git stack's polling on or off and sets how often it is checked, and has the poller schedule the
// stack afresh. The interval is kept whole seconds and must be within MinPollInterval and MaxPollInterval. A stack that
// is not from git is ErrNoGit.
func (r Repo) SetPolling(ctx context.Context, stackID string, enabled bool, interval time.Duration) error {
	if interval < MinPollInterval || interval > MaxPollInterval {
		return fmt.Errorf("stackops: the polling interval %v is outside %v to %v", interval, MinPollInterval, MaxPollInterval)
	}
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)
	on := int64(0)
	if enabled {
		on = 1
	}
	n, err := q.SetStackGitPolling(ctx, sqlc.SetStackGitPollingParams{
		PollEnabled: on, PollIntervalS: int64(interval / time.Second), UpdatedAt: r.now().UnixNano(), StackID: stackID,
	})
	if err != nil {
		return fmt.Errorf("set the polling: %w", err)
	}
	if n == 0 {
		return ErrNoGit
	}
	if err := q.ClearStackGitNextCheck(ctx, stackID); err != nil {
		return fmt.Errorf("reschedule the polling: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// RefreshDraftFromGit replaces the files of a git stack's draft that came from the repository with those of a newer
// commit, when the draft is still at version: the draft's repository files (origin repo) are dropped and repoFiles take
// their place, a file of Bayplan's own (overlay, or one a person added) and the variables are kept, the compose files
// are the source's, and the commit, tree and source the draft records are rev. A path both a kept file and a
// repository file hold goes to the repository file, as an import does (D-09). The version of the draft goes up by one.
// A stack with no draft (it was discarded) starts one from its last good revision, and only a caller at version 0 may,
// as for a save. A draft that has moved is a *DraftConflictError. Nothing changes when the draft already holds exactly
// this: it is returned as it is.
func (r Repo) RefreshDraftFromGit(ctx context.Context, stackID string, version int64, actor string, repoFiles []stacks.File, rev GitRevision) (Snapshot, error) {
	if len(rev.Source.ComposeFiles) == 0 {
		return Snapshot{}, ErrNoComposeFiles
	}
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	draft, err := r.readDraft(ctx, q, stackID)
	hasDraft := err == nil
	switch {
	case errors.Is(err, ErrNoDraft):
		if version != 0 {
			return Snapshot{}, &DraftConflictError{}
		}
		if draft, err = r.fallback(ctx, q, stackID); err != nil {
			return Snapshot{}, err
		}
	case err != nil:
		return Snapshot{}, err
	case draft.Version != version:
		return Snapshot{}, &DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
	}

	next := refreshedCopy(draft.Copy, repoFiles, rev.Source.ComposeFiles)
	next.Files = stacks.MarkSecretFilesFor(next.Files, draft.Copy.Files, next.ComposeFiles)
	rev.Source.ComposeFiles = nonNil(slices.Clone(rev.Source.ComposeFiles))
	if hasDraft && sameContent(draft.Copy, next) && sameGitRevision(draft.Git, rev) {
		return draft, nil
	}
	now := r.now()
	out := Snapshot{
		StackID: stackID, Kind: KindDraft, BaseSeq: draft.BaseSeq, Outcome: OutcomePending, CreatedBy: actor, CreatedAt: now,
		Git: rev, Copy: normalise(next), IsDraft: true,
	}
	if hasDraft {
		n, err := q.UpdateDraftVersion(ctx, sqlc.UpdateDraftVersionParams{
			CreatedBy: actor, CreatedAt: now.UnixNano(), ID: draft.ID, Version: version,
		})
		if err != nil {
			return Snapshot{}, fmt.Errorf("update draft: %w", err)
		}
		if n != 1 {
			return Snapshot{}, &DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
		}
		if err := q.DeleteSnapshotFiles(ctx, draft.ID); err != nil {
			return Snapshot{}, fmt.Errorf("clear files: %w", err)
		}
		if err := q.DeleteSnapshotVars(ctx, draft.ID); err != nil {
			return Snapshot{}, fmt.Errorf("clear variables: %w", err)
		}
		out.ID, out.Version = draft.ID, version+1
	} else {
		id, err := uuid.NewV7()
		if err != nil {
			return Snapshot{}, fmt.Errorf("new snapshot id: %w", err)
		}
		if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
			ID: id.String(), StackID: stackID, Kind: KindDraft, BaseSeq: draft.BaseSeq, Version: firstSavedVersion,
			Outcome: OutcomePending, Pins: "{}", CreatedBy: actor, CreatedAt: now.UnixNano(), Origin: OriginDeploy,
		}); err != nil {
			return Snapshot{}, fmt.Errorf("insert draft: %w", err)
		}
		out.ID, out.Version = id.String(), firstSavedVersion
	}
	if err := r.writeContent(ctx, q, out.ID, next); err != nil {
		return Snapshot{}, err
	}
	if err := r.copyGit(ctx, q, out.ID, rev, ""); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// refreshedCopy is the working copy of a draft with its repository files replaced by repoFiles and the compose files
// set. A file of the draft that is not from the repository stays unless a repository file holds its path.
func refreshedCopy(current stacks.WorkingCopy, repoFiles []stacks.File, composeFiles []string) stacks.WorkingCopy {
	out := stacks.WorkingCopy{Vars: slices.Clone(current.Vars), ComposeFiles: slices.Clone(composeFiles)}
	taken := make(map[string]bool, len(repoFiles))
	for _, f := range repoFiles {
		f.Origin = stacks.OriginRepo
		taken[f.Path] = true
		out.Files = append(out.Files, f)
	}
	for _, f := range current.Files {
		if f.Origin != stacks.OriginRepo && !taken[f.Path] {
			out.Files = append(out.Files, f)
		}
	}
	return out
}

func sameGitRevision(a, b GitRevision) bool {
	return a.Commit == b.Commit && a.Tree == b.Tree && a.Source.RepoID == b.Source.RepoID && a.Source.RepoName == b.Source.RepoName &&
		a.Source.RefKind == b.Source.RefKind && a.Source.Ref == b.Source.Ref && a.Source.Folder == b.Source.Folder &&
		slices.Equal(a.Source.ComposeFiles, b.Source.ComposeFiles)
}

// SetAppliedSource makes the repository, ref, folder and compose files a git stack follows those of src: the settings
// follow what was deployed (D-08). The sync state is not touched. A stack that is not from git is ErrNoGit.
func (r Repo) SetAppliedSource(ctx context.Context, stackID string, src GitSource) error {
	raw, err := json.Marshal(nonNil(src.ComposeFiles))
	if err != nil {
		return fmt.Errorf("encode the compose files: %w", err)
	}
	n, err := r.Store.Q.SetStackGitSource(ctx, sqlc.SetStackGitSourceParams{
		RepoID: src.RepoID, RefKind: src.RefKind, Ref: src.Ref, Folder: src.Folder, ComposeFiles: string(raw),
		UpdatedAt: r.now().UnixNano(), StackID: stackID,
	})
	if err != nil {
		return fmt.Errorf("set the git source: %w", err)
	}
	if n == 0 {
		return ErrNoGit
	}
	return nil
}

// ErrNoGit means the stack has no git settings: it is not a stack from a repository.
var ErrNoGit = errors.New("stackops: the stack is not from a git repository")

func stackGitOf(row sqlc.StackGit) StackGit {
	return StackGit{
		StackID: row.StackID, RepoID: row.RepoID, RefKind: row.RefKind, Ref: row.Ref, Folder: row.Folder,
		ComposeFiles: composeFilesOf(row.StackID, row.ComposeFiles), PollEnabled: row.PollEnabled != 0,
		PollIntervalSeconds: int(row.PollIntervalS), AutoDeploy: row.AutoDeploy != 0,
		AutoDeployPausedReason: row.AutoDeployPausedReason, AutoDeployPausedSeq: row.AutoDeployPausedSeq,
		WebhookFlavour: row.WebhookFlavour, WebhookPlacement: row.WebhookPlacement, WebhookTokenHash: row.WebhookTokenHash,
		WebhookSecretLast4: row.WebhookSecretLast4, WebhookCreatedBy: row.WebhookCreatedBy,
		WebhookLastAt: unixNanoPtr(row.WebhookLastAt), WebhookLastResult: row.WebhookLastResult,
		PendingConvert: row.PendingConvert != 0, CreatedAt: time.Unix(0, row.CreatedAt).UTC(),
		UpdatedAt: time.Unix(0, row.UpdatedAt).UTC(),
	}
}

func gitStateOf(row sqlc.StackGitState) GitState {
	return GitState{
		StackID: row.StackID, DeployedCommit: row.DeployedCommit, DeployedTree: row.DeployedTree,
		HeadCommit: row.HeadCommit, HeadTree: row.HeadTree, Behind: int(row.Behind), Status: row.Status,
		ReasonCode: row.ReasonCode, ReasonDetail: row.ReasonDetail, CheckedAt: unixNanoPtr(row.CheckedAt),
		ErrorSince: unixNanoPtr(row.ErrorSince), ConsecutiveFailures: int(row.ConsecutiveFailures),
		PendingCommit: row.PendingCommit, NextCheckAt: unixNanoPtr(row.NextCheckAt),
	}
}

func nullUnixNano(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

func unixNanoPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}

// keepGitShape returns next with what a person's save cannot see kept from current: the ordered compose files, when
// next names none, and the origin of each file that next does not say (the editor sends none), by path. A save of a
// stack that is not from git has neither, so it is unchanged.
func keepGitShape(current, next stacks.WorkingCopy) stacks.WorkingCopy {
	if len(next.ComposeFiles) == 0 {
		next.ComposeFiles = current.ComposeFiles
	}
	origin := map[string]stacks.FileOrigin{}
	for _, f := range current.Files {
		if f.Origin != stacks.OriginInline {
			origin[f.Path] = f.Origin
		}
	}
	if len(origin) == 0 {
		return next
	}
	files := slices.Clone(next.Files)
	for i := range files {
		if files[i].Origin == stacks.OriginInline {
			files[i].Origin = origin[files[i].Path]
		}
	}
	next.Files = files
	return next
}

// copyGit stores on the snapshot id what another snapshot was made from. The zero value with no trigger stores
// nothing: the columns already hold their defaults.
func (r Repo) copyGit(ctx context.Context, q *sqlc.Queries, id string, rev GitRevision, trigger string) error {
	if rev.isZero() && trigger == "" {
		return nil
	}
	rev.Source.ComposeFiles = nonNil(rev.Source.ComposeFiles)
	raw, err := json.Marshal(rev.Source)
	if err != nil {
		return fmt.Errorf("encode the git source: %w", err)
	}
	if _, err := q.SetSnapshotGit(ctx, sqlc.SetSnapshotGitParams{
		GitCommit: rev.Commit, GitTree: rev.Tree, GitSource: string(raw), SyncTrigger: trigger, ID: id,
	}); err != nil {
		return fmt.Errorf("store the git revision: %w", err)
	}
	return nil
}
