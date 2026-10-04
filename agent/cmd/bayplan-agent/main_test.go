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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/agent/internal/connect"
	"github.com/ianglennon/bayplan/agent/internal/privdrop"
	"github.com/ianglennon/bayplan/agent/internal/selfmaint"
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
	dir := env["BAYPLAN_AGENT_STATE_DIR"]
	if dir == "" {
		dir = "/var/lib/bayplan-agent"
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

// TestNothingInTheStateDirectoryIsReadBeforeTheDrop pins the order that G-02-5
// broke: with a token file and a state directory that refuses every read until the
// privileges are dropped (as capability-less root meets it), the agent reads the
// token, drops, and connects, and never touches the state directory as root (D-22).
func TestNothingInTheStateDirectoryIsReadBeforeTheDrop(t *testing.T) {
	h := &harness{stateLocked: true}
	d := h.deps(
		map[string]string{
			"BAYPLAN_AGENT_URL":             "wss://bayplan.example/agent/connect",
			"BAYPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/bayplan-agent-token",
		},
		map[string]string{"/run/secrets/bayplan-agent-token": secret + "\n"},
	)
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if want := []string{"read /run/secrets/bayplan-agent-token", "drop", "connect"}; !reflect.DeepEqual(h.events, want) {
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
	for name, dir := range map[string]string{"default": "/var/lib/bayplan-agent", "custom": "/state"} {
		t.Run(name, func(t *testing.T) {
			h := &harness{stateLocked: true}
			env := map[string]string{"BAYPLAN_AGENT_URL": "wss://bayplan.example/agent/connect"}
			if name == "custom" {
				env["BAYPLAN_AGENT_STATE_DIR"] = dir
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
	d := h.deps(map[string]string{"BAYPLAN_AGENT_URL": "wss://bayplan.example/agent/connect"}, nil)
	if code := run(d); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if want := []string{"drop"}; !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
	for _, name := range []string{"BAYPLAN_AGENT_JOIN_TOKEN_FILE", "BAYPLAN_AGENT_JOIN_TOKEN"} {
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
	d := h.deps(map[string]string{"BAYPLAN_AGENT_URL": "wss://bayplan.example/agent/connect"}, map[string]string{"/var/lib/bayplan-agent/identity.json": "{}"})
	if code := run(d); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if slices.Contains(h.events, "connect") {
		t.Fatalf("events = %v: the agent connected without being able to read its identity", h.events)
	}
	for _, want := range []string{"read the enrolled state in", "/var/lib/bayplan-agent"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
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

func TestTheHelperSubcommandRecreatesTheTargetWithoutAURLOrAToken(t *testing.T) {
	dir := t.TempDir()
	h := &harness{}
	d := h.deps(map[string]string{"BAYPLAN_AGENT_STATE_DIR": dir, "BAYPLAN_AGENT_SOCKET": "/s/docker.sock", "BAYPLAN_AGENT_DOCKER_GID": "42"}, nil)
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
			d := h.deps(map[string]string{"BAYPLAN_AGENT_STATE_DIR": t.TempDir()}, nil)
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
	d := h.deps(map[string]string{"BAYPLAN_AGENT_DOCKER_GID": "x"}, nil)
	d.args = []string{"helper", "recreate", "--target", "x"}
	if code := run(d); code != 2 || len(h.events) != 0 {
		t.Fatalf("exit %d, events %v", code, h.events)
	}
}

func TestTheRemoveVolumeHelperPassesItsFlagsAndDropsPrivilegesFirst(t *testing.T) {
	h := &harness{}
	d := h.deps(map[string]string{"BAYPLAN_AGENT_SOCKET": "/s/docker.sock", "BAYPLAN_AGENT_DOCKER_GID": "42"}, nil)
	d.args = []string{"helper", "remove-volume", "--wait", "abc123", "--volume", "bayplan-agent", "--remove-container", "--keep-volume"}
	if code := run(d); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if want := []string{"drop", "remove-volume"}; !reflect.DeepEqual(h.events, want) {
		t.Fatalf("events = %v, want %v: no connect loop, and the privileges are dropped first", h.events, want)
	}
	o := h.gotRemove
	if o.ContainerID != "abc123" || o.Volume != "bayplan-agent" || o.SocketPath != "/s/docker.sock" || !o.RemoveContainer || !o.KeepVolume {
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
