// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
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

// seedRamp stores a ramp the way an earlier run of the server left it.
func (g *hstsRig) seedRamp(host string, step int, since time.Time, held time.Time) {
	g.t.Helper()
	if _, err := g.st.Write.ExecContext(testsupport.Context(g.t),
		`UPDATE https_state SET hsts_host = ?, hsts_step = ?, hsts_step_since = ?, hsts_held_until = ? WHERE id = 1`,
		host, step, since.UnixNano(), held.UnixNano()); err != nil {
		g.t.Fatal(err)
	}
}

// A 90-day certificate on step 3: the last second before the withdrawal window it still sends, and at
// exactly NotAfter minus 7 days it sends max-age=0 and the ramp starts over (D-12).
func TestHSTSPolicyWithdrawAtExactlySevenDaysBeforeNotAfter(t *testing.T) {
	g := newHSTSRig(t)
	notAfter := hstsStart.Add(30 * 24 * time.Hour)
	g.serve(tlsKindSupplied, g.leaf(notAfter.Add(-90*24*time.Hour), notAfter, hstsHost))
	g.setStored(true)
	g.seedRamp(hstsHost, 3, hstsStart, time.Time{})
	p := g.policy(nil)

	if got := g.header(p, hstsHost); got != "max-age=2592000" {
		t.Fatalf("header = %q, want max-age=2592000", got)
	}
	g.clock.Advance(23*24*time.Hour - time.Nanosecond)
	if got := g.header(p, hstsHost); got != "max-age=2592000" {
		t.Fatalf("1 ns before the window the header is %q, want max-age=2592000", got)
	}
	g.clock.Advance(time.Nanosecond)
	if got := g.header(p, hstsHost); got != "max-age=0" {
		t.Fatalf("at the window's start the header is %q, want max-age=0", got)
	}
	if row := g.row(); row.HstsStep != 0 || row.HstsStepSince != 0 {
		t.Errorf("row = %+v, want the ramp reset", row)
	}
	if d := p.snap.Load(); d.reason != "withdrawn" {
		t.Errorf("reason = %q, want withdrawn", d.reason)
	}
}

// After the renewal that missed its window, the new certificate is sent at step 1 (D-12).
func TestHSTSPolicyWithdrawnThenRenewedStartsAgainAtFiveMinutes(t *testing.T) {
	g := newHSTSRig(t)
	notAfter := hstsStart.Add(5 * 24 * time.Hour)
	g.serve(tlsKindACME, g.leaf(notAfter.Add(-90*24*time.Hour), notAfter, hstsHost))
	g.setStored(true)
	g.seedRamp(hstsHost, 3, hstsStart.Add(-time.Hour), time.Time{})
	p := g.policy(nil)

	if got := g.header(p, hstsHost); got != "max-age=0" {
		t.Fatalf("inside the window the header is %q, want max-age=0", got)
	}
	renewed := hstsStart.Add(90 * 24 * time.Hour)
	g.serve(tlsKindACME, g.leaf(hstsStart, renewed, hstsHost))
	if got := g.header(p, hstsHost); got != "max-age=300" {
		t.Errorf("after the renewal the header is %q, want max-age=300", got)
	}
}

// A renewal that keeps sending keeps the step and when it began.
func TestHSTSPolicyWithdrawRenewalWhileSendingKeepsTheRamp(t *testing.T) {
	g := newHSTSRig(t)
	g.serve(tlsKindACME, g.leaf(hstsStart.Add(-30*24*time.Hour), hstsStart.Add(60*24*time.Hour), hstsHost))
	g.setStored(true)
	p := g.policy(nil)
	g.header(p, hstsHost)
	g.clock.Advance(24 * time.Hour)
	if got := g.header(p, hstsHost); got != "max-age=604800" {
		t.Fatalf("header = %q, want step 2", got)
	}
	before := g.row()

	g.clock.Advance(time.Hour)
	g.serve(tlsKindACME, g.leaf(g.clock.Now(), g.clock.Now().Add(90*24*time.Hour), hstsHost))
	if got := g.header(p, hstsHost); got != "max-age=604800" {
		t.Errorf("after a renewal the header is %q, want step 2 kept", got)
	}
	if after := g.row(); after.HstsStep != before.HstsStep || after.HstsStepSince != before.HstsStepSince {
		t.Errorf("a renewal changed the ramp from %+v to %+v", before, after)
	}
}

