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
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Snapshot kinds and revision outcomes, as the stack_snapshots table spells them.
const (
	KindDraft    = "draft"
	KindRevision = "revision"

	OutcomePending = "pending"
	OutcomeGood    = "good"
	OutcomeFailed  = "failed"
)

// How a revision came to be (stack_snapshots.origin): a deploy, an Update images deploy, a roll back that
// copied an earlier revision, or the first revision of an adopted stack. A draft reads deploy.
const (
	OriginDeploy   = "deploy"
	OriginUpdate   = "update"
	OriginRollback = "rollback"
	OriginAdopted  = "adopted"
)

// The kinds of a stack operation and the states of its row in stack_deploys.
const (
	DeployKindDeploy   = "deploy"
	DeployKindRollback = "rollback"
	DeployKindStop     = "stop"
	DeployKindStart    = "start"
	DeployKindRemove   = "remove"

	DeployRunning     = "running"
	DeployDone        = "done"
	DeployFailed      = "failed"
	DeployCancelled   = "cancelled"
	DeployInterrupted = "interrupted"
)

// Repo reads and writes the stored definition of stacks. A secret variable and a secret file are
// sealed with the vault under a context bound to the snapshot and the name or path, so a sealed
// value copied to another row does not open.
type Repo struct {
	Store *store.Store
	Vault *vault.Vault
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (r Repo) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Stack is one stack Bayplan manages.
type Stack struct {
	ID, EnvID, Name string
	Target          stacks.Target
	Source          stacks.Source
	CreatedBy       string
	CreatedAt       time.Time
	Stopped         bool
	// Discovered is true for a stack found on the engine by its labels that Bayplan does not hold (D-07). It
	// is never stored: DiscoveredStack makes one, and Ownership reads it.
	Discovered bool
	// Adoption is what was recorded when Bayplan took the stack over, the zero value for a stack that was not
	// adopted (Source.Kind is SourceAdopted exactly for the others).
	Adoption stacks.Adoption
}

// DiscoveredStack is the stack of a name found on an environment's engine by its labels. It has no ID and
// is never stored; it exists so the overview, the resources and the logs read it like any stack.
func DiscoveredStack(envID, name string, target stacks.Target) Stack {
	return Stack{EnvID: envID, Name: name, Target: target, Discovered: true}
}

// Snapshot is a stack's definition at a moment: the draft, or a revision a deploy took. CreatedBy
// and CreatedAt say who last saved a draft and when, or who started the deploy a revision was
// taken for.
type Snapshot struct {
	ID, StackID, Kind     string
	Seq, BaseSeq, Version int64
	Outcome, CreatedBy    string
	CreatedAt             time.Time
	// Origin is how a revision came to be (OriginDeploy and the others); RestoredFrom is the number of the
	// revision a roll back copied, and 0 for any other.
	Origin       string
	RestoredFrom int64
	Copy         stacks.WorkingCopy
	// IsDraft is true for a draft that is stored. It is false for what Draft answers when the stack
	// has none: the last good revision, or the starter, so the editor always has content.
	IsDraft bool
}

// DeployRow is one operation on a stack.
type DeployRow struct {
	ID, StackID, EnvID, Kind, State string
	RevisionSeq                     int64
	JobID, StartedBy                string
	StartedAt, EndedAt              time.Time
	FailedService, Code             string
}

func stackOf(row sqlc.Stack) Stack {
	return Stack{
		ID: row.ID, EnvID: row.EnvID, Name: row.Name, Target: stacks.Target(row.Target),
		Source:    stacks.Source{Kind: stacks.SourceKind(row.SourceKind), Ref: row.SourceRef},
		CreatedBy: row.CreatedBy, CreatedAt: time.Unix(0, row.CreatedAt).UTC(), Stopped: row.Stopped != 0,
		Adoption: adoptionOf(row),
	}
}

// adoptionOf reads the adoption record of a stack row. A record that is not valid JSON reads as the zero value
// and is logged: a stack must stay listable, and ownership then falls back to the exact bayplan.stack.id rule,
// which is the safe side.
func adoptionOf(row sqlc.Stack) stacks.Adoption {
	var a stacks.Adoption
	if row.Adoption == "" || row.Adoption == "{}" {
		return a
	}
	if err := json.Unmarshal([]byte(row.Adoption), &a); err != nil {
		slog.Default().Warn("could not read the adoption record of a stack", "stack", row.ID, "error", err)
		return stacks.Adoption{}
	}
	return a
}

func snapshotOf(row sqlc.StackSnapshot) Snapshot {
	return Snapshot{
		ID: row.ID, StackID: row.StackID, Kind: row.Kind, Seq: row.Seq, BaseSeq: row.BaseSeq, Version: row.Version,
		Outcome: row.Outcome, CreatedBy: row.CreatedBy, CreatedAt: time.Unix(0, row.CreatedAt).UTC(),
		Origin: row.Origin, RestoredFrom: row.RestoredFrom, IsDraft: row.Kind == KindDraft,
	}
}

func deployOf(row sqlc.StackDeploy) DeployRow {
	d := DeployRow{
		ID: row.ID, StackID: row.StackID, EnvID: row.EnvID, Kind: row.Kind, State: row.State,
		RevisionSeq: row.RevisionSeq, JobID: row.JobID, StartedBy: row.StartedBy,
		StartedAt: time.Unix(0, row.StartedAt).UTC(), FailedService: row.FailedService, Code: row.Code,
	}
	if row.EndedAt != 0 {
		d.EndedAt = time.Unix(0, row.EndedAt).UTC()
	}
	return d
}

func varContext(snapshotID, name string) string {
	return vault.ContextStackVar + snapshotID + "/" + name
}

func fileContext(snapshotID, path string) string {
	return vault.ContextStackFile + snapshotID + "/" + path
}

// CreateStack stores a new stack and its first draft in one transaction. A name the environment
// already has is ErrNameTaken and nothing is stored.
func (r Repo) CreateStack(ctx context.Context, envID, name string, target stacks.Target, actor string, copy stacks.WorkingCopy) (Stack, Snapshot, error) {
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
		ID: stackID.String(), EnvID: envID, Name: name, Target: string(target),
		SourceKind: string(stacks.SourceInline), CreatedBy: actor, CreatedAt: now.UnixNano(),
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Stack{}, Snapshot{}, ErrNameTaken
		}
		return Stack{}, Snapshot{}, fmt.Errorf("insert stack: %w", err)
	}
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: snapID.String(), StackID: stackID.String(), Kind: KindDraft, Version: 1, Outcome: OutcomePending,
		Pins: "{}", CreatedBy: actor, CreatedAt: now.UnixNano(), Origin: OriginDeploy,
	}); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("insert draft: %w", err)
	}
	if err := r.writeContent(ctx, q, snapID.String(), copy); err != nil {
		return Stack{}, Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("commit: %w", err)
	}
	st := Stack{
		ID: stackID.String(), EnvID: envID, Name: name, Target: target,
		Source: stacks.Source{Kind: stacks.SourceInline}, CreatedBy: actor, CreatedAt: now,
	}
	snap := Snapshot{
		ID: snapID.String(), StackID: st.ID, Kind: KindDraft, Version: 1, Outcome: OutcomePending,
		CreatedBy: actor, CreatedAt: now, Copy: normalise(copy), IsDraft: true,
	}
	return st, snap, nil
}

