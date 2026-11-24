// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// TestKeepalivedSecretIsIngressOwned pins D-14 and A-13 (T-26-06): the virtual IP's VRRP key secret lists and
// reads with the ingress owner flag, and its delete and rotate are refused ingress_owned before the engine is
// touched, exactly as a proxy join token's secret is. The keepalived label on a name outside the prefix, the
// prefix without the label, and the ingress label on a keepalived key name are the person's own secrets.
func TestKeepalivedSecretIsIngressOwned(t *testing.T) {
	t.Parallel()

	key := dockertest.FakeSecret{ID: "sec-key", Name: ingress.SecretPrefixKeepalived + "g1-abcd1234", Labels: ingress.KeepalivedLabels()}
	otherName := dockertest.FakeSecret{ID: "sec-other", Name: "db-password", Labels: ingress.KeepalivedLabels()}
	bare := dockertest.FakeSecret{ID: "sec-bare", Name: ingress.SecretPrefixKeepalived + "g1-bare"}
	cross := dockertest.FakeSecret{ID: "sec-cross", Name: ingress.SecretPrefixKeepalived + "g1-cross", Labels: ingress.ObjectLabels()}
	f, e := secretsEnv(t, []dockertest.FakeSecret{key, otherName, bare, cross})

	list := decode[listBody[ingressFlag]](t, f.get(t, e, "secrets"))
	flags := map[string]bool{}
	for _, it := range list.Items {
		flags[it.ID] = it.Ingress
	}
	want := map[string]bool{"sec-key": true, "sec-other": false, "sec-bare": false, "sec-cross": false}
	for id, w := range want {
		if got, ok := flags[id]; !ok || got != w {
			t.Errorf("list: secret %s ingress = %v (listed %v), want %v", id, got, ok, w)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "secrets/"+id)); d.Ingress != w {
			t.Errorf("detail of %s: ingress = %v, want %v", id, d.Ingress, w)
		}
	}
	if rec := f.get(t, e, "secrets/sec-key/inspect"); rec.code != http.StatusOK {
		t.Errorf("inspect = %d %s, want it open", rec.code, rec.body)
	}

	wantIngressOwnedRec(t, "delete", f.deleteSecret(t, e, "sec-key", "confirm="+key.Name))
	wantIngressOwnedRec(t, "delete without the name", f.deleteSecret(t, e, "sec-key", ""))
	wantIngressOwnedRec(t, "rotate preview", f.rotateCall(t, e, "secrets", "sec-key", "rotate/preview", `{}`))
	wantIngressOwnedRec(t, "rotate", f.rotateCall(t, e, "secrets", "sec-key", "rotate",
		`{"data":"`+base64.StdEncoding.EncodeToString([]byte("new-value"))+`"}`))
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none: every refusal comes before the engine is touched", w)
	}
	rows := f.secretAudits(t)
	if len(rows) == 0 || rows[0].Reason != "ingress_owned" {
		t.Errorf("audit rows = %+v, want the delete refusal recorded with ingress_owned", rows)
	}

	// The look-alikes are the person's own: a delete with the typed name goes through.
	for _, s := range []dockertest.FakeSecret{otherName, bare, cross} {
		if rec := f.deleteSecret(t, e, s.ID, "confirm="+s.Name); rec.code != http.StatusNoContent {
			t.Errorf("delete of %s = %d %s, want 204: it is not the keepalived key", s.Name, rec.code, rec.body)
		}
	}
}
