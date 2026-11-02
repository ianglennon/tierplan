// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package proxy is the agent's policy: an allowlisting reverse proxy in front of
// the Docker socket, served once per multiplexed stream. The server is not
// trusted with the host (D-27, PITFALLS 1): whatever it asks for, the agent
// forwards only the Engine API calls in its route table (routes.go), with the
// query keys and request bodies each one is documented to take, and refuses the
// rest with a typed answer. A host set read-only forwards reads and nothing else.
//
// Go's own HTTP parser decides what a request is. The agent never filters bytes
// by hand, so a request smuggled past a hand-written parser cannot exist.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/proto/wire"
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
	// maxRegistryAuth caps the X-Registry-Auth header, which holds a base64url
	// JSON registry credential.
	maxRegistryAuth = 16 << 10
)

// Policy is what the host owner set on this agent (D-28).
type Policy struct {
	// ReadOnly refuses every request that is not GET or HEAD, whatever the
	// allowlist says.
	ReadOnly bool
	// AllowPrivilegedServices lets a service spec that gives tasks host access
	// through, a container create that gives the container host access, and a volume
	// create that mounts a host path or a device or names a volume plugin, on every
	// agent-managed host: a swarm node or a standalone host
	// (TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES). On a manager it also lets a service
	// mount a named volume whose backing gives host access.
	AllowPrivilegedServices bool
}

// Allowed decides whether a request may be forwarded to the engine, and says why
// not when it may not. It is the whole decision, kept free of http types so that a
// table of tricky paths and a fuzz target can drive it directly. When it allows the
// request it also returns the kind of body the route takes, so the caller knows
// which validator the body must go through before it is forwarded.
//
// The reason is wire.DeniedReadOnly when the host set the read-only flag and the
// method would change something, checked first so that no future growth of the
// route table can reach a write on a read-only host (D-35); every other refusal is
// wire.DeniedAllowlist. The request is allowed only when its raw path is already
// clean (no //, no .., no encoded slash), has no control character anywhere, and
// is exactly one route of the table, with a query within maxQuery that carries only
// that route's keys and values, a body only where the route takes one, and an
// upgrade only where the route allows one.
func Allowed(method, rawPath, rawQuery string, hasBody, hasUpgrade bool, p Policy) (ok bool, reason string, body BodyKind) {
	if p.ReadOnly && method != http.MethodGet && method != http.MethodHead {
		return false, wire.DeniedReadOnly, BodyNone
	}
	if len(rawQuery) > maxQuery || hasControl(rawPath) || hasControl(rawQuery) || rawPath != path.Clean(rawPath) {
		return false, wire.DeniedAllowlist, BodyNone
	}
	rt, found := Match(method, rawPath)
	if !found || !queryAllowed(rt, rawQuery) {
		return false, wire.DeniedAllowlist, BodyNone
	}
	// A route that takes a body must get one, and one that does not must not.
	if hasBody != (rt.body != BodyNone) || (hasUpgrade && !rt.upgrade) {
		return false, wire.DeniedAllowlist, BodyNone
	}
	return true, "", rt.body
}

