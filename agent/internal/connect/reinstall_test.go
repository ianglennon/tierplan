// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/agent/internal/state"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

// A TierPlan that was reinstalled with an empty data directory has none of the
// environments its agents were enrolled into, and refuses their hello with the
// uniform auth_rejected (unknown_environment, T-02-21). The operator reruns the new
// install command on each host, which starts the agent with a new join token on
// the tierplan-agent volume that still holds the old identity. These tests are
// about how the agent tells that case from one where its token is the one it
// enrolled with.

const (
	// oldToken enrolled the identity on the volume; newToken is what the
	// reinstalled agent is started with. Both are distinctive so a test can look
	// for them in the log.
	oldToken = "tpjt_0190d2a4-7a3b-7c3d-8e4f-0123456789ab_T0xELXRva2VuLW9sZC10b2tlbi1vbGQtdG9rZW4" //nolint:gosec // G101: a made-up token for the test
	newToken = "tpjt_0190d2a4-7a3b-7c3d-8e4f-ba9876543210_TkVXLXRva2VuLW5ldy10b2tlbi1uZXctdG9rZW4" //nolint:gosec // G101: a made-up token for the test
)

// reinstallLog is what the agent says when it sets its identity aside to try its
// join token (it used to drop the identity at this point; it no longer does).
const reinstallLog = setAsideLog

// enrolledWith is enrolledState with the record of the join token it was enrolled
// with, as an agent of this version leaves it.
func enrolledWith(t *testing.T, dir, token string) state.State {
	t.Helper()
	s := enrolledState(t, dir)
	if err := state.SaveEnrolToken(dir, state.TokenHash(token)); err != nil {
		t.Fatal(err)
	}
	return s
}

// noSecretsIn fails the test when the log holds a join token, or the hash of one.
func noSecretsIn(t *testing.T, logs string, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if strings.Contains(logs, tok) {
			t.Errorf("the log holds a join token:\n%s", logs)
		}
		if strings.Contains(logs, state.TokenHash(tok)) {
			t.Errorf("the log holds the hash of a join token:\n%s", logs)
		}
		// The secret part on its own, in case only it were printed.
		if _, secret, ok := strings.Cut(strings.TrimPrefix(tok, "tpjt_"), "_"); ok && strings.Contains(logs, secret) {
			t.Errorf("the log holds the secret of a join token:\n%s", logs)
		}
	}
}

// reinstalledServer refuses every hello auth_rejected and welcomes every enrolment,
// recording whether the old identity was still on disk when the agent enrolled.
func reinstalledServer(t *testing.T, dir string) (*fakeServer, func() []bool) {
	t.Helper()
	var mu sync.Mutex
	var onDiskAtEnrol []bool
	srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonAuthRejected}
		}
		mu.Lock()
		onDiskAtEnrol = append(onDiskAtEnrol, !noIdentityOnDisk(dir))
		mu.Unlock()
		return reply{cert: []byte("reinstalled-cert-der")}
	})
	return srv, func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), onDiskAtEnrol...)
	}
}

