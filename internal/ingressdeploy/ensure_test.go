// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// manualClock is a clock a test fires by hand: After hands out a channel the test sends on, and says it armed one so a
// test never fires a timer the loop has not yet set.
type manualClock struct {
	at    time.Time
	armed chan struct{}
	mu    sync.Mutex
	timer chan time.Time
	// waits are the durations the loop armed, in order.
	waits []time.Duration
}

func newManualClock() *manualClock {
	return &manualClock{at: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), armed: make(chan struct{}, 1)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance moves the clock on by d.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// lastWait is the duration of the timer the loop armed last.
func (c *manualClock) lastWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waits) == 0 {
		return 0
	}
	return c.waits[len(c.waits)-1]
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.timer = ch
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return ch
}

// fire ends the timer the loop armed last.
func (c *manualClock) fire() {
	c.mu.Lock()
	ch, at := c.timer, c.at
	c.mu.Unlock()
	ch <- at
}

// countingEngine counts every call the deployer makes of the engine.
type countingEngine struct {
	ingressdeploy.Engine
	calls atomic.Int32
}

func (e *countingEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	e.calls.Add(1)
	return e.Engine.WriteOnManager(ctx, envID, fn)
}

func (e *countingEngine) WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	e.calls.Add(1)
	return e.Engine.WriteOnNode(ctx, envID, nodeID, fn)
}

func (e *countingEngine) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	e.calls.Add(1)
	return e.Engine.ClusterRead(ctx, envID, fn)
}

func (e *countingEngine) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	e.calls.Add(1)
	return e.Engine.OnNode(ctx, envID, nodeID, fn)
}

// enabledRig is a rig whose ingress an enable has switched on.
func enabledRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	r := newRig(t, o)
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	return r
}

// round takes one round of the ensure loop.
func (r *rig) round() {
	r.t.Helper()
	r.svc.Round(testsupport.Context(r.t))
}

