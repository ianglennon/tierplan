// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// afterReadEngine runs hook once, right after the next cluster read has returned: the moment between the ensure loop's plan,
// which it makes lock-free, and its write.
type afterReadEngine struct {
	ingressdeploy.Engine
	mu   sync.Mutex
	hook func()
}

func (e *afterReadEngine) arm(fn func()) {
	e.mu.Lock()
	e.hook = fn
	e.mu.Unlock()
}

func (e *afterReadEngine) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	err := e.Engine.ClusterRead(ctx, envID, fn)
	e.mu.Lock()
	h := e.hook
	e.hook = nil
	e.mu.Unlock()
	if h != nil {
		h()
	}
	return err
}

// TestEnsureDropsItsWriteWhenTheSwitchChangesBeforeTheLock: a pass that planned a write on a world where ingress was on
// finds, once it holds the lock, that a switch-off (or a restore's pause) landed in between, and writes nothing: the
// service it meant to make is not made, the network and the secret are not touched, and a switched-off environment gets no
// ensure record (WR-05).
func TestEnsureDropsItsWriteWhenTheSwitchChangesBeforeTheLock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(r *rig)
		wantClass string
		recorded  bool
	}{
		{"a switch-off lands", func(r *rig) {
			if _, err := r.istore.SetEnabled(testsupport.Context(r.t), r.envID, false, "u-admin", nil); err != nil {
				r.t.Fatalf("SetEnabled: %v", err)
			}
		}, "", false},
		{"a restore pauses it", func(r *rig) {
			if _, err := r.istore.SetPaused(testsupport.Context(r.t), r.envID, true, "system:restore", nil); err != nil {
				r.t.Fatalf("SetPaused: %v", err)
			}
		}, ingressdeploy.EnsurePaused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hooks *afterReadEngine
			r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
				hooks = &afterReadEngine{Engine: d.Engine}
				d.Engine = hooks
			}})
			r.removeService()
			hooks.arm(func() { tc.change(r) })

			written := len(r.fake.Writes())
			r.round()
			if got := len(r.fake.Writes()); got != written {
				t.Fatalf("a pass whose plan went stale wrote %v", r.fake.Writes()[written:])
			}
			if _, ok := r.service(); ok {
				t.Error("the service was made after the environment's switch changed")
			}
			at, class := r.ensureState()
			if tc.recorded != !at.IsZero() || class != tc.wantClass {
				t.Errorf("ensure record = %v %q, want recorded %v with class %q", at, class, tc.recorded, tc.wantClass)
			}
			if _, held := r.locks.Held(r.envID); held {
				t.Error("the pass kept the lock")
			}
		})
	}
}

// TestEnsureReplansUnderTheLock: a pass whose plan went stale for another reason, the service came back while it waited for
// the lock, makes no write either: the plan is made again from a fresh read once the lock is held (WR-05).
func TestEnsureReplansUnderTheLock(t *testing.T) {
	var hooks *afterReadEngine
	r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		hooks = &afterReadEngine{Engine: d.Engine}
		d.Engine = hooks
	}})
	spec := r.liveSpec()
	r.removeService()
	creates := len(r.fake.ServiceCreates())
	hooks.arm(func() {
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Errorf("encode the spec: %v", err)
			return
		}
		if _, _, err := r.cli.ServiceCreate(testsupport.Context(t), raw, ""); err != nil {
			t.Errorf("ServiceCreate: %v", err)
		}
	})
	r.round()
	if got := len(r.fake.ServiceCreates()); got != creates+1 {
		t.Errorf("service creates = %d, want only the one made behind the pass's back (%d)", got-creates, 1)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class = %q, want ok", class)
	}
}

// urlVar is an ingress URL a test changes while the deployer runs.
type urlVar struct{ v atomic.Value }

