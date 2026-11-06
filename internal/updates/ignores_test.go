// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stacks"
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

// TestIgnoreScopeNeverLeaks: an image ignore covers its repo:tag in its own environment, a workload ignore
// its own workload, and nothing is ignored anywhere else (T-12-110).
func TestIgnoreScopeNeverLeaks(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	s := updates.IgnoreStore{Q: r.st.Q, W: r.st.Write}
	at := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	for _, ig := range []updates.Ignore{
		{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeImage, Host: "docker.io", Repo: "library/nginx", Tag: "1.27", SetBy: "ann", SetAt: at},
		{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", SetBy: "ann", SetAt: at},
	} {
		if err := s.Add(ctx, ig); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.List(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.List(ctx, "env-b")
	if err != nil || len(b) != 0 {
		t.Fatalf("env-b lists %+v, %v, want nothing", b, err)
	}

	// The image ignore: both services of env-a on nginx:1.27 are covered.
	for _, name := range []string{"api", "worker"} {
		if m := a.Match(workloadOn(t, "env-a", name, "nginx:1.27"), digestD); m == nil || m.Scope != updates.ScopeImage {
			t.Errorf("%s on nginx:1.27 in env-a = %+v, want the image ignore", name, m)
		}
	}
	// Not another tag, another repository or another registry.
	for _, image := range []string{"nginx:1.28", "nginx:latest", "library/redis:1.27", "ghcr.io/library/nginx:1.27"} {
		if m := a.Match(workloadOn(t, "env-a", "api", image), digestD); m != nil {
			t.Errorf("api on %s = %+v, want none", image, m)
		}
	}
	// Not another environment: env-b has no rows and a service of env-a's name or image is not covered there.
	if m := b.Match(workloadOn(t, "env-b", "web", "nginx:1.27"), digestD); m != nil {
		t.Errorf("web on nginx:1.27 in env-b = %+v, want none", m)
	}
	// Even a list handed the wrong environment's workload does not match it.
	if m := a.Match(workloadOn(t, "env-b", "web", "nginx:1.27"), digestD); m != nil {
		t.Errorf("env-b's web matched env-a's rows: %+v", m)
	}
	// The workload ignore: web, not another service, and not a container of the same name.
	if m := a.Match(workloadOn(t, "env-a", "web", "redis:7"), digestD); m == nil || m.Scope != updates.ScopeWorkload {
		t.Errorf("web = %+v, want the workload ignore", m)
	}
	if m := a.Match(workloadOn(t, "env-a", "web2", "redis:7"), digestD); m != nil {
		t.Errorf("web2 = %+v, want none", m)
	}
	c := workloadOn(t, "env-a", "web", "redis:7")
	c.Kind = "container"
	if m := a.Match(c, digestD); m != nil {
		t.Errorf("the container web = %+v, want none", m)
	}
	// A reference with no tag is never matched by an image ignore.
	if m := a.Match(updates.Workload{EnvID: "env-a", Kind: "service", Name: "x"}, digestD); m != nil {
		t.Errorf("a workload with no image = %+v, want none", m)
	}
}

// TestSkipLiftsOnNewerDigest: a skip passes over the digest it holds and stops applying when the tag moves
// on.
func TestSkipLiftsOnNewerDigest(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := updates.IgnoreStore{Q: r.st.Q, W: r.st.Write}
	at := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	if err := s.Add(ctx, updates.Ignore{EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: digestD, SetBy: "ann", SetAt: at}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	web := workloadOn(t, "env-a", "web", "nginx:1.27")
	if m := list.Match(web, digestD); m == nil || m.Mode != updates.ModeSkip {
		t.Errorf("remote D = %+v, want the skip", m)
	}
	if m := list.Match(web, digestE); m != nil {
		t.Errorf("remote E = %+v, want none: the skip lifts when the remote digest changes", m)
	}
	if m := list.Match(web, ""); m != nil {
		t.Errorf("no remote digest = %+v, want none", m)
	}
	if m := list.Match(workloadOn(t, "env-a", "api", "nginx:1.27"), digestD); m != nil {
		t.Errorf("another service at remote D = %+v, want none: a skip is one workload's", m)
	}

	// Skipping the newer version replaces the earlier skip and does not add a second row.
	if err := s.Add(ctx, updates.Ignore{EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: digestE, SetBy: "bob", SetAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, "env-a")
	if len(list) != 1 || list[0].Digest != digestE || list[0].SetBy != "bob" {
		t.Fatalf("List = %+v, want one skip at E set by bob", list)
	}
	if m := list.Match(web, digestD); m != nil {
		t.Errorf("remote D after skipping E = %+v, want none", m)
	}
}

// TestLabelIgnoreWins: tierplan.updates=ignore on the workload hides its updates before any stored row is
// read, and only that exact value does.
func TestLabelIgnoreWins(t *testing.T) {
	t.Parallel()
	web := workloadOn(t, "env-a", "web", "nginx:1.27")
	web.Labels = map[string]string{stacks.LabelUpdates: "ignore"}
	skip := updates.Ignores{{ID: "1", EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: digestD}}

	m := skip.Match(web, digestD)
	if m == nil || m.Mode != updates.ModeIgnore || m.Scope != updates.ScopeLabel || !m.ByLabel {
		t.Fatalf("a labelled workload = %+v, want the label ignore ahead of the stored skip", m)
	}
	if m := (updates.Ignores)(nil).Match(web, ""); m == nil || !m.ByLabel {
		t.Errorf("a labelled workload with no rows = %+v, want the label ignore", m)
	}
	// It covers only the workload that carries it.
	if m := (updates.Ignores)(nil).Match(workloadOn(t, "env-a", "api", "nginx:1.27"), ""); m != nil {
		t.Errorf("a workload with no label = %+v, want none", m)
	}
	for _, v := range []string{"other", "true", "IGNORE", ""} {
		web.Labels = map[string]string{stacks.LabelUpdates: v}
		if m := (updates.Ignores)(nil).Match(web, ""); m != nil {
			t.Errorf("tierplan.updates=%q = %+v, want no label ignore", v, m)
		}
	}
	web.Labels = map[string]string{stacks.LabelUpdates: "other"}
	if m := skip.Match(web, digestD); m == nil || m.Mode != updates.ModeSkip {
		t.Errorf("a non-ignore label leaves the stored skip in force, got %+v", m)
	}
}
