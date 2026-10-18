// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

func serviceFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "web-id", Name: "web", Mode: "replicated", Running: 2, Desired: 2, Image: "registry.example.com/web:1", Version: 7,
				Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "agent-id", Name: "bayplan-agent", Mode: "global", Running: 3, Desired: 3},
			{ID: "job-id", Name: "migrate", Mode: "replicated-job"},
		},
	})
	return newClient(t, fake), fake
}

func TestServiceInspectAnswersTheTypedDetailAndTheRawDocument(t *testing.T) {
	c, _ := serviceFake(t)
	d, raw, err := c.ServiceInspect(context.Background(), "web")
	if err != nil {
		t.Fatalf("ServiceInspect: %v", err)
	}
	if d.ID != "web-id" || d.Name != "web" || d.Mode != "replicated" || d.Stack != "shop" || d.Image != "registry.example.com/web:1" ||
		d.Version != 7 || d.Replicas != 2 || d.Agent || d.PreviousSpec != nil || d.UpdateStatus != nil {
		t.Errorf("detail = %+v", d)
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(d.Spec, &spec); err != nil || spec.Name != "web" {
		t.Errorf("spec = %s (%v)", d.Spec, err)
	}
	if !strings.Contains(string(raw), `"web-id"`) {
		t.Errorf("raw = %s, want the inspect document", raw)
	}
	if a, _, err := c.ServiceInspect(context.Background(), "agent-id"); err != nil || !a.Agent {
		t.Errorf("agent service = %+v, %v; want Agent true", a, err)
	}
}

func TestServiceInspectOfAnUnknownServiceIsNotFound(t *testing.T) {
	c, _ := serviceFake(t)
	_, _, err := c.ServiceInspect(context.Background(), "nope")
	if !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
}

func TestServiceScaleSendsOneVersionedUpdateWithTheNewReplicasOnly(t *testing.T) {
	c, fake := serviceFake(t)
	prev, changed, err := c.ServiceScale(context.Background(), "web-id", 5, "Zm9v")
	if err != nil || prev != 2 || !changed {
		t.Fatalf("ServiceScale = %d, %v, %v; want 2, true", prev, changed, err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.ServiceID != "web-id" || u.Version != "7" || u.Auth != "Zm9v" {
		t.Errorf("update = %+v, want version 7 and the credential", u)
	}
	if got := u.Query.Get("registryAuthFrom") + u.Query.Get("rollback"); got != "" {
		t.Errorf("query = %v, want only the version", u.Query)
	}
	if r := u.Spec.Mode.Replicated; r == nil || r.Replicas == nil || *r.Replicas != 5 {
		t.Errorf("replicas = %+v, want 5", u.Spec.Mode.Replicated)
	}
	if u.Spec.Name != "web" || u.Spec.Labels["com.docker.stack.namespace"] != "shop" || u.Spec.TaskTemplate.ContainerSpec == nil ||
		u.Spec.TaskTemplate.ContainerSpec.Image != "registry.example.com/web:1" {
		t.Errorf("the rest of the spec changed: %+v", u.Spec)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /services/web-id/update?version=7"}) {
		t.Errorf("writes = %v", got)
	}
}

func TestServiceScaleToTheCountItHasWritesNothing(t *testing.T) {
	c, fake := serviceFake(t)
	prev, changed, err := c.ServiceScale(context.Background(), "web", 2, "")
	if err != nil || prev != 2 || changed {
		t.Fatalf("ServiceScale = %d, %v, %v; want 2, false", prev, changed, err)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceScaleRefusesGlobalAndJobServices(t *testing.T) {
	c, fake := serviceFake(t)
	for _, id := range []string{"agent-id", "job-id"} {
		if _, _, err := c.ServiceScale(context.Background(), id, 2, ""); !errors.Is(err, dockerx.ErrNotScalable) {
			t.Errorf("ServiceScale(%s) = %v, want ErrNotScalable", id, err)
		}
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceScaleMapsTheOutOfSequenceAnswerAndSendsNoSecondUpdate(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Version: 7}},
		WriteDelay:  50 * time.Millisecond,
	})
	c := newClient(t, fake)
	done := make(chan error, 1)
	go func() {
		_, _, err := c.ServiceScale(context.Background(), "web-id", 4, "")
		done <- err
	}()
	// The update is on its way (the fake holds it for WriteDelay); somebody else changes
	// the service now, so the version the update carries is no longer current.
	if err := fake.Await(testsupport.Context(t), func() bool { return len(fake.Writes()) > 0 }); err != nil {
		t.Fatalf("the update never reached the daemon: %v", err)
	}
	fake.BumpService("web-id")
	err := <-done
	if !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("err = %v, want ErrServiceChanged", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: a refused write is never retried", got)
	}
	if got := len(fake.ServiceUpdates()); got != 0 {
		t.Errorf("accepted updates = %d, want none", got)
	}
}

// ResolveImage, ServiceForceUpdate and ServiceRollback (plan 06-08, D-08, D-11).

func TestResolveImageAsksForTheTagAndPinsItToTheAnswer(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	res, err := c.ResolveImage(context.Background(), "nginx:1.27@"+oldDigest, "Zm9v")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	want := dockertest.FakeDigestOf("docker.io/library/nginx:1.27")
	if res.Tag != "1.27" || res.Digest != want || res.Ref != "nginx:1.27@"+want {
		t.Errorf("resolved = %+v, want nginx:1.27@%s", res, want)
	}
	if want := []string{"linux/amd64", "linux/arm64"}; !slices.Equal(res.Platforms, want) {
		t.Errorf("platforms = %q, want %q: attestations dropped, variants dropped", res.Platforms, want)
	}
	if got := fake.DistributionRefs(); !slices.Equal(got, []string{"docker.io/library/nginx:1.27"}) {
		t.Errorf("registry was asked for %q, want the tag without the old digest", got)
	}
	if got := fake.DistributionAuth(); !slices.Equal(got, []string{"Zm9v"}) {
		t.Errorf("auth = %q, want the credential sent", got)
	}
}

func TestResolveImagePicksUpATagThatMoved(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	first, err := c.ResolveImage(context.Background(), "registry.example.com/web:1", "")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	moved := "sha256:" + strings.Repeat("b", 64)
	fake.SetDistributionDigest("registry.example.com/web:1", moved)
	second, err := c.ResolveImage(context.Background(), "registry.example.com/web:1@"+first.Digest, "")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if second.Digest != moved || second.Ref != "registry.example.com/web:1@"+moved {
		t.Errorf("second = %+v, want the moved digest %s", second, moved)
	}
}

func TestResolveImageWithNoTagResolvesLatest(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	res, err := c.ResolveImage(context.Background(), "nginx", "")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if res.Tag != "latest" || !strings.HasPrefix(res.Ref, "nginx:latest@sha256:") {
		t.Errorf("resolved = %+v, want nginx:latest@sha256:...", res)
	}
	if got := fake.DistributionRefs(); !slices.Equal(got, []string{"docker.io/library/nginx:latest"}) {
		t.Errorf("registry was asked for %q", got)
	}
}

func TestResolveImageOfADigestOnlyReferenceIsRefusedWithoutAsking(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	_, err := c.ResolveImage(context.Background(), "nginx@sha256:"+strings.Repeat("c", 64), "")
	if !errors.Is(err, dockerx.ErrDigestOnly) {
		t.Fatalf("err = %v, want ErrDigestOnly", err)
	}
	if got := fake.DistributionRefs(); len(got) != 0 {
		t.Errorf("registry was asked %q, want nothing", got)
	}
}

func TestResolveImageFailuresAreClassifiedAsAPullIsAndNeverFallBack(t *testing.T) {
	c := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 401, Message: "unauthorized: authentication required"}}))
	res, err := c.ResolveImage(context.Background(), "ghcr.io/acme/app:1", "c2VjcmV0LXRva2Vu")
	if err == nil {
		t.Fatalf("ResolveImage = %+v, want an error", res)
	}
	if res.Ref != "" {
		t.Errorf("resolved = %+v, want nothing: no fallback to the unpinned tag", res)
	}
	if got := dockerx.PullFailure(err); got != dockerx.PullCodeUnauthorized {
		t.Errorf("PullFailure(%v) = %q, want unauthorized", err, got)
	}
	if strings.Contains(err.Error(), "c2VjcmV0LXRva2Vu") {
		t.Errorf("error %q carries the credential", err)
	}
}

