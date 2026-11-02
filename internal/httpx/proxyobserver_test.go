// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package httpx

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

const observedPublicURL = "https://tierplan.example.com"

var observedTrusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

// probe is a request as a test sends it through the client-IP middleware and the
// observer: the TCP peer, the Host and any headers.
type probe struct {
	peer, host, path string
	tls              bool
	headers          map[string]string
}

func (p probe) request() *http.Request {
	path := p.path
	if path == "" {
		path = "/"
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = p.peer
	if p.host != "" {
		req.Host = p.host
	}
	if p.tls {
		req.TLS = &tls.ConnectionState{}
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return req
}

type observerRig struct {
	obs *ProxyObserver
	now time.Time
	mu  sync.Mutex
	new []string
	url string
	h   http.Handler
}

func (r *observerRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *observerRig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *observerRig) appeared() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.new)
}

func newObserverRig(trusted []netip.Prefix, publicURL string) *observerRig {
	r := &observerRig{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), url: publicURL}
	r.obs = NewProxyObserver(func() string { return r.url }, r.clock).OnNew(func(kind string) {
		r.mu.Lock()
		r.new = append(r.new, kind)
		r.mu.Unlock()
	})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.h = ClientIPMiddleware(trusted)(r.obs.Observe(ok))
	return r
}

func (r *observerRig) send(p probe) {
	r.h.ServeHTTP(httptest.NewRecorder(), p.request())
}

func kinds(sigs []Signal) []string {
	out := make([]string, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, s.Kind)
	}
	return out
}

// goodViaProxy is a request that a correct proxy in front of TierPlan would send.
func goodViaProxy(extra map[string]string) probe {
	h := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"}
	for k, v := range extra {
		h[k] = v
	}
	return probe{peer: "10.1.2.3:4000", host: "tierplan.example.com", headers: h}
}

