// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stackops/stackopstest"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/internal/vault"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// Adoption (plan 08-13, STK-13, D-14): the commit reads what runs, rebuilds the definition itself and stores it as
// the stack's draft and first revision, and sends nothing to the engine.

func adoptRepo(t *testing.T, rig *swarmRig) stackops.Repo {
	t.Helper()
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	return stackops.Repo{Store: rig.store, Vault: v}
}

// adopt reads the fingerprint of the stack now and commits the adoption with it, as the page and the route do.
func (r *swarmRig) adopt(t *testing.T, repo stackops.Repo, target stacks.Target, name string, self stackops.SelfMarks) (stackops.Stack, error) {
	t.Helper()
	ctx := testsupport.Context(t)
	fp, err := r.deps.AdoptFingerprint(ctx, r.envID, target, name)
	if err != nil {
		t.Fatalf("AdoptFingerprint: %v", err)
	}
	return r.adoptWith(t, repo, target, name, fp, self)
}

func (r *swarmRig) adoptWith(t *testing.T, repo stackops.Repo, target stacks.Target, name, fingerprint string, self stackops.SelfMarks) (stackops.Stack, error) {
	t.Helper()
	ctx := testsupport.Context(t)
	managed, err := repo.Stacks(ctx, r.envID)
	if err != nil {
		t.Fatal(err)
	}
	return r.deps.AdoptCommit(ctx, repo, stackops.AdoptInput{
		EnvID: r.envID, Name: name, Target: target, Fingerprint: fingerprint,
		Caller: stackops.Caller{Actor: "ada"}, Managed: managed, Self: self,
	})
}

