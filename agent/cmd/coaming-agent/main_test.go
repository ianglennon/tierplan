// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/coaming/agent/internal/connect"
	"github.com/ianglennon/coaming/agent/internal/privdrop"
	"github.com/ianglennon/coaming/agent/internal/selfmaint"
	"github.com/ianglennon/coaming/agent/internal/version"
	"github.com/ianglennon/coaming/proto/wire"
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
	// recreateErr is what the stubbed helper returns; gotRecreate what it was given.
	recreateErr error
	gotRecreate selfmaint.RecreateOptions
	// removeErr is what the stubbed removal helper returns; gotRemove what it was given.
	removeErr error
	gotRemove selfmaint.RemoveVolumeOptions
	// stateLocked makes every read under the state directory fail with a permission
	// error until the stubbed drop has run: what the agent meets as root with every
	// capability but SETUID and SETGID dropped, which cannot open UID 1000's 0700
	// directory (G-02-5). stateUnreadable keeps refusing after the drop as well.
	stateLocked, stateUnreadable bool
	dropped                      bool
}

// inState says whether a path is under the state directory the environment names.
func inState(env map[string]string, name string) bool {
	dir := env["COAMING_AGENT_STATE_DIR"]
	if dir == "" {
		dir = "/var/lib/coaming-agent"
	}
	return strings.HasPrefix(name, dir+"/")
}

func (h *harness) deps(env map[string]string, files map[string]string) deps {
	return deps{
		getenv: func(k string) string { return env[k] },
		readFile: func(name string) ([]byte, error) {
			if inState(env, name) && (h.stateUnreadable || (h.stateLocked && !h.dropped)) {
				h.events = append(h.events, "denied "+name)
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
			}
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
			h.dropped = true
			return privdrop.Result{Dropped: true, GID: 998}, nil
		},
		connect: func(_ context.Context, c connect.Config) error {
			h.events = append(h.events, "connect")
			h.gotConn = c
			return nil
		},
		recreate: func(_ context.Context, o selfmaint.RecreateOptions) error {
			h.events = append(h.events, "recreate")
			h.gotRecreate = o
			return h.recreateErr
		},
		removeVolume: func(_ context.Context, o selfmaint.RemoveVolumeOptions) error {
			h.events = append(h.events, "remove-volume")
			h.gotRemove = o
			return h.removeErr
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
			"COAMING_AGENT_URL":             "wss://coaming.example/agent/connect",
			"COAMING_AGENT_JOIN_TOKEN_FILE": "/run/secrets/coaming-agent-token",
			"COAMING_AGENT_DOCKER_GID":      "42",
		},
		map[string]string{"/run/secrets/coaming-agent-token": secret + "\n"},
	)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	want := []string{"read /run/secrets/coaming-agent-token", "drop", "connect"}
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
	for _, want := range []string{"coaming.example", "token_present=true", "uid=1000"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the start line does not contain %q: %s", want, h.stderr.String())
		}
	}
}

// TestNothingInTheStateDirectoryIsReadBeforeTheDrop pins the order that G-02-5
// broke: with a token file and a state directory that refuses every read until the
// privileges are dropped (as capability-less root meets it), the agent reads the
// token, drops, and connects, and never touches the state directory as root (D-22).
func TestNothingInTheStateDirectoryIsReadBeforeTheDrop(t *testing.T) {
	h := &harness{stateLocked: true}
	d := h.deps(
		map[string]string{
			"COAMING_AGENT_URL":             "wss://coaming.example/agent/connect",
			"COAMING_AGENT_JOIN_TOKEN_FILE": "/run/secrets/coaming-agent-token",
		},
		map[string]string{"/run/secrets/coaming-agent-token": secret + "\n"},
	)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if want := []string{"read /run/secrets/coaming-agent-token", "drop", "connect"}; !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
	if strings.Contains(h.stderr.String(), secret) || strings.Contains(h.stdout.String(), secret) {
		t.Fatalf("the token reached the output: %s", h.stderr.String())
	}
}