// queryAllowed reports whether every key of the query is one the route takes and
// every value of it matches the key's pattern, and every key the route requires is there. It parses the query the way the
// engine does, so a key or value spelled with escapes is judged as the engine will
// read it.
func queryAllowed(rt route, rawQuery string) bool {
	if rt.anyQuery {
		return true
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	for key, values := range q {
		pattern, ok := rt.query[key]
		if !ok {
			return false
		}
		for _, v := range values {
			if !pattern.MatchString(v) {
				return false
			}
		}
	}
	for _, key := range rt.required {
		if len(q[key]) == 0 {
			return false
		}
	}
	return true
}

// Handler returns the policy handler for the Docker socket at socketPath. Every
// request it refuses is answered 403 with the typed message and reported once to
// onDenied, which may be nil, with the method, the path (capped, without the query)
// and the reason; onDenied runs on the request's goroutine and must not block.
func Handler(socketPath string, policy Policy, onDenied func(method, path, reason string)) http.Handler {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	lookup := networkLookupOver(transport)
	volumes := volumeLookupOver(transport)
	containers := containerLookupOver(transport)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite, unlike Director, drops any X-Forwarded-* the server
			// sent. The host is a placeholder: the transport dials the socket.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "docker"
			pr.Out.Host = "docker"
			// ReverseProxy strips the hop-by-hop headers before Rewrite runs, but it
			// puts `TE: trailers` back for backends that care about trailers. The
			// engine does not, and nothing but the request itself should cross.
			pr.Out.Header.Del("Te")
		},
		Transport: transport,
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
		rawPath, query, _ := strings.Cut(r.RequestURI, "?")
		hasBody := r.ContentLength != 0 || len(r.TransferEncoding) > 0
		hasUpgrade := r.Header.Get("Upgrade") != "" || connectionUpgrade(r.Header)
		deny := func(reason string) {
			if onDenied != nil {
				onDenied(r.Method, shownPath(r), reason)
			}
			// A host-policy refusal says so, so the server can name the setting that
			// lifts it; every other refusal is the bare prefix.
			prefix := DeniedPrefix + ": "
			if reason == wire.DeniedHostPolicy {
				prefix += wire.DeniedHostPolicy + ": "
			}
			writeJSON(w, http.StatusForbidden, prefix+r.Method+" "+shownPath(r), true)
		}
		ok, reason, kind := Allowed(r.Method, rawPath, query, hasBody, hasUpgrade, policy)
		if !ok {
			deny(reason)
			return
		}
		// A registry credential crosses only on the routes that use one, and then
		// only as a single well-formed value. The value is never decoded, logged or
		// kept: the only thing done with it is a length and character check.
		r.Header.Del("X-Registry-Config")
		if rt, found := Match(r.Method, rawPath); !found || !rt.registryAuth {
			r.Header.Del("X-Registry-Auth")
		} else if !registryHeaderOK(r.Header.Values("X-Registry-Auth")) {
			deny(wire.DeniedAllowlist)
			return
		}
		if kind != BodyNone {
			// What is forwarded is the rebuilt body, never the bytes the server sent.
			rebuilt, err := validateBody(kind, http.MaxBytesReader(w, r.Body, bodyCap(kind)), bodyEnv{policy: policy, lookup: lookup, volumes: volumes, ctx: r.Context()})
			if err != nil {
				if errors.Is(err, errVolumeLookup) {
					// The engine did not answer a volume lookup. That is an engine fault,
					// not a policy refusal, so it is not reported to onDenied and nothing
					// is forwarded.
					writeJSON(w, http.StatusBadGateway, "agent_engine_unreachable: the Docker engine did not answer a volume lookup", false)
					return
				}
				if errors.Is(err, errHostPolicy) {
					deny(wire.DeniedHostPolicy)
				} else {
					deny(wire.DeniedAllowlist)
				}
				return
			}
			if kind == BodyArchive {
				// The tar is fine; the container it is for must be a stopped compose container.
				// A lookup the engine does not answer is an engine fault (502), never an allow.
				if err := archiveTargetOK(r.Context(), archiveContainerID(rawPath), containers); err != nil {
					if errors.Is(err, errContainerLookup) {
						writeJSON(w, http.StatusBadGateway, "agent_engine_unreachable: the Docker engine did not answer a container lookup", false)
						return
					}
					deny(wire.DeniedAllowlist)
					return
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(rebuilt))
			r.ContentLength = int64(len(rebuilt))
			r.TransferEncoding = nil
			r.Header.Set("Content-Length", strconv.Itoa(len(rebuilt)))
			// The engine reads a form body as query keys when the type says form, so
			// the type is always the one the rebuilt body has.
			if kind == BodyArchive {
				r.Header.Set("Content-Type", "application/x-tar")
			} else {
				r.Header.Set("Content-Type", "application/json")
			}
			r.Header.Del("Content-Encoding")
		}
		rp.ServeHTTP(w, r)
	})
}

// lookupTimeout bounds the one network lookup a service-spec check makes.
const lookupTimeout = 5 * time.Second

// networkLookupOver asks the engine behind transport about a network: one GET
// /networks/{target}. A target that is not shaped like a network name or ID, or
// that the engine does not answer for, is an error, which the check reads as "not
// the host network"; the engine refuses an unknown network itself.
func networkLookupOver(transport http.RoundTripper) networkLookup {
	client := &http.Client{Transport: transport}
	return func(ctx context.Context, target string) (string, string, error) {
		if !idRe.MatchString(target) {
			return "", "", errors.New("not a network name or ID")
		}
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/networks/"+target, nil)
		if err != nil {
			return "", "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return "", "", fmt.Errorf("network lookup answered %d", resp.StatusCode)
		}
		var n struct{ Name, Driver string }
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxLookupAnswer)).Decode(&n); err != nil {
			return "", "", err
		}
		return n.Name, n.Driver, nil
	}
}

