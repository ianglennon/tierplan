// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// Who sees and who manages which registry credential (plan 05-10, D-09, D-30, T-05-36). The
// router, policy, rbac checker, vault and audit writer are the real ones.

// layeredCredentials stores one credential per layer as the instance admin: the instance's, and
// one for each of two environments.
func (f *envFix) layeredCredentials(t *testing.T, a, b envBody) (instance, inA, inB credentialResp) {
	t.Helper()
	admin := credentialUser{f.cookie, f.csrf}
	body := func(name string) string {
		return `{"name":"` + name + `","type":"ghcr","prefix":"acme","username":"bot","secret":"` + credToken + `"}`
	}
	instance = f.storeCredential(t, admin, body("layer-instance"))
	for env, dst := range map[string]*credentialResp{a.ID: &inA, b.ID: &inB} {
		name := "layer-a"
		if env == b.ID {
			name = "layer-b"
		}
		rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+env+"/registries", body(name))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
		*dst = decodeCred(t, rec)
	}
	return instance, inA, inB
}

func TestCredentialVisibility(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	instance, inA, inB := f.layeredCredentials(t, a, b)
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)

	const (
		viewOnly = "view"
		manage   = "manage"
	)
	type rows = map[string]string
	for name, tc := range map[string]struct {
		u    credentialUser
		want rows
	}{
		"instance admin": {
			credentialUser{f.cookie, f.csrf},
			rows{"layer-instance": manage, "layer-a": manage, "layer-b": manage},
		},
		"admin of A": {
			f.asRoleOn(t, "admina", a.ID, "admin"),
			rows{"layer-instance": viewOnly, "layer-a": manage},
		},
		"operator of A": {
			f.asRoleOn(t, "operatora", a.ID, "operator"),
			rows{"layer-instance": viewOnly, "layer-a": viewOnly},
		},
		"viewer of B": {
			f.asRoleOn(t, "viewerb", b.ID, "viewer"),
			rows{"layer-instance": viewOnly, "layer-b": viewOnly},
		},
		"admin of everything": {
			f.asRoleOn(t, "adminall", "", "admin"),
			rows{"layer-instance": viewOnly, "layer-a": manage, "layer-b": manage},
		},
		"user with no grant": {credentialUser{nobodyCookie, nobodyCSRF}, rows{}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.credCall(t, tc.u, http.MethodGet, "/api/registries", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body)
			}
			got := rows{}
			for _, c := range decodeCreds(t, rec) {
				got[c.Name] = viewOnly
				if c.CanManage {
					got[c.Name] = manage
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("list = %v, want %v", got, tc.want)
			}
			for credName, want := range tc.want {
				if got[credName] != want {
					t.Errorf("list = %v, want %v", got, tc.want)
					break
				}
			}
			if strings.Contains(rec.Body.String(), credToken) || strings.Contains(rec.Body.String(), "SECRETtoken") {
				t.Errorf("the list carries a secret: %s", rec.Body)
			}

			// An item answers exactly as the list does, and one that is not listed is the same
			// 404 as one that does not exist.
			for _, c := range []credentialResp{instance, inA, inB} {
				one := f.credCall(t, tc.u, http.MethodGet, "/api/registries/"+c.ID, "")
				want, listed := tc.want[c.Name]
				switch {
				case !listed:
					if one.Code != http.StatusNotFound || problemType(t, one) != "credential_not_found" {
						t.Errorf("get %s: %d %s, want 404 credential_not_found", c.Name, one.Code, one.Body)
					}
				case one.Code != http.StatusOK || decodeCred(t, one).CanManage != (want == manage):
					t.Errorf("get %s: %d %s, want 200 with canManage %v", c.Name, one.Code, one.Body, want == manage)
				}
			}
		})
	}
}

func TestRemovingAnEnvironmentDeletesItsCredentials(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	instance, inA, inB := f.layeredCredentials(t, a, b)

	// B is the agent environment; removing it takes its credentials with it, in the same
	// transaction, and leaves the other layers alone.
	if rec := f.req(t, http.MethodDelete, "/api/environments/"+b.ID, ""); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.code, rec.body)
	}
	var left []string
	rows, err := f.st.Read.Query(`SELECT id FROM registry_credentials ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, id := range left {
		if id == inB.ID {
			t.Errorf("the removed environment's credential %s is still stored", id)
		}
	}
	if len(left) != 2 {
		t.Errorf("credentials left = %v, want the instance's %s and A's %s", left, instance.ID, inA.ID)
	}
	// Nothing refers to it any more: it is not listed and not found.
	list := decodeCreds(t, f.credCall(t, credentialUser{f.cookie, f.csrf}, http.MethodGet, "/api/registries", ""))
	for _, c := range list {
		if c.ID == inB.ID {
			t.Errorf("the list still holds %s", c.ID)
		}
	}
	if rec := f.credCall(t, credentialUser{f.cookie, f.csrf}, http.MethodGet, "/api/registries/"+inB.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get: %d %s, want 404", rec.Code, rec.Body)
	}
}
