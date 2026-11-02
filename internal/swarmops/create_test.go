// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package swarmops_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/swarmops"
)

// Create a service (plan 06-27, RES-03, D-04, D-07, D-08, D-13). The daemon is the fake one with
// injected faults; the checker is the real one over a real store.

const chatImage = "localhost:5000/app/chat:1"

// chatSpec is the smallest service the editor creates: a name, an image and a replica count.
func chatSpec(mut ...func(*swarm.ServiceSpec)) json.RawMessage {
	one := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "chat"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: chatImage}},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
	}
	for _, m := range mut {
		m(&spec)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	return b
}

func (o *objectRig) create(who swarmops.Caller, spec json.RawMessage, objects ...swarmops.StagedObject) (swarmops.CreateResult, error) {
	o.t.Helper()
	return swarmops.Create(context.Background(), o.deps, swarmops.CreateInput{
		EnvID: envID, Client: o.client, Spec: spec, Caller: who, Objects: objects,
	})
}

func (o *objectRig) reviewCreate(who swarmops.Caller, spec json.RawMessage) (swarmops.ReviewResult, error) {
	o.t.Helper()
	return swarmops.ReviewCreate(context.Background(), o.deps, swarmops.CreateInput{
		EnvID: envID, Client: o.client, Spec: spec, Caller: who,
	})
}

func TestCreateSendsOneServiceCreateWithTheImagePinnedAndNoRegistryQuery(t *testing.T) {
	o := newObjectRig(t)
	res, err := o.create(o.admin, chatSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	digest := dockertest.FakeDigestOf(chatImage)
	pinned := chatImage + "@" + digest
	if res.Name != "chat" || res.ServiceID == "" || res.Image.Ref != pinned || res.Image.Digest != digest || !res.Image.Pinned {
		t.Errorf("result = %+v, want chat pinned to %s", res, pinned)
	}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, []string{"POST services"}) {
		t.Errorf("writes = %v, want exactly one service create", got)
	}
	creates := o.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.ContainerSpec.Image != pinned {
		t.Fatalf("creates = %+v, want one carrying the pinned image", creates)
	}
	if got := creates[0].Query.Get("registryAuthFrom"); got != "" {
		t.Errorf("query = %v, want no registry query", creates[0].Query)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Step+"/"+s.State)
	}
	if !slices.Equal(steps, []string{"image/done", "service/done"}) {
		t.Errorf("steps = %v", steps)
	}
}

func TestCreateKeepsAnImageThatCarriesADigestAndAsksNoRegistry(t *testing.T) {
	o := newObjectRig(t)
	ref := chatImage + "@sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	res, err := o.create(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = ref }))
	if err != nil || res.Image.Ref != ref || res.Image.Pinned {
		t.Fatalf("Create = %+v, %v; want the digest kept and not re-pinned", res.Image, err)
	}
	if got := o.fake.DistributionRefs(); len(got) != 0 {
		t.Errorf("registry asked %v, want no lookup", got)
	}
}

func TestCreateWithoutAnImageIsRefusedBeforeAnyCall(t *testing.T) {
	o := newObjectRig(t)
	for name, mut := range map[string]func(*swarm.ServiceSpec){
		"empty":      func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = "" },
		"whitespace": func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = "  " },
		"no spec":    func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec = nil },
	} {
		if _, err := o.create(o.admin, chatSpec(mut)); !errors.Is(err, swarmops.ErrImageRequired) {
			t.Errorf("%s: err = %v, want ErrImageRequired", name, err)
		}
		if _, err := o.reviewCreate(o.admin, chatSpec(mut)); !errors.Is(err, swarmops.ErrImageRequired) {
			t.Errorf("%s review: err = %v, want ErrImageRequired", name, err)
		}
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestCreateRefusesABadName(t *testing.T) {
	o := newObjectRig(t)
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	for _, name := range []string{"", "has space", "slash/name", "unié", string(long)} {
		_, err := o.create(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.Name = name }))
		if !errors.Is(err, swarmops.ErrNameInvalid) {
			t.Errorf("name %q: err = %v, want ErrNameInvalid", name, err)
		}
	}
	for _, name := range []string{"a", "chat", "My_chat.v2-x", string(long[:63])} {
		if _, err := o.reviewCreate(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.Name = name })); err != nil {
			t.Errorf("name %q: review err = %v, want it accepted", name, err)
		}
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestCreateOfATakenNameCreatesNothing(t *testing.T) {
	o := newObjectRig(t)
	res, err := o.create(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.Name = "api" }),
		swarmops.StagedObject{Kind: swarmops.KindSecret, Name: "db-pass", Data: []byte(secretValue)})
	if !errors.Is(err, dockerx.ErrServiceNameTaken) || res.Name != "api" {
		t.Fatalf("err = %v (name %q), want ErrServiceNameTaken for api", err, res.Name)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v: the name is checked before any object is made", got)
	}
	// Review says so too, so the page can tell the person before the last step.
	if _, err := o.reviewCreate(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.Name = "api" })); !errors.Is(err, dockerx.ErrServiceNameTaken) {
		t.Errorf("review err = %v, want ErrServiceNameTaken", err)
	}
}

