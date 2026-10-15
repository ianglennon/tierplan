// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

// Rotate a secret or config to a new version (plan 06-28, RES-08, D-26). The daemon is the fake one
// with injected faults; the checker is the real one over a real store.

const (
	oldSecretID = "sec-db1"
	oldConfigID = "cfg-app1"
)

// pwRef is how a service mounts the secret: a file named pw, owned by 70:71, readable by its owner.
func pwRef() dockertest.FakeServiceObject {
	return dockertest.FakeServiceObject{ID: oldSecretID, Name: "db_password", File: "pw", UID: "70", GID: "71", Mode: 0o400}
}

func confRef() dockertest.FakeServiceObject {
	return dockertest.FakeServiceObject{ID: oldConfigID, Name: "app.conf", File: "/etc/app.conf", UID: "0", GID: "0", Mode: 0o440}
}

// rotateWorld adds a secret and a config to the rig and makes three services use them: api (a spec
// of its own), worker (a plain row) and shop_web (a stack's). A fourth, zulu, sorts last.
func rotateWorld(cfg *dockertest.FakeConfig) {
	cfg.Secrets = []dockertest.FakeSecret{{ID: oldSecretID, Name: "db_password", Labels: map[string]string{"team": "core", "tier": "db"}}}
	cfg.Configs = []dockertest.FakeConfigObject{{
		ID: oldConfigID, Name: "app.conf", Labels: map[string]string{"team": "core"}, Data: []byte("level=info"), Templating: "golang",
	}}
	cs := cfg.ServiceRows[0].Spec.TaskTemplate.ContainerSpec
	cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
		SecretID: oldSecretID, SecretName: "db_password", File: &swarm.SecretReferenceFileTarget{Name: "db_password", UID: "0", GID: "0", Mode: 0o444},
	})
	cs.Configs = append(cs.Configs, &swarm.ConfigReference{
		ConfigID: oldConfigID, ConfigName: "app.conf", File: &swarm.ConfigReferenceFileTarget{Name: "/etc/app.conf", UID: "0", GID: "0", Mode: 0o444},
	})
	cfg.ServiceRows[1].Secrets = []dockertest.FakeServiceObject{pwRef()}
	cfg.ServiceRows[1].Configs = []dockertest.FakeServiceObject{confRef()}
	for _, name := range []string{"worker", "zulu"} {
		cfg.ServiceRows = append(cfg.ServiceRows, dockertest.FakeService{
			ID: "svc-" + name, Name: name, Mode: "replicated", Running: 1, Desired: 1, Version: 4, Image: name + ":1",
			Secrets: []dockertest.FakeServiceObject{pwRef()}, Configs: []dockertest.FakeServiceObject{confRef()},
		})
	}
}

func newRotateRig(t *testing.T, tune ...cfgFn) *objectRig {
	t.Helper()
	return newObjectRig(t, append([]cfgFn{rotateWorld}, tune...)...)
}

// noStackUsers takes the stack's service off the secret and the config, so that nothing is left behind
// the rotate.
func noStackUsers(cfg *dockertest.FakeConfig) {
	cfg.ServiceRows[1].Secrets, cfg.ServiceRows[1].Configs = nil, nil
}

func (o *objectRig) rotateSecret(who swarmops.Caller, mut ...func(*swarmops.RotateInput)) (swarmops.RotateResult, error) {
	o.t.Helper()
	in := swarmops.RotateInput{
		EnvID: envID, Client: o.client, Kind: swarmops.KindSecret, OldID: oldSecretID, Data: []byte(secretValue), RemoveOld: true, Caller: who,
	}
	for _, m := range mut {
		m(&in)
	}
	return swarmops.Rotate(context.Background(), o.deps, in)
}

func (o *objectRig) rotateConfig(who swarmops.Caller, mut ...func(*swarmops.RotateInput)) (swarmops.RotateResult, error) {
	o.t.Helper()
	in := swarmops.RotateInput{
		EnvID: envID, Client: o.client, Kind: swarmops.KindConfig, OldID: oldConfigID, Data: []byte("level=debug"), Templating: "golang",
		RemoveOld: true, Caller: who,
	}
	for _, m := range mut {
		m(&in)
	}
	return swarmops.Rotate(context.Background(), o.deps, in)
}

