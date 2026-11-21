// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestUpdatesSkipIngress pins D-28 and D-41: the proxy's image follows the server (the ensure loop updates it,
// D-11), so the update detector never lists the proxy and the update path refuses it. The detector's exemption
// is the component label alone, because skipping an update fails safe; the refusal on the row needs the flag,
// which needs the label and the reserved name.
func TestUpdatesSkipIngress(t *testing.T) {
	t.Parallel()

	image := "ghcr.io/ianglennon/tierplan-ingress:0.1.0"
	proxy := dockerx.ServiceRow{ID: "svc-proxy", Name: ingress.ServiceName, Mode: "global", Image: image, Labels: ingress.ObjectLabels(), Ingress: true}
	// The label on a service of another name: not the proxy for a refusal, but still skipped by the detector.
	forged := dockerx.ServiceRow{ID: "svc-forged", Name: "web", Mode: "replicated", Image: "registry.test/team/web:1", Labels: ingress.ObjectLabels()}
	plain := dockerx.ServiceRow{ID: "svc-plain", Name: "api", Mode: "replicated", Image: "registry.test/team/api:1", Labels: map[string]string{"team": "core"}}

	t.Run("detector skips a service row", func(t *testing.T) {
		t.Parallel()
		got := updates.ServiceWorkloads([]dockerx.ServiceRow{proxy, forged, plain})
		if len(got) != 1 || got[0].Name != "api" {
			names := make([]string, 0, len(got))
			for _, w := range got {
				names = append(names, w.Name)
			}
			t.Fatalf("workloads = %v, want only api: the proxy and anything carrying the component label are skipped", names)
		}
	})

	t.Run("detector skips a container", func(t *testing.T) {
		t.Parallel()
		mk := func(id string, labels map[string]string) envs.ContainerRow {
			return envs.ContainerRow{ContainerRow: dockerx.ContainerRow{ID: id, Name: "/" + id, State: "running", Image: "registry.test/team/" + id + ":1", Labels: labels}, NodeID: "n1", Node: "node-one"}
		}
		agg := envs.Aggregate{Containers: []envs.ContainerRow{
			mk("proxy", ingress.ObjectLabels()), mk("plain", map[string]string{"team": "core"}),
		}}
		got := updates.ContainerWorkloads(agg, func(string) (string, bool) { return "", false })
		if len(got) != 1 || got[0].ID != "plain" {
			t.Fatalf("container workloads = %+v, want only plain", got)
		}
		if ids := updates.ImageTextWanted(envs.Aggregate{Containers: []envs.ContainerRow{mk("sha256:0123456789ab", ingress.ObjectLabels())}}); len(ids) != 0 {
			t.Errorf("image text wanted for %v: the proxy's container is never a workload", ids)
		}
	})

	t.Run("the refusal needs the flag", func(t *testing.T) {
		t.Parallel()
		if code := updates.NotUpdatableCode(proxy); code != updates.NotUpdatableIngress {
			t.Errorf("proxy: code %q, want %q", code, updates.NotUpdatableIngress)
		}
		if updates.NotUpdatableIngress != "ingress" {
			t.Errorf("NotUpdatableIngress = %q, want ingress", updates.NotUpdatableIngress)
		}
		for _, r := range []dockerx.ServiceRow{forged, plain} {
			if code := updates.NotUpdatableCode(r); code != "" {
				t.Errorf("%s: code %q, want none: a label on another name is not the proxy", r.Name, code)
			}
		}
	})

	t.Run("the update path refuses the proxy and writes nothing", func(t *testing.T) {
		t.Parallel()
		spec := swarmSpec(ingress.ServiceName, 'a')
		spec.Labels = ingress.ObjectLabels()
		svc := dockertest.FakeService{
			ID: "svc-" + ingress.ServiceName, Name: ingress.ServiceName, Mode: "global", Running: 3, Desired: 3, Version: 1,
			Image: spec.TaskTemplate.ContainerSpec.Image, Labels: spec.Labels, Spec: &spec,
		}
		r := newApplyRig(t, nil, svc, swarmService("web", 'a'))
		r.heads.set("registry.test/team/"+ingress.ServiceName+":1", answer{digest: dg('b')})
		r.offer("web", dg('b'))

		_, err := r.applier.Start(testsupport.Context(t), r.request(ingress.ServiceName, ""))
		var nu *updates.NotUpdatableError
		if !errors.As(err, &nu) || nu.Code != updates.NotUpdatableIngress {
			t.Fatalf("Start(proxy) = %v, want not updatable: %s", err, updates.NotUpdatableIngress)
		}
		p, err := r.applier.Preview(testsupport.Context(t), r.envID, "service", ingress.ServiceName)
		if err != nil || p.Refusal != updates.NotUpdatableIngress {
			t.Errorf("Preview(proxy) = %+v, %v, want the refusal %s", p, err, updates.NotUpdatableIngress)
		}
		if n := len(r.fake.ServiceUpdates()); n != 0 {
			t.Errorf("service updates = %d after the refusal, want none", n)
		}
		if _, held := r.locks.Held(r.envID); held {
			t.Errorf("the lock is held after the refusal")
		}
	})
}