// The window is a third of the lifetime when that is under a week, in integer nanoseconds, and the cap
// for short-lived certificates begins below 10 days exactly.
func TestHSTSPolicyWithdrawWindowAndCapBoundaries(t *testing.T) {
	const day = 24 * time.Hour
	if got := withdrawWindow(90 * day); got != 7*day {
		t.Errorf("window of 90 days = %s, want 7 days", got)
	}
	if got := withdrawWindow(6 * day); got != 2*day {
		t.Errorf("window of 6 days = %s, want 2 days", got)
	}
	if got := withdrawWindow(21 * day); got != 7*day {
		t.Errorf("window of 21 days = %s, want 7 days (the cap meets a third exactly)", got)
	}
	if got := withdrawWindow(21*day - 1); got != 7*day-1 {
		t.Errorf("window of 21 days less 1 ns = %s, want 7 days less 1 ns (truncated)", got)
	}
	if got := withdrawWindow(10); got != 3 {
		t.Errorf("window of 10 ns = %d, want 3: integer division truncates", got)
	}

	v := trustVerdict{Checked: true, Verified: true, Host: hstsHost}
	for _, tc := range []struct {
		name     string
		lifetime time.Duration
		maxAge   int64
		capped   bool
	}{
		{"exactly 10 days is not capped", 10 * day, 2592000, false},
		{"1 ns under 10 days is capped at one week", 10*day - 1, 604800, true},
		{"a 6-day certificate is capped", 6 * day, 604800, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notBefore := hstsStart.Add(-time.Hour)
			ramp := hstsRamp{host: hstsHost, step: 2, since: hstsStart.Add(-7 * day).UnixNano()}
			d, got := hstsDecide(hstsInput{
				kind: tlsKindACME, verdict: v, enabled: true, now: hstsStart,
				notBefore: notBefore, notAfter: notBefore.Add(tc.lifetime),
			}, ramp)
			if d.kind != hstsSend || d.maxAge != tc.maxAge {
				t.Fatalf("decision = %+v, want a send of %d", d, tc.maxAge)
			}
			if (d.reason == "capped") != tc.capped {
				t.Errorf("reason = %q, capped want %v", d.reason, tc.capped)
			}
			if tc.capped && got.step != 2 {
				t.Errorf("step = %d, want it held at 2", got.step)
			}
		})
	}

	t.Run("the window starts at exactly NotAfter minus the window", func(t *testing.T) {
		notAfter := hstsStart.Add(30 * day)
		in := hstsInput{kind: tlsKindACME, verdict: v, enabled: true, notBefore: notAfter.Add(-90 * day), notAfter: notAfter}
		start := notAfter.Add(-7 * day)
		in.now = start.Add(-time.Nanosecond)
		if d, _ := hstsDecide(in, hstsRamp{}); d.kind != hstsSend {
			t.Errorf("1 ns before the window the decision is %+v, want a send", d)
		}
		in.now = start
		if d, _ := hstsDecide(in, hstsRamp{}); d.kind != hstsWithdraw {
			t.Errorf("at the window's start the decision is %+v, want a withdrawal", d)
		}
	})
}

// A short-lived certificate never reaches the one-month step, even when renewal after renewal keeps the
// ramp running past the days that would have lifted it (D-22).
func TestHSTSPolicyCapAcrossRenewalsOfShortCertificates(t *testing.T) {
	const day = 24 * time.Hour
	g := newHSTSRig(t)
	issue := func() {
		now := g.clock.Now()
		g.serve(tlsKindACME, g.leaf(now, now.Add(6*day), hstsHost))
	}
	issue()
	g.setStored(true)
	p := g.policy(nil)
	g.header(p, hstsHost)
	for range 20 {
		g.clock.Advance(day)
		if got := g.header(p, hstsHost); got != "max-age=604800" && got != "max-age=300" {
			t.Fatalf("day %v: header = %q, want 300 or 604800", g.clock.Now().Sub(hstsStart), got)
		}
		issue() // renewed at half life or earlier, each time before the window
	}
	if got := g.header(p, hstsHost); got != "max-age=604800" {
		t.Errorf("after 20 days of short certificates the header is %q, want the 1 week cap", got)
	}
	if d := p.snap.Load(); d.reason != "capped" || d.step != 2 {
		t.Errorf("decision = %+v, want step 2 reported as capped", d)
	}
}