func (o *objectRig) previewRotate(who swarmops.Caller, kind, id, newName string) (swarmops.RotatePreviewResult, error) {
	o.t.Helper()
	return swarmops.RotatePreview(context.Background(), o.deps, swarmops.RotatePreviewInput{
		EnvID: envID, Client: o.client, Kind: kind, OldID: id, NewName: newName, Caller: who,
	})
}

// states lists "name:state" for every service of a result, in order.
func states(res swarmops.RotateResult) []string {
	out := make([]string, len(res.Services))
	for i, s := range res.Services {
		out[i] = s.Name + ":" + s.State
	}
	return out
}

func updateOf(t *testing.T, o *objectRig, service string) swarm.ServiceSpec {
	t.Helper()
	for _, u := range o.fake.ServiceUpdates() {
		if u.ServiceID == service {
			return u.Spec
		}
	}
	t.Fatalf("no update of %s among %+v", service, o.fake.ServiceUpdates())
	return swarm.ServiceSpec{}
}

func TestRotateASecretCreatesTheNewVersionAndMovesEveryStandaloneUserKeepingItsFileTarget(t *testing.T) {
	o := newRotateRig(t)
	res, err := o.rotateSecret(o.admin)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if res.OldID != oldSecretID || res.OldName != "db_password" || res.NewName != "db_password_v2" || res.NewID == "" || !res.Created {
		t.Fatalf("result = %+v, want db_password rotated to a new db_password_v2", res)
	}
	creates := o.fake.SecretCreates()
	if len(creates) != 1 || creates[0].Name != "db_password_v2" || creates[0].DataBytes != len(secretValue) ||
		creates[0].Labels["team"] != "core" || creates[0].Labels["tier"] != "db" {
		t.Fatalf("creates = %+v, want one with the value and the old object's labels", creates)
	}
	want := []string{"api:updated", "shop_web:skipped", "worker:updated", "zulu:updated"}
	if got := states(res); !slices.Equal(got, want) {
		t.Fatalf("services = %v, want %v", got, want)
	}
	if res.Updated != 3 || res.Skipped != 1 || res.FailedAt != "" {
		t.Errorf("updated %d skipped %d failedAt %q, want 3, 1 and none", res.Updated, res.Skipped, res.FailedAt)
	}
	if res.Services[1].Stack != "shop" {
		t.Errorf("the skipped service's stack = %q, want shop", res.Services[1].Stack)
	}
	ups := o.fake.ServiceUpdates()
	if len(ups) != 3 || ups[0].ServiceID != "svc-api" || ups[1].ServiceID != "svc-worker" || ups[2].ServiceID != "svc-zulu" {
		t.Fatalf("updates = %+v, want api, worker and zulu one at a time in name order", ups)
	}
	api := updateOf(t, o, "svc-api").TaskTemplate.ContainerSpec.Secrets
	if len(api) != 1 || api[0].SecretID != res.NewID || api[0].SecretName != "db_password_v2" ||
		api[0].File.Name != "db_password" || api[0].File.UID != "0" || api[0].File.GID != "0" || api[0].File.Mode != 0o444 {
		t.Errorf("api's secret = %+v file %+v, want the new object with the same file target", api[0], api[0].File)
	}
	worker := updateOf(t, o, "svc-worker").TaskTemplate.ContainerSpec.Secrets
	if len(worker) != 1 || worker[0].SecretID != res.NewID || worker[0].File.Name != "pw" || worker[0].File.UID != "70" ||
		worker[0].File.GID != "71" || worker[0].File.Mode != 0o400 {
		t.Errorf("worker's secret = %+v file %+v, want file pw owned 70:71 mode 400", worker[0], worker[0].File)
	}
	if cfgs := updateOf(t, o, "svc-api").TaskTemplate.ContainerSpec.Configs; len(cfgs) != 1 || cfgs[0].ConfigID != oldConfigID {
		t.Errorf("api's config = %+v, a secret rotate must leave configs alone", cfgs)
	}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, []string{"POST secrets", "POST services", "POST services", "POST services"}) {
		t.Errorf("writes = %v, want the create then one update each and no delete while a stack still uses it", got)
	}
	if res.OldRemoved || res.RemoveCode != "object_in_use" {
		t.Errorf("oldRemoved %v code %q, want it kept because a stack's service still uses it", res.OldRemoved, res.RemoveCode)
	}
	neverHolds(t, "the result", res)
	neverHolds(t, "the audit rows", o.rows)
}

