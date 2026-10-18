// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

// Staged objects, their creation in dependency order and their cleanup (plan 06-26, RES-03, D-07,
// D-13). The daemon is the fake one with injected faults.

const secretValue = "s3cret-staged-value" //nolint:gosec // G101: a fixture value the tests look for in every answer

// objectRig is a rig whose deps record audit rows and let the caller create every kind.
type objectRig struct {
	*rig
	rows []audit.Entry
}

func newObjectRig(t *testing.T, tune ...cfgFn) *objectRig {
	t.Helper()
	o := &objectRig{rig: newRig(t, tune...)}
	o.deps.Record = func(e audit.Entry) { o.rows = append(o.rows, e) }
	o.deps.CanCreate = func(string) bool { return true }
	return o
}

// withRefs makes the spec name a network, a secret and a config by name only, as the editor does
// for objects it has staged.
func withRefs(s *swarm.ServiceSpec) {
	cs := s.TaskTemplate.ContainerSpec
	s.TaskTemplate.Networks = append(s.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: "front"})
	cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
		SecretName: "db-pass", File: &swarm.SecretReferenceFileTarget{Name: "db-pass", UID: "0", GID: "0", Mode: 0o444},
	})
	cs.Configs = append(cs.Configs, &swarm.ConfigReference{
		ConfigName: "app.conf", File: &swarm.ConfigReferenceFileTarget{Name: "/app.conf", UID: "0", GID: "0", Mode: 0o444},
	})
}

// staged is the three objects withRefs names, in an order that is not the creation order.
func staged() []swarmops.StagedObject {
	return []swarmops.StagedObject{
		{Kind: swarmops.KindConfig, Name: "app.conf", Data: []byte("level=debug")},
		{Kind: swarmops.KindSecret, Name: "db-pass", Data: []byte(secretValue), Labels: map[string]string{"team": "core"}},
		{Kind: swarmops.KindNetwork, Name: "front"},
	}
}

func (o *objectRig) applyStaged(who swarmops.Caller, spec json.RawMessage, objects []swarmops.StagedObject) (swarmops.ApplyResult, error) {
	o.t.Helper()
	return swarmops.Apply(context.Background(), o.deps, swarmops.ApplyInput{
		EnvID: envID, Client: o.client, ServiceID: "svc-api", Version: 5, Spec: spec, Caller: who, Objects: objects,
	})
}

func (o *objectRig) secretNamed(name string) (dockerx.SecretRow, bool) {
	o.t.Helper()
	rows, err := o.client.SecretList(context.Background())
	if err != nil {
		o.t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return dockerx.SecretRow{}, false
}

func (o *objectRig) leftovers() []string {
	o.t.Helper()
	ctx := context.Background()
	var out []string
	secrets, err := o.client.SecretList(ctx)
	if err != nil {
		o.t.Fatal(err)
	}
	for _, s := range secrets {
		out = append(out, "secret "+s.Name)
	}
	configs, err := o.client.ConfigList(ctx)
	if err != nil {
		o.t.Fatal(err)
	}
	for _, c := range configs {
		out = append(out, "config "+c.Name)
	}
	nets, err := o.client.Networks(ctx)
	if err != nil {
		o.t.Fatal(err)
	}
	for _, n := range nets {
		if n.Name == "front" {
			out = append(out, "network "+n.Name)
		}
	}
	return out
}

func writeKinds(writes []string) []string {
	out := make([]string, 0, len(writes))
	for _, w := range writes {
		method, rest, _ := strings.Cut(w, " ")
		parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
		out = append(out, method+" "+parts[0])
	}
	return out
}

func neverHolds(t *testing.T, what string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), secretValue) || strings.Contains(fmt.Sprint(v), secretValue) {
		t.Errorf("%s holds the staged value: %s", what, b)
	}
}