// TestAnEnrolledAgentWithoutATokenStartsWhenRootCannotReadItsStateDirectory is the
// G-02-5 regression: no join token, an identity on the state volume, and a state
// directory that root cannot open before the drop. The agent must drop first, then
// find its identity as UID 1000 and connect with no token, in the default state
// directory and in a custom one.
func TestAnEnrolledAgentWithoutATokenStartsWhenRootCannotReadItsStateDirectory(t *testing.T) {
	for name, dir := range map[string]string{"default": "/var/lib/coaming-agent", "custom": "/state"} {
		t.Run(name, func(t *testing.T) {
			h := &harness{stateLocked: true}
			env := map[string]string{"COAMING_AGENT_URL": "wss://coaming.example/agent/connect"}
			if name == "custom" {
				env["COAMING_AGENT_STATE_DIR"] = dir
			}
			d := h.deps(env, map[string]string{dir + "/identity.json": "{}"})
			if code := run(d); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
			}
			if want := []string{"drop", "read " + dir + "/identity.json", "connect"}; !reflect.DeepEqual(h.events, want) {
				t.Fatalf("events = %v, want %v", h.events, want)
			}
			if h.gotConn.JoinToken != "" {
				t.Errorf("connect got a token %q, want none", h.gotConn.JoinToken)
			}
			for _, want := range []string{"token_present=false", "enrolled=true"} {
				if !strings.Contains(h.stderr.String(), want) {
					t.Errorf("the start line does not contain %q: %s", want, h.stderr.String())
				}
			}
		})
	}
}

// TestAnAgentWithNoTokenAndNoIdentityStopsAfterTheDrop pins that the refusal keeps
// its exit code and message but is now decided after the drop, as UID 1000, and
// that the agent never connects.
func TestAnAgentWithNoTokenAndNoIdentityStopsAfterTheDrop(t *testing.T) {
	h := &harness{stateLocked: true}
	d := h.deps(map[string]string{"COAMING_AGENT_URL": "wss://coaming.example/agent/connect"}, nil)
	if code := run(d); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if want := []string{"drop"}; !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
	for _, name := range []string{"COAMING_AGENT_JOIN_TOKEN_FILE", "COAMING_AGENT_JOIN_TOKEN"} {
		if !strings.Contains(h.stderr.String(), name) {
			t.Errorf("stderr %q does not name %s", h.stderr.String(), name)
		}
	}
}

// TestAnIdentityUnreadableAfterTheDropStopsTheAgent pins that an identity the
// dropped user still cannot read is reported with the state directory, exits 2 and
// never connects.
func TestAnIdentityUnreadableAfterTheDropStopsTheAgent(t *testing.T) {
	h := &harness{stateUnreadable: true}
	d := h.deps(map[string]string{"COAMING_AGENT_URL": "wss://coaming.example/agent/connect"}, map[string]string{"/var/lib/coaming-agent/identity.json": "{}"})
	if code := run(d); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if slices.Contains(h.events, "connect") {
		t.Fatalf("events = %v: the agent connected without being able to read its identity", h.events)
	}
	for _, want := range []string{"read the enrolled state in", "/var/lib/coaming-agent"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
		}
	}
}

