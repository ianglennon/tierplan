// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package privdrop

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// rig fakes every privileged call and records them, in order. The process
// starts as root and becomes UID 1000 when the fake Setuid runs.
type rig struct {
	euid      int
	socketGID int // 0 means no socket
	calls     []string
	failAt    string // the name of the call that fails
}

func newRig() *rig { return &rig{euid: 0} }

func (r *rig) fail(name string) error {
	if r.failAt == name {
		return errors.New(name + " failed")
	}
	return nil
}

func (r *rig) options(socket string) Options {
	return Options{
		SocketPath: socket,
		Geteuid:    func() int { return r.euid },
		Stat: func(p string) (int, error) {
			if p == socket && r.socketGID != 0 {
				return r.socketGID, nil
			}
			return 0, errors.New("no such socket")
		},
		Setgroups: func(g []int) error {
			r.calls = append(r.calls, "Setgroups"+fmtInts(g))
			return r.fail("Setgroups")
		},
		Setgid: func(g int) error {
			r.calls = append(r.calls, fmt.Sprintf("Setgid(%d)", g))
			return r.fail("Setgid")
		},
		Setuid: func(u int) error {
			r.calls = append(r.calls, fmt.Sprintf("Setuid(%d)", u))
			if err := r.fail("Setuid"); err != nil {
				return err
			}
			r.euid = u
			return nil
		},
		NoNewPrivs: func() error {
			r.calls = append(r.calls, "NoNewPrivs")
			return r.fail("NoNewPrivs")
		},
	}
}

func fmtInts(v []int) string { return fmt.Sprint(v) }

func TestDropJoinsTheSocketGroupThenDropsThenSetsNoNewPrivileges(t *testing.T) {
	r := newRig()
	r.socketGID = 998
	res, err := Drop(r.options("/var/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Setgroups[998]", "Setgid(1000)", "Setuid(1000)", "NoNewPrivs"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
	if !res.Dropped || res.GID != 998 {
		t.Fatalf("Result = %+v, want Dropped with GID 998", res)
	}
}

func TestDropUsesTheConfiguredDockerGIDOverTheSocketGroup(t *testing.T) {
	r := newRig()
	r.socketGID = 998
	gid := 42
	o := r.options("/var/run/docker.sock")
	o.DockerGID = &gid
	res, err := Drop(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "Setgroups[42]" {
		t.Fatalf("first call = %q, want Setgroups[42]", r.calls[0])
	}
	if res.GID != 42 {
		t.Fatalf("Result.GID = %d, want 42", res.GID)
	}
}

func TestDropJoinsNoGroupWhenThereIsNoSocket(t *testing.T) {
	r := newRig()
	res, err := Drop(r.options("/var/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "Setgroups[]" {
		t.Fatalf("first call = %q, want Setgroups[] (a missing socket is not an error)", r.calls[0])
	}
	if res.GID != NoGroup || !res.Dropped {
		t.Fatalf("Result = %+v, want Dropped with NoGroup", res)
	}
}

func TestDropChangesNothingWhenNotRoot(t *testing.T) {
	r := newRig()
	r.euid = 1000
	r.socketGID = 998
	res, err := Drop(r.options("/var/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %v, want none", r.calls)
	}
	if res.Dropped {
		t.Fatalf("Result = %+v, want Dropped false", res)
	}
}

func TestDropStopsAtTheFirstFailingCall(t *testing.T) {
	order := []string{"Setgroups", "Setgid", "Setuid", "NoNewPrivs"}
	for i, name := range order {
		t.Run(name, func(t *testing.T) {
			r := newRig()
			r.socketGID = 998
			r.failAt = name
			_, err := Drop(r.options("/var/run/docker.sock"))
			if err == nil {
				t.Fatalf("want an error when %s fails", name)
			}
			if !strings.Contains(err.Error(), name+" failed") {
				t.Errorf("the error %q does not carry the cause", err)
			}
			if got := len(r.calls); got != i+1 {
				t.Errorf("%d calls made (%v), want %d: nothing runs after a failure", got, r.calls, i+1)
			}
		})
	}
}

func TestDropFailsWhenStillRootAfterSetuid(t *testing.T) {
	r := newRig()
	o := r.options("/var/run/docker.sock")
	o.Setuid = func(int) error { return nil } // pretends to succeed, changes nothing
	_, err := Drop(o)
	if err == nil {
		t.Fatal("want an error when the process is still root after setuid")
	}
	if !strings.Contains(err.Error(), "still running as uid 0") {
		t.Errorf("the error %q does not say the process is still root", err)
	}
}

// The agent logs nothing from here, but a future change must not start
// writing the socket path's neighbours or the token into a log (T-02-18).
func TestDropLogsNothingSecret(t *testing.T) {
	const secret = "bpjt_SECRETVALUE_zzz" //nolint:gosec // a test fixture, not a credential
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("BAYPLAN_AGENT_JOIN_TOKEN", secret)
	r := newRig()
	r.socketGID = 998
	if _, err := Drop(r.options("/var/run/docker.sock")); err != nil {
		t.Fatal(err)
	}
	r2 := newRig()
	r2.failAt = "Setuid"
	_, err := Drop(r2.options("/var/run/docker.sock"))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) {
		t.Fatalf("the token leaked: log=%q err=%v", logs.String(), err)
	}
}
