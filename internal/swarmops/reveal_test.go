// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops_test

import (
	"errors"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
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
		newTagAndEnv(s)
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
