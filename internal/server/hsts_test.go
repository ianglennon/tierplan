// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// hstsHost is the public host the HSTS tests serve and sign certificates for.
const hstsHost = "tierplan.example"

// hstsOverTLS asks the built server for /healthz over TLS with Host set to host.
func hstsOverTLS(t *testing.T, env map[string]string, host string, opts ...Option) string {
	t.Helper()
	return hstsFor(t, env, func(r *http.Request) {
		r.TLS = &tls.ConnectionState{}
		r.Host = host
	}, opts...)
}

// The toggle comes from TIERPLAN_HSTS: on over a certificate that verifies for the public host sends
// the first lifetime for that host and no other, off clears, and the self-signed certificate gets
// nothing whatever the variable says (D-10, D-14, T-16-29).
func TestHSTSFollowsTheEnvToggle(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &root, pkiFrom, pkiTo, hstsHost)
	crt, key := pkiWritePair(t, leaf)
	trusted := WithTrustRoots(pkiPool(root))
	files := func(extra map[string]string) map[string]string {
		env := map[string]string{
			"TIERPLAN_PUBLIC_URL":    "https://" + hstsHost,
			"TIERPLAN_TLS_CERT_FILE": crt,
			"TIERPLAN_TLS_KEY_FILE":  key,
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}

	t.Run("on, over TLS, for the public host", func(t *testing.T) {
		if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), hstsHost, trusted); got != "max-age=300" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=300", got)
		}
	})
	t.Run("on, with a port and capitals in the Host header", func(t *testing.T) {
		if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), "TierPlan.Example:8443", trusted); got != "max-age=300" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=300", got)
		}
	})
	t.Run("on, but plain HTTP", func(t *testing.T) {
		got := hstsFor(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), func(r *http.Request) { r.Host = hstsHost }, trusted)
		if got != "" {
			t.Errorf("Strict-Transport-Security = %q over plain HTTP, want none", got)
		}
	})
	t.Run("on, but another Host header", func(t *testing.T) {
		for _, host := range []string{"other.example", "evil." + hstsHost, hstsHost + ".evil.example", "example.com", ""} {
			if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), host, trusted); got != "" {
				t.Errorf("Host %q: Strict-Transport-Security = %q, want none", host, got)
			}
		}
	})
	t.Run("unset clears", func(t *testing.T) {
		if got := hstsOverTLS(t, files(nil), hstsHost, trusted); got != "max-age=0" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=0", got)
		}
	})
	t.Run("off clears", func(t *testing.T) {
		if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "off"}), hstsHost, trusted); got != "max-age=0" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=0", got)
		}
	})
	t.Run("self-signed sends nothing even when on", func(t *testing.T) {
		env := map[string]string{"TIERPLAN_PUBLIC_URL": "https://" + hstsHost, "TIERPLAN_HSTS": "on"}
		if got := hstsOverTLS(t, env, hstsHost); got != "" {
			t.Errorf("Strict-Transport-Security = %q, want none", got)
		}
	})
	t.Run("never a directive beyond max-age", func(t *testing.T) {
		got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), hstsHost, trusted)
		if got != "max-age=300" {
			t.Errorf("Strict-Transport-Security = %q, want exactly max-age=300", got)
		}
	})
}

// hstsStart is the moment the policy tests begin, inside the validity of every certificate they build.
var hstsStart = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

// hstsRig is a policy over a real migrated store, a clock the test moves and a runtime serving a
// certificate that a test root signed. It builds no server: the ramp and the withdrawal are decided by
// the policy alone, and the header tests above show the server wiring.
type hstsRig struct {
	t     *testing.T
	clock *acmetest.Clock
	st    *store.Store
	root  pkiCert
	rt    *httpsRuntime
	trust *trustChecker
	host  atomic.Pointer[string]
}

func newHSTSRig(t *testing.T) *hstsRig {
	t.Helper()
	g := &hstsRig{t: t, clock: acmetest.NewClock(hstsStart), st: storetest.New(t)}
	g.root = pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	g.rt = newHTTPSRuntime()
	g.setHost(hstsHost)
	g.trust = newTrustChecker(pkiPool(g.root), g.clock.Now, func() string { return *g.host.Load() })
	return g
}

func (g *hstsRig) setHost(h string) { g.host.Store(&h) }