func TestAConfigurationErrorExitsBeforeTheDropAndNeverShowsTheToken(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{
		"COAMING_AGENT_URL":        "https://not-a-websocket",
		"COAMING_AGENT_JOIN_TOKEN": secret,
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
		"COAMING_AGENT_URL":        "wss://coaming.example/agent/connect",
		"COAMING_AGENT_JOIN_TOKEN": secret,
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

// The image asks for a private state directory, but a builder or a release
// tool may keep the mode of the checkout (git stores none), so the agent makes
// its own directory private when it starts.
func TestTheStateDirectoryIsMadePrivateAtStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the test widens its own temporary directory on purpose
		t.Fatal(err)
	}
	if err := touchState(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestTheStateDirectoryAndBackoffReachTheConnection(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{
		"COAMING_AGENT_URL":         "wss://coaming.example/agent/connect",
		"COAMING_AGENT_JOIN_TOKEN":  secret,
		"COAMING_AGENT_STATE_DIR":   "/state",
		"COAMING_AGENT_BACKOFF_MIN": "2s",
		"COAMING_AGENT_BACKOFF_MAX": "20s",
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
		"COAMING_AGENT_URL":         "wss://coaming.example/agent/connect",
		"COAMING_AGENT_JOIN_TOKEN":  secret,
		"COAMING_AGENT_READ_ONLY":   "true",
		"COAMING_AGENT_SELF_UPDATE": "false",
	}, nil)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !h.gotConn.ReadOnly || h.gotConn.SelfUpdate {
		t.Fatalf("connect got ReadOnly %v SelfUpdate %v, want true and false", h.gotConn.ReadOnly, h.gotConn.SelfUpdate)
	}
}

func TestTheAllowInsecureSwitchReachesTheConnection(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"true", true}, {"", false}, {"false", false}} {
		h := &harness{}
		d := h.deps(map[string]string{
			"COAMING_AGENT_URL":            "ws://coaming.example/agent/connect",
			"COAMING_AGENT_JOIN_TOKEN":     secret,
			"COAMING_AGENT_ALLOW_INSECURE": tc.value,
		}, nil)
		code := run(d)
		if tc.want {
			if code != 0 {
				t.Fatalf("switch %q: exit %d, stderr: %s", tc.value, code, h.stderr.String())
			}
			if !h.gotConn.AllowInsecure {
				t.Errorf("switch %q: connect got AllowInsecure false, want true", tc.value)
			}
			if !strings.Contains(h.stderr.String(), "allow_insecure=true") {
				t.Errorf("switch %q: the start line does not carry allow_insecure=true: %s", tc.value, h.stderr.String())
			}
			continue
		}
		// Without the switch the same address never reaches the connection.
		if code != 2 || h.gotConn.AllowInsecure || slices.Contains(h.events, "connect") {
			t.Errorf("switch %q: exit %d, events %v; want exit 2 and no connect", tc.value, code, h.events)
		}
	}
	// With an allowed (wss) URL and no switch, AllowInsecure is false and the line says so.
	h := &harness{}
	d := h.deps(map[string]string{"COAMING_AGENT_URL": "wss://coaming.example/agent/connect", "COAMING_AGENT_JOIN_TOKEN": secret}, nil)
	if code := run(d); code != 0 || h.gotConn.AllowInsecure || !strings.Contains(h.stderr.String(), "allow_insecure=false") {
		t.Errorf("wss without the switch: exit %d, AllowInsecure %v, stderr %s", code, h.gotConn.AllowInsecure, h.stderr.String())
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
				"COAMING_AGENT_URL":         "wss://coaming.example/agent/connect",
				"COAMING_AGENT_JOIN_TOKEN":  secret,
				"COAMING_AGENT_SELF_UPDATE": tc.flag,
				"COAMING_AGENT_SOCKET":      "/nonexistent/docker.sock",
			}, nil)
			if code := run(d); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
			}
			if h.gotConn.Maint == nil {
				t.Fatal("connect got no self-maintenance")
			}
			// Off: the switch answers. On, with no Docker to read its own container
			// from, the agent cannot know its own repository and updates nothing.
			res := h.gotConn.Maint.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: "ghcr.io/ianglennon/coaming-agent:1"})
			if res.OK || res.Error != tc.wantCode {
				t.Fatalf("update answered %+v, want error %s", res, tc.wantCode)
			}
		})
	}
}

func TestTheHelperSubcommandRecreatesTheTargetWithoutAURLOrAToken(t *testing.T) {
	dir := t.TempDir()
	h := &harness{}
	d := h.deps(map[string]string{"COAMING_AGENT_STATE_DIR": dir, "COAMING_AGENT_SOCKET": "/s/docker.sock", "COAMING_AGENT_DOCKER_GID": "42"}, nil)
	d.args = []string{"helper", "recreate", "--target", "abc123"}
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if want := []string{"drop", "recreate"}; !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v: no connect loop, and the privileges are dropped first", h.events, want)
	}
	if h.dropOpt.SocketPath != "/s/docker.sock" || h.dropOpt.DockerGID == nil || *h.dropOpt.DockerGID != 42 {
		t.Fatalf("drop options = %+v", h.dropOpt)
	}
	o := h.gotRecreate
	if o.TargetID != "abc123" || o.SocketPath != "/s/docker.sock" || o.Timeout != selfmaint.ConnectTimeout {
		t.Fatalf("recreate options = %+v", o)
	}

	// The marker is the modification time of the connected file in the state directory.
	if at, err := o.Marker(); err != nil || !at.IsZero() {
		t.Fatalf("with no marker the time is zero, got %v, %v", at, err)
	}
	path := filepath.Join(dir, "connected")
	if err := os.WriteFile(path, []byte("2026-10-04T10:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 4, 10, 0, 7, 123456789, time.UTC)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}
	if at, err := o.Marker(); err != nil || !at.Equal(want) {
		t.Fatalf("marker = %v, %v, want the file's modification time %v", at, err, want)
	}
}

func TestTheHelperSubcommandExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		err     error
		dropErr error
		want    int
		recreat bool
	}{
		{"success", []string{"helper", "recreate", "--target", "x"}, nil, nil, 0, true},
		{"rolled back", []string{"helper", "recreate", "--target", "x"}, selfmaint.ErrRolledBack, nil, 1, true},
		{"other failure", []string{"helper", "recreate", "--target", "x"}, errors.New("boom"), nil, 1, true},
		{"no target", []string{"helper", "recreate"}, nil, nil, 2, false},
		{"empty target", []string{"helper", "recreate", "--target", ""}, nil, nil, 2, false},
		{"no subcommand", []string{"helper"}, nil, nil, 2, false},
		{"unknown subcommand", []string{"helper", "explode", "--target", "x"}, nil, nil, 2, false},
		{"unknown flag", []string{"helper", "recreate", "--target", "x", "--force"}, nil, nil, 2, false},
		{"failed drop", []string{"helper", "recreate", "--target", "x"}, nil, errors.New("no"), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{recreateErr: tc.err, dropErr: tc.dropErr}
			d := h.deps(map[string]string{"COAMING_AGENT_STATE_DIR": t.TempDir()}, nil)
			d.args = tc.args
			if code := run(d); code != tc.want {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tc.want, h.stderr.String())
			}
			if got := slices.Contains(h.events, "recreate"); got != tc.recreat {
				t.Fatalf("recreate ran = %v, want %v (events %v)", got, tc.recreat, h.events)
			}
			if slices.Contains(h.events, "connect") {
				t.Fatalf("the helper must not run the connect loop: %v", h.events)
			}
		})
	}
}