// AdoptRecord is a discovered stack to be stored as a managed one (plan 08-13, STK-13, D-14). Copy is the
// definition rebuilt from what runs, Pins the images it runs by service and Rendered the masked rendering of the
// two, which is what the first review compares a draft with.
type AdoptRecord struct {
	EnvID, Name string
	Target      stacks.Target
	// SourceRef is the swarm namespace or the Compose project the stack was found as.
	SourceRef string
	Actor     string
	Copy      stacks.WorkingCopy
	Pins      map[string]string
	Rendered  []byte
	Adoption  stacks.Adoption
}

// Adopt stores a discovered stack in one transaction: the stack (source kind adopted, with the adoption record),
// its draft at version 1 based on revision 1, and revision 1 itself (origin adopted, outcome good, pinned to
// the images that run, with its rendering), both holding the same content. The draft and the revision are
// created at the same instant, so the draft reads as having no changes (DraftSaves). Nothing is sent to an
// engine. A name the environment already has is ErrNameTaken and nothing is stored, which is also how the
// second of two adoptions that race ends (UNIQUE env and name).
func (r Repo) Adopt(ctx context.Context, rec AdoptRecord) (Stack, Snapshot, error) {
	stackID, err := uuid.NewV7()
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("new stack id: %w", err)
	}
	draftID, err := uuid.NewV7()
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("new snapshot id: %w", err)
	}
	revID, err := uuid.NewV7()
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("new snapshot id: %w", err)
	}
	pins := rec.Pins
	if pins == nil {
		pins = map[string]string{}
	}
	rawPins, err := json.Marshal(pins)
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("encode pins: %w", err)
	}
	rawAdoption, err := json.Marshal(rec.Adoption)
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("encode the adoption record: %w", err)
	}
	now := r.now()
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	err = q.InsertAdoptedStack(ctx, sqlc.InsertAdoptedStackParams{
		ID: stackID.String(), EnvID: rec.EnvID, Name: rec.Name, Target: string(rec.Target),
		SourceKind: string(stacks.SourceAdopted), SourceRef: rec.SourceRef, CreatedBy: rec.Actor,
		CreatedAt: now.UnixNano(), Adoption: string(rawAdoption),
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Stack{}, Snapshot{}, ErrNameTaken
		}
		return Stack{}, Snapshot{}, fmt.Errorf("insert stack: %w", err)
	}
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: draftID.String(), StackID: stackID.String(), Kind: KindDraft, BaseSeq: 1, Version: 1, Outcome: OutcomePending,
		Pins: "{}", CreatedBy: rec.Actor, CreatedAt: now.UnixNano(), Origin: OriginDeploy,
	}); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("insert draft: %w", err)
	}
	if err := r.writeContent(ctx, q, draftID.String(), rec.Copy); err != nil {
		return Stack{}, Snapshot{}, err
	}
	seq, err := q.NextRevisionSeq(ctx, stackID.String())
	if err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("next revision: %w", err)
	}
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: revID.String(), StackID: stackID.String(), Kind: KindRevision, Seq: seq, Version: 1, Outcome: OutcomeGood,
		Rendered: string(rec.Rendered), Pins: string(rawPins), CreatedBy: rec.Actor, CreatedAt: now.UnixNano(), Origin: OriginAdopted,
	}); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("insert revision: %w", err)
	}
	if err := r.writeContent(ctx, q, revID.String(), rec.Copy); err != nil {
		return Stack{}, Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stack{}, Snapshot{}, fmt.Errorf("commit: %w", err)
	}
	st := Stack{
		ID: stackID.String(), EnvID: rec.EnvID, Name: rec.Name, Target: rec.Target,
		Source: stacks.Source{Kind: stacks.SourceAdopted, Ref: rec.SourceRef}, CreatedBy: rec.Actor, CreatedAt: now,
		Adoption: rec.Adoption,
	}
	rev := Snapshot{
		ID: revID.String(), StackID: st.ID, Kind: KindRevision, Seq: seq, Version: 1, Outcome: OutcomeGood,
		CreatedBy: rec.Actor, CreatedAt: now, Origin: OriginAdopted, Copy: normalise(rec.Copy),
	}
	return st, rev, nil
}

