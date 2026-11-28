// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
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
	if in.EnvID != f.envA {
		t.Errorf("the item names environment %q, want %q (the bell opens its Routes tab)", in.EnvID, f.envA)
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

// D-29: several failures give one item that names the oldest (the earliest Since) and counts the others.
func TestIngressRenderFailuresAggregateIntoOneItem(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	f.renderFailures = func() []ingressrecon.Failure {
		return []ingressrecon.Failure{
			{EnvID: "env-a", Hostname: "late.example.com", PathPrefix: "/late", Code: "ingress_render_strip_root", Since: t0.Add(2 * time.Minute)},
			{EnvID: "env-b", Hostname: "first.example.com", PathPrefix: "/first", Code: "ingress_render_unsafe_value", Since: t0},
			{EnvID: "env-c", Hostname: "middle.example.com", PathPrefix: "/middle", Code: "ingress_render_name_collision", Since: t0.Add(time.Minute)},
		}
	}

	var n int
	var got *api.WarningBody
	for _, it := range f.warningItems(t, f.admin) {
		if it.ID == "ingress_render_failed" {
			n++
			got = &it
		}
	}
	if n != 1 || got.IngressRender == nil {
		t.Fatalf("got %d ingress_render_failed items, want exactly one with a body", n)
	}
	in := got.IngressRender
	if in.Hostname != "first.example.com" || in.Path != "/first" || in.Code != "ingress_render_unsafe_value" || in.More != 2 {
		t.Errorf("the item is %+v, want the oldest failure with more 2", in)
	}
}

// A failure that is not about one route (a read error) names no hostname and no path.
func TestIngressRenderReadFailureNamesNoRoute(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.renderFailures = func() []ingressrecon.Failure {
		return []ingressrecon.Failure{{
			EnvID: f.envA, Code: ingressrecon.CodeReadFailed, Since: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC),
		}}
	}

	got := renderWarning(f.warningItems(t, f.admin))
	if got == nil || got.IngressRender == nil {
		t.Fatalf("no ingress_render_failed item for a read failure")
	}
	in := got.IngressRender
	if in.Code != "ingress_render_read_failed" || in.Hostname != "" || in.Path != "" || in.More != 0 {
		t.Errorf("the item is %+v, want the read-failed code and no route", in)
	}
}

// D-29: the item is derived on every read; once the reconciler holds no failure the next read has no item.
func TestIngressRenderItemIsDerivedOnRead(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	if renderWarning(f.warningItems(t, f.admin)) != nil {
		t.Fatalf("an ingress_render_failed item with no failure")
	}

	reads := 0
	f.renderFailures = func() []ingressrecon.Failure {
		reads++
		if reads > 1 {
			return nil
		}
		return []ingressrecon.Failure{{
			EnvID: f.envA, Hostname: "app.example.com", PathPrefix: "/api",
			Code: "ingress_render_unsafe_value", Since: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC),
		}}
	}
	if renderWarning(f.warningItems(t, f.admin)) == nil {
		t.Fatalf("no item on the first read, while the failure is held")
	}
	if renderWarning(f.warningItems(t, f.admin)) != nil {
		t.Errorf("the item is still there on the second read, after the failure is gone")
	}
}

// T-21-45: the item is facts only: its JSON has the keys id and ingressRender, and ingressRender only envId (an
// identifier), hostname, path, code and more, whatever the failure was handed.
func TestIngressRenderItemCarriesFactsOnly(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.renderFailures = func() []ingressrecon.Failure {
		return []ingressrecon.Failure{
			{EnvID: "env-a", Hostname: "app.example.com", PathPrefix: "/api", Code: "ingress_render_unsafe_value", Since: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)},
			{EnvID: "env-b", Hostname: "other.example.com", PathPrefix: "/", Code: "ingress_render_failed", Since: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)},
		}
	}

	rec := f.credCall(t, f.admin, http.MethodGet, "/api/warnings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/warnings: %d %s", rec.Code, rec.Body)
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode the warnings: %v", err)
	}
	var item map[string]json.RawMessage
	for _, it := range raw.Items {
		if string(it["id"]) == `"ingress_render_failed"` {
			item = it
		}
	}
	if item == nil {
		t.Fatalf("no ingress_render_failed item in %s", rec.Body)
	}
	if keys := sortedKeys(item); !slices.Equal(keys, []string{"id", "ingressRender"}) {
		t.Errorf("the item has the keys %v, want id and ingressRender", keys)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(item["ingressRender"], &body); err != nil {
		t.Fatalf("decode the body: %v", err)
	}
	if keys := sortedKeys(body); !slices.Equal(keys, []string{"code", "envId", "hostname", "more", "path"}) {
		t.Errorf("the body has the keys %v, want code, envId, hostname, more and path", keys)
	}
}

// sortedKeys is the keys of m in order.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