// liveSpec is the proxy service's spec as the fake swarm holds it.
func (r *rig) liveSpec() swarm.ServiceSpec {
	r.t.Helper()
	d, ok := r.service()
	if !ok {
		r.t.Fatal("the swarm holds no proxy service")
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(d.Spec, &spec); err != nil {
		r.t.Fatalf("decode the live spec: %v", err)
	}
	return spec
}

// ensureState is the last ensure pass recorded for the rig's environment.
func (r *rig) ensureState() (at time.Time, class string) {
	r.t.Helper()
	st, err := r.istore.DeployState(testsupport.Context(r.t), r.envID)
	if err != nil {
		r.t.Fatalf("DeployState: %v", err)
	}
	return st.EnsureAt, st.EnsureError
}

// removeService takes the proxy service away from the fake swarm.
func (r *rig) removeService() {
	r.t.Helper()
	d, ok := r.service()
	if !ok {
		r.t.Fatal("the swarm holds no proxy service")
	}
	if err := r.cli.ServiceRemove(testsupport.Context(r.t), d.ID); err != nil {
		r.t.Fatalf("ServiceRemove: %v", err)
	}
}

// TestEnsureRepairsAndIsQuiet is the tracer: with the loop running, the proxy service that is removed comes back on a
// wake with the spec it had and the secret reference, a pass over an unchanged swarm writes nothing, and the loop's
// own tick takes a pass too.
func TestEnsureRepairsAndIsQuiet(t *testing.T) {
	clock := newManualClock()
	r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.Clock = clock }})
	rounds := make(chan struct{}, 16)
	r.svc.SetRoundHook(func() {
		select {
		case rounds <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.svc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The pass at start finds the swarm as the enable left it and writes nothing.
	written := len(r.fake.Writes())
	testsupport.Await(t, "the first round", rounds)
	testsupport.Await(t, "the timer", clock.armed)
	if got := len(r.fake.Writes()); got != written {
		t.Fatalf("a pass over an unchanged swarm wrote %v", r.fake.Writes()[written:])
	}
	if at, class := r.ensureState(); !at.Equal(clock.at) || class != ingressdeploy.EnsureOK {
		t.Fatalf("ensure state = %v %q, want %v and ok", at, class, clock.at)
	}
	first := r.fake.ServiceCreates()
	if len(first) != 1 {
		t.Fatalf("service creates = %d, want 1", len(first))
	}

	// The service is removed; a wake brings it back as it was.
	r.removeService()
	r.svc.Wake()
	testsupport.Await(t, "the round after the wake", rounds)
	creates := r.fake.ServiceCreates()
	if len(creates) != 2 {
		t.Fatalf("service creates after the repair = %d, want 2", len(creates))
	}
	if d := firstDiff("spec", asTree(t, creates[0].Spec), asTree(t, creates[1].Spec)); d != "" {
		t.Errorf("the repaired service differs from the first at %s", d)
	}
	if got := creates[1].Spec.TaskTemplate.ContainerSpec.Secrets[0].SecretName; got == "" {
		t.Error("the repaired service carries no secret reference")
	}
	if len(r.fake.SecretCreates()) != 1 || len(r.fake.NetworkCreates()) != 1 {
		t.Errorf("the repair made %d secrets and %d networks, want it to reuse both", len(r.fake.SecretCreates()), len(r.fake.NetworkCreates()))
	}

	// A further pass over the repaired swarm writes nothing, on a wake and on the tick.
	written = len(r.fake.Writes())
	r.svc.Wake()
	testsupport.Await(t, "the round after a second wake", rounds)
	clock.fire()
	testsupport.Await(t, "the round after the tick", rounds)
	testsupport.Await(t, "the timer", clock.armed)
	if got := len(r.fake.Writes()); got != written {
		t.Fatalf("passes over a repaired swarm wrote %v", r.fake.Writes()[written:])
	}
}

// TestEnsureRetriesSoonAfterStart (28-UAT 9a): right after start the agents have not reconnected, so the first pass finds no
// manager. The loop then tries again after EnsureStartupRetry, not a whole EnsureTick later, so TierPlan's own network is
// observed within seconds of the manager coming back; once a pass reads the swarm, and after the start-up window, the loop
// keeps its minute.
func TestEnsureRetriesSoonAfterStart(t *testing.T) {
	clock := newManualClock()
	r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.Clock = clock }})
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
	rounds := make(chan struct{}, 16)
	r.svc.SetRoundHook(func() {
		select {
		case rounds <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.svc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	testsupport.Await(t, "the first round", rounds)
	testsupport.Await(t, "the timer", clock.armed)
	if _, class := r.ensureState(); class != ingressdeploy.EnsureUnreachable {
		t.Fatalf("class = %q, want unreachable", class)
	}
	if got := clock.lastWait(); got != ingressdeploy.EnsureStartupRetry {
		t.Fatalf("after an unreachable pass at start the loop waits %v, want %v", got, ingressdeploy.EnsureStartupRetry)
	}

	// The manager is back: the retry reads the swarm, and the loop goes back to its minute.
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = true })
	clock.advance(ingressdeploy.EnsureStartupRetry)
	clock.fire()
	testsupport.Await(t, "the retry", rounds)
	testsupport.Await(t, "the timer", clock.armed)
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Fatalf("class = %q, want ok", class)
	}
	if got := clock.lastWait(); got != ingressdeploy.EnsureTick {
		t.Fatalf("after a pass that read the swarm the loop waits %v, want %v", got, ingressdeploy.EnsureTick)
	}

	// Past the start-up window an unreachable environment is tried once a minute, as before.
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
	clock.advance(ingressdeploy.EnsureStartupWindow)
	clock.fire()
	testsupport.Await(t, "the round after the window", rounds)
	testsupport.Await(t, "the timer", clock.armed)
	if _, class := r.ensureState(); class != ingressdeploy.EnsureUnreachable {
		t.Fatalf("class = %q, want unreachable", class)
	}
	if got := clock.lastWait(); got != ingressdeploy.EnsureTick {
		t.Fatalf("an unreachable pass after the start-up window waits %v, want %v", got, ingressdeploy.EnsureTick)
	}
}