// leaf is a certificate for hosts under the rig's root, valid from notBefore to notAfter.
func (g *hstsRig) leaf(notBefore, notAfter time.Time, hosts ...string) pkiCert {
	return pkiLeaf(g.t, &g.root, notBefore, notAfter, hosts...)
}

// serve installs c as the certificate of kind.
func (g *hstsRig) serve(kind tlsKind, c pkiCert) {
	g.rt.Install(kind, func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &tls.Certificate{Certificate: [][]byte{c.cert.Raw}}, nil
	})
}

// serveForever serves a certificate under the root that is valid for the whole test.
func (g *hstsRig) serveForever(kind tlsKind, hosts ...string) pkiCert {
	if len(hosts) == 0 {
		hosts = []string{hstsHost}
	}
	c := g.leaf(pkiFrom, pkiTo, hosts...)
	g.serve(kind, c)
	return c
}

func (g *hstsRig) db() hstsStore { return hstsDB{r: g.st.RQ, w: g.st.Q} }

// policy is a new policy over the rig's store, as a restart makes one.
func (g *hstsRig) policy(env *bool) *hstsPolicy {
	return g.policyOver(env, g.db(), slog.New(slog.DiscardHandler))
}

func (g *hstsRig) policyOver(env *bool, db hstsStore, log *slog.Logger) *hstsPolicy {
	return newHSTSPolicy(g.clock.Now, g.trust, g.rt, env, acmetest.NewClock(hstsStart), db, log)
}

// setStored writes the stored toggle the way the settings route will.
func (g *hstsRig) setStored(on bool) {
	g.t.Helper()
	v := 0
	if on {
		v = 1
	}
	if _, err := g.st.Write.ExecContext(testsupport.Context(g.t), `UPDATE https_state SET hsts_enabled = ? WHERE id = 1`, v); err != nil {
		g.t.Fatal(err)
	}
}

func (g *hstsRig) row() sqlc.HttpsState {
	g.t.Helper()
	row, err := g.st.RQ.GetHTTPSState(testsupport.Context(g.t))
	if err != nil {
		g.t.Fatal(err)
	}
	return row
}

// header recomputes p and asks it for the header on a TLS request for host.
func (g *hstsRig) header(p *hstsPolicy, host string) string {
	g.t.Helper()
	p.Recompute(testsupport.Context(g.t))
	return hstsHeaderFor(p, host)
}

// hstsHeaderFor is the header p gives a TLS request for host, without recomputing.
func hstsHeaderFor(p *hstsPolicy, host string) string {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.TLS = &tls.ConnectionState{}
	r.Host = host
	return p.Header(r)
}

// countingHSTSStore counts the writes the policy makes and can refuse them.
type countingHSTSStore struct {
	hstsStore
	ramp, held int
	fail       bool
}

func (c *countingHSTSStore) SaveHSTSRamp(ctx context.Context, a sqlc.SaveHSTSRampParams) error {
	c.ramp++
	if c.fail {
		return errors.New("disk is full")
	}
	return c.hstsStore.SaveHSTSRamp(ctx, a)
}

func (c *countingHSTSStore) SaveHSTSHeldUntil(ctx context.Context, a sqlc.SaveHSTSHeldUntilParams) error {
	c.held++
	if c.fail {
		return errors.New("disk is full")
	}
	return c.hstsStore.SaveHSTSHeldUntil(ctx, a)
}

func boolPtr(b bool) *bool { return &b }

// A fresh database has the toggle off and the ramp at step 0: a verified certificate is told to clear
// any policy, and nothing is written (D-10, D-14).
func TestHSTSPolicyRampFreshDatabaseIsOff(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	p := g.policy(nil)
	if got := g.header(p, hstsHost); got != "max-age=0" {
		t.Errorf("header = %q, want max-age=0", got)
	}
	if row := g.row(); row.HstsEnabled != 0 || row.HstsStep != 0 || row.HstsStepSince != 0 || row.HstsHost != "" {
		t.Errorf("row = %+v, want an untouched default", row)
	}
}

