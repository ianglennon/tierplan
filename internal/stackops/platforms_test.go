// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

const (
	platformsDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	platformsDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// runningWith is a stored service running image with the platforms given.
func runningWith(t *testing.T, image string, platforms ...swarm.Platform) dockerx.ServiceDetail {
	t.Helper()
	raw, err := json.Marshal(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
		ContainerSpec: &swarm.ContainerSpec{Image: image},
		Placement:     &swarm.Placement{Platforms: platforms},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return dockerx.ServiceDetail{Image: image, Spec: raw}
}

// TestCarryPlatformsKeepsThePlatformsOfTheSameImageOnly: the platforms belong to the image. A pin takes the running
// platforms while the service runs the image it names, and never from a service on another image (T-08-97).
func TestCarryPlatformsKeepsThePlatformsOfTheSameImageOnly(t *testing.T) {
	t.Parallel()
	amd, arm := swarm.Platform{OS: "linux", Architecture: "amd64"}, swarm.Platform{OS: "linux", Architecture: "arm64"}
	live := map[string]dockerx.ServiceDetail{
		"s_same":    runningWith(t, "nginx:1.27@"+platformsDigestA, amd, arm),
		"s_other":   runningWith(t, "nginx:1.28@"+platformsDigestB, amd, arm),
		"s_nodig":   runningWith(t, "local/app:1", amd),
		"s_nodig2":  runningWith(t, "local/app:2", amd),
		"s_mixed":   runningWith(t, "nginx:1.27", amd),
		"s_noplats": runningWith(t, "nginx:1.27@"+platformsDigestA),
	}
	pins := map[string]Pin{
		"s_same":    {Service: "s_same", Ref: "nginx:1.27@" + platformsDigestA},
		"s_other":   {Service: "s_other", Ref: "nginx:1.27@" + platformsDigestA},
		"s_nodig":   {Service: "s_nodig", Ref: "local/app:1"},
		"s_nodig2":  {Service: "s_nodig2", Ref: "local/app:1"},
		"s_mixed":   {Service: "s_mixed", Ref: "nginx:1.27@" + platformsDigestA},
		"s_noplats": {Service: "s_noplats", Ref: "nginx:1.27@" + platformsDigestA},
		"s_absent":  {Service: "s_absent", Ref: "nginx:1.27@" + platformsDigestA},
		"s_kept":    {Service: "s_kept", Ref: "nginx:1.27@" + platformsDigestA, Platforms: []swarm.Platform{arm}},
	}
	live["s_kept"] = runningWith(t, "nginx:1.27@"+platformsDigestA, amd)

	carryPlatforms(pins, live)

	for name, want := range map[string][]swarm.Platform{
		"s_same":    {amd, arm},
		"s_other":   nil,
		"s_nodig":   {amd},
		"s_nodig2":  nil,
		"s_mixed":   nil,
		"s_noplats": nil,
		"s_absent":  nil,
		"s_kept":    {arm},
	} {
		if got := pins[name].Platforms; !slices.Equal(got, want) {
			t.Errorf("%s: platforms = %v, want %v", name, got, want)
		}
	}
}

// TestWithPinSetsTheImageAndItsPlatformsOnACopy: the spec given is not changed, and a pin with no platforms leaves
// the placement without any.
func TestWithPinSetsTheImageAndItsPlatformsOnACopy(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	spec := swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
		ContainerSpec: &swarm.ContainerSpec{Image: "nginx:1.27"},
		Placement:     &swarm.Placement{Constraints: []string{"node.role==worker"}, Platforms: []swarm.Platform{{OS: "plan9"}}},
	}}

	got := withPin(spec, Pin{Ref: "nginx:1.27@" + platformsDigestA, Platforms: []swarm.Platform{amd}})

	if got.TaskTemplate.ContainerSpec.Image != "nginx:1.27@"+platformsDigestA || !slices.Equal(got.TaskTemplate.Placement.Platforms, []swarm.Platform{amd}) {
		t.Errorf("pinned spec = %+v / %+v", got.TaskTemplate.ContainerSpec, got.TaskTemplate.Placement)
	}
	if !slices.Equal(got.TaskTemplate.Placement.Constraints, []string{"node.role==worker"}) {
		t.Errorf("constraints = %v, want them kept", got.TaskTemplate.Placement.Constraints)
	}
	if spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27" || spec.TaskTemplate.Placement.Platforms[0].OS != "plan9" {
		t.Errorf("withPin changed the spec it was given: %+v / %+v", spec.TaskTemplate.ContainerSpec, spec.TaskTemplate.Placement)
	}

	bare := withPin(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "x"}}}, Pin{Ref: "x@" + platformsDigestB})
	if bare.TaskTemplate.Placement == nil || len(bare.TaskTemplate.Placement.Platforms) != 0 {
		t.Errorf("a pin without platforms gave placement %+v, want an empty one", bare.TaskTemplate.Placement)
	}
}

