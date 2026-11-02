// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The stored secret is write-only: it may only go where it was stored for (CR-A01, CR-B01).
// Each of these tries to point it somewhere else without typing it again.

func TestAnEditThatMovesTheSecretToAnotherHostNeedsTheSecretTypedAgain(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin,
		`{"name":"harbor","type":"generic","host":"harbor.example.org","prefix":"team","username":"ci","secret":"hunter2hunter2"}`)

	for name, body := range map[string]string{
		"host":     `{"host":"evil.example.org"}`,
		"type":     `{"type":"gitlab"}`,
		"username": `{"username":"someone-else"}`,
		"ca":       `{"caPem":"` + strings.ReplaceAll(newFakeRegistry(t).caOf(), "\n", `\n`) + `"}`,
		"insecure": `{"insecure":true}`,
	} {
		rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"message":"secret_required"`) || !strings.Contains(rec.Body.String(), "body.secret") {
			t.Errorf("%s with no secret: %d %s, want 422 secret_required on body.secret", name, rec.Code, rec.Body)
		}
	}
	var host, user string
	if err := f.st.Read.QueryRow(`SELECT host, username FROM registry_credentials WHERE id = ?`, c.ID).Scan(&host, &user); err != nil || host != "harbor.example.org" || user != "ci" {
		t.Errorf("stored = %q %q, %v; a refused edit changes nothing", host, user, err)
	}
	if actions, _ := f.registryAudit(t); len(actions) != 1 {
		t.Errorf("audit actions = %v; a refused edit writes no update row", actions)
	}

	// With a secret typed now the host moves, and the audit row says from where to where.
	rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"host":"harbor2.example.org","secret":"`+newCredToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch with a secret: %d %s", rec.Code, rec.Body)
	}
	actions, details := f.registryAudit(t)
	last := details[len(details)-1]
	if actions[len(actions)-1] != "registry.credential_updated" || last["host_from"] != "harbor.example.org" || last["host_to"] != "harbor2.example.org" {
		t.Errorf("update audit = %v %v, want host_from and host_to", actions, last)
	}
}

func TestATestOfAStoredCredentialNeverSendsItsSecretToAnotherHost(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	home, evil := newFakeRegistry(t), newFakeRegistry(t)
	f.trusting(t, home, evil)
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, home.credential("home", home.pass))

	for name, body := range map[string]string{
		"another host":     `{"credentialId":"` + c.ID + `","host":"` + evil.host() + `"}`,
		"another username": `{"credentialId":"` + c.ID + `","username":"someone-else"}`,
		"another type":     `{"credentialId":"` + c.ID + `","type":"gitlab","host":""}`,
		"insecure":         `{"credentialId":"` + c.ID + `","insecure":true}`,
		"another CA":       `{"credentialId":"` + c.ID + `","caPem":"` + strings.ReplaceAll(evil.caOf(), "\n", `\n`) + `"}`,
	} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"message":"secret_required"`) {
			t.Errorf("%s with no secret: %d %s, want 422 secret_required", name, rec.Code, rec.Body)
		}
	}
	if n := evil.hits.Load(); n != 0 {
		t.Errorf("the other registry was sent %d requests; the stored secret must not be lent to it", n)
	}
	// With a secret typed now the same test is allowed.
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test",
		`{"credentialId":"`+c.ID+`","host":"`+evil.host()+`","secret":`+jsonString(evil.pass)+`}`)
	if rec.Code != http.StatusOK {
		t.Errorf("test with a typed secret: %d %s", rec.Code, rec.Body)
	}
}

func TestATestFromAnEnvironmentNeverSendsTheStoredSecretToAnotherHost(t *testing.T) {
	t.Parallel()
	f, env, node, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	inst := f.addPullCredential(t, "inst", "")
	envCred := f.addPullCredential(t, "own", env.ID)

	for name, c := range map[string]struct{ path, id, envID string }{
		"instance":    {"/api/registries/test-from-env", inst.ID, `"envId":"` + env.ID + `",`},
		"environment": {"/api/env/" + env.ID + "/registries/test-from-env", envCred.ID, ""},
	} {
		rec := f.credCall(t, admin, http.MethodPost, c.path,
			`{`+c.envID+`"credentialId":"`+c.id+`","host":"evil.example.org","type":"generic","repository":"private/app"}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"message":"secret_required"`) {
			t.Errorf("%s family: %d %s, want 422 secret_required", name, rec.Code, rec.Body)
		}
	}
	if got := node.fake.DistributionAuth(); len(got) != 0 {
		t.Errorf("the node was sent %d registry headers; the stored secret must not be lent to another host", len(got))
	}
}

// A check from an environment is made by the node's engine, outside the server's egress policy, so
// an address the policy refuses is not sent to the node at all (WR-B02).
func TestATestFromAnEnvironmentNeverAsksANodeToReachAnAddressTheEgressPolicyRefuses(t *testing.T) {
	t.Parallel()
	f, env, node, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	for _, host := range []string{"169.254.169.254", "169.254.169.254:5000", "100.100.100.200", "192.0.0.192:5000"} {
		body := `{"envId":"` + env.ID + `","type":"generic","host":"` + host + `","username":"bot","secret":"` + pullCredSecret + `","repository":"private/app"}`
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s, want a failed result", host, rec.Code, rec.Body)
			continue
		}
		if got := decodeFromEnv(t, rec); got.State != "failed" || got.Code != "registry_unreachable" {
			t.Errorf("%s: %+v, want failed registry_unreachable", host, got)
		}
	}
	if got := node.fake.DistributionRefs(); len(got) != 0 {
		t.Errorf("the node was asked about %v; a refused address must not reach an engine", got)
	}
}