func TestCreateOfANameThatIsAnotherServicesIDPrefixIsNotTaken(t *testing.T) {
	o := newObjectRig(t)
	// "svc-a" is a prefix of the ID svc-api, not a name anybody holds.
	if _, err := o.create(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.Name = "svc-api" })); err != nil {
		t.Fatalf("Create: %v, want a name that equals another service's ID but not its name to be free", err)
	}
}

func TestCreateRefusesTheStackNamespaceLabels(t *testing.T) {
	o := newObjectRig(t)
	for name, mut := range map[string]func(*swarm.ServiceSpec){
		"service": func(s *swarm.ServiceSpec) { s.Labels = map[string]string{"com.docker.stack.namespace": "shop"} },
		"container": func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Labels = map[string]string{"com.docker.stack.namespace": "shop"}
		},
	} {
		if _, err := o.create(o.admin, chatSpec(mut)); !errors.Is(err, swarmops.ErrStackLabel) {
			t.Errorf("%s label: err = %v, want ErrStackLabel", name, err)
		}
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestCreateRefusesASpecThatDoesNotDecodeStrictly(t *testing.T) {
	o := newObjectRig(t)
	for _, raw := range []string{`{"Name":"chat","Bogus":1}`, `{"Name":5}`, `{"Name":"chat"} {}`, ``} {
		_, err := o.create(o.admin, json.RawMessage(raw))
		var invalid *swarmops.SpecInvalidError
		if !errors.Is(err, swarmops.ErrSpecInvalid) || !errors.As(err, &invalid) {
			t.Errorf("%q: err = %v, want SpecInvalidError", raw, err)
		}
	}
}

func TestCreateRefusesAHiddenPlaceholderWhoeverSendsIt(t *testing.T) {
	o := newObjectRig(t)
	for name, who := range map[string]swarmops.Caller{"admin": o.admin, "revealing": {Principal: o.admin.Principal, Reveal: true}} {
		_, err := o.create(who, chatSpec(func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Env = []string{"DB=" + redact.Placeholder} }))
		var ph *redact.PlaceholderError
		if !errors.As(err, &ph) || !errors.Is(err, redact.ErrPlaceholderUnmatched) {
			t.Errorf("%s: err = %v, want an unmatched placeholder", name, err)
		}
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestCreateRunsTheHostEscalationPolicyOnTheFullSpec(t *testing.T) {
	o := newObjectRig(t)
	sock := func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/sock"}}
	}
	_, err := o.create(o.operator, chatSpec(sock))
	var refused *swarmops.RefusedError
	if !errors.Is(err, swarmops.ErrRefused) || !errors.As(err, &refused) || len(refused.Findings) != 1 {
		t.Fatalf("err = %v, want a RefusedError with one finding", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v: a refused spec creates nothing", got)
	}
	rev, err := o.reviewCreate(o.operator, chatSpec(sock))
	if err != nil || rev.Allowed || len(rev.Findings) != 1 || rev.Findings[0].Rule != "bind_path" {
		t.Fatalf("review = allowed %v findings %v, %v; want not allowed with the bind finding", rev.Allowed, rev.Findings, err)
	}
	if _, err := o.create(o.admin, chatSpec(sock)); err != nil {
		t.Errorf("an Env Admin may have host access: %v", err)
	}
}

func TestCreateOfAnUnresolvableImageSendsNothing(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: authentication required"}
	})
	_, err := o.create(o.admin, chatSpec(), swarmops.StagedObject{Kind: swarmops.KindSecret, Name: "db-pass", Data: []byte(secretValue)})
	var unresolved *swarmops.ImageUnresolvedError
	if !errors.Is(err, swarmops.ErrImageUnresolved) || !errors.As(err, &unresolved) || unresolved.Reason != dockerx.PullCodeUnauthorized {
		t.Fatalf("err = %v, want ImageUnresolvedError", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, an unresolved tag is never sent unpinned", got)
	}
	rev, err := o.reviewCreate(o.admin, chatSpec())
	if err != nil || rev.ImageError == nil || rev.ImageError.Host != "localhost:5000" || rev.Image.Pinned {
		t.Errorf("review = %+v, %v; want the lookup failure beside an unpinned spec", rev.ImageError, err)
	}
}

func TestReviewCreateAnswersTheNormalisedSpecAndWritesNothing(t *testing.T) {
	o := newObjectRig(t)
	rev, err := o.reviewCreate(o.admin, chatSpec(func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Env = []string{"DB=" + dbURL} }))
	if err != nil {
		t.Fatalf("ReviewCreate: %v", err)
	}
	pinned := chatImage + "@" + dockertest.FakeDigestOf(chatImage)
	if !rev.Allowed || !rev.Changed || rev.Image.Ref != pinned || rev.Spec.Name != "chat" || rev.Spec.TaskTemplate.ContainerSpec.Image != pinned {
		t.Errorf("review = %+v", rev)
	}
	// The caller may not reveal: the values of the spec are masked in the answer, as on an edit.
	if env := rev.Spec.TaskTemplate.ContainerSpec.Env; env != nil {
		for _, e := range env {
			if e == "DB="+dbURL {
				t.Errorf("env %v holds a value the caller may not see", env)
			}
		}
	}
	if rev.Warnings == nil || rev.Findings == nil {
		t.Errorf("warnings %v findings %v, want empty lists and not null", rev.Warnings, rev.Findings)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, a review changes nothing", got)
	}
}

