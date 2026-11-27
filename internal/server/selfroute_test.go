// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The self-route holder (D-06, D-07, A-08): the live trusted-proxy set and the route's origin come from the stored claim
// and TierPlan's own environment's observation, and from nothing a request carries.

const (
	holderEnv    = "env-1"
	holderHost   = "tp.apps.example.org"
	holderOrigin = "https://tp.apps.example.org"
)

// claimSource is a settable system claim read, as the ingress store gives it.
type claimSource struct {
	mu  sync.Mutex
	c   ingressstore.SystemClaim
	ok  bool
	err error
}

func (s *claimSource) set(c ingressstore.SystemClaim, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c, s.ok, s.err = c, ok, nil
}

func (s *claimSource) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *claimSource) read(context.Context) (ingressstore.SystemClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c, s.ok, s.err
}

// holderClock is a clock the test moves.
type holderClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *holderClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *holderClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newHolderRig builds a holder over a settable claim, a moved clock and a discarded log, holding the claim
// {env-1, tp.apps.example.org} when claimed is true. It does not Reload.
func newHolderRig(t *testing.T, claimed bool, log *slog.Logger) (*selfRoute, *claimSource, *holderClock) {
	t.Helper()
	src := &claimSource{}
	if claimed {
		src.set(ingressstore.SystemClaim{EnvID: holderEnv, Hostname: holderHost, Service: "tierplan", Port: 7480}, true)
	}
	clk := &holderClock{t: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return newSelfRoute(src.read, clk.now, log), src, clk
}

// attachedObs is TierPlan's own network in env, observed with TierPlan on it: 10.9.0.0/24 with gateway 10.9.0.1.
func attachedObs(env string) ingressdeploy.SelfObservation {
	return ingressdeploy.SelfObservation{
		EnvID: env, Exists: true, Wanted: true, Attached: true, Observed: true, Kind: "swarm_service", BackendHost: "tierplan_tierplan",
		Subnet: netip.MustParsePrefix("10.9.0.0/24"), Gateway: netip.MustParseAddr("10.9.0.1"),
	}
}

type holderEchoOut struct {
	Body struct {
		IP string `json:"ip"`
	}
}

// holderRouter is a router built with the holder's funcs, as the server builds it, with one public mutation that answers
// the client address the router believed.
func holderRouter(h *selfRoute) http.Handler {
	r := httpx.New(httpx.Options{
		PublicOrigin:       func() string { return "https://tierplan.example.org:7480" },
		LiveTrustedProxies: h.TrustedPrefixes,
		ExtraOrigins:       h.ExtraOrigins,
		Logger:             slog.New(slog.DiscardHandler),
	})
	httpx.Register(r, huma.Operation{OperationID: "postHolder", Method: http.MethodPost, Path: "/api/holder"}, rbac.Public,
		func(ctx context.Context, _ *struct{}) (*holderEchoOut, error) {
			out := &holderEchoOut{}
			out.Body.IP = httpx.ClientIP(ctx)
			return out, nil
		})
	return r.Handler()
}

// throughRoute posts as the proxy on the self network would: from 10.9.0.5, for the client 203.0.113.7, from the route's
// origin. It answers the status and the client address the router believed.
func throughRoute(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/holder", nil)
	req.RemoteAddr = "10.9.0.5:41000"
	req.Host = holderHost
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("Origin", holderOrigin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		IP string `json:"ip"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out.IP
}

func TestHolderFeedsTheRouter(t *testing.T) {
	h, _, _ := newHolderRig(t, true, nil)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	router := holderRouter(h)

	// The claim alone opens the origin, but the proxy is not believed yet: no observation has named its network.
	code, ip := throughRoute(t, router)
	if code != http.StatusOK {
		t.Fatalf("before any observation: status = %d, want 200 (the route's origin comes from the claim)", code)
	}
	if ip != "10.9.0.5" {
		t.Fatalf("before any observation: client = %q, want the peer 10.9.0.5", ip)
	}

	// One observation of TierPlan's own environment, and the same request is believed, with no restart.
	h.Observe(attachedObs(holderEnv))
	code, ip = throughRoute(t, router)
	if code != http.StatusOK || ip != "203.0.113.7" {
		t.Fatalf("after the observation: status %d client %q, want 200 and the forwarded 203.0.113.7", code, ip)
	}
}

// wantSelfPrefixes is 10.9.0.0/24 without its gateway 10.9.0.1: eight prefixes in address order.
var wantSelfPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.9.0.0/32"), netip.MustParsePrefix("10.9.0.2/31"), netip.MustParsePrefix("10.9.0.4/30"),
	netip.MustParsePrefix("10.9.0.8/29"), netip.MustParsePrefix("10.9.0.16/28"), netip.MustParsePrefix("10.9.0.32/27"),
	netip.MustParsePrefix("10.9.0.64/26"), netip.MustParsePrefix("10.9.0.128/25"),
}

func TestHolderTrustedPrefixesFollowTheObservation(t *testing.T) {
	reloaded := func(t *testing.T) *selfRoute {
		t.Helper()
		h, _, _ := newHolderRig(t, true, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		return h
	}
	t.Run("attached: the subnet without its gateway", func(t *testing.T) {
		h := reloaded(t)
		h.Observe(attachedObs(holderEnv))
		got := h.TrustedPrefixes()
		if !slices.Equal(got, wantSelfPrefixes) {
			t.Fatalf("prefixes = %v, want %v", got, wantSelfPrefixes)
		}
		for _, p := range got {
			if p.Contains(netip.MustParseAddr("10.9.0.1")) {
				t.Fatalf("prefix %v holds the gateway", p)
			}
		}
	})
	t.Run("not attached: none", func(t *testing.T) {
		h := reloaded(t)
		h.Observe(attachedObs(holderEnv))
		o := attachedObs(holderEnv)
		o.Attached = false
		h.Observe(o)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none once TierPlan is off the network", got)
		}
	})
	t.Run("foreign: none", func(t *testing.T) {
		h := reloaded(t)
		o := attachedObs(holderEnv)
		o.Foreign, o.Exists, o.Attached = true, false, false
		h.Observe(o)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none for a foreign network", got)
		}
		// Even an observation that claimed Attached on a foreign network trusts nothing.
		o.Attached, o.Exists = true, true
		h.Observe(o)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none for a foreign network said to be attached", got)
		}
	})
	t.Run("another environment changes nothing (T-28-50)", func(t *testing.T) {
		h := reloaded(t)
		other := attachedObs("env-2")
		other.Subnet, other.Gateway = netip.MustParsePrefix("10.66.0.0/24"), netip.MustParseAddr("10.66.0.1")
		h.Observe(other)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none: env-2 is not the claim's environment", got)
		}
		h.Observe(attachedObs(holderEnv))
		h.Observe(other)
		if got := h.TrustedPrefixes(); !slices.Equal(got, wantSelfPrefixes) {
			t.Fatalf("prefixes = %v, want env-1's %v unchanged by env-2", got, wantSelfPrefixes)
		}
		if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin}) {
			t.Fatalf("origins = %q, want only the claim's", got)
		}
	})
	t.Run("a foreign member on the network: none until it leaves (CR-02)", func(t *testing.T) {
		h := reloaded(t)
		o := attachedObs(holderEnv)
		o.ForeignMembers = []string{"shop_web"}
		h.Observe(o)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none while a stack's service is on the network", got)
		}
		h.Observe(attachedObs(holderEnv))
		if got := h.TrustedPrefixes(); !slices.Equal(got, wantSelfPrefixes) {
			t.Fatalf("prefixes = %v, want %v once it left", got, wantSelfPrefixes)
		}
	})
	t.Run("an ingress subnet overlapping the subnet: none", func(t *testing.T) {
		h := reloaded(t)
		o := attachedObs(holderEnv)
		o.IngressSubnets = []netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")}
		h.Observe(o)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none when the ingress overlay overlaps", got)
		}
	})
	t.Run("a bridge gateway inside the subnet is carved out", func(t *testing.T) {
		h := reloaded(t)
		o := attachedObs(holderEnv)
		o.BridgeGateways = []netip.Addr{netip.MustParseAddr("10.9.0.200")}
		h.Observe(o)
		for _, p := range h.TrustedPrefixes() {
			if p.Contains(netip.MustParseAddr("10.9.0.200")) {
				t.Fatalf("prefix %v holds the bridge gateway", p)
			}
		}
	})
	t.Run("a claim moved to another environment drops the old network", func(t *testing.T) {
		h, src, _ := newHolderRig(t, true, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.Observe(attachedObs(holderEnv))
		src.set(ingressstore.SystemClaim{EnvID: "env-2", Hostname: holderHost, Service: "tierplan", Port: 7480}, true)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none: env-1 is no longer the claim's environment", got)
		}
	})
	t.Run("no claim: the environment TierPlan runs in, for the carve-out", func(t *testing.T) {
		h, _, _ := newHolderRig(t, false, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		notHere := attachedObs("env-2")
		notHere.Kind = ""
		h.Observe(notHere)
		if got := h.TrustedPrefixes(); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none from an environment TierPlan does not run in", got)
		}
		h.Observe(attachedObs(holderEnv))
		if got := h.TrustedPrefixes(); !slices.Equal(got, wantSelfPrefixes) {
			t.Fatalf("prefixes = %v, want %v from TierPlan's own environment", got, wantSelfPrefixes)
		}
		h.Observe(notHere)
		if got := h.TrustedPrefixes(); !slices.Equal(got, wantSelfPrefixes) {
			t.Fatalf("prefixes = %v, want TierPlan's own unchanged by env-2", got)
		}
		if got := h.ExtraOrigins(); len(got) != 0 {
			t.Fatalf("origins = %q, want none without a claim", got)
		}
	})
}

func TestHolderOriginsFollowTheClaim(t *testing.T) {
	h, src, _ := newHolderRig(t, false, nil)
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("before Reload: origins = %q, want none", got)
	}
	src.set(ingressstore.SystemClaim{EnvID: holderEnv, Hostname: "TP.Apps.Example.Org.", Service: "tierplan", Port: 7480}, true)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin}) {
		t.Fatalf("with the claim: origins = %q, want [%q]", got, holderOrigin)
	}
	// A read that fails keeps what the holder knew.
	src.fail(errors.New("database is locked"))
	if err := h.Reload(context.Background()); err == nil {
		t.Fatal("Reload with a failing read: want the error")
	}
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin}) {
		t.Fatalf("after a failed read: origins = %q, want [%q] kept", got, holderOrigin)
	}
	src.set(ingressstore.SystemClaim{}, false)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("after the claim is cleared: origins = %q, want none", got)
	}
	// An observation never adds an origin: not of TierPlan's environment, not of another, whatever it carries.
	h.Observe(attachedObs(holderEnv))
	h.Observe(attachedObs("env-2"))
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("after observations with no claim: origins = %q, want none", got)
	}
	// The slice handed out is the caller's: changing it changes nothing.
	src.set(ingressstore.SystemClaim{EnvID: holderEnv, Hostname: holderHost, Service: "tierplan", Port: 7480}, true)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.ExtraOrigins()[0] = "https://evil.example"
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin}) {
		t.Fatalf("after the caller changed its copy: origins = %q, want [%q]", got, holderOrigin)
	}
}

// holderURLs is a settable pair of the instance's own URLs, as instanceSelfURLs reads them.
type holderURLs struct {
	mu            sync.Mutex
	public, agent string
}

func (u *holderURLs) set(public, agent string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.public, u.agent = public, agent
}

func (u *holderURLs) read() ingressdeploy.SelfURLs {
	u.mu.Lock()
	defer u.mu.Unlock()
	return ingressdeploy.SelfURLs{PublicURL: u.public, AgentURL: u.agent, ListenPort: 7480}
}

// TestHolderOriginsKeepTheDirectAddress (D-07, D-10, CR-01): the agent URL's origin, TierPlan's own direct address, is in
// the set whenever an agent URL is set, with the route on or off and wherever the public URL points. The route host's origin
// is there only while the claim exists. The set is read from the stored URLs on every call, normalised.
func TestHolderOriginsKeepTheDirectAddress(t *testing.T) {
	const agent = "https://tierplan.internal:7480"
	h, src, _ := newHolderRig(t, true, nil)
	urls := &holderURLs{}
	h.wire(selfInputs{urls: urls.read})
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	urls.set("https://tierplan.example.org:7480", agent)
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin, agent}) {
		t.Fatalf("public URL off the route host: origins = %q, want [%q %q]", got, holderOrigin, agent)
	}
	urls.set("https://"+holderHost, agent)
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin, agent}) {
		t.Fatalf("public URL on the route host: origins = %q, want [%q %q]", got, holderOrigin, agent)
	}
	urls.set("https://TP.Apps.Example.Org.:443/", "HTTPS://TierPlan.Internal:7480/")
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin, agent}) {
		t.Fatalf("both URLs in other forms: origins = %q, want [%q %q]", got, holderOrigin, agent)
	}
	urls.set("https://"+holderHost, "")
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{holderOrigin}) {
		t.Fatalf("public URL on the route host, no agent URL: origins = %q, want [%q]", got, holderOrigin)
	}
	urls.set("https://"+holderHost, agent)
	src.set(ingressstore.SystemClaim{}, false)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.ExtraOrigins(); !slices.Equal(got, []string{agent}) {
		t.Fatalf("route off: origins = %q, want [%q]: the direct address outlives the claim", got, agent)
	}
	urls.set("https://"+holderHost, "")
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("route off, no agent URL: origins = %q, want none", got)
	}
}

// holderPost posts to a router built over the holder and the stored public URL, from origin, and answers the status.
func holderPost(h *selfRoute, urls *holderURLs, origin string) int {
	r := httpx.New(httpx.Options{
		PublicOrigin: func() string { return httpx.OriginOf(urls.read().PublicURL) },
		ExtraOrigins: h.ExtraOrigins,
		Logger:       slog.New(slog.DiscardHandler),
	})
	httpx.Register(r, huma.Operation{OperationID: "postHolder", Method: http.MethodPost, Path: "/api/holder"}, rbac.Public,
		func(context.Context, *struct{}) (*holderEchoOut, error) { return &holderEchoOut{}, nil })
	req := httptest.NewRequest(http.MethodPost, "/api/holder", nil)
	req.RemoteAddr = "192.0.2.10:41000"
	req.Host = "tierplan.internal:7480"
	req.Header.Set("Origin", origin)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// TestHolderDirectAddressCanStillChangeThings: a change from the agent URL's origin is accepted while the route is on,
// whichever host the public URL names.
func TestHolderDirectAddressCanStillChangeThings(t *testing.T) {
	const agent = "https://tierplan.internal:7480"
	h, _, _ := newHolderRig(t, true, nil)
	urls := &holderURLs{}
	h.wire(selfInputs{urls: urls.read})
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, public := range []string{"https://" + holderHost, "https://tierplan.example.org:7480"} {
		urls.set(public, agent)
		if code := holderPost(h, urls, agent); code != http.StatusOK {
			t.Errorf("public URL %s: a change from %s answered %d, want 200", public, agent, code)
		}
	}
	urls.set("https://tierplan.example.org:7480", agent)
	if code := holderPost(h, urls, "https://elsewhere.example.org"); code != http.StatusForbidden {
		t.Errorf("a change from an origin in no setting answered %d, want 403", code)
	}
}

// TestHolderDirectAddressSurvivesTheRelease (CR-01): with the public URL on the route host, no way the claim goes, a switch-
// off, a deleted environment or a claim that could never be read, refuses a change from the direct address. Sign-in is one
// such change, so a refusal there would lock every browser out.
func TestHolderDirectAddressSurvivesTheRelease(t *testing.T) {
	const agent = "https://tierplan.internal:7480"
	t.Run("released", func(t *testing.T) {
		h, src, _ := newHolderRig(t, true, nil)
		urls := &holderURLs{}
		urls.set("https://"+holderHost, agent)
		h.wire(selfInputs{urls: urls.read})
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		src.set(ingressstore.SystemClaim{}, false)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if code := holderPost(h, urls, agent); code != http.StatusOK {
			t.Fatalf("after the release: a change from %s answered %d, want 200", agent, code)
		}
		if code := holderPost(h, urls, holderOrigin); code != http.StatusOK {
			// The public URL itself is still the route host: the public origin is always accepted.
			t.Fatalf("after the release: a change from the public origin answered %d, want 200", code)
		}
	})
	t.Run("claim never read", func(t *testing.T) {
		h, src, _ := newHolderRig(t, true, nil)
		src.fail(errors.New("database is locked"))
		urls := &holderURLs{}
		urls.set("https://"+holderHost, agent)
		h.wire(selfInputs{urls: urls.read})
		if err := h.Reload(context.Background()); err == nil {
			t.Fatal("Reload with a failing read: want the error")
		}
		if code := holderPost(h, urls, agent); code != http.StatusOK {
			t.Fatalf("claim unreadable: a change from %s answered %d, want 200", agent, code)
		}
	})
}

// logCapture keeps every record a slog handler is given.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// trustChanges are the info lines of trusted-set changes, as "before->after" counts.
func (c *logCapture) trustChanges() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.recs {
		if r.Level != slog.LevelInfo || r.Message != "the trusted proxies on TierPlan's own network changed" {
			continue
		}
		var before, after int64 = -1, -1
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "prefixes_before":
				before = a.Value.Int64()
			case "prefixes_after":
				after = a.Value.Int64()
			}
			return true
		})
		out = append(out, fmt.Sprintf("%d->%d", before, after))
	}
	return out
}

func TestHolderLogsTrustedSetChanges(t *testing.T) {
	logs := &logCapture{}
	h, _, _ := newHolderRig(t, true, slog.New(logs))
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Observe(attachedObs(holderEnv))
	h.Observe(attachedObs(holderEnv)) // the same set again: no line
	o := attachedObs(holderEnv)
	o.Attached = false
	h.Observe(o)
	if got, want := logs.trustChanges(), []string{"0->8", "8->0"}; !slices.Equal(got, want) {
		t.Fatalf("trusted-set log lines = %v, want %v", got, want)
	}
}

func TestHolderReadyGate(t *testing.T) {
	t.Run("no claim: ready once read", func(t *testing.T) {
		h, _, _ := newHolderRig(t, false, nil)
		if h.Ready() {
			t.Fatal("before Reload: ready, want not ready")
		}
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !h.Ready() {
			t.Fatal("after Reload with no claim: not ready, want ready")
		}
	})
	t.Run("a claim: ready on its environment's first observation", func(t *testing.T) {
		h, _, _ := newHolderRig(t, true, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if h.Ready() {
			t.Fatal("with a claim and no observation: ready, want not ready")
		}
		h.Observe(attachedObs("env-2"))
		if h.Ready() {
			t.Fatal("after another environment's observation: ready, want not ready")
		}
		o := attachedObs(holderEnv)
		o.Attached = false // observed, though TierPlan is not on the network yet: the set is known, and empty
		h.Observe(o)
		if !h.Ready() {
			t.Fatal("after the claim's environment's observation: not ready, want ready")
		}
	})
	t.Run("a claim and no observation: ready after the fallback", func(t *testing.T) {
		h, _, clk := newHolderRig(t, true, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		clk.advance(selfReadyTimeout - time.Second)
		if h.Ready() {
			t.Fatal("a second before the fallback: ready, want not ready")
		}
		clk.advance(time.Second)
		if !h.Ready() {
			t.Fatal("at the fallback: not ready, want ready so the bell is not held forever")
		}
	})
	t.Run("a claim never read: ready after the fallback", func(t *testing.T) {
		h, src, clk := newHolderRig(t, true, nil)
		src.fail(errors.New("database is locked"))
		if err := h.Reload(context.Background()); err == nil {
			t.Fatal("Reload with a failing read: want the error")
		}
		if h.Ready() {
			t.Fatal("after a failed read: ready, want not ready")
		}
		clk.advance(selfReadyTimeout)
		if !h.Ready() {
			t.Fatal("at the fallback after a failed read: not ready, want ready")
		}
	})
}

// TestHolderHoldsTheFirstRender (28-UAT 9a): after a restart the claim is read at once but TierPlan's own network is
// observed only by the first ensure pass that reaches the manager. Until then the holder holds the first render of the
// claim's environment, so the proxies keep the cached bundle with TierPlan's own route in it rather than being sent one
// without it; it lets go on the claim's environment's first observation, attached or not, or at the ready fallback, and it
// never holds another environment or one with no claim.
func TestHolderHoldsTheFirstRender(t *testing.T) {
	t.Run("held until the claim's environment is observed", func(t *testing.T) {
		h, _, _ := newHolderRig(t, true, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !h.HoldFirstRender(holderEnv) {
			t.Fatal("a claim and no observation: not held, want the first render held")
		}
		if h.HoldFirstRender("env-2") {
			t.Fatal("another environment is held, want only the claim's")
		}
		h.Observe(attachedObs("env-2"))
		if !h.HoldFirstRender(holderEnv) {
			t.Fatal("another environment's observation let the claim's go, want it still held")
		}
		o := attachedObs(holderEnv)
		o.Attached = false // a pass that says TierPlan is not on the network is an answer too: the route goes
		h.Observe(o)
		if h.HoldFirstRender(holderEnv) {
			t.Fatal("after the claim's environment's observation: held, want it let go")
		}
	})
	t.Run("let go at the fallback", func(t *testing.T) {
		h, _, clk := newHolderRig(t, true, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		clk.advance(selfReadyTimeout)
		if h.HoldFirstRender(holderEnv) {
			t.Fatal("at the fallback: held, want the render to go ahead")
		}
	})
	t.Run("no claim holds nothing", func(t *testing.T) {
		h, _, _ := newHolderRig(t, false, nil)
		if err := h.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if h.HoldFirstRender(holderEnv) {
			t.Fatal("with no claim: held, want nothing held")
		}
	})
	t.Run("a nil holder holds nothing", func(t *testing.T) {
		var h *selfRoute
		if h.HoldFirstRender(holderEnv) {
			t.Fatal("a nil holder held a render")
		}
	})
}

// TestHolderClaimChangedReloads (WR-01): the store's change hook reloads the holder at once, without waiting for an ensure
// pass to publish an observation, so the route's origin goes with the claim even while TierPlan's environment is unreachable.
func TestHolderClaimChangedReloads(t *testing.T) {
	h, src, _ := newHolderRig(t, true, nil)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.set(ingressstore.SystemClaim{}, false)
	h.claimChanged()
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("after the claim went: origins = %q, want none", got)
	}
	src.fail(errors.New("database is locked"))
	h.claimChanged() // a read that fails keeps what the holder knew, and is only logged
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("after a failed reload: origins = %q, want none", got)
	}
}

// gatedClaims answers the claim as it is when read, but holds the first read's answer until release is closed: the read of
// one reload overtaken by a later one.
type gatedClaims struct {
	src     *claimSource
	first   sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedClaims) read(ctx context.Context) (ingressstore.SystemClaim, bool, error) {
	c, ok, err := g.src.read(ctx)
	gate := false
	g.first.Do(func() { gate = true })
	if gate {
		close(g.entered)
		<-g.release
	}
	return c, ok, err
}

// TestHolderReloadNeverGoesBackwards (WR-02): a reload whose read began before the claim was cleared cannot publish its
// older answer over a later reload's: the read and the publish of one reload are one step.
func TestHolderReloadNeverGoesBackwards(t *testing.T) {
	src := &claimSource{}
	src.set(ingressstore.SystemClaim{EnvID: holderEnv, Hostname: holderHost, Service: "tierplan", Port: 7480}, true)
	g := &gatedClaims{src: src, entered: make(chan struct{}), release: make(chan struct{})}
	h := newSelfRoute(g.read, nil, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // A: reads "claimed", then is held
		defer wg.Done()
		_ = h.Reload(context.Background())
	}()
	<-g.entered
	src.set(ingressstore.SystemClaim{}, false) // the switch-off commits
	bDone := make(chan struct{})
	go func() { // B: reads "no claim"
		defer close(bDone)
		_ = h.Reload(context.Background())
	}()
	select {
	case <-bDone:
	case <-time.After(100 * time.Millisecond): // wall-clock: gives an unserialised reload time to finish first; with the fix the outcome holds whatever the timing
	}
	close(g.release)
	wg.Wait()
	<-bDone
	if got := h.ExtraOrigins(); len(got) != 0 {
		t.Fatalf("origins = %q, want none: the older reload published over the newer", got)
	}
	if s := h.snap.Load(); s.Claimed {
		t.Fatalf("snapshot = %+v, want no claim", s)
	}
}
