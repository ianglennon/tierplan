// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The container list's server-side search, status filter and counts (plan 04-06,
// D-03, T-04-29, T-04-31). The engine is a FakeDaemon behind the real monitor.

// filterItem is a container row as the list page reads it, including the fields the
// filter adds.
type filterItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Project  string `json:"project"`
	Stack    string `json:"stack"`
	Service  string `json:"service"`
	Health   string `json:"health"`
	SelfKind string `json:"selfKind"`
	ReadOnly bool   `json:"readOnly"`
	IsSelf   bool   `json:"isSelf"`
}

type statusCounts struct {
	Running    int `json:"running"`
	Exited     int `json:"exited"`
	Paused     int `json:"paused"`
	Restarting int `json:"restarting"`
	Unhealthy  int `json:"unhealthy"`
}

type filterList struct {
	Items           []filterItem `json:"items"`
	Total           int          `json:"total"`
	Truncated       bool         `json:"truncated"`
	UnfilteredTotal int          `json:"unfilteredTotal"`
	StatusCounts    statusCounts `json:"statusCounts"`
}

func listContainersFor(t *testing.T, f *envFix, e envBody, query string) filterList {
	t.Helper()
	rec := f.get(t, e, "containers"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("containers%s = %d %s", query, rec.code, rec.body)
	}
	return decode[filterList](t, rec)
}

// manyContainers is n numbered containers, three of them in the compose project shop.
func manyContainers(n int) dockertest.FakeConfig {
	cs := make([]dockertest.FakeContainer, 0, n)
	for i := range n {
		c := dockertest.FakeContainer{
			ID:    fmt.Sprintf("%064x", i+1),
			Names: []string{fmt.Sprintf("/c%03d", i)}, Image: "busybox:1", State: "running", Status: "Up 1 hour",
		}
		if i < 3 {
			c.Labels = map[string]string{"com.docker.compose.project": "shop"}
		}
		cs = append(cs, c)
	}
	return dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", Containers: cs}
}

func TestContainerFilterSearchesTheComposeProjectBeforeTheCap(t *testing.T) {
	f, e := dashEnv(t, manyContainers(600))
	got := listContainersFor(t, f, e, "?q=shop")
	if len(got.Items) != 3 || got.Total != 3 || got.Truncated || got.UnfilteredTotal != 600 {
		t.Fatalf("q=shop: items %d total %d truncated %v unfiltered %d", len(got.Items), got.Total, got.Truncated, got.UnfilteredTotal)
	}
	for _, it := range got.Items {
		if it.Project != "shop" {
			t.Errorf("project of %s = %q, want shop", it.Name, it.Project)
		}
	}
	if up := listContainersFor(t, f, e, "?q=SHOP"); up.Total != 3 {
		t.Errorf("q=SHOP total = %d, want 3 (case-insensitive)", up.Total)
	}
	if empty := listContainersFor(t, f, e, "?q="); empty.Total != 600 {
		t.Errorf("q= total = %d, want 600 (no search)", empty.Total)
	}
}

func TestContainerFilterWithNoSearchKeepsTheFirst500ByName(t *testing.T) {
	f, e := dashEnv(t, manyContainers(600))
	got := listContainersFor(t, f, e, "")
	if len(got.Items) != 500 || got.Total != 600 || !got.Truncated || got.UnfilteredTotal != 600 {
		t.Fatalf("items %d total %d truncated %v unfiltered %d", len(got.Items), got.Total, got.Truncated, got.UnfilteredTotal)
	}
	if got.Items[0].Name != "c000" || got.Items[499].Name != "c499" {
		t.Errorf("order = %s ... %s, want name order", got.Items[0].Name, got.Items[499].Name)
	}
}

func TestContainerFilterRefusesAQueryOver128Characters(t *testing.T) {
	f, e := dashEnv(t, manyContainers(3))
	rec := f.get(t, e, "containers?q="+url.QueryEscape(strings.Repeat("a", 129)))
	if rec.code != http.StatusUnprocessableEntity {
		t.Errorf("129-character q = %d %s, want 422", rec.code, rec.body)
	}
	if rec := f.get(t, e, "containers?q="+strings.Repeat("a", 128)); rec.code != http.StatusOK {
		t.Errorf("128-character q = %d %s, want 200", rec.code, rec.body)
	}
}

func TestContainerFilterFindsALabelValueButNeverSendsItBack(t *testing.T) {
	cfg := manyContainers(2)
	cfg.Containers[1].Labels = map[string]string{"secret.token": "abc-9f3", "com.docker.stack.namespace": "blog", "com.docker.swarm.service.name": "blog_web"}
	cfg.Containers[1].Mounts = []string{"/srv/mount-9f3"}
	f, e := dashEnv(t, cfg)
	rec := f.get(t, e, "containers?q=abc-9f3")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	got := decode[filterList](t, rec)
	if got.Total != 1 || got.Items[0].Name != "c001" || got.Items[0].Stack != "blog" || got.Items[0].Service != "blog_web" {
		t.Fatalf("q=abc-9f3 = %+v", got)
	}
	for _, leaked := range []string{"abc-9f3", "secret.token", "mount-9f3"} {
		if strings.Contains(string(rec.body), leaked) {
			t.Errorf("the list carries %q (T-04-29): %s", leaked, rec.body)
		}
	}
}