// What each certificate kind gets when it does not verify, with the toggle on and off.
func TestHSTSPolicyClearsOverUntrustedCertificateFiles(t *testing.T) {
	g := newHSTSRig(t)
	stranger := pkiLeaf(t, nil, pkiFrom, pkiTo, hstsHost) // signed by nobody the rig trusts

	for _, toggle := range []bool{true, false} {
		g.setStored(toggle)
		g.serve(tlsKindSupplied, stranger)
		if got := g.header(g.policy(nil), hstsHost); got != "max-age=0" {
			t.Errorf("cert files that fail the trust check, toggle %v: header = %q, want max-age=0", toggle, got)
		}
	}

	g.setStored(true)
	g.serve(tlsKindSelfSigned, stranger)
	p := g.policy(nil)
	if got := g.header(p, hstsHost); got != "" {
		t.Errorf("self-signed: header = %q, want none", got)
	}
	if d := p.snap.Load(); d.reason != "self_signed" {
		t.Errorf("self-signed reason = %q", d.reason)
	}

	// A Let's Encrypt certificate from a test directory chains to a root no system trusts.
	g.serve(tlsKindACME, stranger)
	p = g.policy(nil)
	if got := g.header(p, hstsHost); got != "" {
		t.Errorf("unverified Let's Encrypt: header = %q, want none", got)
	}
	if d := p.snap.Load(); d.reason != trustUnknownAuthority && d.reason != trustSelfSigned {
		t.Errorf("unverified Let's Encrypt reason = %q, want the trust reason", d.reason)
	}
}

// The bound on what a browser may hold is kept at or after now plus the max-age, and is rewritten at
// most once an hour of the clock (D-26).
func TestHSTSPolicyHeldUntil(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	g.setStored(true)
	db := &countingHSTSStore{hstsStore: g.db()}
	p := g.policyOver(nil, db, slog.New(slog.DiscardHandler))

	for i := range 180 {
		got := g.header(p, hstsHost)
		if got != "max-age=300" {
			t.Fatalf("minute %d: header = %q", i, got)
		}
		need := g.clock.Now().Add(300 * time.Second).UnixNano()
		if held := g.row().HstsHeldUntil; held < need {
			t.Fatalf("minute %d: held until %d, want at least %d", i, held, need)
		}
		g.clock.Advance(time.Minute)
	}
	if db.held != 3 {
		t.Errorf("three hours of recomputes wrote the bound %d times, want 3 (at minutes 0, 61 and 122)", db.held)
	}

	// Withdrawing does not lower it: a browser may still hold what was sent.
	before := g.row().HstsHeldUntil
	g.setStored(false)
	g.header(p, hstsHost)
	if after := g.row().HstsHeldUntil; after != before {
		t.Errorf("a withdrawal moved the bound from %d to %d", before, after)
	}
}