// TestEnsureRollsImage: a proxy service on another image than this server's is updated in place at the version read,
// with the stop-first update config and the live force-update counter, and a pass after that writes nothing.
func TestEnsureRollsImage(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	live := r.liveSpec()
	live.TaskTemplate.ContainerSpec.Image = ingressdeploy.ImageRepository + ":0.0.1"
	live.TaskTemplate.ForceUpdate = 3
	if !r.fake.ReplaceServiceSpec(ingress.ServiceName, live) {
		t.Fatal("could not change the live service")
	}
	d, _ := r.service()

	r.round()
	ups := r.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("service updates = %d, want 1", len(ups))
	}
	if ups[0].Version != strconv.FormatUint(d.Version, 10) {
		t.Errorf("the update was sent at version %s, want the live %d", ups[0].Version, d.Version)
	}
	got := ups[0].Spec
	if want := ingressdeploy.IngressImage(testServerVersion); got.TaskTemplate.ContainerSpec.Image != want {
		t.Errorf("the update's image = %q, want %q", got.TaskTemplate.ContainerSpec.Image, want)
	}
	if got.UpdateConfig == nil || got.UpdateConfig.Order != swarm.UpdateOrderStopFirst {
		t.Errorf("the update config = %+v, want stop-first", got.UpdateConfig)
	}
	if got.TaskTemplate.ForceUpdate != 3 {
		t.Errorf("force update = %d, want the live 3 carried", got.TaskTemplate.ForceUpdate)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class = %q, want ok", class)
	}

	r.round()
	if got := len(r.fake.ServiceUpdates()); got != 1 {
		t.Errorf("a pass after the roll wrote another update (%d in all)", got)
	}
}

// TestEnsureSkipsWhenLocked: a pass that has something to write while a stack operation holds the environment writes
// nothing, records the class locked and never queues; the next pass after the lock is free makes the repair.
func TestEnsureSkipsWhenLocked(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	r.removeService()
	release, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackID: "s1", StackName: "web", By: "user:u2"})
	if !ok {
		t.Fatal("could not take the lock for the stack deploy")
	}
	written := len(r.fake.Writes())
	r.round()
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("a pass under a held lock wrote %v", r.fake.Writes()[written:])
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureLocked {
		t.Errorf("class = %q, want locked", class)
	}
	if held, isHeld := r.locks.Held(r.envID); !isHeld || held.Kind != "deploy" {
		t.Errorf("the lock holder = %+v (held %v), want the stack deploy still", held, isHeld)
	}
	release()

	r.round()
	if _, ok := r.service(); !ok {
		t.Error("the service was not made once the lock was free")
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class after the repair = %q, want ok", class)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("a pass kept the lock after it ended")
	}
}

// TestEnsureTakesTheLockOnlyToWrite: a pass over a swarm that is as wanted never takes the lock, so a stack deploy
// is never refused by it; one that writes holds it under HolderKindIngress while it does.
func TestEnsureTakesTheLockOnlyToWrite(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	var seen []stackops.Holder
	var mu sync.Mutex
	eng := &lockProbeEngine{Engine: r.eng, locks: r.locks, envID: r.envID, onWrite: func(h stackops.Holder, held bool) {
		mu.Lock()
		defer mu.Unlock()
		if held {
			seen = append(seen, h)
		}
	}}
	svc := ingressdeploy.New(ingressdeploy.Deps{
		Engine: eng, Envs: r.envs, Store: r.istore, Tokens: r.tokens, Jobs: r.hub, Locks: r.locks, Audit: nil,
		AddressSpaces: &fakeAddressing{cli: r.cli}, ServerVersion: testServerVersion,
		URL:   func() string { return "wss://tierplan.example:7480/ingress/connect" },
		Pins:  func() []string { return []string{"sha256/AAAA", "sha256/BBBB"} },
		Clock: fixedClock{at: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)},
	})
	svc.Round(testsupport.Context(t))
	if len(seen) != 0 {
		t.Fatalf("a quiet pass held the lock during %d calls", len(seen))
	}
	r.removeService()
	svc.Round(testsupport.Context(t))
	if len(seen) == 0 {
		t.Fatal("a pass that wrote did not hold the lock")
	}
	for _, h := range seen {
		if h.Kind != stackops.HolderKindIngress {
			t.Errorf("the holder during a write = %+v, want kind %s", h, stackops.HolderKindIngress)
		}
	}
}

// lockProbeEngine notes who holds the environment's lock at every write.
type lockProbeEngine struct {
	ingressdeploy.Engine
	locks   *stackops.Locks
	envID   string
	onWrite func(stackops.Holder, bool)
}