func TestAdoptCommitStoresRevisionOne(t *testing.T) {
	t.Parallel()
	t.Run("swarm", func(t *testing.T) {
		t.Parallel()
		rig := objectsRig(t)
		repo := adoptRepo(t, rig)
		ctx := testsupport.Context(t)

		st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
		if err != nil {
			t.Fatalf("AdoptCommit: %v", err)
		}

		if st.Source.Kind != stacks.SourceAdopted || st.Source.Ref != "shop" || st.Name != "shop" || st.CreatedBy != "ada" || !st.Adoption.Pending {
			t.Errorf("stack = %+v, want adopted from shop by ada, pending its first deploy", st)
		}
		rev, err := repo.Revision(ctx, st.ID, 1)
		if err != nil {
			t.Fatalf("Revision 1: %v", err)
		}
		if rev.Origin != stackops.OriginAdopted || rev.Outcome != stackops.OutcomeGood {
			t.Errorf("revision 1 = origin %q outcome %q, want adopted and good", rev.Origin, rev.Outcome)
		}
		pins, err := repo.RevisionPins(ctx, rev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := pins["shop_web"], "nginx:1.27@"+rebuildDigest; got != want {
			t.Errorf("pin of shop_web = %q, want the image the service runs, %q", got, want)
		}
		if rendered, err := repo.RevisionRendering(ctx, rev.ID); err != nil || !strings.Contains(string(rendered), "nginx:1.27@"+rebuildDigest) {
			t.Errorf("rendering = %q, %v, want the stack shown with its pinned image", rendered, err)
		}
		// D-15: the stored definition holds the environment values as literals.
		if text := string(rev.Copy.Compose()); !strings.Contains(text, "s3cret-value") {
			t.Errorf("revision 1 does not hold the environment values as literals:\n%s", text)
		}
		draft, err := repo.Draft(ctx, st.ID)
		if err != nil || !draft.IsDraft || draft.Version != 1 || string(draft.Copy.Compose()) != string(rev.Copy.Compose()) {
			t.Errorf("draft = %+v, %v, want version 1 with revision 1's definition", draft, err)
		}
		read, err := repo.Stack(ctx, rig.envID, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		a := read.Adoption
		if len(a.Networks) != 2 || a.Networks[0] != "shop_back" || a.Networks[1] != "shop_default" {
			t.Errorf("adoption networks = %v, want the two the stack owns, by their engine names", a.Networks)
		}
		if len(a.Volumes) != 1 || a.Volumes[0] != "shop_data" {
			t.Errorf("adoption volumes = %v, want shop_data", a.Volumes)
		}
		if len(a.Secrets) == 0 || len(a.Configs) == 0 {
			t.Errorf("adoption = %+v, want the existing secret and config recorded as referenced", a)
		}
	})

	t.Run("standalone", func(t *testing.T) {
		t.Parallel()
		rig := hostRig(t, stackopstest.Foreign(t, "shop", foreignProject))
		repo := adoptRepo(t, rig)
		ctx := testsupport.Context(t)

		st, err := rig.adopt(t, repo, stacks.TargetStandalone, "shop", stackops.SelfMarks{})
		if err != nil {
			t.Fatalf("AdoptCommit: %v", err)
		}

		rev, err := repo.Revision(ctx, st.ID, 1)
		if err != nil || rev.Origin != stackops.OriginAdopted || rev.Outcome != stackops.OutcomeGood {
			t.Fatalf("revision 1 = %+v, %v, want adopted and good", rev, err)
		}
		pins, err := repo.RevisionPins(ctx, rev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := pins["web"], "nginx:1.27@"+stackopstest.Digest("a"); got != want {
			t.Errorf("pin of web = %q, want the digest the container runs, %q", got, want)
		}
		read, err := repo.Stack(ctx, rig.envID, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Adoption.Networks) != 1 || read.Adoption.Networks[0] != "shop_back" {
			t.Errorf("adoption networks = %v, want shop_back", read.Adoption.Networks)
		}
	})
}

func TestAdoptChangesNothingOnTheEngine(t *testing.T) {
	t.Parallel()
	{ // swarm
		rig := objectsRig(t)
		if _, err := rig.adopt(t, adoptRepo(t, rig), stacks.TargetSwarm, "shop", stackops.SelfMarks{}); err != nil {
			t.Fatalf("AdoptCommit: %v", err)
		}
		if writes := rig.fake.Writes(); len(writes) != 0 {
			t.Errorf("adopting wrote to the engine: %v", writes)
		}
		for _, req := range rig.fake.Requests() {
			if secretOrConfigRead.MatchString(req) {
				t.Errorf("adopting asked for %s: a secret or a config was inspected", req)
			}
		}
	}
	{ // standalone
		rig := hostRig(t, stackopstest.Foreign(t, "shop", foreignProject))
		if _, err := rig.adopt(t, adoptRepo(t, rig), stacks.TargetStandalone, "shop", stackops.SelfMarks{}); err != nil {
			t.Fatalf("AdoptCommit: %v", err)
		}
		if writes := rig.fake.Writes(); len(writes) != 0 {
			t.Errorf("adopting wrote to the engine: %v", writes)
		}
	}
}

// TestAdoptStaleFingerprint: a fingerprint that is not what runs now stores nothing (T-08-46). The browser says what
// it looked at; the server rebuilds from what runs.
func TestAdoptStaleFingerprint(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)

	{ // a swarm service that was updated since
		before := objectsRig(t)
		fp, err := before.deps.AdoptFingerprint(ctx, before.envID, stacks.TargetSwarm, "shop")
		if err != nil {
			t.Fatal(err)
		}
		// The same stack after an update: the engine raised the version of its service.
		after := objectsRigWith(t, 7, nil)
		repo := adoptRepo(t, after)

		_, err = after.adoptWith(t, repo, stacks.TargetSwarm, "shop", fp, stackops.SelfMarks{})

		if !errors.Is(err, stackops.ErrAdoptStale) {
			t.Fatalf("AdoptCommit with the fingerprint of an older state = %v, want ErrAdoptStale", err)
		}
		if list, _ := repo.Stacks(ctx, after.envID); len(list) != 0 {
			t.Errorf("a stale adoption stored %+v, want nothing", list)
		}
	}

	{ // a container that was replaced since
		before := hostRig(t, stackopstest.Foreign(t, "shop", foreignProject))
		fp, err := before.deps.AdoptFingerprint(ctx, before.envID, stacks.TargetStandalone, "shop")
		if err != nil {
			t.Fatal(err)
		}
		changed := stackopstest.Foreign(t, "shop", strings.Replace(foreignProject, "nginx:1.27", "nginx:1.28", 1))
		after := hostRig(t, changed)
		repo := adoptRepo(t, after)

		_, err = after.adoptWith(t, repo, stacks.TargetStandalone, "shop", fp, stackops.SelfMarks{})

		if !errors.Is(err, stackops.ErrAdoptStale) {
			t.Fatalf("AdoptCommit with the fingerprint of an older state = %v, want ErrAdoptStale", err)
		}
		if list, _ := repo.Stacks(ctx, after.envID); len(list) != 0 {
			t.Errorf("a stale adoption stored %+v, want nothing", list)
		}
	}

	{ // a fingerprint that was never read
		rig := objectsRig(t)
		repo := adoptRepo(t, rig)
		if _, err := rig.adoptWith(t, repo, stacks.TargetSwarm, "shop", "", stackops.SelfMarks{}); !errors.Is(err, stackops.ErrAdoptStale) {
			t.Fatalf("AdoptCommit with an empty fingerprint = %v, want ErrAdoptStale", err)
		}
	}

	{ // the same state gives the same fingerprint
		rig := objectsRig(t)
		a, err := rig.deps.AdoptFingerprint(ctx, rig.envID, stacks.TargetSwarm, "shop")
		if err != nil {
			t.Fatal(err)
		}
		b, err := rig.deps.AdoptFingerprint(ctx, rig.envID, stacks.TargetSwarm, "shop")
		if err != nil || a == "" || a != b {
			t.Errorf("fingerprints %q and %q (%v), want one non-empty digest for one state", a, b, err)
		}
	}
}

func TestAdoptRefusesCoamingAndAgentStacks(t *testing.T) {
	t.Parallel()
	own := map[string]string{stacks.LabelSwarmNamespace: "selfns"}
	agent := map[string]string{stacks.LabelSwarmNamespace: "swarmstack", "coaming.agent": "true"}
	dotted := map[string]string{stacks.LabelSwarmNamespace: "my.stack"}
	rig := newSwarmRig(t, nil,
		dockertest.FakeService{ID: "s1", Name: "selfns_server", Mode: "replicated", Running: 1, Desired: 1, Image: "coaming:1", Labels: own},
		dockertest.FakeService{ID: "s2", Name: "swarmstack_agent", Mode: "global", Running: 1, Desired: 1, Image: "coaming-agent:1", Labels: agent},
		dockertest.FakeService{ID: "s3", Name: "my.stack_web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Labels: dotted},
	)
	repo := adoptRepo(t, rig)
	self := stackops.SelfMarks{Namespace: "selfns"}

	for name, want := range map[string]string{"selfns": stackops.OwnerCoaming, "swarmstack": stackops.OwnerAgent, "my.stack": ""} {
		_, err := rig.adopt(t, repo, stacks.TargetSwarm, name, self)

		var refused *stackops.NotAdoptableError
		if !errors.Is(err, stackops.ErrNotAdoptable) || !errors.As(err, &refused) {
			t.Errorf("AdoptCommit of %s = %v, want a *NotAdoptableError", name, err)
			continue
		}
		if refused.Found.Owner != want {
			t.Errorf("%s: owner = %q, want %q", name, refused.Found.Owner, want)
		}
		if want == "" && refused.Found.NameReason == "" {
			t.Errorf("%s: a name the target cannot hold must say why", name)
		}
	}
	if list, _ := repo.Stacks(testsupport.Context(t), rig.envID); len(list) != 0 {
		t.Errorf("refused adoptions stored %+v, want nothing", list)
	}
	if writes := rig.fake.Writes(); len(writes) != 0 {
		t.Errorf("refused adoptions wrote to the engine: %v", writes)
	}
}

// TestAdoptRefusesAStackCoamingAlreadyManages and a name nothing carries.
func TestAdoptRefusesAManagedOrAMissingName(t *testing.T) {
	t.Parallel()
	rig := objectsRig(t)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)

	if _, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{}); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	if _, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{}); !errors.Is(err, stackops.ErrNameTaken) {
		t.Errorf("a second adoption = %v, want ErrNameTaken", err)
	}
	managed, err := repo.Stacks(ctx, rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rig.deps.AdoptCommit(ctx, repo, stackops.AdoptInput{
		EnvID: rig.envID, Name: "ghost", Target: stacks.TargetSwarm, Fingerprint: "x", Caller: stackops.Caller{Actor: "ada"}, Managed: managed,
	})
	if !errors.Is(err, stackops.ErrUnmanagedGone) {
		t.Errorf("an adoption of a name nothing carries = %v, want ErrUnmanagedGone", err)
	}
}

// TestAdoptPrivilegedStackNeverFailsOnPolicy: adoption only records what already runs, so a stack that bind-mounts
// the Docker socket or is privileged is adopted; a later deploy of it is judged as any deploy (D-17).
func TestAdoptPrivilegedStackNeverFailsOnPolicy(t *testing.T) {
	t.Parallel()
	{ // a swarm service that mounts the Docker socket
		rig := objectsRigWith(t, 0, func(spec *swarm.ServiceSpec) {
			spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts,
				mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"})
		})
		repo := adoptRepo(t, rig)

		st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
		if err != nil {
			t.Fatalf("AdoptCommit of a stack that mounts the Docker socket: %v", err)
		}
		draft, err := repo.Draft(testsupport.Context(t), st.ID)
		if err != nil || !strings.Contains(string(draft.Copy.Compose()), "docker.sock") {
			t.Errorf("draft = %v, %v, want the socket mount kept in the definition", draft.Copy.Compose(), err)
		}
	}

	{ // a privileged container
		rig := hostRig(t, stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n    privileged: true\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"))
		repo := adoptRepo(t, rig)

		st, err := rig.adopt(t, repo, stacks.TargetStandalone, "shop", stackops.SelfMarks{})
		if err != nil {
			t.Fatalf("AdoptCommit of a privileged stack: %v", err)
		}
		draft, err := repo.Draft(testsupport.Context(t), st.ID)
		if err != nil || !strings.Contains(string(draft.Copy.Compose()), "privileged: true") {
			t.Errorf("draft = %s, %v, want privileged kept in the definition", draft.Copy.Compose(), err)
		}
	}
}

// TestAdoptCheckChangesNothing: the check is a read. It sends nothing to an engine, inspects no secret or config
// and stores no row (T-08-52, STK-13).
func TestAdoptCheckChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	for name, tc := range map[string]struct {
		rig    *swarmRig
		target stacks.Target
	}{
		"swarm":      {strayRig(t), stacks.TargetSwarm},
		"standalone": {hostRig(t, stackopstest.Foreign(t, "shop", foreignProject)), stacks.TargetStandalone},
	} {
		repo := adoptRepo(t, tc.rig)

		if _, err := tc.rig.checkOf(t, tc.target, true); err != nil {
			t.Fatalf("%s: AdoptCheck: %v", name, err)
		}

		if writes := tc.rig.fake.Writes(); len(writes) != 0 {
			t.Errorf("%s: the check wrote to the engine: %v", name, writes)
		}
		for _, req := range tc.rig.fake.Requests() {
			if secretOrConfigRead.MatchString(req) {
				t.Errorf("%s: the check asked for %s: a secret or a config was inspected", name, req)
			}
		}
		if list, err := repo.Stacks(ctx, tc.rig.envID); err != nil || len(list) != 0 {
			t.Errorf("%s: the check stored %+v, %v, want nothing", name, list, err)
		}
	}
}

// TestAdoptStoresGapPaths: adopting a stack with gaps stores the paths of the gaps by service, never their values
// (D-55), so drift can say a difference was there at adoption.
func TestAdoptStoresGapPaths(t *testing.T) {
	t.Parallel()
	rig := strayRig(t)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)

	st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}

	read, err := repo.Stack(ctx, rig.envID, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	gaps := read.Adoption.Gaps
	for _, service := range []string{"web", "shop_web"} {
		if _, ok := gaps[service]; !ok {
			t.Errorf("gaps = %v, want an entry for %s", gaps, service)
		}
	}
	for service, paths := range gaps {
		for _, p := range paths {
			if strings.Contains(p, "s3cret-value") || strings.Contains(p, "cmd-secret") {
				t.Errorf("the gap of %s stores a value: %q", service, p)
			}
		}
	}

	// A clean stack stores no gap.
	clean := corpusRig(t, "deploy.yaml", nil)
	cleanRepo := adoptRepo(t, clean)
	cst, err := clean.adopt(t, cleanRepo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit of the clean stack: %v", err)
	}
	cread, err := cleanRepo.Stack(ctx, clean.envID, cst.ID)
	if err != nil || len(cread.Adoption.Gaps) != 0 {
		t.Errorf("a clean round trip stored gaps %v (%v), want none", cread.Adoption.Gaps, err)
	}
}

