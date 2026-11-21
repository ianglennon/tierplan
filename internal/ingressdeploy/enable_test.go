// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// wantAllDone fails the test unless every item of the enable job ended done.
func wantAllDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemToken, ingressdeploy.ItemNetwork, ingressdeploy.ItemService, ingressdeploy.ItemSwitch} {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
}

// TestEnableCreatesObjects is the tracer: an enable on a swarm with a connected manager makes the secret,
// the network and the global proxy service, and switches ingress on with one audit row, and the clear join
// token shows nowhere in the job.
func TestEnableCreatesObjects(t *testing.T) {
	r := newRig(t, rigOptions{})
	job := r.enable(ingressdeploy.EnableRequest{})
	wantAllDone(t, job)
	if job.Kind != ingressdeploy.JobKindEnable || job.EnvID != r.envID || job.Owner != testOwner {
		t.Errorf("job = kind %q env %q owner %q", job.Kind, job.EnvID, job.Owner)
	}

	// The secret is named after the token the server issued and carries the object labels.
	tok, err := r.tokens.TokenStatus(testsupport.Context(t), r.envID)
	if err != nil || !tok.Exists || tok.Generation != 1 {
		t.Fatalf("TokenStatus = %+v, %v; want generation 1", tok, err)
	}
	secretName := ingressdeploy.SecretName(r.envID, 1, tok.ID)
	secrets := r.secrets()
	if len(secrets) != 1 || secrets[0].Name != secretName {
		t.Fatalf("secrets = %+v, want exactly %s", secrets, secretName)
	}
	if !ingress.OwnsSecret(secrets[0].Name, secrets[0].Labels) || secrets[0].Labels["tierplan.keep"] != "true" {
		t.Errorf("secret labels = %v, want the object labels", secrets[0].Labels)
	}
	if creates := r.fake.SecretCreates(); len(creates) != 1 || creates[0].DataBytes == 0 {
		t.Errorf("secret creates = %+v, want one with a value", creates)
	}

	// The network is a /22 from the pool beside the ingress network's /24, not attachable.
	n, ok := r.network()
	if !ok {
		t.Fatal("the swarm holds no tierplan-ingress network")
	}
	if len(n.Subnets) != 1 || n.Subnets[0] != "10.0.4.0/22" {
		t.Errorf("network subnets = %v, want [10.0.4.0/22]", n.Subnets)
	}
	if n.Driver != "overlay" || n.Scope != "swarm" || n.Attachable != ingressdeploy.Attachable || !ingress.OwnsNetwork(n.Name, n.Labels) {
		t.Errorf("network = %+v", n)
	}

	// The service is exactly what ProxyServiceSpec builds, with the secret's ID filled in.
	creates := r.fake.ServiceCreates()
	if len(creates) != 1 {
		t.Fatalf("service creates = %d, want 1", len(creates))
	}
	want := ingressdeploy.ProxyServiceSpec(ingressdeploy.SpecInput{
		Image: ingressdeploy.IngressImage(testServerVersion), URL: "wss://tierplan.example:7480/ingress/connect",
		Pin: "sha256/AAAA,sha256/BBBB", SecretName: secretName,
	})
	want.TaskTemplate.ContainerSpec.Secrets[0].SecretID = secrets[0].ID
	if d := firstDiff("spec", asTree(t, want), asTree(t, creates[0].Spec)); d != "" {
		t.Errorf("the created service differs from ProxyServiceSpec at %s", d)
	}
	if creates[0].Spec.Mode.Global == nil {
		t.Error("the proxy service is not global")
	}

	// Ingress is on, with one audit row that records the subnet and the image.
	state, err := r.istore.DeployState(testsupport.Context(t), r.envID)
	if err != nil || !state.Enabled {
		t.Fatalf("DeployState = %+v, %v; want enabled", state, err)
	}
	if got := r.auditCount("ingress.enabled"); got != 1 {
		t.Fatalf("ingress.enabled audit rows = %d, want 1", got)
	}
	var details string
	if err := r.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'ingress.enabled'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"10.0.4.0/22", ingressdeploy.IngressImage(testServerVersion)} {
		if !strings.Contains(details, w) {
			t.Errorf("audit details %q do not name %q", details, w)
		}
	}

	// The clear token is in no item, output line or audit row.
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := r.hub.Output(job.ID)
	var all strings.Builder
	all.Write(raw)
	all.WriteString(details)
	for _, l := range lines {
		all.WriteString(l.Text)
	}
	if strings.Contains(all.String(), ingresswire.TokenPrefix) {
		t.Error("the job, its output or the audit row carries a join token")
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}

