// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/agent/internal/state"
	"github.com/ianglennon/bayplan/proto/wire"
)

// deadURL is an address nothing listens on: the dial is refused at once.
const deadURL = "wss://127.0.0.1:1/agent/connect"

func pushLine(t *testing.T, pins []string, urls ...string) string {
	t.Helper()
	b, err := json.Marshal(wire.URLUpdate{T: wire.TypeURLs, URLs: urls, Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func urlsOnDisk(t *testing.T, dir string) []state.URLEntry {
	t.Helper()
	got, err := state.LoadURLs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// tlsPair is two TLS fake servers, A the agent starts on and B the one it is sent to.
func tlsPair(t *testing.T, a, b func(n int, au wire.Auth) reply) (*fakeServer, *fakeServer, []string) {
	t.Helper()
	fa, fb := newFakeServerTLS(t, a), newFakeServerTLS(t, b)
	return fa, fb, []string{serverPin(fa), serverPin(fb)}
}

func TestAPushedURLIsDialedFirstFromTheNextConnectionAndTheOldOneStaysAsAFallback(t *testing.T) {
	dir := t.TempDir()
	var b *fakeServer
	a, b, pins := tlsPair(t,
		func(n int, _ wire.Auth) reply {
			if n == 1 {
				return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, nil, b.url())}, hangup: true}
			}
			return reply{heartbeatMs: 30}
		},
		func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })

	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: pins}, fastEnv())
	waitUntil(t, "the agent to connect to B", func() bool { return b.attempts() >= 1 })
	if n := a.attempts(); n != 1 {
		t.Fatalf("A was dialed %d times, want once: after the push B is dialed first and B works", n)
	}
	got := urlsOnDisk(t, dir)
	if len(got) != 2 || got[0].URL != b.url() || !got[0].ExpiresAt.IsZero() || got[1].URL != a.url() {
		t.Fatalf("urls.json = %+v, want B first with no expiry, then A", got)
	}
	if d := time.Until(got[1].ExpiresAt); d < 29*24*time.Hour || d > 30*24*time.Hour {
		t.Errorf("the previous address expires in %v, want about 30 days", d)
	}
}

func TestWhenThePushedURLCannotBeReachedTheAgentFallsBackToThePreviousOne(t *testing.T) {
	dir := t.TempDir()
	a := newFakeServerTLS(t, nil)
	a.script = func(n int, _ wire.Auth) reply {
		if n == 1 {
			return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, nil, deadURL)}, hangup: true}
		}
		return reply{heartbeatMs: 30}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	waitUntil(t, "the agent to come back to A after the dead address", func() bool { return a.attempts() >= 2 })
	got := urlsOnDisk(t, dir)
	if len(got) != 2 || got[0].URL != deadURL || got[1].URL != a.url() {
		t.Fatalf("urls.json = %+v, want the dead address first and A as the fallback", got)
	}
	// It reconnected as itself, not as a new agent.
	if a2, _ := a.authN(t, 2); a2.Mode != wire.ModeHello {
		t.Errorf("the fallback connection said %q, want hello", a2.Mode)
	}
}

func TestAPreviousURLPastItsThirtyDaysIsNotDialedAndOneInsideThemIs(t *testing.T) {
	for name, tc := range map[string]struct {
		expires    func() time.Time
		wantDialed bool
	}{
		"inside the overlap": {func() time.Time { return time.Now().Add(24 * time.Hour) }, true},
		"past the overlap":   {func() time.Time { return time.Now().Add(-time.Minute) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			enrolledState(t, dir)
			a := newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
			if err := state.SaveURLs(dir, []state.URLEntry{{URL: deadURL}, {URL: a.url(), ExpiresAt: tc.expires()}}); err != nil {
				t.Fatal(err)
			}
			cancel, _ := startAgent(t, Config{URL: a.url(), StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
			if tc.wantDialed {
				waitUntil(t, "the fallback to be dialed", func() bool { return a.attempts() >= 1 })
				return
			}
			time.Sleep(300 * time.Millisecond) // many attempts at the dead address
			cancel()
			if n := a.attempts(); n != 0 {
				t.Fatalf("the expired address was dialed %d times", n)
			}
		})
	}
}

func TestTheEnvironmentURLIsTheOnlyAddressUntilTheServerPushesADifferentOne(t *testing.T) {
	dir := t.TempDir()
	a := newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30, urls: nil} })
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 1)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
		t.Fatal("urls.json was written although the server never said anything about addresses")
	}
}