func TestReviewCreateListsTheObjectsTheSpecNamesThatDoNotExist(t *testing.T) {
	o := newObjectRig(t)
	rev, err := o.reviewCreate(o.adminReveal, chatSpec(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "front"}, {Target: "app_net"}}
		s.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{SecretName: "db-pass", File: &swarm.SecretReferenceFileTarget{Name: "db-pass", Mode: 0o444}}}
		s.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{ConfigName: "app.conf", File: &swarm.ConfigReferenceFileTarget{Name: "/app.conf", Mode: 0o444}}}
	}))
	if err != nil {
		t.Fatalf("ReviewCreate: %v", err)
	}
	if !slices.Equal(rev.Missing.Networks, []string{"front"}) || !slices.Equal(rev.Missing.Secrets, []string{"db-pass"}) || !slices.Equal(rev.Missing.Configs, []string{"app.conf"}) {
		t.Errorf("missing = %+v, want front, db-pass and app.conf (app_net exists)", rev.Missing)
	}
}

func TestCreateMakesTheStagedObjectsFirstInOrderAndPutsTheirIDsInTheSpec(t *testing.T) {
	o := newObjectRig(t)
	res, err := o.create(o.adminReveal, chatSpec(func(s *swarm.ServiceSpec) {
		withRefs(s)
	}), staged()...)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []string{"POST networks", "POST secrets", "POST configs", "POST services"}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	creates := o.fake.ServiceCreates()
	cs := creates[0].Spec.TaskTemplate.ContainerSpec
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretID == "" || len(cs.Configs) != 1 || cs.Configs[0].ConfigID == "" {
		t.Errorf("the spec the daemon received = %+v, want the IDs of the new secret and config", cs)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Step+"/"+s.Kind+"/"+s.State)
	}
	if want := []string{"image//done", "object/network/done", "object/secret/done", "object/config/done", "service//done"}; !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
	if len(o.rows) != 3 {
		t.Errorf("audit rows = %d, want one per object made", len(o.rows))
	}
	neverHolds(t, "the result", res)
	for _, r := range o.rows {
		neverHolds(t, "an audit row", r)
	}
}

func TestCreateRemovesWhatItMadeWhenTheServiceCreateFails(t *testing.T) {
	o := newObjectRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the manager refuses this spec" })
	res, err := o.create(o.adminReveal, chatSpec(withRefs), staged()...)
	var rej *swarmops.RejectedError
	if !errors.As(err, &rej) || !errors.Is(err, swarmops.ErrRejected) {
		t.Fatalf("err = %v, want RejectedError", err)
	}
	want := []string{"POST networks", "POST secrets", "POST configs", "POST services", "DELETE configs", "DELETE secrets", "DELETE networks"}
	if got := writeKinds(o.fake.Writes()); !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	if len(res.Cleanup.Removed) != 3 || len(res.Cleanup.NotRemoved) != 0 {
		t.Errorf("cleanup = %+v, want all three removed", res.Cleanup)
	}
	if left := o.leftovers(); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	last := res.Steps[len(res.Steps)-1]
	if last.Step != "service" || last.State != "failed" || last.Code != "spec_rejected" {
		t.Errorf("service step = %+v, want failed spec_rejected", last)
	}
	neverHolds(t, "the error", err)
}

func TestCreateWithAnObjectThatIsMissingAndNotStagedIsRefusedBeforeAnyWrite(t *testing.T) {
	o := newObjectRig(t)
	_, err := o.create(o.adminReveal, chatSpec(withRefs))
	var missing *swarmops.MissingError
	if !errors.Is(err, swarmops.ErrObjectsMissing) || !errors.As(err, &missing) || !slices.Equal(missing.Missing.Secrets, []string{"db-pass"}) {
		t.Fatalf("err = %v, want MissingError naming db-pass", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}

func TestCreateObjectPermissionIsCheckedBeforeAnythingIsMade(t *testing.T) {
	o := newObjectRig(t)
	o.deps.CanCreate = func(kind string) bool { return kind == swarmops.KindNetwork }
	_, err := o.create(o.adminReveal, chatSpec(withRefs), staged()...)
	var perm *swarmops.ObjectPermissionError
	if !errors.Is(err, swarmops.ErrObjectPermission) || !errors.As(err, &perm) || !slices.Equal(perm.Kinds, []string{"secret", "config"}) {
		t.Fatalf("err = %v, want ObjectPermissionError for secret and config", err)
	}
	if got := o.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v", got)
	}
}
