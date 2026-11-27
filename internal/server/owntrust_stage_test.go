// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/x509"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// stageRig is a self-signed source served by a runtime, an ACME installer over them and a trust set over the runtime, all on
// one step clock, as Build wires them. installs counts the installs and installedAt is the clock at the last one.
type stageRig struct {
	clk  *stepClock
	src  *certSource
	rt   *httpsRuntime
	tr   *ownTrust
	inst *acmeInstaller
	dir  string

	mu          sync.Mutex
	installs    int
	installedAt time.Time
}

func newStageRig(t *testing.T) *stageRig {
	t.Helper()
	f := newSSFixture(t)
	clk := newStepClock(fixedNow)
	r := &stageRig{clk: clk, dir: f.cfg.DataDir}
	r.src = f.source(t, "https://a.example.test", "", fixedNow)
	r.rt = newHTTPSRuntime()
	r.rt.Install(tlsKindSelfSigned, r.src.GetCertificate)
	r.tr = newOwnTrust(r.rt.servedChain, r.src.Leaf, nil).useClock(clk)
	r.rt.AddOnInstall(func() {
		r.tr.Observe()
		r.mu.Lock()
		r.installs++
		r.installedAt = clk.Now()
		r.mu.Unlock()
	})
	r.inst = installer(r.rt, r.src, r.dir)
	r.inst.trust = r.tr
	r.inst.waitIn(t.Context())
	return r
}

func (r *stageRig) installed() (int, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installs, r.installedAt
}

// stagedView is what applied saw on a call: the version it was asked about, whether the trust set held the incoming
// leaf, and whether the incoming leaf was being served already.
type stagedView struct {
	version int64
	holds   bool
	served  bool
}

