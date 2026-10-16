// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

// A caller without secrets.reveal cannot add a secret reference the service does not have (review
// finding S1-WR-02, D-13 as amended). The daemon is the fake one with injected faults; the checker is
// the real one over a real store.

// withDBPass makes the cluster hold the secret db-pass, which the api service does not mount.
func withDBPass(cfg *dockertest.FakeConfig) {
	cfg.Secrets = append(cfg.Secrets, dockertest.FakeSecret{ID: "sec-db", Name: "db-pass"})
}

// mountSecret adds a reference to the secret named name, mounted as a file of that name.
func mountSecret(name string) func(*swarm.ServiceSpec) {
	return func(s *swarm.ServiceSpec) {
		cs := s.TaskTemplate.ContainerSpec
		cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
			SecretName: name, File: &swarm.SecretReferenceFileTarget{Name: name, UID: "0", GID: "0", Mode: 0o444},
		})
	}
}

func TestApplyByACallerWhoCannotRevealSecretsCannotAddASecretTheServiceDoesNotUse(t *testing.T) {
	r := newRig(t, withDBPass)
	both := func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Image = "localhost:5000/app/api:2"
		mountSecret("db-pass")(s)
	}
	_, err := r.apply(r.operator, 5, edit(t, both))
	var need *swarmops.RevealRequiredError
	if !errors.Is(err, swarmops.ErrRevealRequired) || !errors.As(err, &need) {
		t.Fatalf("err = %v, want a RevealRequiredError", err)
	}
	if len(need.Secrets) != 1 || need.Secrets[0].Path != "TaskTemplate.ContainerSpec.Secrets[0]" || need.Secrets[0].Name != "db-pass" {
		t.Errorf("secrets = %+v, want Secrets[0] db-pass", need.Secrets)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, a refused spec writes nothing", got)
	}
	if got := r.fake.DistributionRefs(); len(got) != 0 {
		t.Errorf("registry lookups = %v, the refusal comes before the image is looked up", got)
	}

	// A caller who holds secrets.reveal is not asked anything: the update carries the secret's ID.
	if _, err := r.apply(r.adminReveal, 5, editFor(t, r.adminReveal, both)); err != nil {
		t.Fatalf("a revealing caller: %v", err)
	}
	ups := r.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %d, want one", len(ups))
	}
	refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 || refs[0].SecretID != "sec-db" {
		t.Errorf("secret references = %+v, want db-pass with its ID sec-db", refs)
	}
}

// dbPassRef is how the service mounts db-pass (ID sec-db) in the fixtures that use it.
func dbPassRef() *swarm.SecretReference {
	return &swarm.SecretReference{
		SecretID: "sec-db", SecretName: "db-pass", File: &swarm.SecretReferenceFileTarget{Name: "/run/secrets/db-pass", UID: "0", GID: "0", Mode: 0o444},
	}
}

func specWith(refs ...*swarm.SecretReference) swarm.ServiceSpec {
	return swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Secrets: refs}}}
}

