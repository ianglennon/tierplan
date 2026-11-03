// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/regclient/regclient/types/errs"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/registry"
)

// Check now and the update object of the services list (plan 12-37, IMG-04, IMG-05). Docker is a fake
// daemon behind the real monitor, snapshot cache, aggregator and cluster reader; the registry is a
// scripted Heads; the update service, its SQLite store, the router, policy and permission checks are the
// real ones.

// updDigest is a digest made of one repeated character.
func updDigest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// headAnswer is what the scripted registry says for a repository: a digest, or an error.
type headAnswer struct {
	digest string
	err    error
}

// scriptedHeads answers for the registry by repository path and counts nothing else. A repository nobody
// scripted is unreachable. Every digest names one image, not an index.
type scriptedHeads struct {
	mu      sync.Mutex
	answers map[string]headAnswer
}

func newScriptedHeads(answers map[string]headAnswer) *scriptedHeads {
	return &scriptedHeads{answers: answers}
}

func (h *scriptedHeads) Digest(_ context.Context, _ string, ref registry.Ref) (string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.answers[ref.Path]
	if !ok {
		return "", "", errors.New("connection refused")
	}
	return a.digest, "", a.err
}

func (h *scriptedHeads) Index(context.Context, string, registry.Ref, string) (bool, map[string]string, error) {
	return false, nil, nil
}

// updatesEnv is a local swarm manager environment whose routes check images with heads.
func updatesEnv(t *testing.T, heads *scriptedHeads, services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, swarmManagerEngine(services...))
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

// updatedService is a service running the digest given of repo team/<name>.
func updatedService(name string, running byte) dockertest.FakeService {
	return dockertest.FakeService{
		ID: "svc-" + name, Name: name, Mode: "replicated", Running: 1, Desired: 1, Version: 1,
		Image: fmt.Sprintf("registry.test/team/%s:1@%s", name, updDigest(running)),
	}
}

type updateInfo struct {
	State            string   `json:"state"`
	Reason           string   `json:"reason"`
	Registry         string   `json:"registry"`
	Repository       string   `json:"repository"`
	Tag              string   `json:"tag"`
	RunningDigest    string   `json:"runningDigest"`
	RemoteDigest     string   `json:"remoteDigest"`
	ChangedPlatforms []string `json:"changedPlatforms"`
	CheckedAt        string   `json:"checkedAt"`
	SuccessAt        string   `json:"successAt"`
	Note             string   `json:"note"`
}

type updatedItem struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Update *updateInfo `json:"update"`
}

type updatedList struct {
	Items []updatedItem `json:"items"`
}

type updateCheckAnswer struct {
	Checked int `json:"checked"`
	Updates int `json:"updates"`
	Failed  int `json:"failed"`
}

// updateOf is the update object of the service named name in a list, nil when it has none.
func updateOf(t *testing.T, list updatedList, name string) *updateInfo {
	t.Helper()
	for _, it := range list.Items {
		if it.Name == name {
			return it.Update
		}
	}
	t.Fatalf("no service %q in %+v", name, list)
	return nil
}

func (f *envFix) checkNow(t *testing.T, e envBody) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/updates/check", "")
}

func TestCheckNowThenServicesListShowsState(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/behind": {digest: updDigest('b')},
		"team/same":   {digest: updDigest('c')},
		"team/denied": {err: fmt.Errorf("denied: token=abc: %w", errs.ErrHTTPUnauthorized)},
	})
	f, e := updatesEnv(t, heads,
		updatedService("behind", 'a'), updatedService("same", 'c'), updatedService("denied", 'd'))

	// Nothing has been checked yet: the list says so, in every row.
	before := decode[updatedList](t, f.get(t, e, "services"))
	for _, name := range []string{"behind", "same", "denied"} {
		if u := updateOf(t, before, name); u == nil || u.State != "not_checked" {
			t.Errorf("%s before the check = %+v, want not_checked", name, u)
		}
	}

	rec := f.checkNow(t, e)
	if rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	if got := decode[updateCheckAnswer](t, rec); got != (updateCheckAnswer{Checked: 3, Updates: 1, Failed: 1}) {
		t.Errorf("check now = %+v, want 3 checked, 1 update, 1 failed", got)
	}

	after := decode[updatedList](t, f.get(t, e, "services"))
	behind := updateOf(t, after, "behind")
	if behind == nil || behind.State != "update_available" || behind.RunningDigest != updDigest('a') || behind.RemoteDigest != updDigest('b') ||
		behind.Registry != "registry.test" || behind.Repository != "team/behind" || behind.Tag != "1" || behind.CheckedAt == "" || behind.SuccessAt == "" {
		t.Errorf("behind = %+v, want update_available naming both digests and the image", behind)
	}
	if same := updateOf(t, after, "same"); same == nil || same.State != "up_to_date" || same.Reason != "" {
		t.Errorf("same = %+v, want up_to_date", same)
	}
	if denied := updateOf(t, after, "denied"); denied == nil || denied.State != "check_failed" || denied.Reason != "auth" || denied.SuccessAt != "" {
		t.Errorf("denied = %+v, want check_failed with reason auth", denied)
	}
}
