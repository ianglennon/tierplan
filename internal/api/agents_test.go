// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
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

// The node list, revoke and unblock (plan 02-15, ENV-05, D-08, D-09, D-38).

type agentItem struct {
	ID                 string `json:"id"`
	Node               string `json:"node"`
	Role               string `json:"role"`
	Status             string `json:"status"`
	Blocked            bool   `json:"blocked"`
	Connected          bool   `json:"connected"`
	AgentVersion       string `json:"agentVersion"`
	EngineVersion      string `json:"engineVersion"`
	CertFingerprint    string `json:"certFingerprint"`
	CertNotAfter       string `json:"certNotAfter"`
	ClockOffsetSeconds int    `json:"clockOffsetSeconds"`
	LastSeen           string `json:"lastSeen"`
	ReadOnly           bool   `json:"readOnly"`
	SelfUpdate         bool   `json:"selfUpdate"`
	RevokedAt          string `json:"revokedAt"`
	RevokeReason       string `json:"revokeReason"`
	LastDenial         *struct {
		Endpoint string `json:"endpoint"`
		At       string `json:"at"`
	} `json:"lastDenial"`
}

func (f *agentFix) nodes(t *testing.T, envID string) []agentItem {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/env/"+envID+"/agents", "")
	if rec.code != http.StatusOK {
		t.Fatalf("list agents: %d %s", rec.code, rec.body)
	}
	var got struct {
		Items []agentItem `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return got.Items
}

func (f *agentFix) node(t *testing.T, envID, id string) agentItem {
	t.Helper()
	for _, n := range f.nodes(t, envID) {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("agent %s is not in the list of %s", id, envID)
	return agentItem{}
}

func swarmNode(name string, control bool) *wire.Facts {
	return &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true, EngineVersion: "29.1.0",
		Swarm:  &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: control},
		Policy: wire.PolicyFacts{ReadOnly: name == "zulu", SelfUpdate: name == "alpha"},
	}
}

func TestAgentListGivesEveryNodeItsCertificateAndLivenessWithProblemsFirst(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	zulu := f.connect(t, env, dockertest.FakeConfig{Hostname: "zulu", SwarmState: "active"}, swarmNode("zulu", false))
	alpha := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}, swarmNode("alpha", true))
	mike := f.connect(t, env, dockertest.FakeConfig{Hostname: "mike", SwarmState: "active"}, swarmNode("mike", false))
	kilo := f.connect(t, env, dockertest.FakeConfig{Hostname: "kilo", SwarmState: "active"}, swarmNode("kilo", false))
	f.srv.Events.WaitLen(t, 4)

	// mike drops its connection; kilo is revoked by an admin.
	_ = mike.conn.WS.CloseNow()
	f.srv.Events.WaitLen(t, 5)
	if err := f.srv.Svc.Revoke(context.Background(), audit.SystemCLI, env.ID, kilo.id, "retired", "", ""); err != nil {
		t.Fatal(err)
	}
	f.srv.Events.WaitLen(t, 6)
	// zulu has been refused a request by its own policy.
	b, _ := json.Marshal(wire.Denied{T: wire.TypeDenied, Method: "POST", Path: "/containers/create", Reason: wire.DeniedReadOnly})
	if _, err := zulu.control.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the denial", func() bool { return f.node(t, env.ID, zulu.id).LastDenial != nil })

	got := f.nodes(t, env.ID)
	var order []string
	for _, n := range got {
		order = append(order, n.Node)
	}
	if want := []string{"kilo", "mike", "alpha", "zulu"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want revoked, then not connected, then the rest by name: %v", order, want)
	}
	kiloN, mikeN, alphaN, zuluN := got[0], got[1], got[2], got[3]

	if kiloN.Status != "revoked" || !kiloN.Blocked || kiloN.Connected || kiloN.RevokeReason != "retired" || kiloN.RevokedAt == "" {
		t.Errorf("kilo = %+v", kiloN)
	}
	if mikeN.Status != "active" || mikeN.Blocked || mikeN.Connected || mikeN.LastSeen == "" {
		t.Errorf("mike = %+v", mikeN)
	}
	if alphaN.ID != alpha.id || alphaN.Role != "manager" || alphaN.Status != "active" || alphaN.Blocked || !alphaN.Connected {
		t.Errorf("alpha = %+v", alphaN)
	}
	if zuluN.Role != "worker" || !zuluN.Connected || !zuluN.ReadOnly || zuluN.SelfUpdate || alphaN.ReadOnly || !alphaN.SelfUpdate {
		t.Errorf("zulu = %+v alpha = %+v: the policy flags are the agent's own", zuluN, alphaN)
	}
	for _, n := range []agentItem{alphaN, zuluN} {
		if n.AgentVersion != "0.2.0" || n.EngineVersion != "29.1.0" {
			t.Errorf("%s versions = agent %q engine %q", n.Node, n.AgentVersion, n.EngineVersion)
		}
	}
	for _, n := range got {
		if len(n.CertFingerprint) != 64 || strings.Trim(n.CertFingerprint, "0123456789abcdef") != "" {
			t.Errorf("%s fingerprint %q is not a full hex SHA-256", n.Node, n.CertFingerprint)
		}
		if p, err := time.Parse(time.RFC3339, n.CertNotAfter); err != nil || p.Location() != time.UTC {
			t.Errorf("%s certNotAfter %q is not RFC 3339 UTC: %v", n.Node, n.CertNotAfter, err)
		}
	}
	if zuluN.LastDenial == nil || zuluN.LastDenial.Endpoint != "/containers/create" || zuluN.LastDenial.At == "" {
		t.Errorf("zulu's last denial = %+v", zuluN.LastDenial)
	}
	if alphaN.LastDenial != nil {
		t.Errorf("alpha has a denial it never had: %+v", alphaN.LastDenial)
	}
	// Nothing secret is in the list.
	rec := f.req(t, http.MethodGet, "/api/env/"+env.ID+"/agents", "")
	for _, word := range []string{"publicKey", "public_key", "joinToken", "keySealed", "certSerial"} {
		if strings.Contains(string(rec.body), word) {
			t.Errorf("the list carries %q: %s", word, rec.body)
		}
	}
}

func TestAgentListReportsTheClockOffsetOfAConnectedAgent(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Edge")
	c := f.srv.Dial(t, tunneltest.Agent{Token: env.Token, ClockSkew: 7 * time.Second})
	if c.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", c.Refuse)
	}
	f.srv.Events.WaitLen(t, 1)
	if got := f.node(t, env.ID, c.Identity.AgentID).ClockOffsetSeconds; got != 7 {
		t.Errorf("clockOffsetSeconds = %d, want 7", got)
	}
}

func TestAgentListOfAnEnvironmentTheCallerCannotReadIs404AndOfALocalHostIsEmpty(t *testing.T) {
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "A")
	b := f.srv.NewEnvironment(t, "B")
	f.connect(t, b, dockertest.FakeConfig{Hostname: "b-node"}, nil)
	f.addUser(t, "viewer", false)
	f.grantRead(t, "viewer", a.ID)
	cookie, csrf := f.signIn(t, "viewer", false)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + b.ID + "/agents", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" || strings.Contains(rec.Body.String(), "b-node") {
		t.Fatalf("an environment the viewer may not read: %d %s, want 404 environment_not_found and nothing of it", rec.Code, rec.Body)
	}
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + a.ID + "/agents", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("the viewer's own environment: %d %s", rec.Code, rec.Body)
	}
	if rec := f.req(t, http.MethodGet, "/api/env/0197b8a0-0000-7000-8000-000000000000/agents", ""); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("an unknown environment: %d %s", rec.code, rec.body)
	}
}

func TestAgentListOfALocalSocketEnvironmentIsEmpty(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/agents", "")
	if rec.code != http.StatusOK || !strings.Contains(string(rec.body), `"items":[]`) {
		t.Errorf("a local environment: %d %s, want an empty list", rec.code, rec.body)
	}
}

func TestRevokeNeedsEnvironmentsRevokeAndAnAdminWhoHasItDisconnectsTheAgentAndBlocksTheHost(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active"}, swarmNode("alpha", false))
	f.srv.Events.WaitLen(t, 1)
	s := f.srv.Hub.Session(a.id)
	path := "/api/environments/" + env.ID + "/agents/" + a.id + "/revoke"

	f.addUserWithGlobalPerms(t, "reader", "environments.read", "environments.manage", "environments.token")
	cookie, csrf := f.signIn(t, "reader", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: path, body: `{}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke without environments.revoke: %d %s, want 403", rec.Code, rec.Body)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 || denied[0].ActorID != "id-reader" || denied[0].Result != "denied" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}
	if f.node(t, env.ID, a.id).Status != "active" || isClosed(s) {
		t.Fatal("a refused revoke changed the agent")
	}

	start := time.Now()
	rec2 := f.req(t, http.MethodPost, path, `{"reason":"decommissioned"}`)
	if rec2.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec2.code, rec2.body)
	}
	waitFor(t, "the session to close", func() bool { return isClosed(s) })
	if took := time.Since(start); took > time.Second {
		t.Errorf("the agent was disconnected %v after the revoke", took)
	}
	n := f.node(t, env.ID, a.id)
	if n.Status != "revoked" || !n.Blocked || n.Connected || n.RevokeReason != "decommissioned" {
		t.Errorf("after the revoke: %+v", n)
	}
	entries := f.audits(t, audit.ActionAgentRevoked)
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != a.id {
		t.Errorf("agent.revoked = %+v", entries)
	}
	if got := len(f.audits(t, audit.ActionAgentBlocked)); got != 2 {
		t.Errorf("agent.blocked entries = %d, want 2 (engine ID and swarm node ID)", got)
	}

	// A second admin who revokes the same node a moment later is told so.
	rec3 := f.req(t, http.MethodPost, path, `{}`)
	if rec3.code != http.StatusConflict || rec3.ptyp != "agent_already_revoked" {
		t.Errorf("revoking twice: %d type %q %s", rec3.code, rec3.ptyp, rec3.body)
	}
}