// Every value the policy can produce is one of the five exact integers and nothing else (D-13).
func TestHSTSHeaderValuesAreExact(t *testing.T) {
	const day = 24 * time.Hour
	valid := regexp.MustCompile(`^max-age=(0|300|604800|2592000|31536000)$`)
	good := trustVerdict{Checked: true, Verified: true, Host: hstsHost}
	bad := trustVerdict{Checked: true, Reason: trustUnknownAuthority, Host: hstsHost}
	verdicts := []trustVerdict{{}, good, bad, {Checked: true, Reason: trustNoPublicURL}}
	kinds := []tlsKind{0, tlsKindSelfSigned, tlsKindSupplied, tlsKindACME}
	lifetimes := []time.Duration{day, 6 * day, 10*day - 1, 10 * day, 90 * day}
	seen := map[string]bool{}
	for _, kind := range kinds {
		for _, v := range verdicts {
			for _, enabled := range []bool{false, true} {
				for _, life := range lifetimes {
					for step := 0; step <= 4; step++ {
						for _, offset := range []time.Duration{0, life / 2, life - life/3, life - time.Hour} {
							notBefore := hstsStart.Add(-offset)
							in := hstsInput{kind: kind, verdict: v, enabled: enabled, now: hstsStart, notBefore: notBefore, notAfter: notBefore.Add(life)}
							d, _ := hstsDecide(in, hstsRamp{host: hstsHost, step: step, since: hstsStart.Add(-40 * day).UnixNano()})
							p := &hstsPolicy{}
							p.snap.Store(&d)
							got := hstsHeaderFor(p, hstsHost)
							if got != "" && !valid.MatchString(got) {
								t.Fatalf("%+v gave %q", in, got)
							}
							seen[got] = true
						}
					}
				}
			}
		}
	}
	for _, want := range []string{"", "max-age=0", "max-age=300", "max-age=604800", "max-age=2592000", "max-age=31536000"} {
		if !seen[want] {
			t.Errorf("the grid never produced %q", want)
		}
	}
}

// hstsPut sends PUT /api/https/hsts as the signed-in admin and returns the status and the body.
func (r *acmeRig) hstsPut(c *http.Client, cookie *http.Cookie, csrf, body string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodPut, r.base+"/api/https/hsts", strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// hstsHeaderOverTheWire is the Strict-Transport-Security header of a real TLS response for the public host.
func (r *acmeRig) hstsHeaderOverTheWire(c *http.Client) string {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodGet, r.base+"/healthz", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.Header.Get("Strict-Transport-Security")
}

// An admin turns HSTS on and off from the API: the choice is stored, the policy recomputes at once so the
// next TLS response carries the new decision, and the status shows the step (D-10, D-11, TLS-06).
func TestHSTSToggleFromTheAPI(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &root, pkiFrom, pkiTo, acmeHost)
	crt, key := pkiWritePair(t, leaf)
	r := newACMERig(t, acmetest.Options{}, map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key},
		WithTrustRoots(pkiPool(root)))
	srv := r.startNew()
	client := r.client(pkiPool(root))
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)

	st := r.trustStatus(client, cookie)
	if st.HSTS == nil {
		t.Fatal("the status has no hsts block")
	}
	if st.HSTS.Enabled || st.HSTS.State != api.HSTSStateOffClearing || !st.HSTS.Clearing || st.HSTS.Lock != nil {
		t.Errorf("before the switch hsts = %+v, want off and clearing with no lock", st.HSTS)
	}
	if got := r.hstsHeaderOverTheWire(client); got != "max-age=0" {
		t.Errorf("Strict-Transport-Security before the switch = %q, want max-age=0", got)
	}

	if code, body := r.hstsPut(client, cookie, csrf, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("PUT hsts on: %d %s", code, body)
	}
	if got := r.hstsHeaderOverTheWire(client); got != "max-age=300" {
		t.Errorf("Strict-Transport-Security after switching on = %q, want max-age=300", got)
	}
	on := r.trustStatus(client, cookie).HSTS
	if !on.Enabled || !on.StoredEnabled || on.State != api.HSTSStateSending || on.Step != 1 || on.MaxAge != 300 || on.Clearing {
		t.Errorf("after switching on hsts = %+v, want sending at step 1 with max-age 300", on)
	}
	if on.NextStepAt == nil || on.ForgetBy != nil {
		t.Errorf("after switching on next step %v and forget-by %v, want a next step and no forget-by", on.NextStepAt, on.ForgetBy)
	}
	if row := srv.hstsRow(t); row.HstsEnabled != 1 {
		t.Errorf("stored toggle = %d, want 1", row.HstsEnabled)
	}

	if code, body := r.hstsPut(client, cookie, csrf, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("PUT hsts off: %d %s", code, body)
	}
	if got := r.hstsHeaderOverTheWire(client); got != "max-age=0" {
		t.Errorf("Strict-Transport-Security after switching off = %q, want max-age=0", got)
	}
	off := r.trustStatus(client, cookie).HSTS
	if off.Enabled || off.State != api.HSTSStateOffClearing || !off.Clearing || off.Step != 0 {
		t.Errorf("after switching off hsts = %+v, want off and clearing", off)
	}
	if off.ForgetBy == nil {
		t.Error("after a policy was sent and switched off the status names no forget-by time")
	}
}