func TestEffectiveSchemeFollowsTLSAndOnlyATrustedProxy(t *testing.T) {
	cases := []struct {
		name string
		p    probe
		want string
	}{
		{"a TLS request", probe{peer: "198.51.100.7:1", tls: true}, "https"},
		{"a trusted peer saying https", probe{peer: "10.1.2.3:1", headers: map[string]string{"X-Forwarded-Proto": "https"}}, "https"},
		{"a trusted peer saying http", probe{peer: "10.1.2.3:1", headers: map[string]string{"X-Forwarded-Proto": "http"}}, "http"},
		{"a trusted peer saying nothing", probe{peer: "10.1.2.3:1"}, "http"},
		{"a trusted peer saying something else", probe{peer: "10.1.2.3:1", headers: map[string]string{"X-Forwarded-Proto": "gopher"}}, "http"},
		{"an untrusted peer saying https", probe{peer: "198.51.100.7:1", headers: map[string]string{"X-Forwarded-Proto": "https"}}, "http"},
		{"a chain whose last hop is trusted and says https", probe{peer: "10.1.2.3:1", headers: map[string]string{"X-Forwarded-Proto": "http, https"}}, "https"},
		{"a chain whose last hop says http", probe{peer: "10.1.2.3:1", headers: map[string]string{"X-Forwarded-Proto": "https, http"}}, "http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := ClientIPMiddleware(observedTrusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = EffectiveScheme(r.Context())
			}))
			h.ServeHTTP(httptest.NewRecorder(), tc.p.request())
			if got != tc.want {
				t.Errorf("EffectiveScheme = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveSchemeOutsideTheMiddlewareIsHTTP(t *testing.T) {
	if got := EffectiveScheme(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "http" {
		t.Errorf("EffectiveScheme with no middleware = %q, want http", got)
	}
}

func TestForwardedHeadersFromAnUntrustedPeerRecordTheTCPPeer(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.send(probe{
		peer: "198.51.100.7:5555", host: "tierplan.example.com", tls: true,
		headers: map[string]string{"X-Forwarded-For": "203.0.113.9"},
	})
	sigs := rig.obs.Signals()
	if len(sigs) != 1 || sigs[0].Kind != SignalUntrustedForwarded || sigs[0].Peer != "198.51.100.7" || sigs[0].Count != 1 {
		t.Fatalf("signals = %+v, want one untrusted_forwarded from the TCP peer 198.51.100.7", sigs)
	}
}

func TestForwardedHeadersFromATrustedPeerRecordNothing(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.send(goodViaProxy(nil))
	if sigs := rig.obs.Signals(); len(sigs) != 0 {
		t.Fatalf("signals = %+v, want none", sigs)
	}
}

func TestForwardedHostAndPrefixChangeNoSignal(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.send(goodViaProxy(map[string]string{
		"X-Forwarded-Host": "evil.example", "X-Forwarded-Prefix": "/elsewhere",
		"Forwarded": "for=6.6.6.6;host=evil.example;proto=http",
	}))
	if sigs := rig.obs.Signals(); len(sigs) != 0 {
		t.Fatalf("signals = %+v, want none: the public URL alone decides", sigs)
	}
}

func TestHostAndPrefixHeadersNeverWidenTheOriginCheck(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.TrustedProxies = observedTrusted })
	h := f.r.Handler()
	forged := func(r *http.Request) {
		r.Host = "tierplan.example"
		r.RemoteAddr = "10.1.2.3:4000"
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.Header.Set("X-Forwarded-Prefix", "/evil")
		r.Header.Set("Forwarded", "host=evil.example;proto=https")
	}
	if rec := do(h, http.MethodPost, "/api/public", forged, header("Origin", "https://evil.example")); rec.Code != http.StatusForbidden {
		t.Errorf("an Origin the forwarded host vouched for = %d, want 403", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/public", forged, header("Origin", publicOrigin)); rec.Code != http.StatusOK {
		t.Errorf("the public origin = %d, want 200; %s", rec.Code, rec.Body)
	}
}

func TestAHostThatIsNotThePublicURLRecordsAMismatchWithThatHost(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	p := goodViaProxy(nil)
	p.host = "other.example.com"
	rig.send(p)
	sigs := rig.obs.Signals()
	if len(sigs) != 1 || sigs[0].Kind != SignalURLMismatch || sigs[0].Host != "other.example.com" || sigs[0].Scheme != "https" {
		t.Fatalf("signals = %+v, want one url_mismatch for host other.example.com over https", sigs)
	}
}

func TestPlainHTTPThroughATrustedProxyThatSendsNoProtoRecordsAMismatchWithThatScheme(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.send(probe{peer: "10.1.2.3:4000", host: "tierplan.example.com", headers: map[string]string{"X-Forwarded-For": "203.0.113.9"}})
	sigs := rig.obs.Signals()
	if len(sigs) != 1 || sigs[0].Kind != SignalURLMismatch || sigs[0].Scheme != "http" || sigs[0].Host != "tierplan.example.com" {
		t.Fatalf("signals = %+v, want one url_mismatch over http", sigs)
	}
}

func TestADefaultPortInTheHostIsNotAMismatch(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	p := goodViaProxy(nil)
	p.host = "TierPlan.Example.com:443"
	rig.send(p)
	if sigs := rig.obs.Signals(); len(sigs) != 0 {
		t.Fatalf("signals = %+v, want none for the same host with its default port", sigs)
	}
}

func TestProbesAndLoopbackAreNotCheckedAgainstThePublicURL(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.send(probe{peer: "127.0.0.1:5000", host: "localhost:7480", path: "/healthz"})
	rig.send(probe{peer: "127.0.0.1:5000", host: "localhost:7480", path: "/readyz"})
	rig.send(probe{peer: "172.18.0.9:5000", host: "tierplan:7480", path: "/healthz"})
	rig.send(probe{peer: "172.18.0.9:5000", host: "tierplan:7480", path: "/readyz"})
	rig.send(probe{peer: "127.0.0.1:5000", host: "localhost:7480", path: "/api/version"})
	rig.send(probe{peer: "[::1]:5000", host: "localhost:7480", path: "/"})
	if sigs := rig.obs.Signals(); len(sigs) != 0 {
		t.Fatalf("signals = %+v, want none", sigs)
	}
}

func TestWithNoPublicURLNothingIsCompared(t *testing.T) {
	rig := newObserverRig(observedTrusted, "")
	rig.send(probe{peer: "198.51.100.7:5000", host: "box.lan:7480"})
	if sigs := rig.obs.Signals(); len(sigs) != 0 {
		t.Fatalf("signals = %+v, want none before a public URL is set", sigs)
	}
}

func TestAWebSocketUpgradeFailureRecordsWebsocketFailingWithThePeer(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.obs.RecordUpgradeFailure("203.0.113.9")
	rig.obs.RecordUpgradeFailure("203.0.113.9")
	sigs := rig.obs.Signals()
	if len(sigs) != 1 || sigs[0].Kind != SignalWebSocketFailing || sigs[0].Peer != "203.0.113.9" || sigs[0].Count != 2 {
		t.Fatalf("signals = %+v, want one websocket_failing from 203.0.113.9 seen twice", sigs)
	}
}

func TestASignalLastsAnHourFromItsLastSightingAndNoLonger(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.obs.RecordUpgradeFailure("203.0.113.9")
	first := rig.clock()
	rig.advance(59 * time.Minute)
	if got := rig.obs.Signals(); len(got) != 1 {
		t.Fatalf("after 59 minutes: %+v, want the signal", got)
	}
	rig.obs.RecordUpgradeFailure("203.0.113.9")
	rig.advance(59 * time.Minute)
	got := rig.obs.Signals()
	if len(got) != 1 || got[0].Count != 2 || !got[0].FirstSeen.Equal(first) || !got[0].LastSeen.Equal(first.Add(59*time.Minute)) {
		t.Fatalf("a sighting renews the signal and keeps when it began: %+v", got)
	}
	rig.advance(2 * time.Minute)
	if got := rig.obs.Signals(); len(got) != 0 {
		t.Fatalf("an hour after the last sighting: %+v, want none", got)
	}
}

func TestTheObserverHoldsAtMost256SignalsAndKeepsTheNewest(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	for i := range 300 {
		rig.send(probe{
			peer: fmt.Sprintf("198.51.%d.%d:5000", i/250, i%250+1), host: "tierplan.example.com", tls: true,
			headers: map[string]string{"X-Forwarded-For": "203.0.113.9"},
		})
		rig.advance(time.Second)
	}
	sigs := rig.obs.Signals()
	if len(sigs) != 256 {
		t.Fatalf("%d signals, want 256", len(sigs))
	}
	newest := fmt.Sprintf("198.51.%d.%d", 299/250, 299%250+1)
	oldest := "198.51.0.1"
	var haveNewest, haveOldest bool
	for _, s := range sigs {
		haveNewest = haveNewest || s.Peer == newest
		haveOldest = haveOldest || s.Peer == oldest
	}
	if !haveNewest || haveOldest {
		t.Errorf("newest present = %v, oldest present = %v; want the newest kept and the oldest dropped", haveNewest, haveOldest)
	}
}

func TestTheNewestSignalOfAKindComesFirst(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.obs.RecordUpgradeFailure("203.0.113.1")
	rig.advance(time.Second)
	rig.obs.RecordUpgradeFailure("203.0.113.2")
	sigs := rig.obs.Signals()
	if len(sigs) != 2 || sigs[0].Peer != "203.0.113.2" {
		t.Fatalf("signals = %+v, want the newest first", sigs)
	}
}

func TestAKindIsAnnouncedOnceWhenItFirstAppearsAndAgainAfterItWentAway(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.obs.RecordUpgradeFailure("203.0.113.1")
	rig.obs.RecordUpgradeFailure("203.0.113.2")
	rig.obs.RecordUpgradeFailure("203.0.113.1")
	if got := rig.appeared(); !slices.Equal(got, []string{SignalWebSocketFailing}) {
		t.Fatalf("announced %v, want websocket_failing once", got)
	}
	rig.advance(2 * time.Hour)
	rig.obs.RecordUpgradeFailure("203.0.113.1")
	if got := rig.appeared(); !slices.Equal(got, []string{SignalWebSocketFailing, SignalWebSocketFailing}) {
		t.Fatalf("announced %v, want it again after the first went away", got)
	}
}

func TestOneRequestCanRecordSeveralKinds(t *testing.T) {
	rig := newObserverRig(observedTrusted, observedPublicURL)
	rig.send(probe{peer: "198.51.100.7:5000", host: "other.example.com", headers: map[string]string{"X-Forwarded-For": "203.0.113.9"}})
	got := kinds(rig.obs.Signals())
	slices.Sort(got)
	if !slices.Equal(got, []string{SignalUntrustedForwarded, SignalURLMismatch}) {
		t.Fatalf("kinds = %v, want url_mismatch and untrusted_forwarded", got)
	}
}
