// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx

import (
	"container/heap"
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	// DefaultTouchLimit is how many commits CommitsTouching walks when it is not given a limit (RESEARCH Code Example
	// 3): a repository with more between the deployed commit and the head reports Capped.
	DefaultTouchLimit = 2000
	// MaxTouchCommits is how many of the commits that touched the folder CommitsTouching names; Count is the whole
	// number.
	MaxTouchCommits = 50
	// MaxSubjectChars is the longest commit subject or author name kept; a longer one is cut (T-09-47).
	MaxSubjectChars = 200
)

// TouchCommit is a commit that changed a stack folder.
type TouchCommit struct {
	SHA string
	// Subject is the first line of the message and Author the author's name, each at most MaxSubjectChars characters.
	Subject string
	Author  string
	When    time.Time
}

// Touch is the commits between a deployed commit and a head that changed one folder.
type Touch struct {
	// Count is how many commits changed the folder. It is 0 when the base is not an ancestor of the head.
	Count int
	// Capped says the walk stopped at the limit: the folder may have been changed by more commits than Count.
	Capped bool
	// NotAncestor says the base is not an ancestor of the head (the branch was force-pushed, or a tag moved), or the
	// mirror does not hold the base. Count and Commits are then empty: the commits between the two are not defined.
	NotAncestor bool
	// Commits are the first MaxTouchCommits of them, newest first, ties by SHA.
	Commits []TouchCommit
}

// CommitsTouching counts the commits reachable from head and not from base that changed the folder dir ("" is the
// root), reading the object database of the mirror m only (D-13). A commit changed the folder when the hash of the
// folder's tree differs from that of each of its parents, so a merge that brings a branch in is not counted as well as
// the branch's own commits. At most limit commits are walked (DefaultTouchLimit when it is not positive). A head the
// mirror does not hold is commit_missing.
func (g *GoGit) CommitsTouching(ctx context.Context, m MirrorID, head, base, dir string, limit int) (Touch, error) {
	if err := ctx.Err(); err != nil {
		return Touch{}, Classify(err, "")
	}
	if limit <= 0 {
		limit = DefaultTouchLimit
	}
	path, err := g.mirrorPath(m)
	if err != nil {
		return Touch{}, err
	}
	ctx, release, err := g.readLock(ctx, m)
	if err != nil {
		return Touch{}, err
	}
	defer release()

	repo, err := openExisting(path)
	if errors.Is(err, errMirrorMissing) {
		return Touch{}, &Error{Code: CodeCommitMissing}
	}
	if err != nil {
		g.log.Error("could not open a git mirror", "error", err)
		return Touch{}, &Error{Code: CodeUnreachable}
	}
	g.markUsed(m, path)
	headCommit, err := repo.CommitObject(plumbing.NewHash(head))
	if err != nil {
		return Touch{}, &Error{Code: CodeCommitMissing}
	}
	baseCommit, err := repo.CommitObject(plumbing.NewHash(base))
	if err != nil {
		return Touch{NotAncestor: true}, nil
	}
	if baseCommit.Hash == headCommit.Hash {
		return Touch{}, nil
	}
	dir = strings.Trim(dir, "/")
	folderOf := folderHashes(dir)

	var out Touch
	var touched []TouchCommit
	walk, err := walkRange(ctx, headCommit, baseCommit, limit)
	if err != nil {
		return Touch{}, err
	}
	out.Capped = walk.capped
	for _, c := range walk.commits {
		changed, err := changesFolder(c, folderOf)
		if err != nil {
			return Touch{}, &Error{Code: CodeCommitMissing}
		}
		if changed {
			touched = append(touched, TouchCommit{
				SHA: c.Hash.String(), Subject: clip(firstLine(c.Message)), Author: clip(c.Author.Name), When: c.Author.When.UTC(),
			})
		}
	}
	if !walk.reached && !out.Capped {
		return Touch{NotAncestor: true}, nil
	}
	slices.SortFunc(touched, func(a, b TouchCommit) int {
		if c := b.When.Compare(a.When); c != 0 {
			return c
		}
		return strings.Compare(a.SHA, b.SHA)
	})
	out.Count = len(touched)
	out.Commits = touched
	if len(out.Commits) > MaxTouchCommits {
		out.Commits = out.Commits[:MaxTouchCommits]
	}
	return out, nil
}

// walkSlop is how many commits are still looked at once every commit left to walk is known to be reachable from the
// base: the times of commits made on different machines disagree a little, and the walk goes by time.
const walkSlop = 5

// rangeWalk is what walkRange found.
type rangeWalk struct {
	// commits are the commits reachable from the head and not from the base, in no particular order.
	commits []*object.Commit
	// reached says the base is an ancestor of the head.
	reached bool
	// capped says the walk stopped at its limit.
	capped bool
}

// commitQueue is the commits still to look at, the newest first (ties by hash, so that a walk is the same every time).
type commitQueue []*object.Commit

