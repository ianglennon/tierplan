// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// GET /api/local-host (plan 15, D-19). The fake daemon is a real unix socket,
// so the whole path runs: socket check, client, probe, JSON.

type localHostResp struct {
	SocketPath string `json:"socketPath"`
	Detected   bool   `json:"detected"`
	Probe      *struct {
		Reachable     bool   `json:"reachable"`
		Supported     bool   `json:"supported"`
		Reason        string `json:"reason"`
		Detail        string `json:"detail"`
		EngineName    string `json:"engineName"`
		EngineVersion string `json:"engineVersion"`
		APIVersion    string `json:"apiVersion"`
		Hostname      string `json:"hostname"`
		Type          string `json:"type"`
	} `json:"probe"`
}

func (f *authFixture) localHostCall(t *testing.T, cookie string) (*localHostResp, int) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/local-host", cookie: cookie})
	var out localHostResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	}
	return &out, rec.Code
}

func localHostFixture(t *testing.T, dockerHost string) (f *authFixture, cookie string) {
	t.Helper()
	f = newAuthFixture(t, config.Config{})
	f.localHost.DockerHost = dockerHost
	f.localHost.ProbeTimeout = 500 * time.Millisecond
	f.h = f.build(t)
	cookie, _ = f.signIn(t, "admin", false)
	return f, cookie
}

func TestLocalHostReportsNoSocketWhenNoneExistsAtThePath(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "docker.sock")
	f, cookie := localHostFixture(t, "unix://"+missing)
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || got.Detected || got.Probe != nil || got.SocketPath != missing {
		t.Fatalf("got %d %+v probe=%v, want 200, detected=false, no probe, path %s", code, got, got.Probe, missing)
	}
}

func TestLocalHostIgnoresAPlainFileAtThePath(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(file, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, cookie := localHostFixture(t, "unix://"+file)
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || got.Detected {
		t.Fatalf("got %d detected=%v, want 200 and detected=false for a regular file", code, got.Detected)
	}
}

func TestLocalHostProbesTheEngineBehindTheSocket(t *testing.T) {
	t.Parallel()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.52", Version: "29.1.0", Hostname: "node-1",
		SwarmState: "active", SwarmControl: true})
	f, cookie := localHostFixture(t, "unix://"+fake.SocketPath())
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || !got.Detected || got.Probe == nil {
		t.Fatalf("got %d detected=%v probe=%v", code, got.Detected, got.Probe)
	}
	p := got.Probe
	if !p.Reachable || !p.Supported || p.EngineName != "Docker Engine" || p.EngineVersion != "29.1.0" ||
		p.APIVersion != "1.52" || p.Hostname != "node-1" || p.Type != "swarm-manager" {
		t.Errorf("probe = %+v", *p)
	}
}

func TestLocalHostNamesAnUnsupportedEngine(t *testing.T) {
	t.Parallel()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24"})
	f, cookie := localHostFixture(t, "unix://"+fake.SocketPath())
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || got.Probe == nil {
		t.Fatalf("got %d probe=%v", code, got.Probe)
	}
	p := got.Probe
	if !p.Reachable || p.Supported || p.Reason != "api_too_old" || p.EngineVersion != "20.10.24" || p.APIVersion != "1.41" {
		t.Errorf("probe = %+v, want a reachable, unsupported engine named with its version and API", *p)
	}
}

func TestLocalHostReportsASocketNobodyAnswersOnAsUnreachable(t *testing.T) {
	t.Parallel()
	// A socket that accepts and hangs up: it exists, but nothing speaks Docker.
	dir, err := os.MkdirTemp("", "bplh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	f, cookie := localHostFixture(t, "unix://"+path)
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || !got.Detected || got.Probe == nil {
		t.Fatalf("got %d detected=%v probe=%v, want 200 with an unreachable probe", code, got.Detected, got.Probe)
	}
	if got.Probe.Reachable || got.Probe.Supported || got.Probe.Reason != "unreachable" || got.Probe.Detail == "" {
		t.Errorf("probe = %+v", *got.Probe)
	}
}

func TestLocalHostGivesUpOnAnEngineThatAcceptsAndNeverAnswers(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "bplh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { <-hold; _ = c.Close() }()
		}
	}()

	f, cookie := localHostFixture(t, "unix://"+path)
	// The engine never answers until the test ends, so only the probe timeout (500 ms here) can
	// return this request: that it returns is the proof, and a regression hangs until go test's
	// own -timeout prints the goroutines. No elapsed-time bound is kept.
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || got.Probe == nil || got.Probe.Reachable || got.Probe.Reason != "unreachable" {
		t.Fatalf("got %d probe=%+v, want 200 and an unreachable probe", code, got.Probe)
	}
}

func TestLocalHostDefaultsToTheStandardSocketPath(t *testing.T) {
	t.Parallel()
	// An empty DOCKER_HOST is the default path. Whether that socket exists on
	// the machine running the tests is not this test's business.
	f, cookie := localHostFixture(t, "")
	got, code := f.localHostCall(t, cookie)
	if code != http.StatusOK || got.SocketPath != "/var/run/docker.sock" {
		t.Fatalf("got %d path %q, want 200 and /var/run/docker.sock", code, got.SocketPath)
	}
}

func TestLocalHostIsForInstanceAdminsOnly(t *testing.T) {
	t.Parallel()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	f, _ := localHostFixture(t, "unix://"+fake.SocketPath())
	f.addUser(t, "viewer", false) // no assignment at all
	viewer, _ := f.signIn(t, "viewer", false)

	if _, code := f.localHostCall(t, viewer); code != http.StatusForbidden {
		t.Errorf("without any assignment: %d, want 403", code)
	}
	f.addAllEnvAdmin(t, "envadmin")
	envAdmin, _ := f.signIn(t, "envadmin", false)
	if _, code := f.localHostCall(t, envAdmin); code != http.StatusForbidden {
		t.Errorf("as an admin of every environment who is not an instance admin: %d, want 403", code)
	}
	if _, code := f.localHostCall(t, ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", code)
	}
	if len(fake.Requests()) != 0 {
		t.Errorf("a refused caller still made the server talk to Docker: %v", fake.Requests())
	}
}

func TestLocalHostHasNoWritePath(t *testing.T) {
	t.Parallel()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	f, cookie := localHostFixture(t, "unix://"+fake.SocketPath())
	_, csrf := f.signIn(t, "admin", false)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := f.call(t, authReq{method: m, path: "/api/local-host", cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s /api/local-host = %d, want 405 or 404: Phase 1 is read-only (D-52)", m, rec.Code)
		}
	}
	// Whatever else was asked, only the read-only endpoints were ever called.
	for _, p := range fake.Requests() {
		if !strings.HasSuffix(p, "/_ping") && !strings.HasSuffix(p, "/version") && !strings.HasSuffix(p, "/info") {
			t.Errorf("the server called %s on the daemon", p)
		}
	}
}