// TestAPlatformMemo (G-08-4): a nil memo remembers nothing and recalls nothing, a memo answers by repository and
// digest, an answer is copied both ways, and a full memo starts again instead of growing without bound.
func TestAPlatformMemo(t *testing.T) {
	t.Parallel()
	amd := []swarm.Platform{{OS: "linux", Architecture: "amd64"}}

	var none *PlatformMemo
	none.remember("env-a", "nginx:1.27", platformsDigestA, amd)
	if got, ok := none.recall("env-a", "nginx:1.27", platformsDigestA); ok || got != nil {
		t.Errorf("a nil memo recalled %v %v, want nothing", got, ok)
	}

	m := NewPlatformMemo()
	m.remember("env-a", "nginx:1.27", platformsDigestA, amd)
	got, ok := m.recall("env-a", "nginx:1.27", platformsDigestA)
	if !ok || !slices.Equal(got, amd) {
		t.Fatalf("recall = %v %v, want %v", got, ok, amd)
	}
	got[0].OS = "windows"
	if again, _ := m.recall("env-a", "nginx:1.27", platformsDigestA); again[0].OS != "linux" {
		t.Errorf("a caller changed the memo through the slice it was given: %v", again)
	}
	if _, ok := m.recall("env-a", "nginx:1.27", platformsDigestB); ok {
		t.Error("the memo answered for a digest it never saw")
	}
	m.remember("env-a", "nginx:1.27", "not-a-digest", amd)
	if _, ok := m.recall("env-a", "nginx:1.27", "not-a-digest"); ok {
		t.Error("the memo kept an answer for a key that is not a sha256 digest")
	}

	for i := 0; i < platformMemoLimit+5; i++ {
		m.remember("env-a", "nginx:1.27", fmt.Sprintf("sha256:%064d", i), amd)
	}
	m.mu.Lock()
	size := len(m.byKey)
	m.mu.Unlock()
	if size > platformMemoLimit {
		t.Errorf("memo holds %d digests, want at most %d", size, platformMemoLimit)
	}
}

// TestAPlatformMemoIsKeyedByRepositoryAndDigest (CR-01): a lookup of one repository never answers for another
// repository that names the same digest; the tag and the registry's short names do not make two keys for one
// repository; and a write that disagrees with what is held drops the entry, so the digest is looked up again.
func TestAPlatformMemoIsKeyedByRepositoryAndDigest(t *testing.T) {
	t.Parallel()
	amd := []swarm.Platform{{OS: "linux", Architecture: "amd64"}}
	odd := []swarm.Platform{{OS: "plan9", Architecture: "mips"}}

	t.Run("two repositories with one digest do not cross", func(t *testing.T) {
		t.Parallel()
		m := NewPlatformMemo()
		m.remember("env-a", "nginx:1.27-alpine", platformsDigestA, amd)
		m.remember("env-a", "evil.example/x:t", platformsDigestA, odd)
		if got, ok := m.recall("env-a", "nginx:1.27-alpine", platformsDigestA); !ok || !slices.Equal(got, amd) {
			t.Errorf("nginx recalled %v %v, want %v", got, ok, amd)
		}
		if got, ok := m.recall("env-a", "evil.example/x:t", platformsDigestA); !ok || !slices.Equal(got, odd) {
			t.Errorf("evil.example/x recalled %v %v, want %v", got, ok, odd)
		}
		if got, ok := m.recall("env-a", "other.example/nginx:1.27-alpine", platformsDigestA); ok {
			t.Errorf("a repository that was never looked up recalled %v", got)
		}
	})

	t.Run("one repository is one key whatever its tag or spelling", func(t *testing.T) {
		t.Parallel()
		m := NewPlatformMemo()
		m.remember("env-a", "nginx:1.27", platformsDigestA, amd)
		for _, text := range []string{"nginx", "docker.io/library/nginx:latest", "library/nginx@" + platformsDigestA, "nginx:1.27@" + platformsDigestA} {
			if got, ok := m.recall("env-a", text, platformsDigestA); !ok || !slices.Equal(got, amd) {
				t.Errorf("recall(%q) = %v %v, want %v", text, got, ok, amd)
			}
		}
	})

	t.Run("a conflicting write drops the entry", func(t *testing.T) {
		t.Parallel()
		m := NewPlatformMemo()
		m.remember("env-a", "nginx:1.27", platformsDigestA, amd)
		m.remember("env-a", "nginx:1.28", platformsDigestA, amd)
		if got, ok := m.recall("env-a", "nginx", platformsDigestA); !ok || !slices.Equal(got, amd) {
			t.Fatalf("an equal write changed the entry: %v %v", got, ok)
		}
		m.remember("env-a", "nginx:1.29", platformsDigestA, odd)
		if got, ok := m.recall("env-a", "nginx", platformsDigestA); ok {
			t.Errorf("a conflicting write left %v, want the entry dropped (a miss)", got)
		}
		m.remember("env-a", "nginx:1.30", platformsDigestA, amd)
		if got, ok := m.recall("env-a", "nginx", platformsDigestA); !ok || !slices.Equal(got, amd) {
			t.Errorf("after the drop a new answer is kept as the first one: %v %v", got, ok)
		}
	})

	t.Run("a text that is not a reference is not kept", func(t *testing.T) {
		t.Parallel()
		m := NewPlatformMemo()
		m.remember("env-a", "NOT A REF", platformsDigestA, amd)
		if got, ok := m.recall("env-a", "NOT A REF", platformsDigestA); ok {
			t.Errorf("recalled %v for a text that is not a reference", got)
		}
	})
}

