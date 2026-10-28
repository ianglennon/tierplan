// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package gittest builds real git repositories and serves them over smart HTTP, for tests that
// need a git server without the network. It uses the git command line and git-http-backend, and
// fails the test, never skips it, when either is missing: a runner without git would otherwise
// pass the git tests by not running them (scripts/ci-preflight.sh checks the same).
package gittest

import (
	"bytes"
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
	bare string
	work string
	env  []string
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
			"GIT_AUTHOR_NAME=Bayplan Test", "GIT_AUTHOR_EMAIL=test@bayplan.invalid",
			"GIT_COMMITTER_NAME=Bayplan Test", "GIT_COMMITTER_EMAIL=test@bayplan.invalid",
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
	r.git(tb, r.work, "add", "-A")
	r.git(tb, r.work, "commit", "--allow-empty", "-m", msg)
	sha := strings.TrimSpace(r.git(tb, r.work, "rev-parse", "HEAD"))
	r.git(tb, r.work, "push", "--force", "origin", "HEAD:refs/heads/main")
	return sha
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