func TestResolveImageRefusesAnInvalidReference(t *testing.T) {
	c := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{}))
	if _, err := c.ResolveImage(context.Background(), "NOT A REF", ""); err == nil {
		t.Fatal("ResolveImage of an invalid reference succeeded")
	}
}

func updatableFake(t *testing.T, row dockertest.FakeService) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, ServiceRows: []dockertest.FakeService{row},
	})
	return newClient(t, fake), fake
}

func TestServiceForceUpdateBumpsTheCounterPinsTheImageAndChangesNothingElse(t *testing.T) {
	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "web", Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "registry.example.com/web:1", Env: []string{"A=1"}}, ForceUpdate: 3},
	}
	c, fake := updatableFake(t, dockertest.FakeService{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Version: 7, Spec: &spec})
	pinned := "registry.example.com/web:1@sha256:" + strings.Repeat("d", 64)
	err := c.ServiceForceUpdate(context.Background(), "web-id", dockerx.ForceUpdateOptions{
		Version: 7, Image: pinned, Platforms: []string{"linux/amd64", "linux/"}, EncodedAuth: "Zm9v",
	})
	if err != nil {
		t.Fatalf("ServiceForceUpdate: %v", err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.Version != "7" || u.Auth != "Zm9v" || u.Query.Get("rollback") != "" || u.Query.Get("registryAuthFrom") != "" {
		t.Errorf("update = %+v, want version 7, the credential and no rollback", u)
	}
	if got := u.Spec.TaskTemplate.ForceUpdate; got != 4 {
		t.Errorf("ForceUpdate = %d, want 4", got)
	}
	cs := u.Spec.TaskTemplate.ContainerSpec
	if cs == nil || cs.Image != pinned || !slices.Equal(cs.Env, []string{"A=1"}) {
		t.Errorf("container spec = %+v, want the pinned image and the rest as it was", cs)
	}
	pl := u.Spec.TaskTemplate.Placement
	if pl == nil || len(pl.Platforms) != 2 || pl.Platforms[0] != (swarm.Platform{OS: "linux", Architecture: "amd64"}) || pl.Platforms[1] != (swarm.Platform{OS: "linux"}) {
		t.Errorf("placement = %+v, want both platforms", pl)
	}
	if u.Spec.Name != "web" || u.Spec.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("the rest of the spec changed: %+v", u.Spec)
	}
	if got := fake.DistributionRefs(); len(got) != 0 {
		t.Errorf("the update asked the registry %q itself; Bayplan resolves the digest, not the engine", got)
	}
}