// hstsRow is the stored HSTS row of the server's database.
func (s *Server) hstsRow(t *testing.T) sqlc.HttpsState {
	t.Helper()
	row, err := s.Store().RQ.GetHTTPSState(testsupport.Context(t))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// view recomputes p and describes it.
func (g *hstsRig) view(p *hstsPolicy) api.HSTSView {
	g.t.Helper()
	p.Recompute(testsupport.Context(g.t))
	return p.View(testsupport.Context(g.t))
}

// stranger is a certificate for the public host that a root the rig does not trust signed.
func (g *hstsRig) stranger() pkiCert {
	other := pkiCA(g.t, "other root", nil, pkiFrom, pkiTo)
	return pkiLeaf(g.t, &other, pkiFrom, pkiTo, hstsHost)
}

// The status says where the ramp is and when the next step comes: the step in force, the instant its
// dwell ends, nothing at the last step a certificate may reach, and "capped" when a short-lived
// certificate holds it at a week (D-11, D-22).
func TestHSTSPolicyViewSending(t *testing.T) {
	const day = 24 * time.Hour
	t.Run("at step 2 the next step is a week after it began", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		g.setStored(true)
		g.seedRamp(hstsHost, 2, hstsStart, time.Time{})
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateSending || v.MaxAge != 604800 || v.Step != 2 || v.Clearing || v.Capped {
			t.Errorf("view = %+v, want sending at step 2 with max-age 604800", v)
		}
		if want := hstsStart.Add(7 * day); !v.NextStepAt.Equal(want) {
			t.Errorf("next step at %v, want %v", v.NextStepAt, want)
		}
		if !v.Enabled || !v.StoredEnabled || !v.ForgetBy.IsZero() || v.Lock != nil || v.Withdrawal != nil || v.NotSentReason != "" {
			t.Errorf("view = %+v, want enabled, no forget-by, no lock, no withdrawal and no reason", v)
		}
	})
	t.Run("at step 4 there is no next step", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		g.setStored(true)
		g.seedRamp(hstsHost, 4, hstsStart, time.Time{})
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateSending || v.Step != 4 || v.MaxAge != 31536000 || !v.NextStepAt.IsZero() || v.Capped {
			t.Errorf("view = %+v, want sending at step 4, uncapped, with no next step", v)
		}
	})
	t.Run("a short-lived certificate is capped and has no next step", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serve(tlsKindACME, g.leaf(hstsStart.Add(-time.Hour), hstsStart.Add(6*day), hstsHost))
		g.setStored(true)
		g.seedRamp(hstsHost, 2, hstsStart, time.Time{})
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateSending || v.Step != 2 || !v.Capped || !v.NextStepAt.IsZero() {
			t.Errorf("view = %+v, want sending at step 2, capped, with no next step", v)
		}
	})
	t.Run("a step that can still grow names the dwell of its own step", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindACME)
		g.setStored(true)
		v := g.view(g.policy(nil))
		if v.Step != 1 || !v.NextStepAt.Equal(hstsStart.Add(day)) {
			t.Errorf("view = %+v, want step 1 and the next step a day on", v)
		}
	})
}