// TestAdoptCheckRedactsForAViewer: a caller without secrets.reveal reads <hidden> for every environment value, command
// and log option in the gaps, though equality was decided on the real values (T-08-52, D-47).
func TestAdoptCheckRedactsForAViewer(t *testing.T) {
	t.Parallel()
	rig := strayRig(t)

	hidden, err := rig.checkOf(t, stacks.TargetSwarm, false)
	if err != nil {
		t.Fatalf("AdoptCheck without reveal: %v", err)
	}
	shown, err := rig.checkOf(t, stacks.TargetSwarm, true)
	if err != nil {
		t.Fatalf("AdoptCheck with reveal: %v", err)
	}

	whole := func(res stackops.AdoptCheckResult) string {
		var b strings.Builder
		for _, g := range res.Gaps {
			b.Write(g.Current)
			b.Write(g.Next)
		}
		return b.String()
	}
	for _, secret := range []string{"s3cret-value", "cmd-secret", "log-secret"} {
		if strings.Contains(whole(hidden), secret) {
			t.Errorf("the gaps for a viewer carry %q: %s", secret, whole(hidden))
		}
		if !strings.Contains(whole(shown), secret) {
			t.Errorf("the gaps for a caller who may reveal lack %q: %s", secret, whole(shown))
		}
	}
	if !strings.Contains(whole(hidden), `\u003chidden\u003e`) { // the placeholder, as JSON writes it
		t.Errorf("the gaps for a viewer do not say what they hide: %s", whole(hidden))
	}
	if hidden.Fingerprint != shown.Fingerprint || len(hidden.Gaps) != len(shown.Gaps) {
		t.Errorf("a viewer's check differs in more than its values: %d gaps against %d", len(hidden.Gaps), len(shown.Gaps))
	}
}