func TestServiceForceUpdateWithNoImageOnlyBumpsTheCounter(t *testing.T) {
	c, fake := updatableFake(t, dockertest.FakeService{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Image: "web@sha256:" + strings.Repeat("e", 64), Version: 4})
	if err := c.ServiceForceUpdate(context.Background(), "web", dockerx.ForceUpdateOptions{Version: 4}); err != nil {
		t.Fatalf("ServiceForceUpdate: %v", err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.Spec.TaskTemplate.ForceUpdate != 1 || u.Spec.TaskTemplate.ContainerSpec.Image != "web@sha256:"+strings.Repeat("e", 64) || u.Spec.TaskTemplate.Placement != nil {
		t.Errorf("spec = %+v, want ForceUpdate 1 and the image and placement untouched", u.Spec.TaskTemplate)
	}
}

func TestServiceForceUpdateRepeatedReplacesEveryTaskAgain(t *testing.T) {
	c, fake := updatableFake(t, dockertest.FakeService{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Image: "web:1", Version: 4})
	for i, version := range []uint64{4, 5} {
		if err := c.ServiceForceUpdate(context.Background(), "web-id", dockerx.ForceUpdateOptions{Version: version}); err != nil {
			t.Fatalf("force update %d: %v", i+1, err)
		}
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 2 || ups[0].Spec.TaskTemplate.ForceUpdate != 1 || ups[1].Spec.TaskTemplate.ForceUpdate != 2 {
		t.Errorf("updates = %+v, want ForceUpdate 1 then 2", ups)
	}
}

func TestServiceForceUpdateWithAStaleVersionWritesNothing(t *testing.T) {
	c, fake := updatableFake(t, dockertest.FakeService{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Image: "web:1", Version: 7})
	err := c.ServiceForceUpdate(context.Background(), "web-id", dockerx.ForceUpdateOptions{Version: 6})
	if !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("err = %v, want ErrServiceChanged", err)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceForceUpdateOfAnUnknownServiceIsNotFound(t *testing.T) {
	c, _ := updatableFake(t, dockertest.FakeService{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Version: 7})
	if err := c.ServiceForceUpdate(context.Background(), "nope", dockerx.ForceUpdateOptions{Version: 7}); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
}

func rollbackRow(extra func(*dockertest.FakeService)) dockertest.FakeService {
	current := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "web"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "web:2"}},
	}
	previous := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "web"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "web:1"}},
	}
	row := dockertest.FakeService{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Version: 9, Spec: &current, PreviousSpec: &previous}
	if extra != nil {
		extra(&row)
	}
	return row
}

