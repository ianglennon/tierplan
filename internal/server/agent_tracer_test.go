// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/proto/testsupport"
	"github.com/ianglennon/coaming/proto/wire"
)

// agentAllowlist mirrors the paths the agent's proxy forwards (D-27). The test
// asserts the fake daemon was asked for nothing else, which proves the whole
// path (monitor, tunnel, agent proxy) stays inside the read-only surface.
var agentAllowlist = regexp.MustCompile(
	`^/(v[0-9]+\.[0-9]+/)?(_ping|version|info|containers/json|images/json|volumes|networks|events|nodes|services|tasks)$`)

// lockedBuffer is an io.Writer a process and a test can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAgentTracerStandaloneEnrolsAndComesOnline is the tracer for the agent
// architecture (ENV-01, ENV-05, PLAT-07): an admin creates an agent
// environment, a real agent binary built from the agent/ module enrols over WSS
// with the join token, the server multiplexes Docker HTTP over yamux through the
// agent's allowlisting proxy, and the environment comes online in
// GET /api/environments with the engine version read through the tunnel.
func TestAgentTracerStandaloneEnrolsAndComesOnline(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}

	// The agent binary, built from its own module.
	agentBin := filepath.Join(t.TempDir(), "coaming-agent")
	build := exec.Command("go", "build", "-o", agentBin, "./cmd/coaming-agent") //nolint:gosec // the agent module is built from this repository
	build.Dir = "../../agent"
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}

	// Coaming over TLS. The listener exists before Build so the public URL, which
	// agents are told to dial and which the handshake audience is checked against,
	// is known.
	ts := httptest.NewUnstartedServer(nil)
	publicURL := "https://" + ts.Listener.Addr().String()
	cfg := testConfig(t, map[string]string{
		"COAMING_DATA_DIR":   t.TempDir() + "/data",
		"COAMING_PUBLIC_URL": publicURL,
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close()
	})

	// Trust for the agent: the test server's own certificate, through
	// SSL_CERT_FILE, which the Go standard library honours for system roots.
	certFile := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	// An admin signs in.
	cookie, csrf := signInAdmin(t, srv, ts)
	call := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// The admin creates an agent environment.
	code, body := call(http.MethodPost, "/api/environments", `{"kind":"agent"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/environments kind agent: %d %s", code, body)
	}
	var created struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Install struct {
			JoinToken string `json:"joinToken"`
			AgentURL  string `json:"agentUrl"`
		} `json:"install"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != "agent" || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}
	if !strings.HasPrefix(created.Install.JoinToken, "cmjt_"+created.ID+"_") {
		t.Fatalf("join token %q is not cmjt_<envId>_<secret>", created.Install.JoinToken)
	}
	if want := "wss://" + ts.Listener.Addr().String() + "/agent/connect"; created.Install.AgentURL != want {
		t.Fatalf("agent URL = %q, want %q", created.Install.AgentURL, want)
	}

	// A tab watches the global health stream before the agent starts (plan 02-12).
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, ts.URL+"/api/environments/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamReq.AddCookie(cookie)
	streamResp, err := ts.Client().Do(streamReq) //nolint:bodyclose // closed by stopStream, which ends the request
	if err != nil || streamResp.StatusCode != http.StatusOK {
		t.Fatalf("open the environments stream: %v, %v", streamResp, err)
	}
	streamed := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(streamResp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				streamed <- name
			}
		}
		close(streamed)
	}()

	// The agent starts on a host whose Docker is a fake daemon.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "tracer-host"})
	agentCtx, stopAgent := context.WithCancel(context.Background())
	var agentLog lockedBuffer
	agent := exec.CommandContext(agentCtx, agentBin) //nolint:gosec // the binary this test just built
	agent.Env = []string{
		"COAMING_AGENT_URL=" + created.Install.AgentURL,
		"COAMING_AGENT_JOIN_TOKEN=" + created.Install.JoinToken,
		"COAMING_AGENT_SOCKET=" + fake.SocketPath(),
		"SSL_CERT_FILE=" + certFile,
		"PATH=" + os.Getenv("PATH"),
	}
	agent.Stdout, agent.Stderr = &agentLog, &agentLog
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = agent.Wait(); close(exited) }()
	t.Cleanup(func() {
		stopAgent()
		<-exited
	})

	// Within 15 seconds the environment is online with the engine version read
	// through the tunnel.
	type environment struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
		Engine struct {
			Version string `json:"version"`
		} `json:"engine"`
		EnvType string `json:"envType"`
		Nodes   *struct {
			Ready int `json:"ready"`
			Total int `json:"total"`
		} `json:"nodes"`
		AgentVersion    *string `json:"agentVersion"`
		UpdateAvailable bool    `json:"updateAvailable"`
	}
	// The server's session and monitor say when the agent is online with its engine
	// read; after that the list is read once.
	awaitAgentOnline(t, srv, created.ID, exited, &agentLog, true)
	var got environment
	code, body = call(http.MethodGet, "/api/environments", "")
	var list struct {
		Items []environment `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET /api/environments: %d %s", code, body)
	}
	for _, e := range list.Items {
		if e.ID == created.ID {
			got = e
		}
	}
	if got.Status.State != "online" || got.Engine.Version == "" {
		t.Fatalf("the environment is not online with an engine version although the monitor found it so: %+v\nagent output:\n%s", got, agentLog.String())
	}
	if got.Kind != "agent" || got.Engine.Version != "29.1.0" {
		t.Fatalf("environment = %+v, want kind agent and engine version 29.1.0", got)
	}
	// D-23: the environment had no name, so the first agent's hostname is it.
	if got.Name != "tracer-host" {
		t.Errorf("environment name = %q, want the agent's hostname tracer-host", got.Name)
	}
	if strings.Contains(agentLog.String(), created.Install.JoinToken) {
		t.Error("the agent logged the join token")
	}

	// The overview columns come from the real agent (plan 02-12).
	if got.EnvType != "standalone" || got.Nodes == nil || got.Nodes.Ready != 1 || got.Nodes.Total != 1 || got.AgentVersion == nil || *got.AgentVersion == "" || got.UpdateAvailable {
		t.Errorf("overview = envType %q, nodes %+v, agentVersion %v, updateAvailable %v; want standalone, 1 of 1, the agent's version and no update", got.EnvType, got.Nodes, got.AgentVersion, got.UpdateAvailable)
	}
	// The tab hears it: the snapshot, the agent connecting, and the environment changing state.
	seen := map[string]bool{}
	for timeout := time.After(testsupport.Deadline); !seen["snapshot"] || !seen["agent.connected"] || !seen["env.status"]; {
		select {
		case name, ok := <-streamed:
			if !ok {
				t.Fatalf("the environments stream ended; saw %v", seen)
			}
			seen[name] = true
		case <-timeout:
			t.Fatalf("the environments stream carried only %v, want snapshot, agent.connected and env.status", seen)
		}
	}
	// The admin's last-used environment is kept on the server.
	if code, body := call(http.MethodPut, "/api/me/recent-environments/"+created.ID, ""); code != http.StatusNoContent {
		t.Fatalf("PUT recent environment: %d %s", code, body)
	}
	if code, body := call(http.MethodGet, "/api/me/recent-environments", ""); code != http.StatusOK || !strings.Contains(string(body), created.ID) {
		t.Errorf("GET recent environments = %d %s, want it to list %s", code, body, created.ID)
	}

	// Everything the fake daemon was asked for is a read on an allowlisted path.
	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("the fake daemon was never asked anything")
	}
	for _, p := range reqs {
		if !agentAllowlist.MatchString(p) {
			t.Errorf("the fake daemon received %q, which is not an allowlisted read path", p)
		}
	}
	// The agent itself reads /info and /version for its facts but never pings:
	// a /_ping here is the server's monitor, reaching the daemon through the tunnel.
	if !slices.Contains(reqs, "/_ping") {
		t.Errorf("the monitor never reached the daemon through the tunnel: %v", reqs)
	}

	// The enrolment was audited under the system principal, with the source IP.
	code, body = call(http.MethodGet, "/api/audit?action=agent.enrolled", "")
	if code != http.StatusOK || !strings.Contains(string(body), "system:agent-enrol") {
		t.Errorf("GET /api/audit?action=agent.enrolled = %d %s", code, body)
	}
}

// signInAdmin creates an admin directly in the store and signs in over HTTPS. It
// returns the session cookie and the CSRF token.
func signInAdmin(t *testing.T, srv *Server, ts *httptest.Server) (*http.Cookie, string) {
	t.Helper()
	const password = "correct horse battery"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := srv.Store().Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	if err := srv.Store().Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, srv.Store().Q, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Post(ts.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	me, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = me.Body.Close() }()
	var got struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(me.Body).Decode(&got); err != nil || got.CSRFToken == "" {
		t.Fatalf("no CSRF token from /api/me: %v", err)
	}
	return cookie, got.CSRFToken
}

// A wrong join token is refused, the agent exits with the refusal on its log, no
// agent row is written and the environment stays unreached.
func TestAgentTracerWrongTokenIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	agentBin := filepath.Join(t.TempDir(), "coaming-agent")
	build := exec.Command("go", "build", "-o", agentBin, "./cmd/coaming-agent") //nolint:gosec // the agent module is built from this repository
	build.Dir = "../../agent"
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}

	ts := httptest.NewUnstartedServer(nil)
	cfg := testConfig(t, map[string]string{
		"COAMING_DATA_DIR":   t.TempDir() + "/data",
		"COAMING_PUBLIC_URL": "https://" + ts.Listener.Addr().String(),
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close()
	})
	certFile := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := srv.Agents().CreateEnvironment(context.Background(), audit.SystemCLI, "Production", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Right environment, wrong secret.
	wrong := "cmjt_" + created.EnvID + "_" + strings.Repeat("A", 43)

	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	var out lockedBuffer
	agent := exec.Command(agentBin) //nolint:gosec // the binary this test just built
	agent.Env = []string{
		"COAMING_AGENT_URL=wss://" + ts.Listener.Addr().String() + "/agent/connect",
		"COAMING_AGENT_JOIN_TOKEN=" + wrong,
		"COAMING_AGENT_SOCKET=" + fake.SocketPath(),
		"SSL_CERT_FILE=" + certFile,
		"PATH=" + os.Getenv("PATH"),
	}
	agent.Stdout, agent.Stderr = &out, &out
	done := make(chan error, 1)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- agent.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the agent exited cleanly after being refused")
		}
	case <-time.After(testsupport.Deadline):
		_ = agent.Process.Kill()
		t.Fatalf("the agent did not exit after being refused; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), wire.ReasonAuthRejected) {
		t.Errorf("the agent did not log the refusal code %s; output:\n%s", wire.ReasonAuthRejected, out.String())
	}
	if strings.Contains(out.String(), wrong) {
		t.Error("the agent logged the join token")
	}

	var n int
	if err := srv.Store().Read.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agents rows = %d, %v; want none", n, err)
	}
	e, err := srv.Environments().Get(context.Background(), created.EnvID)
	if err != nil || e.Status.State != envs.StateUnknown {
		t.Fatalf("environment = %+v, %v; want it unreached", e.Status, err)
	}
}