// assertReEnrolled checks the outcome of a hello refused auth_rejected that made the
// agent drop its identity and enrol with token: hello, then one enrolment with the
// token and a fresh key, made after the old identity was gone; the new identity and
// the record of token saved; the reason logged once; and no token in the log.
func assertReEnrolled(t *testing.T, srv *fakeServer, dir string, saved state.State, token string, onDisk func() []bool, logs *lockedBuffer) {
	t.Helper()
	first, _ := srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if first.Mode != wire.ModeHello || second.Mode != wire.ModeEnrol || second.Token != token {
		t.Fatalf("modes = %q then %q; want hello, refused auth_rejected, then enrol with the token the agent was started with", first.Mode, second.Mode)
	}
	if bytes.Equal(second.Pub, publicOf(saved.Key)) {
		t.Error("the re-enrolment presented the old key; it must be a fresh one")
	}
	if got := onDisk(); len(got) == 0 || got[0] {
		t.Errorf("identity files still in the state directory when the agent enrolled again (%v); the old identity must be set aside first", got)
	}
	srv.heartbeatsAfterWelcome(t, 1) // the identity, the record and the removal of the stash happen before the first heartbeat
	if _, err := os.Stat(filepath.Join(dir, state.StashDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the identity set aside is still there after the enrolment was accepted: %v", err)
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || string(got.Cert) != "reinstalled-cert-der" || got.Key.Equal(saved.Key) {
		t.Fatalf("the identity on disk = %+v %v %v; want the new enrolment's", got, ok, err)
	}
	if hash, known, err := state.LoadEnrolToken(dir); err != nil || !known || hash != state.TokenHash(token) {
		t.Errorf("the record of the enrolling token = %q %v %v; want the hash of the token the agent enrolled with", hash, known, err)
	}
	if n := srv.attempts(); n != 2 {
		t.Errorf("the agent made %d attempts, want 2 (hello, then one enrolment)", n)
	}
	out := logs.String()
	if n := strings.Count(out, "code=auth_rejected"); n != 1 {
		t.Errorf("the code auth_rejected was logged %d times, want once:\n%s", n, out)
	}
	if n := strings.Count(out, reinstallLog); n != 1 {
		t.Errorf("the reason for enrolling again was logged %d times, want once:\n%s", n, out)
	}
	noSecretsIn(t, out, oldToken, newToken)
}

// (a) The reinstall: the identity was enrolled with one token and the agent is
// started with another. It drops the identity TierPlan rejects and enrols with the
// new token, and from then on the record names the new one.
func TestAnAgentStartedWithANewJoinTokenDropsTheIdentityTierPlanRejectsAndEnrolsWithIt(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledWith(t, dir, oldToken)
	srv, onDisk := reinstalledServer(t, dir)
	var logs lockedBuffer
	startAgent(t, Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}, fastEnv())
	assertReEnrolled(t, srv, dir, saved, newToken, onDisk, &logs)
}

// (c) An identity enrolled by an agent older than the record has none. The agent
// cannot tell whether its token is the one it enrolled with, and an identity
// TierPlan rejects is of no use to it, so it enrols again with the token it has.
func TestAnIdentityWithNoRecordOfItsTokenEnrolsAgainWhenTierPlanRejectsIt(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	srv, onDisk := reinstalledServer(t, dir)
	var logs lockedBuffer
	startAgent(t, Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}, fastEnv())
	assertReEnrolled(t, srv, dir, saved, newToken, onDisk, &logs)
}

// (b) The token the identity was enrolled with cannot fix an identity TierPlan
// rejects: the agent keeps the identity and backs off as before, and never enrols.
func TestAnAgentStartedWithTheTokenItEnrolledWithKeepsItsIdentityWhenRejected(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledWith(t, dir, oldToken)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} })
	var logs lockedBuffer
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), JoinToken: oldToken, StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 3)
	if err != nil {
		t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
	}
	half := func() float64 { return 0.5 }
	for i, d := range sl.all() {
		if want := backoff(i+1, time.Second, 60*time.Second, half); d != want {
			t.Errorf("wait %d = %v, want the ordinary backoff %v", i+1, d, want)
		}
	}
	for n := 1; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
			t.Errorf("attempt %d used mode %q, want hello: the same token cannot help", n, a.Mode)
		}
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || !got.Key.Equal(saved.Key) || string(got.Cert) != "saved-cert-der" {
		t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
	}
	if hash, known, _ := state.LoadEnrolToken(dir); !known || hash != state.TokenHash(oldToken) {
		t.Errorf("the record changed: %q %v", hash, known)
	}
	out := logs.String()
	if strings.Contains(out, reinstallLog) {
		t.Errorf("the agent says it dropped its identity, and it must not have:\n%s", out)
	}
	if n := strings.Count(out, "code=auth_rejected"); n != 1 {
		t.Errorf("the code auth_rejected was logged %d times, want once:\n%s", n, out)
	}
	noSecretsIn(t, out, oldToken)
}

