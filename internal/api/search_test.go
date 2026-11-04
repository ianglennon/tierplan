// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Search across the environments (plan 12-32, ENV-11, D-18). The fixtures are those of fleet_test.go.

type searchHit struct {
	Type     string `json:"type"`
	EnvID    string `json:"envId"`
	EnvName  string `json:"envName"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Managed  bool   `json:"managed"`
	Stack    string `json:"stack"`
	Image    string `json:"image"`
	Node     string `json:"node"`
	Driver   string `json:"driver"`
	Subnet   string `json:"subnet"`
	Size     int64  `json:"size"`
	Services int    `json:"services"`
}

type searchResp struct {
	Results []searchHit `json:"results"`
	Counts  struct {
		Stack, Service, Container, Image, Network, All int
	} `json:"counts"`
	Truncated   bool               `json:"truncated"`
	NotSearched []fleetNotIncluded `json:"notSearched"`
	SearchedAt  string             `json:"searchedAt"`
}

// search asks GET /api/search as a user ("" is the instance admin) and decodes the answer.
func (f *fleetFix) search(t *testing.T, as, query string) searchResp {
	t.Helper()
	rec := f.fleetGet(t, as, "/api/search"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET /api/search%s as %q = %d %s", query, as, rec.code, rec.body)
	}
	var r searchResp
	if err := json.Unmarshal(rec.body, &r); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return r
}

// hits is the results as "type env name", in order, for a comparison that reads.
func (r searchResp) hits() []string {
	out := make([]string, 0, len(r.Results))
	for _, h := range r.Results {
		out = append(out, h.Type+" "+h.EnvName+" "+h.Name)
	}
	return out
}

// containerIn makes a standalone host's container row.
func containerIn(id, name, image, project string) envs.ContainerRow {
	labels := map[string]string{}
	if project != "" {
		labels["com.docker.compose.project"] = project
	}
	return envs.ContainerRow{
		ContainerRow: dockerx.ContainerRow{ID: id, Name: name, Image: image, State: "running", Labels: labels},
		Node:         "host-1", NodeID: "n1",
	}
}

// standalone makes id a reporting standalone host holding these containers.
func (f *fleetFake) standalone(id string, containers ...envs.ContainerRow) {
	f.online(id)
	s := f.snap[id]
	s.agg.Containers = containers
	f.snap[id] = s
}

// managedStack stores a stack TierPlan manages in an environment.
func (f *fleetFix) managedStack(t *testing.T, envID, name string, target stacks.Target) stackops.Stack {
	t.Helper()
	st, _, err := stackops.Repo{Store: f.st, Vault: f.v}.CreateStack(testsupport.Context(t), envID, name, target, "ada",
		stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSearchFindsAcrossEnvironments(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{
		{ID: "s1", Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1", Stack: "webshop"},
		{ID: "s2", Name: "cache", Mode: "replicated", Running: 1, Desired: 1, Image: "redis:7", Stack: "other"},
	})
	managed := f.managedStack(t, alpha, "mywebstack", stacks.TargetSwarm)
	f.fake.standalone(beta,
		containerIn("aaaa1111", "web-1", "nginx:1", "site"),
		containerIn("bbbb2222", "db", "web:2", "site"),
		containerIn("cccc3333", "worker", "busybox", ""))
	f.fleet.Store = f.st
	f.rebuild(t)

	got := f.search(t, "", "?q=web")
	// Exact name first, then names that start with it, then names that hold it, then an image that does.
	// Equal matches go by environment name, then name.
	want := []string{
		"service Alpha web",
		"stack Alpha webshop",
		"container Beta web-1",
		"stack Alpha mywebstack",
		"container Beta db",
	}
	if !slices.Equal(got.hits(), want) {
		t.Fatalf("results = %v, want %v", got.hits(), want)
	}
	if c := got.Counts; c.Stack != 2 || c.Service != 1 || c.Container != 2 || c.Image != 0 || c.Network != 0 || c.All != 5 {
		t.Errorf("counts = %+v", c)
	}
	if got.Truncated || len(got.NotSearched) != 0 || got.SearchedAt != fleetNow.Format("2006-01-02T15:04:05Z") {
		t.Errorf("truncated %v, notSearched %+v, searchedAt %q", got.Truncated, got.NotSearched, got.SearchedAt)
	}

	byName := map[string]searchHit{}
	for _, h := range got.Results {
		byName[h.Type+" "+h.Name] = h
	}
	if h := byName["service web"]; h.EnvID != alpha || h.ID != "s1" || h.Image != "nginx:1" || h.Stack != "webshop" {
		t.Errorf("service = %+v", h)
	}
	if h := byName["container web-1"]; h.EnvID != beta || h.ID != "aaaa1111" || h.Image != "nginx:1" || h.Node != "host-1" || h.Stack != "site" {
		t.Errorf("container = %+v", h)
	}
	if h := byName["stack mywebstack"]; h.ID != managed.ID || !h.Managed {
		t.Errorf("managed stack = %+v", h)
	}
	if h := byName["stack webshop"]; h.ID != "" || h.Managed || h.Services != 1 {
		t.Errorf("discovered stack = %+v", h)
	}
}
