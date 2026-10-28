// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Checking a repository folder before a stack exists (plan 09-12, GIT-08, D-09): the services, the problems, the
// variables with where each value comes from, and the files the repository lacks.

const varsCompose = `services:
  web:
    image: nginx:1.27
    environment:
      FROM_TABLE: ${FROM_TABLE}
      FROM_ENV: ${FROM_ENV}
      WITH_DEFAULT: ${WITH_DEFAULT:-dflt}
      NEEDED: ${NEEDED}
      DB_PASSWORD: ${DB_PASSWORD}
  worker:
    image: busybox:1.36
`

func varByName(t *testing.T, vars []stacks.ResolvedVar, name string) stacks.ResolvedVar {
	t.Helper()
	for _, v := range vars {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no variable %s in %+v", name, vars)
	return stacks.ResolvedVar{}
}

func TestCheckSourceVarsAndSources(t *testing.T) {
	t.Parallel()
	const secretInEnv = "hunter2-committed-secret"
	f := newFixture(t, map[string]string{
		"stacks/web/compose.yaml": varsCompose,
		"stacks/web/.env":         "FROM_TABLE=from-env-file\nFROM_ENV=from-the-env\nDB_PASSWORD=" + secretInEnv + "\n",
	}, nil)
	req := gitsync.CheckSourceRequest{ImportRequest: f.request()}
	req.Vars = []stacks.Var{{Name: "FROM_TABLE", Value: "from-table"}}

	got, err := f.svc.CheckSource(context.Background(), req)
	if err != nil {
		t.Fatalf("CheckSource: %v", err)
	}
	if got.Commit.SHA != f.sha || got.Commit.When.IsZero() {
		t.Errorf("commit = %+v, want %s with its time", got.Commit, f.sha)
	}
	if !slices.Equal(got.Services, []string{"web", "worker"}) {
		t.Errorf("services = %v, want web and worker", got.Services)
	}

	for name, want := range map[string]struct {
		source, value string
		required      bool
	}{
		"FROM_TABLE":   {stacks.SourceTable, "from-table", false},
		"FROM_ENV":     {stacks.SourceRepoEnv, "from-the-env", false},
		"WITH_DEFAULT": {stacks.SourceDefault, "dflt", false},
		"NEEDED":       {stacks.SourceUnset, "", true},
	} {
		v := varByName(t, got.Vars, name)
		if v.Source != want.source || v.Value != want.value || v.Required != want.required {
			t.Errorf("%s = %+v, want source %s value %q required %v", name, v, want.source, want.value, want.required)
		}
	}
	if v := varByName(t, got.Vars, "DB_PASSWORD"); v.Source != stacks.SourceRepoEnv || v.Value != stacks.Hidden || !v.SecretLike {
		t.Errorf("DB_PASSWORD = %+v, want a hidden secret-like value from the repository's .env", v)
	}
	var warned, blocked bool
	for _, p := range got.Problems {
		if p.Code == stacks.CodeEnvSecretLike && p.Severity == stacks.SeverityWarning && p.Key == "DB_PASSWORD" {
			warned = true
		}
		if p.Code == stacks.CodeUnsetVariable && p.Severity == stacks.SeverityError && strings.HasPrefix(p.Message, "NEEDED ") {
			blocked = true
		}
	}
	if !warned {
		t.Errorf("problems = %+v, want the warning that DB_PASSWORD is a secret committed in .env", got.Problems)
	}
	if !blocked {
		t.Errorf("problems = %+v, want NEEDED, which has no value, to block", got.Problems)
	}
	if all := fmt.Sprintf("%+v", got); strings.Contains(all, secretInEnv) {
		t.Errorf("the secret from .env is in the check: %s", all)
	}
	if len(got.Missing) != 0 {
		t.Errorf("missing = %v, want none", got.Missing)
	}
}

func TestCheckSourceChecksNothingIntoStorage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	got, err := f.svc.CheckSource(context.Background(), gitsync.CheckSourceRequest{ImportRequest: f.request()})
	if err != nil {
		t.Fatalf("CheckSource: %v", err)
	}
	if !slices.Equal(got.NotImported, []string{"README.md", "docs/a.md"}) || got.NotImportedTotal != 2 {
		t.Errorf("not imported = %v (%d)", got.NotImported, got.NotImportedTotal)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stacks) + (SELECT count(*) FROM stack_snapshots)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a check left %d rows, %v", n, err)
	}
}

