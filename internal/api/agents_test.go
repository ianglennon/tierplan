// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The join token endpoints (plan 02-10, D-02): rotate, disable and inspect.

type rotatedBody struct {
	Install *struct {
		JoinToken  string `json:"joinToken"`
		SecretName string `json:"secretName"`
		Generation int    `json:"generation"`
		Swarm      struct {
			Command       string `json:"command"`
			StackFile     string `json:"stackFile"`
			RotateCommand string `json:"rotateCommand"`
		} `json:"swarm"`
		Standalone struct {
			Command     string `json:"command"`
			ComposeFile string `json:"composeFile"`
		} `json:"standalone"`
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
	f.inst.Apply("TierPlan", "https://example.com/tierplan")
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
	t.Parallel()
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
	if got.Install.Generation != 2 || got.Install.SecretName != "tierplan-agent-token-g2" {
		t.Errorf("generation %d secret %q, want 2 and tierplan-agent-token-g2", got.Install.Generation, got.Install.SecretName)
	}
	if !strings.HasPrefix(got.Install.JoinToken, "tpjt_"+id+"_") || got.Install.JoinToken == first {
		t.Errorf("the new token %q is not a fresh token of the environment", got.Install.JoinToken)
	}
	if !strings.Contains(got.Install.Swarm.Command, got.Install.JoinToken) || !strings.Contains(got.Install.Swarm.Command, "tierplan-agent-token-g2") {
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
	t.Parallel()
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
	if !strings.Contains(got.Install.Swarm.RotateCommand, "tierplan-agent-token-g2") {
		t.Errorf("rotate command lacks the new secret name:\n%s", got.Install.Swarm.RotateCommand)
	}
	if !strings.Contains(got.Install.Swarm.RotateCommand, got.Install.JoinToken) {
		t.Errorf("rotate command lacks the new token:\n%s", got.Install.Swarm.RotateCommand)
	}
}

func TestTokenRotateOfAStandaloneHostHasNoSwarmRotateCommand(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.inst.Apply("TierPlan", "")
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

// A rotation whose install would point agents at a plain address for another host
// is refused before anything changes: the current token stays and nothing is audited
// (G-02-R4-3, T-02-144, T-02-145).
func TestTokenRotateWithAPlainAddressForAnotherHostRefusesAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.inst.ApplyAgentURL("http://10.0.0.5:7480")
	_, rec := f.rotate(t, id)
	if rec.code != http.StatusConflict || rec.ptyp != "insecure_agent_url" {
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
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("TierPlan", "https://example.com")
	const missing = "0197b8a0-0000-7000-8000-000000000000"
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodGet} {
		rec := f.req(t, method, "/api/environments/"+missing+"/token", "")
		if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
			t.Errorf("%s: status = %d type %q: %s", method, rec.code, rec.ptyp, rec.body)
		}
	}
}

func TestTokenEndpointsAreForInstanceAdminsNotForAnAdminOfEveryEnvironment(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	id, _ := f.agentEnv(t)
	f.addAllEnvAdmin(t, "manager")

	cookie, csrf := f.signIn(t, "manager", false)
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodGet} {
		rec := f.call(t, authReq{method: method, path: "/api/environments/" + id + "/token", cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s as an admin of every environment: %d %s, want 403", method, rec.Code, rec.Body)
		}
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 3 || denied[0].ActorID != "id-manager" || denied[0].Result != "denied" {
		t.Fatalf("permission.denied entries = %+v, want three for the manager", denied)
	}
	if n := len(f.audits(t, audit.ActionEnvironmentTokenRotated)) + len(f.audits(t, audit.ActionEnvironmentTokenDisabled)); n != 0 {
		t.Errorf("%d token entries written by a caller who was refused", n)
	}

	for method, want := range map[string]int{http.MethodGet: http.StatusOK, http.MethodPost: http.StatusOK, http.MethodDelete: http.StatusNoContent} {
		rec := f.req(t, method, "/api/environments/"+id+"/token", "")
		if rec.code != want {
			t.Errorf("%s as an instance admin: %d %s, want %d", method, rec.code, rec.body, want)
		}
	}
}

func TestTokenRotateAndDisableNeverPutTheTokenInTheAudit(t *testing.T) {
	t.Parallel()
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
	RenewalFailing     bool   `json:"renewalFailing"`
	ClockOffsetSeconds int    `json:"clockOffsetSeconds"`
	LastSeen           string `json:"lastSeen"`
	ReadOnly           bool   `json:"readOnly"`
	SelfUpdate         bool   `json:"selfUpdate"`
	UpdateState        struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	} `json:"updateState"`
	ManualUpdateCommand string `json:"manualUpdateCommand"`
	UpdateAvailable     bool   `json:"updateAvailable"`
	RevokedAt           string `json:"revokedAt"`
	RevokeReason        string `json:"revokeReason"`
	LastDenial          *struct {
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
	t.Parallel()
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
	awaitHeartbeat(t, f.srv.Hub.Session(zulu.id), func(st tunnel.SessionStatus) bool { return st.LastDenial != nil })

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
	t.Parallel()
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

func TestAgentListOfAnEnvironmentTheCallerHasNoGrantOnIs404WithNothingOfItLikeAnUnknownOne(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "A")
	b := f.srv.NewEnvironment(t, "B")
	f.connect(t, b, dockertest.FakeConfig{Hostname: "b-node"}, nil)
	f.addUser(t, "viewer", false)
	f.grantRead(t, "viewer", a.ID)
	cookie, csrf := f.signIn(t, "viewer", false)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + b.ID + "/agents", cookie: cookie, csrf: csrf})
	// Every /api/env route answers an environment the caller has no grant on with
	// the 404 of a missing one and a permission.denied entry (D-08): the answer
	// carries nothing of the environment's agents.
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "b-node") || strings.Contains(rec.Body.String(), b.ID) {
		t.Fatalf("an environment the viewer may not read: %d %s, want 404 and nothing of it", rec.Code, rec.Body)
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 1 || denied[0].ActorID != "id-viewer" || denied[0].Reason != "environment_not_visible" {
		t.Errorf("permission.denied entries = %+v", denied)
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/agents", "")
	if rec.code != http.StatusOK || !strings.Contains(string(rec.body), `"items":[]`) {
		t.Errorf("a local environment: %d %s, want an empty list", rec.code, rec.body)
	}
}

func TestRevokeNeedsRevokeOnTheEnvironmentAndDisconnectsTheAgentAndBlocksTheHost(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active"}, swarmNode("alpha", false))
	f.srv.Events.WaitLen(t, 1)
	s := f.srv.Hub.Session(a.id)
	path := "/api/env/" + env.ID + "/agents/" + a.id + "/revoke"

	f.addEnvUser(t, "reader", env.ID, "viewer")
	cookie, csrf := f.signIn(t, "reader", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: path, body: `{}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke as a Viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 || denied[0].ActorID != "id-reader" || denied[0].Result != "denied" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}
	if f.node(t, env.ID, a.id).Status != "active" || isClosed(s) {
		t.Fatal("a refused revoke changed the agent")
	}

	rec2 := f.req(t, http.MethodPost, path, `{"reason":"decommissioned"}`)
	if rec2.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec2.code, rec2.body)
	}
	testsupport.Await(t, "the session to close", s.Done())
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
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	other := f.srv.NewEnvironment(t, "Other")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, nil)
	b := f.connect(t, other, dockertest.FakeConfig{Hostname: "beta"}, nil)

	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/agents/"+a.id+"/revoke", `{"reason":"`+strings.Repeat("x", 257)+`"}`); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a 257-character reason: %d %s, want 422", rec.code, rec.body)
	}
	if f.node(t, env.ID, a.id).Status != "active" {
		t.Fatal("a refused revoke changed the agent")
	}
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/agents/no-such-agent/revoke", `{}`); rec.code != http.StatusNotFound || rec.ptyp != "agent_not_found" {
		t.Errorf("an unknown agent: %d type %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/agents/"+b.id+"/revoke", `{}`); rec.code != http.StatusNotFound || rec.ptyp != "agent_not_found" {
		t.Errorf("an agent of another environment: %d type %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPost, "/api/env/0197b8a0-0000-7000-8000-000000000000/agents/"+a.id+"/revoke", `{}`); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("an unknown environment: %d type %q", rec.code, rec.ptyp)
	}
	if f.node(t, other.ID, b.id).Status != "active" {
		t.Error("the other environment's agent was changed")
	}
	// No body at all is a revoke without a reason.
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/agents/"+a.id+"/revoke", ""); rec.code != http.StatusNoContent {
		t.Errorf("a revoke without a body: %d %s, want 204", rec.code, rec.body)
	}
}

func TestUnblockLiftsTheBlocksOfARevokedAgentAndIsNotBlockedOtherwise(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, swarmNode("alpha", false))
	revoke := "/api/env/" + env.ID + "/agents/" + a.id + "/revoke"
	unblock := "/api/env/" + env.ID + "/agents/" + a.id + "/unblock"

	if rec := f.req(t, http.MethodPost, unblock, ""); rec.code != http.StatusConflict || rec.ptyp != "not_blocked" {
		t.Errorf("unblocking an active agent: %d type %q %s", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.req(t, http.MethodPost, revoke, `{}`); rec.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.code, rec.body)
	}
	if !f.node(t, env.ID, a.id).Blocked {
		t.Fatal("the revoked agent is not blocked")
	}

	f.addEnvUser(t, "reader", env.ID, "viewer")
	cookie, csrf := f.signIn(t, "reader", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: unblock, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Errorf("unblock as a Viewer: %d, want 403", rec.Code)
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

// The adopt-cluster endpoint (plan 02-19, D-14).

// pendingView is the part of an environment the cluster change shows in.
type pendingView struct {
	ID               string  `json:"id"`
	PendingClusterID *string `json:"pendingClusterId"`
	AutoUpdate       *bool   `json:"autoUpdate"`
	Status           struct {
		State string `json:"state"`
	} `json:"status"`
}

func (f *agentFix) environmentView(t *testing.T, envID string) pendingView {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/environments", "")
	if rec.code != http.StatusOK {
		t.Fatalf("list environments: %d %s", rec.code, rec.body)
	}
	var got struct {
		Items []pendingView `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	for _, e := range got.Items {
		if e.ID == envID {
			return e
		}
	}
	t.Fatalf("environment %s is not listed", envID)
	return pendingView{}
}

// clusterChanged records a change of cluster the way the handshake does.
func (f *agentFix) clusterChanged(t *testing.T, envID, pending string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`UPDATE environments SET cluster_changed_at = ?, pending_cluster_id = ? WHERE id = ?`,
		f.tclk.Now().UnixNano(), pending, envID); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentsListShowsThePendingClusterOnlyWhileTheClusterIsChanged(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}, swarmNode("alpha", true))
	f.srv.Events.WaitLen(t, 1)

	if got := f.environmentView(t, env.ID); got.PendingClusterID != nil || got.Status.State == "cluster_changed" {
		t.Fatalf("before a change: %+v, want no pending cluster", got)
	}
	f.clusterChanged(t, env.ID, "cluster-2")
	got := f.environmentView(t, env.ID)
	if got.PendingClusterID == nil || *got.PendingClusterID != "cluster-2" || got.Status.State != "cluster_changed" {
		t.Fatalf("after a change: %+v, want cluster-2 pending and state cluster_changed", got)
	}
	// A leftover pending ID with no change recorded is not shown.
	if _, err := f.st.Write.Exec(`UPDATE environments SET cluster_changed_at = NULL WHERE id = ?`, env.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.environmentView(t, env.ID); got.PendingClusterID != nil {
		t.Fatalf("with no change recorded the pending cluster is %q", *got.PendingClusterID)
	}
}

func TestAdoptClusterNeedsConfigureOnTheEnvironmentAndRebindsThroughAnAuditedAction(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}, swarmNode("alpha", true))
	f.srv.Events.WaitLen(t, 1)
	path := "/api/env/" + env.ID + "/adopt-cluster"
	f.clusterChanged(t, env.ID, "cluster-2")

	f.addEnvUser(t, "ops", env.ID, "viewer")
	cookie, csrf := f.signIn(t, "ops", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: path, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Fatalf("adopt as a Viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 1 || denied[0].ActorID != "id-ops" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}
	if got := f.environmentView(t, env.ID); got.PendingClusterID == nil {
		t.Fatal("a refused adopt changed the environment")
	}

	rec := f.req(t, http.MethodPost, path, "")
	if rec.code != http.StatusOK {
		t.Fatalf("adopt: %d %s", rec.code, rec.body)
	}
	var body struct {
		ClusterID         string `json:"clusterId"`
		PreviousClusterID string `json:"previousClusterId"`
	}
	if err := json.Unmarshal(rec.body, &body); err != nil || body.ClusterID != "cluster-2" || body.PreviousClusterID != "cluster-1" {
		t.Fatalf("body = %s (%v), want the new and the previous cluster", rec.body, err)
	}
	if got := f.environmentView(t, env.ID); got.PendingClusterID != nil || got.Status.State == "cluster_changed" {
		t.Fatalf("after the adopt: %+v, want the change cleared", got)
	}
	entries := f.audits(t, audit.ActionEnvironmentClusterAdopted)
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != env.ID {
		t.Fatalf("environment.cluster_adopted = %+v", entries)
	}

	// A second adopt, or one for an environment that does not exist, says so.
	if rec := f.req(t, http.MethodPost, path, ""); rec.code != http.StatusConflict || rec.ptyp != "no_cluster_change" {
		t.Errorf("a second adopt: %d type %q %s, want 409 no_cluster_change", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.req(t, http.MethodPost, "/api/env/0197b8a0-0000-7000-8000-000000000000/adopt-cluster", ""); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("an unknown environment: %d type %q", rec.code, rec.ptyp)
	}
}

func TestAnAgentOfTheAdoptedClusterIsAcceptedOnItsNextHello(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	first := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}, swarmNode("alpha", true))
	f.srv.Events.WaitLen(t, 1)

	reinit := swarmNode("alpha", true)
	reinit.Swarm.ClusterID = "cluster-2"
	if c := f.srv.Dial(t, tunneltest.Agent{Identity: &first.conn.Identity, Facts: reinit}); c.Refuse == nil || c.Refuse.Code != wire.ReasonClusterChanged {
		t.Fatalf("hello after the swarm was re-initialised: %+v (err %v), want cluster_changed", c.Refuse, c.Err)
	}
	if got := f.environmentView(t, env.ID); got.PendingClusterID == nil || *got.PendingClusterID != "cluster-2" {
		t.Fatalf("the handshake did not record the change: %+v", got)
	}
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/adopt-cluster", ""); rec.code != http.StatusOK {
		t.Fatalf("adopt: %d %s", rec.code, rec.body)
	}
	if c := f.srv.Dial(t, tunneltest.Agent{Identity: &first.conn.Identity, Facts: reinit}); c.Welcome == nil {
		t.Fatalf("hello after the adopt: %+v (err %v), want a welcome", c.Refuse, c.Err)
	}
}