func (q commitQueue) Len() int { return len(q) }
func (q commitQueue) Less(i, j int) bool {
	if c := q[i].Committer.When.Compare(q[j].Committer.When); c != 0 {
		return c > 0
	}
	return q[i].Hash.String() < q[j].Hash.String()
}
func (q commitQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *commitQueue) Push(x any) {
	if c, ok := x.(*object.Commit); ok {
		*q = append(*q, c)
	}
}
func (q *commitQueue) Pop() any {
	old := *q
	c := old[len(old)-1]
	*q = old[:len(old)-1]
	return c
}

// walkRange finds the commits reachable from head and not from base (git rev-list base..head). The two histories are
// walked together, newest first, and the walk ends once every commit left to look at is reachable from the base, so
// the base's whole history is not read. Handing the base alone to a history walk as the commit to leave out would
// leave the commits behind it in, and a merge of a branch that forked before the base brings those back (WR-A04). At
// most limit commits not known to be reachable from the base are walked (capped when there are more), and the work on
// the base's side is bounded too: the commits between a deployed commit and a head are few, a history is not.
func walkRange(ctx context.Context, head, base *object.Commit, limit int) (rangeWalk, error) {
	var out rangeWalk
	behindBase := map[plumbing.Hash]bool{base.Hash: true}
	looked := map[plumbing.Hash]*object.Commit{}
	queued := map[plumbing.Hash]bool{head.Hash: true, base.Hash: true}
	q := &commitQueue{head, base}
	heap.Init(q)

	// markBehind says that h, and the commits below it that were looked at already, are reachable from the base.
	markBehind := func(h plumbing.Hash) {
		stack := []plumbing.Hash{h}
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if behindBase[x] {
				continue
			}
			behindBase[x] = true
			if c, ok := looked[x]; ok {
				stack = append(stack, c.ParentHashes...)
			}
		}
	}
	allBehind := func() bool {
		for _, c := range *q {
			if !behindBase[c.Hash] {
				return false
			}
		}
		return true
	}

	var walked []*object.Commit
	slop := walkSlop
	steps := 0
	for q.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return rangeWalk{}, Classify(err, "")
		}
		if allBehind() {
			if slop--; slop < 0 {
				break
			}
		} else {
			slop = walkSlop
		}
		if steps++; steps > 4*limit+walkSlop {
			out.capped = true
			break
		}
		c, ok := heap.Pop(q).(*object.Commit)
		if !ok {
			break
		}
		looked[c.Hash] = c
		if behindBase[c.Hash] {
			for _, p := range c.ParentHashes {
				markBehind(p)
			}
		} else {
			if len(walked) >= limit {
				out.capped = true
				break
			}
			walked = append(walked, c)
			if slices.Contains(c.ParentHashes, base.Hash) {
				out.reached = true
			}
		}
		for i, p := range c.ParentHashes {
			if queued[p] {
				continue
			}
			queued[p] = true
			pc, err := c.Parent(i)
			if err != nil {
				return rangeWalk{}, &Error{Code: CodeCommitMissing}
			}
			heap.Push(q, pc)
		}
	}
	for _, c := range walked {
		if !behindBase[c.Hash] {
			out.commits = append(out.commits, c)
		}
	}
	return out, nil
}

// folderHashes returns a function that answers the hash of the tree of the folder dir at a commit, or the zero hash
// when the commit has no such folder, remembering each answer: the walk asks about every commit once as itself and once
// for each child that names it as a parent.
func folderHashes(dir string) func(c *object.Commit) (plumbing.Hash, error) {
	memo := map[plumbing.Hash]plumbing.Hash{}
	return func(c *object.Commit) (plumbing.Hash, error) {
		if h, ok := memo[c.Hash]; ok {
			return h, nil
		}
		t, err := c.Tree()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		h := t.Hash
		if dir != "" {
			e, err := t.FindEntry(dir)
			switch {
			case errors.Is(err, object.ErrEntryNotFound), errors.Is(err, object.ErrDirectoryNotFound):
				h = plumbing.ZeroHash
			case err != nil:
				return plumbing.ZeroHash, err
			default:
				h = e.Hash
			}
		}
		memo[c.Hash] = h
		return h, nil
	}
}

// changesFolder says whether the commit's folder differs from the folder of each of its parents; a commit with no
// parent changed whatever it holds.
func changesFolder(c *object.Commit, folderOf func(*object.Commit) (plumbing.Hash, error)) (bool, error) {
	cur, err := folderOf(c)
	if err != nil {
		return false, err
	}
	if len(c.ParentHashes) == 0 {
		return cur != plumbing.ZeroHash, nil
	}
	for i := range c.ParentHashes {
		p, err := c.Parent(i)
		if err != nil {
			return false, err
		}
		ph, err := folderOf(p)
		if err != nil {
			return false, err
		}
		if ph == cur {
			return false, nil
		}
	}
	return true, nil
}

// firstLine is the subject of a commit message.
func firstLine(msg string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return strings.TrimSpace(line)
}

// clip cuts s to MaxSubjectChars characters without splitting one.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= MaxSubjectChars {
		return s
	}
	return string([]rune(s)[:MaxSubjectChars])
}
