// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/hashicorp/yamux"
)

// liveSession is a Session over a yamux pair that is still open, so isGone is false.
func liveSession(t *testing.T) *Session {
	t.Helper()
	a, b := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	srv, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	return &Session{AgentID: "agent-1", ym: srv, done: make(chan struct{})}
}

// A dial whose first write hit the dead connection gets the connection's own error
// from yamux before yamux closes the session, so the session still reads as open.
// That dial must still say ErrAgentGone (CI run 39162361452).
func TestADialFailedByTheConnectionIsAgentGoneBeforeTheSessionReadsClosed(t *testing.T) {
	s := liveSession(t)
	if s.isGone() {
		t.Fatal("the session reads as gone; the test needs a live one")
	}
	tr := StreamTransport{s: s}
	for _, raw := range []error{
		errors.New("failed to write msg: write tcp 10.0.0.1:7480->10.0.0.2:51234: write: broken pipe"),
		io.ErrClosedPipe,
		io.EOF,
	} {
		if err := tr.wrap(raw); !errors.Is(err, ErrAgentGone) {
			t.Errorf("wrap(%v) = %v, want ErrAgentGone", raw, err)
		}
	}
}

// yamux's own errors on a live session keep their meaning: a full session, a closed
// or reset stream, a go-away or a timeout is not the agent being gone.
func TestADialFailedByYamuxItselfOnALiveSessionIsNotAgentGone(t *testing.T) {
	s := liveSession(t)
	tr := StreamTransport{s: s}
	for _, own := range muxErrors {
		if errors.Is(own, yamux.ErrSessionShutdown) {
			continue // the session shutting down is the agent being gone
		}
		if err := tr.wrap(own); errors.Is(err, ErrAgentGone) {
			t.Errorf("wrap(%v) = %v, want it not ErrAgentGone on a live session", own, err)
		}
	}
	if err := tr.wrap(ErrBusy); errors.Is(err, ErrAgentGone) {
		t.Errorf("wrap(ErrBusy) = %v, want it not ErrAgentGone", err)
	}
	if err := tr.wrap(yamux.ErrSessionShutdown); !errors.Is(err, ErrAgentGone) {
		t.Errorf("wrap(ErrSessionShutdown) = %v, want ErrAgentGone", err)
	}
}
