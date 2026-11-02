// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package gittest builds real git repositories and serves them over smart HTTP, for tests that
// need a git server without the network. It uses the git command line and git-http-backend, and
// fails the test, never skips it, when either is missing: a runner without git would otherwise
// pass the git tests by not running them (scripts/ci-preflight.sh checks the same).
package gittest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Delete, as the content of a file in Commit, removes the file.
const Delete = "\x00gittest:delete"

// Repo is a bare repository and the working tree commits are made in.
type Repo struct {
	bare    string
	work    string
	env     []string
	orphans int
}

// requireGit fails the test when git is not on the PATH.
func requireGit(tb testing.TB) string {
	tb.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		tb.Fatal("git is required for the git fixtures; see scripts/ci-preflight.sh")
	}
	return path
}

// NewRepo makes an empty bare repository whose HEAD is main, with a working tree beside it.
func NewRepo(tb testing.TB) *Repo {
	tb.Helper()
	requireGit(tb)
	root := tb.TempDir()
	r := &Repo{
		bare: filepath.Join(root, "repo.git"),
		work: filepath.Join(root, "work"),
		env: append(os.Environ(),
			"HOME="+root,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=TierPlan Test", "GIT_AUTHOR_EMAIL=test@tierplan.invalid",
			"GIT_COMMITTER_NAME=TierPlan Test", "GIT_COMMITTER_EMAIL=test@tierplan.invalid",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		),
	}
	if err := os.MkdirAll(r.work, 0o750); err != nil {
		tb.Fatal(err)
	}
	r.git(tb, root, "init", "--bare", "-b", "main", r.bare)
	r.git(tb, r.work, "init", "-b", "main")
	r.git(tb, r.work, "config", "commit.gpgsign", "false")
	r.git(tb, r.work, "remote", "add", "origin", r.bare)
	return r
}

// Dir is the bare repository's directory, the name of which ends in .git.
func (r *Repo) Dir() string { return r.bare }

// Commit writes files into the working tree (a file whose content is Delete is removed), commits
// them on the current branch with msg and pushes the branch to the bare repository. It returns the
// commit's ID.
func (r *Repo) Commit(tb testing.TB, files map[string]string, msg string) string {
	tb.Helper()
	r.writeFiles(tb, files)
	r.git(tb, r.work, "add", "-A")
	r.git(tb, r.work, "commit", "--allow-empty", "-m", msg)
	sha := strings.TrimSpace(r.git(tb, r.work, "rev-parse", "HEAD"))
	r.git(tb, r.work, "push", "--force", "origin", "HEAD:refs/heads/main")
	return sha
}

// Fork makes a commit that writes files on a new branch started at the commit from, in the working tree only: nothing is
// pushed, and main is checked out again. It returns the commit's ID. Merge brings the branch in.
func (r *Repo) Fork(tb testing.TB, from, branch string, files map[string]string, msg string) string {
	tb.Helper()
	r.git(tb, r.work, "checkout", "-b", branch, from)
	r.writeFiles(tb, files)
	r.git(tb, r.work, "add", "-A")
	r.git(tb, r.work, "commit", "--allow-empty", "-m", msg)
	sha := strings.TrimSpace(r.git(tb, r.work, "rev-parse", "HEAD"))
	r.git(tb, r.work, "checkout", "main")
	return sha
}

// Merge merges the branch Fork made into main with a merge commit and pushes main. It returns the merge commit's ID.
func (r *Repo) Merge(tb testing.TB, branch, msg string) string {
	tb.Helper()
	r.git(tb, r.work, "merge", "--no-ff", "-m", msg, branch)
	sha := strings.TrimSpace(r.git(tb, r.work, "rev-parse", "HEAD"))
	r.git(tb, r.work, "push", "--force", "origin", "HEAD:refs/heads/main")
	return sha
}

