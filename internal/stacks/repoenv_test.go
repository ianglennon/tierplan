// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// The committed .env of a stack made from a git repository (D-07, GIT-08): it supplies defaults below
// the variables table and above the default written in the compose file, a secret-looking key in it is
// flagged and its value is masked, and an inline stack's .env supplies nothing.

const repoEnvCompose = `services:
  web:
    image: nginx:1.27-alpine
    environment:
      GREETING: ${GREETING:-hello}
      PORT: ${PORT}
      DB_PASSWORD: ${DB_PASSWORD}
`

// repoCopy is a working copy of repoEnvCompose whose .env has the given origin.
func repoCopy(origin stacks.FileOrigin, dotenv string, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: stacks.ComposeFile, Content: []byte(repoEnvCompose), Origin: stacks.OriginRepo},
			{Path: stacks.DotEnvFile, Content: []byte(dotenv), Origin: origin},
		},
		Vars: vars,
	}
}

func resolvedNamed(t *testing.T, rows []stacks.ResolvedVar, name string) stacks.ResolvedVar {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", name, rows)
	return stacks.ResolvedVar{}
}

func TestRepoEnvPrecedence(t *testing.T) {
	t.Parallel()
	const dotenv = "GREETING=from-env\nPORT=8080\n"
	c := repoCopy(stacks.OriginRepo, dotenv, stacks.Var{Name: "PORT", Value: "9090"})
	env, err := stacks.RepoEnv(c)
	if err != nil {
		t.Fatalf("RepoEnv: %v", err)
	}
	rows := stacks.ResolveVarsLayered(stacks.Refs(c), c.Vars, env)

	// The table beats the .env.
	if r := resolvedNamed(t, rows, "PORT"); r.Value != "9090" || r.Source != stacks.SourceTable {
		t.Errorf("PORT = %q from %q, want 9090 from table", r.Value, r.Source)
	}
	// The .env beats the default written in the compose file.
	if r := resolvedNamed(t, rows, "GREETING"); r.Value != "from-env" || r.Source != stacks.SourceRepoEnv {
		t.Errorf("GREETING = %q from %q, want from-env from repo-env", r.Value, r.Source)
	}
	if stacks.SourceRepoEnv != "repo-env" {
		t.Errorf("SourceRepoEnv = %q, want repo-env", stacks.SourceRepoEnv)
	}

	// With no .env value the default is used.
	c2 := repoCopy(stacks.OriginRepo, "PORT=8080\n")
	env2, _ := stacks.RepoEnv(c2)
	if r := resolvedNamed(t, stacks.ResolveVarsLayered(stacks.Refs(c2), c2.Vars, env2), "GREETING"); r.Value != "hello" || r.Source != stacks.SourceDefault {
		t.Errorf("GREETING = %q from %q, want hello from default", r.Value, r.Source)
	}

	// The load interpolates with the same layers.
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "p", Target: stacks.TargetSwarm, Copy: c})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := loaded.Project.Services["web"].Environment
	if v := got["PORT"]; v == nil || *v != "9090" {
		t.Errorf("loaded PORT = %v, want 9090 (the table beats the .env)", v)
	}
	if v := got["GREETING"]; v == nil || *v != "from-env" {
		t.Errorf("loaded GREETING = %v, want from-env (the .env beats the default)", v)
	}
}

