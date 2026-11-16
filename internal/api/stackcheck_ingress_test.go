// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// The editor's check and the review read the environment's ingress world (plan 18-11, D-19, D-27): a
// hostname outside the environment's domains, a reserved name and a name another stack holds each come back
// as a problem with a stable code, and what the message says of another environment follows the caller's
// access (D-20).

const (
	ingressOtherEnvID     = "env-ingress-b"
	ingressOtherStackName = "web-in-b"
	// ingressDomain is registered in both environments (D-04).
	ingressDomain   = "apps.example.com"
	ingressOutside  = "app.elsewhere.org"
	ingressReserved = "reserved.apps.example.com"
	ingressTaken    = "taken.apps.example.com"
)

// ingressWorldSeed is what seedIngressWorld wrote: the second environment, its stack and the domain of the
// first environment. The deploy-gate tests of plan 18-13 seed through the same helper.
type ingressWorldSeed struct {
	otherEnvID   string
	otherEnvName string
	otherStackID string
	otherStack   string
	domainID     string
	reservedHost string
	takenHost    string
	outsideHost  string
}

// seedIngressWorld gives the environment e the domain apps.example.com and one reserved name under it,
// and makes a second environment that registers the same suffix and whose stack claims taken.apps.example.com.
func seedIngressWorld(t *testing.T, f *envFix, e envBody) ingressWorldSeed {
	t.Helper()
	ctx := context.Background()
	s := f.stackDeps.Ingress
	if s == nil {
		t.Fatal("the fixture has no ingress store")
	}
	dom, err := s.CreateDomain(ctx, ingressstore.CreateDomainInput{EnvID: e.ID, Suffix: ingressDomain, Issuer: ingress.IssuerStaging, Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("register the domain in the first environment: %v", err)
	}
	if _, _, err := s.Reserve(ctx, ingressstore.ReserveInput{EnvID: e.ID, DomainID: dom.ID, Hostname: ingressReserved, Actor: "admin"}, nil); err != nil {
		t.Fatalf("reserve %s: %v", ingressReserved, err)
	}
	f.addEnvironmentRow(t, ingressOtherEnvID)
	if _, err := f.st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'swarm', 'tester', 1)`,
		"stack-in-b", ingressOtherEnvID, ingressOtherStackName); err != nil {
		t.Fatalf("seed the stack of the second environment: %v", err)
	}
	if _, err := s.CreateDomain(ctx, ingressstore.CreateDomainInput{EnvID: ingressOtherEnvID, Suffix: ingressDomain, Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); err != nil {
		t.Fatalf("register the domain in the second environment: %v", err)
	}
	decl := ingress.Declaration{Service: "web", Host: ingressTaken, Label: "tierplan.ingress.host", Port: 80}
	if _, err := s.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: ingressOtherEnvID, StackID: "stack-in-b", Actor: "bob", Decls: []ingress.Declaration{decl}}); err != nil {
		t.Fatalf("claim %s in the second environment: %v", ingressTaken, err)
	}
	return ingressWorldSeed{
		otherEnvID: ingressOtherEnvID, otherEnvName: "env " + ingressOtherEnvID, otherStackID: "stack-in-b", otherStack: ingressOtherStackName,
		domainID: dom.ID, reservedHost: ingressReserved, takenHost: ingressTaken, outsideHost: ingressOutside,
	}
}

// ingressCompose is a swarm compose file with one service per hostname, each declaring its host on the
// service's deploy labels.
func ingressCompose(hosts ...string) string {
	var b strings.Builder
	b.WriteString("services:\n")
	for i, h := range hosts {
		name := "web" + string(rune('a'+i))
		b.WriteString("  " + name + ":\n    image: nginx:1.27\n    deploy:\n      labels:\n")
		b.WriteString("        tierplan.ingress.host: " + h + "\n        tierplan.ingress.port: \"80\"\n")
	}
	return b.String()
}

// ingressAnswers returns the problems of an editor check by code.
func ingressAnswers(t *testing.T, c *roleCaller, stackID, compose string) map[string]checkProblem {
	t.Helper()
	answer, code, raw := c.check(stackID, checkBody(compose))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s, want 200", code, raw)
	}
	byCode := map[string]checkProblem{}
	for _, p := range answer.Problems {
		byCode[p.Code] = p
	}
	return byCode
}

func TestStackCheckIngressNamesThePolicyProblems(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "ingress-check")
	compose := ingressCompose(seed.outsideHost, seed.reservedHost, seed.takenHost)

	got := ingressAnswers(t, admin, st.ID, compose)
	for _, code := range []string{ingress.CodeDomainNotAllowed, ingress.CodeHostReserved, ingress.CodeHostTaken} {
		p, ok := got[code]
		if !ok {
			t.Fatalf("problems = %+v, want %s", got, code)
		}
		if p.Severity != "error" || p.File != "compose.yaml" || p.Line == 0 {
			t.Errorf("%s = %+v, want an error positioned in compose.yaml", code, p)
		}
	}
	if len(got) != 3 {
		t.Errorf("problems = %+v, want exactly the three policy problems", got)
	}
}

func TestStackCheckIngressAcceptsAHostnameUnderADomain(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "ingress-ok")

	if got := ingressAnswers(t, admin, st.ID, ingressCompose("free.apps.example.com")); len(got) != 0 {
		t.Errorf("problems = %+v, want none for a free name under a registered domain", got)
	}
}

func TestStackCheckIngressRedactsAnotherEnvironmentByAccess(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	envAdmin := f.caller(t, e, "envadmin-1", "admin")
	instanceAdmin := &roleCaller{f: f, t: t, e: e, cookie: f.cookie, csrf: f.csrf}
	st := newStack(t, envAdmin, "ingress-redact")
	compose := ingressCompose(seed.takenHost)

	// An instance admin may see every environment: the stack and its environment are named.
	seen := ingressAnswers(t, instanceAdmin, st.ID, compose)[ingress.CodeHostTaken]
	if !strings.Contains(seen.Message, seed.otherStack) || !strings.Contains(seen.Message, seed.otherEnvName) {
		t.Errorf("message for an instance admin = %q, want the stack %q and the environment %q named", seen.Message, seed.otherStack, seed.otherEnvName)
	}

	// An Admin of this environment only has no role on the other one: the message names neither.
	hidden := ingressAnswers(t, envAdmin, st.ID, compose)[ingress.CodeHostTaken]
	if hidden.Code == "" {
		t.Fatal("the Admin of one environment got no ingress_host_taken problem")
	}
	for _, leak := range []string{seed.otherStack, seed.otherEnvName, seed.otherEnvID, seed.otherStackID} {
		if strings.Contains(hidden.Message, leak) {
			t.Errorf("message for an Admin of the first environment only = %q, must not name %q", hidden.Message, leak)
		}
	}
	if !strings.Contains(hidden.Message, "an environment you can't see") {
		t.Errorf("message = %q, want the redacted sentence", hidden.Message)
	}

	// A role on the other environment makes it visible: the same caller, read again per request, now sees the names.
	f.addEnvRole(t, "envadmin-1", seed.otherEnvID, "viewer")
	granted := ingressAnswers(t, envAdmin, st.ID, compose)[ingress.CodeHostTaken]
	if !strings.Contains(granted.Message, seed.otherStack) || !strings.Contains(granted.Message, seed.otherEnvName) {
		t.Errorf("message after a viewer role on the other environment = %q, want the stack and the environment named", granted.Message)
	}
}

// The review a person reads before deploying carries the same policy (D-27): a draft that names a reserved
// host lists ingress_host_reserved among its problems, which block the deploy.
func TestStackReviewIngress(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "ingress-review")
	saveCompose(t, admin, st.ID, ingressCompose(seed.reservedHost), "[]")

	rv, code, raw := admin.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s, want 200", code, raw)
	}
	found := false
	for _, p := range rv.Problems {
		if p.Code == ingress.CodeHostReserved {
			found = true
			if p.Severity != "error" || p.File != "compose.yaml" {
				t.Errorf("problem = %+v, want an error in compose.yaml", p)
			}
		}
	}
	if !found {
		t.Errorf("review problems = %+v, want %s", rv.Problems, ingress.CodeHostReserved)
	}
}