// maxLookupAnswer caps what is read of a network inspect. Only the name and the
// driver are used, but a network with many containers has a long answer.
const maxLookupAnswer = 8 << 20

// maxVolumeAnswer caps what is read of a volume inspect. Only the driver and the
// options are used.
const maxVolumeAnswer = 1 << 20

// volumeLookupOver asks the engine behind transport about a named volume: one GET
// /volumes/{name}. A 404 is a volume the engine does not have, found false with no
// error. A name not shaped like a volume name, any other status, a timeout or an
// answer that cannot be read is an error, which the check reads as host access.
func volumeLookupOver(transport http.RoundTripper) volumeLookup {
	client := &http.Client{Transport: transport}
	return func(ctx context.Context, name string) (string, map[string]string, bool, error) {
		if !volumeNameRe.MatchString(name) {
			return "", nil, false, errors.New("not a volume name")
		}
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/volumes/"+name, nil)
		if err != nil {
			return "", nil, false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", nil, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch resp.StatusCode {
		case http.StatusNotFound:
			return "", nil, false, nil
		case http.StatusOK:
		default:
			return "", nil, false, fmt.Errorf("volume lookup answered %d", resp.StatusCode)
		}
		var v struct {
			Driver  string
			Options map[string]string
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxVolumeAnswer)).Decode(&v); err != nil {
			return "", nil, false, err
		}
		return v.Driver, v.Options, true, nil
	}
}

// maxContainerAnswer caps what is read of a container inspect. Only the labels and the running
// flag are used, but the answer carries the whole config and the mounts.
const maxContainerAnswer = 8 << 20

// archiveRe reads the container out of the path of an archive write, which Allowed has already
// matched against the route.
var archiveRe = regexp.MustCompile(`^/(?:v[0-9]+\.[0-9]+/)?containers/(` + idShape + `)/archive$`)

// archiveContainerID is the container an archive path names, or "" for any other path.
func archiveContainerID(rawPath string) string {
	m := archiveRe.FindStringSubmatch(rawPath)
	if m == nil {
		return ""
	}
	return m[1]
}

// containerLookupOver asks the engine behind transport about a container: one GET
// /containers/{id}/json, read for Config.Labels and State.Running. A 404 is a container the engine
// does not have, found false with no error. A name not shaped like a container name or ID, any
// other status, a timeout or an answer that cannot be read is an error, which archiveTargetOK
// reads as an engine fault and never as permission.
func containerLookupOver(transport http.RoundTripper) containerLookup {
	client := &http.Client{Transport: transport}
	return func(ctx context.Context, id string) (map[string]string, bool, bool, error) {
		if !idRe.MatchString(id) {
			return nil, false, false, errors.New("not a container name or ID")
		}
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		// G704: id is shaped by idRe above and the host is the fixed placeholder; the transport
		// dials the engine's unix socket whatever the URL says.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+id+"/json", nil) //nolint:gosec // G704, see above
		if err != nil {
			return nil, false, false, err
		}
		resp, err := client.Do(req) //nolint:gosec // G704, see above
		if err != nil {
			return nil, false, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, false, false, nil
		case http.StatusOK:
		default:
			return nil, false, false, fmt.Errorf("container lookup answered %d", resp.StatusCode)
		}
		var c struct {
			State  struct{ Running bool }
			Config struct{ Labels map[string]string }
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxContainerAnswer)).Decode(&c); err != nil {
			return nil, false, false, err
		}
		return c.Config.Labels, c.State.Running, true, nil
	}
}

// registryHeaderOK reports whether the values of an X-Registry-Auth header may be
// forwarded: none at all, or exactly one that is empty or at most maxRegistryAuth
// bytes of base64url. A second value is refused because the engine would read only
// one of them, and a character outside base64url because the value is only ever
// the encoding of a JSON credential.
func registryHeaderOK(values []string) bool {
	switch len(values) {
	case 0:
		return true
	case 1:
	default:
		return false
	}
	v := values[0]
	if len(v) > maxRegistryAuth {
		return false
	}
	end := len(v)
	for end > 0 && v[end-1] == '=' {
		end--
	}
	if len(v)-end > 2 || (v != "" && end == 0) {
		return false
	}
	for i := range end {
		switch c := v[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
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