// SetAdoption replaces the adoption record of a stack: the first deploy clears Pending, and the adoption review
// records its gaps. A stack that is gone is ErrStackNotFound.
func (r Repo) SetAdoption(ctx context.Context, stackID string, a stacks.Adoption) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode the adoption record: %w", err)
	}
	n, err := r.Store.Q.SetStackAdoption(ctx, sqlc.SetStackAdoptionParams{Adoption: string(raw), ID: stackID})
	if err != nil {
		return fmt.Errorf("set the adoption record: %w", err)
	}
	if n != 1 {
		return ErrStackNotFound
	}
	return nil
}

// Stack reads one stack of an environment by ID. An ID of another environment is
// ErrStackNotFound, the same as an ID that does not exist.
func (r Repo) Stack(ctx context.Context, envID, stackID string) (Stack, error) {
	row, err := r.Store.RQ.GetStack(ctx, sqlc.GetStackParams{EnvID: envID, ID: stackID})
	if errors.Is(err, sql.ErrNoRows) {
		return Stack{}, ErrStackNotFound
	}
	if err != nil {
		return Stack{}, fmt.Errorf("read stack: %w", err)
	}
	return stackOf(row), nil
}

// Stacks lists the stacks of an environment by name.
func (r Repo) Stacks(ctx context.Context, envID string) ([]Stack, error) {
	rows, err := r.Store.RQ.ListStacksByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list stacks: %w", err)
	}
	out := make([]Stack, 0, len(rows))
	for _, row := range rows {
		out = append(out, stackOf(row))
	}
	return out, nil
}

// Draft reads a stack's draft with its files and variables opened. A stack that has none (its
// draft was discarded) answers what the editor starts from instead: the last good revision, or
// for a stack never deployed the starter compose file. That snapshot has IsDraft false and
// version 0, which is the version a save from it is made at.
func (r Repo) Draft(ctx context.Context, stackID string) (Snapshot, error) {
	snap, err := r.readDraft(ctx, r.Store.RQ, stackID)
	if errors.Is(err, ErrNoDraft) {
		return r.fallback(ctx, r.Store.RQ, stackID)
	}
	return snap, err
}

