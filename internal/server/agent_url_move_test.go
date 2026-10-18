// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// TestAnAgentOfflineDuringASecondAddressChange is the tracer for G-02-4b: an
// agent that followed one change of Bayplan's agent address, was offline during a
// second, and is started again with the new BAYPLAN_AGENT_URL on its own volume
// must connect at the new address as the same agent, with no join token and
// without enrolling again (ENV-05). The server is one handler behind three TLS
// listeners, A (the install address), B and C; it accepts agent handshakes only for
// the address it currently hands out, so the agent's stored address B is refused
// once C is the setting.
func TestAnAgentOfflineDuringASecondAddressChange(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}

	agentBin := filepath.Join(t.TempDir(), "bayplan-agent")
	build := exec.Command("go", "build", "-o", agentBin, "./cmd/bayplan-agent") //nolint:gosec // the agent module is built from this repository
	build.Dir = "../../agent"
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}

	// A is the install address and the public URL. B and C answer with the same
	// handler, so they differ only in their port, which is part of the audience.
	tsA := httptest.NewUnstartedServer(nil)
	cfg := testConfig(t, map[string]string{
		"BAYPLAN_DATA_DIR":   t.TempDir() + "/data",
		"BAYPLAN_PUBLIC_URL": "https://" + tsA.Listener.Addr().String(),
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tsA.Config.Handler = srv.Handler()
	tsA.StartTLS()
	tsB := httptest.NewUnstartedServer(srv.Handler())
	tsB.StartTLS()
	tsC := httptest.NewUnstartedServer(srv.Handler())
	tsC.StartTLS()
	t.Cleanup(func() {
		tsC.Close()
		tsB.Close()
		tsA.Close()
		_ = srv.Close()
	})
	// One certificate for all three, so one SSL_CERT_FILE trusts them.
	if !bytes.Equal(tsA.Certificate().Raw, tsB.Certificate().Raw) || !bytes.Equal(tsA.Certificate().Raw, tsC.Certificate().Raw) {
		t.Fatal("the three test listeners do not present the same certificate")
	}
	certFile := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tsA.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	cookie, csrf := signInAdmin(t, srv, tsA)
	call := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, tsA.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := tsA.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	code, body := call(http.MethodPost, "/api/environments", `{"kind":"agent"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/environments kind agent: %d %s", code, body)
	}
	var created struct {
		ID      string `json:"id"`
		Install struct {
			JoinToken string `json:"joinToken"`
			AgentURL  string `json:"agentUrl"`
		} `json:"install"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "mover-host"})

	// startAgent runs the agent binary on stateDir and returns a function that
	// stops it, and its log.
	startAgent := func(agentURL, joinToken string) (stop func(), log *lockedBuffer) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		log = &lockedBuffer{}
		cmd := exec.CommandContext(ctx, agentBin) //nolint:gosec // the binary this test just built
		cmd.Env = []string{
			"BAYPLAN_AGENT_URL=" + agentURL,
			"BAYPLAN_AGENT_SOCKET=" + fake.SocketPath(),
			"BAYPLAN_AGENT_STATE_DIR=" + stateDir,
			"SSL_CERT_FILE=" + certFile,
			"PATH=" + os.Getenv("PATH"),
		}
		if joinToken != "" {
			cmd.Env = append(cmd.Env, "BAYPLAN_AGENT_JOIN_TOKEN="+joinToken)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		var stopped bool
		stop = func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			<-exited
		}
		t.Cleanup(stop)
		return stop, log
	}

	settings := func() (enrolled, offline int) {
		t.Helper()
		code, body := call(http.MethodGet, "/api/settings", "")
		var s struct {
			EnrolledAgents int `json:"enrolledAgents"`
			OfflineAgents  int `json:"offlineAgents"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &s) != nil {
			t.Fatalf("GET /api/settings: %d %s", code, body)
		}
		return s.EnrolledAgents, s.OfflineAgents
	}
	// waitOnline waits until the server holds a session from the agent and the monitor
	// has found its engine online, failing with the agent's log otherwise. The session
	// is what matters after a restart: the environment can still read online for a
	// moment after its agent left, so the caller has seen the old session end first.
	waitOnline := func(log *lockedBuffer) {
		t.Helper()
		awaitAgentOnline(t, srv, created.ID, nil, log, false)
	}
	setAgentURL := func(u string) {
		t.Helper()
		if code, body := call(http.MethodPatch, "/api/settings", `{"agentUrl":"`+u+`"}`); code != http.StatusOK {
			t.Fatalf("PATCH /api/settings agentUrl %s: %d %s", u, code, body)
		}
	}
	// The server shows the environment online once it has welcomed the agent,
	// and the agent saves identity.json just after, so wait for the file.
	agentID := func() string {
		t.Helper()
		var got string
		testsupport.Poll(t, testsupport.Deadline, "identity.json to hold an agent ID", func() (bool, string) { // testsupport: external, the agent process writing its state file
			raw, err := os.ReadFile(filepath.Join(stateDir, "identity.json")) //nolint:gosec // the test's own temporary directory
			var id struct {
				AgentID string `json:"agentId"`
			}
			if err == nil && json.Unmarshal(raw, &id) == nil && id.AgentID != "" {
				got = id.AgentID
				return true, ""
			}
			return false, fmt.Sprintf("%q, %v", raw, err)
		})
		return got
	}

	// The agent enrols at the install address.
	stop, log := startAgent(created.Install.AgentURL, created.Install.JoinToken)
	waitOnline(log)
	enrolledAs := agentID()

	// The first change: B. The connected agent is told and keeps it first.
	wssB := "wss://" + tsB.Listener.Addr().String() + "/agent/connect"
	setAgentURL("https://" + tsB.Listener.Addr().String())
	testsupport.Poll(t, testsupport.Deadline, "urls.json to list "+wssB+" first", func() (bool, string) { // testsupport: external, the agent process writing its state file
		raw, _ := os.ReadFile(filepath.Join(stateDir, "urls.json")) //nolint:gosec // the test's own temporary directory
		var entries []struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &entries) == nil && len(entries) > 0 && entries[0].URL == wssB {
			return true, ""
		}
		return false, string(raw) + "\nagent output:\n" + log.String()
	})

	// The agent goes offline, and the address changes again while it is away.
	stop()
	if _, err := srv.tunnel.AwaitSession(testsupport.Context(t), enrolledAs, func(s *tunnel.Session) bool { return s == nil }); err != nil {
		t.Fatalf("the server never saw the agent leave: %v", err)
	}
	wssC := "wss://" + tsC.Listener.Addr().String() + "/agent/connect"
	setAgentURL("https://" + tsC.Listener.Addr().String())

	// The operator starts the agent again on its volume with the new address and no
	// join token. It comes back as the same agent.
	_, log = startAgent(wssC, "")
	waitOnline(log)
	if got := agentID(); got != enrolledAs {
		t.Errorf("the agent ID changed from %s to %s: it enrolled again", enrolledAs, got)
	}
	if enrolled, offline := settings(); enrolled != 1 || offline != 0 {
		t.Errorf("settings report enrolledAgents %d and offlineAgents %d, want 1 and 0", enrolled, offline)
	}
	code, body = call(http.MethodGet, "/api/audit?action=agent.enrolled", "")
	var trail struct {
		Items []json.RawMessage `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &trail) != nil || len(trail.Items) != 1 {
		t.Errorf("GET /api/audit?action=agent.enrolled = %d %s, want exactly one entry", code, body)
	}
}
