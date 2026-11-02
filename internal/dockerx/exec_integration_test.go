// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// screen collects what a session prints, so a test can wait for a piece of text
// without sleeping for a fixed time. Every write wakes the waiters, and the end of
// the stream does too, so a wait is a signal and never a poll.
type screen struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{} // closed and replaced on every write and at the end
	ended   bool
	done    chan struct{} // closed when the stream ends
}

func watch(r io.Reader) *screen {
	s := &screen{changed: make(chan struct{}), done: make(chan struct{})}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(b[:n])
				s.broadcastLocked()
				s.mu.Unlock()
			}
			if err != nil {
				s.mu.Lock()
				s.ended = true
				s.broadcastLocked()
				s.mu.Unlock()
				close(s.done)
				return
			}
		}
	}()
	return s
}

// broadcastLocked wakes every waiter. The caller holds s.mu.
func (s *screen) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// awaitOutput blocks until the output contains want, failing after testsupport.Deadline with
// what was wanted and what arrived. It wakes on every write the session makes.
func (s *screen) awaitOutput(t *testing.T, want string) {
	t.Helper()
	ctx := testsupport.Context(t)
	for {
		s.mu.Lock()
		got, ended, changed := s.buf.String(), s.ended, s.changed
		s.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		if ended {
			t.Fatalf("the stream ended before %q appeared; output: %q", want, got)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("%q did not appear within %s; output: %q", want, testsupport.Deadline, s.text())
		}
	}
}

func pullImage(t *testing.T, ctx context.Context, raw *client.Client, ref string) {
	t.Helper()
	pull, err := raw.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}
}

func runningContainer(t *testing.T, ctx context.Context, raw *client.Client, image string) string {
	t.Helper()
	name := "tp-exec-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: image, Cmd: []string{"sleep", "300"}},
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := raw.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	return name
}

// TestIntegrationExecSession drives a shell in a real container through dockerx
// (plan 04-04, OBS-03): bytes go in, output comes back, a resize is seen by stty,
// and exit 3 ends the stream with exit code 3.
func TestIntegrationExecSession(t *testing.T) {
	host := dockertest.DockerHost(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)
	pullImage(t, ctx, raw, "alpine:3")
	pullImage(t, ctx, raw, "busybox:1")

	t.Run("type, read, resize and the exit code", func(t *testing.T) {
		name := runningContainer(t, ctx, raw, "alpine:3")
		cmd, err := dockerx.ShellCommand("auto", "")
		if err != nil {
			t.Fatal(err)
		}
		s, err := cli.StartExec(ctx, name, dockerx.ExecOptions{Cmd: cmd, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatalf("StartExec: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		out := watch(s)

		if _, err := s.Write([]byte("echo hello-$((1+2))-tierplan\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out.awaitOutput(t, "hello-3-tierplan")

		if _, err := s.Write([]byte("stty size\n")); err != nil {
			t.Fatal(err)
		}
		out.awaitOutput(t, "24 80")

		if err := s.Resize(ctx, 100, 30); err != nil {
			t.Fatalf("Resize: %v", err)
		}
		if _, err := s.Write([]byte("stty size\n")); err != nil {
			t.Fatal(err)
		}
		out.awaitOutput(t, "30 100")

		if st, err := s.Inspect(ctx); err != nil || !st.Running {
			t.Fatalf("Inspect while open = %+v, %v; want running", st, err)
		}

		if _, err := s.Write([]byte("exit 3\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-out.done:
		case <-time.After(testsupport.Deadline):
			t.Fatal("the stream did not end after exit 3")
		}
		st, err := s.Wait(testsupport.Context(t))
		if err != nil || st.Running || st.ExitCode != 3 {
			t.Fatalf("Wait = %+v, %v; want not running with exit code 3", st, err)
		}
		if st, err := s.Inspect(ctx); err != nil || st.Running || st.ExitCode != 3 {
			t.Fatalf("Inspect after exit = %+v, %v", st, err)
		}
		if err := s.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Logf("Close: %v", err)
		}
		_ = s.Close() // a second call is harmless
	})

	t.Run("an image without bash is no shell", func(t *testing.T) {
		name := runningContainer(t, ctx, raw, "busybox:1")
		start := time.Now()
		s, err := cli.StartExec(ctx, name, dockerx.ExecOptions{Cmd: []string{"/bin/bash"}, Cols: 80, Rows: 24})
		if err != nil {
			if !errors.Is(err, dockerx.ErrNoShell) {
				t.Fatalf("StartExec = %v, want ErrNoShell", err)
			}
			t.Logf("the engine refused at start: %v", err)
			return
		}
		t.Cleanup(func() { _ = s.Close() })
		out := watch(s)
		select {
		case <-out.done:
		case <-time.After(testsupport.Deadline):
			t.Fatalf("the stream did not end; output %q", out.text())
		}
		// Wait polls until the process ends or its context does, so a missing shell that is
		// not recognised ends as a deadline error here, not as ErrNoShell. No elapsed-time
		// bound is asserted: how fast it is recognised depends on the machine, not on the code.
		st, err := s.Wait(testsupport.Context(t))
		t.Logf("after %s: state %+v, err %v, output %q", time.Since(start), st, err, out.text())
		if !errors.Is(err, dockerx.ErrNoShell) {
			t.Fatalf("Wait = %+v, %v; want ErrNoShell", st, err)
		}
	})
}