func TestApplyCreatesObjectsInOrder(t *testing.T) {
	o := newObjectRig(t)
	res, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), staged())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{"POST networks", "POST secrets", "POST configs", "POST services"}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	updates := o.fake.ServiceUpdates()
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want one", len(updates))
	}
	sec, ok := o.secretNamed("db-pass")
	if !ok {
		t.Fatal("the secret was not created")
	}
	cs := updates[0].Spec.TaskTemplate.ContainerSpec
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretID != sec.ID || cs.Secrets[0].SecretName != "db-pass" {
		t.Errorf("secret references = %+v, want the new secret's ID %q filled in", cs.Secrets, sec.ID)
	}
	if len(cs.Configs) != 1 || cs.Configs[0].ConfigID == "" || cs.Configs[0].ConfigName != "app.conf" {
		t.Errorf("config references = %+v, want the new config's ID filled in", cs.Configs)
	}
	nets := o.fake.NetworkCreates()
	if len(nets) != 1 || nets[0].Name != "front" || nets[0].Driver != "overlay" || !nets[0].Attachable {
		t.Errorf("network creates = %+v, want one attachable overlay named front", nets)
	}
	if got := o.fake.SecretCreates(); len(got) != 1 || got[0].DataBytes != len(secretValue) || got[0].Labels["team"] != "core" {
		t.Errorf("secret creates = %+v", got)
	}
	if got := o.fake.ConfigCreates(); len(got) != 1 || string(got[0].Data) != "level=debug" {
		t.Errorf("config creates = %+v", got)
	}

	wantSteps := []swarmops.ApplyStep{
		{Step: "image", State: "skipped"},
		{Step: "object", Kind: "network", Name: "front", State: "done"},
		{Step: "object", Kind: "secret", Name: "db-pass", State: "done"},
		{Step: "object", Kind: "config", Name: "app.conf", State: "done"},
		{Step: "service", Name: "api", State: "done"},
	}
	if !slices.Equal(res.Steps, wantSteps) {
		t.Errorf("steps = %+v, want %+v", res.Steps, wantSteps)
	}
	if len(res.Cleanup.Removed)+len(res.Cleanup.NotRemoved) != 0 {
		t.Errorf("cleanup = %+v, want none", res.Cleanup)
	}

	var actions []string
	for _, row := range o.rows {
		actions = append(actions, row.Action)
		if row.Details["source"] != swarmops.SourceApply || row.Result != audit.ResultSuccess {
			t.Errorf("row %s: source %q result %q", row.Action, row.Details["source"], row.Result)
		}
	}
	if want := []string{audit.ActionNetworkCreated, audit.ActionSecretCreated, audit.ActionConfigCreated}; !slices.Equal(actions, want) {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
	neverHolds(t, "the result", res)
	neverHolds(t, "the audit rows", o.rows)
}

func TestApplyCleansUpOnServiceFailure(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) { c.ServiceRows[0].UpdateError = "the manager refuses this spec" })
	res, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), staged())
	var rej *swarmops.RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("err = %v, want RejectedError", err)
	}
	want := []string{"POST networks", "POST secrets", "POST configs", "POST services", "DELETE configs", "DELETE secrets", "DELETE networks"}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v: the cleanup runs in reverse order", got, want)
	}
	if left := o.leftovers(); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	var removed []string
	for _, c := range res.Cleanup.Removed {
		removed = append(removed, c.Kind+" "+c.Name)
	}
	if want := []string{"config app.conf", "secret db-pass", "network front"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	if len(res.Cleanup.NotRemoved) != 0 {
		t.Errorf("not removed = %+v", res.Cleanup.NotRemoved)
	}
	last := res.Steps[len(res.Steps)-1]
	if last.Step != "service" || last.State != "failed" || last.Code != "spec_rejected" {
		t.Errorf("last step = %+v, want the service step failed with spec_rejected", last)
	}
	var created, removedRows int
	for _, row := range o.rows {
		switch row.Details["source"] {
		case swarmops.SourceApply:
			created++
		case swarmops.SourceApplyCleanup:
			removedRows++
			if row.Result != audit.ResultSuccess {
				t.Errorf("cleanup row %s: result %q", row.Action, row.Result)
			}
		}
	}
	if created != 3 || removedRows != 3 {
		t.Errorf("audit rows: %d created, %d removed, want 3 and 3", created, removedRows)
	}
	neverHolds(t, "the error", err.Error())
	neverHolds(t, "the result", res)
	neverHolds(t, "the audit rows", o.rows)
}