// TestEnableSendsTheRegistryCredential: the service create carries the credential the resolver chose for the
// proxy image, as a stack deploy does (Pitfall 8).
func TestEnableSendsTheRegistryCredential(t *testing.T) {
	r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.Resolver = fakeRegistry{header: "header-for-ghcr"} }})
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	creates := r.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Auth != "header-for-ghcr" {
		t.Fatalf("service creates = %+v, want one sent with the resolver's credential", creates)
	}
}

// TestEnableTakesTheOperationLock: an enable is refused while a stack operation holds the environment, holds
// the lock for the whole job under HolderKindIngress, and refuses a stack operation meanwhile.
func TestEnableTakesTheOperationLock(t *testing.T) {
	r := newRig(t, rigOptions{})
	ctx := testsupport.Context(t)

	release, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackID: "s1", StackName: "web", By: "user:u2"})
	if !ok {
		t.Fatal("could not take the lock for the stack deploy")
	}
	_, err := r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{})
	var locked *ingressdeploy.LockedError
	if !errors.As(err, &locked) || locked.Holder.StackName != "web" || locked.Holder.Kind != "deploy" {
		t.Fatalf("Enable under a stack deploy = %v, want a *LockedError naming the holder", err)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("a refused enable wrote %v", got)
	}
	release()

	g := newGate()
	r.eng.gate = g
	id, err := r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("the job never reached its first write")
	}
	held, isHeld := r.locks.Held(r.envID)
	if !isHeld || held.Kind != stackops.HolderKindIngress || held.JobID != id || held.By != testOwner || held.Workload != "ingress" {
		t.Fatalf("holder while the job runs = %+v (held %v), want the ingress job %s", held, isHeld, id)
	}
	if _, who, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackName: "web"}); ok || who.Kind != stackops.HolderKindIngress {
		t.Fatalf("a stack deploy while ingress runs: ok=%v holder=%+v, want refused naming the ingress job", ok, who)
	}
	close(g.proceed)
	wantAllDone(t, r.wait(id))
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is held after the job ended")
	}
}

// TestEnableStandaloneUnavailable: a standalone environment is refused before anything is written, by both
// Preview and Enable.
func TestEnableStandaloneUnavailable(t *testing.T) {
	r := newRig(t, rigOptions{standalone: true})
	ctx := testsupport.Context(t)
	if _, err := r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{}); !errors.Is(err, ingressdeploy.ErrStandaloneUnavailable) {
		t.Fatalf("Enable = %v, want ErrStandaloneUnavailable", err)
	}
	if _, err := r.svc.Preview(ctx, r.envID, ingressdeploy.EnableRequest{}); !errors.Is(err, ingressdeploy.ErrStandaloneUnavailable) {
		t.Fatalf("Preview = %v, want ErrStandaloneUnavailable", err)
	}
	if ingressdeploy.CodeStandaloneUnavailable != "ingress_standalone_unavailable" {
		t.Errorf("code = %q", ingressdeploy.CodeStandaloneUnavailable)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
	if st, _ := r.tokens.TokenStatus(ctx, r.envID); st.Exists {
		t.Error("a join token was issued")
	}
	if state, _ := r.istore.DeployState(ctx, r.envID); state.Enabled {
		t.Error("ingress was switched on")
	}
	if got := r.auditCount("ingress.enabled"); got != 0 {
		t.Errorf("ingress.enabled audit rows = %d", got)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock was left held")
	}
}

