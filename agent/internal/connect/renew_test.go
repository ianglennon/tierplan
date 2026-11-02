// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/agent/internal/state"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

const day = 24 * time.Hour

// testCert is a client certificate for key, as the server's CA would issue it, valid
// between notBefore and notAfter. The agent does not verify the chain, only the key
// and the dates, so any CA will do.
func testCert(t *testing.T, key ed25519.PrivateKey, agentID string, notBefore, notAfter time.Time) []byte {
	t.Helper()
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: notBefore.Add(-day), NotAfter: notAfter.Add(day), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<60))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: agentID, OrganizationalUnit: []string{"env-7"}},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestNextRenewalIsTwoThirdsOfTheCertificatesLifeAndNotBefore(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	notBefore, notAfter := t0, t0.Add(30*day)
	want := t0.Add(20 * day)
	if got := nextRenewal(notBefore, notAfter, t0, 0, nil); !got.Equal(want) {
		t.Fatalf("a certificate that runs from day 0 to day 30 is renewed at %v, want day 20 (%v)", got, want)
	}
	// The answer does not depend on when it is asked, only on the certificate.
	if got := nextRenewal(notBefore, notAfter, t0.Add(19*day), 0, nil); !got.Equal(want) {
		t.Fatalf("asked on day 19 it says %v, want day 20 (%v)", got, want)
	}
	// An agent that was off past the due date is due at that date, in the past.
	if got := nextRenewal(notBefore, notAfter, t0.Add(25*day), 0, nil); !got.Equal(want) {
		t.Fatalf("asked on day 25 it says %v, want the past date day 20 (%v)", got, want)
	}
}

func TestNextRenewalAfterAFailureIsAboutAnHourLaterAndJittered(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := t0.Add(20*day + time.Minute)
	for _, tc := range []struct {
		rnd  float64
		want time.Duration
	}{{0, 45 * time.Minute}, {0.5, time.Hour}, {0.999999, 75*time.Minute - 1}} {
		got := nextRenewal(t0, t0.Add(30*day), now, 1, func() float64 { return tc.rnd }).Sub(now)
		if d := got - tc.want; d < -time.Second || d > time.Second {
			t.Errorf("after a failure with jitter %v the retry is %v away, want about %v", tc.rnd, got, tc.want)
		}
	}
	// Further failures retry at the same pace: an hour, never longer.
	if got := nextRenewal(t0, t0.Add(30*day), now, 9, func() float64 { return 0.5 }).Sub(now); got != time.Hour {
		t.Errorf("after nine failures the retry is %v away, want an hour", got)
	}
}

// clock is a clock the test moves.
type clock struct{ ns atomic.Int64 }

func newClock(t time.Time) *clock {
	c := &clock{}
	c.ns.Store(t.UnixNano())
	return c
}
func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }
func (c *clock) set(t time.Time)         { c.ns.Store(t.UnixNano()) }

// controlRig runs runControl against the server end of an in-memory pipe, with a
// renewer and a clock the test moves.
type controlRig struct {
	t      *testing.T
	clk    *clock
	key    ed25519.PrivateKey
	t0     time.Time
	server net.Conn
	lines  chan string
	// beatClocks carries the clock each heartbeat reported, and results the
	// update_result lines (answers to fences), apart from lines so that waitFor and
	// count never discard them.
	beatClocks chan int64
	results    chan struct{}
	saved      chan []byte
	saveFn     func([]byte) error
	rn         *renewer
	done       chan error
}

func newControlRig(t *testing.T, tweak func(*controlRig)) *controlRig {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := &controlRig{t: t, clk: newClock(t0), key: key, t0: t0, lines: make(chan string, 1024), beatClocks: make(chan int64, 1<<16), results: make(chan struct{}, 64), saved: make(chan []byte, 8), done: make(chan error, 1)}
	r.saveFn = func(c []byte) error { r.saved <- c; return nil }
	if tweak != nil {
		tweak(r)
	}
	cert := testCert(t, key, "agent-7", t0, t0.Add(30*day))
	r.rn = newRenewer(cert, "agent-7", key, r.clk.now, func() float64 { return 0.5 }, slog.New(slog.DiscardHandler), func(c []byte) error { return r.saveFn(c) })
	if r.rn == nil {
		t.Fatal("newRenewer returned nil for a certificate it can read")
	}
	agentEnd, serverEnd := net.Pipe()
	r.server = serverEnd
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = serverEnd.Close()
		select {
		case <-r.done:
		case <-time.After(testsupport.Deadline):
			t.Error("runControl did not stop")
		}
	})
	go func() {
		r.done <- runControl(ctx, agentEnd, filepath.Join(t.TempDir(), "none.sock"), 5*time.Millisecond, r.clk.now,
			slog.New(slog.DiscardHandler), newDenials(), controlHooks{renew: r.rn})
	}()
	go func() {
		br := bufio.NewReader(serverEnd)
		if _, err := br.ReadByte(); err != nil { // the kind byte
			return
		}
		sc := bufio.NewScanner(br)
		for sc.Scan() {
			var h struct {
				T       string `json:"t"`
				ClockMs int64  `json:"clockMs"`
			}
			if json.Unmarshal(sc.Bytes(), &h) != nil {
				continue
			}
			switch h.T {
			case wire.TypeUpdateResult:
				r.results <- struct{}{}
			case wire.TypeHeartbeat:
				r.lines <- h.T
				r.beatClocks <- h.ClockMs
			default:
				r.lines <- h.T
			}
		}
	}()
	return r
}