func TestRepoEnvSecretLikeMasked(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-committed-value"
	c := repoCopy(stacks.OriginRepo, "DB_PASSWORD="+secret+"\nPORT=8080\n")
	env, err := stacks.RepoEnv(c)
	if err != nil {
		t.Fatalf("RepoEnv: %v", err)
	}
	rows := stacks.ResolveVarsLayered(stacks.Refs(c), c.Vars, env)
	r := resolvedNamed(t, rows, "DB_PASSWORD")
	if r.Source != stacks.SourceRepoEnv || !r.SecretLike || r.Value != stacks.Hidden {
		t.Errorf("DB_PASSWORD = %+v, want repo-env, SecretLike and Hidden", r)
	}
	if p := resolvedNamed(t, rows, "PORT"); p.SecretLike || p.Value != "8080" {
		t.Errorf("PORT = %+v, want plain 8080", p)
	}

	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "p", Target: stacks.TargetSwarm, Copy: c})
	var warn *stacks.Problem
	for i, p := range res.Problems {
		if p.Code == stacks.CodeEnvSecretLike {
			warn = &res.Problems[i]
		}
	}
	if warn == nil {
		t.Fatalf("no env_secret_like warning in %+v", res.Problems)
	}
	const want = "DB_PASSWORD looks like a secret and is committed in the repository's .env. Set a secret variable here to override it."
	if warn.Message != want || warn.Severity != stacks.SeverityWarning || warn.File != stacks.DotEnvFile || warn.Line != 1 {
		t.Errorf("warning = %+v, want the UI-SPEC wording as a warning on .env line 1", *warn)
	}

	// The value is in no part of the answer.
	m := stacks.NewMasker(c.Vars, c.MaskFiles())
	if got := m.Mask("failed: " + secret + " rejected"); strings.Contains(got, secret) {
		t.Errorf("masker leaked the committed secret: %q", got)
	}
	for _, p := range res.Problems {
		if strings.Contains(p.Message, secret) || strings.Contains(p.Key, secret) {
			t.Errorf("problem leaked the committed secret: %+v", p)
		}
	}
	for _, v := range res.Variables {
		if strings.Contains(v.Value, secret) || strings.Contains(v.Default, secret) {
			t.Errorf("variable row leaked the committed secret: %+v", v)
		}
	}
}

func TestRepoEnvSecretLikeWarningStopsWhenTheTableOverrides(t *testing.T) {
	t.Parallel()
	c := repoCopy(stacks.OriginRepo, "DB_PASSWORD=committed-one\n", stacks.Var{Name: "DB_PASSWORD", Value: "real-secret-1", Secret: true})
	for _, p := range stacks.GitLint(c, nil) {
		if p.Code == stacks.CodeEnvSecretLike {
			t.Errorf("warned about a key the table already overrides: %+v", p)
		}
	}
}

func TestSecretLikeName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"PASSWORD": true, "DB_PASSWORD": true, "PASSWD": true, "ROOT_PWD": true, "APP_SECRET": true, "SECRETS_DIR": true,
		"GITHUB_TOKEN": true, "API_KEY": true, "APIKEY": true, "KEY": true, "AWS_CREDENTIALS": true, "PRIVATE_URL": true,
		"db_password":  true,
		"KEYCLOAK_URL": false, "MONKEY": false, "PORT": false, "PASSWORDLESS": false, "TOKENS": false, "": false,
	} {
		if got := stacks.SecretLikeName(name); got != want {
			t.Errorf("SecretLikeName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestInlineDotEnvIgnored(t *testing.T) {
	t.Parallel()
	for _, origin := range []stacks.FileOrigin{stacks.OriginInline, stacks.OriginOverlay} {
		c := repoCopy(origin, "GREETING=from-env\nPORT=8080\n")
		c.Files[0].Origin = stacks.OriginInline
		env, err := stacks.RepoEnv(c)
		if err != nil || env != nil {
			t.Fatalf("origin %q: RepoEnv = %v, %v, want nothing", origin, env, err)
		}
		rows := stacks.ResolveVars(stacks.Refs(c), c.Vars)
		if r := resolvedNamed(t, rows, "GREETING"); r.Source != stacks.SourceDefault || r.Value != "hello" {
			t.Errorf("origin %q: GREETING = %q from %q, want the default", origin, r.Value, r.Source)
		}
		if r := resolvedNamed(t, rows, "PORT"); !r.Required {
			t.Errorf("origin %q: PORT = %+v, want Required: an .env that is not the repository's supplies nothing", origin, r)
		}
	}
}

func TestRequiredWithoutAnySource(t *testing.T) {
	t.Parallel()
	c := repoCopy(stacks.OriginRepo, "GREETING=from-env\n")
	env, err := stacks.RepoEnv(c)
	if err != nil {
		t.Fatalf("RepoEnv: %v", err)
	}
	rows := stacks.ResolveVarsLayered(stacks.Refs(c), c.Vars, env)
	r := resolvedNamed(t, rows, "PORT")
	if !r.Required || r.Source != stacks.SourceUnset || r.Value != "" {
		t.Errorf("PORT = %+v, want Required and unset: no table value, no .env value and no default (GIT-08)", r)
	}
	// Check refuses the deploy for it, so the create page can prompt for the value.
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "p", Target: stacks.TargetSwarm, Copy: c})
	found := false
	for _, p := range res.Problems {
		if p.Code == stacks.CodeUnsetVariable && strings.HasPrefix(p.Message, "PORT ") {
			found = true
		}
	}
	if !found {
		t.Errorf("no unset_variable problem for PORT in %+v", res.Problems)
	}
}