func TestCheckSourceNoFetch(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	ctx := context.Background()
	req := gitsync.CheckSourceRequest{ImportRequest: f.request()}

	if _, err := f.svc.CheckSource(ctx, req); err != nil {
		t.Fatalf("the first check: %v", err)
	}
	fetched := f.srv.Requests()
	if fetched == 0 {
		t.Fatal("the first check, which had no commit, asked the server nothing")
	}

	// A commit the mirror holds is read from the mirror: five checks, no request.
	req.Commit = f.sha
	for i := range 5 {
		got, err := f.svc.CheckSource(ctx, req)
		if err != nil {
			t.Fatalf("check %d with the commit: %v", i, err)
		}
		if got.Commit.SHA != f.sha || got.Commit.When.IsZero() {
			t.Errorf("check %d: commit = %+v, want %s with its time", i, got.Commit, f.sha)
		}
	}
	if n := f.srv.Requests(); n != fetched {
		t.Errorf("the server saw %d requests after five checks with a held commit, want still %d", n, fetched)
	}

	// A commit the mirror does not hold is not trusted: the ref is fetched, once.
	req.Commit = strings.Repeat("ab", 20)
	got, err := f.svc.CheckSource(ctx, req)
	if err != nil {
		t.Fatalf("check with an unknown commit: %v", err)
	}
	if got.Commit.SHA != f.sha {
		t.Errorf("commit = %s, want the ref's %s", got.Commit.SHA, f.sha)
	}
	if n := f.srv.Requests(); n != fetched+1 {
		t.Errorf("the server saw %d requests after a check with an unknown commit, want %d", n, fetched+1)
	}

	// Something that is not a full ID is not a commit to hold either.
	req.Commit = f.sha[:7]
	if _, err := f.svc.CheckSource(ctx, req); err != nil {
		t.Fatalf("check with a prefix: %v", err)
	}
	if n := f.srv.Requests(); n != fetched+2 {
		t.Errorf("the server saw %d requests after a check with a prefix, want %d", n, fetched+2)
	}
}

func TestCheckSourceProblems(t *testing.T) {
	t.Parallel()
	const marker = "OUTSIDE-MARKER-31d8"
	f := newFixture(t, map[string]string{
		"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:1.27\n    env_file:\n      - lfs.env\n      - link.env\n      - ../outside.env\n      - missing.env\n      - vendor/app.env\n",
		"stacks/web/lfs.env":      "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a\nsize 12345\n",
		"stacks/secret/key.env":   marker + "=1\n",
	}, func(r *gittest.Repo) {
		r.Symlink(t, "stacks/web/link.env", "../secret/key.env")
		r.Submodule(t, "stacks/web/vendor")
	})
	got, err := f.svc.CheckSource(context.Background(), gitsync.CheckSourceRequest{ImportRequest: f.request()})
	if err != nil {
		t.Fatalf("CheckSource: %v", err)
	}
	if all := fmt.Sprintf("%+v", got); strings.Contains(all, marker) {
		t.Errorf("the content of a file outside the folder is in the check: %s", all)
	}
	var blocking []string
	for _, p := range got.Problems {
		if p.Severity == stacks.SeverityError {
			blocking = append(blocking, p.Code+" "+p.File+" "+p.Message)
		}
	}
	slices.Sort(blocking)
	t.Logf("blocking = %q", blocking)
	want := map[string]string{
		stacks.CodeLFSPointer:      "lfs.env",
		stacks.CodeSymlinkOutside:  "link.env",
		stacks.CodeSubmodule:       "vendor",
		stacks.CodeRepoFileMissing: "missing.env",
	}
	for code, path := range want {
		var ok bool
		for _, p := range got.Problems {
			ok = ok || (p.Code == code && p.Severity == stacks.SeverityError && (p.File == path || strings.Contains(p.Message, path)))
		}
		if !ok {
			t.Errorf("no blocking %s naming %s in %q", code, path, blocking)
		}
	}
	if len(blocking) != 5 {
		t.Errorf("%d blocking problems, want the five of the folder: %q", len(blocking), blocking)
	}
	if !slices.Contains(got.Missing, "missing.env") {
		t.Errorf("missing = %v, want missing.env among them", got.Missing)
	}
}

func TestCheckSourceIncludeUsesTheGitWording(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"stacks/web/compose.yaml": "include:\n  - other.yaml\nservices:\n  web:\n    image: nginx:1.27\n",
		"stacks/web/other.yaml":   "services: {}\n",
	}, nil)
	got, err := f.svc.CheckSource(context.Background(), gitsync.CheckSourceRequest{ImportRequest: f.request()})
	if err != nil {
		t.Fatalf("CheckSource: %v", err)
	}
	var found bool
	for _, p := range got.Problems {
		if p.Code == stacks.CodeIncludeUnsupported {
			found = true
			if p.Severity != stacks.SeverityError || p.Message != "include isn't supported. Add the file to the stack's compose files instead." {
				t.Errorf("include problem = %+v, want the blocking git wording", p)
			}
		}
	}
	if !found {
		t.Errorf("problems = %+v, want include refused", got.Problems)
	}
}
