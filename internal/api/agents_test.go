// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The join token endpoints (plan 02-10, D-02): rotate, disable and inspect.

type rotatedBody struct {
	Install *struct {
		JoinToken  string `json:"joinToken"`
		SecretName string `json:"secretName"`
		Generation int    `json:"generation"`
		Swarm      struct {
			Command       string `json:"command"`
			RotateCommand string `json:"rotateCommand"`
		} `json:"swarm"`
	} `json:"install"`
	EnvType   string `json:"envType"`
	HasAgents bool   `json:"hasAgents"`
}

type tokenStatusBody struct {
	Generation int    `json:"generation"`
	CreatedAt  string `json:"createdAt"`
	CreatedBy  string `json:"createdBy"`
	Disabled   bool   `json:"disabled"`
}

// agentEnv adds an agent environment behind a public URL and returns its ID and
// first join token.
func (f *envFix) agentEnv(t *testing.T) (id, token string) {
	t.Helper()
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil {
		t.Fatalf("add agent environment: %d %s", rec.code, rec.body)
	}
	return got.ID, got.Install.JoinToken
}

// insertAgent puts an enrolled agent row into the environment, as an enrolment would.
func (f *envFix) insertAgent(t *testing.T, envID, id, role, status string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`INSERT INTO agents (id, env_id, name, role, engine_id, public_key, cert_serial, cert_fingerprint, cert_not_after, agent_version, status, created_at)
		VALUES (?, ?, 'node', ?, 'ENG', x'00', '1', 'ff', 0, '0.2.0', ?, 0)`, id, envID, role, status); err != nil {
		t.Fatal(err)
	}
}

func (f *envFix) rotate(t *testing.T, envID string) (rotatedBody, *envRec) {
	t.Helper()
	rec := f.req(t, http.MethodPost, "/api/environments/"+envID+"/token", "")
	var got rotatedBody
	if rec.code == http.StatusOK {
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("decode %s: %v", rec.body, err)
		}
	}
	return got, rec
}

func TestTokenRotateAnswersTheInstallObjectOnceWithTheNextGenerationAndNoStore(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, first := f.agentEnv(t)

	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/environments/" + id + "/token", cookie: f.cookie, csrf: f.csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got rotatedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Install == nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if got.Install.Generation != 2 || got.Install.SecretName != "bayplan-agent-token-g2" {
		t.Errorf("generation %d secret %q, want 2 and bayplan-agent-token-g2", got.Install.Generation, got.Install.SecretName)
	}
	if !strings.HasPrefix(got.Install.JoinToken, "bpjt_"+id+"_") || got.Install.JoinToken == first {
		t.Errorf("the new token %q is not a fresh token of the environment", got.Install.JoinToken)
	}
	if !strings.Contains(got.Install.Swarm.Command, got.Install.JoinToken) || !strings.Contains(got.Install.Swarm.Command, "bayplan-agent-token-g2") {
		t.Errorf("the swarm command does not carry the new token and secret name:\n%s", got.Install.Swarm.Command)
	}
	if got.HasAgents || got.EnvType != "" || got.Install.Swarm.RotateCommand != "" {
		t.Errorf("an environment with no agents: hasAgents %v envType %q rotate %q, want none of them", got.HasAgents, got.EnvType, got.Install.Swarm.RotateCommand)
	}

	// The token is shown once: the status never has it, and the old one no longer enrols.
	status := f.req(t, http.MethodGet, "/api/environments/"+id+"/token", "")
	if strings.Contains(string(status.body), got.Install.JoinToken) || strings.Contains(strings.ToLower(string(status.body)), "jointoken") {
		t.Errorf("the status shows a token: %s", status.body)
	}
	if n := len(f.audits(t, audit.ActionEnvironmentTokenRotated)); n != 1 {
		t.Errorf("%d environment.token_rotated entries, want 1", n)
	}
}

