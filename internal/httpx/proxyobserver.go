// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kinds of proxy problem the observer records (D-53). The warning IDs of
// GET /api/warnings are these with a "proxy_" prefix.
const (
	// SignalUntrustedForwarded: a peer outside COAMING_TRUSTED_PROXIES sent
	// forwarded headers, which Coaming ignored.
	SignalUntrustedForwarded = "untrusted_forwarded"
	// SignalURLMismatch: a request arrived for a host, or over a scheme, that is
	// not the public URL's.
	SignalURLMismatch = "url_mismatch"
	// SignalWebSocketFailing: a request to the agent endpoint was not a WebSocket
	// upgrade, which is what a proxy that drops the Upgrade headers produces.
	SignalWebSocketFailing = "websocket_failing"
)

const (
	// SignalTTL is how long a signal stays after it was last seen: the warning
	// goes away by itself once the cause has stopped.
	SignalTTL = time.Hour
	// MaxSignals bounds the observer's memory against a scanner (T-02-81).
	MaxSignals = 256
)

// Signal is one kind of proxy problem as seen from one peer, host and scheme.
// Which of Peer, Host and Scheme are set depends on the kind: Peer for an
// untrusted forwarded header and a failing upgrade, Host and Scheme for a URL
// mismatch.
type Signal struct {
	Kind      string
	Peer      string
	Host      string
	Scheme    string
	FirstSeen time.Time
	LastSeen  time.Time
	Count     int
}

// signalKey is what makes two sightings the same signal.
type signalKey struct{ kind, peer, host, scheme string }

// ProxyObserver notices a reverse proxy that is misconfigured, from what reaches
// Coaming, and remembers it in memory for an hour. It reads no forwarded header
// for its value: the peer it records is the TCP peer, the host is the request's
// own Host, and the scheme is EffectiveScheme (D-51, D-53). It is safe for
// concurrent use.
type ProxyObserver struct {
	publicURL func() string
	now       func() time.Time

	mu    sync.Mutex
	sigs  map[signalKey]*Signal
	onNew func(kind string)
}

// NewProxyObserver returns an observer that compares requests with publicURL,
// read on every request so a change of the public URL applies at once. now
// defaults to time.Now.
func NewProxyObserver(publicURL func() string, now func() time.Time) *ProxyObserver {
	if now == nil {
		now = time.Now
	}
	if publicURL == nil {
		publicURL = func() string { return "" }
	}
	return &ProxyObserver{publicURL: publicURL, now: now, sigs: map[signalKey]*Signal{}}
}

// OnNew sets a function that runs, outside the observer's lock, when a kind of
// problem appears while none of that kind was live: the first sighting, and the
// first one after the last went away. The server publishes proxy.warning from it
// so an open bell updates at once. It must not block. Call it before the observer
// serves.
func (o *ProxyObserver) OnNew(f func(kind string)) *ProxyObserver {
	o.onNew = f
	return o
}

// Observe is the middleware. It belongs right after the client-address
// middleware, which says whether the TCP peer is a trusted proxy and which scheme
// the request effectively used. It never changes the request or the response.
func (o *ProxyObserver) Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		o.look(req)
		next.ServeHTTP(w, req)
	})
}

// look applies the two checks that need a request.
func (o *ProxyObserver) look(req *http.Request) {
	peer := peerOf(req.RemoteAddr)
	ctx := req.Context()
	if !peerTrusted(ctx) && hasForwardedHeader(req.Header) {
		o.record(signalKey{kind: SignalUntrustedForwarded, peer: peer})
	}
	scheme := EffectiveScheme(ctx)
	if want := o.publicURL(); want != "" && !isProbe(req, peer) {
		if mismatch(want, req.Host, scheme) {
			o.record(signalKey{kind: SignalURLMismatch, host: clipHost(strings.ToLower(req.Host)), scheme: scheme})
		}
	}
}

