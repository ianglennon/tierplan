// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package servertest runs the real Bayplan server inside a test process, on an
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

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/server"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

const adminPassword = "correct horse battery"

// Options configure Start.
type Options struct {
	// Host is the address the dind nodes reach this machine on: the gateway of
	// the network they share (dockertest.Swarm.Gateway or Node.Gateway). It
	// becomes the host of the public URL, which agents are told to dial and
	// which the self-signed certificate covers. Required.
	Host string
	// Env is extra BAYPLAN_* settings, applied over the defaults.
	Env map[string]string
}

// Server is a running Bayplan with an admin signed in.
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

	dataDir := filepath.Join(t.TempDir(), "data")
	var (
		srv     *server.Server
		port    int
		publicU string
	)
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
		port = addr.Port
		publicU = "https://" + net.JoinHostPort(o.Host, strconv.Itoa(port))

		env := map[string]string{
			"BAYPLAN_DATA_DIR":        dataDir,
			"BAYPLAN_LISTEN":          "0.0.0.0:" + strconv.Itoa(port),
			"BAYPLAN_PUBLIC_URL":      publicU,
			"BAYPLAN_TLS_SELF_SIGNED": "true",
		}
		for k, v := range o.Env {
			env[k] = v
		}
		fl := pflag.NewFlagSet("servertest", pflag.ContinueOnError)
		config.BindFlags(fl)
		cfg, err := config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
		if err != nil {
			t.Fatalf("servertest config: %v", err)
		}
		srv, err = server.Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("build the server: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- srv.Run(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Error("servertest: the server did not shut down within 20s")
			}
		})
		// Run returns at once when it cannot listen; a server that is serving does not.
		select {
		case err := <-done:
			cancel()
			if attempt >= 4 {
				t.Fatalf("servertest: the server stopped at start: %v", err)
			}
			_ = os.RemoveAll(dataDir) // Run has closed the server
			done <- nil               // the cleanup above reads one value
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}

	s := &Server{URL: publicU, DataDir: dataDir, t: t, srv: srv}
	s.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: s.trustServerCertificate(),
	}}}
	t.Cleanup(s.http.CloseIdleConnections)
	s.waitReady()
	s.signIn()
	return s
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

func (s *Server) waitReady() {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := s.http.Get(s.URL + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the server never became ready at %s: %v", s.URL, err)
		}
		time.Sleep(100 * time.Millisecond)
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
	if err := rbac.AssignGlobalRole(ctx, tx, st.Q, rbac.PrincipalUser, "u1", rbac.AdminRoleID); err != nil {
		s.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatal(err)
	}

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
		Node              string `json:"node"`
		Containers        int    `json:"containers"`
		ContainersRunning int    `json:"containersRunning"`
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
// string without the question mark, such as "node=bp-w1".
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

// WaitFor polls cond every 250 ms until it is true and fails the test with the
// last observation when timeout passes first. what names what is awaited.
func (s *Server) WaitFor(what string, timeout time.Duration, cond func() (bool, string)) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	var seen string
	for {
		ok, obs := cond()
		if ok {
			return
		}
		if obs != "" {
			seen = obs
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("gave up waiting for %s after %s; last seen: %s", what, timeout, seen)
		}
		time.Sleep(250 * time.Millisecond)
	}
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
