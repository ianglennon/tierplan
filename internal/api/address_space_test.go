// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// The address-space route (plan 10-01, NET-02, D-02, D-03, D-07 as amended, T-10-02). Docker is a
// fake daemon behind the real monitor, snapshot cache and aggregator; the router, policy and
// the address reader are the real ones.

type addressPool struct {
	CIDR               string `json:"cidr"`
	Mask               int    `json:"mask"`
	Used               int64  `json:"used"`
	Capacity           int64  `json:"capacity"`
	Percent            int    `json:"percent"`
	State              string `json:"state"`
	LargestFree        string `json:"largestFree"`
	LargestFreeSubnets int64  `json:"largestFreeSubnets"`
	Source             string `json:"source"`
	Estimated          bool   `json:"estimated"`
}

type addressSpace struct {
	EnvType      string        `json:"envType"`
	Measured     bool          `json:"measured"`
	NotMeasured  string        `json:"notMeasured"`
	Pools        []addressPool `json:"pools"`
	NotReporting []string      `json:"notReporting"`
}

func TestAddressSpaceRoute(t *testing.T) {
	t.Parallel()
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.SwarmDefaultAddrPool = []string{"10.60.0.0/16"}
		cfg.Networks = []dockertest.FakeNetwork{
			{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.60.0.0/24"}},
			{ID: "ovl", Name: "ovl", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.60.1.0/24"}},
		}
	})

	raw := s.call(t, authReq{method: http.MethodGet, path: "/api/env/" + s.env.ID + "/address-space", cookie: s.cookie, csrf: s.csrf})
	if raw.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", raw.Code, raw.Body)
	}
	if cc := raw.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := decode[addressSpace](t, &envRec{code: raw.Code, body: raw.Body.Bytes()})
	if got.EnvType != "swarm" || !got.Measured || got.NotMeasured != "" || len(got.Pools) != 1 || got.NotReporting == nil || len(got.NotReporting) != 0 {
		t.Fatalf("answer = %+v, want a measured swarm with one pool and an empty (not null) notReporting", got)
	}
	want := addressPool{
		CIDR: "10.60.0.0/16", Mask: 24, Used: 2, Capacity: 256, Percent: 0, State: "normal",
		LargestFree: "10.60.128.0/17", LargestFreeSubnets: 128, Source: "swarm", Estimated: false,
	}
	if got.Pools[0] != want {
		t.Fatalf("pool = %+v\nwant   %+v", got.Pools[0], want)
	}
}

func TestAddressSpaceRouteAViewerMayReadAndAUserWithoutTheEnvironmentGets404(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "outsider", false)
	awaitOnline(t, f.mon, e.ID)

	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/address-space", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("a Viewer: status = %d, want 200: %s", rec.Code, rec.Body)
	}
	cookie, csrf = f.signIn(t, "outsider", false)
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/address-space", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a user without the environment: status = %d, want 404: %s", rec.Code, rec.Body)
	}
	unknown := f.req(t, http.MethodGet, "/api/env/nope/address-space", "")
	if unknown.code != http.StatusNotFound || unknown.ptyp != "environment_not_found" {
		t.Fatalf("an unknown environment: %d %q, want 404 environment_not_found", unknown.code, unknown.ptyp)
	}
}