// RecordUpgradeFailure notes a request to the agent endpoint that was not a
// WebSocket upgrade. The tunnel hub calls it, with the client address, through
// its OnUpgradeFailure hook.
func (o *ProxyObserver) RecordUpgradeFailure(peer string) {
	o.record(signalKey{kind: SignalWebSocketFailing, peer: peer})
}

// Signals are the problems seen within the last hour, newest first. The result
// is a copy.
func (o *ProxyObserver) Signals() []Signal {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expire(o.now())
	out := make([]Signal, 0, len(o.sigs))
	for _, s := range o.sigs {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b Signal) int {
		if c := b.LastSeen.Compare(a.LastSeen); c != 0 {
			return c
		}
		return strings.Compare(a.Kind+a.Peer+a.Host, b.Kind+b.Peer+b.Host)
	})
	return out
}

// record counts one sighting, adds the signal when it is new and makes room by
// dropping the signal seen longest ago when MaxSignals are held.
func (o *ProxyObserver) record(k signalKey) {
	now := o.now()
	o.mu.Lock()
	o.expire(now)
	announce := false
	if s, ok := o.sigs[k]; ok {
		s.Count++
		s.LastSeen = now
	} else {
		announce = !o.live(k.kind)
		if len(o.sigs) >= MaxSignals {
			o.dropOldest()
		}
		o.sigs[k] = &Signal{
			Kind: k.kind, Peer: k.peer, Host: k.host, Scheme: k.scheme,
			FirstSeen: now, LastSeen: now, Count: 1,
		}
	}
	f := o.onNew
	o.mu.Unlock()
	if announce && f != nil {
		f(k.kind)
	}
}

// live reports whether any signal of the kind is held. The caller holds mu.
func (o *ProxyObserver) live(kind string) bool {
	for k := range o.sigs {
		if k.kind == kind {
			return true
		}
	}
	return false
}

// expire drops what was last seen an hour ago or more. The caller holds mu.
func (o *ProxyObserver) expire(now time.Time) {
	for k, s := range o.sigs {
		if now.Sub(s.LastSeen) >= SignalTTL {
			delete(o.sigs, k)
		}
	}
}

// dropOldest removes the signal seen longest ago. The caller holds mu.
func (o *ProxyObserver) dropOldest() {
	var oldest signalKey
	var at time.Time
	first := true
	for k, s := range o.sigs {
		if first || s.LastSeen.Before(at) {
			oldest, at, first = k, s.LastSeen, false
		}
	}
	if !first {
		delete(o.sigs, oldest)
	}
}

// maxHostLen is how much of a Host header a signal keeps; a DNS name is at most
// 253 characters, and a longer one is the caller's doing (T-02-81).
const maxHostLen = 253

func clipHost(h string) string {
	if len(h) > maxHostLen {
		return h[:maxHostLen]
	}
	return h
}

// peerOf is the host part of a TCP peer address.
func peerOf(remote string) string {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr().Unmap().String()
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// hasForwardedHeader reports whether the request carries any header a proxy
// would add. Only the names are looked at, never the values, and it is the one
// place the family of forwarded headers is touched at all.
func hasForwardedHeader(h http.Header) bool {
	for name := range h {
		if strings.HasPrefix(name, "X-Forwarded-") || name == "Forwarded" {
			return true
		}
	}
	return false
}

// isProbe is a request that says nothing about the proxy: a health probe, and
// anything from the machine itself, such as the container's own health check.
func isProbe(req *http.Request, peer string) bool {
	switch req.URL.Path {
	case "/healthz", "/readyz", AgentConnectPath:
		// The agent endpoint is dialled at the agent URL, which may be another
		// address than the public URL.
		return true
	}
	addr, err := netip.ParseAddr(peer)
	return err == nil && addr.IsLoopback()
}

// mismatch reports whether a request for host over scheme is not one for the
// public URL: another host, or another scheme. A default port is the same as none.
func mismatch(publicURL, host, scheme string) bool {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return false
	}
	return scheme != strings.ToLower(u.Scheme) ||
		hostOf(canonicalOrigin(scheme, host)) != hostOf(canonicalOrigin(strings.ToLower(u.Scheme), u.Host))
}
