// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestIngressDisablePreviewListsTheVIP (D-17): the switch-off's preview names the virtual IP's address and the name of its key
// secret in removes.virtualIP when the environment has one, and says nothing of it when it has none.
func TestIngressDisablePreviewListsTheVIP(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	ctx := testsupport.Context(t)
	path := "/api/env/" + f.envA + "/ingress/disable/preview"
	f.enableThroughTheRoute(t)

	rec := f.credCall(t, f.adminA, http.MethodPost, path, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "virtualIP") {
		t.Errorf("preview without a virtual IP = %s, want no virtualIP", rec.Body)
	}
	if pv := decodeBody[disablePreviewResp](t, rec); pv.Removes.VirtualIP != nil {
		t.Errorf("removes.virtualIP = %+v, want none", pv.Removes.VirtualIP)
	}

	sealed, err := f.ingressStore.SealVIPKey(f.envA, make([]byte, 32))
	if err != nil {
		t.Fatalf("SealVIPKey: %v", err)
	}
	cfg := ingressstore.VIPConfig{
		EnvID: f.envA, Address: netip.MustParseAddr("10.9.0.50"), PrefixLen: 24, RouterID: 51, RouterDerived: true,
		KeySealed: sealed, KeyGeneration: 2, Enabled: true,
	}
	if err := f.ingressStore.PutVIP(ctx, cfg); err != nil {
		t.Fatalf("PutVIP: %v", err)
	}
	rec = f.credCall(t, f.adminA, http.MethodPost, path, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	pv := decodeBody[disablePreviewResp](t, rec)
	if v := pv.Removes.VirtualIP; v == nil || v.Address != "10.9.0.50/24" || v.Secret != ingressdeploy.KeepalivedSecretName(f.envA, 2) {
		t.Errorf("removes.virtualIP = %+v, want 10.9.0.50/24 and %s", v, ingressdeploy.KeepalivedSecretName(f.envA, 2))
	}
	if strings.Contains(rec.Body.String(), "hex:") || strings.Contains(rec.Body.String(), sealed) {
		t.Errorf("the preview carries key material: %s", rec.Body)
	}
}
