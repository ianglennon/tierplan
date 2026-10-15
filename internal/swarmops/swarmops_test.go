// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

// Review and Apply (plan 06-18, RES-03, D-03, D-08, D-09, D-13). The daemon is the fake one with
// injected faults; the checker is the real one over a real store.

const (
	envID    = "env-a"
	apiImage = "localhost:5000/app/api:1"
	dbURL    = "postgres://user:hunter2@db/app" //nolint:gosec // G101: a fixture secret the tests look for in every answer
)

// apiSpec is a standalone service whose spec holds a secret in each place a spec can: its
// environment, its command line, its health check and its log options.
func apiSpec() swarm.ServiceSpec {
	replicas := uint64(2)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "api", Labels: map[string]string{"team": "core"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:       apiImage,
				Env:         []string{"DB=" + dbURL, "LEVEL=info"},
				Command:     []string{"api", "--admin-token=s3cr3t-token"},
				Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", "probe --key=hc-secret-key"}},
			},
			LogDriver: &swarm.Driver{Name: "gelf", Options: map[string]string{"gelf-address": "udp://logtoken-123@h:12201"}},
			Networks:  []swarm.NetworkAttachmentConfig{{Target: "app_net"}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
}

type rig struct {
	t        *testing.T
	fake     *dockertest.Fake
	client   *dockerx.Client
	deps     swarmops.Deps
	operator swarmops.Caller
	admin    swarmops.Caller
}

type cfgFn func(*dockertest.FakeConfig)

func newRig(t *testing.T, tune ...cfgFn) *rig {
	t.Helper()
	spec := apiSpec()
	cfg := dockertest.FakeConfig{
		Hostname: "mgr-1", Version: "29.1.0", SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "svc-api", Name: "api", Mode: "replicated", Running: 2, Desired: 2, Version: 5, Spec: &spec},
			{ID: "svc-shop", Name: "shop_web", Mode: "replicated", Running: 1, Desired: 1, Version: 3, Image: "shop:1",
				Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "svc-glob", Name: "node-agent", Mode: "global", Running: 3, Desired: 3, Version: 2, Image: "agent:1"},
		},
		Networks: []dockertest.FakeNetwork{
			{ID: "ovl-app-0123456789", Name: "app_net", Driver: "overlay", Scope: "swarm"},
			{ID: "hostnetid0123456789", Name: "host", Driver: "host", Scope: "local"},
			{ID: "bridgeid", Name: "bridge", Driver: "bridge", Scope: "local"},
		},
	}
	for _, fn := range tune {
		fn(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	c, err := dockerx.New(fake)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	st := storetest.New(t)
	for _, u := range []string{"operator", "envadmin"} {
		if _, err := st.Write.ExecContext(context.Background(),
			`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, u, u); err != nil {
			t.Fatal(err)
		}
	}
	for i, ra := range [][2]string{{"operator", "operator"}, {"envadmin", "admin"}} {
		if _, err := st.Write.ExecContext(context.Background(),
			`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, 'user', ?, ?, ?, ?)`,
			"ra-"+ra[0], ra[0], ra[1], envID, i+1); err != nil {
			t.Fatal(err)
		}
	}
	who := func(id string) swarmops.Caller {
		return swarmops.Caller{Principal: &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id}}
	}
	return &rig{t: t, fake: fake, client: c, deps: swarmops.Deps{Checker: rbac.NewChecker(st)}, operator: who("operator"), admin: who("envadmin")}
}

func clone(t *testing.T, spec swarm.ServiceSpec) swarm.ServiceSpec {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var out swarm.ServiceSpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// edit is what a caller without secrets.reveal sends: the masked spec, changed by mut.
func edit(t *testing.T, mut func(*swarm.ServiceSpec)) json.RawMessage {
	t.Helper()
	masked, _ := redact.MaskServiceSpec(apiSpec(), false)
	spec := clone(t, masked)
	if mut != nil {
		mut(&spec)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *rig) review(who swarmops.Caller, version uint64, spec json.RawMessage) (swarmops.ReviewResult, error) {
	r.t.Helper()
	return swarmops.Review(context.Background(), r.deps, swarmops.ReviewInput{
		EnvID: envID, Client: r.client, ServiceID: "svc-api", Version: version, Spec: spec, Caller: who,
	})
}

func (r *rig) apply(who swarmops.Caller, version uint64, spec json.RawMessage) (swarmops.ApplyResult, error) {
	r.t.Helper()
	return swarmops.Apply(context.Background(), r.deps, swarmops.ApplyInput{
		EnvID: envID, Client: r.client, ServiceID: "svc-api", Version: version, Spec: spec, Caller: who,
	})
}

func newTagAndEnv(spec *swarm.ServiceSpec) {
	cs := spec.TaskTemplate.ContainerSpec
	cs.Image = "localhost:5000/app/api:2"
	cs.Env = []string{"DB=<hidden>", "LEVEL=debug"}
}

func TestReviewAnswersTheMergedMaskedSpecAndChangesNothing(t *testing.T) {
	r := newRig(t)
	res, err := r.review(r.operator, 5, edit(t, newTagAndEnv))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	cs := res.Spec.TaskTemplate.ContainerSpec
	if !res.Changed || !res.Allowed || res.Version != 5 || len(res.Findings) != 0 {
		t.Fatalf("result = changed %v allowed %v version %d findings %v", res.Changed, res.Allowed, res.Version, res.Findings)
	}
	// The caller may not reveal: the answer is the masked merged spec.
	if !slices.Equal(cs.Env, []string{"DB=<hidden>", "LEVEL=<hidden>"}) || !slices.Equal(cs.Command, []string{"<hidden>"}) {
		t.Errorf("the answer is not masked: env %v command %v", cs.Env, cs.Command)
	}
	pinned := "localhost:5000/app/api:2@" + dockertest.FakeDigestOf("localhost:5000/app/api:2")
	if res.Image.Ref != pinned || res.Image.Tag != "2" || res.Image.Digest != dockertest.FakeDigestOf("localhost:5000/app/api:2") ||
		res.Image.Host != "localhost:5000" || !res.Image.Changed || res.ImageError != nil {
		t.Errorf("image = %+v (error %+v), want %s pinned", res.Image, res.ImageError, pinned)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, a review changes nothing", got)
	}
	if got := r.fake.ServiceUpdates(); len(got) != 0 {
		t.Errorf("updates = %v", got)
	}
	if len(res.Missing.Networks)+len(res.Missing.Secrets)+len(res.Missing.Configs) != 0 || res.Warnings == nil {
		t.Errorf("missing = %+v, warnings %v", res.Missing, res.Warnings)
	}
}

func TestReviewOfAnUntouchedMaskedSpecHasNothingChanged(t *testing.T) {
	r := newRig(t)
	res, err := r.review(r.operator, 5, edit(t, nil))
	if err != nil || res.Changed || res.Image.Changed {
		t.Fatalf("Review = changed %v image changed %v, %v; want nothing changed", res.Changed, res.Image.Changed, err)
	}
	if res.Image.Ref != apiImage {
		t.Errorf("image = %q, want the stored one", res.Image.Ref)
	}
}

func TestReviewNamesTheFindingsAndRefusesAnOperatorWhileAnAdminMayHaveThem(t *testing.T) {
	r := newRig(t)
	sock := edit(t, func(s *swarm.ServiceSpec) {
		cs := s.TaskTemplate.ContainerSpec
		cs.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/sock"}}
		cs.CapabilityAdd = []string{"NET_ADMIN"}
	})
	res, err := r.review(r.operator, 5, sock)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Allowed || res.Reason != rbac.ReasonPrivilegedService || len(res.Findings) != 2 ||
		!strings.Contains(res.Refusal, "TaskTemplate.ContainerSpec.Mounts[0].Source: /var/run/docker.sock") ||
		!strings.Contains(res.Refusal, "TaskTemplate.ContainerSpec.CapabilityAdd[0]: NET_ADMIN") {
		t.Fatalf("operator: allowed %v reason %q findings %d refusal %q", res.Allowed, res.Reason, len(res.Findings), res.Refusal)
	}
	res, err = r.review(r.admin, 5, sock)
	if err != nil || !res.Allowed || len(res.Findings) != 2 || res.Refusal != "" {
		t.Fatalf("admin: %+v, %v; want allowed with the findings returned", res, err)
	}
}

func TestReviewSeesTheHostNetworkBehindAnIDAndListsMissingNetworks(t *testing.T) {
	r := newRig(t)
	hostByID := edit(t, func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "hostnetid0123456789"}}
	})
	res, err := r.review(r.operator, 5, hostByID)
	if err != nil || res.Allowed || len(res.Findings) != 1 || res.Findings[0].Rule != "host_network" {
		t.Fatalf("host network by id: %+v, %v", res, err)
	}
	gone := edit(t, func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "app_net"}, {Target: "front"}, {Target: "ovl-app-0123"}}
	})
	res, err = r.review(r.operator, 5, gone)
	if err != nil || !slices.Equal(res.Missing.Networks, []string{"front"}) {
		t.Fatalf("missing = %+v, %v; want only front (app_net by name and ovl-app-0123 by ID prefix exist)", res.Missing, err)
	}
}

func TestReviewReportsAFailedDigestLookupAndStillAnswersTheUnpinnedSpec(t *testing.T) {
	r := newRig(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: authentication required"}
	})
	res, err := r.review(r.operator, 5, edit(t, newTagAndEnv))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.ImageError == nil || res.ImageError.Ref != "localhost:5000/app/api:2" || res.ImageError.Host != "localhost:5000" || res.ImageError.Reason != dockerx.PullCodeUnauthorized {
		t.Fatalf("image error = %+v", res.ImageError)
	}
	if res.Image.Ref != "localhost:5000/app/api:2" || res.Image.Digest != "" {
		t.Errorf("image = %+v, want the tag unpinned", res.Image)
	}
}

func TestReviewAndApplyRefuseAVersionThatIsNotTheCurrentOne(t *testing.T) {
	r := newRig(t)
	if _, err := r.review(r.operator, 4, edit(t, newTagAndEnv)); !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Errorf("Review with a stale version: err = %v, want ErrServiceChanged", err)
	}
	if _, err := r.apply(r.operator, 4, edit(t, newTagAndEnv)); !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Errorf("Apply with a stale version: err = %v, want ErrServiceChanged", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, a stale version is refused before any write", got)
	}
}

func TestApplyOfASpecEqualToTheCurrentOneIsNothingToApplyAndWritesNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.apply(r.operator, 5, edit(t, nil))
	if !errors.Is(err, swarmops.ErrNothingToApply) {
		t.Fatalf("err = %v, want ErrNothingToApply", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestApplyPinsAChangedTagAndKeepsTheStoredPinOfAnUnchangedImage(t *testing.T) {
	r := newRig(t)
	res, err := r.apply(r.operator, 5, edit(t, newTagAndEnv))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	digest := dockertest.FakeDigestOf("localhost:5000/app/api:2")
	pinned := "localhost:5000/app/api:2@" + digest
	ups := r.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].Version != "5" || ups[0].Spec.TaskTemplate.ContainerSpec.Image != pinned || ups[0].Query.Get("registryAuthFrom") != "" {
		t.Fatalf("updates = %+v, want one at version 5 with %s", ups, pinned)
	}
	if res.Image.Ref != pinned || res.Image.Digest != digest || !res.Image.Changed || res.Name != "api" || res.ServiceID != "svc-api" {
		t.Errorf("result = %+v", res)
	}
	if refs := r.fake.DistributionRefs(); !slices.Equal(refs, []string{"localhost:5000/app/api:2"}) {
		t.Errorf("the registry was asked for %v, want the new tag once", refs)
	}

	// The image is now pinned. An edit that leaves it alone keeps the stored tag@digest and asks no registry.
	spec := clone(t, ups[0].Spec)
	spec.TaskTemplate.ContainerSpec.Env = []string{"DB=" + dbURL, "LEVEL=warn"}
	spec.TaskTemplate.ContainerSpec.Command = []string{"api"}
	spec.TaskTemplate.ContainerSpec.Healthcheck = nil
	spec.TaskTemplate.LogDriver = nil
	raw, _ := json.Marshal(spec)
	asked := len(r.fake.DistributionRefs())
	res, err = swarmops.Apply(context.Background(), r.deps, swarmops.ApplyInput{
		EnvID: envID, Client: r.client, ServiceID: "svc-api", Version: 6, Spec: raw, Caller: r.admin,
	})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if res.Image.Changed || res.Image.Ref != pinned || len(r.fake.DistributionRefs()) != asked {
		t.Errorf("image = %+v, registry asked %v; want the stored pin kept and no lookup", res.Image, r.fake.DistributionRefs())
	}
}

func TestApplyAFailedDigestLookupSendsNothing(t *testing.T) {
	r := newRig(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: authentication required"}
	})
	_, err := r.apply(r.operator, 5, edit(t, newTagAndEnv))
	var unresolved *swarmops.ImageUnresolvedError
	if !errors.Is(err, swarmops.ErrImageUnresolved) || !errors.As(err, &unresolved) || unresolved.Host != "localhost:5000" || unresolved.Reason != dockerx.PullCodeUnauthorized {
		t.Fatalf("err = %v, want ImageUnresolvedError", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, an unresolved tag is never sent unpinned", got)
	}
}

func TestApplyMergesTheHiddenValuesBackAndTheDaemonReceivesTheRealOnes(t *testing.T) {
	r := newRig(t)
	if _, err := r.apply(r.operator, 5, edit(t, newTagAndEnv)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ups := r.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %d", len(ups))
	}
	got := ups[0].Spec.TaskTemplate
	cs := got.ContainerSpec
	if !slices.Equal(cs.Env, []string{"DB=" + dbURL, "LEVEL=debug"}) {
		t.Errorf("env = %v, want the stored DB value back and the typed LEVEL", cs.Env)
	}
	if !slices.Equal(cs.Command, []string{"api", "--admin-token=s3cr3t-token"}) || !slices.Equal(cs.Healthcheck.Test, []string{"CMD-SHELL", "probe --key=hc-secret-key"}) ||
		got.LogDriver.Options["gelf-address"] != "udp://logtoken-123@h:12201" {
		t.Errorf("the command, health check or log option was not merged back: %+v", got)
	}
}

func TestApplyRefusesAnEscalatingSpecForAnOperatorWithoutAWriteAndSendsItForAnAdmin(t *testing.T) {
	r := newRig(t)
	sock := edit(t, func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/sock"}}
	})
	res, err := r.apply(r.operator, 5, sock)
	var refused *swarmops.RefusedError
	if !errors.Is(err, swarmops.ErrRefused) || !errors.As(err, &refused) || refused.Reason != rbac.ReasonPrivilegedService || len(refused.Findings) != 1 {
		t.Fatalf("err = %v, want a privileged_service RefusedError", err)
	}
	if len(res.Findings) != 1 {
		t.Errorf("the result lost the findings for the audit: %+v", res)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Fatalf("writes = %v, a refused spec is never sent", got)
	}
	if _, err := r.apply(r.admin, 5, sock); err != nil {
		t.Fatalf("admin Apply: %v", err)
	}
	if ups := r.fake.ServiceUpdates(); len(ups) != 1 || ups[0].Spec.TaskTemplate.ContainerSpec.Mounts[0].Source != "/var/run/docker.sock" {
		t.Errorf("updates = %+v", ups)
	}
}

func TestApplyRefusesWhatCannotChangeAndWhatIsReserved(t *testing.T) {
	r := newRig(t)
	cases := map[string]struct {
		mut  func(*swarm.ServiceSpec)
		want error
	}{
		"a new name": {func(s *swarm.ServiceSpec) { s.Name = "api2" }, swarmops.ErrNameImmutable},
		"a global mode": {func(s *swarm.ServiceSpec) {
			s.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
		}, swarmops.ErrModeImmutable},
		"a stack label on the service": {func(s *swarm.ServiceSpec) { s.Labels["com.docker.stack.namespace"] = "shop" }, swarmops.ErrStackLabel},
		"a stack image label on the container": {func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Labels = map[string]string{"com.docker.stack.image": "x"}
		}, swarmops.ErrStackLabel},
		"a hidden variable that does not exist": {func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Env = append(s.TaskTemplate.ContainerSpec.Env, "GHOST=<hidden>")
		}, redact.ErrPlaceholderUnmatched},
	}
	for name, c := range cases {
		_, err := r.apply(r.operator, 5, edit(t, c.mut))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// A caller who may reveal secrets cannot send the placeholder as a value.
	reveal := swarmops.Caller{Principal: r.admin.Principal, Reveal: true}
	unmasked := apiSpec()
	unmasked.TaskTemplate.ContainerSpec.Env = []string{"DB=<hidden>"}
	raw, _ := json.Marshal(unmasked)
	if _, err := r.apply(reveal, 5, raw); !errors.Is(err, redact.ErrPlaceholderReserved) {
		t.Errorf("reveal with a placeholder: err = %v, want ErrPlaceholderReserved", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, none of these is ever sent", got)
	}
}

func TestApplyRefusesASpecThatDoesNotDecodeStrictlyForEveryone(t *testing.T) {
	r := newRig(t)
	for name, raw := range map[string]string{"unknown field": `{"Name":"api","Bogus":true}`, "wrong type": `{"Name":3}`, "not json": `{`} {
		for who, c := range map[string]swarmops.Caller{"operator": r.operator, "admin": r.admin} {
			_, err := r.apply(c, 5, json.RawMessage(raw))
			var bad *swarmops.SpecInvalidError
			if !errors.Is(err, swarmops.ErrSpecInvalid) || !errors.As(err, &bad) || bad.Message == "" {
				t.Errorf("%s by %s: err = %v, want SpecInvalidError with the decoder's message", name, who, err)
			}
		}
	}
}

func TestApplyOfAStackOwnedServiceIsRefused(t *testing.T) {
	r := newRig(t)
	spec := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "shop_web", Labels: map[string]string{"com.docker.stack.namespace": "shop"}}}
	raw, _ := json.Marshal(spec)
	_, err := swarmops.Apply(context.Background(), r.deps, swarmops.ApplyInput{
		EnvID: envID, Client: r.client, ServiceID: "svc-shop", Version: 3, Spec: raw, Caller: r.admin,
	})
	var owned *swarmops.StackOwnedError
	if !errors.Is(err, swarmops.ErrStackOwned) || !errors.As(err, &owned) || owned.Stack != "shop" {
		t.Fatalf("err = %v, want StackOwnedError for shop", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestApplyWithAMissingNetworkSendsNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.apply(r.operator, 5, edit(t, func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = append(s.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: "front"})
	}))
	var missing *swarmops.MissingError
	if !errors.Is(err, swarmops.ErrObjectsMissing) || !errors.As(err, &missing) || !slices.Equal(missing.Missing.Networks, []string{"front"}) {
		t.Fatalf("err = %v, want MissingError for front", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestApplyTheSameVersionTwiceMakesTheSecondServiceChanged(t *testing.T) {
	r := newRig(t)
	spec := edit(t, newTagAndEnv)
	if _, err := r.apply(r.operator, 5, spec); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := r.apply(r.operator, 5, spec); !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("second Apply: err = %v, want ErrServiceChanged", err)
	}
	if got := len(r.fake.ServiceUpdates()); got != 1 {
		t.Errorf("updates = %d, want exactly one", got)
	}
}

func TestApplyRacedByAnotherWriterIsServiceChangedAndNotRetried(t *testing.T) {
	r := newRig(t, func(c *dockertest.FakeConfig) { c.WriteDelay = 50 * time.Millisecond })
	done := make(chan error, 1)
	go func() {
		_, err := r.apply(r.operator, 5, edit(t, newTagAndEnv))
		done <- err
	}()
	// The update is on its way (the fake holds it); somebody else changes the service now.
	deadline := time.Now().Add(5 * time.Second)
	for len(r.fake.Writes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.fake.BumpService("svc-api")
	if err := <-done; !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("err = %v, want ErrServiceChanged", err)
	}
	if got := len(r.fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: a refused write is never retried", got)
	}
}

func TestApplyDockersRefusalKeepsItsWordsButNotTheValuesOfASpecTheCallerCannotSee(t *testing.T) {
	r := newRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceRows[0].UpdateError = "invalid env " + dbURL + " (secret not found: db_password) and --admin-token=s3cr3t-token"
	})
	_, err := r.apply(r.operator, 5, edit(t, newTagAndEnv))
	var rej *swarmops.RejectedError
	if !errors.Is(err, swarmops.ErrRejected) || !errors.As(err, &rej) {
		t.Fatalf("err = %v, want RejectedError", err)
	}
	for _, secret := range []string{"hunter2", "s3cr3t-token"} {
		if strings.Contains(rej.Message, secret) || strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal %q / %q holds %q", rej.Message, err.Error(), secret)
		}
	}
	if !strings.Contains(rej.Message, "secret not found: db_password") {
		t.Errorf("the refusal lost the manager's words: %q", rej.Message)
	}
	// A caller who may reveal the values reads Docker's words as they are.
	reveal := swarmops.Caller{Principal: r.admin.Principal, Reveal: true}
	unmasked := apiSpec()
	unmasked.TaskTemplate.ContainerSpec.Image = "localhost:5000/app/api:2"
	raw, _ := json.Marshal(unmasked)
	_, err = swarmops.Apply(context.Background(), r.deps, swarmops.ApplyInput{
		EnvID: envID, Client: r.client, ServiceID: "svc-api", Version: 5, Spec: raw, Caller: reveal,
	})
	if !errors.As(err, &rej) || !strings.Contains(rej.Message, "hunter2") {
		t.Errorf("reveal: err = %v, want Docker's words unscrubbed", err)
	}
}

type fixedAuth struct {
	header string
	seen   []string
}

func (a *fixedAuth) AuthFor(_ context.Context, _ string, ref registry.Ref) (string, *registry.Candidate, error) {
	a.seen = append(a.seen, ref.String())
	return a.header, &registry.Candidate{ID: "cred-1", Name: "local-bot"}, nil
}

func TestApplySendsTheCredentialHeaderAndTheReviewNamesTheCredentialNeverItsSecret(t *testing.T) {
	r := newRig(t)
	auth := &fixedAuth{header: "Zm9vYmFy"}
	r.deps.Resolver = auth
	rev, err := r.review(r.operator, 5, edit(t, newTagAndEnv))
	if err != nil || rev.Image.Credential != "local-bot" {
		t.Fatalf("Review credential = %q, %v; want the credential's name", rev.Image.Credential, err)
	}
	if _, err := r.apply(r.operator, 5, edit(t, newTagAndEnv)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ups := r.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].Auth != "Zm9vYmFy" {
		t.Fatalf("updates = %+v, want the credential header", ups)
	}
	if auths := r.fake.DistributionAuth(); len(auths) == 0 || auths[len(auths)-1] != "Zm9vYmFy" {
		t.Errorf("the digest lookup carried %v, want the same credential", auths)
	}
}

func TestEveryAnswerAndErrorOfAFixtureWithSecretsHoldsNoMaskedValueForACallerWhoCannotRevealThem(t *testing.T) {
	secrets := []string{"hunter2", "s3cr3t-token", "hc-secret-key", "logtoken-123"}
	r := newRig(t)
	seen := func(what string, v any) {
		b, _ := json.Marshal(v)
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("%s holds %q: %s", what, s, b)
			}
		}
	}
	res, err := r.review(r.operator, 5, edit(t, newTagAndEnv))
	if err != nil {
		t.Fatal(err)
	}
	seen("the review", res)
	for name, mut := range map[string]func(*swarm.ServiceSpec){
		"a rename":      func(s *swarm.ServiceSpec) { s.Name = "other" },
		"a ghost value": func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Env = []string{"X=<hidden>"} },
		"a missing net": func(s *swarm.ServiceSpec) {
			s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "front"}}
		},
		"a bind": func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/", Target: "/h"}}
		},
	} {
		ar, err := r.apply(r.operator, 5, edit(t, mut))
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		seen(name+" error", err.Error())
		seen(name+" result", ar)
	}
}