func TestRotateRemovesTheOldObjectOnceNothingUsesIt(t *testing.T) {
	o := newRotateRig(t, noStackUsers)
	res, err := o.rotateSecret(o.admin)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if !res.OldRemoved || res.RemoveCode != "" || res.Updated != 3 || res.Skipped != 0 {
		t.Errorf("result = %+v, want the old secret removed after three updates", res)
	}
	want := []string{"POST secrets", "POST services", "POST services", "POST services", "DELETE secrets"}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	if _, ok := o.secretNamed("db_password"); ok {
		t.Error("the old secret is still there")
	}
	var rows []string
	for _, r := range o.rows {
		rows = append(rows, r.Action+":"+r.Details["name"]+":"+r.Details["source"]+":"+r.Details["code"])
	}
	want = []string{"secret.created:db_password_v2:rotate:created", "secret.removed:db_password:rotate:removed"}
	if !slices.Equal(rows, want) {
		t.Errorf("object audit rows = %v, want %v", rows, want)
	}
}

func TestRotateKeepsTheOldObjectWhenRemoveOldIsOff(t *testing.T) {
	o := newRotateRig(t, noStackUsers)
	res, err := o.rotateSecret(o.admin, func(in *swarmops.RotateInput) { in.RemoveOld = false })
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if res.OldRemoved || res.RemoveCode != "" {
		t.Errorf("result = %+v, want the old secret kept and no code", res)
	}
	if got := writeKinds(o.fake.Writes()); slices.Contains(got, "DELETE secrets") {
		t.Errorf("writes = %v, want no delete", got)
	}
}

func TestRotateUsesTheNameItIsGivenAndOtherwiseTheNextVersion(t *testing.T) {
	o := newRotateRig(t)
	res, err := o.rotateSecret(o.admin, func(in *swarmops.RotateInput) { in.NewName = "db_password_2026" })
	if err != nil || res.NewName != "db_password_2026" {
		t.Fatalf("Rotate = %+v, %v, want the name it was given", res, err)
	}
	// db_password_v2 is taken now, so the next rotation of the first secret goes past it.
	o = newRotateRig(t, func(cfg *dockertest.FakeConfig) {
		cfg.Secrets = append(cfg.Secrets, dockertest.FakeSecret{ID: "sec-taken", Name: "db_password_v2"})
	})
	res, err = o.rotateSecret(o.admin)
	if err != nil || res.NewName != "db_password_v3" {
		t.Fatalf("Rotate = %+v, %v, want db_password_v3", res, err)
	}
}

func TestRotateAConfigCopiesItsLabelsAndTakesTheTemplatingItIsGiven(t *testing.T) {
	o := newRotateRig(t, noStackUsers)
	res, err := o.rotateConfig(o.admin)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if res.NewName != "app.conf_v2" || res.Updated != 3 || !res.OldRemoved {
		t.Fatalf("result = %+v", res)
	}
	creates := o.fake.ConfigCreates()
	if len(creates) != 1 || creates[0].Name != "app.conf_v2" || string(creates[0].Data) != "level=debug" ||
		creates[0].Templating != "golang" || creates[0].Labels["team"] != "core" {
		t.Fatalf("creates = %+v, want the new content, the golang driver and the old labels", creates)
	}
	conf := updateOf(t, o, "svc-worker").TaskTemplate.ContainerSpec.Configs
	if len(conf) != 1 || conf[0].ConfigID != res.NewID || conf[0].ConfigName != "app.conf_v2" || conf[0].File.Name != "/etc/app.conf" || conf[0].File.Mode != 0o440 {
		t.Errorf("worker's config = %+v file %+v, want the new object with the same file target", conf[0], conf[0].File)
	}
	if got := writeKinds(o.fake.Writes()); got[len(got)-1] != "DELETE configs" {
		t.Errorf("writes = %v, want the old config deleted last", got)
	}
}