// leaderIs is the cluster reader's answer for the leading manager's agent.
type leaderIs string

func (l leaderIs) LeaderAgent(string) string { return string(l) }

func TestAgentListCallsTheLeadersAgentLeaderAndTheOthersManagerOrWorker(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	alpha := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}, swarmNode("alpha", true))
	bravo := f.connect(t, env, dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true}, swarmNode("bravo", true))
	charlie := f.connect(t, env, dockertest.FakeConfig{Hostname: "charlie", SwarmState: "active"}, swarmNode("charlie", false))

	// No cluster reader, or one that knows no leader: every manager is a manager.
	for _, id := range []string{alpha.id, bravo.id} {
		if got := f.node(t, env.ID, id).Role; got != "manager" {
			t.Fatalf("without a known leader %s is a %q, want manager", id, got)
		}
	}

	f.agentCluster = leaderIs(bravo.id)
	f.h = f.build(t)
	for id, want := range map[string]string{alpha.id: "manager", bravo.id: "leader", charlie.id: "worker"} {
		if got := f.node(t, env.ID, id).Role; got != want {
			t.Errorf("role of %s = %q, want %q", id, got, want)
		}
	}

	// Only a manager can be the leader: a worker named as one is still a worker.
	f.agentCluster = leaderIs(charlie.id)
	f.h = f.build(t)
	if got := f.node(t, env.ID, charlie.id).Role; got != "worker" {
		t.Errorf("a worker named as leader is listed as %q, want worker", got)
	}
}