// TestHostAccessListsForAnAdmin: HostAccess lists every host-access setting of a definition, for whoever asks, with
// its service and its kind. It judges nothing and needs no caller (D-17).
func TestHostAccessListsForAnAdmin(t *testing.T) {
	t.Parallel()
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(`services:
  web:
    image: nginx:1.27
    privileged: true
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
  db:
    image: postgres:16
`)}}}

	got, err := stackops.Deps{}.HostAccess(copy, convert.Plan{})
	if err != nil {
		t.Fatalf("HostAccess: %v", err)
	}

	var texts []string
	for _, e := range got.Entries {
		texts = append(texts, e.Text)
	}
	for _, want := range []string{"a bind mount of /var/run/docker.sock in web", "privileged: true in web"} {
		if !slices.Contains(texts, want) {
			t.Errorf("entries = %q, want %q among them", texts, want)
		}
	}
	if len(texts) != 2 {
		t.Errorf("entries = %q, want the two settings of web and none of db", texts)
	}
	if want := []string{stackops.KindBindMounts, stackops.KindPrivileges}; !slices.Equal(got.Kinds, want) {
		t.Errorf("kinds = %v, want %v", got.Kinds, want)
	}
	if got.Refused || len(got.Problems) != 0 {
		t.Errorf("HostAccess refused %+v: it decides nothing", got)
	}

	none, err := stackops.Deps{}.HostAccess(stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services:\n  db:\n    image: postgres:16\n")}}}, convert.Plan{})
	if err != nil || len(none.Entries) != 0 || len(none.Kinds) != 0 {
		t.Errorf("a definition with no host access listed %+v (%v)", none, err)
	}
}