func newURLVar(s string) *urlVar { u := &urlVar{}; u.v.Store(s); return u }
func (u *urlVar) get() string {
	s, _ := u.v.Load().(string)
	return s
}
func (u *urlVar) set(s string) { u.v.Store(s) }

// TestEnsureNeverWritesAnUnusableURL: when the ingress URL is empty or is not a wss:// address with a host, the loop records
// url_unset and writes nothing, whether the service is gone or running: the running proxies keep the spec they have. Once
// the URL is back the next pass puts the service right (WR-06).
func TestEnsureNeverWritesAnUnusableURL(t *testing.T) {
	good := "wss://tierplan.example:7480/ingress/connect"
	for _, bad := range []string{"", "http://tierplan.example/ingress/connect", "wss://", "wss://user@tierplan.example/ingress/connect", "wss://tierplan.example/ingress/connect?x=1"} {
		t.Run("url "+bad, func(t *testing.T) {
			u := newURLVar(good)
			r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.URL = u.get }})
			u.set(bad)

			// The service is running with the good URL: no update is sent to it.
			written := len(r.fake.Writes())
			r.round()
			if got := len(r.fake.Writes()); got != written {
				t.Fatalf("a pass with the URL %q wrote %v", bad, r.fake.Writes()[written:])
			}
			if _, class := r.ensureState(); class != ingressdeploy.EnsureURLUnset {
				t.Errorf("class = %q, want %q", class, ingressdeploy.EnsureURLUnset)
			}
			for _, e := range r.liveSpec().TaskTemplate.ContainerSpec.Env {
				if e == "TIERPLAN_INGRESS_URL=" || e == "TIERPLAN_INGRESS_URL="+bad {
					t.Errorf("the live service carries %q", e)
				}
			}

			// The service is gone: it is not made without an address either.
			r.removeService()
			r.round()
			if _, ok := r.service(); ok {
				t.Error("the service was made with an unusable URL")
			}
			if got := len(r.fake.Writes()); got != written+1 {
				t.Errorf("writes = %d, want only the test's own removal beyond %d", got, written)
			}

			u.set(good)
			r.round()
			if _, ok := r.service(); !ok {
				t.Error("the service was not made once the URL was back")
			}
			if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
				t.Errorf("class after the URL came back = %q, want ok", class)
			}
		})
	}
}

// advClock is a clock a test moves by hand.
type advClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *advClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}
func (c *advClock) After(time.Duration) <-chan time.Time { return nil }
func (c *advClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// TestEnsureDoesNotRetryARolledBackUpdate: an update the swarm rolled back is not written again, tick after tick, while the
// spec this server wants is the same one: the pass records update_failed and sends nothing, so the stop-first update does not
// take a node's proxy down each minute. A different wanted spec is written at once, and the same one again after
// UpdateRetryAfter, so a cause that has gone is not held against the proxy for ever (WR-07).
func TestEnsureDoesNotRetryARolledBackUpdate(t *testing.T) {
	u := newURLVar("wss://tierplan.example:7480/ingress/connect")
	clock := &advClock{at: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	r := enabledRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.URL = u.get; d.Clock = clock }})
	before := r.liveSpec()

	// The URL changes: the loop updates the service, and the swarm rolls the update back.
	u.set("wss://other.example:7480/ingress/connect")
	r.round()
	if got := len(r.fake.ServiceUpdates()); got != 1 {
		t.Fatalf("service updates = %d, want the one update", got)
	}
	r.fake.ReplaceServiceSpec(ingress.ServiceName, before)
	r.fake.SetUpdateState(ingress.ServiceName, "rollback_completed", "update rolled back")

	for range 3 {
		r.round()
	}
	if got := len(r.fake.ServiceUpdates()); got != 1 {
		t.Fatalf("service updates after three more passes = %d, want the rolled-back update not retried", got)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureUpdateFailed {
		t.Errorf("class = %q, want %q", class, ingressdeploy.EnsureUpdateFailed)
	}

	// Just short of the retry time, nothing; at it, the same spec is tried once more.
	clock.advance(ingressdeploy.UpdateRetryAfter - time.Second)
	r.round()
	if got := len(r.fake.ServiceUpdates()); got != 1 {
		t.Errorf("service updates just before the retry time = %d, want 1", got)
	}
	clock.advance(2 * time.Second)
	r.round()
	if got := len(r.fake.ServiceUpdates()); got != 2 {
		t.Fatalf("service updates after the retry time = %d, want the retry", got)
	}
	r.fake.ReplaceServiceSpec(ingress.ServiceName, before)
	r.round()
	if got := len(r.fake.ServiceUpdates()); got != 2 {
		t.Errorf("service updates right after the retry rolled back = %d, want it held again", got)
	}

	// A spec that differs from the failed one is written at once.
	u.set("wss://third.example:7480/ingress/connect")
	r.round()
	if got := len(r.fake.ServiceUpdates()); got != 3 {
		t.Fatalf("service updates after the wanted spec changed = %d, want a new update", got)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class after a new update = %q, want ok", class)
	}
}

