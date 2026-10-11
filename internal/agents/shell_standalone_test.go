// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// output collects what an exec session prints so a test can wait for text.
type output struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func collect(r io.Reader) *output {
	o := &output{done: make(chan struct{})}
	go func() {
		defer close(o.done)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				o.mu.Lock()
				o.buf.Write(b[:n])
				o.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *output) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *output) waitFor(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(o.text(), want) {
			return
		}
		select {
		case <-o.done:
			if strings.Contains(o.text(), want) {
				return
			}
			t.Fatalf("the stream ended before %q appeared; output %q", want, o.text())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("%q did not appear within %s; output %q", want, within, o.text())
}

// TestStandaloneAgentExec opens a TTY shell in a container on a standalone host
// through the real agent: the upgrade crosses the yamux tunnel and the agent's
// reverse proxy, typed bytes reach the shell, its output comes back, a resize is
// seen by stty and exit 3 is reported as exit code 3 (plan 04-04, OBS-03, the
// riskiest path of the phase).
func TestStandaloneAgentExec(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)
	agents := srv.Agents(env.ID)
	if len(agents) != 1 {
		t.Fatalf("agents = %+v, want the one that enrolled", agents)
	}
	node := agents[0].ID

	sh(t, host, "docker run -d --name sh1 alpine sleep 600")
	sh1 := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.Id}}' sh1"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := srv.Core().Aggregator().WriteOnNode(ctx, env.ID, node, func(c *dockerx.Client) error {
		cmd, err := dockerx.ShellCommand("auto", "")
		if err != nil {
			return err
		}
		s, err := c.StartExec(ctx, sh1, dockerx.ExecOptions{Cmd: cmd, Cols: 80, Rows: 24})
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		out := collect(s)

		if _, err := s.Write([]byte("echo hello-$((1+2))-bayplan\n")); err != nil {
			return err
		}
		out.waitFor(t, "hello-3-bayplan", 20*time.Second)

		if err := s.Resize(ctx, 100, 30); err != nil {
			return err
		}
		if _, err := s.Write([]byte("stty size\n")); err != nil {
			return err
		}
		out.waitFor(t, "30 100", 20*time.Second)

		if _, err := s.Write([]byte("exit 3\n")); err != nil {
			return err
		}
		select {
		case <-out.done:
		case <-time.After(20 * time.Second):
			t.Error("the stream did not end after exit 3")
		}
		wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
		defer wcancel()
		st, err := s.Wait(wctx)
		if err != nil {
			return err
		}
		if st.Running || st.ExitCode != 3 {
			t.Errorf("state after exit 3 = %+v, want not running with exit code 3", st)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("exec through the agent: %v", err)
	}
}
