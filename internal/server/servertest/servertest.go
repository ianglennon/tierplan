// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package servertest runs the real TierPlan server inside a test process, on an
// address containers can reach, with an admin signed in. It exists for the
// agent tests that run real agents in dind nodes: the node dials the address in
// the install command, so the server cannot be on loopback.
//
// Nothing in the product imports it.
package servertest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/server"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

const adminPassword = "correct horse battery"

// Options configure Start.
type Options struct {
	// Host is the address the dind nodes reach this machine on: the gateway of
	// the network they share (dockertest.Swarm.Gateway or Node.Gateway). It
	// becomes the host of the public URL, which agents are told to dial and
	// which the self-signed certificate covers. Required.
	Host string
	// Env is extra TIERPLAN_* settings, applied over the defaults.
	Env map[string]string
	// Version is the version the server reports, as a release build would: it sets
	// buildinfo.Version for this test process before the server is built, and puts
	// the previous value back when the test ends. Empty keeps whatever is set, which
	// in a test binary is a development build. Servers in one process must not run
	// at the same time with different versions, so a test that sets it is not parallel.
	Version string
	// AddressSpaceClock moves the clock of the address-space reader only, for a test that
	// needs networks to be past the orphan grace period at once (server.WithAddressSpaceClock).
	// Nil keeps real time. It is kept across Restart.
	AddressSpaceClock func() time.Time
	// JanitorClock moves the clock a janitor run judges ages by, for a test that needs containers to
	// be past the grace period at once (server.WithJanitorClock). Nil keeps real time. It is kept
	// across Restart.
	JanitorClock stackops.Clock
	// JanitorSchedule lets the janitor's scheduler run, on JanitorClock when that is set. By default it
	// does not, so that no test depends on the time of day it runs at (server.WithoutJanitorScheduler).
	JanitorSchedule bool
}

// Server is a running TierPlan with an admin signed in.
type Server struct {
	// URL is the public URL, https://<host>:<port>.
	URL string
	// DataDir is the server's data directory.
	DataDir string

	t      testing.TB
	srv    *server.Server
	http   *http.Client
	cookie *http.Cookie
	csrf   string

	// env is the TIERPLAN_* settings the server was built from, kept so Restart builds
	// the same one on the same address. cancel and done stop the running server.
	env    map[string]string
	opts   []server.Option
	cancel context.CancelFunc
	done   chan error
}

// Start builds the real server on 0.0.0.0:<free port> with a generated
// self-signed certificate and a public URL of https://<Host>:<port>, serves it
// until the test ends, and signs in a throwaway admin. It listens on every
// interface only for the test's lifetime, on a random port, behind a random
// password that is never printed (T-02-77).
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	if net.ParseIP(o.Host) == nil {
		t.Fatalf("servertest.Start: Host %q is not an IP address", o.Host)
	}
	if o.Version != "" {
		// Registered before the server's own cleanup, so it runs after the server
		// has stopped: the server reads the version while it runs.
		prev := buildinfo.Version
		buildinfo.Version = o.Version
		t.Cleanup(func() { buildinfo.Version = prev })
	}

	dataDir := filepath.Join(t.TempDir(), "data")
	s := &Server{DataDir: dataDir, t: t}
	if o.AddressSpaceClock != nil {
		s.opts = append(s.opts, server.WithAddressSpaceClock(o.AddressSpaceClock))
	}
	if o.JanitorClock != nil {
		s.opts = append(s.opts, server.WithJanitorClock(o.JanitorClock))
	}
	if !o.JanitorSchedule {
		s.opts = append(s.opts, server.WithoutJanitorScheduler())
	}
	// The port has to be known before Build (it is in the public URL), so one is
	// picked by binding and releasing it. The window is small; a taken port makes
	// Run fail and the next try picks another.
	for attempt := 0; ; attempt++ {
		ln, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // reachable from the dind nodes on purpose, for this test only (T-02-77)
		if err != nil {
			t.Fatalf("pick a port: %v", err)
		}
		addr, ok := ln.Addr().(*net.TCPAddr)
		_ = ln.Close()
		if !ok {
			t.Fatalf("pick a port: %v is not a TCP address", ln.Addr())
		}
		port := addr.Port
		s.URL = "https://" + net.JoinHostPort(o.Host, strconv.Itoa(port))

		s.env = map[string]string{
			"TIERPLAN_DATA_DIR":        dataDir,
			"TIERPLAN_LISTEN":          "0.0.0.0:" + strconv.Itoa(port),
			"TIERPLAN_PUBLIC_URL":      s.URL,
			"TIERPLAN_TLS_SELF_SIGNED": "true",
		}
		for k, v := range o.Env {
			s.env[k] = v
		}
		if s.boot() {
			break
		}
		if attempt >= 4 {
			t.Fatal("servertest: the server could not listen on any port it tried")
		}
		_ = os.RemoveAll(dataDir) // Run has closed the server
	}
	t.Cleanup(s.stop)

	s.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: s.trustServerCertificate(),
	}}}
	t.Cleanup(s.http.CloseIdleConnections)
	s.waitReady()
	s.signIn()
	return s
}