func TestRotateWithNoUsersOnlyCreatesTheNewObject(t *testing.T) {
	o := newRotateRig(t, func(cfg *dockertest.FakeConfig) {
		for i := range cfg.ServiceRows {
			cfg.ServiceRows[i].Secrets = nil
		}
		cs := cfg.ServiceRows[0].Spec.TaskTemplate.ContainerSpec
		cs.Secrets = nil
	})
	res, err := o.rotateSecret(o.admin)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if len(res.Services) != 0 || res.Updated != 0 || res.OldRemoved || !res.Created {
		t.Errorf("result = %+v, want only the new object and the old one kept", res)
	}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, []string{"POST secrets"}) {
		t.Errorf("writes = %v, want one create", got)
	}
}

func TestRotateStopsAtTheFirstFailureLeavesTheOldObjectAndSaysWhatMoved(t *testing.T) {
	o := newRotateRig(t, noStackUsers, func(cfg *dockertest.FakeConfig) {
		for i := range cfg.ServiceRows {
			if cfg.ServiceRows[i].Name == "worker" {
				cfg.ServiceRows[i].UpdateError = "rpc error: code = InvalidArgument desc = bad secret reference"
			}
		}
	})
	res, err := o.rotateSecret(o.operator)
	var stop *swarmops.RotateStopError
	if !errors.As(err, &stop) || !errors.Is(err, swarmops.ErrRotateStopped) || stop.Service != "worker" {
		t.Fatalf("err = %v, want a *RotateStopError at worker", err)
	}
	var rej *swarmops.RejectedError
	if !errors.As(err, &rej) {
		t.Errorf("err = %v, want the manager's refusal inside", err)
	}
	if want := []string{"api:updated", "worker:failed", "zulu:pending"}; !slices.Equal(states(res), want) {
		t.Errorf("services = %v, want %v", states(res), want)
	}
	if res.FailedAt != "worker" || res.Updated != 1 || res.Services[1].Code != "spec_rejected" {
		t.Errorf("result = %+v, want it stopped at worker after one update", res)
	}
	if !res.Created || res.OldRemoved {
		t.Errorf("created %v oldRemoved %v, want the new object kept and the old one too", res.Created, res.OldRemoved)
	}
	if ups := o.fake.ServiceUpdates(); len(ups) != 1 || ups[0].ServiceID != "svc-api" {
		t.Errorf("updates = %+v, want only api moved", ups)
	}
	if got := writeKinds(o.fake.Writes()); slices.Contains(got, "DELETE secrets") {
		t.Errorf("writes = %v, want no delete after a failure", got)
	}
	neverHolds(t, "the error", err.Error())
	neverHolds(t, "the result", res)
}

// bumping is a credential resolver that also plays the other writer: the first time it is asked, it
// changes a service, after the rotate has read it and before the rotate writes it.
type bumping struct {
	fake    *dockertest.Fake
	service string
	done    bool
}

func (b *bumping) AuthFor(_ context.Context, _ string, _ registry.Ref) (string, *registry.Candidate, error) {
	if !b.done {
		b.done = true
		b.fake.BumpService(b.service)
	}
	return "", nil, nil
}