func TestAgentListSaysRenewalIsFailingWhenLessThanSevenDaysOfCertificateAreLeft(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Edge")
	fresh := f.srv.Enrol(t, env)
	f.srv.Events.WaitLen(t, 1)

	set := func(id string, left time.Duration) {
		f.srv.Exec(t, `UPDATE agents SET cert_not_after = ? WHERE id = ?`, time.Now().Add(left).UnixNano(), id)
	}
	set(fresh.Identity.AgentID, 8*24*time.Hour)
	if f.node(t, env.ID, fresh.Identity.AgentID).RenewalFailing {
		t.Error("renewalFailing is set with 8 days left")
	}
	set(fresh.Identity.AgentID, 6*24*time.Hour)
	if !f.node(t, env.ID, fresh.Identity.AgentID).RenewalFailing {
		t.Error("renewalFailing is not set with 6 days left")
	}
	// An expired certificate is the worst case of the same state.
	set(fresh.Identity.AgentID, -time.Hour)
	if !f.node(t, env.ID, fresh.Identity.AgentID).RenewalFailing {
		t.Error("renewalFailing is not set for an expired certificate")
	}

	// A revoked agent is not trying to renew, so it is not failing to.
	set(fresh.Identity.AgentID, 2*24*time.Hour)
	if err := f.srv.Svc.Revoke(context.Background(), audit.SystemCLI, env.ID, fresh.Identity.AgentID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if f.node(t, env.ID, fresh.Identity.AgentID).RenewalFailing {
		t.Error("renewalFailing is set for a revoked agent")
	}
}

// Agent updates (plan 02-28, D-13, D-24, D-25, D-28): the per-environment switch, the
// two update endpoints and what the node list says of an update.

// selfUpdating gives an agent's facts a host that lets it update itself.
func selfUpdating(name string) *wire.Facts {
	return &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true, EngineVersion: "29.1.0",
		Policy: wire.PolicyFacts{SelfUpdate: true},
	}
}