// Off, and on but not sent: each of the situations the card words differently (D-10, D-14, D-23).
func TestHSTSPolicyViewOffAndNotSent(t *testing.T) {
	t.Run("off over a verified certificate clears", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateOffClearing || !v.Clearing || v.Enabled || v.MaxAge != 0 || v.Step != 0 {
			t.Errorf("view = %+v, want off and clearing", v)
		}
	})
	t.Run("off over the self-signed certificate is silent", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSelfSigned)
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateOffSilent || v.Clearing {
			t.Errorf("view = %+v, want off and silent", v)
		}
	})
	t.Run("on over the self-signed certificate is not sent", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSelfSigned)
		g.setStored(true)
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateNotSent || v.NotSentReason != trustSelfSigned || v.Clearing || !v.Enabled {
			t.Errorf("view = %+v, want not sent because of %s", v, trustSelfSigned)
		}
	})
	t.Run("on over an untrusted certificate file clears and gives the trust reason", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serve(tlsKindSupplied, g.stranger())
		g.setStored(true)
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateNotSent || !v.Clearing || v.NotSentReason != trustUnknownAuthority {
			t.Errorf("view = %+v, want not sent and clearing because of %s", v, trustUnknownAuthority)
		}
	})
	t.Run("on over a test-directory Let's Encrypt certificate is not sent and does not clear", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serve(tlsKindACME, g.stranger())
		g.setStored(true)
		v := g.view(g.policy(nil))
		if v.State != api.HSTSStateNotSent || v.Clearing || v.NotSentReason != trustUnknownAuthority {
			t.Errorf("view = %+v, want not sent, not clearing, because of %s", v, trustUnknownAuthority)
		}
	})
}

// Inside the withdrawal window the card says so, with the certificate's end and where it came from (D-12).
func TestHSTSPolicyViewWithdrawn(t *testing.T) {
	const day = 24 * time.Hour
	for _, tc := range []struct {
		kind   tlsKind
		source string
	}{{tlsKindSupplied, api.SourceCertFiles}, {tlsKindACME, api.SourceLetsEncrypt}} {
		t.Run(tc.source, func(t *testing.T) {
			g := newHSTSRig(t)
			notAfter := hstsStart.Add(3 * day)
			g.serve(tc.kind, g.leaf(notAfter.Add(-90*day), notAfter, hstsHost))
			g.setStored(true)
			v := g.view(g.policy(nil))
			if v.State != api.HSTSStateWithdrawn || !v.Clearing || v.Withdrawal == nil {
				t.Fatalf("view = %+v, want withdrawn and clearing", v)
			}
			if !v.Withdrawal.NotAfter.Equal(notAfter) || v.Withdrawal.Source != tc.source {
				t.Errorf("withdrawal = %+v, want %v from %s", v.Withdrawal, notAfter, tc.source)
			}
		})
	}
}

// forgetBy is the time after which no browser can hold a policy that was sent: at least the max-age
// after the last send, kept while the header is off, and gone once it has passed (D-26).
func TestHSTSPolicyViewForgetBy(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	g.setStored(true)
	g.seedRamp(hstsHost, 2, hstsStart, time.Time{})
	p := g.policy(nil)

	if v := g.view(p); !v.ForgetBy.IsZero() {
		t.Fatalf("while sending forgetBy = %v, want none", v.ForgetBy)
	}
	g.clock.Advance(10 * time.Minute)
	g.view(p)
	turnedOff := g.clock.Now()
	if err := p.SetEnabled(testsupport.Context(t), false); err != nil {
		t.Fatal(err)
	}
	v := p.View(testsupport.Context(t))
	if v.State != api.HSTSStateOffClearing {
		t.Fatalf("view = %+v, want off and clearing", v)
	}
	if want := turnedOff.Add(604800 * time.Second); v.ForgetBy.Before(want) {
		t.Errorf("forgetBy = %v, want at least %v", v.ForgetBy, want)
	}
	g.clock.Advance(v.ForgetBy.Sub(g.clock.Now()))
	if got := g.view(p); !got.ForgetBy.IsZero() {
		t.Errorf("at the forget-by time itself forgetBy = %v, want none", got.ForgetBy)
	}
}

