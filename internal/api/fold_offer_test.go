// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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

// mustExec seeds a row straight into the store; the seeded query takes the Local environment's ID where it has a `?`.
func (f *foldFix) mustExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
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

func TestTheFoldOfferCarriesRemovalsAndRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a Local environment's manager fold lists what it removes", func(t *testing.T) {
		t.Parallel()
		f := newFoldFix(t, nil)
		f.mustExec(t, `INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('s1', ?, 'one', 'swarm', 'u1', 1)`, f.local.ID)
		f.mustExec(t, `INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('s2', ?, 'two', 'swarm', 'u1', 1)`, f.local.ID)
		f.mustExec(t, `INSERT INTO janitor_env_policy (env_id) VALUES (?)`, f.local.ID)

		got, raw := f.offer(t)
		if !got.Offered || got.Source != "local_environment" || got.Role != "manager" || got.LocalEnvironmentID != f.local.ID || got.Refusal != "" {
			t.Fatalf("fold-offer = %s, want offered from the Local environment as a manager", raw)
		}
		if got.Removes == nil || got.Removes.Stacks != 2 || got.Removes.GitRepos != 0 || got.Removes.Registries != 0 || !got.Removes.Janitor {
			t.Errorf("removes = %+v, want 2 stacks, 0 Git repositories, 0 registries and janitor settings", got.Removes)
		}
		for _, key := range []string{`"removes"`, `"gitRepos"`, `"registries"`, `"janitor"`} {
			if !strings.Contains(raw, key) {
				t.Errorf("fold-offer lacks the key %s: %s", key, raw)
			}
		}
	})

	t.Run("ingress state refuses the fold and is counted", func(t *testing.T) {
		t.Parallel()
		f := newFoldFix(t, nil)
		f.mustExec(t, `INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES ('dom-1', ?, 'one.example.org', 'u1', 1, 1)`, f.local.ID)
		f.mustExec(t, `INSERT INTO ingress_env (env_id, enabled, config_version, updated_at, updated_by) VALUES (?, 1, 4, 1, 'u1')`, f.local.ID)

		got, raw := f.offer(t)
		if got.Offered || got.Refusal != "fold_ingress_active" || got.Source != "local_environment" {
			t.Fatalf("fold-offer = %s, want refused with fold_ingress_active", raw)
		}
		if got.Ingress == nil || got.Ingress.Domains != 1 || got.Ingress.Reserved != 0 || !got.Ingress.Enabled {
			t.Errorf("ingress = %+v, want 1 domain and ingress enabled", got.Ingress)
		}
		if !strings.Contains(raw, `"ingress"`) || !strings.Contains(raw, `"refusal"`) {
			t.Errorf("fold-offer lacks the ingress or refusal key: %s", raw)
		}
	})

	t.Run("a worker the swarm's managers do not list is refused", func(t *testing.T) {
		t.Parallel()
		f := newSocketOnlyWorkerFoldFix(t, func(c *dockertest.FakeConfig) { c.SwarmNodeID = "node-removed" })
		got, raw := f.offer(t)
		if got.Offered || got.Refusal != "fold_node_not_listed" || got.Ingress != nil || got.Removes != nil {
			t.Fatalf("fold-offer = %s, want refused with fold_node_not_listed and nothing else", raw)
		}
	})
}

func TestTheFoldOfferIsForInstanceAdminsOnly(t *testing.T) {
	t.Parallel()
	f := newFoldFix(t, nil)
	f.addEnvUser(t, "adminone", f.env.ID, "admin")

	if rec := f.asUser(t, "adminone", http.MethodGet, f.offerPath(), ""); rec.code != http.StatusForbidden {
		t.Fatalf("fold-offer as an env Admin: %d %s, want 403", rec.code, rec.body)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 {
		t.Errorf("permission.denied entries = %d, want 1", len(denied))
	}

	if rec := f.req(t, http.MethodGet, "/api/environments/no-such-environment/fold-offer", ""); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("fold-offer of an unknown environment: %d type %q %s, want 404 environment_not_found", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.req(t, http.MethodGet, f.offerPath(), ""); rec.code != http.StatusOK {
		t.Errorf("fold-offer as an instance admin: %d %s, want 200", rec.code, rec.body)
	}
}