func TestServiceRollbackSendsTheCurrentSpecWithRollbackPreviousAndTheVersion(t *testing.T) {
	c, fake := updatableFake(t, rollbackRow(nil))
	if err := c.ServiceRollback(context.Background(), "web-id", 9, "Zm9v"); err != nil {
		t.Fatalf("ServiceRollback: %v", err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.Version != "9" || u.Query.Get("rollback") != "previous" || u.Auth != "Zm9v" || u.Query.Get("registryAuthFrom") != "" {
		t.Errorf("update = %+v, want version 9, rollback=previous and the credential", u)
	}
	if cs := u.Spec.TaskTemplate.ContainerSpec; cs == nil || cs.Image != "web:2" {
		t.Errorf("body spec = %+v, want the current spec", u.Spec)
	}
	_, raw, err := c.ServiceInspect(context.Background(), "web-id")
	if err != nil {
		t.Fatalf("ServiceInspect: %v", err)
	}
	var doc struct {
		Spec         swarm.ServiceSpec
		PreviousSpec swarm.ServiceSpec
		UpdateStatus struct{ State, Message string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if doc.Spec.TaskTemplate.ContainerSpec.Image != "web:1" || doc.PreviousSpec.TaskTemplate.ContainerSpec.Image != "web:2" ||
		doc.UpdateStatus.State != "rollback_started" || doc.UpdateStatus.Message != "manually requested rollback" {
		t.Errorf("after rollback = %+v, want the specs swapped and rollback_started", doc)
	}
}

func TestServiceRollbackWithoutACredentialTakesItFromThePreviousSpec(t *testing.T) {
	c, fake := updatableFake(t, rollbackRow(nil))
	if err := c.ServiceRollback(context.Background(), "web-id", 9, ""); err != nil {
		t.Fatalf("ServiceRollback: %v", err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.Auth != "" || u.Query.Get("registryAuthFrom") != "previous-spec" {
		t.Errorf("update = %+v, want registryAuthFrom=previous-spec and no header", u)
	}
}

func TestServiceRollbackRepeatedTogglesBackToTheSpecJustReplaced(t *testing.T) {
	c, _ := updatableFake(t, rollbackRow(nil))
	for i, step := range []struct {
		version uint64
		image   string
	}{{9, "web:1"}, {10, "web:2"}} {
		if err := c.ServiceRollback(context.Background(), "web-id", step.version, ""); err != nil {
			t.Fatalf("rollback %d: %v", i+1, err)
		}
		d, _, err := c.ServiceInspect(context.Background(), "web-id")
		if err != nil {
			t.Fatalf("ServiceInspect: %v", err)
		}
		if d.Image != step.image {
			t.Errorf("image after rollback %d = %q, want %s", i+1, d.Image, step.image)
		}
	}
}

func TestServiceRollbackWithoutAPreviousSpecWritesNothing(t *testing.T) {
	c, fake := updatableFake(t, rollbackRow(func(r *dockertest.FakeService) { r.PreviousSpec = nil }))
	if err := c.ServiceRollback(context.Background(), "web-id", 9, ""); !errors.Is(err, dockerx.ErrNoPreviousSpec) {
		t.Fatalf("err = %v, want ErrNoPreviousSpec", err)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceRollbackWithAStaleVersionWritesNothing(t *testing.T) {
	c, fake := updatableFake(t, rollbackRow(nil))
	if err := c.ServiceRollback(context.Background(), "web-id", 8, ""); !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("err = %v, want ErrServiceChanged", err)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceRollbackRefusedByTheManagerKeepsItsWords(t *testing.T) {
	c, fake := updatableFake(t, rollbackRow(func(r *dockertest.FakeService) { r.RollbackError = "secret not found: db_password" }))
	err := c.ServiceRollback(context.Background(), "web-id", 9, "")
	var refused *dockerx.RollbackRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Message, "secret not found: db_password") {
		t.Fatalf("err = %v, want a RollbackRefusedError with the manager's words", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: never retried", got)
	}
}

func TestServiceRemoveRemovesOneServiceOnceAndNothingElse(t *testing.T) {
	c, fake := serviceFake(t)
	if err := c.ServiceRemove(context.Background(), "web-id"); err != nil {
		t.Fatalf("ServiceRemove: %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"DELETE /services/web-id"}) {
		t.Errorf("writes = %v, want exactly one DELETE of the service", got)
	}
	if _, _, err := c.ServiceInspect(context.Background(), "web-id"); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Errorf("inspect after the remove: err = %v, want ErrServiceNotFound", err)
	}
	if _, _, err := c.ServiceInspect(context.Background(), "agent-id"); err != nil {
		t.Errorf("another service went with it: %v", err)
	}
}

func TestServiceRemoveOfAnUnknownServiceIsNotFound(t *testing.T) {
	c, _ := serviceFake(t)
	if err := c.ServiceRemove(context.Background(), "nope"); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
}

// ServiceUpdate (plan 06-18, RES-03, D-03, D-08).

const editedSpec = `{"Name":"web","Labels":{"com.docker.stack.namespace":"shop"},"TaskTemplate":{"ContainerSpec":{"Image":"registry.example.com/web:2@sha256:abc","Env":["A=1"]}},"Mode":{"Replicated":{"Replicas":4}}}`

func TestServiceUpdateSendsTheWholeSpecOnceWithTheVersionTheCredentialAndNoRegistryQuery(t *testing.T) {
	c, fake := serviceFake(t)
	warnings, err := c.ServiceUpdate(context.Background(), "web-id", 7, json.RawMessage(editedSpec), "Zm9v")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("ServiceUpdate = %v, %v", warnings, err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.ServiceID != "web-id" || u.Version != "7" || u.Auth != "Zm9v" {
		t.Errorf("update = %+v, want version 7 and the credential", u)
	}
	if got := u.Query.Get("registryAuthFrom") + u.Query.Get("rollback"); got != "" {
		t.Errorf("query = %v, want only the version", u.Query)
	}
	cs := u.Spec.TaskTemplate.ContainerSpec
	if cs == nil || cs.Image != "registry.example.com/web:2@sha256:abc" || !slices.Equal(cs.Env, []string{"A=1"}) ||
		u.Spec.Mode.Replicated == nil || *u.Spec.Mode.Replicated.Replicas != 4 {
		t.Errorf("the spec the daemon received is not the one sent: %+v", u.Spec)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /services/web-id/update?version=7"}) {
		t.Errorf("writes = %v", got)
	}
}

func TestServiceUpdateRefusesUnknownFieldsAndTrailingDataBeforeAnyCall(t *testing.T) {
	c, fake := serviceFake(t)
	for name, raw := range map[string]string{
		"unknown field": `{"Name":"web","Bogus":1}`,
		"wrong type":    `{"Name":5}`,
		"not json":      `{"Name":`,
		"trailing data": editedSpec + ` {}`,
		"empty":         ``,
	} {
		_, err := c.ServiceUpdate(context.Background(), "web-id", 7, json.RawMessage(raw), "")
		if !errors.Is(err, dockerx.ErrServiceSpecInvalid) {
			t.Errorf("%s: err = %v, want ErrServiceSpecInvalid", name, err)
		}
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none: a spec that does not decode is never sent", got)
	}
}

func TestServiceUpdateWithAStaleVersionIsServiceChangedAndSendsNoSecondUpdate(t *testing.T) {
	c, fake := serviceFake(t)
	_, err := c.ServiceUpdate(context.Background(), "web-id", 6, json.RawMessage(editedSpec), "")
	if !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("err = %v, want ErrServiceChanged", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: a refused write is never retried", got)
	}
	if got := len(fake.ServiceUpdates()); got != 0 {
		t.Errorf("accepted updates = %d, want none", got)
	}
}

func TestServiceUpdateOfAnUnknownServiceIsNotFound(t *testing.T) {
	c, _ := serviceFake(t)
	if _, err := c.ServiceUpdate(context.Background(), "nope", 1, json.RawMessage(editedSpec), ""); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
}

func TestServiceUpdateReturnsTheDaemonsWarnings(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{{ID: "web-id", Name: "web", Version: 3, UpdateWarnings: []string{"unable to pin image"}}},
	})
	c := newClient(t, fake)
	warnings, err := c.ServiceUpdate(context.Background(), "web-id", 3, json.RawMessage(editedSpec), "")
	if err != nil || !slices.Equal(warnings, []string{"unable to pin image"}) {
		t.Fatalf("ServiceUpdate = %v, %v; want the daemon's warning", warnings, err)
	}
}

func TestServiceUpdateTheDaemonsRefusalOfASpecKeepsItsWords(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{{ID: "web-id", Name: "web", Version: 3, UpdateError: "secret not found: db_password"}},
	})
	c := newClient(t, fake)
	_, err := c.ServiceUpdate(context.Background(), "web-id", 3, json.RawMessage(editedSpec), "")
	var rej *dockerx.SpecRejectedError
	if !errors.As(err, &rej) || rej.Message == "" || !strings.Contains(rej.Message, "secret not found: db_password") {
		t.Fatalf("err = %v, want a SpecRejectedError with the daemon's words", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one", got)
	}
}

// Networks (plan 06-18): the engine's networks, local ones too.

func TestNetworksListsTheLocalAndTheSwarmNetworksSortedByNameThenID(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: "ovl-1", Name: "app_net", Driver: "overlay", Scope: "swarm"},
			{ID: "host-1", Name: "host", Driver: "host", Scope: "local"},
			{ID: "br-1", Name: "bridge", Driver: "bridge", Scope: "local"},
		},
	})
	rows, err := newClient(t, fake).Networks(context.Background())
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Name+"/"+r.ID+"/"+r.Driver+"/"+r.Scope)
	}
	want := []string{"app_net/ovl-1/overlay/swarm", "bridge/br-1/bridge/local", "host/host-1/host/local"}
	if !slices.Equal(got, want) {
		t.Fatalf("networks = %v, want %v", got, want)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

// ServiceCreate (plan 06-27, RES-03, D-04, D-08).

const createdSpec = `{"Name":"chat","TaskTemplate":{"ContainerSpec":{"Image":"registry.example.com/chat:1@sha256:abc","Env":["A=1"]}},"Mode":{"Replicated":{"Replicas":1}}}`

func TestServiceCreateSendsTheWholeSpecOnceWithTheCredentialAndNoRegistryQuery(t *testing.T) {
	c, fake := serviceFake(t)
	id, warnings, err := c.ServiceCreate(context.Background(), json.RawMessage(createdSpec), "Zm9v")
	if err != nil || id == "" || len(warnings) != 0 {
		t.Fatalf("ServiceCreate = %q, %v, %v", id, warnings, err)
	}
	creates := fake.ServiceCreates()
	if len(creates) != 1 {
		t.Fatalf("creates = %+v, want exactly one", creates)
	}
	u := creates[0]
	if u.Auth != "Zm9v" || u.Query.Get("registryAuthFrom") != "" {
		t.Errorf("create = %+v, want the credential and nothing else in the query", u)
	}
	cs := u.Spec.TaskTemplate.ContainerSpec
	if u.Spec.Name != "chat" || cs == nil || cs.Image != "registry.example.com/chat:1@sha256:abc" || !slices.Equal(cs.Env, []string{"A=1"}) ||
		u.Spec.Mode.Replicated == nil || *u.Spec.Mode.Replicated.Replicas != 1 {
		t.Errorf("the spec the daemon received is not the one sent: %+v", u.Spec)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /services/create"}) {
		t.Errorf("writes = %v, want exactly one create", got)
	}
	// The service is there afterwards, under the ID it was answered with.
	if d, _, err := c.ServiceInspect(context.Background(), id); err != nil || d.Name != "chat" {
		t.Errorf("inspect after the create = %+v, %v", d, err)
	}
}

func TestServiceCreateRefusesUnknownFieldsAndTrailingDataBeforeAnyCall(t *testing.T) {
	c, fake := serviceFake(t)
	for name, raw := range map[string]string{
		"unknown field": `{"Name":"chat","Bogus":1}`,
		"wrong type":    `{"Name":5}`,
		"not json":      `{"Name":`,
		"trailing data": createdSpec + ` {}`,
		"empty":         ``,
	} {
		_, _, err := c.ServiceCreate(context.Background(), json.RawMessage(raw), "")
		if !errors.Is(err, dockerx.ErrServiceSpecInvalid) {
			t.Errorf("%s: err = %v, want ErrServiceSpecInvalid", name, err)
		}
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none: a spec that does not decode is never sent", got)
	}
}

func TestServiceCreateOfATakenNameIsServiceNameTakenAndIsNotRetried(t *testing.T) {
	c, fake := serviceFake(t)
	_, _, err := c.ServiceCreate(context.Background(), json.RawMessage(strings.Replace(createdSpec, `"chat"`, `"web"`, 1)), "")
	if !errors.Is(err, dockerx.ErrServiceNameTaken) {
		t.Fatalf("err = %v, want ErrServiceNameTaken", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: never retried", got)
	}
	if got := len(fake.ServiceCreates()); got != 0 {
		t.Errorf("accepted creates = %d, want none", got)
	}
}

func TestServiceCreateReturnsTheDaemonsWarnings(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, ServiceCreateWarnings: []string{"unable to pin image"},
	})
	_, warnings, err := newClient(t, fake).ServiceCreate(context.Background(), json.RawMessage(createdSpec), "")
	if err != nil || !slices.Equal(warnings, []string{"unable to pin image"}) {
		t.Fatalf("ServiceCreate warnings = %v, %v; want the daemon's warning", warnings, err)
	}
}

func TestServiceCreateTheDaemonsRefusalOfASpecKeepsItsWords(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, ServiceCreateError: "secret not found: db_password",
	})
	_, _, err := newClient(t, fake).ServiceCreate(context.Background(), json.RawMessage(createdSpec), "")
	var rej *dockerx.SpecRejectedError
	if !errors.As(err, &rej) || !strings.Contains(rej.Message, "secret not found: db_password") {
		t.Fatalf("err = %v, want a SpecRejectedError with the daemon's words", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one", got)
	}
}
