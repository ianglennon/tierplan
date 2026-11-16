// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// GET /api/environments/{envId}/fold-offer (D-04, D-15, D-16, D-17): the one read that tells the fold
// card what a fold would do, including for an install with no Local environment, whose socket the
// list never probes.

type foldOfferResp struct {
	Offered            bool   `json:"offered"`
	Source             string `json:"source"`
	Role               string `json:"role"`
	LocalEnvironmentID string `json:"localEnvironmentId"`
	Refusal            string `json:"refusal"`
	Ingress            *struct {
		Domains   int  `json:"domains"`
		Reserved  int  `json:"reserved"`
		Hostnames int  `json:"hostnames"`
		Routes    int  `json:"routes"`
		Enabled   bool `json:"enabled"`
	} `json:"ingress"`
	Removes *struct {
		Stacks     int  `json:"stacks"`
		GitRepos   int  `json:"gitRepos"`
		Registries int  `json:"registries"`
		Janitor    bool `json:"janitor"`
	} `json:"removes"`
}

func (f *foldFix) offerPath() string { return "/api/environments/" + f.env.ID + "/fold-offer" }

// offer reads the fold offer as the instance admin and returns the decoded body next to the raw one.
func (f *foldFix) offer(t *testing.T) (foldOfferResp, string) {
	t.Helper()
	rec := f.req(t, http.MethodGet, f.offerPath(), "")
	if rec.code != http.StatusOK {
		t.Fatalf("fold-offer: %d %s", rec.code, rec.body)
	}
	var out foldOfferResp
	if err := json.Unmarshal(rec.body, &out); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return out, string(rec.body)
}

func TestTracerTheFoldOfferOfASocketOnlyWorkerSaysSocketAndWorker(t *testing.T) {
	t.Parallel()
	f := newSocketOnlyWorkerFoldFix(t, nil)

	got, raw := f.offer(t)
	if !got.Offered || got.Source != "socket" || got.Role != "worker" || got.LocalEnvironmentID != "" || got.Removes != nil || got.Refusal != "" {
		t.Fatalf("fold-offer = %s, want offered, source socket, role worker, no localEnvironmentId, no removes", raw)
	}
	// D-15: the list does not probe the socket, so it cannot say the swarm is foldable.
	if f.list(t)[f.env.ID].FoldOffered {
		t.Error("the environments list says foldOffered for a socket-only install; only fold-offer may probe the socket")
	}

	if rec := f.req(t, http.MethodPost, f.foldPath(), ""); rec.code != http.StatusOK {
		t.Fatalf("fold: %d %s", rec.code, rec.body)
	}
	if after, raw := f.offer(t); after.Offered {
		t.Errorf("fold-offer after the fold = %s, want offered false", raw)
	}
}
