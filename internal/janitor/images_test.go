// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"slices"
	"testing"
	"time"
)

// What the janitor makes of an image (plan 11-10, JAN-05, D-07, RESEARCH F7 and Pitfall 1). Every
// case judges at t0 under the default policy, a day of grace, unless it says otherwise.

// bareImage is an untagged, undigested image ago old on the node n1.
func bareImage(id string, ago time.Duration) Image {
	return Image{ID: "sha256:" + id, NodeID: "n1-id", Node: "n1", Size: 1000, Created: t0.Add(-ago), Labels: map[string]string{}}
}

func TestClassifyImageStrictDangling(t *testing.T) {
	t.Parallel()
	const digestOnly = "busybox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name         string
		tags, digest []string
		wantListed   bool
	}{
		{"only the none:none placeholder", []string{"<none>:<none>"}, nil, true},
		{"the bare none placeholder", []string{"<none>"}, nil, true},
		{"an empty tag", []string{""}, nil, true},
		{"the none@none placeholder digest", []string{"<none>:<none>"}, []string{"<none>@<none>"}, true},
		{"a digest and no tag, the way a swarm pulls", []string{"<none>:<none>"}, []string{digestOnly}, false},
		{"a tag and no digest, unused for a year", []string{"nginx:1.27"}, nil, false},
		{"a tag and a digest", []string{"nginx:1.27"}, []string{digestOnly}, false},
		{"a real tag beside the placeholder", []string{"<none>:<none>", "app:1"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			img := bareImage("a1", 3*24*time.Hour)
			img.RepoTags, img.RepoDigests = c.tags, c.digest
			item, listed := ClassifyImage(img, false, false, true, DefaultPolicy(), t0)
			if listed != c.wantListed {
				t.Fatalf("listed = %v (%+v), want %v", listed, item, c.wantListed)
			}
			if !listed {
				return
			}
			if item.Class != ClassRemovable || item.Reason.Code != ReasonDanglingImage {
				t.Fatalf("item = %+v, want removable dangling_image", item)
			}
			if item.Type != KindImage || item.Ref != "sha256:a1" || item.Name != "a1" || item.NodeID != "n1-id" || item.Node != "n1" {
				t.Errorf("item = %+v, want the image's identity on its node, named by its short ID", item)
			}
			if item.Age != 72*time.Hour || item.Reason.Age != 72*time.Hour || item.Size != 1000 {
				t.Errorf("age = %v size = %d, want 72h and the 1000 bytes the list reports", item.Age, item.Size)
			}
		})
	}
	t.Run("the keep label protects it", func(t *testing.T) {
		t.Parallel()
		img := bareImage("a2", 3*24*time.Hour)
		img.Labels = map[string]string{"tierplan.keep": "true"}
		item, listed := ClassifyImage(img, false, false, true, DefaultPolicy(), t0)
		if !listed || item.Class != ClassProtected || item.Reason.Code != ReasonKeptLabel {
			t.Fatalf("item = %+v listed = %v, want protected kept_label", item, listed)
		}
	})
	t.Run("a keep label that is not true does not", func(t *testing.T) {
		t.Parallel()
		img := bareImage("a3", 3*24*time.Hour)
		img.Labels = map[string]string{"tierplan.keep": "false"}
		if item, listed := ClassifyImage(img, false, false, true, DefaultPolicy(), t0); !listed || item.Class != ClassRemovable {
			t.Fatalf("item = %+v listed = %v, want removable", item, listed)
		}
	})
}

