// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"net"
	"net/http"
	"strings"
)

// acmeChallengePrefix is where an ACME HTTP-01 challenge is fetched. It is the only
// path TierPlan answers over plain HTTP, and it is compared raw: ACME validates at the
// host root, so a base path is never stripped from it.
const acmeChallengePrefix = "/.well-known/acme-challenge/"

// plainHeadLimit caps the bytes the listener reads from a plain connection while it
// looks for a request head. A head that is not complete inside it is closed unanswered.
const plainHeadLimit = 16 << 10

// recorder remembers every byte read through it, so a connection whose head was read
// by the listener can be replayed to net/http from byte 0.
type recorder struct {
	c   net.Conn
	buf bytes.Buffer
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.c.Read(p)
	r.buf.Write(p[:n])
	return n, err
}

// replayConn is a plain connection whose request head the listener already read. Read
// serves the recorded bytes first and the socket after them.
type replayConn struct {
	net.Conn
	pre *bytes.Reader
}

func newReplayConn(c net.Conn, recorded []byte) *replayConn {
	return &replayConn{Conn: c, pre: bytes.NewReader(recorded)}
}

func (c *replayConn) Read(p []byte) (int, error) {
	if c.pre.Len() > 0 {
		return c.pre.Read(p)
	}
	return c.Conn.Read(p)
}

// CloseWrite half-closes the connection. Embedding net.Conn hides *net.TCPConn's
// method, so it is forwarded here, as prefixConn does.
func (c *replayConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

var _ interface{ CloseWrite() error } = (*replayConn)(nil)

// newPlainHandler is what a plain HTTP request reaches. The listener hands on only a
// challenge head, so a request outside the challenge path can arrive only as a second
// request on a kept-alive plain connection; its connection is closed with nothing
// written. The challenge path goes to challenge, which Phase 14 replaces.
func newPlainHandler(challenge http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, acmeChallengePrefix) {
			challenge.ServeHTTP(w, r)
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	})
}

// notFoundChallenge answers every challenge request 404 until an ACME client is wired in.
var notFoundChallenge http.Handler = http.HandlerFunc(http.NotFound)

// plainOrFull routes a request by how it arrived: a plain one reaches only the
// challenge handler, a TLS one reaches the full handler. Handler() stays scheme-free.
func plainOrFull(full, challenge http.Handler) http.Handler {
	if challenge == nil {
		challenge = notFoundChallenge
	}
	plain := newPlainHandler(challenge)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			plain.ServeHTTP(w, r)
			return
		}
		full.ServeHTTP(w, r)
	})
}