func isClosed(s *tunnel.Session) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

func TestRevokeAcceptsNoBodyRejectsAnOverlongReasonAndAnswers404ForUnknownThings(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	other := f.srv.NewEnvironment(t, "Other")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, nil)
	b := f.connect(t, other, dockertest.FakeConfig{Hostname: "beta"}, nil)

	if rec := f.req(t, http.MethodPost, "/api/environments/"+env.ID+"/agents/"+a.id+"/revoke", `{"reason":"`+strings.Repeat("x", 257)+`"}`); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a 257-character reason: %d %s, want 422", rec.code, rec.body)
	}
	if f.node(t, env.ID, a.id).Status != "active" {
		t.Fatal("a refused revoke changed the agent")
	}
	if rec := f.req(t, http.MethodPost, "/api/environments/"+env.ID+"/agents/no-such-agent/revoke", `{}`); rec.code != http.StatusNotFound || rec.ptyp != "agent_not_found" {
		t.Errorf("an unknown agent: %d type %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPost, "/api/environments/"+env.ID+"/agents/"+b.id+"/revoke", `{}`); rec.code != http.StatusNotFound || rec.ptyp != "agent_not_found" {
		t.Errorf("an agent of another environment: %d type %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPost, "/api/environments/0197b8a0-0000-7000-8000-000000000000/agents/"+a.id+"/revoke", `{}`); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("an unknown environment: %d type %q", rec.code, rec.ptyp)
	}
	if f.node(t, other.ID, b.id).Status != "active" {
		t.Error("the other environment's agent was changed")
	}
	// No body at all is a revoke without a reason.
	if rec := f.req(t, http.MethodPost, "/api/environments/"+env.ID+"/agents/"+a.id+"/revoke", ""); rec.code != http.StatusNoContent {
		t.Errorf("a revoke without a body: %d %s, want 204", rec.code, rec.body)
	}
}

func TestUnblockLiftsTheBlocksOfARevokedAgentAndIsNotBlockedOtherwise(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, swarmNode("alpha", false))
	revoke := "/api/environments/" + env.ID + "/agents/" + a.id + "/revoke"
	unblock := "/api/environments/" + env.ID + "/agents/" + a.id + "/unblock"

	if rec := f.req(t, http.MethodPost, unblock, ""); rec.code != http.StatusConflict || rec.ptyp != "not_blocked" {
		t.Errorf("unblocking an active agent: %d type %q %s", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.req(t, http.MethodPost, revoke, `{}`); rec.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.code, rec.body)
	}
	if !f.node(t, env.ID, a.id).Blocked {
		t.Fatal("the revoked agent is not blocked")
	}

	f.addUserWithGlobalPerms(t, "reader", "environments.read", "environments.token")
	cookie, csrf := f.signIn(t, "reader", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: unblock, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Errorf("unblock without environments.revoke: %d, want 403", rec.Code)
	}

	if rec := f.req(t, http.MethodPost, unblock, ""); rec.code != http.StatusNoContent {
		t.Fatalf("unblock: %d %s", rec.code, rec.body)
	}
	if n := f.node(t, env.ID, a.id); n.Blocked || n.Status != "revoked" {
		t.Errorf("after the unblock: %+v, want still revoked and no longer blocked", n)
	}
	if got := len(f.audits(t, audit.ActionAgentUnblocked)); got != 2 {
		t.Errorf("agent.unblocked entries = %d", got)
	}
	if rec := f.req(t, http.MethodPost, unblock, ""); rec.code != http.StatusConflict || rec.ptyp != "not_blocked" {
		t.Errorf("unblocking twice: %d type %q", rec.code, rec.ptyp)
	}
	// The host can enrol again with the token.
	c := f.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmNode("alpha", false)})
	if c.Welcome == nil {
		t.Errorf("the unblocked host was refused: %+v", c.Refuse)
	}
}