func TestAWelcomeThatListsTheAddressTheAgentIsOnChangesNothing(t *testing.T) {
	dir := t.TempDir()
	var a *fakeServer
	a = newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30, urls: []string{a.url()}} })
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 1)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
		t.Fatal("urls.json was written for the address the agent already dials")
	}
}

func TestAWelcomeThatListsAnotherAddressMovesTheAgentThereOnTheNextConnection(t *testing.T) {
	dir := t.TempDir()
	var b *fakeServer
	a, b, pins := tlsPair(t,
		func(n int, _ wire.Auth) reply {
			if n == 1 {
				return reply{heartbeatMs: 30, urls: []string{b.url()}, hangup: true}
			}
			return reply{heartbeatMs: 30}
		},
		func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: pins}, fastEnv())
	waitUntil(t, "the agent to connect to B", func() bool { return b.attempts() >= 1 })
	if got := urlsOnDisk(t, dir); len(got) != 2 || got[0].URL != b.url() || got[1].URL != a.url() {
		t.Fatalf("urls.json = %+v, want B then A", got)
	}
}

func TestPinsInAPushAreKeptLikePinsInAWelcome(t *testing.T) {
	dir := t.TempDir()
	other := "sha256/" + strings.Repeat("Q", 43) + "="
	a := newFakeServerTLS(t, nil)
	a.script = func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, []string{serverPin(a), other}, a.url())}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	waitUntil(t, "the pushed pins to be stored", func() bool {
		ps, err := state.LoadPins(dir)
		return err == nil && len(ps) == 2
	})
	ps, _ := state.LoadPins(dir)
	if got := state.ActivePins(ps, time.Now()); got[0] != serverPin(a) || got[1] != other {
		t.Fatalf("stored pins = %v", got)
	}
}

func TestAPushOverASessionThatWasNotVerifiedIsIgnored(t *testing.T) {
	dir := t.TempDir()
	other := "sha256/" + strings.Repeat("Q", 43) + "="
	var b *fakeServer
	a := newFakeServer(t, nil) // plain ws://, so nothing about the server was verified
	b = newFakeServer(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	a.script = func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, []string{other}, b.url())}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir}, fastEnv())
	a.authN(t, 1)
	time.Sleep(200 * time.Millisecond)
	for _, name := range []string{state.URLsFile, state.PinsFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written from a session whose server was never verified", name)
		}
	}
	if n := b.attempts(); n != 0 {
		t.Errorf("the agent went to an address it was sent over an unverified session (%d attempts)", n)
	}
}

func TestAPushThatIsADowngradeOrNotAnAddressIsIgnoredAndTheOldAddressesStay(t *testing.T) {
	dir := t.TempDir()
	a := newFakeServerTLS(t, nil)
	a.script = func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{
			pushLine(t, nil, "ws://insecure.example/agent/connect"), // plain, over a verified session
			pushLine(t, nil, "https://bayplan.example/agent/connect", "not a url", ""),
			pushLine(t, nil),
			`{"t":"urls","urls":"not-a-list"}`,
		}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 1)
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
		t.Fatalf("urls.json was written from pushes with nothing usable in them: %+v", urlsOnDisk(t, dir))
	}
}

func TestAServerThatPushesTooManyAddressesGetsTheFirstFewKept(t *testing.T) {
	dir := t.TempDir()
	var many []string
	for i := range 30 {
		many = append(many, "wss://"+strings.Repeat("h", 1+i%5)+string(rune('a'+i))+".example.com/agent/connect")
	}
	a := newFakeServerTLS(t, nil)
	a.script = func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, nil, many...)}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	waitUntil(t, "the addresses to be stored", func() bool { return len(urlsOnDisk(t, dir)) > 0 })
	if got := urlsOnDisk(t, dir); len(got) > state.MaxURLs || got[0].URL != many[0] {
		t.Fatalf("kept %d addresses starting with %s, want at most %d starting with the server's first", len(got), got[0].URL, state.MaxURLs)
	}
}

