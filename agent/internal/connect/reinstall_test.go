// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"path/filepath"
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

const reinstallLog = "it dropped the old identity and will enrol again with the new token"

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
		t.Errorf("identity files still on disk when the agent enrolled again (%v); the old identity must be gone first", got)
	}
	srv.heartbeatsAfterWelcome(t, 1) // the identity and the record are written before the first heartbeat
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

// After the agent dropped its identity it is enrolling, and a refusal of that
// enrolment is the same as a refusal of a first one: a wrong token does not fix
// itself, so the agent ends and says so.
func TestARejectedEnrolmentAfterDroppingTheIdentityEndsTheAgent(t *testing.T) {
	dir := t.TempDir()
	enrolledWith(t, dir, oldToken)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} })
	env := fastEnv()
	env.roots = httptestRoots()
	err := runWith(testsupport.Context(t), Config{
		URL: srv.url(), JoinToken: newToken, StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"), Log: slog.New(slog.DiscardHandler),
	}, env)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Code != wire.ReasonAuthRejected {
		t.Fatalf("Run = %v; want the refusal of the enrolment", err)
	}
	if a, _ := srv.authN(t, 2); a.Mode != wire.ModeEnrol || a.Token != newToken {
		t.Errorf("attempt 2 used mode %q, want an enrolment with the new token", a.Mode)
	}
	if n := srv.attempts(); n != 2 {
		t.Errorf("the agent made %d attempts, want 2", n)
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