func TestRotateRacedByAnEditOfAServiceStopsThereWithServiceChangedAndRetriesNothing(t *testing.T) {
	o := newRotateRig(t, noStackUsers)
	o.deps.Resolver = &bumping{fake: o.fake, service: "svc-api"}
	res, err := o.rotateSecret(o.admin)
	if !errors.Is(err, dockerx.ErrServiceChanged) || !errors.Is(err, swarmops.ErrRotateStopped) {
		t.Fatalf("err = %v, want service_changed at the first service", err)
	}
	if want := []string{"api:failed", "worker:pending", "zulu:pending"}; !slices.Equal(states(res), want) {
		t.Errorf("services = %v, want %v", states(res), want)
	}
	if res.Services[0].Code != "service_changed" {
		t.Errorf("code = %q, want service_changed", res.Services[0].Code)
	}
	updates := 0
	for _, w := range o.fake.Writes() {
		if strings.HasPrefix(w, "POST /services/") {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("service writes = %d (%v), want the one refused attempt and no retry", updates, o.fake.Writes())
	}
	if res.OldRemoved {
		t.Error("the old secret was removed")
	}
}

func TestRotateWhoseNewNameIsTakenChangesNothingElse(t *testing.T) {
	o := newRotateRig(t, func(cfg *dockertest.FakeConfig) {
		cfg.Secrets = append(cfg.Secrets, dockertest.FakeSecret{ID: "sec-taken", Name: "db_password_v2"})
	})
	res, err := o.rotateSecret(o.admin, func(in *swarmops.RotateInput) { in.NewName = "db_password_v2" })
	var made *swarmops.ObjectCreateError
	if !errors.As(err, &made) || !errors.Is(err, dockerx.ErrNameTaken) || made.Name != "db_password_v2" {
		t.Fatalf("err = %v, want the create refused as a taken name", err)
	}
	if res.Created || res.Updated != 0 {
		t.Errorf("result = %+v, want nothing created and nothing moved", res)
	}
	if ups := o.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none", ups)
	}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, []string{"POST secrets"}) {
		t.Errorf("writes = %v, want the refused create only", got)
	}
	if len(o.rows) != 1 || o.rows[0].Result != audit.ResultFailure || o.rows[0].Reason != "name_taken" {
		t.Errorf("object audit rows = %+v, want one failed create for name_taken", o.rows)
	}
}

func TestRotateRunsTheFullSpecPolicyForTheCallerOnEachServiceAndStopsAtARefusal(t *testing.T) {
	o := newRotateRig(t, noStackUsers, func(cfg *dockertest.FakeConfig) {
		cs := cfg.ServiceRows[0].Spec.TaskTemplate.ContainerSpec
		cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/etc", Target: "/host-etc"})
	})
	res, err := o.rotateSecret(o.operator)
	if !errors.Is(err, swarmops.ErrRefused) || !errors.Is(err, swarmops.ErrRotateStopped) {
		t.Fatalf("err = %v, want the operator refused host access at api", err)
	}
	if res.FailedAt != "api" || res.Services[0].Code != "privileged_service" || res.Services[0].Findings == 0 {
		t.Errorf("result = %+v, want it stopped at api with the findings counted", res)
	}
	if ups := o.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none", ups)
	}

	o = newRotateRig(t, noStackUsers, func(cfg *dockertest.FakeConfig) {
		cs := cfg.ServiceRows[0].Spec.TaskTemplate.ContainerSpec
		cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/etc", Target: "/host-etc"})
	})
	if _, err := o.rotateSecret(o.admin); err != nil {
		t.Errorf("an Admin is allowed host access: %v", err)
	}
}

func TestRotateSendsEachServiceTheRegistryCredentialOfItsImage(t *testing.T) {
	o := newRotateRig(t, noStackUsers)
	auth := &fixedAuth{header: "Zm9vYmFy"}
	o.deps.Resolver = auth
	if _, err := o.rotateSecret(o.admin); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	ups := o.fake.ServiceUpdates()
	if len(ups) != 3 {
		t.Fatalf("updates = %d, want 3", len(ups))
	}
	for _, u := range ups {
		if u.Auth != "Zm9vYmFy" {
			t.Errorf("update of %s carried %q, want the credential header", u.ServiceID, u.Auth)
		}
	}
}