func TestTheHelperSubcommandRejectsABadDockerGID(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{"COAMING_AGENT_DOCKER_GID": "x"}, nil)
	d.args = []string{"helper", "recreate", "--target", "x"}
	if code := run(d); code != 2 || len(h.events) != 0 {
		t.Fatalf("exit %d, events %v", code, h.events)
	}
}

func TestTheRemoveVolumeHelperPassesItsFlagsAndDropsPrivilegesFirst(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{"COAMING_AGENT_SOCKET": "/s/docker.sock", "COAMING_AGENT_DOCKER_GID": "42"}, nil)
	d.args = []string{"helper", "remove-volume", "--wait", "abc123", "--volume", "coaming-agent", "--remove-container", "--keep-volume"}
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if want := []string{"drop", "remove-volume"}; !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v: no connect loop, and the privileges are dropped first", h.events, want)
	}
	o := h.gotRemove
	if o.ContainerID != "abc123" || o.Volume != "coaming-agent" || o.SocketPath != "/s/docker.sock" || !o.RemoveContainer || !o.KeepVolume {
		t.Fatalf("options = %+v", o)
	}
	if o.Grace <= 0 || o.Timeout != selfmaint.RemoveTimeout {
		t.Fatalf("grace %v and timeout %v: a standalone helper gives the agent a moment to answer, and waits the stated time", o.Grace, o.Timeout)
	}

	h = &harness{}
	d = h.deps(nil, nil)
	d.args = []string{"helper", "remove-volume", "--wait", "abc123"}
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if o := h.gotRemove; o.RemoveContainer || o.KeepVolume || o.Volume != "" || o.Grace != 0 {
		t.Fatalf("defaults = %+v: nothing is removed that was not asked for, and there is no grace without RemoveContainer", o)
	}
}

func TestTheRemoveVolumeHelperExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		err     error
		dropErr error
		want    int
		ran     bool
	}{
		{"success", []string{"helper", "remove-volume", "--wait", "x"}, nil, nil, 0, true},
		{"failure", []string{"helper", "remove-volume", "--wait", "x"}, errors.New("boom"), nil, 1, true},
		{"no wait", []string{"helper", "remove-volume"}, nil, nil, 2, false},
		{"empty wait", []string{"helper", "remove-volume", "--wait", ""}, nil, nil, 2, false},
		{"unknown flag", []string{"helper", "remove-volume", "--wait", "x", "--force"}, nil, nil, 2, false},
		{"stray argument", []string{"helper", "remove-volume", "--wait", "x", "extra"}, nil, nil, 2, false},
		{"failed drop", []string{"helper", "remove-volume", "--wait", "x"}, nil, errors.New("no"), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{removeErr: tc.err, dropErr: tc.dropErr}
			d := h.deps(nil, nil)
			d.args = tc.args
			if code := run(d); code != tc.want {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tc.want, h.stderr.String())
			}
			if got := slices.Contains(h.events, "remove-volume"); got != tc.ran {
				t.Fatalf("remove-volume ran = %v, want %v (events %v)", got, tc.ran, h.events)
			}
			if slices.Contains(h.events, "connect") {
				t.Fatalf("the helper must not run the connect loop: %v", h.events)
			}
		})
	}
}

// nonLoopbackIPv4 is an IPv4 address of an up interface of this machine that is
// neither loopback nor link-local, or "" when there is none.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}

// refusingServer answers every upgrade with the agent subprotocol and a refusal of
// the join token, and counts the upgrades. It listens on host.
type refusingServer struct {
	ts       *httptest.Server
	upgrades atomic.Int32
}