// (d) With no join token there is nothing to enrol with, whatever the record says.
func TestAnAgentWithNoJoinTokenKeepsItsIdentityWhenRejected(t *testing.T) {
	for name, record := range map[string]bool{"with a record": true, "without a record": false} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var saved state.State
			if record {
				saved = enrolledWith(t, dir, oldToken)
			} else {
				saved = enrolledState(t, dir)
			}
			srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} })
			var logs lockedBuffer
			if _, err := runUntilSleeps(t, Config{
				URL: srv.url(), StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
				SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
			}, 3); err != nil {
				t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
			}
			for n := 1; n <= srv.attempts(); n++ {
				if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
					t.Errorf("attempt %d used mode %q, want hello", n, a.Mode)
				}
			}
			got, ok, err := state.Load(dir)
			if err != nil || !ok || !got.Key.Equal(saved.Key) {
				t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
			}
			if strings.Contains(logs.String(), reinstallLog) {
				t.Errorf("the agent says it dropped its identity, and it must not have:\n%s", logs.String())
			}
		})
	}
}

// (e) The rate limit of every rejoin holds here too. An agent that just dropped an
// identity and is refused auth_rejected again with a token it cannot match waits
// the maximum before it drops a second one, and drops it once the maximum passed.
func TestASecondAuthRejectedRightAfterARejoinWaitsTheMaximum(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	var logs lockedBuffer
	r := &runner{
		cfg: Config{JoinToken: newToken, BackoffMin: time.Second, BackoffMax: 60 * time.Second},
		env: runEnv{now: func() time.Time { return clock }, rnd: func() float64 { return 0.5 }},
		log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	hold := func() {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		r.cur, r.have, r.enrolHash = state.State{AgentID: "agent-7", EnvID: "env-7", Key: priv, Cert: []byte("c")}, true, state.TokenHash(oldToken)
	}
	refusal := &RefusedError{Code: wire.ReasonAuthRejected}
	failures := 0

	hold()
	if wait := r.refused(refusal, &failures); wait != 0 || r.have {
		t.Fatalf("first refusal: wait %v, identity kept %v; want an enrolment now with the identity dropped", wait, r.have)
	}
	// The same thing again ten seconds later: an identity the agent cannot match.
	hold()
	clock = clock.Add(10 * time.Second)
	if wait := r.refused(refusal, &failures); wait != 60*time.Second || !r.have {
		t.Fatalf("a refusal 10s after the rejoin: wait %v, identity kept %v; want the 60s maximum with the identity kept", wait, r.have)
	}
	clock = clock.Add(60 * time.Second)
	if wait := r.refused(refusal, &failures); wait != 0 || r.have {
		t.Fatalf("a refusal after the maximum passed: wait %v, identity kept %v; want an enrolment now", wait, r.have)
	}
	noSecretsIn(t, logs.String(), oldToken, newToken)
}

// (e) The limit is shared with the other reasons to rejoin: an agent that enrolled
// again for a new token and is revoked at once waits the maximum before it enrols a
// third agent.
func TestAnAgentRevokedRightAfterEnrollingWithANewTokenWaitsTheMaximum(t *testing.T) {
	dir := t.TempDir()
	enrolledWith(t, dir, oldToken)
	srv := newFakeServer(t, func(n int, a wire.Auth) reply {
		switch {
		case a.Mode == wire.ModeEnrol:
			return reply{cert: []byte("second-cert-der"), drop: true}
		case n == 1:
			return reply{refuse: wire.ReasonAuthRejected}
		}
		return reply{refuse: wire.ReasonRevoked}
	})
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	waits := sl.all()
	if len(waits) != 3 || waits[0] != 0 || waits[2] != 60*time.Second {
		t.Fatalf("waits = %v; want none after the auth_rejected hello, then the 60s maximum after the revoke", waits)
	}
	enrols := 0
	for n := 1; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode == wire.ModeEnrol {
			enrols++
		}
	}
	if enrols != 1 {
		t.Errorf("the agent enrolled %d times, want once", enrols)
	}
	if got, ok, err := state.Load(dir); err != nil || !ok || string(got.Cert) != "second-cert-der" {
		t.Errorf("the identity on disk = %+v %v %v, want the one the re-enrolment saved", got, ok, err)
	}
}