// TestAdoptCheckObjectsAndFlags: the check names how each network, volume, secret and config is treated, and carries
// the Portainer and orphan-ID flags and the host-access settings of the stack.
func TestAdoptCheckObjectsAndFlags(t *testing.T) {
	t.Parallel()
	rig := objectsRigWith(t, 0, func(spec *swarm.ServiceSpec) {
		spec.Labels["io.portainer.accesscontrol.teams"] = "ops"
		spec.Labels[stacks.LabelStackID] = "stack-made-before"
		spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts,
			mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"})
	})

	res, err := rig.checkOf(t, stacks.TargetSwarm, true)
	if err != nil {
		t.Fatalf("AdoptCheck: %v", err)
	}

	var objects []string
	for _, o := range res.Objects {
		objects = append(objects, o.Kind+" "+o.Name+" "+o.Treatment)
	}
	for _, want := range []string{"network shop_back owned", "network shared external", "volume shop_data owned", "secret shop_pw external", "config shop_conf external"} {
		if !slices.Contains(objects, want) {
			t.Errorf("objects = %v, want %q among them", objects, want)
		}
	}
	if !res.Portainer || !res.Orphan {
		t.Errorf("Portainer = %v, Orphan = %v, want both: the service carries Portainer's label and another stack's ID", res.Portainer, res.Orphan)
	}
	if !slices.Contains(res.Policy.Kinds, stackops.KindBindMounts) {
		t.Errorf("policy kinds = %v, want bind mounts for the Docker socket", res.Policy.Kinds)
	}
	var texts []string
	for _, e := range res.Policy.Entries {
		texts = append(texts, e.Text)
	}
	if want := "a bind mount of /var/run/docker.sock in web"; !slices.Contains(texts, want) {
		t.Errorf("policy entries = %q, want %q", texts, want)
	}
}

// The paste source (plan 08-15, STK-13, D-12): a person who still has the compose file the stack was deployed from
// pastes it; it is checked by the one check pipeline, compared with what runs, and adopted as written.

// pastedShop is the file docker stack deploy was given: comments and a key order the rebuild would not write.
const pastedShop = `# The shop, as deployed in 2023.
services:
  web:
    environment:
      MODE: prod
    image: nginx:1.27   # stay on 1.27
    deploy:
      replicas: 2
  worker:
    image: busybox:1
`

// pasteCheck runs the adopt check of the stack named shop with a pasted file.
func (r *swarmRig) pasteCheck(t *testing.T, target stacks.Target, pasted stacks.WorkingCopy, reveal bool) (stackops.AdoptCheckResult, error) {
	t.Helper()
	managed, err := adoptRepo(t, r).Stacks(testsupport.Context(t), r.envID)
	if err != nil {
		t.Fatal(err)
	}
	return r.deps.AdoptCheck(testsupport.Context(t), stackops.AdoptInput{
		EnvID: r.envID, Name: "shop", Target: target, Caller: stackops.Caller{Actor: "ada"}, Managed: managed,
		Source: stackops.AdoptSourcePaste, Pasted: &pasted,
	}, reveal)
}

func composeOnly(text string, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(text)}}, Vars: vars}
}

// TestAdoptCheckPasteMatches: the file the stack was deployed from, pasted back, differs from what runs in nothing but
// Coaming's ownership labels.
func TestAdoptCheckPasteMatches(t *testing.T) {
	t.Parallel()
	t.Run("swarm", func(t *testing.T) {
		t.Parallel()
		rig := composeRig(t, []byte(pastedShop), nil)

		res, err := rig.pasteCheck(t, stacks.TargetSwarm, composeOnly(pastedShop), true)
		if err != nil {
			t.Fatalf("AdoptCheck with a pasted file: %v", err)
		}

		if res.Source != stackops.AdoptSourcePaste || !res.Matches || !res.Clean || res.Blocked || len(res.Gaps) != 0 {
			var names []string
			for _, g := range res.Gaps {
				names = append(names, g.Short+" "+g.Change)
			}
			t.Fatalf("source %q matches %v clean %v blocked %v gaps %v, want a pasted file that matches", res.Source, res.Matches, res.Clean, res.Blocked, names)
		}
		if res.Problems.Errors() != 0 {
			t.Errorf("problems = %+v, want none", res.Problems)
		}
		if res.Fingerprint == "" {
			t.Errorf("the check answers no fingerprint to adopt with")
		}
	})

	t.Run("standalone", func(t *testing.T) {
		t.Parallel()
		rig := hostRig(t, stackopstest.Foreign(t, "shop", foreignProject))

		res, err := rig.pasteCheck(t, stacks.TargetStandalone, composeOnly(foreignProject), true)
		if err != nil {
			t.Fatalf("AdoptCheck with a pasted file: %v", err)
		}

		if !res.Matches || res.Blocked {
			t.Fatalf("matches %v blocked %v gaps %+v problems %+v, want the file the project was made from to match", res.Matches, res.Blocked, res.Gaps, res.Problems)
		}
	})
}

