// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// The rules a stack made from a git repository adds (D-09, D-10): an overlay may fill a path the
// repository lacks and never shadow one, a file the compose files need must exist, and the files git
// cannot hand over whole (LFS pointers, submodules, links out of the folder) are refused by name.

const gitEnvFileCompose = `services:
  web:
    image: nginx:1.27-alpine
    env_file: ./app.env
`

func repoFile(path, content string) stacks.File {
	return stacks.File{Path: path, Content: []byte(content), Origin: stacks.OriginRepo}
}

func overlayFile(path, content string) stacks.File {
	return stacks.File{Path: path, Content: []byte(content), Origin: stacks.OriginOverlay}
}

// gitCopy is a working copy of a repository whose one compose file is compose.yaml.
func gitCopy(compose string, files ...stacks.File) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files:        append([]stacks.File{repoFile(stacks.ComposeFile, compose)}, files...),
		ComposeFiles: []string{stacks.ComposeFile},
	}
}

func problemsWithCode(ps []stacks.Problem, code string) []stacks.Problem {
	var out []stacks.Problem
	for _, p := range ps {
		if p.Code == code {
			out = append(out, p)
		}
	}
	return out
}

func TestGitLintOverlayClash(t *testing.T) {
	t.Parallel()
	c := gitCopy(gitEnvFileCompose, repoFile("app.env", "A=1\n"), overlayFile("app.env", "A=2\n"), overlayFile("other.txt", "x"))
	got := problemsWithCode(stacks.GitLint(c, nil), stacks.CodeOverlayClash)
	if len(got) != 1 {
		t.Fatalf("overlay_clash problems = %+v, want exactly one (for app.env)", got)
	}
	p := got[0]
	const want = "app.env exists in the repository and as an overlay file. Remove the overlay file or rename it."
	if p.Severity != stacks.SeverityError || p.Message != want || p.File != "app.env" {
		t.Errorf("problem = %+v, want a blocking error on app.env saying %q", p, want)
	}
}

func TestGitLintRepoFileMissing(t *testing.T) {
	t.Parallel()
	got := problemsWithCode(stacks.GitLint(gitCopy(gitEnvFileCompose), nil), stacks.CodeRepoFileMissing)
	if len(got) != 1 {
		t.Fatalf("repo_file_missing problems = %+v, want exactly one (for app.env)", got)
	}
	p := got[0]
	if p.Severity != stacks.SeverityError || p.Message != "app.env isn't in the repository." || p.Action != stacks.ActionAddOverlay {
		t.Errorf("problem = %+v, want a blocking error saying \"app.env isn't in the repository.\" with the add_overlay action", p)
	}
	if p.File != stacks.ComposeFile || p.Line != 4 {
		t.Errorf("problem at %s:%d, want the env_file entry at compose.yaml:4", p.File, p.Line)
	}
}

func TestGitLintOverlayFillsAMissingFile(t *testing.T) {
	t.Parallel()
	got := stacks.GitLint(gitCopy(gitEnvFileCompose, overlayFile("app.env", "A=1\n")), nil)
	if bad := problemsWithCode(got, stacks.CodeRepoFileMissing); len(bad) != 0 {
		t.Errorf("an overlay file for the path still reported missing: %+v", bad)
	}
	if bad := problemsWithCode(got, stacks.CodeOverlayClash); len(bad) != 0 {
		t.Errorf("an overlay file with no repository twin reported as a clash: %+v", bad)
	}
}