// runUntilSleepsClocked is runUntilSleeps with a clock that each fake sleep moves
// on, so the agent sees the time pass that it would in production: a test of what it
// does a minute later is not answered by a rate limit on a clock that stood still
// (review 17-2 IN-01).
func runUntilSleepsClocked(t *testing.T, cfg Config, stopAt int) (*sleeps, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Deadline)
	defer cancel()
	sl := &sleeps{stopAt: stopAt}
	ctx, sl.cancel = context.WithCancel(ctx)
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	var mu sync.Mutex
	clock := time.Now()
	err := runWith(ctx, cfg, runEnv{
		sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			clock = clock.Add(d)
			mu.Unlock()
			return sl.sleep(ctx, d)
		},
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		},
		rnd: func() float64 { return 0.5 }, roots: httptestRoots(),
	})
	return sl, err
}

// The identity is only set aside for the enrolment. When TierPlan refuses that
// enrolment too, which is what happens when the hello was refused for a cause the
// token cannot fix (an audience after an address change, a channel binding behind a
// proxy: both auth_rejected), the agent takes the identity back, keeps running and
// waits the maximum. Before this the identity was deleted first and the refusal
// ended the agent, so a reinstall was the only way back.
//
// The refused token is then not tried again for that identity (review 17-2 WR-04):
// the agent says hello with its identity and backs off as it does with the token on
// record, and never sets the identity aside again for that token, however much time
// passes. The clock moves with the waits, so the hellos after the first minute are
// there because the token was tried, not because of the rate limit.
func TestARefusedEnrolmentAfterSettingTheIdentityAsideGivesItBackAndKeepsTheAgentRunning(t *testing.T) {
	for name, record := range map[string]bool{"no record of the token (an older agent)": false, "a record of another token": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var saved state.State
			if record {
				saved = enrolledWith(t, dir, oldToken)
			} else {
				saved = enrolledState(t, dir)
			}
			var mu sync.Mutex
			var onDiskAtEnrol []bool
			srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
				if a.Mode == wire.ModeEnrol {
					mu.Lock()
					onDiskAtEnrol = append(onDiskAtEnrol, !noIdentityOnDisk(dir))
					mu.Unlock()
				}
				return reply{refuse: wire.ReasonAuthRejected} // as for audience or channel_binding
			})
			var logs lockedBuffer
			sl, err := runUntilSleepsClocked(t, Config{
				URL: srv.url(), JoinToken: newToken, StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
				SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
			}, 6)
			if err != nil {
				t.Fatalf("Run = %v; an agent with an identity set aside never ends for the refusal of its enrolment", err)
			}
			half := func() float64 { return 0.5 }
			want := []time.Duration{0, 60 * time.Second}
			for n := 1; len(want) < 6; n++ {
				want = append(want, backoff(n, time.Second, 60*time.Second, half))
			}
			if waits := sl.all(); !slices.Equal(waits, want) {
				t.Errorf("waits = %v; want none before the enrolment, the 60s maximum after it was refused, then the ordinary backoff of hellos %v", waits, want[2:])
			}
			var total time.Duration
			for _, d := range sl.all()[:5] {
				total += d
			}
			if total <= 60*time.Second {
				t.Fatalf("only %v passed on the agent's clock; the test must pass the minute of the rate limit", total)
			}
			for n := 1; n <= srv.attempts(); n++ {
				a, _ := srv.authN(t, n)
				wantMode := wire.ModeHello
				if n == 2 {
					wantMode = wire.ModeEnrol
				}
				if a.Mode != wantMode {
					t.Fatalf("attempt %d used mode %q, want %q: one enrolment with the token, then hellos only", n, a.Mode, wantMode)
				}
				if wantMode == wire.ModeEnrol && (a.Token != newToken || bytes.Equal(a.Pub, publicOf(saved.Key))) {
					t.Errorf("the enrolment did not present the token and a fresh key")
				}
				if wantMode == wire.ModeHello && (a.AgentID != saved.AgentID || string(a.Cert) != "saved-cert-der") {
					t.Errorf("hello %d presented %q %q, want the identity that was set aside", n, a.AgentID, a.Cert)
				}
			}
			mu.Lock()
			if len(onDiskAtEnrol) != 1 || onDiskAtEnrol[0] {
				t.Errorf("identity in the state directory at the enrolment = %v; it must be set aside", onDiskAtEnrol)
			}
			mu.Unlock()
			got, ok, err := state.Load(dir)
			if err != nil || !ok || !got.Key.Equal(saved.Key) || got.AgentID != saved.AgentID || string(got.Cert) != "saved-cert-der" {
				t.Fatalf("the identity on disk = %+v %v %v; want the one that was set aside, back in place", got, ok, err)
			}
			hash, known, err := state.LoadEnrolToken(dir)
			if record && (err != nil || !known || hash != state.TokenHash(oldToken)) {
				t.Errorf("the record = %q %v %v; want the old one back", hash, known, err)
			}
			if !record && known {
				t.Errorf("a record appeared (%q); an identity with none keeps none", hash)
			}
			if tried, known, err := state.LoadTriedToken(dir, saved.AgentID); err != nil || !known || tried != state.TokenHash(newToken) {
				t.Errorf("the tried token record = %q %v %v; want the refused token's hash for this identity", tried, known, err)
			}
			if _, err := os.Stat(filepath.Join(dir, state.StashDir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the stash is still there after the identity was taken back: %v", err)
			}
			out := logs.String()
			for _, line := range []string{takenBackLog, triedLog, setAsideLog} {
				if n := strings.Count(out, line); n != 1 {
					t.Errorf("%q was logged %d times, want once:\n%s", line, n, out)
				}
			}
			noSecretsIn(t, out, oldToken, newToken)
		})
	}
}

