// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

func execFake(t *testing.T, missing ...string) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{
			{ID: "web-id", Names: []string{"/web"}, Image: "nginx", State: "running", Status: "Up 3 hours"},
			{ID: "db-id", Names: []string{"/db"}, Image: "postgres", State: "exited", Status: "Exited (0) 1 hour ago"},
		},
		MissingExecutables: missing,
	})
	return newClient(t, fake), fake
}

func readUntil(t *testing.T, s *dockerx.ExecSession, want string) string {
	t.Helper()
	var got strings.Builder
	buf := make([]byte, 256)
	deadline := time.Now().Add(testsupport.Deadline)
	for !strings.Contains(got.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%q did not arrive; got %q", want, got.String())
		}
		n, err := s.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if strings.Contains(got.String(), want) {
				break
			}
			t.Fatalf("Read: %v before %q; got %q", err, want, got.String())
		}
	}
	return got.String()
}

func drain(t *testing.T, s *dockerx.ExecSession) string {
	t.Helper()
	var got strings.Builder
	buf := make([]byte, 256)
	for {
		n, err := s.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "closed") {
				t.Logf("stream ended with %v", err)
			}
			return got.String()
		}
	}
}

func TestExecRoundTripsBytesAndResizeReachesTheDaemon(t *testing.T) {
	c, fake := execFake(t)
	ctx := context.Background()
	s, err := c.StartExec(ctx, "web", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.ID() == "" {
		t.Error("the session has no exec ID")
	}
	if _, err := s.Write([]byte("hello-bayplan\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readUntil(t, s, "hello-bayplan")

	if err := s.Resize(ctx, 100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if got := fake.Resizes(); !slices.Equal(got, []string{"h=30&w=100"}) {
		t.Errorf("resizes = %v, want [h=30&w=100]", got)
	}
	if st, err := s.Inspect(ctx); err != nil || !st.Running {
		t.Errorf("Inspect while open = %+v, %v; want running", st, err)
	}
	for _, bad := range [][2]uint{{0, 10}, {10, 0}, {70000, 10}} {
		if err := s.Resize(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("Resize(%d, %d) succeeded, want a refusal", bad[0], bad[1])
		}
	}
}

func TestExecWaitReportsTheExitCodeOnceTheStreamEnds(t *testing.T) {
	c, _ := execFake(t)
	ctx := testsupport.Context(t)
	s, err := c.StartExec(ctx, "web", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}})
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Write([]byte("exit 2\n")); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	st, err := s.Wait(ctx)
	if err != nil || st.Running || st.ExitCode != 2 {
		t.Fatalf("Wait = %+v, %v; want not running with exit code 2", st, err)
	}
	if st, err := s.Inspect(ctx); err != nil || st.Running || st.ExitCode != 2 {
		t.Errorf("Inspect = %+v, %v", st, err)
	}
}

func TestExecWaitHonoursItsContext(t *testing.T) {
	c, _ := execFake(t)
	s, err := c.StartExec(context.Background(), "web", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// The shell never ends on its own, so only the context can end Wait. The test cancels it
	// itself instead of waiting for a short deadline to expire.
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		_, err := s.Wait(ctx)
		waited <- err
	}()
	cancel()
	if err := testsupport.Await(t, "Wait on a shell that still runs to end with its context", waited); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait on a shell that still runs = %v, want the context's cancellation", err)
	}
}

func TestExecAMissingExecutableIsErrNoShell(t *testing.T) {
	c, _ := execFake(t, "/bin/bash")
	ctx := testsupport.Context(t)
	s, err := c.StartExec(ctx, "web", dockerx.ExecOptions{Cmd: []string{"/bin/bash"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	out := drain(t, s)
	if !strings.Contains(out, "no such file or directory") {
		t.Errorf("output %q should carry the runtime's message", out)
	}
	st, err := s.Wait(ctx)
	if !errors.Is(err, dockerx.ErrNoShell) {
		t.Fatalf("Wait = %+v, %v; want ErrNoShell", st, err)
	}
	if st.ExitCode != 127 || st.Running {
		t.Errorf("state = %+v, want exit code 127", st)
	}
}

// A shell exits with the status of the last command, so a mistyped command followed by
// exit ends a working session with 127. That is not a missing shell (WR-02).
func TestExecAShellThatWasGivenInputAndEndsWith127IsNotErrNoShell(t *testing.T) {
	for _, code := range []string{"126", "127"} {
		t.Run(code, func(t *testing.T) {
			c, _ := execFake(t)
			ctx := testsupport.Context(t)
			s, err := c.StartExec(ctx, "web", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}})
			if err != nil {
				t.Fatalf("StartExec: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if _, err := s.Write([]byte("exit " + code + "\n")); err != nil {
				t.Fatal(err)
			}
			drain(t, s)
			st, err := s.Wait(ctx)
			if err != nil || st.Running || strconv.Itoa(st.ExitCode) != code {
				t.Fatalf("Wait = %+v, %v; want a normal end with exit code %s", st, err, code)
			}
		})
	}
}

func TestExecCloseTwiceIsHarmlessAndEndsTheStream(t *testing.T) {
	c, _ := execFake(t)
	s, err := c.StartExec(context.Background(), "web", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.Read(make([]byte, 8)); err == nil {
		t.Error("Read after Close succeeded, want an error")
	}
}

func TestExecRefusesAnUnknownOrStoppedContainerAndBadInput(t *testing.T) {
	c, fake := execFake(t)
	ctx := context.Background()
	if _, err := c.StartExec(ctx, "nope", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}}); !dockerx.IsNotFound(err) {
		t.Errorf("unknown container = %v, want not found", err)
	}
	if _, err := c.StartExec(ctx, "db", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}}); !dockerx.IsConflict(err) {
		t.Errorf("stopped container = %v, want a conflict", err)
	}
	before := len(fake.Writes())
	if _, err := c.StartExec(ctx, "web", dockerx.ExecOptions{}); err == nil {
		t.Error("an empty command was accepted")
	}
	if _, err := c.StartExec(ctx, "", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}}); err == nil {
		t.Error("an empty container was accepted")
	}
	if _, err := c.StartExec(ctx, "web", dockerx.ExecOptions{Cmd: []string{"/bin/sh"}, Cols: 70000, Rows: 24}); err == nil {
		t.Error("an absurd terminal size was accepted")
	}
	if got := len(fake.Writes()); got != before {
		t.Errorf("bad input still reached the daemon: %v", fake.Writes()[before:])
	}
}

