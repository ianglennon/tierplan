// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