// Switching on starts at step 1 with the clock reading of that recompute, and each step is entered
// exactly when its dwell has passed: not a nanosecond before.
func TestHSTSPolicyRampStepsAtExactlyTheDwell(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	g.setStored(true)
	p := g.policy(nil)

	if got := g.header(p, hstsHost); got != "max-age=300" {
		t.Fatalf("first header = %q, want max-age=300", got)
	}
	if row := g.row(); row.HstsStep != 1 || row.HstsStepSince != g.clock.Now().UnixNano() || row.HstsHost != hstsHost {
		t.Errorf("row after the first send = %+v, want step 1 since now for %s", row, hstsHost)
	}

	for _, s := range []struct {
		dwell time.Duration
		want  string
		step  int64
	}{
		{24 * time.Hour, "max-age=604800", 2},
		{7 * 24 * time.Hour, "max-age=2592000", 3},
		{30 * 24 * time.Hour, "max-age=31536000", 4},
	} {
		g.clock.Advance(s.dwell - time.Nanosecond)
		if got := g.header(p, hstsHost); got == s.want {
			t.Fatalf("1 ns before the dwell of %s the header is already %q", s.dwell, got)
		}
		g.clock.Advance(time.Nanosecond)
		if got := g.header(p, hstsHost); got != s.want {
			t.Fatalf("at exactly the dwell of %s the header is %q, want %q", s.dwell, got, s.want)
		}
		if row := g.row(); row.HstsStep != s.step || row.HstsStepSince != g.clock.Now().UnixNano() {
			t.Errorf("row after step %d = %+v, want it to begin now", s.step, row)
		}
	}
	g.clock.Advance(1000 * 24 * time.Hour)
	if got := g.header(p, hstsHost); got != "max-age=31536000" {
		t.Errorf("after a long time the header is %q, want the top step to stay", got)
	}
}

// A new policy on the same database keeps the step and when it began; downtime longer than a dwell
// moves up one step only (D-11).
func TestHSTSPolicyRampSurvivesARestart(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	g.setStored(true)
	p := g.policy(nil)
	g.header(p, hstsHost)
	g.clock.Advance(24 * time.Hour)
	if got := g.header(p, hstsHost); got != "max-age=604800" {
		t.Fatalf("header = %q, want step 2", got)
	}
	since := g.row().HstsStepSince

	g.clock.Advance(time.Hour)
	if got := g.header(g.policy(nil), hstsHost); got != "max-age=604800" {
		t.Errorf("after a restart the header = %q, want step 2 kept", got)
	}
	if got := g.row().HstsStepSince; got != since {
		t.Errorf("a restart moved the step's start from %d to %d", since, got)
	}

	g.clock.Advance(100 * 24 * time.Hour)
	if got := g.header(g.policy(nil), hstsHost); got != "max-age=2592000" {
		t.Errorf("after a long outage the header = %q, want one step up (step 3)", got)
	}
	if row := g.row(); row.HstsStep != 3 || row.HstsStepSince != g.clock.Now().UnixNano() {
		t.Errorf("row after the outage = %+v, want step 3 beginning now", row)
	}
}

// Anything but sending starts the ramp over: another certificate kind, the toggle off, an unverified
// certificate. The next send begins at step 1.
func TestHSTSPolicyRampResetsWhenItStopsSending(t *testing.T) {
	g := newHSTSRig(t)
	good := g.serveForever(tlsKindSupplied)
	g.setStored(true)
	p := g.policy(nil)
	g.header(p, hstsHost)
	g.clock.Advance(24 * time.Hour)
	if got := g.header(p, hstsHost); got != "max-age=604800" {
		t.Fatalf("header = %q, want step 2", got)
	}

	t.Run("the self-signed certificate", func(t *testing.T) {
		g.serve(tlsKindSelfSigned, good)
		if got := g.header(p, hstsHost); got != "" {
			t.Errorf("header = %q, want none", got)
		}
		if row := g.row(); row.HstsStep != 0 || row.HstsStepSince != 0 {
			t.Errorf("row = %+v, want step 0", row)
		}
		g.serve(tlsKindSupplied, good)
		if got := g.header(p, hstsHost); got != "max-age=300" {
			t.Errorf("header after returning = %q, want the ramp to start again at 300", got)
		}
	})

	t.Run("the toggle off", func(t *testing.T) {
		g.clock.Advance(24 * time.Hour)
		g.header(p, hstsHost)
		g.setStored(false)
		if got := g.header(p, hstsHost); got != "max-age=0" {
			t.Errorf("header = %q, want max-age=0", got)
		}
		if row := g.row(); row.HstsStep != 0 || row.HstsStepSince != 0 {
			t.Errorf("row = %+v, want step 0", row)
		}
		g.setStored(true)
		if got := g.header(p, hstsHost); got != "max-age=300" {
			t.Errorf("header after switching on again = %q, want 300", got)
		}
	})

	t.Run("a certificate that does not verify", func(t *testing.T) {
		other := pkiLeaf(t, nil, pkiFrom, pkiTo, hstsHost)
		g.serve(tlsKindSupplied, other)
		if got := g.header(p, hstsHost); got != "max-age=0" {
			t.Errorf("header = %q, want max-age=0", got)
		}
		if row := g.row(); row.HstsStep != 0 {
			t.Errorf("row = %+v, want step 0", row)
		}
	})
}