// A token the agent tried and was refused stays tried after a restart, since the
// record is next to the identity. A different token is tried: the operator who starts
// the agent with a new install command gets an enrolment within a minute.
func TestARefusedTokenStaysTriedAcrossARestartAndADifferentTokenIsTried(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	refuseAll := func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} }
	run := func(t *testing.T, token string, script func(int, wire.Auth) reply, stopAt int) (*fakeServer, string) {
		t.Helper()
		srv := newFakeServer(t, script)
		var logs lockedBuffer
		if _, err := runUntilSleepsClocked(t, Config{
			URL: srv.url(), JoinToken: token, StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
			SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
		}, stopAt); err != nil {
			t.Fatal(err)
		}
		return srv, logs.String()
	}
	enrols := func(t *testing.T, srv *fakeServer) int {
		t.Helper()
		n := 0
		for i := 1; i <= srv.attempts(); i++ {
			if a, _ := srv.authN(t, i); a.Mode == wire.ModeEnrol {
				n++
			}
		}
		return n
	}

	if srv, _ := run(t, newToken, refuseAll, 3); enrols(t, srv) != 1 {
		t.Fatalf("the first run enrolled %d times, want once", enrols(t, srv))
	}
	// Restarted with the same token: hellos only, and the reason is said once.
	srv, out := run(t, newToken, refuseAll, 4)
	if n := enrols(t, srv); n != 0 {
		t.Errorf("after a restart with the refused token the agent enrolled %d times, want none", n)
	}
	if n := strings.Count(out, triedLog); n != 1 {
		t.Errorf("the agent said %d times that it does not try the token, want once:\n%s", n, out)
	}
	// Restarted with another token: it is tried, and accepted.
	srv, _ = run(t, rotatedTo, func(_ int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonAuthRejected}
		}
		return reply{cert: []byte("accepted-cert-der"), drop: true}
	}, 2)
	if a, _ := srv.authN(t, 2); a.Mode != wire.ModeEnrol || a.Token != rotatedTo {
		t.Fatalf("attempt 2 with another token = %q, want an enrolment with that token", a.Mode)
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || got.Key.Equal(saved.Key) || string(got.Cert) != "accepted-cert-der" {
		t.Fatalf("the identity on disk = %+v %v %v; want the accepted enrolment's", got, ok, err)
	}
	// The new identity has tried nothing: the record of the old one went with it.
	if _, known, _ := state.LoadTriedToken(dir, got.AgentID); known {
		t.Error("the new identity carries a tried token record")
	}
	if _, err := os.Stat(filepath.Join(dir, state.TriedTokenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old identity's tried token record is still in the state directory: %v", err)
	}
}

// A blocked host, a busy server, a host whose other agent is connected or a
// connection that fails during the enrolment is treated the same: any enrolment that does not end in a welcome gives the identity
// back.
func TestAnEnrolmentThatDoesNotEndInAWelcomeGivesTheIdentityBack(t *testing.T) {
	for _, code := range []string{wire.ReasonBlocked, wire.RefuseRateLimited, wire.RefuseHostAlreadyConnected, ""} {
		t.Run(cmp.Or(code, "welcomed without a certificate"), func(t *testing.T) {
			dir := t.TempDir()
			saved := enrolledWith(t, dir, oldToken)
			srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
				switch {
				case a.Mode == wire.ModeHello:
					return reply{refuse: wire.ReasonAuthRejected}
				case code != "":
					return reply{refuse: code}
				}
				// A welcome without a certificate is not an enrolment the agent can take.
				return reply{cert: []byte{}}
			})
			sl, err := runUntilSleeps(t, Config{
				URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
				BackoffMin: time.Second, BackoffMax: 60 * time.Second,
			}, 2)
			if err != nil {
				t.Fatalf("Run = %v", err)
			}
			if waits := sl.all(); len(waits) != 2 || waits[1] != 60*time.Second {
				t.Errorf("waits = %v; want the 60s maximum after the enrolment", waits)
			}
			if got, ok, err := state.Load(dir); err != nil || !ok || !got.Key.Equal(saved.Key) {
				t.Fatalf("the identity on disk = %+v %v %v; want the one that was set aside", got, ok, err)
			}
		})
	}
}