func TestTokenRotateOfASwarmWithAgentsCarriesTheRotateCommand(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.insertAgent(t, id, "agent-1", "manager", "active")

	got, rec := f.rotate(t, id)
	if rec.code != http.StatusOK || got.Install == nil {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if got.EnvType != "swarm" || !got.HasAgents {
		t.Errorf("envType %q hasAgents %v, want swarm and true", got.EnvType, got.HasAgents)
	}
	if !strings.Contains(got.Install.Swarm.RotateCommand, "bayplan-agent-token-g2") {
		t.Errorf("rotate command lacks the new secret name:\n%s", got.Install.Swarm.RotateCommand)
	}
	if !strings.Contains(got.Install.Swarm.RotateCommand, got.Install.JoinToken) {
		t.Errorf("rotate command lacks the new token:\n%s", got.Install.Swarm.RotateCommand)
	}
}

func TestTokenRotateOfAStandaloneHostHasNoSwarmRotateCommand(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.insertAgent(t, id, "agent-1", "standalone", "active")

	got, rec := f.rotate(t, id)
	if rec.code != http.StatusOK || got.Install == nil {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if got.EnvType != "standalone" || !got.HasAgents || got.Install.Swarm.RotateCommand != "" {
		t.Errorf("envType %q hasAgents %v rotate %q, want standalone, true and no rotate command", got.EnvType, got.HasAgents, got.Install.Swarm.RotateCommand)
	}
}

func TestTokenRotateWithoutAnAddressRefusesAndChangesNothing(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.inst.Apply("Bayplan", "")
	_, rec := f.rotate(t, id)
	if rec.code != http.StatusConflict || rec.ptyp != "public_url_required" {
		t.Fatalf("status = %d type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM env_join_tokens WHERE env_id = ?`, id).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d token rows (%v), want the one the environment started with", n, err)
	}
	if got := len(f.audits(t, audit.ActionEnvironmentTokenRotated)); got != 0 {
		t.Errorf("%d rotation entries after a refused rotation", got)
	}
}

func TestTokenDisableAnswers204AuditsAndShowsInTheStatusUntilARotation(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)

	rec := f.req(t, http.MethodDelete, "/api/environments/"+id+"/token", "")
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if n := len(f.audits(t, audit.ActionEnvironmentTokenDisabled)); n != 1 {
		t.Errorf("%d environment.token_disabled entries, want 1", n)
	}
	var st tokenStatusBody
	rec = f.req(t, http.MethodGet, "/api/environments/"+id+"/token", "")
	if rec.code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.code, rec.body)
	}
	if err := json.Unmarshal(rec.body, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Disabled || st.Generation != 1 || st.CreatedBy != "admin" || st.CreatedAt == "" {
		t.Errorf("status = %+v, want generation 1, disabled, created by admin", st)
	}

	if got, rec := f.rotate(t, id); rec.code != http.StatusOK || got.Install == nil || got.Install.Generation != 2 {
		t.Fatalf("rotation after a disable: %d %s", rec.code, rec.body)
	}
	rec = f.req(t, http.MethodGet, "/api/environments/"+id+"/token", "")
	st = tokenStatusBody{}
	if err := json.Unmarshal(rec.body, &st); err != nil || st.Disabled || st.Generation != 2 {
		t.Errorf("status after the rotation = %+v (%v), want generation 2, enabled", st, err)
	}
}

func TestTokenEndpointsAnswer404EnvironmentNotFoundForAnUnknownEnvironment(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	const missing = "0197b8a0-0000-7000-8000-000000000000"
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodGet} {
		rec := f.req(t, method, "/api/environments/"+missing+"/token", "")
		if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
			t.Errorf("%s: status = %d type %q: %s", method, rec.code, rec.ptyp, rec.body)
		}
	}
}

func TestTokenEndpointsNeedEnvironmentsTokenNotManage(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.addUserWithGlobalPerms(t, "manager", "environments.manage", "environments.create", "environments.delete", "environments.read")
	f.addUserWithGlobalPerms(t, "keeper", "environments.token")

	cookie, csrf := f.signIn(t, "manager", false)
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodGet} {
		rec := f.call(t, authReq{method: method, path: "/api/environments/" + id + "/token", cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without environments.token: %d %s, want 403", method, rec.Code, rec.Body)
		}
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 3 || denied[0].ActorID != "id-manager" || denied[0].Result != "denied" {
		t.Fatalf("permission.denied entries = %+v, want three for the manager", denied)
	}
	if n := len(f.audits(t, audit.ActionEnvironmentTokenRotated)) + len(f.audits(t, audit.ActionEnvironmentTokenDisabled)); n != 0 {
		t.Errorf("%d token entries written by a caller who was refused", n)
	}

	cookie, csrf = f.signIn(t, "keeper", false)
	for method, want := range map[string]int{http.MethodGet: http.StatusOK, http.MethodPost: http.StatusOK, http.MethodDelete: http.StatusNoContent} {
		rec := f.call(t, authReq{method: method, path: "/api/environments/" + id + "/token", cookie: cookie, csrf: csrf})
		if rec.Code != want {
			t.Errorf("%s with environments.token: %d %s, want %d", method, rec.Code, rec.Body, want)
		}
	}
}

func TestTokenRotateAndDisableNeverPutTheTokenInTheAudit(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, first := f.agentEnv(t)
	got, rec := f.rotate(t, id)
	if rec.code != http.StatusOK || got.Install == nil {
		t.Fatalf("rotate: %d %s", rec.code, rec.body)
	}
	if rec := f.req(t, http.MethodDelete, "/api/environments/"+id+"/token", ""); rec.code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", rec.code, rec.body)
	}
	var secrets []string
	for _, tok := range []string{first, got.Install.JoinToken} {
		_, s, ok := agents.ParseToken(tok)
		if !ok {
			t.Fatalf("token %q does not parse", tok)
		}
		secrets = append(secrets, s)
	}
	rows, err := f.st.Read.Query(`SELECT action, target_id, COALESCE(env_id, ''), reason, ip, user_agent, details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cols [7]string
		if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6]); err != nil {
			t.Fatal(err)
		}
		row := strings.Join(cols[:], "|")
		for _, s := range secrets {
			if strings.Contains(row, s) {
				t.Errorf("an audit row holds a join token secret: %s", row)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
