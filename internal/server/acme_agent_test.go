// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// buildAgentBinary builds the agent module's binary into the test's temporary directory.
func buildAgentBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tierplan-agent")
	build := exec.Command("go", "build", "-o", bin, "./cmd/tierplan-agent") //nolint:gosec // the agent module is built from this repository
	build.Dir = "../../agent"
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}
	return bin
}

// listenLoopback listens on a loopback port before Build, so the public URL agents are told to
// dial (https://localhost:port) is known. It returns the listener, its address and the port.
func listenLoopback(t *testing.T) (ln net.Listener, addr, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()
	_, port, err = net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return ln, addr, port
}

// agentInstall is what an admin reads from a new agent environment: its ID and the install
// command's join token, address and pin.
type agentInstall struct {
	ID        string
	JoinToken string
	AgentURL  string
	Pin       string
}

// createAgentEnv adds an agent environment through the API as the signed-in admin.
func (r *acmeRig) createAgentEnv(client *http.Client, cookie *http.Cookie, csrf string) agentInstall {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodPost, r.base+"/api/environments", strings.NewReader(`{"kind":"agent"}`))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		r.t.Fatalf("POST /api/environments: %d %s", resp.StatusCode, body)
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
		r.t.Fatal(err)
	}
	return agentInstall{ID: created.ID, JoinToken: created.Install.JoinToken, AgentURL: created.Install.AgentURL, Pin: created.Install.Pin}
}

// agentProc is a running agent binary and what it printed.
type agentProc struct {
	log    lockedBuffer
	exited chan struct{}
	stop   context.CancelFunc
}

