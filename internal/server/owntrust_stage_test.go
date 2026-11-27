// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingresshub"
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

func TestStagedInstallTimesOutAndInstalls(t *testing.T) {
	t.Run("proxies that never apply the trust hold the install for ownTrustStageWait, then it installs and warns", func(t *testing.T) {
		r := newStageRig(t)
		var logs lockedBuffer
		r.inst.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		chainPEM, keyPEM, le, _ := acmeChain(t, "le.example.test")
		applied, seen := r.watchApplied(t, le, func(time.Time) bool { return false })
		r.inst.applied = applied

		done := serveACME(r.inst, chainPEM, keyPEM)
		polls := int(ownTrustStageWait / ownTrustStagePoll)
		for range polls {
			r.clk.WaitArmed(t)
			if n, _ := r.installed(); n != 0 {
				t.Fatalf("installed at %s, before the bounded wait ended", r.clk.Now().Sub(fixedNow))
			}
			r.clk.Advance(ownTrustStagePoll)
		}
		if err := testsupport.Await(t, "ServeACME to return after the bounded wait", done); err != nil {
			t.Fatal(err)
		}
		n, at := r.installed()
		if n != 1 || !at.Equal(fixedNow.Add(ownTrustStageWait)) {
			t.Fatalf("installed %d times at %s, want once at %s", n, at.Sub(fixedNow), ownTrustStageWait)
		}
		views := seen()
		if len(views) == 0 {
			t.Fatal("applied was never asked")
		}
		var warn string
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") {
				warn = line
			}
		}
		if warn == "" {
			t.Fatalf("no warn line after the timeout; log:\n%s", logs.String())
		}
		if !strings.Contains(warn, "version="+strconv.FormatInt(views[0].version, 10)) || !strings.Contains(warn, "environment=") {
			t.Errorf("the warn line does not name the environment and the staged version %d: %s", views[0].version, warn)
		}
	})

	t.Run("a cancelled context installs at once", func(t *testing.T) {
		r := newStageRig(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r.inst.waitIn(ctx)
		r.inst.applied = func(int64) bool { return false }
		chainPEM, keyPEM, _, _ := acmeChain(t, "le.example.test")
		if err := r.inst.ServeACME(chainPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		if n, at := r.installed(); n != 1 || !at.Equal(fixedNow) {
			t.Errorf("installed %d times at %s, want once at once: the server is stopping", n, at.Sub(fixedNow))
		}
	})
}

// stagedSourceRig drives a certSource refresh whose beforeSet stages through an ownStaging on a step clock, with the proxies
// applying the staged trust readyAfter after the wait began.
type stagedSourceRig struct {
	clk *stepClock
	src *certSource
	tr  *ownTrust

	mu          sync.Mutex
	hooked      []*x509.Certificate // the incoming leaf of each beforeSet call
	whileServed []*x509.Certificate // the leaf the source served on each beforeSet call
}

func newStagedSourceRig(t *testing.T, src *certSource, readyAfter time.Duration) *stagedSourceRig {
	t.Helper()
	clk := newStepClock(fixedNow)
	r := &stagedSourceRig{clk: clk, src: src}
	rt := newHTTPSRuntime()
	rt.Install(tlsKindSelfSigned, src.GetCertificate)
	r.tr = newOwnTrust(rt.servedChain, nil, nil).useClock(clk)
	readyAt := fixedNow.Add(readyAfter)
	stg := &ownStaging{
		trust:   r.tr,
		applied: func(int64) bool { return !clk.Now().Before(readyAt) },
		env:     func() string { return "env-self" },
		log:     slog.New(slog.DiscardHandler),
	}
	src.beforeSet = func(ctx context.Context, cert tls.Certificate) {
		chain := certChain(cert)
		r.mu.Lock()
		r.hooked = append(r.hooked, chain[0])
		r.whileServed = append(r.whileServed, src.Leaf())
		r.mu.Unlock()
		stg.stage(ctx, chain, true)
	}
	return r
}

// refresh runs one Refresh at the given instant and moves the step clock through the staged wait, checking before each
// step that the source still serves before. It returns the leaf served after the refresh.
func (r *stagedSourceRig) refresh(t *testing.T, at time.Time, before *x509.Certificate, polls int, still func(t *testing.T)) *x509.Certificate {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.src.Refresh(at)
	}()
	for range polls {
		r.clk.WaitArmed(t)
		if !r.src.Leaf().Equal(before) {
			t.Fatalf("the new certificate was served at %s, before the proxies applied the staged trust", r.clk.Now().Sub(fixedNow))
		}
		if still != nil {
			still(t)
		}
		r.clk.Advance(ownTrustStagePoll)
	}
	testsupport.Await(t, "the refresh to finish", done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hooked) != 1 {
		t.Fatalf("beforeSet was called %d times, want once", len(r.hooked))
	}
	if !r.whileServed[0].Equal(before) {
		t.Error("beforeSet was called after the new certificate was set")
	}
	after := r.src.Leaf()
	if !after.Equal(r.hooked[0]) {
		t.Error("the certificate served after the refresh is not the one beforeSet was given")
	}
	if !slices.ContainsFunc(pemCerts(t, mustPEM(r.tr)), after.Equal) {
		t.Error("the served certificate is not in the trust set")
	}
	return after
}

func mustPEM(tr *ownTrust) []byte {
	data, _ := tr.PEM()
	return data
}