// TestEnsureRollbackCompletedSettlesStaleSecrets: a service whose last update was rolled back has settled, so the token
// secrets of earlier generations are removed (WR-07); but the secret the rolled-back service still references is kept,
// even when it is not the current generation, since the manager would otherwise be left with a service whose secret is gone.
func TestEnsureRollbackCompletedSettlesStaleSecrets(t *testing.T) {
	t.Run("the live spec is the wanted one", func(t *testing.T) {
		r := enabledRig(t, rigOptions{})
		ctx := testsupport.Context(t)
		st1, _ := r.tokens.TokenStatus(ctx, r.envID)
		if err := r.tokens.DisableToken(ctx, testActor, r.envID, "", ""); err != nil {
			t.Fatal(err)
		}
		wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
		st2, _ := r.tokens.TokenStatus(ctx, r.envID)
		name2 := ingressdeploy.SecretName(r.envID, 2, st2.ID)
		if names := secretNames(r); len(names) != 2 {
			t.Fatalf("secrets after the enable = %v, want both generations (%s is the old one)", names, ingressdeploy.SecretName(r.envID, 1, st1.ID))
		}
		r.fake.SetUpdateState(ingress.ServiceName, "rollback_completed", "an earlier update was rolled back")

		r.round()
		if names := secretNames(r); len(names) != 1 || names[0] != name2 {
			t.Fatalf("secrets after the pass = %v, want only %s", names, name2)
		}
	})

	t.Run("the live spec still references the older secret", func(t *testing.T) {
		r := enabledRig(t, rigOptions{})
		ctx := testsupport.Context(t)
		st1, _ := r.tokens.TokenStatus(ctx, r.envID)
		name1 := ingressdeploy.SecretName(r.envID, 1, st1.ID)
		old := r.liveSpec()
		if err := r.tokens.DisableToken(ctx, testActor, r.envID, "", ""); err != nil {
			t.Fatal(err)
		}
		wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
		// The update to the new token was rolled back: the service names the first secret again.
		r.fake.ReplaceServiceSpec(ingress.ServiceName, old)
		r.fake.SetUpdateState(ingress.ServiceName, "rollback_completed", "update rolled back")

		written := len(r.fake.Writes())
		r.round()
		if got := len(r.fake.Writes()); got != written {
			t.Errorf("the pass wrote %v: the rolled-back update of the same spec must not be retried, and nothing is stale", r.fake.Writes()[written:])
		}
		if names := secretNames(r); len(names) != 2 || !contains(names, name1) {
			t.Errorf("secrets = %v, want the secret the live service references (%s) kept", names, name1)
		}
		if _, class := r.ensureState(); class != ingressdeploy.EnsureUpdateFailed {
			t.Errorf("class = %q, want %q", class, ingressdeploy.EnsureUpdateFailed)
		}
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
