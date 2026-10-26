// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
)

// ErrNoDeployedRevision is a stack with no revision that deployed, which has nothing to take as the base of a
// rebuild, and nothing to restore.
var ErrNoDeployedRevision = errors.New("stackops: the stack has no deployed revision")

// ErrStackStopped is a stack a person stopped. Its replicated services run no replicas and its global and job
// services are gone from the engine, so a rebuild from what runs would write replicas 0 and drop them; a stopped
// stack is not drift (D-21), and there is nothing running to keep.
var ErrStackStopped = errors.New("stackops: the stack is stopped")

// AcceptLiveInput says which stack's draft is rebuilt from what runs. DraftVersion is the version of the draft the
// person saw (0 for a stack that has none), so a draft that moved since is not overwritten unseen.
type AcceptLiveInput struct {
	EnvID        string
	Stack        Stack
	DraftVersion int64
	Caller       Caller
}

// AcceptLiveResult is the draft an accept saved, the number of the revision it starts from and how many services it
// was rebuilt from.
type AcceptLiveResult struct {
	Draft    Snapshot
	Revision int64
	Services int
}

// AcceptLive turns what runs into the stack's next draft (D-23). The draft becomes the deployed revision's files and
// variables with a compose.yaml rebuilt from the engine by the adoption rebuilder (D-12), made on top of that
// revision (D-54): the stack's own secrets and configs stay the files they were, an image keeps the text it was
// written with while it is the image that runs, and a value that is a secret variable's is written as ${NAME}, so
// no secret's value is copied into the file. The person reviews the draft and deploys it, which records a revision.
//
// Nothing is written to any engine: the rebuild only reads. A rebuild that fails, or whose file does not pass the
// check every stack passes against the revision's own variables and files, leaves the draft as it was. A draft that
// has moved since DraftVersion is a *DraftConflictError. A stack that is stopped is refused with ErrStackStopped
// before anything is read.
func (d Deps) AcceptLive(ctx context.Context, repo Repo, in AcceptLiveInput) (AcceptLiveResult, error) {
	if in.Stack.Stopped {
		return AcceptLiveResult{}, ErrStackStopped
	}
	deployed, ok, err := repo.LatestGoodRevision(ctx, in.Stack.ID)
	if err != nil {
		return AcceptLiveResult{}, err
	}
	if !ok {
		return AcceptLiveResult{}, ErrNoDeployedRevision
	}
	base, err := baseOf(ctx, in.Stack, deployed.Copy)
	if err != nil {
		return AcceptLiveResult{}, err
	}
	res, err := d.RebuildLive(ctx, in.EnvID, in.Stack, RebuildOptions{Reveal: true, Base: base, Beside: deployed.Copy})
	if err != nil {
		return AcceptLiveResult{}, err
	}

	// The revision's files keep their order, with compose.yaml replaced where it was. RebuildLive has already
	// checked the file beside them and the revision's variables, as a deploy would load it.
	next := stacks.WorkingCopy{Files: slices.Clone(deployed.Copy.Files), Vars: slices.Clone(deployed.Copy.Vars)}
	if i := slices.IndexFunc(next.Files, func(f stacks.File) bool { return f.Path == stacks.ComposeFile }); i >= 0 {
		next.Files[i].Content = res.YAML
	} else {
		next.Files = append([]stacks.File{{Path: stacks.ComposeFile, Content: res.YAML}}, next.Files...)
	}

	saved, _, err := repo.SaveDraft(ctx, in.Stack.ID, in.DraftVersion, in.Caller.Actor, next)
	if err != nil {
		return AcceptLiveResult{}, err
	}
	if err := repo.RebaseDraft(ctx, in.Stack.ID, deployed.Seq); err != nil {
		return AcceptLiveResult{}, err
	}
	saved.BaseSeq = deployed.Seq
	return AcceptLiveResult{Draft: saved, Revision: deployed.Seq, Services: len(res.Project.Services)}, nil
}

// baseOf is the base a rebuild is made on: the revision's compose file loaded as every deploy loads it, with the
// values of its secret variables and the text of its images.
func baseOf(ctx context.Context, st Stack, copy stacks.WorkingCopy) (*rebuild.Base, error) {
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: copy})
	if err != nil {
		var refused *stacks.NotDeployableError
		if errors.As(err, &refused) && len(refused.Problems) > 0 {
			return nil, &RebuildError{Reason: "the deployed revision can't be read: " + refused.Problems[0].Message}
		}
		return nil, fmt.Errorf("stackops: load the deployed revision: %w", err)
	}
	base := &rebuild.Base{
		Project: loaded.Project, SecretValues: map[string]string{},
		ImageText: rebuild.ImageTextOf(copy.Compose()),
	}
	// Two secret variables with one value are written as the one whose name sorts first, so the same input gives
	// the same file.
	vars := slices.Clone(copy.Vars)
	slices.SortFunc(vars, func(x, y stacks.Var) int { return cmp.Compare(x.Name, y.Name) })
	for _, v := range vars {
		if !v.Secret || v.Value == "" {
			continue
		}
		if _, taken := base.SecretValues[v.Value]; !taken {
			base.SecretValues[v.Value] = v.Name
		}
	}
	return base, nil
}