func TestClassifyImageUsage(t *testing.T) {
	t.Parallel()
	t.Run("used by a container on its node", func(t *testing.T) {
		t.Parallel()
		if item, listed := ClassifyImage(bareImage("a1", 72*time.Hour), true, false, true, DefaultPolicy(), t0); listed {
			t.Fatalf("item = %+v, want an image a container uses left out of the plan", item)
		}
	})
	t.Run("an exited container uses it too, and only on its own node", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{
			Nodes: []Node{{ID: "n1-id", Name: "n1"}, {ID: "n2-id", Name: "n2"}}, ReadAt: t0,
			Containers: []Container{{ID: "c1", NodeID: "n1-id", Node: "n1", State: "exited", ImageID: "sha256:a1"}},
			Images: []Image{
				{ID: "sha256:a1", NodeID: "n1-id", Node: "n1", Created: t0.Add(-72 * time.Hour)},
				{ID: "sha256:a1", NodeID: "n2-id", Node: "n2", Created: t0.Add(-72 * time.Hour)},
			},
		}
		plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0)
		// The stopped container is a plain one the standalone rules look at too (it was not inspected, so it is
		// cant_tell); only the images are this case's business.
		var notRemovedImages int
		for _, it := range plan.NotRemoved {
			if it.Type == KindImage {
				notRemovedImages++
			}
		}
		if len(plan.Items) != 1 || plan.Items[0].NodeID != "n2-id" || notRemovedImages != 0 {
			t.Fatalf("plan = %+v, want the copy on n2 alone: the same image ID is one item per node, judged per node", plan)
		}
	})
	t.Run("a running or created container counts too", func(t *testing.T) {
		t.Parallel()
		for _, state := range []string{"running", "created", "paused", "restarting", "dead"} {
			s := Snapshot{
				Nodes: []Node{{ID: "n1-id", Name: "n1"}}, ReadAt: t0,
				Containers: []Container{{ID: "c1", NodeID: "n1-id", State: state, ImageID: "sha256:a1"}},
				Images:     []Image{{ID: "sha256:a1", NodeID: "n1-id", Node: "n1", Created: t0.Add(-72 * time.Hour)}},
			}
			if plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0); len(plan.Items)+len(plan.NotRemoved) != 0 {
				t.Errorf("a %s container: plan = %+v, want the image left out", state, plan)
			}
		}
	})
}

func TestClassifyImageServices(t *testing.T) {
	t.Parallel()
	t.Run("a spec names it", func(t *testing.T) {
		t.Parallel()
		if item, listed := ClassifyImage(bareImage("a1", 72*time.Hour), false, true, true, DefaultPolicy(), t0); listed {
			t.Fatalf("item = %+v, want an image a service spec names left out", item)
		}
	})
	t.Run("a service scaled to zero still names its image", func(t *testing.T) {
		t.Parallel()
		id := "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
		s := swarmOf()
		s.ServicesRead = true
		s.Services["svc-zero"] = Service{Name: "zero"}
		s.ServiceImages = []string{"busybox:1@" + id}
		s.Images = []Image{{ID: id, NodeID: "n1-id", Node: "n1", Created: t0.Add(-72 * time.Hour)}}
		if plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0); len(plan.Items)+len(plan.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want the image the zero-replica service names left out", plan)
		}
		s.ServiceImages = []string{id}
		if plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0); len(plan.Items)+len(plan.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want the image a spec names by its ID left out", plan)
		}
		s.ServiceImages = []string{"busybox:1", "other@sha256:2222"}
		if plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0); len(plan.Items) != 1 {
			t.Fatalf("plan = %+v, want the image removable when no spec names it: a tag a swarm would pull again is not the image", plan)
		}
	})
	t.Run("services unread on a swarm: nothing is removable", func(t *testing.T) {
		t.Parallel()
		s := swarmOf()
		s.ManagerRead, s.ServicesRead = false, false
		s.Images = []Image{
			{ID: "sha256:a1", NodeID: "n1-id", Node: "n1", Created: t0.Add(-72 * time.Hour)},
			{ID: "sha256:a2", NodeID: "n1-id", Node: "n1", Created: t0.Add(-96 * time.Hour)},
			{ID: "sha256:a3", NodeID: "n1-id", Node: "n1", RepoTags: []string{"app:1"}, Created: t0.Add(-96 * time.Hour)},
		}
		plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0)
		if len(plan.Items) != 0 {
			t.Fatalf("items = %+v, want none removable", plan.Items)
		}
		if len(plan.NotRemoved) != 2 {
			t.Fatalf("notRemoved = %+v, want the two bare images and not the tagged one", plan.NotRemoved)
		}
		for _, it := range plan.NotRemoved {
			if it.Class != ClassCantTell || it.Reason.Code != ReasonServicesUnread {
				t.Errorf("item = %+v, want cant_tell services_unread", it)
			}
		}
	})
	t.Run("an environment that is not a swarm has no specs to read", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{Nodes: []Node{{ID: "n1-id", Name: "n1"}}, ReadAt: t0, Images: []Image{bareImage("a1", 72*time.Hour)}}
		if plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0); len(plan.Items) != 1 {
			t.Fatalf("plan = %+v, want the image removable on a standalone host", plan)
		}
	})
}