// An agent that stopped while its identity was set aside (a crash, a restart of
// the container during the enrolment) takes it back when it starts, and says hello
// with it.
func TestAnAgentThatStoppedWithItsIdentitySetAsideTakesItBackAtStart(t *testing.T) {
	for name, partial := range map[string]bool{"the whole identity set aside": false, "only identity.json moved": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			saved := enrolledWith(t, dir, oldToken)
			if partial {
				if err := os.Mkdir(filepath.Join(dir, state.StashDir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(dir, state.IdentityFile), filepath.Join(dir, state.StashDir, state.IdentityFile)); err != nil {
					t.Fatal(err)
				}
			} else if err := state.Stash(dir); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := state.Load(dir); ok {
				t.Fatal("the set-up left an identity in place")
			}
			srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
			var logs lockedBuffer
			startAgent(t, Config{
				URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
			}, fastEnv())
			a, _ := srv.authN(t, 1)
			if a.Mode != wire.ModeHello || a.AgentID != saved.AgentID || string(a.Cert) != "saved-cert-der" {
				t.Fatalf("the first handshake = %q %q; want a hello with the identity that was set aside", a.Mode, a.AgentID)
			}
			srv.heartbeatsAfterWelcome(t, 1)
			if got, ok, err := state.Load(dir); err != nil || !ok || !got.Key.Equal(saved.Key) {
				t.Fatalf("the identity on disk = %+v %v %v", got, ok, err)
			}
			if _, err := os.Stat(filepath.Join(dir, state.StashDir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the stash is still there: %v", err)
			}
			if !strings.Contains(logs.String(), "it took the identity back") {
				t.Errorf("the agent did not say it took its identity back:\n%s", logs.String())
			}
		})
	}
}