func TestRepoEnvUnreadableIsBlockingOnTheFile(t *testing.T) {
	t.Parallel()
	c := repoCopy(stacks.OriginRepo, "GOOD=1\nBAD=\"never closed\n")
	if _, err := stacks.RepoEnv(c); err == nil {
		t.Fatal("RepoEnv accepted an unterminated quote")
	}
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "p", Target: stacks.TargetSwarm, Copy: c})
	for _, p := range res.Problems {
		if p.Code == stacks.CodeEnvFileInvalid && p.File == stacks.DotEnvFile && p.Severity == stacks.SeverityError {
			if strings.Contains(p.Message, "never closed") {
				t.Errorf("problem quotes the file: %q", p.Message)
			}
			return
		}
	}
	t.Errorf("no blocking env_file_invalid on .env in %+v", res.Problems)
}

func TestRepoEnvIsNotReportedAsUnread(t *testing.T) {
	t.Parallel()
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "p", Target: stacks.TargetSwarm, Copy: repoCopy(stacks.OriginRepo, "PORT=8080\nDB_PASSWORD=pw-pw-pw\n")})
	for _, p := range res.Problems {
		if p.Code == stacks.CodeDotenvNotRead {
			t.Errorf("a .env the repository supplies was reported as not read: %+v", p)
		}
	}
	for _, u := range res.UnusedFiles {
		if u == stacks.DotEnvFile {
			t.Errorf("the repository .env is listed as unused: %v", res.UnusedFiles)
		}
	}
}

// A secret or config that reads `environment: NAME` is resolved from the table over the repository's .env, as
// the compose files are interpolated, so a check, a review and a deploy agree (WR-C04).
func TestObjectVarsLayersTheRepoEnvUnderTheTable(t *testing.T) {
	t.Parallel()
	const dotenv = "FROM_ENV=env-value\nBOTH=env-both\nBLANK_BOTH=\nBLANK_TABLE=env-fills\n"
	vars := []stacks.Var{
		{Name: "BOTH", Value: "table-both"}, {Name: "BLANK_BOTH"}, {Name: "BLANK_TABLE"}, {Name: "TABLE_ONLY", Value: "t"},
	}
	got := stacks.ObjectVars(repoCopy(stacks.OriginRepo, dotenv, vars...))
	want := map[string]string{
		"FROM_ENV": "env-value", "BOTH": "table-both", "BLANK_BOTH": "", "BLANK_TABLE": "env-fills", "TABLE_ONLY": "t",
	}
	if len(got) != len(want) {
		t.Fatalf("ObjectVars = %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("ObjectVars[%s] = %q (%v), want %q", k, g, ok, v)
		}
	}

	// An inline or overlay .env supplies nothing.
	if got := stacks.ObjectVars(repoCopy(stacks.OriginInline, dotenv)); len(got) != 0 {
		t.Errorf("an inline .env supplied %v", got)
	}
}
