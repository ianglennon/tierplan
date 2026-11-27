// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// TestStackCannotClaimTheAgentURLHost follows the host agents and proxies dial from the instance to the policy: the store
// carries it in the world it reads, ingress.Check refuses a stack that declares it, and the stack store's own check, which
// runs inside the write transaction, refuses it before any claim is stored (D-10, SELF-05).
func TestStackCannotClaimTheAgentURLHost(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	// The host is given in mixed case and twice: the store lower-cases it and keeps it once.
	s := New(Deps{St: st, Vault: v, Now: func() time.Time { return fixedNow },
		AgentHosts: func() []string { return []string{"TierPlan.Example.org", "tierplan.example.org"} }})
	seedEnv(t, st, "e1", "prod")
	seedStack(t, st, "s1", "e1", "web")
	registerDomain(t, s, "e1", "example.org")

	w, err := s.World(ctx, "e1", nil)
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	if want := []string{"tierplan.example.org"}; !slices.Equal(w.AgentHosts, want) {
		t.Fatalf("World.AgentHosts = %v, want %v", w.AgentHosts, want)
	}

	decl := func(host string) ingress.Declaration {
		return ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 8080}
	}
	_, problems := ingress.Check("s1", []ingress.Declaration{decl("tierplan.example.org")}, w)
	if len(problems) != 1 || problems[0].Code != ingress.CodeHostIsAgentURL {
		t.Fatalf("a stack declaring the agent host: problems = %+v, want one %s", problems, ingress.CodeHostIsAgentURL)
	}
	if got := problems[0].Message; !strings.Contains(got, "is the host of your agent URL (or of your public URL, because no agent URL is set).") ||
		!strings.Contains(got, "never through ingress, so ingress can't serve it.") {
		t.Errorf("the refusal does not carry the UI-SPEC sentence: %q", got)
	}
	if plan, problems := ingress.Check("s1", []ingress.Declaration{decl("app.example.org")}, w); len(problems) != 0 || len(plan.Claims) != 1 {
		t.Fatalf("a sibling name under the same domain: plan %+v, problems %+v, want it accepted", plan, problems)
	}

	// The store judges the world again in its write transaction: the refusal holds there and nothing is stored.
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "e1", StackID: "s1", Actor: "alice", Decls: []ingress.Declaration{decl("tierplan.example.org")}})
	var refused *RefusedError
	if !errors.As(err, &refused) || len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeHostIsAgentURL {
		t.Fatalf("ReplaceStackRoutes err = %v, want a RefusedError with %s", err, ingress.CodeHostIsAgentURL)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames`); n != 0 {
		t.Errorf("%d claims stored after a refused plan, want 0", n)
	}
	claim(t, s, "e1", "s1", "app.example.org")
}

// TestWorldWithoutAgentHosts leaves AgentHosts empty when the store was built with no source.
func TestWorldWithoutAgentHosts(t *testing.T) {
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "e1", "prod")
	w, err := s.World(context.Background(), "e1", nil)
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	if len(w.AgentHosts) != 0 {
		t.Fatalf("World.AgentHosts = %v, want none", w.AgentHosts)
	}
}
