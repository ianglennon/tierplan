// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/wire"
)

// These tests tie docs/ingress.md to the codes Phase 28 writes: every code a person can meet from TierPlan's own route,
// the self network and the HTTP-01 carve-out has a row in the "## Stable codes" table, read from the Go constants so that a
// new or renamed code fails here until the docs say what it means. guideSection is defined in docs_https_test.go.

// ingressGuideText reads docs/ingress.md. The tests of this package run in internal/server.
func ingressGuideText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/ingress.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// unexportedConst reads the value of the string constant name declared in ../<pkgDir>/<file>. The api package keeps its
// problem and field codes unexported, so the test reads them from the source rather than copying their values.
func unexportedConst(t *testing.T, file, name string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", file), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: %s is not a string literal constant", file, name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
		}
	}
	t.Fatalf("%s declares no constant %s", file, name)
	return ""
}

func TestIngressDocsListEveryStableCode(t *testing.T) {
	section := guideSection(ingressGuideText(t), "Stable codes")
	if section == "" {
		t.Fatal(`docs/ingress.md has no "## Stable codes" section`)
	}
	codes := []struct{ name, value string }{
		// TierPlan's own route: where it may be switched on, and what refuses it.
		{"ingressdeploy.CodeSelfRouteNotHere", ingressdeploy.CodeSelfRouteNotHere},
		{"ingressdeploy.CodeSelfRouteNeedsService", ingressdeploy.CodeSelfRouteNeedsService},
		{"ingressdeploy.CodeSelfRouteBasePath", ingressdeploy.CodeSelfRouteBasePath},
		{"ingressdeploy.CodeSelfRouteNoURL", ingressdeploy.CodeSelfRouteNoURL},
		{"ingressdeploy.CodeSelfRouteAlreadyOn", ingressdeploy.CodeSelfRouteAlreadyOn},
		{"ingressdeploy.CodeSelfCertificateTerms", ingressdeploy.CodeSelfCertificateTerms},
		{"ingressdeploy.CodeSelfCertificatePaused", ingressdeploy.CodeSelfCertificatePaused},
		{"ingressdeploy.CodeSelfCertificatePending", ingressdeploy.CodeSelfCertificatePending},
		{"ingress.CodeHostIsAgentURL", ingress.CodeHostIsAgentURL},
		// The switch-on and switch-off jobs.
		{"ingressdeploy.CodeSelfRouteCheckFailed", ingressdeploy.CodeSelfRouteCheckFailed},
		{"ingressdeploy.CodeSelfRouteClaimFailed", ingressdeploy.CodeSelfRouteClaimFailed},
		{"ingressdeploy.CodeSelfRouteHeld", ingressdeploy.CodeSelfRouteHeld},
		{"ingressdeploy.CodeSelfRouteReleaseFailed", ingressdeploy.CodeSelfRouteReleaseFailed},
		{"ingressdeploy.CodeDisableSelfNetworkFailed", ingressdeploy.CodeDisableSelfNetworkFailed},
		{"ingressdeploy.ReasonTierPlanAttached", ingressdeploy.ReasonTierPlanAttached},
		// The route's state and the self network.
		{"ingressdeploy.CodeSelfNotAttached", ingressdeploy.CodeSelfNotAttached},
		{"ingressrender.SkipSelfNotAttached", string(ingressrender.SkipSelfNotAttached)},
		{"stackops.ReasonSelfNotAttached", stackops.ReasonSelfNotAttached},
		{"stackops.ReasonNotObserved", stackops.ReasonNotObserved},
		{"ingressdeploy.CodeSelfNetworkForeign", ingressdeploy.CodeSelfNetworkForeign},
		{"ingressrender.CodeSelfInput", ingressrender.CodeSelfInput},
		// Agents and proxies never through ingress: the handshake refusals.
		{"wire.ReasonSelfNetworkPeer", wire.ReasonSelfNetworkPeer},
		{"ingresswire.RefuseSelfNetworkPeer", ingresswire.RefuseSelfNetworkPeer},
		// TierPlan's own certificate and HTTP-01.
		{"ingressdeploy.CodeHTTP01Conflict", ingressdeploy.CodeHTTP01Conflict},
		{"ingressdeploy.CodeHTTP01Unknown", ingressdeploy.CodeHTTP01Unknown},
		{"ingressdeploy.CodeHTTP01CarveOut", ingressdeploy.CodeHTTP01CarveOut},
		// The api package's codes are unexported: read from internal/api/ingress_self.go and internal/api/settings.go.
		{"api.problemInstanceAdminRequired", unexportedConst(t, "api/ingress_self.go", "problemInstanceAdminRequired")},
		{"api.problemSelfRouteNotOn", unexportedConst(t, "api/ingress_self.go", "problemSelfRouteNotOn")},
		{"api.codeAgentURLIsIngressHost", unexportedConst(t, "api/settings.go", "codeAgentURLIsIngressHost")},
		{"api.codePublicURLIsIngressHost", unexportedConst(t, "api/settings.go", "codePublicURLIsIngressHost")},
	}
	for _, c := range codes {
		if c.value == "" {
			t.Errorf("%s is empty", c.name)
			continue
		}
		if !strings.Contains(section, "\n| `"+c.value+"` |") {
			t.Errorf("the Stable codes table of docs/ingress.md has no row for %s (`%s`)", c.name, c.value)
		}
	}
}

