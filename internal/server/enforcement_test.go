// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/server"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// The two environments every archetype is measured against. Real UUIDs, so a
// route that validates the path parameter does not reject them before the policy
// has spoken.
const (
	envA = "01980000-0000-7000-8000-00000000000a"
	envB = "01980000-0000-7000-8000-00000000000b"
	// missing never exists.
	envMissing = "01980000-0000-7000-8000-0000000000ff"
	// requestOrigin is the origin httptest gives a request and the one the Origin
	// guard expects before a public URL is stored.
	requestOrigin = "http://example.com"
)

type enforcement struct {
	t   *testing.T
	srv *server.Server
	h   http.Handler
	ids map[servertest.Archetype]string
	// seq gives every request an address of its own. The server throttles the
	// audit rows of anonymous refusals per client address (WR-04), so a harness
	// that sent hundreds of them from one address would see later refusals folded
	// into a summary row instead of one row each.
	seq int
}

func newEnforcement(t *testing.T) *enforcement {
	t.Helper()
	fl := pflag.NewFlagSet("enforcement", pflag.ContinueOnError)
	config.BindFlags(fl)
	env := map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}
	cfg, err := config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return &enforcement{
		t: t, srv: srv, h: srv.Handler(),
		ids: servertest.SeedArchetypes(t, srv.Store(), envA, envB),
	}
}

