// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package agents_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// shiftedClock is the janitor's clock moved forward by an amount the test sets, so an image created
// a moment ago is past the grace period at once. It never fires After: the runner does not wait on it.
type shiftedClock struct{ by atomic.Int64 }

func (c *shiftedClock) Now() time.Time {
	return time.Now().Add(time.Duration(c.by.Load()))
}

func (c *shiftedClock) After(time.Duration) <-chan time.Time { return nil }

// janitorItemRead and janitorPreviewRead are the parts of the preview a test reads.
type janitorItemRead struct {
	Type   string `json:"type"`
	NodeID string `json:"nodeId"`
	Ref    string `json:"ref"`
	Class  string `json:"class"`
	Reason struct {
		Code string `json:"code"`
	} `json:"reason"`
}

type janitorPreviewRead struct {
	Items      []janitorItemRead `json:"items"`
	NotRemoved []janitorItemRead `json:"notRemoved"`
}

// TestJanitorImagesIntegration judges and removes images on a real engine (JAN-05, D-07, RESEARCH F7,
// A1 and A8). The host holds four images the janitor could look at:
//
//   - the first build of app:1, untagged when a second build took its tag: the only one it removes
//   - the second build of app:1: tagged, never listed
//   - an image pulled by digest only (a registry's pinned reference, the way a swarm service runs
//     it): no tag but a digest, never listed
//   - an image a stopped container was made from whose tag then moved: untagged and unused by name,
//     but a container uses it, so never listed
//
// Before the janitor's clock moves on, the first build is inside the grace period and is not
// removable; after 48 hours it is. Clean now removes it and the other three remain.
func TestJanitorImagesIntegration(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dockertest.LoadImage(t, dockertest.ServiceTestImage, host)
	dumpOnFailure(t, host)
	clock := &shiftedClock{}
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway, JanitorClock: clock})

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	// Which image store ran (RESEARCH A8): the classic one keeps layers, the containerd one keeps content.
	t.Logf("image store: %s", strings.TrimSpace(sh(t, host, "docker info --format '{{.DriverStatus}}'")))

	idOf := func(ref string) string {
		return strings.TrimSpace(sh(t, host, "docker inspect -f '{{.Id}}' "+ref))
	}
	base := dockertest.ServiceTestImage
	// Two builds under one tag: the first becomes dangling when the second takes app:1.
	sh(t, host, "docker run --name b1 "+base+" sh -c 'echo one > /v' && docker commit b1 app:1")
	first := idOf("app:1")
	sh(t, host, "docker run --name b2 "+base+" sh -c 'echo two > /v' && docker commit b2 app:1")
	second := idOf("app:1")

	// An image held only by its digest: pushed to a registry, removed locally and pulled back by digest.
	addr := dockertest.StartRegistry(t, host, dockertest.RegistryOptions{})
	pinned := addr + "/pinned:1"
	sh(t, host, "docker run --name b3 "+base+" sh -c 'echo three > /v' && docker commit b3 "+pinned+" && docker push "+pinned)
	digestRef := strings.TrimSpace(sh(t, host, "docker inspect -f '{{index .RepoDigests 0}}' "+pinned))
	sh(t, host, "docker rmi "+pinned+" && docker pull "+digestRef)
	// What the engine reports for it is logged: the containerd store may name a pulled-by-digest image in its tags.
	t.Logf("digest-pulled image: tags %s digests %s", strings.TrimSpace(sh(t, host, "docker inspect -f '{{json .RepoTags}}' "+digestRef)),
		strings.TrimSpace(sh(t, host, "docker inspect -f '{{json .RepoDigests}}' "+digestRef)))
	digestOnly := idOf(digestRef)

	// An image a stopped container was made from, whose tag then moved to another image.
	sh(t, host, "docker run --name b4 "+base+" sh -c 'echo four > /v' && docker commit b4 used:1")
	used := idOf("used:1")
	sh(t, host, "docker run --name user used:1 true")
	sh(t, host, "docker run --name b5 "+base+" sh -c 'echo five > /v' && docker commit b5 used:1")

	if len(slices.Compact([]string{first, second, digestOnly, used})) != 4 {
		t.Fatalf("the four images share an ID: %v", []string{first, second, digestOnly, used})
	}

	preview := func() janitorPreviewRead {
		t.Helper()
		code, body := srv.Do(http.MethodPost, "/api/env/"+env.ID+"/janitor/preview", nil)
		if code != http.StatusOK {
			t.Fatalf("preview: %d %s", code, body)
		}
		var p janitorPreviewRead
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return p
	}
	find := func(items []janitorItemRead, ref string) (janitorItemRead, bool) {
		for _, it := range items {
			if it.Ref == ref {
				return it, true
			}
		}
		return janitorItemRead{}, false
	}

	// Inside the grace period: the first build is held, nothing is removable.
	before := preview()
	if len(before.Items) != 0 {
		t.Fatalf("items before the grace period = %+v, want none", before.Items)
	}
	if it, ok := find(before.NotRemoved, first); !ok || it.Class != "grace" || it.Reason.Code != "younger_than_grace" {
		t.Fatalf("notRemoved = %+v, want the first build held by grace", before.NotRemoved)
	}

	clock.by.Store(int64(48 * time.Hour))
	after := preview()
	if len(after.Items) != 1 || after.Items[0].Ref != first || after.Items[0].Type != "image" ||
		after.Items[0].Class != "removable" || after.Items[0].Reason.Code != "dangling_image" {
		t.Fatalf("items = %+v, want exactly the first build as a dangling_image", after.Items)
	}
	for name, id := range map[string]string{"the second build": second, "the digest-pulled image": digestOnly, "the image a container uses": used} {
		if _, ok := find(after.Items, id); ok {
			t.Errorf("%s (%s) is in the items: it must never be nominated", name, id)
		}
		if _, ok := find(after.NotRemoved, id); ok {
			t.Errorf("%s (%s) is listed under notRemoved: it is not the janitor's business at all", name, id)
		}
	}

	code, body := srv.Do(http.MethodPost, "/api/env/"+env.ID+"/janitor/runs", map[string]any{
		"items": []map[string]string{{"type": "image", "nodeId": after.Items[0].NodeID, "ref": first}},
	})
	if code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", code, body)
	}
	var started struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	var run struct {
		State  string `json:"state"`
		Counts struct {
			RemovedImages int `json:"removedImages"`
			Failed        int `json:"failed"`
		} `json:"counts"`
	}
	srv.WaitFor("the janitor run to end", 2*time.Minute, func() (bool, string) {
		code, body := srv.Do(http.MethodGet, "/api/env/"+env.ID+"/janitor/runs/"+started.RunID, nil)
		if code != http.StatusOK || json.Unmarshal(body, &run) != nil {
			return false, string(body)
		}
		return run.State != "running" && run.State != "waiting", string(body)
	})
	if run.State != "completed" || run.Counts.RemovedImages != 1 || run.Counts.Failed != 0 {
		t.Fatalf("run = %+v, want completed with one image removed", run)
	}

	// On the engine: the first build is gone and the other three are still there.
	if _, err := host.Sh(execCtx(t, time.Minute), "docker image inspect "+first); err == nil {
		t.Errorf("the first build %s is still on the host", first)
	}
	for name, id := range map[string]string{"the second build": second, "the digest-pulled image": digestOnly, "the image a container uses": used} {
		if _, err := host.Sh(execCtx(t, time.Minute), "docker image inspect "+id); err != nil {
			t.Errorf("%s (%s) was removed: %v", name, id, err)
		}
	}
	rows := srv.AuditRows(audit.ActionJanitorCleaned)
	if len(rows) != 1 || rows[0].Details["removed_images"] != "1" {
		t.Errorf("audit rows = %+v, want one summary row that counts one removed image", rows)
	}
}