// TestPreviewWritesNothing: Preview names what an enable would make and the stacks to redeploy, and leaves
// the swarm and the database as they were.
func TestPreviewWritesNothing(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.stacks.refs = []ingressdeploy.StackRef{
		{ID: "s1", Name: "web", HoldsClaims: true},
		{ID: "s2", Name: "done", HoldsClaims: true, Attached: true},
	}
	ctx := testsupport.Context(t)
	res, err := r.svc.Preview(ctx, r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Code != ingressdeploy.CodeRedeployNeeded || res.Findings[0].Severity != ingressdeploy.SeverityWarning {
		t.Errorf("findings = %+v, want only the warning that lists the stack to redeploy", res.Findings)
	}
	if res.Plan.Network.Name != ingress.NetworkName || res.Plan.Network.Subnet != "10.0.4.0/22" {
		t.Errorf("plan network = %+v", res.Plan.Network)
	}
	if res.Plan.Service.Name != ingress.ServiceName || res.Plan.Service.Image != ingressdeploy.IngressImage(testServerVersion) {
		t.Errorf("plan service = %+v", res.Plan.Service)
	}
	if res.Plan.Volume.Name != ingress.VolumeName || !res.Plan.Secret.New {
		t.Errorf("plan = %+v", res.Plan)
	}
	if len(res.Redeploy) != 1 || res.Redeploy[0].Name != "web" {
		t.Errorf("redeploy = %+v, want only the stack not on the network yet", res.Redeploy)
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("Preview wrote %v", got)
	}
	if st, _ := r.tokens.TokenStatus(ctx, r.envID); st.Exists {
		t.Error("Preview issued a join token")
	}
	if state, _ := r.istore.DeployState(ctx, r.envID); state.Enabled {
		t.Error("Preview switched ingress on")
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("Preview left the lock held")
	}
}

// TestEnableRefusesWithoutManager: with no manager agent connected Preview lists ingress_no_manager and
// Enable refuses with it, takes no lock and starts no job.
func TestEnableRefusesWithoutManager(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
	ctx := testsupport.Context(t)

	res, err := r.svc.Preview(ctx, r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Code != ingressdeploy.CodeNoManager || res.Findings[0].Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want ingress_no_manager as an error", res.Findings)
	}
	if ingressdeploy.CodeNoManager != "ingress_no_manager" {
		t.Errorf("code = %q", ingressdeploy.CodeNoManager)
	}

	_, err = r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{})
	var pe *ingressdeploy.PreflightError
	if !errors.As(err, &pe) || len(pe.Findings) != 1 || pe.Findings[0].Code != ingressdeploy.CodeNoManager {
		t.Fatalf("Enable = %v, want a *PreflightError with ingress_no_manager", err)
	}
	if !strings.Contains(err.Error(), "ingress_no_manager") {
		t.Errorf("the error %q does not name the code", err)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the refusal left the lock held")
	}
	if jobs := r.hub.List(func(jobs.Job) bool { return true }); len(jobs) != 0 {
		t.Errorf("%d jobs were started", len(jobs))
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

// secretNames are the names of the secrets the fake swarm holds.
func secretNames(r *rig) []string {
	var out []string
	for _, s := range r.secrets() {
		out = append(out, s.Name)
	}
	return out
}

// TestEnableIsIdempotent: an enable after an enable creates no second network, secret or service and
// issues no new token generation; with only the service gone, it makes the service again and reuses the
// network and the secret.
func TestEnableIsIdempotent(t *testing.T) {
	r := newRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	first, _ := r.tokens.TokenStatus(ctx, r.envID)

	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	if got := len(r.fake.SecretCreates()); got != 1 {
		t.Errorf("secret creates after a repeat = %d, want 1", got)
	}
	if got := len(r.fake.NetworkCreates()); got != 1 {
		t.Errorf("network creates after a repeat = %d, want 1", got)
	}
	if got := len(r.fake.ServiceCreates()); got != 1 {
		t.Errorf("service creates after a repeat = %d, want 1", got)
	}
	if got := len(r.fake.ServiceUpdates()); got != 1 {
		t.Errorf("service updates after a repeat = %d, want 1 (the existing service is brought to the spec)", got)
	}
	again, _ := r.tokens.TokenStatus(ctx, r.envID)
	if again.Generation != first.Generation || again.ID != first.ID {
		t.Errorf("token after a repeat = %+v, want the first %+v", again, first)
	}
	if got := r.auditCount("ingress.enabled"); got != 1 {
		t.Errorf("ingress.enabled audit rows = %d, want 1: a repeat changes nothing", got)
	}

	// Only the service is gone: the rest is reused and the service is made again.
	svc, ok := r.service()
	if !ok {
		t.Fatal("no proxy service")
	}
	if err := r.cli.ServiceRemove(ctx, svc.ID); err != nil {
		t.Fatalf("ServiceRemove: %v", err)
	}
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	if got := len(r.fake.ServiceCreates()); got != 2 {
		t.Errorf("service creates after the service was removed = %d, want 2", got)
	}
	if got := len(r.fake.SecretCreates()); got != 1 {
		t.Errorf("secret creates = %d, want the secret reused", got)
	}
	if got := len(r.fake.NetworkCreates()); got != 1 {
		t.Errorf("network creates = %d, want the network reused", got)
	}
	if st, _ := r.tokens.TokenStatus(ctx, r.envID); st.Generation != 1 {
		t.Errorf("token generation = %d, want 1", st.Generation)
	}
}

// TestEnableRotatesWhenSecretMissing: a current token whose secret is gone from the swarm is replaced by the
// next generation, whose secret is created; so is a disabled token.
func TestEnableRotatesWhenSecretMissing(t *testing.T) {
	r := newRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	st1, _ := r.tokens.TokenStatus(ctx, r.envID)
	name1 := ingressdeploy.SecretName(r.envID, 1, st1.ID)

	// The service holds the secret, so it goes first; then the secret.
	svc, _ := r.service()
	if err := r.cli.ServiceRemove(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	for _, s := range r.secrets() {
		if err := r.cli.SecretRemove(ctx, s.ID); err != nil {
			t.Fatalf("SecretRemove: %v", err)
		}
	}
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	st2, _ := r.tokens.TokenStatus(ctx, r.envID)
	name2 := ingressdeploy.SecretName(r.envID, 2, st2.ID)
	if st2.Generation != 2 || st2.ID == st1.ID {
		t.Fatalf("token after the secret was lost = %+v, want generation 2 with a new ID", st2)
	}
	if names := secretNames(r); len(names) != 1 || names[0] != name2 {
		t.Fatalf("secrets = %v, want only %s (not %s)", names, name2, name1)
	}

	// A disabled token is replaced the same way, and the service moves to the new secret.
	if err := r.tokens.DisableToken(ctx, testActor, r.envID, "", ""); err != nil {
		t.Fatal(err)
	}
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	st3, _ := r.tokens.TokenStatus(ctx, r.envID)
	name3 := ingressdeploy.SecretName(r.envID, 3, st3.ID)
	if st3.Generation != 3 || st3.Disabled {
		t.Fatalf("token after a disabled one = %+v, want an enabled generation 3", st3)
	}
	found := false
	for _, n := range secretNames(r) {
		found = found || n == name3
	}
	if !found {
		t.Errorf("secrets = %v, want %s among them", secretNames(r), name3)
	}
	ups := r.fake.ServiceUpdates()
	if len(ups) == 0 {
		t.Fatal("the service was not updated to the new secret")
	}
	if got := ups[len(ups)-1].Spec.TaskTemplate.ContainerSpec.Secrets[0].SecretName; got != name3 {
		t.Errorf("the service's secret = %q, want %q", got, name3)
	}
}

// foreignNetworkEngine makes a network with the reserved name that is not TierPlan's appear just before the first
// write of the job, as one made by someone else after the preflight read the swarm.
type foreignNetworkEngine struct {
	ingressdeploy.Engine
	once   sync.Once
	create func()
}

func (e *foreignNetworkEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	e.once.Do(e.create)
	return e.Engine.WriteOnManager(ctx, envID, fn)
}

// TestEnableRefusesForeignSameNameNetwork: a network that has the reserved name and not TierPlan's labels is
// never reused. The preflight refuses one it can see; when one appears after the preflight, the network item
// fails naming it, later items are skipped and ingress stays off.
func TestEnableRefusesForeignSameNameNetwork(t *testing.T) {
	var r *rig
	r = newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Engine = &foreignNetworkEngine{Engine: d.Engine, create: func() {
			spec := ingressdeploy.NetworkSpec("10.9.0.0/24")
			spec.Labels = nil
			if _, _, err := r.cli.NetworkCreate(context.Background(), spec); err != nil {
				t.Errorf("make the foreign network: %v", err)
			}
		}}
	}})
	job := r.enable(ingressdeploy.EnableRequest{})
	net := itemOf(job, ingressdeploy.ItemNetwork)
	if net.State != jobs.ItemFailed || net.Code != ingressdeploy.CodeEnableNetworkFailed {
		t.Fatalf("network item = %q %q, want failed enable_network_failed", net.State, net.Code)
	}
	if !strings.Contains(net.Detail, ingress.NetworkName) {
		t.Errorf("the item's detail %q does not name the object", net.Detail)
	}
	for _, id := range []string{ingressdeploy.ItemService, ingressdeploy.ItemSwitch} {
		if it := itemOf(job, id); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
			t.Errorf("item %s = %q %q, want skipped not_run", id, it.State, it.Code)
		}
	}
	if got := len(r.fake.NetworkCreates()); got != 1 {
		t.Errorf("network creates = %d, want only the foreign one", got)
	}
	if got := len(r.fake.ServiceCreates()); got != 0 {
		t.Errorf("service creates = %d, want none", got)
	}
	if state, _ := r.istore.DeployState(testsupport.Context(t), r.envID); state.Enabled {
		t.Error("ingress was switched on")
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is still held after the failed job")
	}
}