// --- a changed BAYPLAN_AGENT_URL (G-02-4b) ---

// strandedAgent is an enrolled agent whose stored list holds the refusing server
// B, started with the accepting server C on a volume that records an older
// BAYPLAN_AGENT_URL.
func strandedAgent(t *testing.T, record string) (dir string, b, c *fakeServer, pins []string) {
	t.Helper()
	dir = t.TempDir()
	enrolledState(t, dir)
	b, c, pins = tlsPair(t,
		func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} },
		func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	if err := state.SaveURLs(dir, []state.URLEntry{{URL: b.url()}}); err != nil {
		t.Fatal(err)
	}
	if record != "" {
		if err := state.SaveEnvURL(dir, record); err != nil {
			t.Fatal(err)
		}
	}
	return dir, b, c, pins
}

func TestAnAgentStartedWithADifferentEnvironmentURLDialsItFirstAndNeverTheStaleStoredOne(t *testing.T) {
	dir, b, c, pins := strandedAgent(t, deadURL)
	startAgent(t, Config{URL: c.url(), StateDir: dir, InstallPins: pins}, fastEnv())
	waitUntil(t, "the agent to say hello to C", func() bool { return c.attempts() >= 1 })
	if a, _ := c.authN(t, 1); a.Mode != wire.ModeHello {
		t.Errorf("the agent said %q at C, want hello: it keeps its identity", a.Mode)
	}
	if n := b.attempts(); n != 0 {
		t.Errorf("the stale stored address was dialed %d times", n)
	}
	got := urlsOnDisk(t, dir)
	if len(got) != 2 || got[0].URL != c.url() || !got[0].ExpiresAt.IsZero() || got[1].URL != b.url() {
		t.Errorf("urls.json = %+v, want C first with no expiry, then B", got)
	}
	if rec, ok, err := state.LoadEnvURL(dir); err != nil || !ok || rec != c.url() {
		t.Errorf("env-url.json = %q, %v, %v; want %q", rec, ok, err, c.url())
	}
}

func TestAnAgentStartedWithTheSameEnvironmentURLAsLastTimeStillDialsTheStoredOneFirst(t *testing.T) {
	dir, b, c, pins := strandedAgent(t, "")
	if err := state.SaveEnvURL(dir, c.url()); err != nil {
		t.Fatal(err)
	}
	startAgent(t, Config{URL: c.url(), StateDir: dir, InstallPins: pins}, fastEnv())
	waitUntil(t, "the stored address to be dialed", func() bool { return b.attempts() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := c.attempts(); n != 0 {
		t.Errorf("BAYPLAN_AGENT_URL was dialed %d times although it did not change and an address pushed by Bayplan is stored", n)
	}
	if got := urlsOnDisk(t, dir); len(got) != 1 || got[0].URL != b.url() {
		t.Errorf("urls.json = %+v, want it untouched", got)
	}
}

func TestAnAgentWithNoRecordOfItsLastEnvironmentURLKeepsTheStoredOrderAndRecordsTheURL(t *testing.T) {
	dir, b, c, pins := strandedAgent(t, "")
	startAgent(t, Config{URL: c.url(), StateDir: dir, InstallPins: pins}, fastEnv())
	waitUntil(t, "the stored address to be dialed", func() bool { return b.attempts() >= 1 })
	if n := c.attempts(); n != 0 {
		t.Errorf("BAYPLAN_AGENT_URL was dialed %d times, want the stored list in charge on the start that has no record", n)
	}
	waitUntil(t, "the start address to be recorded", func() bool {
		rec, ok, _ := state.LoadEnvURL(dir)
		return ok && rec == c.url()
	})
}

func TestAFirstStartRecordsTheEnvironmentURL(t *testing.T) {
	dir := t.TempDir()
	a := newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	startAgent(t, Config{URL: a.url(), JoinToken: "bpjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	waitUntil(t, "the start address to be recorded", func() bool {
		rec, ok, _ := state.LoadEnvURL(dir)
		return ok && rec == a.url()
	})
	if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
		t.Error("urls.json was written although nothing changed")
	}
}