// TestAdoptCheckPasteDiffers: a pasted file with another replica count differs in that one setting, which the first
// deploy would change. The check still answers (a gap never blocks).
func TestAdoptCheckPasteDiffers(t *testing.T) {
	t.Parallel()
	rig := composeRig(t, []byte(pastedShop), nil)

	res, err := rig.pasteCheck(t, stacks.TargetSwarm, composeOnly(strings.Replace(pastedShop, "replicas: 2", "replicas: 3", 1)), true)
	if err != nil {
		t.Fatalf("AdoptCheck with a pasted file: %v", err)
	}

	if res.Matches || res.Clean || res.Blocked {
		t.Fatalf("matches %v clean %v blocked %v, want a file that differs and is not blocked", res.Matches, res.Clean, res.Blocked)
	}
	if len(res.Gaps) != 1 || res.Gaps[0].Short != "web" || res.Gaps[0].Change != stacks.ChangeChanged {
		t.Fatalf("gaps = %+v, want web changed and nothing else", res.Gaps)
	}
	if want := []string{"worker"}; !slices.Equal(res.OwnershipOnly, want) {
		t.Errorf("ownership-only = %v, want %v", res.OwnershipOnly, want)
	}
}

// TestAdoptCheckPasteRefusesInclude: a pasted file is untrusted. An include of a file of the server is a problem that
// stops the check; nothing is read, compared or answered of it (T-08-56).
func TestAdoptCheckPasteRefusesInclude(t *testing.T) {
	t.Parallel()
	rig := composeRig(t, []byte(pastedShop), nil)
	hostname, readErr := os.ReadFile("/etc/hostname")

	res, err := rig.pasteCheck(t, stacks.TargetSwarm, composeOnly("include:\n  - /etc/hostname\nservices:\n  web:\n    image: nginx:1.27\n"), true)
	if err != nil {
		t.Fatalf("AdoptCheck with a pasted file: %v", err)
	}

	if !res.Blocked || res.Problems.Errors() == 0 {
		t.Fatalf("blocked %v problems %+v, want the include refused", res.Blocked, res.Problems)
	}
	if len(res.Gaps) != 0 || res.Matches || res.Clean {
		t.Errorf("a refused file was compared: gaps %+v matches %v clean %v", res.Gaps, res.Matches, res.Clean)
	}
	if text := strings.TrimSpace(string(hostname)); readErr == nil && text != "" {
		for _, p := range res.Problems {
			if strings.Contains(p.Message, text) {
				t.Errorf("a problem carries the content of /etc/hostname: %q", p.Message)
			}
		}
	}
}

// TestAdoptCheckPasteBlocksWhatDoesNotLoad: an empty paste, a file that is not YAML and a file with an unfilled
// required variable are each blocked, with the check's own problems and variables for the page to show.
func TestAdoptCheckPasteBlocksWhatDoesNotLoad(t *testing.T) {
	t.Parallel()
	rig := composeRig(t, []byte(pastedShop), nil)

	for name, tc := range map[string]struct {
		pasted   stacks.WorkingCopy
		required string
	}{
		"empty":    {composeOnly(""), ""},
		"not yaml": {composeOnly("services: [unclosed"), ""},
		"required": {composeOnly("services:\n  web:\n    image: nginx:${TAG}\n"), "TAG"},
	} {
		res, err := rig.pasteCheck(t, stacks.TargetSwarm, tc.pasted, true)
		if err != nil {
			t.Fatalf("%s: AdoptCheck: %v", name, err)
		}
		if !res.Blocked || res.Matches || len(res.Gaps) != 0 {
			t.Errorf("%s: blocked %v matches %v gaps %v, want blocked", name, res.Blocked, res.Matches, res.Gaps)
		}
		if tc.required != "" && !slices.ContainsFunc(res.Variables, func(v stacks.ResolvedVar) bool { return v.Name == tc.required && v.Required }) {
			t.Errorf("%s: variables = %+v, want %s listed as required", name, res.Variables, tc.required)
		}
	}
}