// T-04-18: the create request carries the TTY, the attach flags, the size, the user,
// the working directory and the command, and nothing the agent would refuse.
func TestExecCreateSetsOnlyTheAllowedOptions(t *testing.T) {
	c, fake := execFake(t)
	s, err := c.StartExec(context.Background(), "web", dockerx.ExecOptions{
		Cmd: []string{"/bin/sh", "-c", "x"}, User: "root", WorkingDir: "/tmp", Cols: 90, Rows: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bodies := fake.ExecCreateBodies()
	if len(bodies) != 1 {
		t.Fatalf("create bodies = %v", bodies)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"User", "Tty", "ConsoleSize", "AttachStdin", "AttachStdout", "AttachStderr", "WorkingDir", "Cmd"}
	for k, v := range got {
		if !slices.Contains(allowed, k) {
			// Zero values the client always encodes are harmless; real settings are not.
			if b, ok := v.(bool); ok && !b {
				continue
			}
			if arr, ok := v.([]any); ok && len(arr) == 0 {
				continue
			}
			if str, ok := v.(string); ok && str == "" {
				continue
			}
			if v == nil {
				continue
			}
			t.Errorf("create body sets %s = %v, which is not allowed: %s", k, v, bodies[0])
		}
	}
	for _, k := range []string{"Tty", "AttachStdin", "AttachStdout", "AttachStderr"} {
		if got[k] != true {
			t.Errorf("%s = %v, want true", k, got[k])
		}
	}
	if got["User"] != "root" || got["WorkingDir"] != "/tmp" {
		t.Errorf("user and working directory = %v, %v", got["User"], got["WorkingDir"])
	}
	if cs, _ := got["ConsoleSize"].([]any); len(cs) != 2 || cs[0] != float64(20) || cs[1] != float64(90) {
		t.Errorf("ConsoleSize = %v, want [20 90] (height, width)", got["ConsoleSize"])
	}
}

func TestShellCommand(t *testing.T) {
	cases := []struct {
		name, kind, custom string
		want               []string
		wantErr            bool
	}{
		{"auto is one sh -c wrapper", "auto", "", []string{"/bin/sh", "-c", "if [ -x /bin/bash ]; then exec /bin/bash; else exec /bin/sh; fi"}, false},
		{"bash", "bash", "", []string{"/bin/bash"}, false},
		{"sh", "sh", "", []string{"/bin/sh"}, false},
		{"ash", "ash", "", []string{"/bin/ash"}, false},
		{"custom with double quotes", "custom", `/bin/zsh -c "echo a b"`, []string{"/bin/zsh", "-c", "echo a b"}, false},
		{"custom with single quotes", "custom", `/bin/sh -c 'echo "a b"'`, []string{"/bin/sh", "-c", `echo "a b"`}, false},
		{"custom with an escaped space", "custom", `/bin/my\ shell --x`, []string{"/bin/my shell", "--x"}, false},
		{"custom keeps an empty quoted argument", "custom", `cmd "" x`, []string{"cmd", "", "x"}, false},
		{"custom is not expanded", "custom", `/bin/sh -c $HOME;rm`, []string{"/bin/sh", "-c", "$HOME;rm"}, false},
		{"unbalanced double quote", "custom", `/bin/sh -c "echo`, nil, true},
		{"unbalanced single quote", "custom", `/bin/sh -c 'echo`, nil, true},
		{"trailing backslash", "custom", `/bin/sh \`, nil, true},
		{"empty custom", "custom", "   ", nil, true},
		{"too many arguments", "custom", strings.Repeat("a ", 65), nil, true},
		{"an argument that is too long", "custom", strings.Repeat("a", 4097), nil, true},
		{"unknown kind", "zsh", "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dockerx.ShellCommand(tc.kind, tc.custom)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ShellCommand(%q, %q) = %v, want an error", tc.kind, tc.custom, got)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("ShellCommand(%q, %q) = %q, %v; want %q", tc.kind, tc.custom, got, err, tc.want)
			}
		})
	}
	// 64 arguments are allowed.
	if got, err := dockerx.ShellCommand("custom", strings.TrimSpace(strings.Repeat("a ", 64))); err != nil || len(got) != 64 {
		t.Errorf("64 arguments = %d, %v, want accepted", len(got), err)
	}
}