// swarmFacts is a swarm node's facts, with the host's self-update choice.
func swarmFacts(name string, control, selfUpdate bool) *wire.Facts {
	f := swarmNode(name, control)
	f.Policy = wire.PolicyFacts{SelfUpdate: selfUpdate}
	return f
}

// controlLine reads the next line the server wrote to an agent's control stream.
func controlLine(t *testing.T, control io.Reader) wire.Update {
	t.Helper()
	got := make(chan wire.Update, 1)
	go func() {
		sc := bufio.NewScanner(control)
		if sc.Scan() {
			var u wire.Update
			_ = json.Unmarshal(sc.Bytes(), &u)
			got <- u
		}
	}()
	return testsupport.Await(t, "the server to write to the agent's control stream", got)
}

// asAllEnvAdmin makes the request as a user who is Admin on every environment
// without being an instance admin, and returns what came back.
func (f *agentFix) asAllEnvAdmin(t *testing.T, name string, method, path, body string) *envRec {
	t.Helper()
	f.addAllEnvAdmin(t, name)
	return f.asUser(t, name, method, path, body)
}

func TestSwitchingAutoUpdateNeedsConfigureOnTheEnvironmentStoresItAndIsAudited(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	path := "/api/env/" + env.ID

	if got := f.environmentView(t, env.ID); got.AutoUpdate == nil || !*got.AutoUpdate {
		t.Fatalf("a new agent environment has autoUpdate %v, want on by default", got.AutoUpdate)
	}
	f.addUser(t, "reader", false)
	f.addEnvRole(t, "reader", env.ID, "viewer")
	rec := f.asUser(t, "reader", http.MethodPatch, path, `{"autoUpdate":false}`)
	if rec.code != http.StatusForbidden {
		t.Fatalf("PATCH autoUpdate as a Viewer: %d %s, want 403", rec.code, rec.body)
	}
	if got := f.environmentView(t, env.ID); got.AutoUpdate == nil || !*got.AutoUpdate {
		t.Fatal("a refused PATCH changed the setting")
	}

	rec = f.req(t, http.MethodPatch, path, `{"autoUpdate":false}`)
	if rec.code != http.StatusOK {
		t.Fatalf("PATCH autoUpdate: %d %s", rec.code, rec.body)
	}
	if got := f.environmentView(t, env.ID); got.AutoUpdate == nil || *got.AutoUpdate {
		t.Fatalf("autoUpdate after switching off = %v", got.AutoUpdate)
	}
	entries := f.audits(t, audit.ActionEnvironmentAutoUpdateChanged)
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != env.ID {
		t.Fatalf("environment.auto_update_changed = %+v", entries)
	}
	if got := f.audits(t, audit.ActionEnvironmentRenamed); len(got) != 0 {
		t.Fatalf("switching auto-update renamed the environment: %+v", got)
	}

	// Name and autoUpdate may be sent together; neither is required alone, but one is.
	rec = f.req(t, http.MethodPatch, path, `{"name":"Lab","autoUpdate":true}`)
	if rec.code != http.StatusOK || len(f.audits(t, audit.ActionEnvironmentRenamed)) != 1 || len(f.audits(t, audit.ActionEnvironmentAutoUpdateChanged)) != 2 {
		t.Fatalf("PATCH both: %d %s", rec.code, rec.body)
	}
	if rec := f.req(t, http.MethodPatch, path, `{}`); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "nothing_to_change" {
		t.Fatalf("PATCH with nothing to change: %d type %q %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestAnAdminUpdatesOneAgentThroughTheTunnelAndItIsAuditedUnderTheirName(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, selfUpdating("alpha"))
	f.srv.Events.WaitLen(t, 1)
	path := "/api/env/" + env.ID + "/agents/" + a.id + "/update"

	f.addEnvUser(t, "reader", env.ID, "viewer")
	rec := f.asUser(t, "reader", http.MethodPost, path, ``)
	if rec.code != http.StatusForbidden {
		t.Fatalf("update as a Viewer: %d %s, want 403", rec.code, rec.body)
	}
	if n := f.node(t, env.ID, a.id); n.UpdateState.State != "idle" {
		t.Fatalf("a refused update changed the state: %+v", n.UpdateState)
	}

	rec = f.req(t, http.MethodPost, path, ``)
	if rec.code != http.StatusAccepted {
		t.Fatalf("update: %d %s, want 202", rec.code, rec.body)
	}
	u := controlLine(t, a.control)
	if u.T != wire.TypeUpdate || u.Image != "ghcr.io/ianglennon/tierplan-agent:0.2.1" || u.ID == "" {
		t.Fatalf("the agent was sent %+v", u)
	}
	if n := f.node(t, env.ID, a.id); n.UpdateState.State != "updating" {
		t.Fatalf("state after the update = %+v, want updating", n.UpdateState)
	}
	entries := f.audits(t, audit.ActionAgentUpdated)
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != a.id || entries[0].IP == "" {
		t.Fatalf("agent.updated = %+v", entries)
	}

	// The agent answers that it cannot: the list says why.
	f.updater.HandleResult(a.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: u.ID, OK: false, Error: wire.UpdateErrImageNotOwnRepository})
	n := f.node(t, env.ID, a.id)
	if n.UpdateState.State != "failed" || n.UpdateState.Reason != "image_not_own_repository" {
		t.Fatalf("state after a refusal = %+v", n.UpdateState)
	}
	if got := f.audits(t, audit.ActionAgentUpdateFailed); len(got) != 1 || got[0].Reason != "image_not_own_repository" {
		t.Fatalf("agent.update_failed = %+v", got)
	}
}

func TestUpdatingAnAgentThatIsOfflineUnknownUpToDateOrRefusingIsAProblem(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	base := "/api/env/" + env.ID + "/agents/"

	if rec := f.req(t, http.MethodPost, base+"nobody/update", ``); rec.code != http.StatusNotFound || rec.ptyp != "agent_not_found" {
		t.Errorf("an unknown agent: %d type %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPost, "/api/env/nope/agents/x/update", ``); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("an unknown environment: %d type %q", rec.code, rec.ptyp)
	}

	off := f.connect(t, env, dockertest.FakeConfig{Hostname: "off"}, selfUpdating("off"))
	f.srv.Events.WaitLen(t, 1)
	base = "/api/env/" + env.ID + "/agents/"
	s := f.srv.Hub.Session(off.id)
	_ = off.conn.WS.CloseNow()
	testsupport.Await(t, "the session to end", s.Done())
	if rec := f.req(t, http.MethodPost, base+off.id+"/update", ``); rec.code != http.StatusConflict || rec.ptyp != "agent_offline" {
		t.Errorf("an offline agent: %d type %q %s, want 409 agent_offline", rec.code, rec.ptyp, rec.body)
	}

	env2 := f.srv.NewEnvironment(t, "Refusing")
	refusing := f.connect(t, env2, dockertest.FakeConfig{Hostname: "refusing"}, &wire.Facts{
		EngineID: "ENG:refusing", Hostname: "refusing", EngineReachable: true, Policy: wire.PolicyFacts{SelfUpdate: false}})
	f.srv.Events.WaitLen(t, 3)
	if rec := f.req(t, http.MethodPost, "/api/env/"+env2.ID+"/agents/"+refusing.id+"/update", ``); rec.code != http.StatusConflict || rec.ptyp != "self_update_disabled" {
		t.Errorf("a host that refuses: %d type %q %s, want 409 self_update_disabled", rec.code, rec.ptyp, rec.body)
	}
	if got := f.audits(t, audit.ActionAgentUpdated); len(got) != 0 {
		t.Errorf("a refused update was audited as done: %+v", got)
	}
}

func TestUpdatingAllAgentsAnswers202WithHowManyWereStarted(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, selfUpdating("alpha"))
	f.srv.Events.WaitLen(t, 1)
	path := "/api/env/" + env.ID + "/update-agents"

	f.addEnvUser(t, "reader", env.ID, "viewer")
	if rec := f.asUser(t, "reader", http.MethodPost, path, ``); rec.code != http.StatusForbidden {
		t.Fatalf("update-agents as a Viewer: %d", rec.code)
	}
	rec := f.req(t, http.MethodPost, path, ``)
	if rec.code != http.StatusAccepted {
		t.Fatalf("update-agents: %d %s, want 202", rec.code, rec.body)
	}
	var got struct {
		Started int `json:"started"`
	}
	if err := json.Unmarshal(rec.body, &got); err != nil || got.Started != 1 {
		t.Fatalf("answer %s (%v), want started 1", rec.body, err)
	}
	if u := controlLine(t, a.control); u.T != wire.TypeUpdate {
		t.Fatalf("the agent was sent %+v", u)
	}
	// A second request while the first runs starts nothing.
	rec = f.req(t, http.MethodPost, path, ``)
	if err := json.Unmarshal(rec.body, &got); rec.code != http.StatusAccepted || err != nil || got.Started != 0 {
		t.Fatalf("a repeated update-agents: %d %s, want started 0", rec.code, rec.body)
	}
}

func TestUpdatingAllAgentsOfAnEnvironmentWithNoneConnectedIsAgentOffline(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	f.insertAgent(t, env.ID, "a-offline", "standalone", "active")
	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/update-agents", ``)
	if rec.code != http.StatusConflict || rec.ptyp != "agent_offline" {
		t.Fatalf("update-agents with nothing connected: %d type %q %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestTheNodeListGivesTheManualUpdateCommandWhenTheHostRefusesSelfUpdate(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	refusing := f.connect(t, env, dockertest.FakeConfig{Hostname: "refusing", SwarmState: "active"}, swarmFacts("refusing", true, false))
	letting := f.connect(t, env, dockertest.FakeConfig{Hostname: "letting", SwarmState: "active"}, swarmFacts("letting", false, true))
	f.srv.Events.WaitLen(t, 2)

	n := f.node(t, env.ID, refusing.id)
	if !strings.Contains(n.ManualUpdateCommand, "docker service update --image ghcr.io/ianglennon/tierplan-agent:0.2.1") {
		t.Errorf("an agent behind on a host that refuses: manualUpdateCommand = %q", n.ManualUpdateCommand)
	}
	if n := f.node(t, env.ID, letting.id); n.ManualUpdateCommand != "" {
		t.Errorf("an agent that can update itself was given the manual command %q", n.ManualUpdateCommand)
	}
	if n := f.node(t, env.ID, letting.id); n.UpdateState.State != "idle" {
		t.Errorf("update state = %+v, want idle", n.UpdateState)
	}
}

func TestTheNodeListSaysWhichAgentsAreBehindTheServerAndNeverARevokedOne(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active"}, swarmFacts("alpha", true, true))
	b := f.connect(t, env, dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active"}, swarmFacts("bravo", false, true))
	f.srv.Events.WaitLen(t, 2)

	// The server is v0.2.1 and tunneltest's agents are v0.2.0.
	if !f.node(t, env.ID, a.id).UpdateAvailable || !f.node(t, env.ID, b.id).UpdateAvailable {
		t.Fatalf("agents of v0.2.0 under a v0.2.1 server must be updateAvailable: %+v", f.nodes(t, env.ID))
	}
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/agents/"+b.id+"/revoke", ``); rec.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.code, rec.body)
	}
	if f.node(t, env.ID, b.id).UpdateAvailable {
		t.Fatal("a revoked agent is offered an update")
	}

	// The same agents under a server of their own version are not behind.
	f.serverVersion = "v0.2.0"
	f.h = f.build(t)
	if f.node(t, env.ID, a.id).UpdateAvailable {
		t.Fatal("an agent of the server's own version is offered an update")
	}
	// A development server cannot be compared with, so nothing is offered.
	f.serverVersion = "dev"
	f.h = f.build(t)
	if f.node(t, env.ID, a.id).UpdateAvailable {
		t.Fatal("an agent is offered an update by a development server")
	}
}

// The rotate answer and the node list's manual update command carry no allow-insecure
// line for any address (D-09). The switch's name is spelled in two halves so a
// repository-wide gate for it stays exact.
func TestRotateAndManualUpdateCarryNoRemovedSwitchLine(t *testing.T) {
	t.Parallel()
	const name = "TIERPLAN_AGENT_ALLOW_" + "INSECURE"
	f := newAgentFix(t)
	f.inst.ApplyAgentURL("https://tierplan.example.com")

	// The rotate answer: a new install for the same environment.
	id, _ := f.agentEnv(t)
	got, rec := f.rotate(t, id)
	if rec.code != http.StatusOK || got.Install == nil {
		t.Fatalf("rotate: %d %s", rec.code, rec.body)
	}
	for label, text := range map[string]string{
		"swarm command": got.Install.Swarm.Command, "stack file": got.Install.Swarm.StackFile,
		"standalone command": got.Install.Standalone.Command, "compose file": got.Install.Standalone.ComposeFile,
		"rotate command": got.Install.Swarm.RotateCommand,
	} {
		if strings.Contains(text, name) {
			t.Errorf("rotated %s carries the removed agent switch:\n%s", label, text)
		}
	}

	// The node list: a standalone agent behind the server on a host that refuses
	// self-update is given the recreate command, a swarm node the service update.
	env := f.srv.NewEnvironment(t, "Dev")
	plain := f.connect(t, env, dockertest.FakeConfig{Hostname: "plain"}, &wire.Facts{
		EngineID: "ENG:plain", Hostname: "plain", EngineReachable: true, Policy: wire.PolicyFacts{SelfUpdate: false}})
	f.srv.Events.WaitLen(t, 1)
	cmd := f.node(t, env.ID, plain.id).ManualUpdateCommand
	if !strings.Contains(cmd, "docker run") || strings.Contains(cmd, name) {
		t.Errorf("a standalone agent's manual update = %q, want a docker run without the removed switch", cmd)
	}
	env2 := f.srv.NewEnvironment(t, "DevSwarm")
	node := f.connect(t, env2, dockertest.FakeConfig{Hostname: "node", SwarmState: "active"}, swarmFacts("node", true, false))
	f.srv.Events.WaitLen(t, 2)
	if cmd := f.node(t, env2.ID, node.id).ManualUpdateCommand; !strings.Contains(cmd, "docker service update") || strings.Contains(cmd, name) {
		t.Errorf("the swarm manual update = %q, want a service update without the removed switch", cmd)
	}
}

// A plain agent address on this machine is refused like any other: agents connect
// only over wss://, so a rotation refuses it and changes nothing (D-09, TLS-12).
func TestRotateWithAnHTTPAgentAddressOnThisMachineIsRefused(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	id, _ := f.agentEnv(t)
	f.inst.ApplyAgentURL("http://127.0.0.1:7480")
	if _, rec := f.rotate(t, id); rec.code != http.StatusConflict || rec.ptyp != "insecure_agent_url" {
		t.Fatalf("status = %d type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

// The node list's manual update for a standalone agent that refuses self-update is
// the recreate command, which carries the agent address. For a plain address on
// another host with the switch off that is a command whose agent would exit at
// start, so there is none; a swarm node's service update carries no address and
// stays (G-02-R4-3, T-02-144).
func TestTheNodeListOffersNoStandaloneManualUpdateForAPlainAddressForAnotherHost(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	f.inst.ApplyAgentURL("http://10.0.0.5:7480")

	env := f.srv.NewEnvironment(t, "Dev")
	plain := f.connect(t, env, dockertest.FakeConfig{Hostname: "plain"}, &wire.Facts{
		EngineID: "ENG:plain", Hostname: "plain", EngineReachable: true, Policy: wire.PolicyFacts{SelfUpdate: false}})
	f.srv.Events.WaitLen(t, 1)
	if cmd := f.node(t, env.ID, plain.id).ManualUpdateCommand; cmd != "" {
		t.Errorf("a standalone agent's manual update for a refused address = %q, want none", cmd)
	}

	env2 := f.srv.NewEnvironment(t, "DevSwarm")
	node := f.connect(t, env2, dockertest.FakeConfig{Hostname: "node", SwarmState: "active"}, swarmFacts("node", true, false))
	f.srv.Events.WaitLen(t, 2)
	if cmd := f.node(t, env2.ID, node.id).ManualUpdateCommand; !strings.Contains(cmd, "docker service update") {
		t.Errorf("the swarm manual update = %q, want the service update", cmd)
	}
}

// The D-07 split in one place: an env Admin of A runs the enrolment actions of A that
// became environment-scoped, is told nothing about B, and a Viewer of A is refused.
func TestAnEnvAdminManagesTheEnrolmentOfTheirOwnEnvironmentAndNoOther(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "Alpha")
	b := f.srv.NewEnvironment(t, "Beta")
	alpha := f.connect(t, a, dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}, swarmNode("alpha", true))
	beta := f.connect(t, b, dockertest.FakeConfig{Hostname: "beta"}, selfUpdating("beta"))
	f.srv.Events.WaitLen(t, 2)
	f.clusterChanged(t, a.ID, "cluster-2")
	f.addEnvUser(t, "adminone", a.ID, "admin")
	f.addEnvUser(t, "viewerone", a.ID, "viewer")

	type call struct{ name, path, body string }
	callsOn := func(env tunneltest.Environment, agent string) []call {
		base := "/api/env/" + env.ID
		return []call{
			{"revoke", base + "/agents/" + agent + "/revoke", `{}`},
			{"unblock", base + "/agents/" + agent + "/unblock", ``},
			{"adopt-cluster", base + "/adopt-cluster", ``},
			{"update", base + "/agents/" + agent + "/update", ``},
			{"update-agents", base + "/update-agents", ``},
		}
	}

	for _, c := range callsOn(a, alpha.id) {
		if rec := f.asUser(t, "viewerone", http.MethodPost, c.path, c.body); rec.code != http.StatusForbidden {
			t.Errorf("%s as a Viewer of the environment: %d %s, want 403", c.name, rec.code, rec.body)
		}
	}
	for _, c := range callsOn(b, beta.id) {
		if rec := f.asUser(t, "adminone", http.MethodPost, c.path, c.body); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
			t.Errorf("%s on another environment as its env Admin: %d type %q %s, want 404 environment_not_found", c.name, rec.code, rec.ptyp, rec.body)
		}
	}
	if f.node(t, b.ID, beta.id).Status != "active" || len(f.audits(t, audit.ActionAgentRevoked)) != 0 {
		t.Fatal("a refused call changed the other environment")
	}

	own := callsOn(a, alpha.id)
	if rec := f.asUser(t, "adminone", http.MethodPost, own[2].path, own[2].body); rec.code != http.StatusOK {
		t.Fatalf("adopt-cluster as an env Admin: %d %s, want 200", rec.code, rec.body)
	}
	for _, c := range []call{own[3], own[4]} {
		if rec := f.asUser(t, "adminone", http.MethodPost, c.path, c.body); rec.code == http.StatusForbidden || rec.code == http.StatusNotFound {
			t.Errorf("%s as an env Admin: %d %s, want it authorised", c.name, rec.code, rec.body)
		}
	}
	if rec := f.asUser(t, "adminone", http.MethodPost, own[0].path, own[0].body); rec.code != http.StatusNoContent {
		t.Fatalf("revoke as an env Admin: %d %s, want 204", rec.code, rec.body)
	}
	if rec := f.asUser(t, "adminone", http.MethodPost, own[1].path, own[1].body); rec.code != http.StatusNoContent {
		t.Fatalf("unblock as an env Admin: %d %s, want 204", rec.code, rec.body)
	}
	if got := f.audits(t, audit.ActionAgentRevoked); len(got) != 1 || got[0].ActorID != "id-adminone" {
		t.Errorf("agent.revoked = %+v, want one row for the env Admin", got)
	}
	if got := f.audits(t, audit.ActionEnvironmentClusterAdopted); len(got) != 1 || got[0].ActorID != "id-adminone" || got[0].TargetID != a.ID {
		t.Errorf("environment.cluster_adopted = %+v, want one row for the env Admin", got)
	}
}

// Adding, deleting, folding in the local host and the join token stay with instance
// admins (D-07): an env Admin of the environment is refused each, and it is audited.
func TestAnEnvAdminCannotAddDeleteFoldOrReadTheTokenOfTheirOwnEnvironment(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "Alpha")
	f.addEnvUser(t, "adminone", a.ID, "admin")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/environments", `{"kind":"agent","name":"new"}`},
		{http.MethodDelete, "/api/environments/" + a.ID, ""},
		{http.MethodPost, "/api/environments/" + a.ID + "/fold-local", ""},
		{http.MethodGet, "/api/environments/" + a.ID + "/token", ""},
		{http.MethodPost, "/api/environments/" + a.ID + "/token", ""},
		{http.MethodDelete, "/api/environments/" + a.ID + "/token", ""},
	} {
		if rec := f.asUser(t, "adminone", c.method, c.path, c.body); rec.code != http.StatusForbidden {
			t.Errorf("%s %s as an env Admin: %d %s, want 403", c.method, c.path, rec.code, rec.body)
		}
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 6 {
		t.Errorf("permission.denied entries = %d, want 6", len(denied))
	}
	if rec := f.req(t, http.MethodGet, "/api/environments/"+a.ID+"/token", ""); rec.code != http.StatusOK {
		t.Errorf("token status as an instance admin: %d %s, want 200", rec.code, rec.body)
	}
}

// WR-B-04: a PATCH whose name is invalid changes nothing, even when its autoUpdate
// half would have been valid.
func TestAPatchWithAnInvalidNameDoesNotApplyOrAuditTheAutoUpdateSwitch(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	path := "/api/env/" + env.ID

	for _, name := range []string{"", "   ", strings.Repeat("n", 65)} {
		body, _ := json.Marshal(map[string]any{"autoUpdate": false, "name": name})
		rec := f.req(t, http.MethodPatch, path, string(body))
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "invalid_name" {
			t.Fatalf("PATCH with name %q: %d type %q %s, want 422 invalid_name", name, rec.code, rec.ptyp, rec.body)
		}
		if got := f.environmentView(t, env.ID); got.AutoUpdate == nil || !*got.AutoUpdate {
			t.Fatalf("PATCH with name %q switched auto-update off although it failed", name)
		}
		if got := f.audits(t, audit.ActionEnvironmentAutoUpdateChanged); len(got) != 0 {
			t.Fatalf("PATCH with name %q audited an auto-update change: %+v", name, got)
		}
	}
}