// fallback is what a stack with no draft starts from: the content of its last good revision,
// based on that revision, or the starter compose file when it was never deployed. It is not a
// draft and not stored; the first save from it makes one.
func (r Repo) fallback(ctx context.Context, q *sqlc.Queries, stackID string) (Snapshot, error) {
	row, err := q.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{
			StackID: stackID, Kind: KindDraft, Outcome: OutcomePending,
			Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(stacks.StarterCompose)}}},
		}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read revision: %w", err)
	}
	snap := snapshotOf(row)
	snap.IsDraft, snap.Version, snap.BaseSeq = false, 0, row.Seq
	if snap.Copy, err = r.readContent(ctx, q, snap.ID); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (r Repo) readDraft(ctx context.Context, q *sqlc.Queries, stackID string) (Snapshot, error) {
	row, err := q.GetDraft(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNoDraft
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read draft: %w", err)
	}
	snap := snapshotOf(row)
	if snap.Copy, err = r.readContent(ctx, q, snap.ID); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// SaveDraft replaces the draft's files and variables when the draft is still at version. A draft
// that has moved is a *DraftConflictError naming who saved it and when. Content equal to what
// is saved changes nothing: it returns the draft as it is and changed false, and writes
// nothing, so a second save of the same input does not bump the version.
func (r Repo) SaveDraft(ctx context.Context, stackID string, version int64, actor string, copy stacks.WorkingCopy) (Snapshot, bool, error) {
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	draft, err := r.readDraft(ctx, q, stackID)
	if errors.Is(err, ErrNoDraft) {
		return r.saveFirst(ctx, tx, q, stackID, version, actor, copy)
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	if draft.Version != version {
		return Snapshot{}, false, &DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
	}
	// The server decides which files are secret, not the browser: a file the compose file uses
	// under secrets:, or one that is secret already, is stored sealed (UI-SPEC Defaults 6).
	copy.Files = stacks.MarkSecretFiles(copy.Files, draft.Copy.Files)
	if sameContent(draft.Copy, copy) {
		return draft, false, nil
	}
	now := r.now()
	n, err := q.UpdateDraftVersion(ctx, sqlc.UpdateDraftVersionParams{
		CreatedBy: actor, CreatedAt: now.UnixNano(), ID: draft.ID, Version: version,
	})
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("update draft: %w", err)
	}
	if n != 1 {
		// Another save won between the read and the update; the immediate transaction makes this
		// unreachable, but the version check is the contract, so it is kept.
		return Snapshot{}, false, &DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
	}
	if err := q.DeleteSnapshotFiles(ctx, draft.ID); err != nil {
		return Snapshot{}, false, fmt.Errorf("clear files: %w", err)
	}
	if err := q.DeleteSnapshotVars(ctx, draft.ID); err != nil {
		return Snapshot{}, false, fmt.Errorf("clear variables: %w", err)
	}
	if err := r.writeContent(ctx, q, draft.ID, copy); err != nil {
		return Snapshot{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, false, fmt.Errorf("commit: %w", err)
	}
	draft.Version, draft.CreatedBy, draft.CreatedAt, draft.Copy = version+1, actor, now, normalise(copy)
	return draft, true, nil
}

// DiscardDraft deletes the stack's draft with its files and variables. It reports false when
// there was none, which is not an error: discarding twice is the same as discarding once.
func (r Repo) DiscardDraft(ctx context.Context, stackID string) (discarded bool, version int64, err error) {
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	// The row only: a discard needs neither the vault nor the content, so it works for a draft
	// whose secrets can no longer be opened.
	row, err := q.GetDraft(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("read draft: %w", err)
	}
	if err := q.DeleteSnapshotFiles(ctx, row.ID); err != nil {
		return false, 0, fmt.Errorf("clear files: %w", err)
	}
	if err := q.DeleteSnapshotVars(ctx, row.ID); err != nil {
		return false, 0, fmt.Errorf("clear variables: %w", err)
	}
	n, err := q.DeleteDraft(ctx, row.ID)
	if err != nil {
		return false, 0, fmt.Errorf("delete draft: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit: %w", err)
	}
	return n == 1, row.Version, nil
}

// firstSavedVersion is the version of a draft made by a save, not by the stack's creation: version
// 1 is the draft a stack is created with, and a list tells "a draft with changes" from "a draft
// nobody has touched" by it (DraftStates).
const firstSavedVersion = 2

// saveFirst saves into a stack that has no draft (it was discarded). Only an editor that read the
// fallback, at version 0, may: an editor holding a version of the draft that was discarded under
// it is told so, as for any draft that moved. Content equal to the fallback writes nothing.
func (r Repo) saveFirst(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, stackID string, version int64, actor string, copy stacks.WorkingCopy) (Snapshot, bool, error) {
	if version != 0 {
		return Snapshot{}, false, &DraftConflictError{}
	}
	base, err := r.fallback(ctx, q, stackID)
	if err != nil {
		return Snapshot{}, false, err
	}
	copy.Files = stacks.MarkSecretFiles(copy.Files, base.Copy.Files)
	if sameContent(base.Copy, copy) {
		return base, false, nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("new snapshot id: %w", err)
	}
	now := r.now()
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: id.String(), StackID: stackID, Kind: KindDraft, BaseSeq: base.BaseSeq, Version: firstSavedVersion,
		Outcome: OutcomePending, Pins: "{}", CreatedBy: actor, CreatedAt: now.UnixNano(), Origin: OriginDeploy,
	}); err != nil {
		return Snapshot{}, false, fmt.Errorf("insert draft: %w", err)
	}
	if err := r.writeContent(ctx, q, id.String(), copy); err != nil {
		return Snapshot{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, false, fmt.Errorf("commit: %w", err)
	}
	return Snapshot{
		ID: id.String(), StackID: stackID, Kind: KindDraft, BaseSeq: base.BaseSeq, Version: firstSavedVersion,
		Outcome: OutcomePending, CreatedBy: actor, CreatedAt: now, Copy: normalise(copy), IsDraft: true,
	}, true, nil
}

// NewRevision copies the draft into a new revision, numbered after the stack's last one and
// pending until its deploy ends. Each sealed value is opened and sealed again under the new
// snapshot's ID. origin says how the revision came to be: OriginDeploy, or OriginUpdate for an
// Update images deploy.
func (r Repo) NewRevision(ctx context.Context, stackID, actor, origin string) (Snapshot, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Snapshot{}, fmt.Errorf("new snapshot id: %w", err)
	}
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	draft, err := r.readDraft(ctx, q, stackID)
	if err != nil {
		return Snapshot{}, err
	}
	seq, err := q.NextRevisionSeq(ctx, stackID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("next revision: %w", err)
	}
	now := r.now()
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: id.String(), StackID: stackID, Kind: KindRevision, Seq: seq, BaseSeq: draft.BaseSeq, Version: 1,
		Outcome: OutcomePending, Pins: "{}", CreatedBy: actor, CreatedAt: now.UnixNano(), Origin: origin,
	}); err != nil {
		return Snapshot{}, fmt.Errorf("insert revision: %w", err)
	}
	if err := r.writeContent(ctx, q, id.String(), draft.Copy); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit: %w", err)
	}
	return Snapshot{
		ID: id.String(), StackID: stackID, Kind: KindRevision, Seq: seq, BaseSeq: draft.BaseSeq, Version: 1,
		Outcome: OutcomePending, CreatedBy: actor, CreatedAt: now, Origin: origin, Copy: draft.Copy,
	}, nil
}

// RollbackTarget is the revision a roll back restores: the newest revision that ended good, or, when
// the stack's newest revision is itself the good one that runs, the good one before it. A stack with
// only one good revision, or none, has no target (false). Its files and variables are opened.
func (r Repo) RollbackTarget(ctx context.Context, stackID string) (Snapshot, bool, error) {
	row, ok, err := r.rollbackRow(ctx, stackID)
	if err != nil || !ok {
		return Snapshot{}, false, err
	}
	snap := snapshotOf(row)
	if snap.Copy, err = r.readContent(ctx, r.Store.RQ, snap.ID); err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, nil
}

