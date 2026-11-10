// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// relisten listens on addr again, for a server built on the same address as the one just stopped.
// The kernel releasing the old port is outside the test's control, so it is tried until it can.
func relisten(t *testing.T, addr string) net.Listener {
	t.Helper()
	var ln net.Listener
	testsupport.Poll(t, testsupport.Deadline, "the restarted server to listen on its old address", func() (bool, string) { // testsupport: external, the kernel releasing the old listening port
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return false, err.Error()
		}
		ln = l
		return true, ""
	})
	return ln
}

// restart stops the server and builds another on the same data directory and the same address, as
// after an upgrade, so the agents that dial it find it again.
func (r *acmeRig) restart(addr string) *Server {
	r.t.Helper()
	r.stop()
	return r.start(relisten(r.t, addr))
}

// An agent installed with the self-signed pin stays connected through Let's Encrypt and through the
// fall back to the self-signed certificate when the Let's Encrypt one expires (D-15, T-14-28). While
// Let's Encrypt is served the agent trusts it because it chains to its roots, which stand in for the
// public ones; once the server serves the self-signed certificate again, only the pin from the
// install command covers it.
func TestACMEExpiryFallbackKeepsAPinnedAgentConnected(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	agentBin := filepath.Join(t.TempDir(), "tierplan-agent")
	build := exec.Command("go", "build", "-o", agentBin, "./cmd/tierplan-agent") //nolint:gosec // the agent module is built from this repository
	build.Dir = "../../agent"
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}

	// The listener exists before Build, so the public URL agents are told to dial is known. The
	// self-signed certificate covers localhost, and the test-only directory lets the fake issue for it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	r := newACMERig(t, acmetest.Options{}, map[string]string{"TIERPLAN_PUBLIC_URL": "https://localhost:" + port})
	srv := r.start(ln)
	installPin := srv.https.Pin()
	if installPin == "" {
		t.Fatal("the self-signed certificate publishes no pin")
	}

	// An admin adds an agent environment and reads the install command's join token and pin.
	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)
	req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodPost, r.base+"/api/environments", strings.NewReader(`{"kind":"agent"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/environments: %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID      string `json:"id"`
		Install struct {
			JoinToken string `json:"joinToken"`
			AgentURL  string `json:"agentUrl"`
			Pin       string `json:"pin"`
		} `json:"install"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Install.Pin != installPin || created.Install.JoinToken == "" {
		t.Fatalf("install = %+v, want the join token and the self-signed pin %s", created.Install, installPin)
	}

	// The agent starts with the install pin. Its system roots are the fake CA's issuer, standing in for
	// the public roots a real agent has.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "acme-host"})
	agentCtx, stopAgent := context.WithCancel(context.Background())
	var agentLog lockedBuffer
	agent := exec.CommandContext(agentCtx, agentBin) //nolint:gosec // the binary this test just built
	agent.Env = []string{
		"TIERPLAN_AGENT_URL=" + created.Install.AgentURL,
		"TIERPLAN_AGENT_JOIN_TOKEN=" + created.Install.JoinToken,
		"TIERPLAN_AGENT_SOCKET=" + fake.SocketPath(),
		"TIERPLAN_AGENT_PIN=" + created.Install.Pin,
		"TIERPLAN_AGENT_STATE_DIR=" + t.TempDir(),
		"TIERPLAN_AGENT_BACKOFF_MIN=200ms",
		"TIERPLAN_AGENT_BACKOFF_MAX=1s",
		"SSL_CERT_FILE=" + r.fake.IssuerFile(t),
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
	awaitAgentOnline(t, srv, created.ID, exited, &agentLog, true)

	// Let's Encrypt is issued, and the server restarts on the same address: the agent reconnects to a
	// certificate its pin does not match but its roots cover.
	r.issue()
	first := leafOf(t, srv.https.Config())
	srv = r.restart(addr)
	if srv.https.Kind() != tlsKindACME {
		t.Fatalf("after the restart the kind is %d, want tlsKindACME", srv.https.Kind())
	}
	awaitAgentOnline(t, srv, created.ID, exited, &agentLog, true)

	// Renewals fail and the certificate reaches its end: the running server falls back to self-signed.
	r.fake.FailValidation(r.host, acmetest.Problem{Status: 403, Type: "unauthorized", Detail: "scripted"})
	r.step(first.NotAfter.Sub(r.clock.Now()) + time.Hour)
	r.awaitRuns()
	if srv.https.Kind() != tlsKindSelfSigned {
		t.Fatalf("after the expiry the kind is %d, want the self-signed certificate", srv.https.Kind())
	}

	// The server restarts again (it never installs an expired certificate), and the agent finds the
	// self-signed certificate, which only its install-time pin covers.
	srv = r.restart(addr)
	if srv.https.Kind() != tlsKindSelfSigned || srv.https.Pin() != installPin {
		t.Fatalf("after the second restart kind = %d and pin = %q, want the self-signed certificate with pin %q", srv.https.Kind(), srv.https.Pin(), installPin)
	}
	awaitAgentOnline(t, srv, created.ID, exited, &agentLog, true)
	if strings.Contains(agentLog.String(), "tls_untrusted") {
		t.Errorf("the agent reported an untrusted certificate:\n%s", agentLog.String())
	}
}