// boot builds the server from s.env and starts serving. It reports false when Run
// returned before the server bound its address, which is what a port that was taken
// in the meantime looks like; any other failure ends the test. The server says that
// it is serving by closing Listening, so no timer decides.
func (s *Server) boot() bool {
	s.t.Helper()
	fl := pflag.NewFlagSet("servertest", pflag.ContinueOnError)
	config.BindFlags(fl)
	cfg, err := config.Load(fl, func(k string) string { return s.env[k] }, os.ReadFile)
	if err != nil {
		s.t.Fatalf("servertest config: %v", err)
	}
	srv, err := server.Build(context.Background(), cfg, serverLog(), s.opts...)
	if err != nil {
		s.t.Fatalf("build the server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	// Run returns with its error when it cannot listen; otherwise it closes Listening.
	select {
	case <-done:
		cancel()
		return false
	case <-srv.Listening():
	case <-testsupport.Context(s.t).Done():
		cancel()
		s.t.Fatal("servertest: the server neither listened nor failed to listen")
	}
	s.srv, s.cancel, s.done = srv, cancel, done
	return true
}

// stop shuts the running server down and waits for it. It is safe to call twice.
func (s *Server) stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(testsupport.Deadline):
		s.t.Errorf("servertest: the server did not shut down within %v", testsupport.Deadline)
	}
	s.cancel = nil
}

// Restart stops the server and starts another on the same address, with the same
// data directory, certificate and settings, so everything that dials it (the agents)
// finds it again, as after an upgrade. A non-empty version is set as buildinfo.Version
// first, which is how a test upgrades the server from one release to the next; empty
// keeps the version. It signs the admin in again, since the cookie of the old run is
// not what the new one hands out.
func (s *Server) Restart(version string) {
	s.t.Helper()
	s.stop()
	if version != "" {
		buildinfo.Version = version
	}
	// The listening port is released as the old server stops; a kernel that has not
	// finished with it makes Run fail to listen, so boot is retried until it can.
	testsupport.Poll(s.t, testsupport.Deadline, "the restarted server to listen on its old address", func() (bool, string) { // testsupport: external, the kernel releasing the old listening port
		if s.boot() {
			return true, ""
		}
		return false, "the address was still taken"
	})
	s.waitReady()
	s.login()
}