// rotatedFrom and rotatedTo are two join tokens of the environment enrolledState's
// identity is in (env-7): the one it enrolled with and the one a rotation replaced it
// with. A rotation keeps the environment in the token; newToken names another one.
const (
	rotatedFrom = "tpjt_env-7_Uk9UQVRFRC1mcm9tLXRva2VuLWZyb20tdG9rZW4tZnJvbQ" //nolint:gosec // G101: a made-up token for the test
	rotatedTo   = "tpjt_env-7_Uk9UQVRFRC10by10b2tlbi10by10b2tlbi10by10b2tlbg" //nolint:gosec // G101: a made-up token for the test
)

// A hello TierPlan accepts while the agent holds a token of the identity's own
// environment other than the one on record (a token rotation) records that token, so
// a later auth_rejected with it is no reason to try it. An identity with no record
// keeps none: which token enrolled it is unknown.
func TestAnAcceptedHelloRecordsTheTokenOnlyOverAnExistingRecord(t *testing.T) {
	for name, record := range map[string]bool{"with a record of another token": true, "without a record": false} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if record {
				enrolledWith(t, dir, rotatedFrom)
			} else {
				enrolledState(t, dir)
			}
			srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
			startAgent(t, Config{
				URL: srv.url(), JoinToken: rotatedTo, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
			}, fastEnv())
			if a, _ := srv.authN(t, 1); a.Mode != wire.ModeHello {
				t.Fatalf("mode %q, want hello", a.Mode)
			}
			srv.heartbeatsAfterWelcome(t, 1) // the record is written before the first heartbeat
			hash, known, err := state.LoadEnrolToken(dir)
			if record && (err != nil || !known || hash != state.TokenHash(rotatedTo)) {
				t.Errorf("the record = %q %v %v; want the token TierPlan accepted the hello with", hash, known, err)
			}
			if !record && known {
				t.Errorf("the record = %q; an identity with none keeps none", hash)
			}
		})
	}
}

// Review 17-2 WR-01: an operator who runs another environment's install command on a
// host keeps the volume, so the agent says hello with the identity of the first
// environment while it holds the second one's token. That hello is accepted while
// the first environment exists, and must not record the token as a rotation: once
// the first environment is removed the hello is refused auth_rejected, and the agent
// has to try the token it holds, or it is stuck with a dead identity for good.
func TestAnAcceptedHelloDoesNotRecordAJoinTokenOfAnotherEnvironment(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledWith(t, dir, rotatedFrom)
	srv := newFakeServer(t, func(n int, a wire.Auth) reply {
		switch {
		case n == 1:
			return reply{drop: true} // the first environment still exists
		case a.Mode == wire.ModeHello:
			return reply{refuse: wire.ReasonAuthRejected} // it was removed
		}
		return reply{cert: []byte("second-env-cert-der")}
	})
	var logs lockedBuffer
	startAgent(t, Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}, fastEnv())
	for n, want := range []string{wire.ModeHello, wire.ModeHello, wire.ModeEnrol} {
		a, _ := srv.authN(t, n+1)
		if a.Mode != want {
			t.Fatalf("attempt %d used mode %q, want %q: the accepted hello must not have recorded the other environment's token", n+1, a.Mode, want)
		}
		if want == wire.ModeEnrol && (a.Token != newToken || bytes.Equal(a.Pub, publicOf(saved.Key))) {
			t.Errorf("the enrolment did not present the token the agent holds and a fresh key")
		}
	}
	srv.heartbeatsAfterWelcome(t, 1)
	if hash, known, err := state.LoadEnrolToken(dir); err != nil || !known || hash != state.TokenHash(newToken) {
		t.Errorf("the record = %q %v %v; want the token of the accepted enrolment", hash, known, err)
	}
	noSecretsIn(t, logs.String(), rotatedFrom, newToken)
}

