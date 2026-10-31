// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package tunnel_test

import (
	"errors"
	"testing"

	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/internal/tunnel/tunneltest"
	"github.com/ianglennon/coaming/proto/testsupport"
	"github.com/ianglennon/coaming/proto/wire"
)

// The server reads an agent's host routes through one server-opened stream, and only
// from an agent that said in its handshake that it can answer (plan 10-05, D-17).

// hostNetAgent enrols an agent with the given hostNet fact whose streams are served
// by AnswerHostNet, and returns its connection and the hub's session.
func hostNetAgent(t *testing.T, hostNet string, routes []wire.HostRoute) (*tunneltest.Server, *tunneltest.Conn, *tunnel.Session) {
	t.Helper()
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, HostNet: hostNet, HostRoutes: routes})
	if c.Welcome == nil {
		t.Fatalf("enrol was refused: %+v", c.Refuse)
	}
	c.AnswerHostNet(t)
	s, err := srv.Hub.AwaitSession(testsupport.Context(t), c.Identity.AgentID, func(s *tunnel.Session) bool { return s != nil })
	if err != nil {
		t.Fatal(err)
	}
	return srv, c, s
}

func TestSessionHostNetReadsOneDocument(t *testing.T) {
	want := []wire.HostRoute{
		{Iface: "wg0", Dst: "10.8.0.0/24"},
		{Iface: "eth0", Dst: "192.168.2.0/24", Gateway: "192.168.1.1"},
	}
	srv, c, s := hostNetAgent(t, wire.HostNetOK, want)

	doc, err := s.HostNet(testsupport.Context(t))
	if err != nil {
		t.Fatal(err)
	}
	if doc.V != 1 || len(doc.Routes) != 2 || doc.Routes[0] != want[0] || doc.Routes[1] != want[1] {
		t.Fatalf("document = %+v, want the two routes the agent holds", doc)
	}
	if got := c.HostNetStreams.Load(); got != 1 {
		t.Errorf("the agent saw %d host-net streams, want 1", got)
	}

	// The same through the hub, by agent ID, and the view tells the picker the agent is ready.
	if _, err := srv.Hub.HostNet(testsupport.Context(t), c.Identity.AgentID); err != nil {
		t.Fatalf("Hub.HostNet: %v", err)
	}
	if v := viewOf(t, srv, s.EnvID, c.Identity.AgentID); v.HostNet != wire.HostNetOK {
		t.Errorf("view HostNet = %q, want %q", v.HostNet, wire.HostNetOK)
	}
	if _, err := srv.Hub.HostNet(testsupport.Context(t), "no-such-agent"); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Errorf("Hub.HostNet of an agent with no session = %v, want ErrAgentGone", err)
	}
}

func TestSessionHostNetRefusedWithoutOK(t *testing.T) {
	for name, hostNet := range map[string]string{"an older agent that reports nothing": "", "an agent whose route file is not mounted": wire.HostNetUnmounted} {
		t.Run(name, func(t *testing.T) {
			srv, c, s := hostNetAgent(t, hostNet, nil)
			_, err := s.HostNet(testsupport.Context(t))
			if !errors.Is(err, tunnel.ErrHostNetUnsupported) {
				t.Fatalf("HostNet = %v, want ErrHostNetUnsupported", err)
			}
			if got := c.Streams.Load(); got != 0 {
				t.Fatalf("the agent was opened %d streams, want none: an old agent would log an unknown kind and a denied audit row", got)
			}
			if v := viewOf(t, srv, s.EnvID, c.Identity.AgentID); v.HostNet != hostNet {
				t.Errorf("view HostNet = %q, want %q", v.HostNet, hostNet)
			}
		})
	}
}

func TestAnUnknownHostNetFactReadsAsNothing(t *testing.T) {
	srv, c, s := hostNetAgent(t, "surprise", nil)
	if v := viewOf(t, srv, s.EnvID, c.Identity.AgentID); v.HostNet != "" {
		t.Errorf("view HostNet = %q, want it empty for a value the server does not know", v.HostNet)
	}
	if _, err := s.HostNet(testsupport.Context(t)); !errors.Is(err, tunnel.ErrHostNetUnsupported) {
		t.Errorf("HostNet = %v, want ErrHostNetUnsupported", err)
	}
}