// serverLog is where the server under test writes its log: nowhere, unless
// TIERPLAN_TEST_SERVER_LOG is set, which sends it to stderr at debug level for
// someone working out why a real-engine run did what it did.
func serverLog() *slog.Logger {
	if os.Getenv("TIERPLAN_TEST_SERVER_LOG") == "" {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Row is the environment's database row: the swarm cluster ID it is bound to,
// the engine ID, and the pending cluster after a re-initialisation.
func (s *Server) Row(id string) sqlc.Environment {
	s.t.Helper()
	row, err := s.srv.Store().RQ.GetEnvironment(context.Background(), id)
	if err != nil {
		s.t.Fatalf("read environment %s: %v", id, err)
	}
	return row
}

// AgentRow is the agent's database row, for what the API leaves out, such as who
// revoked it.
func (s *Server) AgentRow(id string) sqlc.Agent {
	s.t.Helper()
	row, err := s.srv.Store().RQ.GetAgent(context.Background(), id)
	if err != nil {
		s.t.Fatalf("read agent %s: %v", id, err)
	}
	return row
}

// RefreshCluster reads the environment's cluster now, the way the server does every
// 15 seconds, so that a test knows which manager the server is using before it stops
// one. It fails the test when the read fails.
func (s *Server) RefreshCluster(id string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.srv.Clusters().Refresh(ctx, id); err != nil {
		s.t.Fatalf("read the cluster of %s: %v", id, err)
	}
}

// Core is the server, for a test that needs something the HTTP API does not say.
func (s *Server) Core() *server.Server { return s.srv }

// trustServerCertificate is a root pool holding the server's own generated
// certificate, which names the host, so the client verifies the connection
// normally.
func (s *Server) trustServerCertificate() *x509.CertPool {
	raw, err := os.ReadFile(filepath.Join(s.DataDir, "tls", "selfsigned.crt"))
	if err != nil {
		s.t.Fatalf("read the server's self-signed certificate: %v", err)
	}
	pool := x509.NewCertPool()
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			s.t.Fatalf("parse the server's certificate: %v", err)
		}
		pool.AddCert(cert)
	}
	return pool
}

// waitReady reads /readyz once. boot has returned only after the server closed
// Listening, and Build had already made the data directory writable and brought the
// store to this binary's schema, so the answer is 200 straight away.
func (s *Server) waitReady() {
	s.t.Helper()
	resp, err := s.http.Get(s.URL + "/readyz")
	if err != nil {
		s.t.Fatalf("the server is not ready at %s: %v", s.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("the server is not ready at %s: status %d %s", s.URL, resp.StatusCode, body)
	}
}

// signIn creates the admin straight in the store and logs in over HTTPS.
func (s *Server) signIn() {
	s.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword(adminPassword)
	if err != nil {
		s.t.Fatal(err)
	}
	st := s.srv.Store()
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	if err := st.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		s.t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, st.Q, "u1"); err != nil {
		s.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatal(err)
	}
	s.login()
}

// login signs the admin in over HTTPS and keeps the session cookie and CSRF token.
func (s *Server) login() {
	s.t.Helper()
	s.cookie, s.csrf = nil, ""
	resp, err := s.http.Post(s.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"`+adminPassword+`"}`))
	if err != nil {
		s.t.Fatalf("log in: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 {
		s.t.Fatalf("log in: status %d", resp.StatusCode)
	}
	s.cookie = resp.Cookies()[0]
	code, body := s.Do(http.MethodGet, "/api/me", nil)
	var me struct {
		CSRFToken string `json:"csrfToken"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &me) != nil || me.CSRFToken == "" {
		s.t.Fatalf("GET /api/me: %d %s", code, body)
	}
	s.csrf = me.CSRFToken
}

// Client is the HTTP client that trusts the server's certificate, for a test that
// has to open something Do cannot, such as a WebSocket or an event stream. It has a
// timeout of its own: copy it and clear Timeout for a connection meant to stay open.
func (s *Server) Client() *http.Client { return s.http }

// Cookie is the session cookie of the signed-in admin, for the same tests.
func (s *Server) Cookie() *http.Cookie { return s.cookie }