// A variable fixes the toggle: the card names it with the stored choice, a different value is refused
// before anything is written, and the same value writes nothing (D-10, D-17, T-16-42).
func TestHSTSPolicyViewAndSetEnabledUnderALock(t *testing.T) {
	ctx := testsupport.Context(t)
	t.Run("on over a stored off", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		p := g.policy(boolPtr(true))
		v := g.view(p)
		if !v.Enabled || v.StoredEnabled {
			t.Errorf("view = %+v, want enabled by the variable and stored off", v)
		}
		if v.Lock == nil || *v.Lock != (api.HTTPSLockBody{Var: "TIERPLAN_HSTS", Value: "on", Stored: "off"}) {
			t.Errorf("lock = %+v, want TIERPLAN_HSTS on over a stored off", v.Lock)
		}
		var locked *ErrHSTSLocked
		if err := p.SetEnabled(ctx, false); !errors.As(err, &locked) || locked.Var != "TIERPLAN_HSTS" {
			t.Errorf("SetEnabled(false) = %v, want ErrHSTSLocked naming TIERPLAN_HSTS", err)
		}
		if err := p.SetEnabled(ctx, true); err != nil {
			t.Errorf("SetEnabled(true), the variable's own value: %v", err)
		}
		if row := g.row(); row.HstsEnabled != 0 {
			t.Errorf("stored toggle = %d after a refused change and an echo, want it untouched at 0", row.HstsEnabled)
		}
	})
	t.Run("off over a stored on", func(t *testing.T) {
		g := newHSTSRig(t)
		g.serveForever(tlsKindSupplied)
		g.setStored(true)
		p := g.policy(boolPtr(false))
		v := g.view(p)
		if v.Enabled || !v.StoredEnabled || v.Lock == nil || v.Lock.Value != "off" || v.Lock.Stored != "on" {
			t.Errorf("view = %+v, want disabled by the variable, stored on and a lock showing off over on", v)
		}
		if err := p.SetEnabled(ctx, true); err == nil {
			t.Error("SetEnabled(true) under TIERPLAN_HSTS=off was accepted")
		}
		if row := g.row(); row.HstsEnabled != 1 {
			t.Errorf("stored toggle = %d after a refused change, want 1", row.HstsEnabled)
		}
	})
}

// SetEnabled stores the choice and the header follows before it returns; Enabled is the effective
// setting the Let's Encrypt manager asks (D-10, D-26).
func TestHSTSPolicySetEnabledStoresAndRecomputes(t *testing.T) {
	g := newHSTSRig(t)
	g.serveForever(tlsKindSupplied)
	p := g.policy(nil)
	g.header(p, hstsHost)
	ctx := testsupport.Context(t)

	if p.Enabled() {
		t.Fatal("HSTS is on in a fresh database")
	}
	if err := p.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if row := g.row(); row.HstsEnabled != 1 || !p.Enabled() {
		t.Errorf("stored toggle = %d, Enabled = %v, want 1 and true", row.HstsEnabled, p.Enabled())
	}
	if got := hstsHeaderFor(p, hstsHost); got != "max-age=300" {
		t.Errorf("header right after SetEnabled(true) = %q, want max-age=300", got)
	}
	if err := p.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if row := g.row(); row.HstsEnabled != 0 || p.Enabled() {
		t.Errorf("stored toggle = %d, Enabled = %v, want 0 and false", row.HstsEnabled, p.Enabled())
	}
	if got := hstsHeaderFor(p, hstsHost); got != "max-age=0" {
		t.Errorf("header right after SetEnabled(false) = %q, want max-age=0", got)
	}
}