// RollbackInfo is who started the deploy of the revision a roll back would restore, when, and its
// number, without opening its content. It reports false when there is none.
func (r Repo) RollbackInfo(ctx context.Context, stackID string) (DeployedRevision, bool, error) {
	row, ok, err := r.rollbackRow(ctx, stackID)
	if err != nil || !ok {
		return DeployedRevision{}, false, err
	}
	return DeployedRevision{Seq: row.Seq, By: row.CreatedBy, At: time.Unix(0, row.CreatedAt).UTC()}, true, nil
}

func (r Repo) rollbackRow(ctx context.Context, stackID string) (sqlc.StackSnapshot, bool, error) {
	latest, err := r.Store.RQ.LatestAttemptSeq(ctx, stackID)
	if err != nil {
		return sqlc.StackSnapshot{}, false, fmt.Errorf("read the newest revision: %w", err)
	}
	row, err := r.Store.RQ.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.StackSnapshot{}, false, nil
	}
	if err != nil {
		return sqlc.StackSnapshot{}, false, fmt.Errorf("read revision: %w", err)
	}
	if row.Seq < latest {
		// The newest attempt did not end good, so the newest good one is what last ran well.
		return row, true, nil
	}
	// The newest revision is the good one that runs: the way back is the good one before it.
	prev, err := r.Store.RQ.GoodRevisionBefore(ctx, sqlc.GoodRevisionBeforeParams{StackID: stackID, Seq: latest})
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.StackSnapshot{}, false, nil
	}
	if err != nil {
		return sqlc.StackSnapshot{}, false, fmt.Errorf("read revision: %w", err)
	}
	return prev, true, nil
}

// CopyRevision makes a new revision, numbered after the stack's last one and pending until its
// operation ends, from the content, pins and rendering of from: a roll back redeploys exactly what
// from ran, and the history keeps it as a revision of its own. Each sealed value is opened with from
// and sealed again under the new snapshot's ID. The draft is not touched. The new revision names the
// one it copied (restored_from) and carries origin, OriginRollback for a roll back.
func (r Repo) CopyRevision(ctx context.Context, from Snapshot, actor, origin string) (Snapshot, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Snapshot{}, fmt.Errorf("new snapshot id: %w", err)
	}
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)

	ran, err := q.GetSnapshotPinsAndRendered(ctx, from.ID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read the revision to copy: %w", err)
	}
	seq, err := q.NextRevisionSeq(ctx, from.StackID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("next revision: %w", err)
	}
	now := r.now()
	if err := q.InsertSnapshot(ctx, sqlc.InsertSnapshotParams{
		ID: id.String(), StackID: from.StackID, Kind: KindRevision, Seq: seq, BaseSeq: from.Seq, Version: 1,
		Outcome: OutcomePending, Rendered: ran.Rendered, Pins: ran.Pins, CreatedBy: actor, CreatedAt: now.UnixNano(),
		Origin: origin, RestoredFrom: from.Seq,
	}); err != nil {
		return Snapshot{}, fmt.Errorf("insert revision: %w", err)
	}
	if err := r.writeContent(ctx, q, id.String(), from.Copy); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit: %w", err)
	}
	return Snapshot{
		ID: id.String(), StackID: from.StackID, Kind: KindRevision, Seq: seq, BaseSeq: from.Seq, Version: 1,
		Outcome: OutcomePending, CreatedBy: actor, CreatedAt: now, Origin: origin, RestoredFrom: from.Seq, Copy: from.Copy,
	}, nil
}

// RevisionPins are the images a revision was deployed with, by swarm service name, as it stored them.
func (r Repo) RevisionPins(ctx context.Context, snapshotID string) (map[string]string, error) {
	ran, err := r.Store.RQ.GetSnapshotPinsAndRendered(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("read revision: %w", err)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(ran.Pins), &pins); err != nil {
		return nil, fmt.Errorf("read the pins of revision %s: %w", snapshotID, err)
	}
	if pins == nil {
		pins = map[string]string{}
	}
	return pins, nil
}

// RevisionRendering is the masked rendering stored with a revision, as it was stored. A revision still deploying, or
// taken before renderings were stored, has none (nil).
func (r Repo) RevisionRendering(ctx context.Context, snapshotID string) ([]byte, error) {
	ran, err := r.Store.RQ.GetSnapshotPinsAndRendered(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("read revision: %w", err)
	}
	if ran.Rendered == "" {
		return nil, nil
	}
	return []byte(ran.Rendered), nil
}