// HSTS belongs to a host. A new verified public host starts again at step 1 for that host.
func TestHSTSPolicyRampRestartsForANewHost(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied, hstsHost, "second.example")
	g.setStored(true)
	p := g.policy(nil)
	g.header(p, hstsHost)
	g.clock.Advance(24 * time.Hour)
	if got := g.header(p, hstsHost); got != "max-age=604800" {
		t.Fatalf("header = %q, want step 2", got)
	}

	g.setHost("second.example")
	if got := g.header(p, "second.example"); got != "max-age=300" {
		t.Errorf("header for the new host = %q, want 300", got)
	}
	if got := hstsHeaderFor(p, hstsHost); got != "" {
		t.Errorf("header for the old host = %q, want none", got)
	}
	if row := g.row(); row.HstsStep != 1 || row.HstsHost != "second.example" || row.HstsStepSince != g.clock.Now().UnixNano() {
		t.Errorf("row = %+v, want step 1 for second.example beginning now", row)
	}
}

// TIERPLAN_HSTS wins over the stored toggle in both directions and never writes it (D-10).
func TestHSTSPolicyRampEnvOverridesTheStoredToggle(t *testing.T) {
	t.Run("off over a stored on", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		g.setStored(true)
		if got := g.header(g.policy(boolPtr(false)), hstsHost); got != "max-age=0" {
			t.Errorf("header = %q, want max-age=0", got)
		}
		if g.row().HstsEnabled != 1 {
			t.Error("the variable overwrote the stored toggle")
		}
	})
	t.Run("on over a stored off", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		if got := g.header(g.policy(boolPtr(true)), hstsHost); got != "max-age=300" {
			t.Errorf("header = %q, want max-age=300", got)
		}
		if g.row().HstsEnabled != 0 {
			t.Error("the variable overwrote the stored toggle")
		}
	})
}

// The database is written when the step, the host or the held-until bound changes and not otherwise.
func TestHSTSPolicyRampWritesOnlyOnChange(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)

	off := &countingHSTSStore{hstsStore: g.db()}
	p := g.policyOver(nil, off, slog.New(slog.DiscardHandler))
	for range 3 {
		g.header(p, hstsHost)
		g.clock.Advance(time.Minute)
	}
	if off.ramp != 0 || off.held != 0 {
		t.Errorf("a policy that never sends wrote %d ramps and %d bounds, want none", off.ramp, off.held)
	}

	g.setStored(true)
	for range 5 {
		g.header(p, hstsHost)
		g.clock.Advance(time.Minute)
	}
	if off.ramp != 1 {
		t.Errorf("five recomputes inside one step wrote the ramp %d times, want once", off.ramp)
	}
}

// A write that fails does not change the answer, is reported once however often it is retried, and
// is retried until it succeeds.
func TestHSTSPolicyRampFailedWriteKeepsTheDecision(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	g.setStored(true)
	var logs bytes.Buffer
	db := &countingHSTSStore{hstsStore: g.db(), fail: true}
	p := g.policyOver(nil, db, slog.New(slog.NewTextHandler(&logs, nil)))

	for range 3 {
		if got := g.header(p, hstsHost); got != "max-age=300" {
			t.Fatalf("header = %q, want max-age=300 although the write failed", got)
		}
		g.clock.Advance(time.Minute)
	}
	if n := strings.Count(logs.String(), "disk is full"); n != 1 {
		t.Errorf("the failed write was logged %d times, want once:\n%s", n, logs.String())
	}
	if g.row().HstsStep != 0 {
		t.Error("a failed write left a step in the row")
	}

	db.fail = false
	g.header(p, hstsHost)
	if row := g.row(); row.HstsStep != 1 {
		t.Errorf("row = %+v after the write recovered, want step 1", row)
	}
}