func TestAddedSecretsNamesOnlyTheSecretsTheServiceDoesNotUse(t *testing.T) {
	changed := dbPassRef()
	changed.File = &swarm.SecretReferenceFileTarget{Name: "/run/secrets/other", UID: "70", GID: "71", Mode: 0o400}
	byName := &swarm.SecretReference{SecretName: "db-pass", File: dbPassRef().File}
	byID := &swarm.SecretReference{SecretID: "sec-db", File: dbPassRef().File}
	apiKey := &swarm.SecretReference{SecretName: "api-key", File: &swarm.SecretReferenceFileTarget{Name: "api-key", Mode: 0o444}}
	swapped := &swarm.SecretReference{SecretID: "sec-other", SecretName: "db-pass", File: dbPassRef().File}
	second := dbPassRef()
	second.File = &swarm.SecretReferenceFileTarget{Name: "/run/secrets/db-pass-2", Mode: 0o444}
	byIDOnly := &swarm.SecretReference{SecretID: "sec-new", File: dbPassRef().File}
	anonymous := &swarm.SecretReference{File: dbPassRef().File}

	renamed := dbPassRef()
	renamed.SecretName = "victim-secret"
	victimAlone := &swarm.SecretReference{SecretName: "victim-secret", File: dbPassRef().File}
	unknown := &swarm.SecretReference{SecretName: "staged-secret", File: dbPassRef().File}
	have := []dockerx.SecretRow{{ID: "sec-db", Name: "db-pass"}, {ID: "sec-key", Name: "api-key"}, {ID: "sec-victim", Name: "victim-secret"}}

	current := specWith(dbPassRef())
	for name, c := range map[string]struct {
		current, next swarm.ServiceSpec
		want          []swarmops.AddedSecret
	}{
		"the same reference kept":                  {current, specWith(dbPassRef()), nil},
		"its file name, UID, GID and mode changed": {current, specWith(changed), nil},
		"the reference dropped":                    {current, specWith(), nil},
		"it named by name only":                    {current, specWith(byName), nil},
		"it named by ID only":                      {current, specWith(byID), nil},
		"a secret the service does not use added":  {current, specWith(dbPassRef(), apiKey), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[1]", Name: "api-key"}}},
		"the name kept with another ID":            {current, specWith(swapped), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "db-pass"}}},
		"a new secret named by ID only":            {current, specWith(byIDOnly), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "sec-new"}}},
		"a reference with neither name nor ID":     {current, specWith(anonymous), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]"}}},
		"the same secret mounted twice":            {current, specWith(dbPassRef(), second), nil},
		"a create with two references":             {swarm.ServiceSpec{}, specWith(dbPassRef(), apiKey), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "db-pass"}, {Path: "TaskTemplate.ContainerSpec.Secrets[1]", Name: "api-key"}}},
		"no container spec in the next":            {current, swarm.ServiceSpec{}, nil},
		"no container spec in the current":         {swarm.ServiceSpec{}, specWith(apiKey), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "api-key"}}},
		"no container spec on either side":         {swarm.ServiceSpec{}, swarm.ServiceSpec{}, nil},
		"a nil reference in the next is no secret": {current, specWith(nil, dbPassRef()), nil},
		"the current reference carries another secret's name and the next names that secret alone": {specWith(renamed), specWith(victimAlone), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "victim-secret"}}},
		"the used ID kept under another name is the same secret":                                   {current, specWith(renamed), nil},
		"a name the swarm does not have, with no ID":                                               {current, specWith(unknown), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "staged-secret"}}},
		"a nil reference in the current uses nothing":                                              {specWith(nil), specWith(apiKey), []swarmops.AddedSecret{{Path: "TaskTemplate.ContainerSpec.Secrets[0]", Name: "api-key"}}},
	} {
		if got := swarmops.AddedSecrets(c.current, c.next, have); !slices.Equal(got, c.want) {
			t.Errorf("%s: added = %+v, want %+v", name, got, c.want)
		}
	}
}

func withDBPassAndAPIKey(cfg *dockertest.FakeConfig) {
	withDBPass(cfg)
	cfg.Secrets = append(cfg.Secrets, dockertest.FakeSecret{ID: "sec-key", Name: "api-key"})
}