// count drains what arrived and returns how many lines of type typ there were.
func (r *controlRig) count(typ string) int {
	n := 0
	for {
		select {
		case l := <-r.lines:
			if l == typ {
				n++
			}
		default:
			return n
		}
	}
}

// waitFor waits for a line of type typ.
func (r *controlRig) waitFor(typ string) {
	r.t.Helper()
	for {
		if l := testsupport.Await(r.t, fmt.Sprintf("a %q line", typ), r.lines); l == typ {
			return
		}
	}
}

// settle returns once the control loop has gone through a whole round with the clock
// as it is now. A heartbeat carries the clock the loop read at the start of its round,
// so one that reports the current clock began after the test set it; the loop writes
// the next heartbeat only when that round, renewal check and renew line included, is
// over, so the one after it proves the round ended. A line the round wrote has by
// then reached lines, ahead of that heartbeat.
func (r *controlRig) settle() {
	r.t.Helper()
	want := r.clk.now().UnixMilli()
	for testsupport.Await(r.t, "a heartbeat that reports the clock as it is now", r.beatClocks) < want {
	}
	testsupport.Await(r.t, "the heartbeat after it", r.beatClocks)
}

func (r *controlRig) reply(v any) {
	r.t.Helper()
	b, _ := json.Marshal(v)
	_ = r.server.SetWriteDeadline(time.Now().Add(testsupport.Deadline))
	if _, err := r.server.Write(append(b, '\n')); err != nil {
		r.t.Fatalf("write to the agent: %v", err)
	}
	// The agent reads its control lines one at a time, so its answer to a fence sent
	// after v says that v has been handled.
	if _, err := r.server.Write([]byte(fenceLine + "\n")); err != nil {
		r.t.Fatalf("write the fence to the agent: %v", err)
	}
	testsupport.Await(r.t, "the agent's answer to the fence", r.results)
}

func TestAnAgentAsksToRenewAtDay20AndNotBefore(t *testing.T) {
	r := newControlRig(t, nil)
	r.clk.set(r.t0.Add(19*day + 23*time.Hour))
	r.settle()
	if n := r.count(wire.TypeRenew); n != 0 {
		t.Fatalf("%d renew messages on day 19.96, want none", n)
	}
	r.clk.set(r.t0.Add(20*day + time.Minute))
	r.waitFor(wire.TypeRenew)
	r.settle()
	if n := r.count(wire.TypeRenew); n != 0 {
		t.Fatalf("%d more renew messages while the first is unanswered, want none", n)
	}
}

func TestARenewedCertificateIsSavedAndTheNextRenewalIsScheduledFromIt(t *testing.T) {
	r := newControlRig(t, nil)
	r.clk.set(r.t0.Add(20*day + time.Minute))
	r.waitFor(wire.TypeRenew)

	now := r.clk.now()
	renewed := testCert(t, r.key, "agent-7", now.Add(-5*time.Minute), now.Add(30*day))
	r.reply(wire.Renewed{T: wire.TypeRenewed, Cert: renewed})
	if got := testsupport.Await(t, "the renewed certificate to be saved", r.saved); string(got) != string(renewed) {
		t.Fatal("the agent saved a certificate other than the one it was sent")
	}

	// Its own two thirds is day 40, not another request now or on day 21.
	r.clk.advance(day)
	r.settle()
	if n := r.count(wire.TypeRenew); n != 0 {
		t.Fatalf("%d renew messages a day after a successful renewal, want none", n)
	}
	r.clk.set(now.Add(-5 * time.Minute).Add(20*day + 2*time.Hour))
	r.waitFor(wire.TypeRenew)
}

func TestARefusedRenewalIsAskedAgainAboutAnHourLater(t *testing.T) {
	r := newControlRig(t, nil)
	r.clk.set(r.t0.Add(20*day + time.Minute))
	r.waitFor(wire.TypeRenew)
	r.reply(wire.RenewRefused{T: wire.TypeRenewRefused, Code: wire.RenewInternal})
	r.settle()

	r.clk.advance(30 * time.Minute)
	r.settle()
	if n := r.count(wire.TypeRenew); n != 0 {
		t.Fatalf("%d renew messages 30 minutes after a refusal, want none", n)
	}
	r.clk.advance(35 * time.Minute)
	r.waitFor(wire.TypeRenew)
}