// TestAdoptPasteStoresTheFileAsWritten: adopting a pasted file stores it byte for byte, comments and key order
// included, as the draft and as revision 1, with its variables (a secret one sealed) and the running digest pinned.
func TestAdoptPasteStoresTheFileAsWritten(t *testing.T) {
	t.Parallel()
	const text = `# Values come from the variables below.
services:
  web:
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    image: nginx:${TAG}   # stay on 1.27
    deploy:
      replicas: 2
  worker:
    image: busybox:1
`
	rig := composeRig(t, []byte(strings.NewReplacer("${DB_PASSWORD}", "pw-value-123", "${TAG}", "1.27").Replace(text)), nil)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)
	pasted := composeOnly(text, stacks.Var{Name: "TAG", Value: "1.27"}, stacks.Var{Name: "DB_PASSWORD", Value: "pw-value-123", Secret: true})
	fp, err := rig.deps.AdoptFingerprint(ctx, rig.envID, stacks.TargetSwarm, "shop")
	if err != nil {
		t.Fatal(err)
	}

	st, err := rig.deps.AdoptCommit(ctx, repo, stackops.AdoptInput{
		EnvID: rig.envID, Name: "shop", Target: stacks.TargetSwarm, Fingerprint: fp, Caller: stackops.Caller{Actor: "ada"},
		Source: stackops.AdoptSourcePaste, Pasted: &pasted,
	})
	if err != nil {
		t.Fatalf("AdoptCommit with a pasted file: %v", err)
	}

	rev, err := repo.Revision(ctx, st.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rev.Copy.Compose()); got != text {
		t.Errorf("revision 1 holds\n%s\nwant the file as written\n%s", got, text)
	}
	if got := string(draft.Copy.Compose()); got != text {
		t.Errorf("the draft holds\n%s\nwant the file as written", got)
	}
	if rev.Origin != stackops.OriginAdopted || rev.Outcome != stackops.OutcomeGood {
		t.Errorf("revision 1 = origin %q outcome %q, want adopted and good", rev.Origin, rev.Outcome)
	}
	pins, err := repo.RevisionPins(ctx, rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pins["shop_web"], "nginx:1.27@"+rebuildDigest; got != want {
		t.Errorf("pin of shop_web = %q, want the digest that runs, %q", got, want)
	}
	for _, snap := range []string{draft.ID, rev.ID} {
		var plain, sealed, tag string
		row := repo.Store.Read.QueryRowContext(ctx, `SELECT value, value_sealed FROM stack_snapshot_vars WHERE snapshot_id = ? AND name = 'DB_PASSWORD'`, snap)
		if err := row.Scan(&plain, &sealed); err != nil {
			t.Fatalf("read the secret variable of %s: %v", snap, err)
		}
		if plain != "" || sealed == "" || strings.Contains(sealed, "pw-value-123") {
			t.Errorf("the secret variable is stored as %q / %q, want it sealed", plain, sealed)
		}
		if err := repo.Store.Read.QueryRowContext(ctx, `SELECT value FROM stack_snapshot_vars WHERE snapshot_id = ? AND name = 'TAG'`, snap).Scan(&tag); err != nil || tag != "1.27" {
			t.Errorf("variable TAG of %s = %q, %v, want 1.27", snap, tag, err)
		}
	}
	if len(draft.Copy.Vars) != 2 {
		t.Errorf("the draft holds %d variables, want 2", len(draft.Copy.Vars))
	}
}

// TestAdoptPasteIsCheckedAgainAtCommit: the commit trusts no verdict of the browser. A pasted file that does not pass
// is refused, with nothing stored, however the page was told.
func TestAdoptPasteIsCheckedAgainAtCommit(t *testing.T) {
	t.Parallel()
	rig := composeRig(t, []byte(pastedShop), nil)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)
	fp, err := rig.deps.AdoptFingerprint(ctx, rig.envID, stacks.TargetSwarm, "shop")
	if err != nil {
		t.Fatal(err)
	}

	for name, pasted := range map[string]*stacks.WorkingCopy{
		"include": ptr(composeOnly("include:\n  - /etc/hostname\nservices:\n  web:\n    image: nginx:1.27\n")),
		"nothing": nil,
	} {
		_, err = rig.deps.AdoptCommit(ctx, repo, stackops.AdoptInput{
			EnvID: rig.envID, Name: "shop", Target: stacks.TargetSwarm, Fingerprint: fp, Caller: stackops.Caller{Actor: "ada"},
			Source: stackops.AdoptSourcePaste, Pasted: pasted,
		})
		var refused *stackops.PasteError
		if !errors.As(err, &refused) {
			t.Errorf("%s: AdoptCommit = %v, want a *PasteError", name, err)
		}
	}
	if list, err := repo.Stacks(ctx, rig.envID); err != nil || len(list) != 0 {
		t.Errorf("a refused paste stored %+v, %v, want nothing", list, err)
	}

	// A secret variable too short to hide is refused at the commit, as a save refuses it.
	short := composeOnly(pastedShop, stacks.Var{Name: "TOKEN", Value: "abc", Secret: true})
	_, err = rig.deps.AdoptCommit(ctx, repo, stackops.AdoptInput{
		EnvID: rig.envID, Name: "shop", Target: stacks.TargetSwarm, Fingerprint: fp, Caller: stackops.Caller{Actor: "ada"},
		Source: stackops.AdoptSourcePaste, Pasted: &short,
	})
	if !errors.Is(err, stacks.ErrSecretTooShort) {
		t.Errorf("AdoptCommit with a three-character secret = %v, want ErrSecretTooShort", err)
	}
}

func ptr[T any](v T) *T { return &v }

// Adoption records the platforms the services run with (G-08-4, D-14): revision 1 holds them beside its pins, so a roll
// back to it after the images moved on puts them back.