// SetOutcome marks a revision good or failed and stores what it rendered and the digests it ran
// with. Empty pins keep the pins SetPins stored while the deploy ran (a revision starts with
// "{}").
func (r Repo) SetOutcome(ctx context.Context, snapshotID, outcome, rendered, pins string) error {
	n, err := r.Store.Q.SetRevisionOutcome(ctx, sqlc.SetRevisionOutcomeParams{Outcome: outcome, Rendered: rendered, Pins: pins, ID: snapshotID})
	if err != nil {
		return fmt.Errorf("set revision outcome: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("set revision outcome: no revision %s", snapshotID)
	}
	return nil
}

// SetPins stores on a revision the image each service of it was deployed with, by swarm service
// name: the pinned reference, tag@sha256 (or the image ID of a local image). A deploy stores
// them as soon as it has looked its images up, so a deploy that stops later still records what
// it meant to run; only the pins of a revision that ended good are read back (GoodPins).
func (r Repo) SetPins(ctx context.Context, snapshotID string, pins map[string]string) error {
	if pins == nil {
		pins = map[string]string{}
	}
	raw, err := json.Marshal(pins)
	if err != nil {
		return fmt.Errorf("encode pins: %w", err)
	}
	n, err := r.Store.Q.SetSnapshotPins(ctx, sqlc.SetSnapshotPinsParams{Pins: string(raw), ID: snapshotID})
	if err != nil {
		return fmt.Errorf("set pins: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("set pins: no revision %s", snapshotID)
	}
	return nil
}

// GoodPins is what the stack's newest good revision ran, by swarm service name. AsWritten is the
// stored reference without its digest, which is the text it was deployed from up to how
// docker spells it; a caller compares it with the current text through sameImageText. A stack
// with no good revision has none.
func (r Repo) GoodPins(ctx context.Context, stackID string) (map[string]stacks.LivePin, error) {
	row, err := r.Store.RQ.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read revision: %w", err)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(row.Pins), &pins); err != nil {
		return nil, fmt.Errorf("read the pins of revision %d: %w", row.Seq, err)
	}
	return LivePins(pins), nil
}

// LivePins reads pins by swarm service name, tag@sha256 each, as stacks.Render and the review take
// them: AsWritten is the reference without its digest.
func LivePins(pins map[string]string) map[string]stacks.LivePin {
	out := make(map[string]stacks.LivePin, len(pins))
	for service, ref := range pins {
		text := ref
		if i := strings.LastIndex(ref, "@"); i > 0 {
			text = ref[:i]
		}
		out[service] = stacks.LivePin{AsWritten: text, Image: ref}
	}
	return out
}

// SetRendered stores the masked rendering (stacks.Render) of what a revision deploys. The next
// review compares the draft's rendering with the newest good revision's, so a deploy stores it.
func (r Repo) SetRendered(ctx context.Context, snapshotID string, rendered []byte) error {
	n, err := r.Store.Q.SetSnapshotRendered(ctx, sqlc.SetSnapshotRenderedParams{Rendered: string(rendered), ID: snapshotID})
	if err != nil {
		return fmt.Errorf("set rendering: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("set rendering: no revision %s", snapshotID)
	}
	return nil
}

// GoodRendering is the masked rendering stored with the stack's newest good revision. A stack with
// no good revision, or one deployed before renderings were stored, has none (nil).
func (r Repo) GoodRendering(ctx context.Context, stackID string) ([]byte, error) {
	row, err := r.Store.RQ.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read revision: %w", err)
	}
	if row.Rendered == "" {
		return nil, nil
	}
	return []byte(row.Rendered), nil
}

// Revision reads one revision of a stack by its number, with its files and variables opened. A
// number the stack has no revision for is ErrRevisionNotFound.
func (r Repo) Revision(ctx context.Context, stackID string, seq int64) (Snapshot, error) {
	row, err := r.Store.RQ.GetRevision(ctx, sqlc.GetRevisionParams{StackID: stackID, Seq: seq})
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrRevisionNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read revision: %w", err)
	}
	snap := snapshotOf(row)
	if snap.Copy, err = r.readContent(ctx, r.Store.RQ, snap.ID); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// RebaseDraft says the stack's draft now starts from revision seq. A deploy of the draft calls it
// once it has taken the revision, so the draft is not later taken for one made before a newer
// deployment. A stack with no draft has nothing to rebase.
func (r Repo) RebaseDraft(ctx context.Context, stackID string, seq int64) error {
	if _, err := r.Store.Q.SetDraftBaseSeq(ctx, sqlc.SetDraftBaseSeqParams{BaseSeq: seq, StackID: stackID}); err != nil {
		return fmt.Errorf("rebase draft: %w", err)
	}
	return nil
}

// GoodRevisionInfo is who started the deploy of the stack's newest good revision, when, and its
// number, without opening its content. It reports false when the stack has none.
func (r Repo) GoodRevisionInfo(ctx context.Context, stackID string) (DeployedRevision, bool, error) {
	row, err := r.Store.RQ.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployedRevision{}, false, nil
	}
	if err != nil {
		return DeployedRevision{}, false, fmt.Errorf("read revision: %w", err)
	}
	return DeployedRevision{Seq: row.Seq, By: row.CreatedBy, At: time.Unix(0, row.CreatedAt).UTC()}, true, nil
}

// LatestGoodRevision is the newest revision whose deploy succeeded, with its files and
// variables opened. It reports false when the stack has none.
func (r Repo) LatestGoodRevision(ctx context.Context, stackID string) (Snapshot, bool, error) {
	row, err := r.Store.RQ.LatestGoodRevision(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("read revision: %w", err)
	}
	snap := snapshotOf(row)
	if snap.Copy, err = r.readContent(ctx, r.Store.RQ, snap.ID); err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, nil
}

// SetStopped records that a person stopped the stack (true) or that it was started or deployed again
// (false). It is the stack's own record, read beside what the engine runs (STK-11).
func (r Repo) SetStopped(ctx context.Context, stackID string, stopped bool) error {
	var v int64
	if stopped {
		v = 1
	}
	n, err := r.Store.Q.SetStackStopped(ctx, sqlc.SetStackStoppedParams{Stopped: v, ID: stackID})
	if err != nil {
		return fmt.Errorf("set stopped: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("set stopped: no stack %s", stackID)
	}
	return nil
}

// DeleteStack deletes the stack with its drafts, revisions, files, variables and operations (STK-11). A stack that
// is not there is ErrStackNotFound, so a second removal changes nothing.
func (r Repo) DeleteStack(ctx context.Context, stackID string) error {
	n, err := r.Store.Q.DeleteStack(ctx, stackID)
	if err != nil {
		return fmt.Errorf("delete stack: %w", err)
	}
	if n != 1 {
		return ErrStackNotFound
	}
	return nil
}

// LatestDeployLikes is the newest deploy, update or roll back of each stack of an environment, by stack ID,
// whatever came after it. A stop or a start does not hide a deploy that failed.
func (r Repo) LatestDeployLikes(ctx context.Context, envID string) (map[string]DeployRow, error) {
	rows, err := r.Store.RQ.LatestDeployLikeStackDeploysByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list deploys: %w", err)
	}
	out := make(map[string]DeployRow, len(rows))
	for _, row := range rows {
		out[row.StackID] = deployOf(row)
	}
	return out, nil
}