func TestRotateOfAnObjectThatIsNotThereIsNotFound(t *testing.T) {
	o := newRotateRig(t)
	_, err := o.rotateSecret(o.admin, func(in *swarmops.RotateInput) { in.OldID = "sec-gone" })
	if !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("err = %v, want ErrObjectNotFound", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestRotatePreviewListsUsersInNameOrderSkipsStackOwnedOnesAndDiffsTheReference(t *testing.T) {
	o := newRotateRig(t)
	res, err := o.previewRotate(o.operator, swarmops.KindSecret, oldSecretID, "")
	if err != nil {
		t.Fatalf("RotatePreview: %v", err)
	}
	if res.OldName != "db_password" || res.NewName != "db_password_v2" || res.NameTaken || res.Skipped != 1 {
		t.Fatalf("preview = %+v", res)
	}
	var names []string
	for _, u := range res.Users {
		names = append(names, u.Name)
	}
	if !slices.Equal(names, []string{"api", "shop_web", "worker", "zulu"}) {
		t.Fatalf("users = %v, want name order", names)
	}
	shop := res.Users[1]
	if !shop.Skipped || shop.Stack != "shop" {
		t.Errorf("shop_web = %+v, want skipped and owned by shop", shop)
	}
	api := res.Users[0]
	if api.Skipped || api.File != "db_password" || api.UID != "0" || api.GID != "0" || api.Mode != 0o444 {
		t.Errorf("api = %+v, want its file target", api)
	}
	cur, next := api.Current.TaskTemplate.ContainerSpec, api.Next.TaskTemplate.ContainerSpec
	if cur.Secrets[0].SecretID != oldSecretID || next.Secrets[0].SecretName != "db_password_v2" || next.Secrets[0].File.Name != "db_password" {
		t.Errorf("current %+v and next %+v, want the reference replaced and the file kept", cur.Secrets[0], next.Secrets[0])
	}
	if !slices.Equal(cur.Env, next.Env) || len(cur.Env) == 0 {
		t.Errorf("env %v and %v, want the same masked list on both sides", cur.Env, next.Env)
	}
	neverHolds(t, "the preview", res)
	for _, v := range []string{"hunter2", "s3cr3t-token", "hc-secret-key", "logtoken-123"} {
		b, _ := json.Marshal(res)
		if strings.Contains(string(b), v) {
			t.Errorf("the preview holds %q for a caller who cannot reveal it", v)
		}
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, a preview changes nothing", got)
	}
}

func TestRotatePreviewOfAConfigCarriesItsTemplatingAndLabelsAndFlagsATakenName(t *testing.T) {
	o := newRotateRig(t, func(cfg *dockertest.FakeConfig) {
		cfg.Configs = append(cfg.Configs, dockertest.FakeConfigObject{ID: "cfg-taken", Name: "app.conf_v9", Data: []byte("x")})
	})
	res, err := o.previewRotate(o.admin, swarmops.KindConfig, oldConfigID, "app.conf_v9")
	if err != nil {
		t.Fatalf("RotatePreview: %v", err)
	}
	if res.Templating != "golang" || res.Labels["team"] != "core" || res.NewName != "app.conf_v9" || !res.NameTaken {
		t.Errorf("preview = %+v, want golang, the labels and the typed name flagged as taken", res)
	}
	if len(res.Users) != 4 || res.Users[0].Next.TaskTemplate.ContainerSpec.Configs[0].ConfigName != "app.conf_v9" {
		t.Errorf("users = %+v", res.Users)
	}
}

func TestRotatePreviewOfAnObjectNothingUsesHasNoUsers(t *testing.T) {
	o := newRotateRig(t)
	res, err := o.previewRotate(o.admin, swarmops.KindSecret, "sec-unused", "")
	if !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Fatalf("RotatePreview = %+v, %v, want not found for an object that is not there", res, err)
	}
	o = newRotateRig(t, func(cfg *dockertest.FakeConfig) {
		cfg.Secrets = append(cfg.Secrets, dockertest.FakeSecret{ID: "sec-unused", Name: "spare"})
	})
	res, err = o.previewRotate(o.admin, swarmops.KindSecret, "sec-unused", "")
	if err != nil || len(res.Users) != 0 || res.NewName != "spare_v2" {
		t.Errorf("RotatePreview = %+v, %v, want no users and spare_v2", res, err)
	}
}
