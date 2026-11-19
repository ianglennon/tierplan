// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/ingressrecon"
)

// The ingress_render_failed item of GET /api/warnings (Phase 21, RTE-01, D-20, D-29).

// renderWarning is the ingress_render_failed item among items, or nil when there is none.
func renderWarning(items []api.WarningBody) *api.WarningBody {
	for i := range items {
		if items[i].ID == "ingress_render_failed" {
			return &items[i]
		}
	}
	return nil
}

// D-29 tracer: a failure the reconciler holds reaches the instance admin's bell with its route and code, and an
// environment Admin gets no such item.
func TestIngressRenderFailureReachesTheBell(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	since := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	f.renderFailures = func() []ingressrecon.Failure {
		return []ingressrecon.Failure{{
			EnvID: f.envA, Hostname: "app.example.com", PathPrefix: "/api",
			Code: "ingress_render_unsafe_value", Since: since,
		}}
	}

	got := renderWarning(f.warningItems(t, f.admin))
	if got == nil || got.IngressRender == nil {
		t.Fatalf("no ingress_render_failed item for the instance admin")
	}
	in := got.IngressRender
	if in.Hostname != "app.example.com" || in.Path != "/api" || in.Code != "ingress_render_unsafe_value" || in.More != 0 {
		t.Errorf("the item is %+v, want the route, the code and no others", in)
	}

	// An environment Admin lacks the global settings.read the feed needs; whatever they are answered, it names no
	// render failure.
	rec := f.credCall(t, f.adminA, http.MethodGet, "/api/warnings", "")
	if rec.Code == http.StatusOK {
		t.Errorf("an environment Admin read the instance warnings: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "ingress_render_failed") || strings.Contains(rec.Body.String(), "app.example.com") {
		t.Errorf("an environment Admin was told of a render failure: %d %s", rec.Code, rec.Body)
	}
}