// platformsRig is the swarm of pastedShop whose web service runs two platforms and whose worker runs one.
func platformsRig(t *testing.T) *swarmRig {
	t.Helper()
	return composeRig(t, []byte(pastedShop), func(short string, spec *swarm.ServiceSpec) {
		switch short {
		case "web":
			setLivePlatforms(swarm.Platform{OS: "linux", Architecture: "amd64"}, swarm.Platform{OS: "linux", Architecture: "arm64"})(spec)
		case "worker":
			setLivePlatforms(swarm.Platform{OS: "linux", Architecture: "arm64"})(spec)
		}
	})
}

var (
	platformAMD = swarm.Platform{OS: "linux", Architecture: "amd64"}
	platformARM = swarm.Platform{OS: "linux", Architecture: "arm64"}
)

func TestAdoptStoresThePlatformsTheServicesRunWith(t *testing.T) {
	t.Parallel()
	rig := platformsRig(t)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)

	st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}

	rev, err := repo.Revision(ctx, st.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]swarm.Platform{"shop_web": {platformAMD, platformARM}, "shop_worker": {platformARM}}
	if !reflect.DeepEqual(rev.Platforms, want) {
		t.Errorf("revision 1 platforms = %v, want %v", rev.Platforms, want)
	}
}

func TestAdoptWithoutPlatformsStoresNone(t *testing.T) {
	t.Parallel()
	rig := composeRig(t, []byte(pastedShop), nil)
	repo := adoptRepo(t, rig)

	st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}

	rev, err := repo.Revision(testsupport.Context(t), st.ID, 1)
	if err != nil || len(rev.Platforms) != 0 {
		t.Errorf("revision 1 = platforms %v, %v, want none for services that run none", rev.Platforms, err)
	}
}

// TestAdoptPasteKeepsPlatformsOnlyWhereTheImageIsTheOneRunning: a pasted file that names another image for a service
// has no platforms recorded for it (D-12); the service whose image is the one running keeps its own.
func TestAdoptPasteKeepsPlatformsOnlyWhereTheImageIsTheOneRunning(t *testing.T) {
	t.Parallel()
	rig := platformsRig(t)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)
	fp, err := rig.deps.AdoptFingerprint(ctx, rig.envID, stacks.TargetSwarm, "shop")
	if err != nil {
		t.Fatal(err)
	}
	pasted := composeOnly("services:\n  web:\n    image: nginx:1.28\n  worker:\n    image: busybox:1\n")

	st, err := rig.deps.AdoptCommit(ctx, repo, stackops.AdoptInput{
		EnvID: rig.envID, Name: "shop", Target: stacks.TargetSwarm, Fingerprint: fp, Caller: stackops.Caller{Actor: "ada"},
		Source: stackops.AdoptSourcePaste, Pasted: &pasted,
	})
	if err != nil {
		t.Fatalf("AdoptCommit with a pasted file: %v", err)
	}

	rev, err := repo.Revision(ctx, st.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]swarm.Platform{"shop_worker": {platformARM}}
	if !reflect.DeepEqual(rev.Platforms, want) {
		t.Errorf("revision 1 platforms = %v, want %v: none for web, whose pasted image is not the one running", rev.Platforms, want)
	}
}

// TestRollbackReviewOfTheAdoptedRevisionShowsItsPlatformsComingBack: the services move to another digest that runs on no
// platform in particular, and the review of a roll back to revision 1 lists the platforms it stored.
func TestRollbackReviewOfTheAdoptedRevisionShowsItsPlatformsComingBack(t *testing.T) {
	t.Parallel()
	rig := platformsRig(t)
	repo := adoptRepo(t, rig)
	ctx := testsupport.Context(t)
	st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}

	owned := rig.ownedNow(t, st)
	newDigest := "sha256:" + strings.Repeat("c", 64)
	for _, name := range []string{"shop_web", "shop_worker"} {
		svc, ok := owned[name]
		if !ok {
			t.Fatalf("the swarm holds no service %s", name)
		}
		var spec swarm.ServiceSpec
		if err := json.Unmarshal(svc.Spec, &spec); err != nil {
			t.Fatal(err)
		}
		cs := *spec.TaskTemplate.ContainerSpec
		cs.Image = withoutPinDigest(cs.Image) + "@" + newDigest
		spec.TaskTemplate.ContainerSpec = &cs
		spec.TaskTemplate.Placement = nil
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		err = rig.deps.Aggregator.WriteOnManager(ctx, rig.envID, func(c *dockerx.Client) error {
			_, e := c.ServiceUpdate(ctx, svc.ID, svc.Version, raw, "")
			return e
		})
		if err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
	}

	rev, err := repo.Revision(ctx, st.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := repo.RevisionPins(ctx, rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := rig.reviewInput(t, st.Name, pastedShop, nil)
	in.Stack = st
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRollback, &rev, pins, rev.Copy
	rv, err := rig.deps.Review(ctx, in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}

	if got := platformsRows(t, rv); len(got) != 2 {
		t.Errorf("platforms rows = %v, want one for each service: the adopted revision stored them and the services run none now", got)
	}
}

// withoutPinDigest is an image reference without the digest that pins it.
func withoutPinDigest(ref string) string {
	if i := strings.LastIndex(ref, "@"); i > 0 {
		return ref[:i]
	}
	return ref
}