// writeFiles writes files into the working tree; a file whose content is Delete is removed.
func (r *Repo) writeFiles(tb testing.TB, files map[string]string) {
	tb.Helper()
	for name, content := range files {
		p := filepath.Join(r.work, filepath.FromSlash(name))
		if content == Delete {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				tb.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
}

// CommitMany makes n commits on the current branch with one fast-import, which is how a test gets a long history
// quickly, and pushes the branch. Commit i (from 0) writes the file file(i) with content "i". It returns the last
// commit's ID. The repository needs a commit already.
func (r *Repo) CommitMany(tb testing.TB, n int, file func(i int) string) string {
	tb.Helper()
	var in bytes.Buffer
	const base = 1893456000 // a time after the commits Commit makes, and rising
	for i := range n {
		msg := fmt.Sprintf("bulk %d", i)
		content := fmt.Sprintf("%d\n", i)
		fmt.Fprintf(&in, "commit refs/heads/main\ncommitter TierPlan Test <test@tierplan.invalid> %d +0000\ndata %d\n%s\n", base+i, len(msg), msg)
		if i == 0 {
			in.WriteString("from refs/heads/main^0\n")
		}
		fmt.Fprintf(&in, "M 100644 inline %s\ndata %d\n%s\n", file(i), len(content), content)
	}
	cmd := exec.Command("git", "fast-import", "--quiet") //nolint:gosec // G204: constant arguments
	cmd.Dir = r.work
	cmd.Env = r.env
	cmd.Stdin = &in
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		tb.Fatalf("git fast-import: %v\n%s", err, errb.String())
	}
	r.git(tb, r.work, "reset", "--hard", "main")
	sha := strings.TrimSpace(r.git(tb, r.work, "rev-parse", "HEAD"))
	r.git(tb, r.work, "push", "--force", "origin", "HEAD:refs/heads/main")
	return sha
}

// Orphan makes a commit with no parent that holds only files, and force-pushes it as main: the history so far is
// no longer reachable from main, as after a force-push of an unrelated history. Later calls of Commit build on it.
func (r *Repo) Orphan(tb testing.TB, files map[string]string, msg string) string {
	tb.Helper()
	r.orphans++
	r.git(tb, r.work, "checkout", "--orphan", fmt.Sprintf("orphan-%d", r.orphans))
	r.git(tb, r.work, "rm", "-rf", "--ignore-unmatch", ".")
	return r.Commit(tb, files, msg)
}

// Tree is the hash of the tree object of the folder dir of the commit sha ("" is the commit's root tree), as
// the git command line reports it.
func (r *Repo) Tree(tb testing.TB, sha, dir string) string {
	tb.Helper()
	spec := sha + "^{tree}"
	if dir != "" {
		spec = sha + ":" + dir
	}
	return strings.TrimSpace(r.git(tb, r.bare, "rev-parse", spec))
}

// Symlink makes name in the working tree a symbolic link to target, for the next Commit to pick up.
func (r *Repo) Symlink(tb testing.TB, name, target string) {
	tb.Helper()
	p := filepath.Join(r.work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		tb.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		tb.Fatal(err)
	}
}

// Submodule makes name in the working tree a nested repository with one commit, which the next
// Commit records as a submodule entry (a gitlink, mode 160000).
func (r *Repo) Submodule(tb testing.TB, name string) {
	tb.Helper()
	p := filepath.Join(r.work, filepath.FromSlash(name))
	if err := os.MkdirAll(p, 0o750); err != nil {
		tb.Fatal(err)
	}
	r.git(tb, p, "init", "-b", "main")
	r.git(tb, p, "config", "commit.gpgsign", "false")
	r.git(tb, p, "commit", "--allow-empty", "-m", "nested")
}

// Tag makes the tag name at the commit sha in the bare repository. An annotated tag is a tag
// object with a message; a lightweight one is a plain reference.
func (r *Repo) Tag(tb testing.TB, name, sha string, annotated bool) {
	tb.Helper()
	args := []string{"tag", "-f"}
	if annotated {
		args = append(args, "-a", "-m", "tag "+name)
	}
	r.git(tb, r.bare, append(args, name, sha)...)
}

// Branch makes (or moves) the branch name to the commit sha in the bare repository.
func (r *Repo) Branch(tb testing.TB, name, sha string) {
	tb.Helper()
	r.git(tb, r.bare, "update-ref", "refs/heads/"+name, sha)
}

// Ref makes (or moves) any reference, such as refs/pull/1/head, to the commit sha in the bare
// repository.
func (r *Repo) Ref(tb testing.TB, name, sha string) {
	tb.Helper()
	r.git(tb, r.bare, "update-ref", name, sha)
}

// DeleteRef removes any reference, such as refs/heads/dev, from the bare repository.
func (r *Repo) DeleteRef(tb testing.TB, name string) {
	tb.Helper()
	r.git(tb, r.bare, "update-ref", "-d", name)
}

// git runs the git command line in dir and returns its standard output. A failure fails the test
// with git's own message.
func (r *Repo) git(tb testing.TB, dir string, args ...string) string {
	tb.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the arguments are a test's own constants
	cmd.Dir = dir
	cmd.Env = r.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		tb.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}
