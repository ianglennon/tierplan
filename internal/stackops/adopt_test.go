// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/testsupport"
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

func TestAdoptRefusesBayplanAndAgentStacks(t *testing.T) {
	t.Parallel()
	own := map[string]string{stacks.LabelSwarmNamespace: "selfns"}
	agent := map[string]string{stacks.LabelSwarmNamespace: "swarmstack", "bayplan.agent": "true"}
	dotted := map[string]string{stacks.LabelSwarmNamespace: "my.stack"}
	rig := newSwarmRig(t, nil,
		dockertest.FakeService{ID: "s1", Name: "selfns_server", Mode: "replicated", Running: 1, Desired: 1, Image: "bayplan:1", Labels: own},
		dockertest.FakeService{ID: "s2", Name: "swarmstack_agent", Mode: "global", Running: 1, Desired: 1, Image: "bayplan-agent:1", Labels: agent},
		dockertest.FakeService{ID: "s3", Name: "my.stack_web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Labels: dotted},
	)
	repo := adoptRepo(t, rig)
	self := stackops.SelfMarks{Namespace: "selfns"}

	for name, want := range map[string]string{"selfns": stackops.OwnerBayplan, "swarmstack": stackops.OwnerAgent, "my.stack": ""} {
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

// TestAdoptRefusesAStackBayplanAlreadyManages and a name nothing carries.
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
	if !strings.Contains(whole(hidden), "<hidden>") {
		t.Errorf("the gaps for a viewer do not say what they hide: %s", whole(hidden))
	}
	if hidden.Fingerprint != shown.Fingerprint || len(hidden.Gaps) != len(shown.Gaps) {
		t.Errorf("a viewer's check differs in more than its values: %d gaps against %d", len(hidden.Gaps), len(shown.Gaps))
	}
}