// watchApplied returns an applied func that records what it saw and answers ready(clock now), and the recorded views.
func (r *stageRig) watchApplied(t *testing.T, leaf *x509.Certificate, ready func(time.Time) bool) (func(int64) bool, func() []stagedView) {
	t.Helper()
	var mu sync.Mutex
	var seen []stagedView
	applied := func(v int64) bool {
		data, _ := r.tr.PEM()
		_, chain := r.rt.servedChain()
		view := stagedView{
			version: v,
			holds:   slices.ContainsFunc(pemCerts(t, data), leaf.Equal),
			served:  len(chain) > 0 && chain[0].Equal(leaf),
		}
		mu.Lock()
		seen = append(seen, view)
		mu.Unlock()
		return ready(r.clk.Now())
	}
	return applied, func() []stagedView {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// serveACME runs ServeACME in the background and returns a channel that delivers its error.
func serveACME(inst *acmeInstaller, chainPEM, keyPEM []byte) <-chan error {
	done := make(chan error, 1)
	go func() { done <- inst.ServeACME(chainPEM, keyPEM) }()
	return done
}

func TestStagedInstallWaitsForTheProxies(t *testing.T) {
	t.Run("the install waits until the proxies applied the staged trust", func(t *testing.T) {
		r := newStageRig(t)
		chainPEM, keyPEM, le, issuer := acmeChain(t, "le.example.test")
		readyAt := fixedNow.Add(3 * time.Second)
		applied, seen := r.watchApplied(t, le, func(now time.Time) bool { return !now.Before(readyAt) })
		r.inst.applied = applied

		done := serveACME(r.inst, chainPEM, keyPEM)
		// Six polls of 500 ms reach 3 s; before each, nothing is installed.
		for range 6 {
			r.clk.WaitArmed(t)
			if n, _ := r.installed(); n != 0 {
				t.Fatalf("installed %d times at %s, before the proxies applied the staged trust", n, r.clk.Now().Sub(fixedNow))
			}
			r.clk.Advance(ownTrustStagePoll)
		}
		if err := testsupport.Await(t, "ServeACME to return", done); err != nil {
			t.Fatal(err)
		}

		n, at := r.installed()
		if n != 1 {
			t.Fatalf("installed %d times, want once", n)
		}
		if !at.Equal(readyAt) {
			t.Errorf("installed at %s, want at %s, right after the proxies applied the trust", at.Sub(fixedNow), readyAt.Sub(fixedNow))
		}
		views := seen()
		if len(views) == 0 {
			t.Fatal("applied was never asked")
		}
		for i, v := range views {
			if !v.holds {
				t.Errorf("call %d: the trust set did not hold the incoming leaf while the installer waited: Stage did not come first", i)
			}
			if v.served {
				t.Errorf("call %d: the incoming leaf was already served while the installer waited", i)
			}
			if v.version != views[0].version {
				t.Errorf("call %d asked about version %d, want the staged version %d throughout", i, v.version, views[0].version)
			}
		}
		if !r.tr.has(t, le) || !r.tr.has(t, issuer) || !r.tr.has(t, r.src.Leaf()) {
			t.Error("after the install the set must hold the anchor and the served chain")
		}
	})

	t.Run("with no environment named the install does not wait and Stage still adds the chain", func(t *testing.T) {
		r := newStageRig(t)
		chainPEM, keyPEM, le, _ := acmeChain(t, "le.example.test")
		// The server's applied answers true at once when the holder names no environment.
		applied, seen := r.watchApplied(t, le, func(time.Time) bool { return true })
		r.inst.applied = applied
		if err := r.inst.ServeACME(chainPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		if n, at := r.installed(); n != 1 || !at.Equal(fixedNow) {
			t.Errorf("installed %d times at %s, want once at once", n, at.Sub(fixedNow))
		}
		views := seen()
		if len(views) != 1 || !views[0].holds || views[0].served {
			t.Errorf("applied saw %+v, want one call with the incoming leaf staged and not yet served", views)
		}
		r.clk.mu.Lock()
		timers := len(r.clk.timers)
		r.clk.mu.Unlock()
		if timers != 0 {
			t.Errorf("%d timers armed, want none: nothing is waited for", timers)
		}
	})

	t.Run("a nil trust and a nil applied install at once", func(t *testing.T) {
		r := newStageRig(t)
		r.inst.trust = nil
		chainPEM, keyPEM, _, _ := acmeChain(t, "le.example.test")
		if err := r.inst.ServeACME(chainPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		if n, _ := r.installed(); n != 1 {
			t.Errorf("installed %d times, want once", n)
		}
	})

	t.Run("before waitIn an install stages without waiting", func(t *testing.T) {
		r := newStageRig(t)
		r.inst.stageCtx.Store(nil)
		var asked atomic.Int32
		r.inst.applied = func(int64) bool { asked.Add(1); return false }
		chainPEM, keyPEM, le, _ := acmeChain(t, "le.example.test")
		if err := r.inst.ServeACME(chainPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		if n := asked.Load(); n != 0 {
			t.Errorf("applied was asked %d times before the server runs, want none", n)
		}
		if n, _ := r.installed(); n != 1 || !r.tr.has(t, le) {
			t.Error("the start-time install must install at once with the chain in the set")
		}
	})
}

func TestOwnTrustStagedChainLeavesWhenNeverServed(t *testing.T) {
	s, a, b := trustCert(t, "self.example.test"), trustCert(t, "a.example.test"), trustCert(t, "b.example.test")
	r := newTrustRig(s, a)
	_, v0 := r.trust.PEM()

	v1 := r.trust.Stage([]*x509.Certificate{b})
	if v1 == v0 || !r.trust.has(t, b) {
		t.Fatal("Stage must add the incoming certificate and move the version")
	}
	if again := r.trust.Stage([]*x509.Certificate{b}); again != v1 {
		t.Errorf("staging the same certificate again moved the version to %d, want %d", again, v1)
	}

	// Never served: it leaves at its time, with no grace period of its own.
	r.now = fixedNow.Add(ownTrustGrace)
	data, v2 := r.trust.PEM()
	if got := pemCerts(t, data); !sameCerts(got, s, a) {
		t.Errorf("after its time the set holds %v, want the anchor and the served chain only", serialsOf(t, data))
	}
	if v2 != v0 {
		t.Errorf("version = %d once the staged certificate left, want the version before it, %d", v2, v0)
	}
	r.now = fixedNow.Add(2 * ownTrustGrace)
	if r.trust.has(t, b) {
		t.Error("the staged certificate came back as previous")
	}
}

func TestOwnTrustStagedChainBecomesServed(t *testing.T) {
	s, a, b := trustCert(t, "self.example.test"), trustCert(t, "a.example.test"), trustCert(t, "b.example.test")
	r := newTrustRig(s, a)
	v1 := r.trust.Stage([]*x509.Certificate{b})

	// Served: b is an ordinary member, a is previous for the grace period, b's staged time no longer applies.
	r.chain = []*x509.Certificate{b}
	data, v2 := r.trust.PEM()
	if got := pemCerts(t, data); !sameCerts(got, s, b, a) {
		t.Errorf("after the swap the set holds %v, want the anchor, the served chain, then the previous one", serialsOf(t, data))
	}
	if v2 == v1 {
		t.Error("the version did not move when the order of the set changed")
	}
	r.now = fixedNow.Add(ownTrustGrace)
	data, _ = r.trust.PEM()
	if got := pemCerts(t, data); !sameCerts(got, s, b) {
		t.Errorf("after the grace period the set holds %v, want the anchor and the served chain", serialsOf(t, data))
	}
}

func TestWaitAppliedEndsWithItsContext(t *testing.T) {
	tr := newOwnTrust(nil, nil, nil).useClock(newStepClock(fixedNow))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := tr.waitApplied(ctx, 1, func(int64) bool { return false }); err == nil {
		t.Fatal("waitApplied with an ended context returned nil")
	}
}
