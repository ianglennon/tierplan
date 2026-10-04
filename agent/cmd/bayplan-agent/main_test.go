// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/agent/internal/connect"
	"github.com/ianglennon/bayplan/agent/internal/privdrop"
	"github.com/ianglennon/bayplan/agent/internal/version"
	"github.com/ianglennon/bayplan/proto/wire"
)

const secret = "bpjt_SECRETVALUE_zzz" //nolint:gosec // a test fixture, not a credential

// harness records the order of the effects run causes.
type harness struct {
	events  []string
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	dropErr error
	dropOpt privdrop.Options
	gotConn connect.Config
}

func (h *harness) deps(env map[string]string, files map[string]string) deps {
	return deps{
		getenv: func(k string) string { return env[k] },
		readFile: func(name string) ([]byte, error) {
			if v, ok := files[name]; ok {
				h.events = append(h.events, "read "+name)
				return []byte(v), nil
			}
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		},
		drop: func(o privdrop.Options) (privdrop.Result, error) {
			h.events = append(h.events, "drop")
			h.dropOpt = o
			if h.dropErr != nil {
				return privdrop.Result{}, h.dropErr
			}
			return privdrop.Result{Dropped: true, GID: 998}, nil
		},
		connect: func(_ context.Context, c connect.Config) error {
			h.events = append(h.events, "connect")
			h.gotConn = c
			return nil
		},
		getuid: func() int { return 1000 },
		getgid: func() int { return 1000 },
		stdout: &h.stdout,
		stderr: &h.stderr,
	}
}

func TestVersionFlagPrintsTheVersionAndExits(t *testing.T) {
	prev := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = prev })

	h := &harness{}
	d := h.deps(nil, nil)
	d.args = []string{"--version"}
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := strings.TrimSpace(h.stdout.String()); got != "v9.9.9" {
		t.Fatalf("printed %q, want v9.9.9", got)
	}
	if len(h.events) != 0 {
		t.Fatalf("--version did something else first: %v", h.events)
	}
}

func TestTheTokenFileIsReadBeforeThePrivilegesAreDropped(t *testing.T) {
	h := &harness{}
	d := h.deps(
		map[string]string{
			"BAYPLAN_AGENT_URL":             "wss://bayplan.example/agent/connect",
			"BAYPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/bayplan-agent-token",
			"BAYPLAN_AGENT_DOCKER_GID":      "42",
		},
		map[string]string{"/run/secrets/bayplan-agent-token": secret + "\n"},
	)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	want := []string{"read /run/secrets/bayplan-agent-token", "drop", "connect"}
	if !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v: the secret can only be read before the drop", h.events, want)
	}
	if h.dropOpt.SocketPath != "/var/run/docker.sock" || h.dropOpt.DockerGID == nil || *h.dropOpt.DockerGID != 42 {
		t.Fatalf("drop options = %+v, want the default socket and GID 42", h.dropOpt)
	}
	if h.gotConn.JoinToken != secret {
		t.Fatalf("connect got token %q, want the file's", h.gotConn.JoinToken)
	}
	if strings.Contains(h.stderr.String(), secret) || strings.Contains(h.stdout.String(), secret) {
		t.Fatalf("the token reached the output: %s", h.stderr.String())
	}
	for _, want := range []string{"bayplan.example", "token_present=true", "uid=1000"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the start line does not contain %q: %s", want, h.stderr.String())
		}
	}
}

func TestAConfigurationErrorExitsBeforeTheDropAndNeverShowsTheToken(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{
		"BAYPLAN_AGENT_URL":        "https://not-a-websocket",
		"BAYPLAN_AGENT_JOIN_TOKEN": secret,
	}, nil)
	if code := run(d); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if len(h.events) != 0 {
		t.Fatalf("events = %v, want none", h.events)
	}
	if strings.Contains(h.stderr.String(), secret) {
		t.Fatalf("the token reached the output: %s", h.stderr.String())
	}
}

func TestAFailedDropExitsNonZeroAndNeverConnects(t *testing.T) {
	h := &harness{dropErr: errors.New("drop privileges: set uid 1000: operation not permitted")}
	d := h.deps(map[string]string{
		"BAYPLAN_AGENT_URL":        "wss://bayplan.example/agent/connect",
		"BAYPLAN_AGENT_JOIN_TOKEN": secret,
	}, nil)
	if code := run(d); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !reflect.DeepEqual(h.events, []string{"drop"}) {
		t.Fatalf("events = %v, want only the drop: the agent must not connect as root", h.events)
	}
	if strings.Contains(h.stderr.String(), secret) {
		t.Fatalf("the token reached the output: %s", h.stderr.String())
	}
}

func TestTheStartMarkerLandsInTheStateDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := touchState(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "started"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	if err := touchState(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("touchState must not create the directory")
	}
}

func TestTheStateDirectoryAndBackoffReachTheConnection(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{
		"BAYPLAN_AGENT_URL":         "wss://bayplan.example/agent/connect",
		"BAYPLAN_AGENT_JOIN_TOKEN":  secret,
		"BAYPLAN_AGENT_STATE_DIR":   "/state",
		"BAYPLAN_AGENT_BACKOFF_MIN": "2s",
		"BAYPLAN_AGENT_BACKOFF_MAX": "20s",
	}, nil)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if h.gotConn.StateDir != "/state" || h.gotConn.BackoffMin != 2*time.Second || h.gotConn.BackoffMax != 20*time.Second {
		t.Fatalf("connect got state %q backoff %v..%v, want /state and 2s..20s", h.gotConn.StateDir, h.gotConn.BackoffMin, h.gotConn.BackoffMax)
	}
}

func TestThePolicyFlagsReachTheConnection(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{
		"BAYPLAN_AGENT_URL":         "wss://bayplan.example/agent/connect",
		"BAYPLAN_AGENT_JOIN_TOKEN":  secret,
		"BAYPLAN_AGENT_READ_ONLY":   "true",
		"BAYPLAN_AGENT_SELF_UPDATE": "false",
	}, nil)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !h.gotConn.ReadOnly || h.gotConn.SelfUpdate {
		t.Fatalf("connect got ReadOnly %v SelfUpdate %v, want true and false", h.gotConn.ReadOnly, h.gotConn.SelfUpdate)
	}
}

func TestSelfMaintenanceReachesTheConnectionAndHonoursTheHostSwitch(t *testing.T) {
	for _, tc := range []struct {
		flag     string
		wantCode string // what an update answers when no Docker is reachable
	}{
		{"false", wire.UpdateErrSelfUpdateDisabled},
		{"true", wire.UpdateErrOwnContainerUnknown},
	} {
		t.Run("self update "+tc.flag, func(t *testing.T) {
			h := &harness{}
			d := h.deps(map[string]string{
				"BAYPLAN_AGENT_URL":         "wss://bayplan.example/agent/connect",
				"BAYPLAN_AGENT_JOIN_TOKEN":  secret,
				"BAYPLAN_AGENT_SELF_UPDATE": tc.flag,
				"BAYPLAN_AGENT_SOCKET":      "/nonexistent/docker.sock",
			}, nil)
			if code := run(d); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
			}
			if h.gotConn.Maint == nil {
				t.Fatal("connect got no self-maintenance")
			}
			// Off: the switch answers. On, with no Docker to read its own container
			// from, the agent cannot know its own repository and updates nothing.
			res := h.gotConn.Maint.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: "ghcr.io/ianglennon/bayplan-agent:1"})
			if res.OK || res.Error != tc.wantCode {
				t.Fatalf("update answered %+v, want error %s", res, tc.wantCode)
			}
		})
	}
}