// startAgentProc runs the agent binary with exactly env as its environment. It is stopped again
// at the end of the test.
func startAgentProc(t *testing.T, bin string, env []string) *agentProc {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	p := &agentProc{exited: make(chan struct{}), stop: stop}
	cmd := exec.CommandContext(ctx, bin) //nolint:gosec // the binary this test just built
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &p.log, &p.log
	if err := cmd.Start(); err != nil {
		stop()
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	t.Cleanup(p.halt)
	return p
}

// halt kills the agent and waits for the process to end.
func (p *agentProc) halt() {
	p.stop()
	<-p.exited
}

// agentEnvOptions says which of the install command's settings an agent process is given.
type agentEnvOptions struct {
	install  agentInstall
	socket   string
	stateDir string
	roots    string // SSL_CERT_FILE: the issuer standing in for the public roots
	withPin  bool
	noToken  bool
}

// agentEnv is the environment of an agent process. Its short backoff makes a reconnect quick.
func agentEnv(o agentEnvOptions) []string {
	env := []string{
		"TIERPLAN_AGENT_URL=" + o.install.AgentURL,
		"TIERPLAN_AGENT_SOCKET=" + o.socket,
		"TIERPLAN_AGENT_STATE_DIR=" + o.stateDir,
		"TIERPLAN_AGENT_BACKOFF_MIN=200ms",
		"TIERPLAN_AGENT_BACKOFF_MAX=1s",
		"PATH=" + os.Getenv("PATH"),
	}
	if o.roots != "" {
		env = append(env, "SSL_CERT_FILE="+o.roots)
	}
	if !o.noToken {
		env = append(env, "TIERPLAN_AGENT_JOIN_TOKEN="+o.install.JoinToken)
	}
	if o.withPin {
		env = append(env, "TIERPLAN_AGENT_PIN="+o.install.Pin)
	}
	return env
}

// awaitSessionEnded waits until the server has no session of the agent of envID, as after the
// agent's process was killed.
func awaitSessionEnded(t *testing.T, srv *Server, envID string) {
	t.Helper()
	testsupport.Poll(t, testsupport.Deadline, "the server to end the killed agent's session", func() (bool, string) { // testsupport: external, the kernel closing the killed agent's connection
		return sessionOfEnv(srv, envID) == nil, "the session is still registered"
	})
}

// pinsFileEntry is one entry of the agent's pins.json.
type pinsFileEntry struct {
	Pin       string    `json:"pin"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// awaitStoredPin waits until the agent's pins.json in dir holds pin and returns that entry. The
// agent writes the file after the welcome that listed the pin, in another process.
func awaitStoredPin(t *testing.T, dir, pin string) pinsFileEntry {
	t.Helper()
	var found pinsFileEntry
	testsupport.Poll(t, testsupport.Deadline, "the agent to store the self-signed pin", func() (bool, string) { // testsupport: external, the agent process writing pins.json
		raw, err := os.ReadFile(filepath.Join(dir, "pins.json")) //nolint:gosec // the agent's own state directory
		if err != nil {
			return false, err.Error()
		}
		var entries []pinsFileEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return false, err.Error()
		}
		for _, e := range entries {
			if e.Pin == pin {
				found = e
				return true, ""
			}
		}
		return false, "pins.json holds " + string(raw)
	})
	return found
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
	agentBin := buildAgentBinary(t)

	// The listener exists before Build, so the public URL agents are told to dial is known. The
	// self-signed certificate covers localhost, and the test-only directory lets the fake issue for it.
	ln, addr, port := listenLoopback(t)
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
	created := r.createAgentEnv(client, cookie, csrf)
	if created.Pin != installPin || created.JoinToken == "" {
		t.Fatalf("install = %+v, want the join token and the self-signed pin %s", created, installPin)
	}

	// The agent starts with the install pin. Its system roots are the fake CA's issuer, standing in for
	// the public roots a real agent has.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "acme-host"})
	agent := startAgentProc(t, agentBin, agentEnv(agentEnvOptions{
		install: created, socket: fake.SocketPath(), stateDir: t.TempDir(), roots: r.fake.IssuerFile(t), withPin: true,
	}))
	agentLog, exited := &agent.log, agent.exited
	awaitAgentOnline(t, srv, created.ID, exited, agentLog, true)

	// Let's Encrypt is issued, and the server restarts on the same address: the agent reconnects to a
	// certificate its pin does not match but its roots cover.
	r.issue()
	first := leafOf(t, srv.https.Config())
	srv = r.restart(addr)
	if srv.https.Kind() != tlsKindACME {
		t.Fatalf("after the restart the kind is %d, want tlsKindACME", srv.https.Kind())
	}
	awaitAgentOnline(t, srv, created.ID, exited, agentLog, true)

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
	awaitAgentOnline(t, srv, created.ID, exited, agentLog, true)
	if strings.Contains(agentLog.String(), "tls_untrusted") {
		t.Errorf("the agent reported an untrusted certificate:\n%s", agentLog.String())
	}
}

// D-20 steps 1 to 3 with the real agent binary: an agent enrolled with the self-signed pin stays
// connected while the running server switches to a Let's Encrypt certificate that has a full chain
// (root, intermediate, leaf), reconnects after its own restart, and still holds the self-signed pin
// with no deadline, because every welcome keeps listing it (D-19, TLS-08).
func TestACMEAgentStaysConnectedThroughALiveSwap(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	agentBin := buildAgentBinary(t)
	ln, _, port := listenLoopback(t)
	r := newACMERig(t, acmetest.Options{Intermediate: true}, map[string]string{"TIERPLAN_PUBLIC_URL": "https://localhost:" + port})
	srv := r.start(ln)
	selfPin := srv.https.Pin()
	if selfPin == "" {
		t.Fatal("the self-signed certificate publishes no pin")
	}

	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)
	created := r.createAgentEnv(client, cookie, csrf)
	if created.Pin != selfPin {
		t.Fatalf("the install pin is %q, want the self-signed pin %q", created.Pin, selfPin)
	}

	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "acme-host"})
	stateDir := t.TempDir()
	env := agentEnv(agentEnvOptions{install: created, socket: fake.SocketPath(), stateDir: stateDir, roots: r.fake.IssuerFile(t), withPin: true})
	agent := startAgentProc(t, agentBin, env)
	session := awaitAgentOnline(t, srv, created.ID, agent.exited, &agent.log, true)

	// The running server switches to the Let's Encrypt certificate (acmeInstaller.ServeACME ->
	// rt.Install). The agent's connection is not dropped by the swap.
	r.issue()
	if srv.https.Kind() != tlsKindACME {
		t.Fatalf("after the issue the kind is %d, want tlsKindACME", srv.https.Kind())
	}
	if got := sessionOfEnv(srv, created.ID); got != session {
		t.Fatalf("the agent's session after the swap is %v, want the one it held before", got)
	}
	// Welcomes still list the self-signed pin, and a new install command carries none. The client
	// verifies the served chain through the intermediate the server sends.
	if got := srv.Tunnel().Pins(); !slices.Equal(got, []string{selfPin}) {
		t.Fatalf("welcome pins under Let's Encrypt = %v, want [%s]", got, selfPin)
	}
	swapped := r.client(r.fake.IssuerPool())
	if again := r.createAgentEnv(swapped, cookie, csrf); again.Pin != "" {
		t.Fatalf("an install command under Let's Encrypt carries the pin %q, want none", again.Pin)
	}

	// The agent restarts on the same state directory and finds the public certificate. The
	// intermediate comes from the server, the root from the agent's system roots.
	agent.halt()
	awaitSessionEnded(t, srv, created.ID)
	agent = startAgentProc(t, agentBin, env)
	awaitAgentOnline(t, srv, created.ID, agent.exited, &agent.log, true)
	if strings.Contains(agent.log.String(), "tls_untrusted") {
		t.Errorf("the agent reported an untrusted certificate:\n%s", agent.log.String())
	}

	// D-20 step 3, structurally: the self-signed pin is stored with no deadline.
	if e := awaitStoredPin(t, stateDir, selfPin); !e.ExpiresAt.IsZero() {
		t.Errorf("the self-signed pin has the deadline %s, want none while welcomes list it", e.ExpiresAt)
	}
}

// awaitAgentLog waits until the agent process has printed want.
func awaitAgentLog(t *testing.T, p *agentProc, want string) {
	t.Helper()
	testsupport.Poll(t, testsupport.Deadline, "the agent to print "+want, func() (bool, string) { // testsupport: external, the agent process writing its log
		out := p.log.String()
		return strings.Contains(out, want), "the agent's output so far:\n" + out
	})
}

// writeRootFile writes the root as PEM to a file in the test's temporary directory, for an agent's
// SSL_CERT_FILE.
func writeRootFile(t *testing.T, root pkiCert) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// D-20 pinless: an agent installed with no pin after Let's Encrypt was issued verifies the public
// chain (root, intermediate, leaf) against its system roots, stores the self-signed pin from its first
// welcome with no deadline, and survives the expiry fallback to self-signed with no tls_untrusted.
func TestACMEAgentPinlessInstallSurvivesTheFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	agentBin := buildAgentBinary(t)
	ln, addr, port := listenLoopback(t)
	r := newACMERig(t, acmetest.Options{Intermediate: true}, map[string]string{"TIERPLAN_PUBLIC_URL": "https://localhost:" + port})
	srv := r.start(ln)
	selfPin := srv.https.Pin()
	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)

	r.issue()
	first := leafOf(t, srv.https.Config())
	created := r.createAgentEnv(r.client(r.fake.IssuerPool()), cookie, csrf)
	if created.Pin != "" || created.JoinToken == "" {
		t.Fatalf("install = %+v, want the join token and no pin under Let's Encrypt", created)
	}

	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "acme-host"})
	stateDir := t.TempDir()
	agent := startAgentProc(t, agentBin, agentEnv(agentEnvOptions{install: created, socket: fake.SocketPath(), stateDir: stateDir, roots: r.fake.IssuerFile(t)}))
	awaitAgentOnline(t, srv, created.ID, agent.exited, &agent.log, true)
	if e := awaitStoredPin(t, stateDir, selfPin); !e.ExpiresAt.IsZero() {
		t.Errorf("the self-signed pin has the deadline %s, want none", e.ExpiresAt)
	}

	// Renewals fail and the certificate reaches its end: the server falls back to self-signed and,
	// restarted, serves it to an agent that has only the pin it stored.
	r.fake.FailValidation(r.host, acmetest.Problem{Status: 403, Type: "unauthorized", Detail: "scripted"})
	r.step(first.NotAfter.Sub(r.clock.Now()) + time.Hour)
	r.awaitRuns()
	if srv.https.Kind() != tlsKindSelfSigned {
		t.Fatalf("after the expiry the kind is %d, want the self-signed certificate", srv.https.Kind())
	}
	srv = r.restart(addr)
	if srv.https.Kind() != tlsKindSelfSigned || srv.https.Pin() != selfPin {
		t.Fatalf("after the restart kind = %d and pin = %q, want the self-signed certificate with pin %q", srv.https.Kind(), srv.https.Pin(), selfPin)
	}
	awaitAgentOnline(t, srv, created.ID, agent.exited, &agent.log, true)
	if strings.Contains(agent.log.String(), "tls_untrusted") {
		t.Errorf("the agent reported an untrusted certificate:\n%s", agent.log.String())
	}
}

// The incomplete-chain pitfall: a certificate served without its intermediate cannot be verified by an
// agent that holds only the root, so a pinless agent is refused with tls_untrusted. The self-signed pin
// does not cover the Let's Encrypt leaf either, so an agent started with it is refused as well, while
// the one that was already connected keeps its session.
func TestACMEAgentLeafOnlyChainIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	agentBin := buildAgentBinary(t)
	ln, _, port := listenLoopback(t)
	r := newACMERig(t, acmetest.Options{Intermediate: true, LeafOnly: true}, map[string]string{"TIERPLAN_PUBLIC_URL": "https://localhost:" + port})
	srv := r.start(ln)
	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "acme-host"})
	roots := r.fake.IssuerFile(t)

	// A pinned agent is connected before the swap.
	pinned := r.createAgentEnv(client, cookie, csrf)
	pinnedAgent := startAgentProc(t, agentBin, agentEnv(agentEnvOptions{install: pinned, socket: fake.SocketPath(), stateDir: t.TempDir(), roots: roots, withPin: true}))
	session := awaitAgentOnline(t, srv, pinned.ID, pinnedAgent.exited, &pinnedAgent.log, true)

	r.issue()
	if srv.https.Kind() != tlsKindACME {
		t.Fatalf("after the issue the kind is %d, want tlsKindACME", srv.https.Kind())
	}
	if got := sessionOfEnv(srv, pinned.ID); got != session {
		t.Fatalf("the pinned agent's session after the swap is %v, want the one it held before", got)
	}

	// The leaf alone is not a chain to the root, so the API client trusts the leaf itself to add the
	// pinless environment.
	leafPool := x509.NewCertPool()
	leafPool.AddCert(leafOf(t, srv.https.Config()))
	pinless := r.createAgentEnv(r.client(leafPool), cookie, csrf)
	if pinless.Pin != "" {
		t.Fatalf("install pin = %q, want none under Let's Encrypt", pinless.Pin)
	}
	agent := startAgentProc(t, agentBin, agentEnv(agentEnvOptions{install: pinless, socket: fake.SocketPath(), stateDir: t.TempDir(), roots: roots}))
	awaitAgentLog(t, agent, "tls_untrusted")
	if sessionOfEnv(srv, pinless.ID) != nil {
		t.Error("the pinless agent has a session although it could not build the chain")
	}

	// The same chain refuses a restarted pinned agent: its pin is the self-signed key's.
	pinnedAgent.halt()
	awaitSessionEnded(t, srv, pinned.ID)
	again := startAgentProc(t, agentBin, agentEnv(agentEnvOptions{install: pinned, socket: fake.SocketPath(), stateDir: t.TempDir(), roots: roots, withPin: true}))
	awaitAgentLog(t, again, "tls_untrusted")
	if sessionOfEnv(srv, pinned.ID) != nil {
		t.Error("the restarted pinned agent has a session although the chain is incomplete")
	}
}

// D-21: a server started with a certificate pair welcomes the self-signed pin it does not serve. A
// pinless agent verifies the pair through its roots, stores the pin, and when the server is restarted
// on the same data directory without the two variables it finds the self-signed certificate and
// reconnects with no tls_untrusted.
func TestACMEAgentCertFilesRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	agentBin := buildAgentBinary(t)
	ln, addr, port := listenLoopback(t)
	root := pkiCA(t, "agent test root", nil, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &root, pkiFrom, pkiTo, "localhost")
	crt, key := pkiWritePair(t, leaf)
	r := newACMERig(t, acmetest.Options{}, map[string]string{
		"TIERPLAN_PUBLIC_URL":    "https://localhost:" + port,
		"TIERPLAN_TLS_CERT_FILE": crt,
		"TIERPLAN_TLS_KEY_FILE":  key,
	})
	srv := r.start(ln)
	if srv.https.Kind() != tlsKindSupplied {
		t.Fatalf("the kind is %d, want the supplied pair", srv.https.Kind())
	}
	pins := srv.Tunnel().Pins()
	if len(pins) != 1 || pins[0] == "" {
		t.Fatalf("welcome pins with a certificate pair = %v, want the self-signed pin it does not serve", pins)
	}
	selfPin := pins[0]

	client := r.client(pkiPool(root))
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)
	created := r.createAgentEnv(client, cookie, csrf)
	if created.Pin != "" || created.JoinToken == "" {
		t.Fatalf("install = %+v, want the join token and no pin with a certificate pair", created)
	}

	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "acme-host"})
	stateDir := t.TempDir()
	agent := startAgentProc(t, agentBin, agentEnv(agentEnvOptions{install: created, socket: fake.SocketPath(), stateDir: stateDir, roots: writeRootFile(t, root)}))
	awaitAgentOnline(t, srv, created.ID, agent.exited, &agent.log, true)
	if e := awaitStoredPin(t, stateDir, selfPin); !e.ExpiresAt.IsZero() {
		t.Errorf("the self-signed pin has the deadline %s, want none", e.ExpiresAt)
	}

	// The variables are removed and the server restarts: the self-signed certificate is served again,
	// with the same key, and only the stored pin covers it.
	r.setEnv(map[string]string{"TIERPLAN_TLS_CERT_FILE": "", "TIERPLAN_TLS_KEY_FILE": ""})
	srv = r.restart(addr)
	if srv.https.Kind() != tlsKindSelfSigned || srv.https.Pin() != selfPin {
		t.Fatalf("after the restart kind = %d and pin = %q, want the self-signed certificate with pin %q", srv.https.Kind(), srv.https.Pin(), selfPin)
	}
	awaitAgentOnline(t, srv, created.ID, agent.exited, &agent.log, true)
	if strings.Contains(agent.log.String(), "tls_untrusted") {
		t.Errorf("the agent reported an untrusted certificate:\n%s", agent.log.String())
	}
}