func newRefusingServer(t *testing.T, host string) *refusingServer {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	s := &refusingServer{}
	s.ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.upgrades.Add(1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		b, _ := json.Marshal(wire.Refuse{T: wire.TypeRefuse, Code: wire.ReasonAuthRejected, Detail: "refused by the test"})
		_ = c.Write(r.Context(), websocket.MessageText, b)
		_ = c.Close(websocket.StatusPolicyViolation, wire.ReasonAuthRejected)
	}))
	s.ts.Listener = ln
	s.ts.Start()
	t.Cleanup(s.ts.Close)
	return s
}

func (s *refusingServer) url() string {
	return "ws" + strings.TrimPrefix(s.ts.URL, "http") + "/agent/connect"
}

// TestAnAgentDialsPlainWSToAnotherHostOnlyWithTheSwitch starts the agent through
// run with the real configuration and the real connection code. A plain ws://
// COAMING_AGENT_URL for a host that is not loopback sends the join token and the
// proof to a peer the agent never authenticated, so the agent refuses it, before
// dropping privileges and without a dial, unless COAMING_AGENT_ALLOW_INSECURE=true
// is set on the agent itself; loopback needs no switch (T-02-31, WR-A05).
func TestAnAgentDialsPlainWSToAnotherHostOnlyWithTheSwitch(t *testing.T) {
	start := func(t *testing.T, raw string, extra map[string]string) (*harness, int) {
		t.Helper()
		sockDir := t.TempDir()
		env := map[string]string{
			"COAMING_AGENT_URL":        raw,
			"COAMING_AGENT_JOIN_TOKEN": secret,
			"COAMING_AGENT_STATE_DIR":  t.TempDir(),
			"COAMING_AGENT_SOCKET":     filepath.Join(sockDir, "missing.sock"),
		}
		for k, v := range extra {
			env[k] = v
		}
		h := &harness{}
		d := h.deps(env, nil)
		d.connect = connect.Run
		return h, run(d)
	}

	t.Run("without the switch the agent refuses to start and never dials", func(t *testing.T) {
		ip := nonLoopbackIPv4(t)
		if ip == "" {
			t.Skip("this machine has no non-loopback IPv4 address")
		}
		srv := newRefusingServer(t, ip)
		h, code := start(t, srv.url(), nil)
		if code != 2 {
			t.Fatalf("exit %d, want 2; stderr: %s", code, h.stderr.String())
		}
		for _, want := range []string{"COAMING_AGENT_ALLOW_INSECURE", "wss://"} {
			if !strings.Contains(h.stderr.String(), want) {
				t.Errorf("stderr does not mention %s: %s", want, h.stderr.String())
			}
		}
		if n := srv.upgrades.Load(); n != 0 {
			t.Errorf("the server saw %d upgrades, want none", n)
		}
		if slices.Contains(h.events, "drop") {
			t.Errorf("the agent dropped privileges before refusing the address: %v", h.events)
		}
		if strings.Contains(h.stderr.String(), secret) {
			t.Fatalf("the token reached the output: %s", h.stderr.String())
		}
	})

	t.Run("with the switch the agent dials and warns", func(t *testing.T) {
		ip := nonLoopbackIPv4(t)
		if ip == "" {
			t.Skip("this machine has no non-loopback IPv4 address")
		}
		srv := newRefusingServer(t, ip)
		h, code := start(t, srv.url(), map[string]string{"COAMING_AGENT_ALLOW_INSECURE": "true"})
		if code != 1 {
			t.Fatalf("exit %d, want 1 (the server refused the first enrolment); stderr: %s", code, h.stderr.String())
		}
		if n := srv.upgrades.Load(); n != 1 {
			t.Errorf("the server saw %d upgrades, want 1", n)
		}
		if !strings.Contains(h.stderr.String(), "plain ws://") {
			t.Errorf("stderr has no plain ws:// warning: %s", h.stderr.String())
		}
		if strings.Contains(h.stderr.String(), secret) {
			t.Fatalf("the token reached the output: %s", h.stderr.String())
		}
	})

	t.Run("loopback needs no switch", func(t *testing.T) {
		srv := newRefusingServer(t, "127.0.0.1")
		h, code := start(t, srv.url(), nil)
		if code != 1 {
			t.Fatalf("exit %d, want 1; stderr: %s", code, h.stderr.String())
		}
		if n := srv.upgrades.Load(); n != 1 {
			t.Errorf("the server saw %d upgrades, want 1", n)
		}
		if strings.Contains(h.stderr.String(), secret) {
			t.Fatalf("the token reached the output: %s", h.stderr.String())
		}
	})
}