func TestEveryWritePathRefusesANewSecretForACallerWhoCannotReveal(t *testing.T) {
	needsReveal := func(t *testing.T, what string, err error, wantPath, wantName string) {
		t.Helper()
		var need *swarmops.RevealRequiredError
		if !errors.Is(err, swarmops.ErrRevealRequired) || !errors.As(err, &need) {
			t.Fatalf("%s: err = %v, want a RevealRequiredError", what, err)
		}
		if len(need.Secrets) != 1 || need.Secrets[0].Path != wantPath || need.Secrets[0].Name != wantName {
			t.Errorf("%s: secrets = %+v, want %s %s", what, need.Secrets, wantPath, wantName)
		}
	}

	t.Run("review", func(t *testing.T) {
		r := newRig(t, withDBPassAndAPIKey)
		_, err := r.review(r.operator, 5, edit(t, mountSecret("api-key")))
		needsReveal(t, "Review", err, "TaskTemplate.ContainerSpec.Secrets[0]", "api-key")
		if _, err := r.review(r.adminReveal, 5, editFor(t, r.adminReveal, mountSecret("api-key"))); err != nil {
			t.Errorf("a revealing caller: %v", err)
		}
	})

	t.Run("create and its review", func(t *testing.T) {
		o := newObjectRig(t, withDBPassAndAPIKey)
		_, err := o.reviewCreate(o.operator, chatSpec(mountSecret("db-pass")))
		needsReveal(t, "ReviewCreate", err, "TaskTemplate.ContainerSpec.Secrets[0]", "db-pass")
		_, err = o.create(o.operator, chatSpec(mountSecret("db-pass")))
		needsReveal(t, "Create", err, "TaskTemplate.ContainerSpec.Secrets[0]", "db-pass")
		if got := o.fake.Writes(); len(got) != 0 {
			t.Errorf("writes = %v, a refused create writes nothing", got)
		}
		if _, err := o.reviewCreate(o.operatorReveal, chatSpec(mountSecret("db-pass"))); err != nil {
			t.Errorf("ReviewCreate by a revealing caller: %v", err)
		}
		if _, err := o.create(o.operatorReveal, chatSpec(mountSecret("db-pass"))); err != nil {
			t.Errorf("Create by a revealing caller: %v", err)
		}
	})

	t.Run("apply with a secret staged as new", func(t *testing.T) {
		o := newObjectRig(t)
		staged := []swarmops.StagedObject{{Kind: swarmops.KindSecret, Name: "api-key", Data: []byte(secretValue)}}
		_, err := o.applyStaged(o.operator, edit(t, mountSecret("api-key")), staged)
		needsReveal(t, "Apply", err, "TaskTemplate.ContainerSpec.Secrets[0]", "api-key")
		if got := o.fake.Writes(); len(got) != 0 {
			t.Errorf("writes = %v, the staged secret is not created before the refusal", got)
		}
		neverHolds(t, "the error", err)
		if _, err := o.applyStaged(o.operatorReveal, editFor(t, o.operatorReveal, mountSecret("api-key")), staged); err != nil {
			t.Errorf("a revealing caller: %v", err)
		}
	})

	t.Run("rotate", func(t *testing.T) {
		o := newRotateRig(t)
		res, err := o.rotateSecret(o.operator)
		var stop *swarmops.RotateStopError
		if !errors.As(err, &stop) || !errors.Is(err, swarmops.ErrRevealRequired) || stop.Service != "api" {
			t.Fatalf("err = %v, want the rotate stopped at api for want of secrets.reveal", err)
		}
		if res.FailedAt != "api" || res.Services[0].Code != "secret_reveal_required" {
			t.Errorf("result = %+v, want it stopped at api with code secret_reveal_required", res)
		}
		if ups := o.fake.ServiceUpdates(); len(ups) != 0 {
			t.Errorf("updates = %+v, the old reference stays in every service", ups)
		}
		if res.OldRemoved {
			t.Error("the old secret was removed")
		}
		o = newRotateRig(t)
		if _, err := o.rotateSecret(o.operatorReveal); err != nil {
			t.Errorf("a revealing caller: %v", err)
		}
	})
}

func TestApplyByACallerWhoCannotRevealKeepingItsSecretsAndChangingReplicasGoesThrough(t *testing.T) {
	usesDBPass := func(cfg *dockertest.FakeConfig) {
		withDBPass(cfg)
		cs := cfg.ServiceRows[0].Spec.TaskTemplate.ContainerSpec
		cs.Secrets = append(cs.Secrets, dbPassRef())
	}
	// The editor's spec is the service's, masked, with the secret it already holds.
	editOwn := func(t *testing.T, mut func(*swarm.ServiceSpec)) json.RawMessage {
		t.Helper()
		base := apiSpec()
		base.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{dbPassRef()}
		masked, _ := redact.MaskServiceSpec(base, false)
		spec := clone(t, masked)
		mut(&spec)
		b, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, c := range map[string]struct {
		mut   func(*swarm.ServiceSpec)
		check func(t *testing.T, spec swarm.ServiceSpec)
	}{
		"the replicas changed": {
			func(s *swarm.ServiceSpec) { n := uint64(4); s.Mode.Replicated.Replicas = &n },
			func(t *testing.T, spec swarm.ServiceSpec) {
				if r := spec.Mode.Replicated; r == nil || r.Replicas == nil || *r.Replicas != 4 {
					t.Errorf("mode = %+v, want four replicas", spec.Mode)
				}
			},
		},
		"the secret's mode changed": {
			func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Secrets[0].File.Mode = 0o400 },
			func(t *testing.T, spec swarm.ServiceSpec) {
				refs := spec.TaskTemplate.ContainerSpec.Secrets
				if len(refs) != 1 || refs[0].SecretID != "sec-db" || refs[0].File.Mode != 0o400 {
					t.Errorf("secrets = %+v, want db-pass with mode 400", refs)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, usesDBPass)
			if _, err := r.apply(r.operator, 5, editOwn(t, c.mut)); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			ups := r.fake.ServiceUpdates()
			if len(ups) != 1 {
				t.Fatalf("updates = %d, want one", len(ups))
			}
			c.check(t, ups[0].Spec)
		})
	}
}
