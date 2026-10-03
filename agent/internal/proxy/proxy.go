// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package proxy is the agent's policy: an allowlisting reverse proxy in front of
// the Docker socket, served once per multiplexed stream. The server is not
// trusted with the host (D-27, PITFALLS 1): whatever it asks for, the agent
// forwards only a read-only set of Engine API calls and refuses the rest with a
// typed answer.
//
// Go's own HTTP parser decides what a request is. The agent never filters bytes
// by hand, so a request smuggled past a hand-written parser cannot exist.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DeniedPrefix starts the message of every refusal. The server's Docker client
// surfaces the message in its error, so the prefix is how a refusal is told
// from an engine error (D-29).
const DeniedPrefix = "agent_policy_denied"

const (
	// maxQuery caps the query string, so a request cannot carry a payload in it.
	maxQuery = 4 << 10
	// maxShownPath caps how much of a refused path is echoed back.
	maxShownPath = 256
	// readHeaderTimeout bounds a stream that opens and sends nothing.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes a keep-alive stream that has gone quiet.
	idleTimeout = 120 * time.Second
)

// allowed is the whole read-only surface of Phase 2: ping, version, info, the
// four resource lists the dashboard reads, events, and the swarm lists. The
// optional /vX.Y prefix is the API version the server's client negotiated.
//
// GET /swarm is deliberately absent: its answer carries the swarm join tokens,
// which are credentials to add nodes (RESEARCH Pitfall 2). The cluster ID is
// read from /info on a manager instead.
var allowed = regexp.MustCompile(
	`^/(v[0-9]+\.[0-9]+/)?(_ping|version|info|containers/json|images/json|volumes|networks|events|nodes|services|tasks)$`)

// Handler returns the policy handler for the Docker socket at socketPath.
func Handler(socketPath string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite, unlike Director, drops any X-Forwarded-* the server
			// sent. The host is a placeholder: the transport dials the socket.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "docker"
			pr.Out.Host = "docker"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
			DisableCompression: true,
		},
		// Flush every write: /events is a stream and must not be buffered.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The engine did not answer. The message names the cause without
			// echoing the socket path.
			msg := "agent_engine_unreachable: the Docker engine did not answer"
			if errors.Is(err, context.Canceled) {
				msg = "agent_request_canceled"
			}
			writeJSON(w, http.StatusBadGateway, msg, false)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check(r) != "" {
			writeJSON(w, http.StatusForbidden, DeniedPrefix+": "+r.Method+" "+shownPath(r), true)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// check returns "" when the request may be forwarded, else a short reason. The
// reason is for the agent's own log and tests; the answer to the server is always
// the same stable message.
func check(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "method"
	}
	rawPath, query, _ := strings.Cut(r.RequestURI, "?")
	if len(query) > maxQuery {
		return "query"
	}
	if hasControl(r.RequestURI) {
		return "control"
	}
	// The raw request target must already be clean: no //, no .., no encoded
	// slash, no absolute-form URL. The expression only matches [a-z0-9_./].
	if rawPath != path.Clean(rawPath) || !allowed.MatchString(rawPath) {
		return "path"
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		return "body"
	}
	if r.Header.Get("Upgrade") != "" || connectionUpgrade(r.Header) {
		return "upgrade"
	}
	return ""
}

func connectionUpgrade(h http.Header) bool {
	for _, v := range h.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// shownPath is the request target echoed in a refusal, without its query and cut
// to a fixed length. It is data from the server, so it is never trusted to be
// short or printable (hasControl requests never get this far, but a method can).
func shownPath(r *http.Request) string {
	p, _, _ := strings.Cut(r.RequestURI, "?")
	if len(p) > maxShownPath {
		p = p[:maxShownPath]
	}
	return strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return '?'
		}
		return c
	}, p)
}

// writeJSON answers {"message": msg}. closeConn ends the stream after a refusal:
// a request that carried a body would otherwise leave unread bytes that the
// next request on the stream would be parsed from.
func writeJSON(w http.ResponseWriter, status int, msg string, closeConn bool) {
	body, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{msg})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if closeConn {
		h.Set("Connection", "close")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ServeConn serves HTTP on one connection with h and returns when the
// connection is done. It is run once per accepted multiplexed stream.
func ServeConn(conn net.Conn, h http.Handler) {
	l := newOneConnListener(conn)
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout and no ReadTimeout: /events lives for hours.
	}
	_ = srv.Serve(l)
}

// oneConnListener hands out its connection once and then blocks until that
// connection is closed, so http.Server.Serve returns exactly when the stream
// ends.
type oneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{})}
	l.conn = &signalConn{Conn: c, done: func() { l.once.Do(func() { close(l.closed) }) }}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "stream" }
func (pipeAddr) String() string  { return "stream" }

// signalConn tells the listener when the connection is closed.
type signalConn struct {
	net.Conn
	done func()
}

func (c *signalConn) Close() error {
	err := c.Conn.Close()
	c.done()
	return err
}