func TestNoAnswerCountsAsAFailureAndTheRetryIsAboutAnHourAfterThat(t *testing.T) {
	r := newControlRig(t, nil)
	r.clk.set(r.t0.Add(20*day + time.Minute))
	r.waitFor(wire.TypeRenew)

	r.clk.advance(3 * time.Minute) // past the wait for an answer: a failure, retried in an hour
	r.settle()
	r.clk.advance(50 * time.Minute)
	r.settle()
	if n := r.count(wire.TypeRenew); n != 0 {
		t.Fatalf("%d renew messages 53 minutes after the unanswered one, want none", n)
	}
	r.clk.advance(15 * time.Minute)
	r.waitFor(wire.TypeRenew)
}

func TestACertificateForAnotherKeyIsNeverKept(t *testing.T) {
	r := newControlRig(t, nil)
	r.clk.set(r.t0.Add(20*day + time.Minute))
	r.waitFor(wire.TypeRenew)

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	now := r.clk.now()
	r.reply(wire.Renewed{T: wire.TypeRenewed, Cert: testCert(t, other, "agent-7", now, now.Add(30*day))})
	r.settle()
	select {
	case <-r.saved:
		t.Fatal("the agent kept a certificate for a key it does not hold")
	default:
	}
	r.clk.advance(70 * time.Minute)
	r.waitFor(wire.TypeRenew)
}

func TestACertificateForAnotherAgentOrOneThatDoesNotOutliveTheCurrentIsNeverKept(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T, r *controlRig) []byte{
		"another agent": func(t *testing.T, r *controlRig) []byte {
			now := r.clk.now()
			return testCert(t, r.key, "agent-8", now, now.Add(30*day))
		},
		"the same expiry": func(t *testing.T, r *controlRig) []byte {
			return testCert(t, r.key, "agent-7", r.t0, r.t0.Add(30*day))
		},
		"garbage": func(*testing.T, *controlRig) []byte { return []byte("not a certificate") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newControlRig(t, nil)
			r.clk.set(r.t0.Add(20*day + time.Minute))
			r.waitFor(wire.TypeRenew)
			r.reply(wire.Renewed{T: wire.TypeRenewed, Cert: mk(t, r)})
			r.settle()
			select {
			case <-r.saved:
				t.Fatal("the agent kept it")
			default:
			}
		})
	}
}

func TestARenewedCertificateThatCannotBeSavedIsRetriedAndTheOldOneKeepsWorking(t *testing.T) {
	var fails atomic.Bool
	fails.Store(true)
	r := newControlRig(t, func(r *controlRig) {
		r.saveFn = func(c []byte) error {
			if fails.Load() {
				return errors.New("read-only volume")
			}
			r.saved <- c
			return nil
		}
	})
	r.clk.set(r.t0.Add(20*day + time.Minute))
	r.waitFor(wire.TypeRenew)
	now := r.clk.now()
	r.reply(wire.Renewed{T: wire.TypeRenewed, Cert: testCert(t, r.key, "agent-7", now, now.Add(30*day))})
	r.settle()
	fails.Store(false)
	r.clk.advance(70 * time.Minute)
	r.waitFor(wire.TypeRenew)
}

func TestARenewedWithNoRenewAskedForIsIgnored(t *testing.T) {
	r := newControlRig(t, nil)
	now := r.clk.now()
	r.reply(wire.Renewed{T: wire.TypeRenewed, Cert: testCert(t, r.key, "agent-7", now, now.Add(60*day))})
	r.settle()
	select {
	case <-r.saved:
		t.Fatal("the agent kept a certificate it did not ask for")
	default:
	}
}

func TestAnUnreadableCertificateMeansNoRenewalAndNoCrash(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	rn := newRenewer([]byte("saved-cert-der"), "agent-7", key, time.Now, nil, slog.New(slog.DiscardHandler), func([]byte) error { return nil })
	if rn != nil {
		t.Fatal("a certificate that cannot be read got a renewer")
	}
}

// --- the whole agent against a server that answers renewals ---

func TestTheAgentRenewsItsCertificateOverTheLiveTunnelAndSavesItAtomically(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	old := testCert(t, key, "agent-7", now.Add(-21*day), now.Add(9*day)) // due since yesterday
	if err := state.Save(dir, state.State{AgentID: "agent-7", EnvID: "env-7", Key: key, Cert: old}); err != nil {
		t.Fatal(err)
	}
	fresh := testCert(t, key, "agent-7", now.Add(-5*time.Minute), now.Add(30*day))
	var asked atomic.Int32
	srv := newFakeServer(t, func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, fenceAfterRenew: true, onRenew: func() any {
			asked.Add(1)
			return wire.Renewed{T: wire.TypeRenewed, Cert: fresh}
		}}
	})
	startAgent(t, Config{URL: srv.url(), StateDir: dir}, fastEnv())
	srv.awaitFence(t) // the agent handled the renewed certificate and saved it
	if s, ok, err := state.Load(dir); err != nil || !ok || string(s.Cert) != string(fresh) {
		t.Fatalf("the renewed certificate is not on the volume: %v %v", ok, err)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the agent asked %d times, want once", n)
	}
	s, _, _ := state.Load(dir)
	if string(s.Key) != string(key) || s.AgentID != "agent-7" {
		t.Fatal("a renewal changed the agent's key or identity")
	}
}