// TestIngressDocsSelfRouteMatchesTheUI ties the "## TierPlan's own route" section to the text the card shows (D-03): its
// stack-file lines are the deployer's snippet, the same as docs/install-in-swarm.md's, and it gives the service update
// command. The heading is what install-in-swarm.md and reverse-proxy.md link to as #tierplans-own-route.
func TestIngressDocsSelfRouteMatchesTheUI(t *testing.T) {
	section := guideSection(ingressGuideText(t), "TierPlan's own route")
	if section == "" {
		t.Fatal(`docs/ingress.md has no "## TierPlan's own route" section, which other pages link to as #tierplans-own-route`)
	}
	if lines, want := firstYAMLBlock(t, section)+"\n", ingressdeploy.StackFileSnippet("tierplan"); lines != want {
		t.Errorf("the section's stack-file lines are\n%s\nthe UI shows\n%s", lines, want)
	}
	if cmd := ingressdeploy.ServiceUpdateCommand("tierplan"); !strings.Contains(section, cmd) {
		t.Errorf("the section does not give the service update command %q", cmd)
	}
}

// TestIngressDocsHTTP01QuotesTheRefusal ties the rewritten HTTP-01 section (D-12) to the code: it quotes the swarm refusal as
// preflight.go writes it, and names the carve-out's path and network.
func TestIngressDocsHTTP01QuotesTheRefusal(t *testing.T) {
	section := guideSection(ingressGuideText(t), "TierPlan's own certificate and HTTP-01")
	if section == "" {
		t.Fatal(`docs/ingress.md has no "## TierPlan's own certificate and HTTP-01" section, which https.md links to`)
	}
	// The quote is a block quote over several lines: drop the markers and join the lines before comparing.
	var flat []string
	for _, line := range strings.Split(section, "\n") {
		flat = append(flat, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">")))
	}
	text := strings.Join(flat, " ")
	for _, want := range []string{
		unexportedConst(t, "ingressdeploy/preflight.go", "http01SwarmConflict"),
		"/.well-known/acme-challenge/",
		ingress.SelfNetworkName,
		"`" + ingressdeploy.CodeHTTP01Conflict + "`",
		"`" + ingressdeploy.CodeHTTP01CarveOut + "`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the HTTP-01 section does not say %q", want)
		}
	}
}

// TestIngressDocsHaveNoCAHold keeps two stale texts out of every page under docs/: the reason code `ca_hold`, which the code
// never wrote (it writes ca_rate_limited), and the promise "A later version lifts this limit", which Phase 28 kept.
func TestIngressDocsHaveNoCAHold(t *testing.T) {
	banned := []string{"ca_hold", "A later version lifts this limit"}
	pages := 0
	docs := os.DirFS("../../docs")
	err := fs.WalkDir(docs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(docs, path)
		if err != nil {
			return err
		}
		pages++
		for _, s := range banned {
			if strings.Contains(string(b), s) {
				t.Errorf("docs/%s still says %q", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 {
		t.Fatal("no page found under docs/")
	}
}