func (e *lockProbeEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	h, held := e.locks.Held(e.envID)
	e.onWrite(h, held)
	return e.Engine.WriteOnManager(ctx, envID, fn)
}

// TestEnsureRecordsState: every pass records when it ended and a class: ok, unreachable when no manager is
// connected, write_failed when a write is refused; none of them is the engine's own text.
func TestEnsureRecordsState(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	r.round()
	at, class := r.ensureState()
	if !at.Equal(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)) || class != ingressdeploy.EnsureOK {
		t.Fatalf("after a quiet pass: %v %q, want the clock's time and ok", at, class)
	}

	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
	written := len(r.fake.Writes())
	r.round()
	if _, class := r.ensureState(); class != ingressdeploy.EnsureUnreachable {
		t.Errorf("class with no manager = %q, want unreachable", class)
	}
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("a pass with no manager wrote %v", r.fake.Writes()[written:])
	}
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = true })

	live := r.liveSpec()
	live.TaskTemplate.ContainerSpec.Image = ingressdeploy.ImageRepository + ":0.0.1"
	r.fake.ReplaceServiceSpec(ingress.ServiceName, live)
	r.fake.RefuseServiceUpdates(ingress.ServiceName, "no secret in this refusal: the update is not accepted")
	r.round()
	if _, class := r.ensureState(); class != ingressdeploy.EnsureWriteFailed {
		t.Errorf("class after a refused write = %q, want write_failed", class)
	}
	if got := len(r.fake.ServiceUpdates()); got != 0 {
		t.Errorf("service updates = %d, want the refused one not kept", got)
	}
	r.fake.RefuseServiceUpdates(ingress.ServiceName, "")
	r.round()
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class after the next pass = %q, want ok: the next pass is the retry", class)
	}
}

// TestEnsureIgnoresDisabled: an environment whose ingress is off is never touched: the loop makes no call of the
// engine for it, even when the objects of an earlier enable are still there, and it gets no ensure record.
func TestEnsureIgnoresDisabled(t *testing.T) {
	var counter *countingEngine
	r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		counter = &countingEngine{Engine: d.Engine}
		d.Engine = counter
	}})
	r.round()
	if got := counter.calls.Load(); got != 0 {
		t.Fatalf("a round over an environment that never enabled made %d engine calls", got)
	}
	if st, _ := r.istore.DeployState(testsupport.Context(t), r.envID); !st.EnsureAt.IsZero() {
		t.Errorf("a pass recorded %v for an environment with ingress off", st.EnsureAt)
	}

	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	if _, err := r.istore.SetEnabled(testsupport.Context(t), r.envID, false, "u-admin", nil); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	r.removeService()
	before := counter.calls.Load()
	written := len(r.fake.Writes())
	r.round()
	if got := counter.calls.Load(); got != before {
		t.Errorf("a round over a switched-off environment made %d engine calls", got-before)
	}
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("a round over a switched-off environment wrote %v", r.fake.Writes()[written:])
	}
	if _, ok := r.service(); ok {
		t.Error("the loop made the service of an environment whose ingress is off")
	}
}

// TestEnsureRotatesMissingSecret: when the secret of the current token is gone the pass issues a new token, makes its
// secret and updates the service to name it (Pitfall 10).
func TestEnsureRotatesMissingSecret(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	st1, _ := r.tokens.TokenStatus(ctx, r.envID)

	// Somebody took the secret away: the service lets go of it first, as the engine insists.
	live := r.liveSpec()
	live.TaskTemplate.ContainerSpec.Secrets = nil
	r.fake.ReplaceServiceSpec(ingress.ServiceName, live)
	for _, s := range r.secrets() {
		if err := r.cli.SecretRemove(ctx, s.ID); err != nil {
			t.Fatalf("SecretRemove: %v", err)
		}
	}

	r.round()
	st2, _ := r.tokens.TokenStatus(ctx, r.envID)
	if st2.Generation != 2 || st2.ID == st1.ID {
		t.Fatalf("token after the secret was lost = %+v, want generation 2 with a new ID", st2)
	}
	name2 := ingressdeploy.SecretName(r.envID, 2, st2.ID)
	if names := secretNames(r); len(names) != 1 || names[0] != name2 {
		t.Fatalf("secrets = %v, want only %s", names, name2)
	}
	ups := r.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("service updates = %d, want 1", len(ups))
	}
	refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 || refs[0].SecretName != name2 || refs[0].SecretID == "" {
		t.Errorf("the service's secret references = %+v, want %s with its ID", refs, name2)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class = %q, want ok", class)
	}
	written := len(r.fake.Writes())
	r.round()
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("a pass after the rotation wrote %v", r.fake.Writes()[written:])
	}
}