func TestApplyReportsIncompleteCleanup(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceRows[0].UpdateError = "the manager refuses this spec"
		c.RemoveErrors = map[string]string{"db-pass": "the manager is busy"}
	})
	res, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), staged())
	if !errors.Is(err, swarmops.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if want := []swarmops.CreatedObject{{Kind: "secret", Name: "db-pass"}}; len(res.Cleanup.NotRemoved) != 1 ||
		res.Cleanup.NotRemoved[0].Kind != want[0].Kind || res.Cleanup.NotRemoved[0].Name != want[0].Name || res.Cleanup.NotRemoved[0].ID == "" {
		t.Errorf("not removed = %+v, want the secret db-pass with its ID", res.Cleanup.NotRemoved)
	}
	var removed []string
	for _, c := range res.Cleanup.Removed {
		removed = append(removed, c.Kind+" "+c.Name)
	}
	if want := []string{"config app.conf", "network front"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v: one failed removal does not stop the others", removed, want)
	}
	if left := o.leftovers(); !slices.Equal(left, []string{"secret db-pass"}) {
		t.Errorf("left behind = %v, want only the secret that could not be removed", left)
	}
	var failure *audit.Entry
	for i := range o.rows {
		if o.rows[i].Result == audit.ResultFailure {
			failure = &o.rows[i]
		}
	}
	if failure == nil || failure.Action != audit.ActionSecretRemoved || failure.Details["source"] != swarmops.SourceApplyCleanup {
		t.Errorf("failure row = %+v, want a failed secret.removed from the cleanup", failure)
	}
	neverHolds(t, "the audit rows", o.rows)
}

func TestApplyNameTakenMidway(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-elsewhere", Name: "db-pass"}}
	})
	res, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), staged())
	var ce *swarmops.ObjectCreateError
	if !errors.Is(err, dockerx.ErrNameTaken) || !errors.As(err, &ce) || ce.Kind != "secret" || ce.Name != "db-pass" {
		t.Fatalf("err = %v, want a name_taken create error for secret db-pass", err)
	}
	want := []string{"POST networks", "POST secrets", "DELETE networks"}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v: nothing is retried and the config is never created", got, want)
	}
	if len(res.Cleanup.Removed) != 1 || res.Cleanup.Removed[0].Kind != "network" {
		t.Errorf("cleanup = %+v, want the network removed", res.Cleanup)
	}
	for _, s := range res.Steps {
		if s.Kind == "secret" && (s.State != "failed" || s.Code != "name_taken") {
			t.Errorf("secret step = %+v, want failed with name_taken", s)
		}
		if s.Kind == "config" && s.State != "skipped" {
			t.Errorf("config step = %+v, want skipped", s)
		}
	}
	if len(o.fake.ServiceUpdates()) != 0 {
		t.Error("the service was updated")
	}
	// The secret someone else made is still there, untouched.
	if row, ok := o.secretNamed("db-pass"); !ok || row.ID != "sec-elsewhere" {
		t.Errorf("secret = %+v, want the one made elsewhere", row)
	}
}

func TestApplyObjectPermission(t *testing.T) {
	o := newObjectRig(t)
	o.deps.CanCreate = func(kind string) bool { return kind == swarmops.KindNetwork }
	_, err := o.applyStaged(o.operatorReveal, editFor(t, o.operatorReveal, withRefs), staged())
	var perm *swarmops.ObjectPermissionError
	if !errors.Is(err, swarmops.ErrObjectPermission) || !errors.As(err, &perm) || !slices.Equal(perm.Kinds, []string{"secret", "config"}) {
		t.Fatalf("err = %v, want ObjectPermissionError for secret and config", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v: nothing is created for a caller who may not create everything", got)
	}
	if len(o.rows) != 0 {
		t.Errorf("audit rows = %+v", o.rows)
	}
}

func TestApplyWithoutACanCreateHookCreatesNothing(t *testing.T) {
	o := newObjectRig(t)
	o.deps.CanCreate = nil
	if _, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), staged()); !errors.Is(err, swarmops.ErrObjectPermission) {
		t.Fatalf("err = %v, want ErrObjectPermission: no hook means nobody may", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestApplyNamesWhatIsMissingAndNotStaged(t *testing.T) {
	o := newObjectRig(t)
	_, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), []swarmops.StagedObject{{Kind: swarmops.KindNetwork, Name: "front"}})
	var missing *swarmops.MissingError
	if !errors.Is(err, swarmops.ErrObjectsMissing) || !errors.As(err, &missing) ||
		len(missing.Missing.Networks) != 0 || !slices.Equal(missing.Missing.Secrets, []string{"db-pass"}) || !slices.Equal(missing.Missing.Configs, []string{"app.conf"}) {
		t.Fatalf("err = %v (%+v), want the secret and config named as missing and the network not", err, missing)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v: a missing object that is not staged stops the apply before anything is made", got)
	}
}