// Do calls the API as the admin. A non-nil body is sent as JSON.
func (s *Server) Do(method, path string, body any) (int, []byte) {
	s.t.Helper()
	code, b, err := s.do(method, path, body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, b
}

func (s *Server) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.URL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cookie != nil {
		req.AddCookie(s.cookie)
	}
	if s.csrf != "" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// Install is the install object POST /api/environments answers for an agent
// environment, trimmed to what the tests run.
type Install struct {
	JoinToken  string `json:"joinToken"`
	AgentURL   string `json:"agentUrl"`
	Image      string `json:"image"`
	Pin        string `json:"pin"`
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
	ManualCleanup struct {
		Swarm      string `json:"swarm"`
		Standalone string `json:"standalone"`
	} `json:"manualCleanup"`
}

// Rotated is the answer to POST /api/environments/{id}/token.
type Rotated struct {
	Install   Install `json:"install"`
	EnvType   string  `json:"envType"`
	HasAgents bool    `json:"hasAgents"`
}

// RotateToken makes a new join token and retires the current one.
func (s *Server) RotateToken(id string) Rotated {
	s.t.Helper()
	code, body := s.Do(http.MethodPost, "/api/environments/"+id+"/token", nil)
	var r Rotated
	if code != http.StatusOK || json.Unmarshal(body, &r) != nil {
		s.t.Fatalf("POST token: %d %s", code, body)
	}
	return r
}

// Created is an environment POST /api/environments created.
type Created struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Install Install `json:"install"`
}

// CreateAgentEnvironment adds an agent environment. An empty name leaves the
// naming to the first agent that connects (D-23).
func (s *Server) CreateAgentEnvironment(name string) Created {
	s.t.Helper()
	in := map[string]any{"kind": "agent"}
	if name != "" {
		in["name"] = name
	}
	code, body := s.Do(http.MethodPost, "/api/environments", in)
	if code != http.StatusCreated {
		s.t.Fatalf("POST /api/environments kind agent: %d %s", code, body)
	}
	var c Created
	if err := json.Unmarshal(body, &c); err != nil || c.ID == "" {
		s.t.Fatalf("created environment: %v: %s", err, body)
	}
	return c
}

// Issue is one entry of an environment's status issues.
type Issue struct {
	Code string `json:"code"`
	Node string `json:"node"`
}

// Environment is one item of GET /api/environments.
type Environment struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	EnvType string `json:"envType"`
	Type    string `json:"type"`
	Status  struct {
		State  string  `json:"state"`
		Reason string  `json:"reason"`
		Issues []Issue `json:"issues"`
	} `json:"status"`
	Engine struct {
		Version    string `json:"version"`
		APIVersion string `json:"apiVersion"`
	} `json:"engine"`
	Nodes *struct {
		Ready int `json:"ready"`
		Total int `json:"total"`
	} `json:"nodes"`
	Agents *struct {
		Connected int `json:"connected"`
		Expected  int `json:"expected"`
	} `json:"agents"`
	Failing *struct {
		Failing int `json:"failing"`
		Total   int `json:"total"`
	} `json:"failingServices"`
	AgentVersion     *string `json:"agentVersion"`
	PendingClusterID string  `json:"pendingClusterId"`
}