// tokenEnv reads the environment out of a join token, and nothing out of a string
// that is not shaped like one.
func TestTokenEnvReadsTheEnvironmentOfAJoinToken(t *testing.T) {
	for tok, want := range map[string]string{
		newToken:     "0190d2a4-7a3b-7c3d-8e4f-ba9876543210",
		rotatedTo:    "env-7",
		"tpjt_e_a_b": "e", // a secret may hold an underscore
		"tpjt_e_":    "",
		"tpjt_e":     "",
		"tpjt_":      "",
		"xxxx_e_s":   "",
		"":           "",
	} {
		if got := tokenEnv(tok); got != want {
			t.Errorf("tokenEnv(%q) = %q, want %q", tok, got, want)
		}
	}
}

// An expired certificate makes the agent enrol with the identity still held. A
// rejection of that enrolment is about the token, not the identity, so it is
// retried as before and the identity is not dropped for it.
func TestARejectedEnrolmentForAnExpiredCertificateKeepsTheIdentity(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledWith(t, dir, oldToken)
	srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonCertExpired}
		}
		return reply{refuse: wire.ReasonAuthRejected}
	})
	var logs lockedBuffer
	if _, err := runUntilSleeps(t, Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 3); err != nil {
		t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || !got.Key.Equal(saved.Key) {
		t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
	}
	if strings.Contains(logs.String(), reinstallLog) {
		t.Errorf("the agent says it dropped its identity, and it must not have:\n%s", logs.String())
	}
}

// (g) Every successful enrolment records the hash of its token next to the identity.
func TestASuccessfulEnrolmentRecordsTheHashOfItsJoinToken(t *testing.T) {
	dir := t.TempDir()
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{cert: []byte("issued-cert-der")} })
	var logs lockedBuffer
	startAgent(t, Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}, fastEnv())
	srv.heartbeatsAfterWelcome(t, 1)
	hash, known, err := state.LoadEnrolToken(dir)
	if err != nil || !known || hash != state.TokenHash(newToken) {
		t.Fatalf("the record = %q %v %v; want the hash of the token the agent enrolled with", hash, known, err)
	}
	noSecretsIn(t, logs.String(), newToken)
}

// With no state directory the record lives in memory with the identity: an agent
// that enrolled in this process and is then rejected keeps backing off, because
// the token it has is the one it enrolled with.
func TestAnInMemoryIdentityKnowsTheTokenItEnrolledWith(t *testing.T) {
	srv := newFakeServer(t, func(n int, a wire.Auth) reply {
		if a.Mode == wire.ModeEnrol {
			return reply{cert: []byte("issued-cert-der"), drop: true}
		}
		return reply{refuse: wire.ReasonAuthRejected}
	})
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Deadline)
	defer cancel()
	sl := &sleeps{stopAt: 4}
	ctx, sl.cancel = context.WithCancel(ctx)
	if err := runWith(ctx, Config{
		URL: srv.url(), JoinToken: newToken, SocketPath: filepath.Join(t.TempDir(), "none.sock"),
		Log: slog.New(slog.DiscardHandler), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, runEnv{sleep: sl.sleep, rnd: func() float64 { return 0.5 }, roots: httptestRoots()}); err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
			t.Errorf("attempt %d used mode %q, want hello: the agent enrolled with the token it has", n, a.Mode)
		}
	}
}