// LatestDeployLike is the newest deploy, update or roll back of one stack, or false when it has had none.
func (r Repo) LatestDeployLike(ctx context.Context, stackID string) (DeployRow, bool, error) {
	row, err := r.Store.RQ.LatestDeployLikeStackDeploy(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployRow{}, false, nil
	}
	if err != nil {
		return DeployRow{}, false, fmt.Errorf("read deploy: %w", err)
	}
	return deployOf(row), true, nil
}

// StartDeploy records an operation as running.
func (r Repo) StartDeploy(ctx context.Context, stackID, envID, kind string, revisionSeq int64, jobID, actor string) (DeployRow, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return DeployRow{}, fmt.Errorf("new deploy id: %w", err)
	}
	now := r.now()
	if err := r.Store.Q.InsertStackDeploy(ctx, sqlc.InsertStackDeployParams{
		ID: id.String(), StackID: stackID, EnvID: envID, Kind: kind, RevisionSeq: revisionSeq,
		JobID: jobID, StartedBy: actor, StartedAt: now.UnixNano(),
	}); err != nil {
		return DeployRow{}, fmt.Errorf("record deploy: %w", err)
	}
	return DeployRow{
		ID: id.String(), StackID: stackID, EnvID: envID, Kind: kind, State: DeployRunning,
		RevisionSeq: revisionSeq, JobID: jobID, StartedBy: actor, StartedAt: now,
	}, nil
}

// FinishDeploy ends a running operation in state (done, failed, cancelled or interrupted) with
// the service it stopped at and a code.
func (r Repo) FinishDeploy(ctx context.Context, id, state, failedService, code string) error {
	n, err := r.Store.Q.FinishStackDeploy(ctx, sqlc.FinishStackDeployParams{
		State: state, EndedAt: r.now().UnixNano(), FailedService: failedService, Code: code, ID: id,
	})
	if err != nil {
		return fmt.Errorf("finish deploy: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("finish deploy: no running deploy %s", id)
	}
	return nil
}

// MarkInterrupted ends every operation still recorded as running as interrupted, and fails the
// revision a deploy or an update of them had taken. The server runs it once, when the store is open
// and before it serves anything: the jobs and the operation lock are in memory, so an operation that
// was running when the server stopped is not running now, and the swarm may be partly updated. The
// stack then reads Deploy failed and offers a roll back (Open Q5). It returns how many operations
// it ended.
func (r Repo) MarkInterrupted(ctx context.Context) (int, error) {
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)
	// The revisions first: they are found through the deploys that are still running.
	if _, err := q.MarkRunningRevisionsFailed(ctx); err != nil {
		return 0, fmt.Errorf("fail the revisions of interrupted deploys: %w", err)
	}
	n, err := q.MarkRunningDeploysInterrupted(ctx, r.now().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("mark running deploys interrupted: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int(n), nil
}

// LatestDeploys is the newest operation of each stack of an environment, by stack ID.
func (r Repo) LatestDeploys(ctx context.Context, envID string) (map[string]DeployRow, error) {
	rows, err := r.Store.RQ.LatestStackDeploysByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list deploys: %w", err)
	}
	out := make(map[string]DeployRow, len(rows))
	for _, row := range rows {
		out[row.StackID] = deployOf(sqlc.StackDeploy(row))
	}
	return out, nil
}

// LastDeployed is the newest operation of each stack of an environment that put a definition on
// the target and succeeded (a deploy, an update or a rollback), by stack ID.
func (r Repo) LastDeployed(ctx context.Context, envID string) (map[string]DeployRow, error) {
	rows, err := r.Store.RQ.LatestDoneStackDeploysByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list deploys: %w", err)
	}
	out := make(map[string]DeployRow, len(rows))
	for _, row := range rows {
		out[row.StackID] = deployOf(sqlc.StackDeploy(row))
	}
	return out, nil
}

// LatestDeploy is the newest operation of one stack, or false when it has none.
func (r Repo) LatestDeploy(ctx context.Context, stackID string) (DeployRow, bool, error) {
	row, err := r.Store.RQ.LatestStackDeploy(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployRow{}, false, nil
	}
	if err != nil {
		return DeployRow{}, false, fmt.Errorf("read deploy: %w", err)
	}
	return deployOf(row), true, nil
}

// DraftSave is the draft of a stack: who last saved it, when, and whether it holds changes that have not
// been deployed.
type DraftSave struct {
	By      string
	At      time.Time
	Pending bool
}

