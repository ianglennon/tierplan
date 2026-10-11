// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// nonLoopbackIPv4 is an address of this machine that is up, is not loopback and
// is not link-local. It is "" when there is none.
func nonLoopbackIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	_, linkLocal, _ := net.ParseCIDR("169.254.0.0/16")
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || linkLocal.Contains(ip) {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// envPair finds the -e 'NAME=value' pairs of a docker run command.
var envPair = regexp.MustCompile(`-e '([A-Z0-9_]+)=([^']*)'`)

// TestTheDevelopmentInstallCommandConnectsOverPlainWS is the shape of make
// dev-go plus make dev-swarm (T-02-31): a development server with a plain agent
// address on a non-loopback interface, and an agent started from exactly the
// command it generates. The agent refuses plain ws:// to another host unless
// BAYPLAN_AGENT_ALLOW_INSECURE=true is set on it, so the generated command has
// to carry the switch, and the same agent without it must stop at start.
func TestTheDevelopmentInstallCommandConnectsOverPlainWS(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary")
	}
	host := nonLoopbackIPv4()
	if host == "" {
		t.Skip("this machine has no non-loopback IPv4 address")
	}

	agentBin := filepath.Join(t.TempDir(), "bayplan-agent")
	build := exec.Command("go", "build", "-o", agentBin, "./cmd/bayplan-agent") //nolint:gosec // the agent module is built from this repository
	build.Dir = "../../agent"
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", host, err)
	}
	ts := httptest.NewUnstartedServer(nil)
	_ = ts.Listener.Close()
	ts.Listener = ln
	addr := ln.Addr().String()
	cfg := testConfig(t, map[string]string{
		"BAYPLAN_DATA_DIR":             t.TempDir() + "/data",
		"BAYPLAN_PUBLIC_URL":           "http://" + addr,
		"BAYPLAN_AGENT_ALLOW_INSECURE": "true",
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close()
	})

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

	code, body := call(http.MethodPost, "/api/environments", `{"kind":"agent"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/environments kind agent: %d %s", code, body)
	}
	var created struct {
		ID      string `json:"id"`
		Install struct {
			JoinToken string `json:"joinToken"`
			Swarm     struct {
				Command   string `json:"command"`
				StackFile string `json:"stackFile"`
			} `json:"swarm"`
			Standalone struct {
				Command     string `json:"command"`
				ComposeFile string `json:"composeFile"`
			} `json:"standalone"`
		} `json:"install"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	in := created.Install

	// The texts: the plain address and the agent's switch, side by side.
	wantURL := "-e 'BAYPLAN_AGENT_URL=ws://" + addr + "/agent/connect'"
	if !strings.Contains(in.Standalone.Command, wantURL) {
		t.Fatalf("the standalone command lacks %s:\n%s", wantURL, in.Standalone.Command)
	}
	if !strings.Contains(in.Standalone.Command, "-e 'BAYPLAN_AGENT_ALLOW_INSECURE=true'") {
		t.Errorf("the standalone command lacks the agent's switch:\n%s", in.Standalone.Command)
	}
	if !strings.Contains(in.Swarm.Command, "--env 'BAYPLAN_AGENT_ALLOW_INSECURE=true'") {
		t.Errorf("the swarm command lacks the agent's switch:\n%s", in.Swarm.Command)
	}
	for name, text := range map[string]string{"stack file": in.Swarm.StackFile, "compose file": in.Standalone.ComposeFile} {
		if !strings.Contains(text, `BAYPLAN_AGENT_ALLOW_INSECURE: "true"`) {
			t.Errorf("the %s lacks the agent's switch:\n%s", name, text)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// Run a real agent with exactly the pairs of the standalone command.
	pairs := envPair.FindAllStringSubmatch(in.Standalone.Command, -1)
	if len(pairs) == 0 {
		t.Fatalf("no -e pairs in the standalone command:\n%s", in.Standalone.Command)
	}
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "plain-host"})
	start := func(skip, stateDir string) (done chan error, log *lockedBuffer, stop func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		log = &lockedBuffer{}
		cmd := exec.CommandContext(ctx, agentBin) //nolint:gosec // the binary this test just built
		cmd.Env = []string{
			"BAYPLAN_AGENT_SOCKET=" + fake.SocketPath(),
			"BAYPLAN_AGENT_STATE_DIR=" + stateDir,
			"PATH=" + os.Getenv("PATH"),
		}
		for _, p := range pairs {
			if p[1] == skip {
				continue
			}
			cmd.Env = append(cmd.Env, p[1]+"="+p[2])
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		// done carries the exit once; exited closes so a cleanup can still wait
		// after done was read.
		done = make(chan error, 1)
		exited := make(chan struct{})
		go func() { done <- cmd.Wait(); close(exited) }()
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
		return done, log, stop
	}

	done, log, stop := start("", t.TempDir())
	deadline := time.Now().Add(30 * time.Second)
	for online := false; !online; {
		code, body := call(http.MethodGet, "/api/environments", "")
		var list struct {
			Items []struct {
				ID     string `json:"id"`
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"items"`
		}
		if code == http.StatusOK && json.Unmarshal(body, &list) == nil {
			for _, e := range list.Items {
				online = online || (e.ID == created.ID && e.Status.State == "online")
			}
		}
		if online {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the agent exited (%v) before the environment came online; its output:\n%s", err, log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the environment was not online within 30s\nagent output:\n%s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The server marks the session online once it has sent the welcome, and the
	// agent logs its enrolment when the welcome arrives, so the line can trail the
	// status by a moment on a loaded machine.
	for !strings.Contains(log.String(), "enrolled with Bayplan") {
		if time.Now().After(deadline) {
			t.Fatalf("the agent's log does not say it enrolled:\n%s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(log.String(), "allow_insecure=true") {
		t.Errorf("the agent's start line does not show allow_insecure=true:\n%s", log.String())
	}
	if strings.Contains(log.String(), in.JoinToken) {
		t.Error("the agent logged the join token")
	}
	stop()

	// The same command without the agent's switch: the agent stops at start.
	done2, log2, _ := start("BAYPLAN_AGENT_ALLOW_INSECURE", t.TempDir())
	select {
	case err := <-done2:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("the agent without the switch exited with %v, want exit code 2\n%s", err, log2.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the agent without the switch did not exit; output:\n%s", log2.String())
	}
	if !strings.Contains(log2.String(), "BAYPLAN_AGENT_ALLOW_INSECURE") {
		t.Errorf("the refusal does not name the switch:\n%s", log2.String())
	}
	if strings.Contains(log2.String(), in.JoinToken) {
		t.Error("the refused agent logged the join token")
	}
	var n int
	if err := srv.Store().Read.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil || n != 1 {
		t.Errorf("agents rows = %d, %v; want exactly one", n, err)
	}
}
