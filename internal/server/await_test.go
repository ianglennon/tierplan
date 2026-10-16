// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The waits of the tests that run the real agent binary against the in-process server (phase
// 6.1, D-07). The agent is a separate process, but what these tests wait for is state of the
// server in this process: the hub registers the agent's session and the monitor finds its
// engine online. Both announce it, so neither is polled. What only the agent process knows (a
// file it wrote, a line in its log) is still awaited with testsupport.Poll, marked external.

// sessionOfEnv is the live session of an agent of envID, or nil.
func sessionOfEnv(srv *Server, envID string) *tunnel.Session {
	for _, s := range srv.tunnel.Sessions() {
		if s.EnvID == envID {
			return s
		}
	}
	return nil
}

// awaitAgentOnline waits until an agent of envID has a session on the server and the monitor has
// probed that agent's engine and found it online (with its version read, when withEngine is
// set). exited, when it is not nil, is closed when the agent process ends: the wait then fails
// with the agent's output at once instead of running out its guard. It returns the session.
func awaitAgentOnline(t *testing.T, srv *Server, envID string, exited <-chan struct{}, agentLog fmt.Stringer, withEngine bool) *tunnel.Session {
	t.Helper()
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	defer cancel()
	if exited != nil {
		go func() {
			select {
			case <-exited:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	fail := func(what string, err error) {
		t.Helper()
		select {
		case <-exited:
			t.Fatalf("the agent exited before %s; its output:\n%s", what, agentLog)
		default:
		}
		t.Fatalf("gave up waiting for %s: %v\nagent output:\n%s", what, err, agentLog)
	}
	// The condition ignores the session it is handed (the agent's ID is not known yet) and looks
	// at the hub; the hub evaluates it after every registration and removal of a session.
	if _, err := srv.tunnel.AwaitSession(ctx, "", func(*tunnel.Session) bool { return sessionOfEnv(srv, envID) != nil }); err != nil {
		fail("the server registered a session of the agent", err)
	}
	s := sessionOfEnv(srv, envID)
	if s == nil {
		t.Fatalf("the session of the agent of %s ended again straight after it was registered\nagent output:\n%s", envID, agentLog)
	}
	st, err := srv.monitor.AwaitStatus(ctx, envs.AgentTarget(s.AgentID), func(st envs.Status) bool {
		return st.State == envs.StateOnline && (!withEngine || st.Probe.EngineVersion != "")
	})
	if err != nil {
		fail(fmt.Sprintf("the monitor found the agent's engine online (last status %+v)", st), err)
	}
	return s
}