// TestAPlatformMemoIsScopedToTheEnvironment (T-08-114): the memo is one for the whole server, so an answer one
// environment's lookup gave is never recalled for another environment, whose registry credential and mirror may differ;
// and a lookup with no environment keeps nothing.
func TestAPlatformMemoIsScopedToTheEnvironment(t *testing.T) {
	t.Parallel()
	amd := []swarm.Platform{{OS: "linux", Architecture: "amd64"}}
	odd := []swarm.Platform{{OS: "plan9", Architecture: "mips"}}
	m := NewPlatformMemo()
	m.remember("env-a", "nginx:1.27", platformsDigestA, amd)
	if got, ok := m.recall("env-b", "nginx:1.27", platformsDigestA); ok {
		t.Errorf("environment env-b recalled %v from env-a's lookup, want a miss", got)
	}
	m.remember("env-b", "nginx:1.27", platformsDigestA, odd)
	if got, ok := m.recall("env-a", "nginx:1.27", platformsDigestA); !ok || !slices.Equal(got, amd) {
		t.Errorf("env-a recalled %v %v after env-b's answer, want %v kept", got, ok, amd)
	}
	if got, ok := m.recall("env-b", "nginx:1.27", platformsDigestA); !ok || !slices.Equal(got, odd) {
		t.Errorf("env-b recalled %v %v, want %v", got, ok, odd)
	}
	m.remember("", "nginx:1.28", platformsDigestB, amd)
	if got, ok := m.recall("", "nginx:1.28", platformsDigestB); ok {
		t.Errorf("a lookup with no environment kept %v", got)
	}
}

// TestAPlatformMemoRemembersAnEmptyAnswer (WR-01): a digest whose lookup found no platform is held as that answer, which
// is not the same as a digest never looked up, and a later answer that does name platforms conflicts with it.
func TestAPlatformMemoRemembersAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	amd := []swarm.Platform{{OS: "linux", Architecture: "amd64"}}
	m := NewPlatformMemo()
	m.remember("env-a", "nginx:1.27", platformsDigestA, nil)
	got, ok := m.recall("env-a", "nginx:1.27", platformsDigestA)
	if !ok || len(got) != 0 {
		t.Errorf("recall = %v %v, want an empty answer that is held", got, ok)
	}
	if _, ok := m.recall("env-a", "nginx:1.27", platformsDigestB); ok {
		t.Error("the memo answered for a digest it never saw")
	}
	m.remember("env-a", "nginx:1.27", platformsDigestA, []swarm.Platform{})
	if _, ok := m.recall("env-a", "nginx:1.27", platformsDigestA); !ok {
		t.Error("an equal empty write dropped the entry")
	}
	m.remember("env-a", "nginx:1.27", platformsDigestA, amd)
	if got, ok := m.recall("env-a", "nginx:1.27", platformsDigestA); ok {
		t.Errorf("an answer that conflicts with the empty one left %v, want the entry dropped", got)
	}
}

// TestPlatformMemoRemember (12-41): the exported Remember is how an image update hands the platforms its check learned
// to the deploy. The same environment, repository and digest answer them; another environment or digest answers
// nothing; no platforms record nothing, so a single-manifest image keeps the deploy's own one daemon call.
func TestPlatformMemoRemember(t *testing.T) {
	t.Parallel()
	both := []swarm.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}

	m := NewPlatformMemo()
	m.Remember("env-a", "nginx:1.27", platformsDigestA, both)
	got, ok := m.recall("env-a", "nginx:1.27", platformsDigestA)
	if !ok || !slices.Equal(got, both) {
		t.Fatalf("recall = %v %v, want %v", got, ok, both)
	}
	if _, ok := m.recall("env-b", "nginx:1.27", platformsDigestA); ok {
		t.Error("the memo answered for another environment")
	}
	if _, ok := m.recall("env-a", "nginx:1.27", platformsDigestB); ok {
		t.Error("the memo answered for another digest")
	}

	m.Remember("env-a", "nginx:1.27", platformsDigestB, nil)
	m.Remember("env-a", "nginx:1.27", platformsDigestB, []swarm.Platform{})
	if got, ok := m.recall("env-a", "nginx:1.27", platformsDigestB); ok {
		t.Errorf("Remember with no platforms recorded %v, want nothing", got)
	}

	var none *PlatformMemo
	none.Remember("env-a", "nginx:1.27", platformsDigestA, both) // a nil memo is a no-op, not a panic
}