func TestReviewListsMissingSecretsAndConfigsAsWellAsNetworks(t *testing.T) {
	o := newObjectRig(t)
	res, err := o.review(o.adminReveal, 5, editFor(t, o.adminReveal, withRefs))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Missing.Networks, []string{"front"}) || !slices.Equal(res.Missing.Secrets, []string{"db-pass"}) || !slices.Equal(res.Missing.Configs, []string{"app.conf"}) {
		t.Errorf("missing = %+v", res.Missing)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v: a review changes nothing", got)
	}
}

func TestApplyBindsNameOnlyReferencesToObjectsThatExist(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-api-key", Name: "api-key"}}
		c.Configs = []dockertest.FakeConfigObject{{ID: "cfg-nginx", Name: "nginx.conf", Data: []byte("x")}}
	})
	if _, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, func(s *swarm.ServiceSpec) {
		cs := s.TaskTemplate.ContainerSpec
		cs.Secrets = append(cs.Secrets, &swarm.SecretReference{SecretName: "api-key", File: &swarm.SecretReferenceFileTarget{Name: "api-key", UID: "0", GID: "0", Mode: 0o444}})
		cs.Configs = append(cs.Configs, &swarm.ConfigReference{ConfigID: "cfg-nginx", ConfigName: "nginx.conf", File: &swarm.ConfigReferenceFileTarget{Name: "/nginx.conf", UID: "0", GID: "0", Mode: 0o444}})
	}), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cs := o.fake.ServiceUpdates()[0].Spec.TaskTemplate.ContainerSpec
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretID != "sec-api-key" {
		t.Errorf("secrets = %+v, want the existing secret's ID filled in", cs.Secrets)
	}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, []string{"POST services"}) {
		t.Errorf("writes = %v, want only the service update", got)
	}
}

func TestApplyIgnoresStagedObjectsTheSpecDoesNotName(t *testing.T) {
	o := newObjectRig(t)
	objects := append(staged(), swarmops.StagedObject{Kind: swarmops.KindSecret, Name: "unused", Data: []byte("v")})
	if _, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), objects); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := o.secretNamed("unused"); ok {
		t.Error("a staged object the spec does not name was created")
	}
}

func TestApplyOfAnExistingNameThatWasStagedIsNameTakenNotABinding(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "front-id-0123456789", Name: "front", Driver: "overlay", Scope: "swarm"})
	})
	_, err := o.applyStaged(o.adminReveal, editFor(t, o.adminReveal, withRefs), staged())
	if !errors.Is(err, dockerx.ErrNameTaken) {
		t.Fatalf("err = %v, want name_taken: the editor staged front, so it must not silently use somebody else's", err)
	}
	if got := o.fake.Writes(); len(got) != 1 {
		t.Errorf("writes = %v, want the one refused network create", got)
	}
}

func TestApplyPutsTheNewNetworkIDInTheSpecAndKeepsAliases(t *testing.T) {
	o := newObjectRig(t)
	spec := edit(t, func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = append(s.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: "front", Aliases: []string{"web"}})
	})
	if _, err := o.applyStaged(o.admin, spec, []swarmops.StagedObject{{Kind: swarmops.KindNetwork, Name: "front", Subnet: "10.77.0.0/24"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	nets := o.fake.ServiceUpdates()[0].Spec.TaskTemplate.Networks
	last := nets[len(nets)-1]
	if len(last.Target) < 32 || !slices.Equal(last.Aliases, []string{"web"}) {
		t.Errorf("network = %+v, want the new network's ID as the target with its aliases kept", last)
	}
	if creates := o.fake.NetworkCreates(); len(creates) != 1 || len(creates[0].IPAM.Config) != 1 || creates[0].IPAM.Config[0].Subnet.String() != "10.77.0.0/24" {
		t.Errorf("network creates = %+v, want the subnet sent", creates)
	}
}