// Through the API, a variable-fixed toggle answers 409 hsts_locked with nothing stored, an echo of the
// variable's value is a no-op, and the status names the lock (D-10, D-17, T-16-42).
func TestHSTSLockedFromTheAPI(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &root, pkiFrom, pkiTo, acmeHost)
	crt, key := pkiWritePair(t, leaf)
	r := newACMERig(t, acmetest.Options{}, map[string]string{
		"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key, "TIERPLAN_HSTS": "on",
	}, WithTrustRoots(pkiPool(root)))
	srv := r.startNew()
	client := r.client(pkiPool(root))
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)

	code, body := r.hstsPut(client, cookie, csrf, `{"enabled":false}`)
	if code != http.StatusConflict || !strings.Contains(body, "hsts_locked") || !strings.Contains(body, "TIERPLAN_HSTS") {
		t.Errorf("PUT enabled=false under TIERPLAN_HSTS=on: %d %s, want 409 hsts_locked naming the variable", code, body)
	}
	if code, body := r.hstsPut(client, cookie, csrf, `{"enabled":true}`); code != http.StatusOK {
		t.Errorf("PUT enabled=true, the variable's value: %d %s, want 200", code, body)
	}
	if row := srv.hstsRow(t); row.HstsEnabled != 0 {
		t.Errorf("stored toggle = %d, want it untouched at 0", row.HstsEnabled)
	}
	st := r.trustStatus(client, cookie).HSTS
	if st == nil || !st.Enabled || st.StoredEnabled || st.Lock == nil || st.Lock.Var != "TIERPLAN_HSTS" || st.Lock.Value != "on" || st.Lock.Stored != "off" {
		t.Errorf("hsts = %+v, want enabled by the variable, stored off and the lock", st)
	}
	if got := r.hstsHeaderOverTheWire(client); got != "max-age=300" {
		t.Errorf("Strict-Transport-Security = %q, want max-age=300", got)
	}
}

// stopACME sends POST /api/https/acme/stop as the signed-in admin and returns the status and the body.
func (r *acmeRig) stopACME(c *http.Client, cookie *http.Cookie, csrf string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodPost, r.base+"/api/https/acme/stop", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// With HSTS on, Stop using Let's Encrypt is refused 409 hsts_on and the Let's Encrypt certificate stays
// served; once HSTS is off the stop works, and the status says when browsers will have forgotten the
// policy that was sent (D-26, T-16-41).
func TestHSTSBlocksStop(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	// The rig's fake CA is the one root the trust verdict knows, so the certificate it issues is
	// trusted and HSTS is really sent over it.
	r.opts = append(r.opts, WithTrustRoots(r.fake.IssuerPool()))
	srv := r.startNew()
	selfSigned := r.client(r.mirrorPool())
	cookie := r.signIn(selfSigned)
	csrf := r.csrf(selfSigned, cookie)
	r.issue()
	if srv.https.Kind() != tlsKindACME {
		t.Fatalf("served kind = %d after the issue, want Let's Encrypt", srv.https.Kind())
	}
	le := r.client(r.fake.IssuerPool())

	if code, body := r.hstsPut(le, cookie, csrf, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("PUT hsts on: %d %s", code, body)
	}
	if got := r.hstsHeaderOverTheWire(le); got != "max-age=300" {
		t.Fatalf("Strict-Transport-Security = %q, want max-age=300 sent over the Let's Encrypt certificate", got)
	}

	code, body := r.stopACME(le, cookie, csrf)
	if code != http.StatusConflict || !strings.Contains(body, "hsts_on") {
		t.Errorf("POST stop with HSTS on: %d %s, want 409 hsts_on", code, body)
	}
	if srv.https.Kind() != tlsKindACME {
		t.Errorf("served kind = %d after a refused stop, want Let's Encrypt", srv.https.Kind())
	}
	if st, err := srv.acme.Status(testsupport.Context(t)); err != nil || st.Mode != "active" {
		t.Errorf("Let's Encrypt mode = %q (%v) after a refused stop, want active", st.Mode, err)
	}

	if code, body := r.hstsPut(le, cookie, csrf, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("PUT hsts off: %d %s", code, body)
	}
	if code, body := r.stopACME(le, cookie, csrf); code != http.StatusNoContent {
		t.Fatalf("POST stop with HSTS off: %d %s, want 204", code, body)
	}
	if srv.https.Kind() != tlsKindSelfSigned {
		t.Errorf("served kind = %d after the stop, want self-signed", srv.https.Kind())
	}
	st := r.trustStatus(r.client(r.mirrorPool()), cookie).HSTS
	if st == nil || st.ForgetBy == nil || !st.ForgetBy.After(r.clock.Now()) {
		t.Errorf("hsts = %+v after the stop, want a forget-by time in the future", st)
	}
}
