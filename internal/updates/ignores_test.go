// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Skips and ignores (plan 12-39, D-15, D-16): where they are kept and what each one covers.

const (
	digestD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	digestE = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// workloadOn is a service of env on image (nginx:1.27 and the like).
func workloadOn(t *testing.T, env, name, image string) updates.Workload {
	t.Helper()
	ref, err := registry.ParseRef(image)
	if err != nil {
		t.Fatal(err)
	}
	return updates.Workload{EnvID: env, Kind: "service", ID: "id-" + name, Name: name, Written: image, Ref: ref}
}

func TestIgnoreStoreRoundTripAndMatch(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := updates.IgnoreStore{Q: r.st.Q, W: r.st.Write}
	at := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

	skip := updates.Ignore{EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: digestD, SetBy: "ann", SetAt: at}
	img := updates.Ignore{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeImage, Host: "docker.io", Repo: "library/nginx", Tag: "1.27", SetBy: "bob", SetAt: at.Add(time.Minute)}
	for _, ig := range []updates.Ignore{skip, img} {
		if err := s.Add(ctx, ig); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(ctx, "env-a")
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v, want the two rows", list, err)
	}
	if list[0].SetBy != "ann" || !list[0].SetAt.Equal(at) || list[0].ID == "" || list[1].SetBy != "bob" || !list[1].SetAt.Equal(at.Add(time.Minute)) {
		t.Errorf("List = %+v, want who set each and when", list)
	}

	web := workloadOn(t, "env-a", "web", "nginx:1.27")
	if m := list.Match(web, digestD); m == nil || m.Mode != updates.ModeIgnore || m.Scope != updates.ScopeImage {
		t.Errorf("web on the ignored image = %+v, want the image ignore first", m)
	}
	// A skip alone, on another image.
	api := workloadOn(t, "env-a", "web", "nginx:1.26")
	if m := list.Match(api, digestD); m == nil || m.Mode != updates.ModeSkip || m.Digest != digestD {
		t.Errorf("web at the skipped digest = %+v, want the skip", m)
	}
	other := workloadOn(t, "env-a", "api", "nginx:1.27")
	if m := list.Match(other, digestE); m == nil || m.Scope != updates.ScopeImage {
		t.Errorf("another service on nginx:1.27 = %+v, want the image ignore", m)
	}
	later := workloadOn(t, "env-a", "api", "nginx:1.28")
	if m := list.Match(later, digestD); m != nil {
		t.Errorf("a service on nginx:1.28 = %+v, want none", m)
	}

	if ok, err := s.Remove(ctx, "env-a", skip); err != nil || !ok {
		t.Errorf("Remove = %v, %v, want true", ok, err)
	}
	if ok, err := s.Remove(ctx, "env-a", skip); err != nil || ok {
		t.Errorf("second Remove = %v, %v, want false", ok, err)
	}
}