func TestClassifyImageGraceBoundary(t *testing.T) {
	t.Parallel()
	grace := DefaultPolicy().Grace
	t.Run("created exactly the grace period ago", func(t *testing.T) {
		t.Parallel()
		item, listed := ClassifyImage(bareImage("a1", grace), false, false, true, DefaultPolicy(), t0)
		if !listed || item.Class != ClassRemovable {
			t.Fatalf("item = %+v listed = %v, want removable at exactly the grace period", item, listed)
		}
	})
	t.Run("one second short", func(t *testing.T) {
		t.Parallel()
		item, listed := ClassifyImage(bareImage("a1", grace-time.Second), false, false, true, DefaultPolicy(), t0)
		if !listed || item.Class != ClassGrace || item.Reason.Code != ReasonYoungerThanGrace || item.Reason.Remaining != time.Second {
			t.Fatalf("item = %+v listed = %v, want grace with 1 s left", item, listed)
		}
	})
	t.Run("the policy's own grace", func(t *testing.T) {
		t.Parallel()
		p := DefaultPolicy()
		p.Grace = 6 * time.Hour
		if item, _ := ClassifyImage(bareImage("a1", 7*time.Hour), false, false, true, p, t0); item.Class != ClassRemovable {
			t.Errorf("item = %+v, want removable past a 6 h grace", item)
		}
		if item, _ := ClassifyImage(bareImage("a1", 5*time.Hour), false, false, true, p, t0); item.Class != ClassGrace || item.Reason.Remaining != time.Hour {
			t.Errorf("item = %+v, want grace with 1 h left", item)
		}
	})
	t.Run("an image from the future is held, not aged", func(t *testing.T) {
		t.Parallel()
		item, _ := ClassifyImage(bareImage("a1", -time.Hour), false, false, true, DefaultPolicy(), t0)
		if item.Class != ClassGrace || item.Age != 0 {
			t.Fatalf("item = %+v, want grace with no age", item)
		}
	})
}

func TestClassifyImageEmptyInputs(t *testing.T) {
	t.Parallel()
	t.Run("a node with no images contributes nothing", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{Nodes: []Node{{ID: "n1-id", Name: "n1"}}, ReadAt: t0}
		plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0)
		if len(plan.Items) != 0 || len(plan.NotRemoved) != 0 || plan.Items == nil || plan.NotRemoved == nil {
			t.Fatalf("plan = %+v, want empty lists that are not nil", plan)
		}
	})
	t.Run("nil tags and no digests is dangling", func(t *testing.T) {
		t.Parallel()
		img := Image{ID: "sha256:a1", NodeID: "n1-id", Node: "n1", Created: t0.Add(-72 * time.Hour), Size: -1}
		item, listed := ClassifyImage(img, false, false, true, DefaultPolicy(), t0)
		if !listed || item.Class != ClassRemovable {
			t.Fatalf("item = %+v listed = %v, want removable", item, listed)
		}
		if item.Size != -1 {
			t.Errorf("size = %d, want -1 for a size the engine did not give", item.Size)
		}
	})
	t.Run("no labels at all", func(t *testing.T) {
		t.Parallel()
		img := Image{ID: "sha256:a1", NodeID: "n1-id", Created: t0.Add(-72 * time.Hour)}
		if item, listed := ClassifyImage(img, false, false, true, DefaultPolicy(), t0); !listed || item.Class != ClassRemovable {
			t.Fatalf("item = %+v listed = %v, want removable with nil labels", item, listed)
		}
	})
	t.Run("an ID shorter than twelve characters", func(t *testing.T) {
		t.Parallel()
		if item, _ := ClassifyImage(Image{ID: "sha256:ab", Created: t0.Add(-72 * time.Hour)}, false, false, true, DefaultPolicy(), t0); item.Name != "ab" {
			t.Errorf("name = %q, want ab", item.Name)
		}
	})
}

// Image items are ordered by node name, then oldest first, then ID, and after the containers.
func TestImageOrder(t *testing.T) {
	t.Parallel()
	mk := func(id, node string, ago time.Duration) Image {
		return Image{ID: "sha256:" + id, NodeID: node + "-id", Node: node, Created: t0.Add(-ago)}
	}
	s := Snapshot{
		Nodes: []Node{{ID: "a-id", Name: "a"}, {ID: "b-id", Name: "b"}}, ReadAt: t0,
		Images: []Image{
			mk("f1", "b", 100*time.Hour), mk("d4", "a", 50*time.Hour), mk("c3", "a", 90*time.Hour),
			mk("e2", "a", 90*time.Hour), mk("b9", "a", 70*time.Hour),
		},
	}
	plan := Build(Input{Snapshot: s}, DefaultPolicy(), t0)
	want := []string{"sha256:c3", "sha256:e2", "sha256:b9", "sha256:d4", "sha256:f1"}
	if got := refs(plan.Items); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v: node a before b, the oldest first, equal ages by ID", got, want)
	}
	// Reversing the input changes nothing.
	slices.Reverse(s.Images)
	if got := refs(Build(Input{Snapshot: s}, DefaultPolicy(), t0).Items); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v whatever the order read", got, want)
	}
}