func TestGitLintMissingSecretAndConfigFiles(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    env_file:
      - path: ./maybe.env
        required: false
secrets:
  db:
    file: ./db_pw.txt
configs:
  nginx:
    file: ./conf/nginx.conf
`
	got := problemsWithCode(stacks.GitLint(gitCopy(compose), nil), stacks.CodeRepoFileMissing)
	var msgs []string
	for _, p := range got {
		msgs = append(msgs, p.Message)
	}
	want := []string{"db_pw.txt isn't in the repository.", "conf/nginx.conf isn't in the repository."} // in file order
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Errorf("messages = %q, want %q (the optional env_file is not reported)", msgs, want)
	}
}

func TestGitLintLFSPointer(t *testing.T) {
	t.Parallel()
	const pointer = "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"
	c := gitCopy(gitEnvFileCompose, repoFile("app.env", pointer), overlayFile("note.txt", pointer))
	got := problemsWithCode(stacks.GitLint(c, nil), stacks.CodeLFSPointer)
	if len(got) != 1 {
		t.Fatalf("git_lfs_pointer problems = %+v, want exactly one (for the repository file, not the overlay)", got)
	}
	if got[0].Severity != stacks.SeverityError || got[0].File != "app.env" ||
		got[0].Message != "app.env is a Git LFS pointer. Bayplan doesn't support Git LFS." {
		t.Errorf("problem = %+v", got[0])
	}
}

func TestIsLFSPointer(t *testing.T) {
	t.Parallel()
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:aa\nsize 1\n"
	for name, tc := range map[string]struct {
		content string
		want    bool
	}{
		"a pointer":               {pointer, true},
		"a pointer without a EOL": {"version https://git-lfs.github.com/spec/v1", true},
		"ordinary text":           {"services:\n  web:\n    image: nginx\n", false},
		"empty":                   {"", false},
		"the version text later":  {"# note\n" + pointer, false},
		"too large to be one":     {pointer + strings.Repeat("x", 1100), false},
	} {
		if got := stacks.IsLFSPointer([]byte(tc.content)); got != tc.want {
			t.Errorf("%s: IsLFSPointer = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSubmoduleProblem(t *testing.T) {
	t.Parallel()
	p := stacks.SubmoduleProblem("vendor/lib")
	if p.Severity != stacks.SeverityError || p.Code != stacks.CodeSubmodule || p.File != "vendor/lib" ||
		p.Message != "vendor/lib is a submodule. Bayplan doesn't support submodules." {
		t.Errorf("SubmoduleProblem = %+v", p)
	}
}

func TestSymlinkOutsideProblem(t *testing.T) {
	t.Parallel()
	p := stacks.SymlinkOutsideProblem("secrets/link")
	if p.Severity != stacks.SeverityError || p.Code != stacks.CodeSymlinkOutside || p.File != "secrets/link" ||
		p.Message != "secrets/link is a link to somewhere outside the stack folder." {
		t.Errorf("SymlinkOutsideProblem = %+v", p)
	}
}

// Check owns the generic file_missing wording for an inline stack. For a stack from a repository the
// same missing file is repo_file_missing, once, with the add-overlay action.
func TestGitLintCheckReportsAMissingRepoFileOnce(t *testing.T) {
	t.Parallel()
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "g", Target: stacks.TargetSwarm, Copy: gitCopy(gitEnvFileCompose)})
	if got := problemsWithCode(res.Problems, stacks.CodeRepoFileMissing); len(got) != 1 || got[0].Action != stacks.ActionAddOverlay {
		t.Errorf("repo_file_missing = %+v, want exactly one with the add_overlay action", got)
	}
	if got := problemsWithCode(res.Problems, stacks.CodeFileMissing); len(got) != 0 {
		t.Errorf("the generic file_missing is also reported for a repository stack: %+v", got)
	}
	if res.Problems.Errors() == 0 {
		t.Error("a missing referenced file does not block")
	}
	// An inline stack is unchanged.
	inline := stacks.Check(context.Background(), stacks.CheckInput{Name: "g", Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{
		Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(gitEnvFileCompose)}},
	}})
	if got := problemsWithCode(inline.Problems, stacks.CodeFileMissing); len(got) == 0 {
		t.Errorf("an inline stack lost its file_missing problem: %+v", inline.Problems)
	}
	// With the overlay in place it passes the rule.
	ok := stacks.Check(context.Background(), stacks.CheckInput{Name: "g", Target: stacks.TargetSwarm, Copy: gitCopy(gitEnvFileCompose, overlayFile("app.env", "A=1\n"))})
	if ok.Problems.Errors() != 0 {
		t.Errorf("a repository stack with its overlay file has errors: %+v", ok.Problems)
	}
}

func TestGitLintCheckReportsAClashAndAPointer(t *testing.T) {
	t.Parallel()
	c := gitCopy(gitEnvFileCompose, repoFile("app.env", "version https://git-lfs.github.com/spec/v1\noid sha256:aa\nsize 1\n"), overlayFile("app.env", "A=2\n"))
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "g", Target: stacks.TargetSwarm, Copy: c})
	if len(problemsWithCode(res.Problems, stacks.CodeOverlayClash)) != 1 || len(problemsWithCode(res.Problems, stacks.CodeLFSPointer)) != 1 {
		t.Errorf("Check problems = %+v, want one overlay_clash and one git_lfs_pointer", res.Problems)
	}
}