// TestEnsureRemovesOldTokenSecrets: the secret of an earlier token generation, left behind by an enable that rotated
// the token, is removed by the pass that finds the service on the current one; the current secret stays, and nothing
// else is written after that.
func TestEnsureRemovesOldTokenSecrets(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	st1, _ := r.tokens.TokenStatus(ctx, r.envID)
	name1 := ingressdeploy.SecretName(r.envID, 1, st1.ID)
	if err := r.tokens.DisableToken(ctx, testActor, r.envID, "", ""); err != nil {
		t.Fatal(err)
	}
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	st2, _ := r.tokens.TokenStatus(ctx, r.envID)
	name2 := ingressdeploy.SecretName(r.envID, 2, st2.ID)
	if names := secretNames(r); len(names) != 2 {
		t.Fatalf("secrets after the enable = %v, want both generations", names)
	}

	r.round()
	if names := secretNames(r); len(names) != 1 || names[0] != name2 {
		t.Fatalf("secrets after the pass = %v, want only %s (not %s)", names, name2, name1)
	}
	written := len(r.fake.Writes())
	r.round()
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("a pass after the clean-up wrote %v", r.fake.Writes()[written:])
	}
}

// TestEnsureLeavesDisabledToken: a join token an admin disabled is not replaced by the loop, which would undo the
// disable; the swarm is left as it is.
func TestEnsureLeavesDisabledToken(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	if err := r.tokens.DisableToken(ctx, testActor, r.envID, "", ""); err != nil {
		t.Fatal(err)
	}
	written := len(r.fake.Writes())
	r.round()
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("a pass over a disabled token wrote %v", r.fake.Writes()[written:])
	}
	if st, _ := r.tokens.TokenStatus(ctx, r.envID); st.Generation != 1 || !st.Disabled {
		t.Errorf("token = %+v, want generation 1 still disabled", st)
	}
}

// TestPausedIsObserveOnly: a pass over an enabled environment a restore paused writes nothing and issues no token
// even when objects are missing, records the class paused, and audits ingress.deploy_paused once across passes.
func TestPausedIsObserveOnly(t *testing.T) {
	var counter *countingEngine
	r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		counter = &countingEngine{Engine: d.Engine}
		d.Engine = counter
	}})
	ctx := testsupport.Context(t)
	r.removeService()
	live := r.secrets()
	if len(live) != 1 {
		t.Fatalf("secrets = %d, want 1", len(live))
	}
	if _, err := r.istore.SetPaused(ctx, r.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	written := len(r.fake.Writes())
	calls := counter.calls.Load()
	for range 3 {
		r.round()
	}
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("passes over a paused environment wrote %v", r.fake.Writes()[written:])
	}
	if got := counter.calls.Load(); got != calls {
		t.Errorf("passes over a paused environment made %d engine calls, want it observed from the store alone", got-calls)
	}
	if st, _ := r.tokens.TokenStatus(ctx, r.envID); st.Generation != 1 {
		t.Errorf("token generation = %d, want 1: no token is issued while paused", st.Generation)
	}
	if _, ok := r.service(); ok {
		t.Error("the service was made while paused")
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsurePaused {
		t.Errorf("class = %q, want paused", class)
	}
	if got := r.auditCount("ingress.deploy_paused"); got != 1 {
		t.Errorf("ingress.deploy_paused audit rows = %d, want 1 across three passes", got)
	}

	// Resumed, the next pass puts the service back.
	if _, err := r.istore.SetPaused(ctx, r.envID, false, "u-admin", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	r.round()
	if _, ok := r.service(); !ok {
		t.Error("the service was not made after the resume")
	}
}
