// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestKeepalivedNotOfferedForUpdate pins D-14 and A-13: the virtual IP's keepalived image follows the server
// like the proxy's, so the update detector never lists the keepalived service or its task containers, and the
// update path refuses the service with the ingress refusal and never finds the containers. The detector's exemption is the keepalived component
// label alone (HasComponentLabel, widened); the refusal on a service row needs the flag, which needs the label
// and the reserved name.
func TestKeepalivedNotOfferedForUpdate(t *testing.T) {
	t.Parallel()

	image := "ghcr.io/ianglennon/tierplan-keepalived:0.1.0"
	ka := dockerx.ServiceRow{ID: "svc-ka", Name: ingress.ServiceNameKeepalived, Mode: "global", Image: image, Labels: ingress.KeepalivedLabels(), Ingress: true}
	// The label with no flag and another name: skipped by the detector, which fails safe on the label alone.
	forged := dockerx.ServiceRow{ID: "svc-forged", Name: "web", Mode: "replicated", Image: "registry.test/team/web:1", Labels: ingress.KeepalivedLabels()}
	plain := dockerx.ServiceRow{ID: "svc-plain", Name: "api", Mode: "replicated", Image: "registry.test/team/api:1", Labels: map[string]string{"team": "core"}}

	t.Run("detector skips the service row", func(t *testing.T) {
		t.Parallel()
		got := updates.ServiceWorkloads([]dockerx.ServiceRow{ka, forged, plain})
		if len(got) != 1 || got[0].Name != "api" {
			t.Fatalf("workloads = %+v, want only api: keepalived and anything carrying the component label are skipped", got)
		}
		unflagged := ka
		unflagged.Ingress = false
		if got := updates.ServiceWorkloads([]dockerx.ServiceRow{unflagged, plain}); len(got) != 1 || got[0].Name != "api" {
			t.Errorf("workloads without the flag = %+v, want only api: the label alone skips it", got)
		}
	})

	t.Run("detector skips a task container", func(t *testing.T) {
		t.Parallel()
		mk := func(id string, labels map[string]string) envs.ContainerRow {
			return envs.ContainerRow{ContainerRow: dockerx.ContainerRow{ID: id, Name: "/" + id, State: "running", Image: "registry.test/team/" + id + ":1", Labels: labels}, NodeID: "n1", Node: "node-one"}
		}
		agg := envs.Aggregate{Containers: []envs.ContainerRow{
			mk("keepalived", ingress.KeepalivedLabels()), mk("plain", map[string]string{"team": "core"}),
		}}
		got := updates.ContainerWorkloads(agg, func(string) (string, bool) { return "", false })
		if len(got) != 1 || got[0].ID != "plain" {
			t.Fatalf("container workloads = %+v, want only plain", got)
		}
		if ids := updates.ImageTextWanted(envs.Aggregate{Containers: []envs.ContainerRow{mk("sha256:0123456789ab", ingress.KeepalivedLabels())}}); len(ids) != 0 {
			t.Errorf("image text wanted for %v: a keepalived container is never a workload", ids)
		}
	})

	t.Run("the refusal needs the flag", func(t *testing.T) {
		t.Parallel()
		if code := updates.NotUpdatableCode(ka); code != updates.NotUpdatableIngress {
			t.Errorf("keepalived: code %q, want %q", code, updates.NotUpdatableIngress)
		}
		for _, r := range []dockerx.ServiceRow{forged, plain} {
			if code := updates.NotUpdatableCode(r); code != "" {
				t.Errorf("%s: code %q, want none: a label on another name is not keepalived", r.Name, code)
			}
		}
	})

	t.Run("the update path refuses a service and writes nothing", func(t *testing.T) {
		t.Parallel()
		spec := swarmSpec(ingress.ServiceNameKeepalived, 'a')
		spec.Labels = ingress.KeepalivedLabels()
		svc := dockertest.FakeService{
			ID: "svc-" + ingress.ServiceNameKeepalived, Name: ingress.ServiceNameKeepalived, Mode: "global", Running: 3, Desired: 3, Version: 1,
			Image: spec.TaskTemplate.ContainerSpec.Image, Labels: spec.Labels, Spec: &spec,
		}
		r := newApplyRig(t, nil, svc, swarmService("web", 'a'))
		r.heads.set("registry.test/team/"+ingress.ServiceNameKeepalived+":1", answer{digest: dg('b')})
		r.offer("web", dg('b'))

		_, err := r.applier.Start(testsupport.Context(t), r.request(ingress.ServiceNameKeepalived, ""))
		var nu *updates.NotUpdatableError
		if !errors.As(err, &nu) || nu.Code != updates.NotUpdatableIngress {
			t.Fatalf("Start(keepalived) = %v, want not updatable: %s", err, updates.NotUpdatableIngress)
		}
		p, err := r.applier.Preview(testsupport.Context(t), r.envID, "service", ingress.ServiceNameKeepalived)
		if err != nil || p.Refusal != updates.NotUpdatableIngress {
			t.Errorf("Preview(keepalived) = %+v, %v, want the refusal %s", p, err, updates.NotUpdatableIngress)
		}
		if n := len(r.fake.ServiceUpdates()); n != 0 {
			t.Errorf("service updates = %d after the refusal, want none", n)
		}
		if _, held := r.locks.Held(r.envID); held {
			t.Errorf("the lock is held after the refusal")
		}
	})

	// The apply path finds a container among the workloads (ContainerWorkloads), which leave out anything carrying
	// a component label, so a keepalived task container is never found: Start answers ErrWorkloadGone, as it does
	// for the proxy's, and nothing is written. containerUpdatable's own NotUpdatableIngress arm reads the same
	// label (HasComponentLabel, widened) and stands behind that as a second line.
	t.Run("the container path never finds a task container and writes nothing", func(t *testing.T) {
		t.Parallel()
		task := appContainer("running")
		task.ID = "keepalived" + strings.Repeat("0", 64-len("keepalived"))
		task.Names = []string{"/" + ingress.ServiceNameKeepalived + ".node1.task1"}
		task.Labels = ingress.KeepalivedLabels()
		task.Labels["com.docker.swarm.service.id"] = "svc-ka"
		r := containerRig(t, nil, task)
		r.offer("app", dg('b'))

		_, err := r.applier.Start(testsupport.Context(t), r.containerRequest(strings.TrimPrefix(task.Names[0], "/"), ""))
		if !errors.Is(err, updates.ErrWorkloadGone) {
			t.Fatalf("Start(keepalived task) = %v, want ErrWorkloadGone: the detector never lists it", err)
		}
		if got := writeKinds(r.fake.Writes()); len(got) != 0 {
			t.Errorf("the engine was asked %v after the refusal, want nothing", got)
		}
		if _, held := r.locks.Held(r.envID); held {
			t.Errorf("the lock is held after the refusal")
		}
	})
}