func TestEveryCertificatePathIsStaged(t *testing.T) {
	t.Run("the fallback to self-signed", func(t *testing.T) {
		r := newStageRig(t)
		chainPEM, keyPEM, le, _ := acmeChain(t, "le.example.test")
		r.inst.applied = func(int64) bool { return true }
		if err := r.inst.ServeACME(chainPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		self := r.src.Leaf()
		readyAt := r.clk.Now().Add(time.Second)
		applied, seen := r.watchApplied(t, self, func(now time.Time) bool { return !now.Before(readyAt) })
		r.inst.applied = applied

		done := make(chan error, 1)
		go func() { done <- r.inst.ServeSelfSigned() }()
		for range 2 {
			r.clk.WaitArmed(t)
			if r.rt.Kind() != tlsKindACME {
				t.Fatal("the self-signed certificate was served before the proxies applied the staged trust")
			}
			r.clk.Advance(ownTrustStagePoll)
		}
		if err := testsupport.Await(t, "ServeSelfSigned to return", done); err != nil {
			t.Fatal(err)
		}
		if r.rt.Kind() != tlsKindSelfSigned {
			t.Fatal("the fallback did not serve the self-signed certificate")
		}
		if n, at := r.installed(); n != 2 || !at.Equal(readyAt) {
			t.Errorf("%d installs, the last at %s, want the fallback installed at %s", n, at.Sub(fixedNow), readyAt.Sub(fixedNow))
		}
		views := seen()
		if len(views) == 0 || !views[0].holds || views[0].served {
			t.Errorf("applied saw %+v, want the self-signed leaf staged while Let's Encrypt was still served", views)
		}
		// Pitfall 4: the Let's Encrypt chain stays as previous, so the return from the fallback is staged the same way.
		if !r.tr.has(t, le) {
			t.Error("the Let's Encrypt chain left the set at the fallback")
		}
	})

	t.Run("a self-signed re-sign for a new public URL", func(t *testing.T) {
		f := newSSFixture(t)
		var public atomic.Pointer[string]
		first, next := "https://a.example.test", "https://new.example.test"
		public.Store(&first)
		src, _, err := newCertSource(context.Background(), f.cfg, f.store, func() string { return *public.Load() }, str(""), nowFn(fixedNow))
		if err != nil {
			t.Fatal(err)
		}
		r := newStagedSourceRig(t, src, 2*time.Second)
		old := src.Leaf()
		mirror := filepath.Join(f.cfg.DataDir, selfSignedDir, mirrorCertFile)
		oldMirror, err := os.ReadFile(mirror) //nolint:gosec // a path inside the test's temporary directory
		if err != nil {
			t.Fatal(err)
		}
		f.store.mu.Lock()
		updates := f.store.updates
		f.store.mu.Unlock()

		public.Store(&next)
		// Neither the row nor the mirror may name the new certificate while the old one is served.
		still := func(t *testing.T) {
			t.Helper()
			if got, _ := os.ReadFile(mirror); !bytes.Equal(got, oldMirror) { //nolint:gosec // a path inside the test's temporary directory
				t.Fatal("the mirror holds the new certificate before it is served")
			}
			f.store.mu.Lock()
			defer f.store.mu.Unlock()
			if f.store.updates != updates {
				t.Fatal("the row was written before the new certificate is served")
			}
		}
		now := r.refresh(t, fixedNow.Add(2*selfSignedNameCheck), old, 4, still)
		if now.Equal(old) {
			t.Fatal("the source did not sign a new certificate")
		}
		if got, _ := os.ReadFile(mirror); bytes.Equal(got, oldMirror) { //nolint:gosec // a path inside the test's temporary directory
			t.Error("the mirror was not written once the new certificate was served")
		}
	})

	t.Run("a supplied pair replaced on disk", func(t *testing.T) {
		dir := t.TempDir()
		crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
		a := writePair(t, crt, key, "a.example.com")
		src, err := newSuppliedSource(crt, key, nowFn(fixedNow))
		if err != nil {
			t.Fatal(err)
		}
		r := newStagedSourceRig(t, src, 3*time.Second)
		b := writePair(t, crt, key, "b.example.com")
		if now := r.refresh(t, fixedNow.Add(2*suppliedRecheck), a, 6, nil); !now.Equal(b) {
			t.Error("the replaced pair is not served after the refresh")
		}
	})

	t.Run("a refresh that finds nothing new does not call beforeSet", func(t *testing.T) {
		f := newSSFixture(t)
		src := f.source(t, "https://a.example.test", "", fixedNow)
		var calls atomic.Int32
		src.beforeSet = func(context.Context, tls.Certificate) { calls.Add(1) }
		src.Refresh(fixedNow.Add(selfSignedNameCheck))
		if n := calls.Load(); n != 0 {
			t.Errorf("beforeSet was called %d times for an unchanged certificate", n)
		}
	})
}

func TestProxiesAppliedCountsConnectedActiveNodes(t *testing.T) {
	live := func(id string) bool { return id != "gone" }
	nodes := []ingresshub.Node{
		{ID: "a", Status: "active", AppliedBundleID: "b2"},
		{ID: "gone", Status: "active", AppliedBundleID: "b1"},
		{ID: "old", Status: "active", AppliedBundleID: "b1", Incompatible: true},
		{ID: "revoked", Status: "revoked", AppliedBundleID: "b0"},
	}
	if !proxiesApplied(nodes, "b2", live) {
		t.Error("a node that is not connected, an incompatible node or a revoked node held the wait")
	}
	nodes = append(nodes, ingresshub.Node{ID: "c", Status: "active", AppliedBundleID: "b1"})
	if proxiesApplied(nodes, "b2", live) {
		t.Error("a connected active node on the previous bundle did not hold the wait")
	}
	if !proxiesApplied(nil, "b2", live) {
		t.Error("an environment with no proxy node must not wait")
	}
}
