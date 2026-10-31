// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// The subnet check (plan 10-13, NET-05, D-14, D-15, T-10-34, T-10-35). Docker is a fake daemon
// behind the real monitor, snapshot cache and aggregator; the router, policy and address reader
// are the real ones.

type subnetCheckAnswer struct {
	Suggestion *struct {
		Subnet string `json:"subnet"`
		Pool   string `json:"pool"`
	} `json:"suggestion"`
	NoFreeBlock *struct {
		Mask int    `json:"mask"`
		Pool string `json:"pool"`
	} `json:"noFreeBlock"`
	Needed            *int `json:"needed"`
	RecommendedMask   *int `json:"recommendedMask"`
	RecommendedUsable *int `json:"recommendedUsable"`
	TooManyTasks      bool `json:"tooManyTasks"`
	SubnetError       *struct {
		Code      string `json:"code"`
		Corrected string `json:"corrected"`
	} `json:"subnetError"`
	IPv6NotChecked bool `json:"ipv6NotChecked"`
}

func (f *agentFix) subnetCheck(t *testing.T, envID, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+envID+"/address-space/subnet-check", body)
}

// subnetSwarm is a swarm with the built-in pool (10.0.0.0/8 at /24) and the given overlays on
// its managers.
func subnetSwarm(t *testing.T, overlays ...dockertest.FakeNetwork) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.Networks = append([]dockertest.FakeNetwork{}, overlays...)
	})
}

func overlayNet(id string, subnet string) dockertest.FakeNetwork {
	return dockertest.FakeNetwork{ID: id, Name: id, Driver: "overlay", Scope: "swarm", Subnets: []string{subnet}}
}

func TestSubnetCheckSuggest(t *testing.T) {
	t.Parallel()
	s := subnetSwarm(t,
		dockertest.FakeNetwork{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
		overlayNet("one", "10.0.1.0/24"),
		overlayNet("two", "10.0.2.0/26"),
	)
	for _, tc := range []struct{ body, subnet string }{
		{`{"suggest":true,"mask":26}`, "10.0.2.64/26"},
		{`{"suggest":true,"mask":24}`, "10.0.3.0/24"},
		{`{"suggest":true,"mask":24,"driver":"overlay"}`, "10.0.3.0/24"},
	} {
		rec := s.subnetCheck(t, s.env.ID, tc.body)
		if rec.code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", tc.body, rec.code, rec.body)
		}
		got := decode[subnetCheckAnswer](t, rec)
		if got.Suggestion == nil || got.Suggestion.Subnet != tc.subnet || got.Suggestion.Pool != "10.0.0.0/8" || got.NoFreeBlock != nil {
			t.Errorf("%s: answer = %s, want %s from 10.0.0.0/8", tc.body, rec.body, tc.subnet)
		}
	}
}