// DraftSaves says, for each stack of an environment that has a draft, who saved it last and when, and
// whether it holds changes that have not been deployed: it was saved after the stack's newest revision was
// taken, or the stack has no revision and its draft has been saved since it was created.
//
// A roll back revision does not count as "the newest revision". It copies an earlier revision and deploys
// nothing of the draft, which a roll back leaves untouched (D-03), so a draft saved before the roll back still
// holds the changes it held (RESEARCH Pitfall 8). Counting it would make a draft read as deployed, and the list
// would lose its Draft badge. The next deploy of that draft is told the deployed version moved by the
// concurrent check instead, which compares revision numbers and not times.
func (r Repo) DraftSaves(ctx context.Context, envID string) (map[string]DraftSave, error) {
	drafts, err := r.Store.RQ.ListDraftsByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	taken, err := r.Store.RQ.ListLatestRevisionTimesByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	revisionAt := make(map[string]int64, len(taken))
	for _, t := range taken {
		revisionAt[t.StackID] = t.TakenAt
	}
	out := make(map[string]DraftSave, len(drafts))
	for _, d := range drafts {
		at, deployed := revisionAt[d.StackID]
		out[d.StackID] = DraftSave{
			By: d.CreatedBy, At: time.Unix(0, d.CreatedAt).UTC(),
			Pending: (deployed && d.CreatedAt > at) || (!deployed && d.Version > 1),
		}
	}
	return out, nil
}

// DraftStates says, for each stack of an environment, whether its draft holds changes that
// have not been deployed (see DraftSaves).
func (r Repo) DraftStates(ctx context.Context, envID string) (map[string]bool, error) {
	saves, err := r.DraftSaves(ctx, envID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(saves))
	for id, s := range saves {
		out[id] = s.Pending
	}
	return out, nil
}

// writeContent stores a working copy under snapshotID, sealing each secret value under its
// row-bound context. A file is stored as written; a path may appear once.
func (r Repo) writeContent(ctx context.Context, q *sqlc.Queries, snapshotID string, copy stacks.WorkingCopy) error {
	for _, f := range copy.Files {
		row := sqlc.InsertSnapshotFileParams{SnapshotID: snapshotID, Path: f.Path}
		if f.Secret {
			sealed, err := r.Vault.Seal(fileContext(snapshotID, f.Path), f.Content)
			if err != nil {
				return fmt.Errorf("seal file %s: %w", f.Path, err)
			}
			row.ContentSealed, row.Secret = sealed, 1
		} else {
			row.Content = string(f.Content)
		}
		if err := q.InsertSnapshotFile(ctx, row); err != nil {
			return fmt.Errorf("store file %s: %w", f.Path, err)
		}
	}
	for _, v := range copy.Vars {
		row := sqlc.InsertSnapshotVarParams{SnapshotID: snapshotID, Name: v.Name}
		if v.Secret {
			sealed, err := r.Vault.Seal(varContext(snapshotID, v.Name), []byte(v.Value))
			if err != nil {
				return fmt.Errorf("seal variable %s: %w", v.Name, err)
			}
			row.ValueSealed, row.Secret = sealed, 1
		} else {
			row.Value = v.Value
		}
		if err := q.InsertSnapshotVar(ctx, row); err != nil {
			return fmt.Errorf("store variable %s: %w", v.Name, err)
		}
	}
	return nil
}

// readContent reads the files and variables of a snapshot and opens the sealed ones.
func (r Repo) readContent(ctx context.Context, q *sqlc.Queries, snapshotID string) (stacks.WorkingCopy, error) {
	var wc stacks.WorkingCopy
	files, err := q.ListSnapshotFiles(ctx, snapshotID)
	if err != nil {
		return wc, fmt.Errorf("read files: %w", err)
	}
	for _, f := range files {
		file := stacks.File{Path: f.Path, Content: []byte(f.Content), Secret: f.Secret != 0}
		if file.Secret {
			plain, err := r.Vault.Open(fileContext(snapshotID, f.Path), f.ContentSealed)
			if err != nil {
				return wc, fmt.Errorf("open file %s: %w", f.Path, err)
			}
			file.Content = plain
		}
		wc.Files = append(wc.Files, file)
	}
	vars, err := q.ListSnapshotVars(ctx, snapshotID)
	if err != nil {
		return wc, fmt.Errorf("read variables: %w", err)
	}
	for _, v := range vars {
		variable := stacks.Var{Name: v.Name, Value: v.Value, Secret: v.Secret != 0}
		if variable.Secret {
			plain, err := r.Vault.Open(varContext(snapshotID, v.Name), v.ValueSealed)
			if err != nil {
				return wc, fmt.Errorf("open variable %s: %w", v.Name, err)
			}
			variable.Value = string(plain)
		}
		wc.Vars = append(wc.Vars, variable)
	}
	return wc, nil
}

// normalise returns a copy of wc with files sorted by path and variables by name, content never
// nil, so two working copies with the same meaning compare and store the same.
func normalise(wc stacks.WorkingCopy) stacks.WorkingCopy {
	out := stacks.WorkingCopy{
		Files: slices.Clone(wc.Files),
		Vars:  slices.Clone(wc.Vars),
	}
	for i := range out.Files {
		if out.Files[i].Content == nil {
			out.Files[i].Content = []byte{}
		}
	}
	slices.SortFunc(out.Files, func(a, b stacks.File) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(out.Vars, func(a, b stacks.Var) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// sameContent reports whether two working copies hold the same files and variables.
func sameContent(a, b stacks.WorkingCopy) bool {
	a, b = normalise(a), normalise(b)
	return slices.EqualFunc(a.Files, b.Files, func(x, y stacks.File) bool {
		return x.Path == y.Path && x.Secret == y.Secret && string(x.Content) == string(y.Content)
	}) && slices.Equal(a.Vars, b.Vars)
}