// HasIssue reports whether the status lists an issue with this code.
func (e Environment) HasIssue(code string) bool {
	for _, i := range e.Status.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// Environment reads one environment from the list.
func (s *Server) Environment(id string) (Environment, bool) {
	s.t.Helper()
	code, body, err := s.do(http.MethodGet, "/api/environments", nil)
	if err != nil || code != http.StatusOK {
		return Environment{}, false
	}
	var list struct {
		Items []Environment `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return Environment{}, false
	}
	for _, e := range list.Items {
		if e.ID == id {
			return e, true
		}
	}
	return Environment{}, false
}

// Summary is GET /api/env/{id}/summary.
type Summary struct {
	Counts struct {
		Containers        int  `json:"containers"`
		ContainersRunning int  `json:"containersRunning"`
		Nodes             *int `json:"nodes"`
		Services          *int `json:"services"`
		Images            int  `json:"images"`
		Networks          int  `json:"networks"`
		Volumes           int  `json:"volumes"`
	} `json:"counts"`
	Engine struct {
		Version string `json:"version"`
	} `json:"engine"`
	IsWorker     bool     `json:"isWorker"`
	NotReporting []string `json:"notReporting"`
	ByNode       []struct {
		Node       string `json:"node"`
		Containers int    `json:"containers"`
		Images     int    `json:"images"`
		Volumes    int    `json:"volumes"`
		Networks   int    `json:"networks"`
	} `json:"byNode"`
}

// Summary reads the dashboard summary through the tunnel. The status code is
// returned with it, so a test can assert a refusal.
func (s *Server) Summary(id string) (Summary, int, []byte) {
	s.t.Helper()
	code, body, err := s.do(http.MethodGet, "/api/env/"+id+"/summary", nil)
	if err != nil {
		return Summary{}, 0, nil
	}
	var sum Summary
	_ = json.Unmarshal(body, &sum)
	return sum, code, body
}

// Container is one row of GET /api/env/{id}/containers.
type Container struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"`
	Node  string `json:"node"`
}

// ContainerList is GET /api/env/{id}/containers.
type ContainerList struct {
	Items        []Container `json:"items"`
	Total        int         `json:"total"`
	Truncated    bool        `json:"truncated"`
	Nodes        []string    `json:"nodes"`
	NotReporting []string    `json:"notReporting"`
}

// Containers lists the containers of an environment. query is a raw query
// string without the question mark, such as "node=tp-w1".
func (s *Server) Containers(id, query string) (ContainerList, int, []byte) {
	s.t.Helper()
	path := "/api/env/" + id + "/containers"
	if query != "" {
		path += "?" + query
	}
	code, body, err := s.do(http.MethodGet, path, nil)
	if err != nil {
		return ContainerList{}, 0, nil
	}
	var l ContainerList
	_ = json.Unmarshal(body, &l)
	return l, code, body
}

// Agent is one row of GET /api/env/{id}/agents.
type Agent struct {
	ID                 string `json:"id"`
	Node               string `json:"node"`
	Role               string `json:"role"`
	Status             string `json:"status"`
	Connected          bool   `json:"connected"`
	Blocked            bool   `json:"blocked"`
	AgentVersion       string `json:"agentVersion"`
	EngineVersion      string `json:"engineVersion"`
	CertFingerprint    string `json:"certFingerprint"`
	CertNotAfter       string `json:"certNotAfter"`
	ClockOffsetSeconds int    `json:"clockOffsetSeconds"`
	RevokedAt          string `json:"revokedAt"`
	RevokeReason       string `json:"revokeReason"`
	UpdateState        struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	} `json:"updateState"`
}

// Agents lists the agents of an environment.
func (s *Server) Agents(id string) []Agent {
	s.t.Helper()
	code, body, err := s.do(http.MethodGet, "/api/env/"+id+"/agents", nil)
	if err != nil || code != http.StatusOK {
		return nil
	}
	var l struct {
		Items []Agent `json:"items"`
	}
	_ = json.Unmarshal(body, &l)
	return l.Items
}

// WaitFor polls cond until it is true and fails the test with the last observation
// when timeout passes first. what names what is awaited. It is for conditions the
// test does not control, such as a real agent in a dind node.
func (s *Server) WaitFor(what string, timeout time.Duration, cond func() (bool, string)) {
	s.t.Helper()
	testsupport.Poll(s.t, timeout, what, cond)
}

// WaitOnline waits for the environment to be online with the engine version
// read through the tunnel, and returns it as the list then showed it.
func (s *Server) WaitOnline(id string, timeout time.Duration) Environment {
	s.t.Helper()
	var got Environment
	s.WaitFor("environment "+id+" to be online with an engine version", timeout, func() (bool, string) {
		e, ok := s.Environment(id)
		got = e
		return ok && e.Status.State == "online" && e.Engine.Version != "", fmt.Sprintf("%+v", e.Status)
	})
	return got
}

// AuditRow is one row of the audit log, as far as a test needs.
type AuditRow struct {
	ActorID  string
	TargetID string
	Result   string
	Reason   string
	Details  map[string]string
}

// AuditRows lists the audit rows of an action, oldest first.
func (s *Server) AuditRows(action string) []AuditRow {
	s.t.Helper()
	rows, err := s.srv.Store().Read.Query(
		`SELECT actor_id, target_id, result, reason, details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		s.t.Fatalf("read the audit log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var details string
		if err := rows.Scan(&r.ActorID, &r.TargetID, &r.Result, &r.Reason, &details); err != nil {
			s.t.Fatalf("read the audit log: %v", err)
		}
		_ = json.Unmarshal([]byte(details), &r.Details)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatalf("read the audit log: %v", err)
	}
	return out
}
