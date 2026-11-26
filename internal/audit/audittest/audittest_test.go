// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package audittest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

const testSecret = "s3cr3t-value"

// recorder is a testing.TB that keeps the failures AssertNoSecrets reports instead of failing the test. Only Helper
// and Errorf are overridden: the helper calls nothing else, and any other call panics on the nil embedded TB.
type recorder struct {
	testing.TB
	msgs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func row(id string) audit.Row {
	return audit.Row{
		ID: id, Action: audit.ActionIngressClaimed, TargetType: "stack", TargetID: "web", Reason: "claimed",
		Details: map[string]string{"stack": "web", "hostnames": "a.example.com", "count": "1"},
	}
}

func TestAssertNoSecretsCatchesEachLeak(t *testing.T) {
	cases := []struct {
		name string
		edit func(*audit.Row)
	}{
		{"detail key names a token", func(r *audit.Row) { r.Details["dnsToken"] = "x" }},
		{"detail value holds the secret", func(r *audit.Row) { r.Details["hostnames"] = "a.example.com " + testSecret }},
		{"reason holds the secret", func(r *audit.Row) { r.Reason = "failed with " + testSecret }},
		{"target id holds the secret", func(r *audit.Row) { r.TargetID = testSecret }},
		{"detail value holds a PEM block", func(r *audit.Row) { r.Details["stack"] = "-----BEGIN PRIVATE KEY-----" }},
		{"empty value under a secret key name", func(r *audit.Row) { r.Details["token"] = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := row("leaky")
			tc.edit(&r)
			rec := &recorder{}
			audittest.AssertNoSecrets(rec, []audit.Row{r}, testSecret)
			if len(rec.msgs) == 0 {
				t.Fatalf("AssertNoSecrets reported no failure for %q", tc.name)
			}
			for _, m := range rec.msgs {
				if strings.Contains(m, testSecret) {
					t.Errorf("a failure message prints the secret: %q", m)
				}
				if !strings.Contains(m, "leaky") || !strings.Contains(m, audit.ActionIngressClaimed) {
					t.Errorf("a failure message does not name the row and the action: %q", m)
				}
			}
		})
	}

	t.Run("a clean row reports nothing", func(t *testing.T) {
		rec := &recorder{}
		audittest.AssertNoSecrets(rec, []audit.Row{row("clean")}, testSecret)
		if len(rec.msgs) != 0 {
			t.Fatalf("a clean row failed: %v", rec.msgs)
		}
	})
}

func TestRowsReadsOneAction(t *testing.T) {
	s := storetest.New(t)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	v, err := vault.LoadOrCreate(key, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	w := audit.NewWriter(s, v, slog.New(slog.DiscardHandler), func() time.Time { return now })
	rec := func(action, env string) {
		t.Helper()
		err := w.Record(context.Background(), audit.Entry{
			Actor:  audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "alice"},
			Action: action, TargetType: "stack", TargetID: "web", EnvID: env, Result: audit.ResultSuccess,
			Details: map[string]string{"stack": "web"},
		})
		if err != nil {
			t.Fatalf("Record %s: %v", action, err)
		}
	}
	rec(audit.ActionIngressClaimed, "env-1")
	rec(audit.ActionIngressVIPMoved, "env-1")
	rec(audit.ActionIngressClaimed, "env-2")

	got := audittest.Rows(t, s, audit.ActionIngressClaimed)
	if len(got) != 2 {
		t.Fatalf("Rows returned %d rows, want the 2 claimed rows", len(got))
	}
	for _, r := range got {
		if r.Action != audit.ActionIngressClaimed {
			t.Errorf("Rows returned a row of action %q", r.Action)
		}
	}
	if n := len(audittest.Rows(t, s, audit.ActionIngressVIPMoved)); n != 1 {
		t.Errorf("Rows(vip_moved) = %d rows, want 1", n)
	}
	if n := len(audittest.Rows(t, s, audit.ActionIngressDisabled)); n != 0 {
		t.Errorf("Rows of an action never recorded = %d rows, want 0", n)
	}
}

// The helper keeps its own copy of the Writer's secret words so a proof test reads no unexported state; this pins the
// two lists equal by reading the literal out of audit.go.
func TestSecretWordsMatchTheWriter(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../audit.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var writer []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "secretWords" || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, e := range lit.Elts {
			if bl, ok := e.(*ast.BasicLit); ok {
				s, err := strconv.Unquote(bl.Value)
				if err != nil {
					t.Fatal(err)
				}
				writer = append(writer, s)
			}
		}
		return false
	})
	if len(writer) == 0 {
		t.Fatal("did not find the secretWords literal in audit.go")
	}
	if got := audittest.SecretWords(); !slices.Equal(got, writer) {
		t.Fatalf("audittest secret words = %v, the Writer's = %v", got, writer)
	}
}
