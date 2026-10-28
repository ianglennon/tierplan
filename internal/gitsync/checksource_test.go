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