// send calls the handler as the archetype. Every call opens a session of its own
// and puts environments A and B back as they were, so what one call does cannot
// change what the next is expected to see. A request that is expected to be
// allowed may hold a connection open (the event streams), so it gets a short
// deadline; a refusal is immediate.
func (e *enforcement) send(a servertest.Archetype, method, path string, allowed bool) *httptest.ResponseRecorder {
	e.t.Helper()
	servertest.EnsureEnvironments(e.t, e.srv.Store(), envA, envB)
	timeout := 10 * time.Second
	if allowed {
		timeout = 400 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body := ""
	if method != http.MethodGet && method != http.MethodDelete {
		body = "{}"
	}
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "enforcement-test")
	e.seq++
	req.RemoteAddr = fmt.Sprintf("[2001:db8:%x::1]:40000", e.seq)
	if method != http.MethodGet {
		req.Header.Set("Origin", requestOrigin)
	}
	if id := e.ids[a]; id != "" {
		cookie, csrf := servertest.SessionFor(e.t, e.srv, id)
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

type denial struct {
	count             int
	target, reason    string
	envID, actorID    string
	actorType, result string
}

func (e *enforcement) denials() denial {
	e.t.Helper()
	st := e.srv.Store()
	var d denial
	if err := st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action = ?`, audit.ActionPermissionDenied).Scan(&d.count); err != nil {
		e.t.Fatal(err)
	}
	if d.count == 0 {
		return d
	}
	var env *string
	if err := st.Read.QueryRow(`SELECT target_id, reason, env_id, actor_id, actor_type, result FROM audit_log
WHERE action = ? ORDER BY seq DESC LIMIT 1`, audit.ActionPermissionDenied).
		Scan(&d.target, &d.reason, &env, &d.actorID, &d.actorType, &d.result); err != nil {
		e.t.Fatal(err)
	}
	if env != nil {
		d.envID = *env
	}
	return d
}

// kind is the shape of a route's requirement the harness knows how to judge.
type kind int

const (
	kindUnknown kind = iota
	kindSkip         // Public or Exempt: outside the policy
	kindSelf
	kindGlobal
	kindEnv
)

func classify(rec httpx.RouteRecord) (kind, rbac.Permission) {
	switch {
	case rec.Exempt || rec.Req.Public:
		return kindSkip, rbac.Permission{}
	case rec.Req.SelfService && rec.Req.Perm == "":
		return kindSelf, rbac.Permission{}
	}
	reg, ok := rbac.Lookup(rec.Req.Perm)
	if !ok || reg.Scope != rec.Req.Scope {
		return kindUnknown, rbac.Permission{}
	}
	envTree := strings.HasPrefix(rec.Path, "/api/env/{envId}/")
	switch {
	case reg.Scope == rbac.ScopeGlobal && !envTree:
		return kindGlobal, reg
	case reg.Scope == rbac.ScopeEnvironment && envTree:
		return kindEnv, reg
	}
	return kindUnknown, rbac.Permission{}
}

type outcome int

const (
	allowed      outcome = iota // reaches the handler
	unauthorized                // 401
	forbidden                   // 403
	notVisible                  // 404 environment_not_found
)

func (o outcome) String() string {
	return [...]string{"allowed", "401", "403", "404 environment_not_found"}[o]
}

// expect says what a route must answer an archetype on one environment. It
// applies the matrix from the archetype's grant table, written out here
// independently of the Checker it is testing.
func expect(a servertest.Archetype, k kind, reg rbac.Permission, envID string) outcome {
	g := servertest.GrantsOf(a)
	if g.Anonymous || g.Disabled {
		return unauthorized
	}
	switch k {
	case kindSelf:
		return allowed
	case kindGlobal:
		if g.InstanceAdmin {
			return allowed
		}
		return forbidden
	}
	if g.InstanceAdmin {
		return allowed
	}
	role := g.OnAll
	switch envID {
	case envA:
		role = max(role, g.OnA)
	case envB:
		role = max(role, g.OnB)
	}
	if role == rbac.RoleNone {
		return notVisible
	}
	if role < reg.MinRole {
		return forbidden
	}
	// An extra is an Admin's always, an Operator's only when the assignment
	// carries it, and a Viewer's never.
	holdsExtra := role == rbac.RoleAdmin || (role == rbac.RoleOperator && slices.Contains(g.Extras, reg.Name))
	if reg.Extra && !holdsExtra {
		return forbidden
	}
	return allowed
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// fill turns a route pattern into a request path: {envId} is the environment
// under test, every other parameter is "x".
func fill(pattern, envID string) string {
	return pathParam.ReplaceAllStringFunc(pattern, func(p string) string {
		if p == "{envId}" {
			return envID
		}
		return "x"
	})
}

func problemTypeOf(body []byte) string {
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Type
}

func TestEveryRouteRefusesEveryArchetypeThatLacksItsRequirement(t *testing.T) {
	e := newEnforcement(t)

	var judged = map[kind]int{}
	for _, rec := range e.srv.Routes() {
		k, reg := classify(rec)
		switch k {
		case kindSkip:
			continue
		case kindUnknown:
			t.Errorf("%s %s: the enforcement harness does not recognise its requirement %+v; add an expectation for it", rec.Method, rec.Path, rec.Req)
			continue
		}
		judged[k]++
		envs := []string{envA}
		if k == kindEnv {
			envs = []string{envA, envB}
		}
		t.Run(rec.Method+" "+rec.Path, func(t *testing.T) {
			for _, envID := range envs {
				path := fill(rec.Path, envID)
				for _, a := range servertest.Archetypes() {
					want := expect(a, k, reg, envID)
					before := e.denials()
					rr := e.send(a, rec.Method, path, want == allowed)
					after := e.denials()
					who := string(a) + " on " + envID + " " + rec.Method + " " + path

					if want == allowed {
						// Reaching the handler is all the policy promises; what the handler
						// then says (400, 404 for an ID of "x", 409 for an offline agent)
						// is its own business, but never a refusal by the policy.
						if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden ||
							(k == kindEnv && rr.Code == http.StatusNotFound && problemTypeOf(rr.Body.Bytes()) == "environment_not_found") {
							t.Errorf("%s: %d %s, but the archetype holds the requirement", who, rr.Code, rr.Body)
						}
						if after.count != before.count {
							t.Errorf("%s: an allowed request wrote %d permission.denied rows", who, after.count-before.count)
						}
						continue
					}

					wantCode := map[outcome]int{unauthorized: http.StatusUnauthorized, forbidden: http.StatusForbidden, notVisible: http.StatusNotFound}[want]
					if rr.Code == http.StatusUnprocessableEntity || rr.Code >= http.StatusInternalServerError {
						t.Errorf("%s: a refusal answered %d, never 422 or 5xx: %s", who, rr.Code, rr.Body)
						continue
					}
					if rr.Code != wantCode {
						t.Errorf("%s: %d %s, want %s", who, rr.Code, rr.Body, want)
						continue
					}
					if want == notVisible && problemTypeOf(rr.Body.Bytes()) != "environment_not_found" {
						t.Errorf("%s: type %q, want environment_not_found", who, problemTypeOf(rr.Body.Bytes()))
					}
					if after.count != before.count+1 {
						t.Errorf("%s: permission.denied rows went from %d to %d, want one more", who, before.count, after.count)
						continue
					}
					if after.target != rec.Method+" "+rec.Path || after.result != "denied" {
						t.Errorf("%s: audit row target %q result %q, want %q denied", who, after.target, after.result, rec.Method+" "+rec.Path)
					}
					wantReason := map[outcome]string{unauthorized: "unauthenticated", forbidden: "forbidden", notVisible: "environment_not_visible"}[want]
					if after.reason != wantReason {
						t.Errorf("%s: audit reason %q, want %q", who, after.reason, wantReason)
					}
					if k == kindEnv && after.envID != envID {
						t.Errorf("%s: audit env_id %q, want the path value", who, after.envID)
					}
				}
			}
		})
	}

	// The walk must have found each shape, or it proved nothing.
	for _, k := range []kind{kindSelf, kindGlobal, kindEnv} {
		if judged[k] == 0 {
			t.Errorf("no route of kind %d was judged: the harness did not reach the registered routes", k)
		}
	}
}

// An environment a caller has no grant on answers exactly as one that does not
// exist, on a real route of the assembled server, byte for byte (D-08).
func TestAnUnseenEnvironmentAnswersExactlyLikeAMissingOne(t *testing.T) {
	e := newEnforcement(t)
	for _, a := range []servertest.Archetype{servertest.ArchetypeNoGrant, servertest.ArchetypeViewerB, servertest.ArchetypeEnvAdminA} {
		unseen := envA
		if a == servertest.ArchetypeEnvAdminA {
			unseen = envB
		}
		got := e.send(a, http.MethodGet, "/api/env/"+unseen+"/summary", false)
		missing := e.send(a, http.MethodGet, "/api/env/"+envMissing+"/summary", false)
		if got.Code != http.StatusNotFound || got.Code != missing.Code || got.Body.String() != missing.Body.String() {
			t.Errorf("%s: an environment with no grant answers %d %s, a missing one %d %s; they must be identical",
				a, got.Code, got.Body, missing.Code, missing.Body)
		}
	}
	// A disabled environment looks the same to someone who cannot see it.
	if _, err := e.srv.Store().Write.Exec(`UPDATE environments SET enabled = 0 WHERE id = ?`, envB); err != nil {
		t.Fatal(err)
	}
	rr := e.srvNoEnsureGet(servertest.ArchetypeNoGrant, "/api/env/"+envB+"/summary")
	if rr.Code != http.StatusNotFound || problemTypeOf(rr.Body.Bytes()) != "environment_not_found" {
		t.Errorf("a disabled environment, asked by a caller with no grant: %d %s, want 404 environment_not_found", rr.Code, rr.Body)
	}
	// Someone who can see it is told it is disabled.
	rr = e.srvNoEnsureGet(servertest.ArchetypeViewerB, "/api/env/"+envB+"/summary")
	if rr.Code != http.StatusConflict || problemTypeOf(rr.Body.Bytes()) != "environment_disabled" {
		t.Errorf("a disabled environment, asked by its Viewer: %d %s, want 409 environment_disabled", rr.Code, rr.Body)
	}
}

// srvNoEnsureGet is send without putting the environments back first.
func (e *enforcement) srvNoEnsureGet(a servertest.Archetype, path string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	cookie, _ := servertest.SessionFor(e.t, e.srv, e.ids[a])
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestEnvironmentListsShowOnlyWhatEachArchetypeCanSee(t *testing.T) {
	e := newEnforcement(t)
	// Every account has used both environments, so the recent list can only be
	// narrowed by what the account may read.
	for _, a := range servertest.Archetypes() {
		if e.ids[a] == "" {
			continue
		}
		for i, env := range []string{envA, envB} {
			if _, err := e.srv.Store().Write.Exec(`INSERT INTO user_recent_environments (user_id, env_id, used_at) VALUES (?, ?, ?)`,
				e.ids[a], env, i+1); err != nil {
				t.Fatal(err)
			}
		}
	}

	want := map[servertest.Archetype][]string{
		servertest.ArchetypeInstanceAdmin: {envA, envB},
		servertest.ArchetypeViewerAll:     {envA, envB},
		servertest.ArchetypeEnvAdminA:     {envA},
		servertest.ArchetypeOperatorA:     {envA},
		servertest.ArchetypeOperatorAExec: {envA},
		servertest.ArchetypeViewerA:       {envA},
		servertest.ArchetypeViewerB:       {envB},
		servertest.ArchetypeNoGrant:       {},
	}
	for a, ids := range want {
		t.Run(string(a), func(t *testing.T) {
			rr := e.send(a, http.MethodGet, "/api/environments", false)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /api/environments = %d %s", rr.Code, rr.Body)
			}
			var list struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(list.Items))
			for _, it := range list.Items {
				got = append(got, it.ID)
			}
			slices.Sort(got)
			if !slices.Equal(got, ids) {
				t.Errorf("environments = %v, want %v", got, ids)
			}

			rr = e.send(a, http.MethodGet, "/api/me/recent-environments", false)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /api/me/recent-environments = %d %s", rr.Code, rr.Body)
			}
			var recent struct {
				IDs []string `json:"environmentIds"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &recent); err != nil {
				t.Fatal(err)
			}
			slices.Sort(recent.IDs)
			if !slices.Equal(append([]string{}, recent.IDs...), ids) {
				t.Errorf("recent environments = %v, want %v", recent.IDs, ids)
			}
		})
	}
}
