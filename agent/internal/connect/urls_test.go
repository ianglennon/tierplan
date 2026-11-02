// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/agent/internal/state"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
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

	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: pins}, fastEnv())
	b.authN(t, 1)
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
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 2) // back at A after the dead address
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
			env, rounds := countingEnv()
			cancel, _ := startAgent(t, Config{URL: a.url(), StateDir: dir, InstallPins: []string{serverPin(a)}}, env)
			if tc.wantDialed {
				a.authN(t, 1)
				return
			}
			// Several rounds of failed attempts at the dead address, each ended by a
			// wait: had the expired address been in the list, it would have been dialed.
			for i := range 5 {
				testsupport.Await(t, fmt.Sprintf("round %d of attempts at the dead address", i+1), rounds)
			}
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
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 1)
	a.heartbeatsAfterWelcome(t, 2) // the welcome was handled before the first heartbeat
	if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
		t.Fatal("urls.json was written although the server never said anything about addresses")
	}
}

func TestAWelcomeThatListsTheAddressTheAgentIsOnChangesNothing(t *testing.T) {
	dir := t.TempDir()
	var a *fakeServer
	a = newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30, urls: []string{a.url()}} })
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 1)
	a.heartbeatsAfterWelcome(t, 2)
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
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: pins}, fastEnv())
	b.authN(t, 1)
	if got := urlsOnDisk(t, dir); len(got) != 2 || got[0].URL != b.url() || got[1].URL != a.url() {
		t.Fatalf("urls.json = %+v, want B then A", got)
	}
}

func TestPinsInAPushAreKeptLikePinsInAWelcome(t *testing.T) {
	dir := t.TempDir()
	other := "sha256/" + strings.Repeat("Q", 43) + "="
	a := newFakeServerTLS(t, nil)
	a.script = func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, []string{serverPin(a), other}, a.url()), fenceLine}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.awaitFence(t) // the push before the fence has been handled
	ps, err := state.LoadPins(dir)
	if err != nil || len(ps) != 2 {
		t.Fatalf("stored pins = %v, %v; want the two pushed", ps, err)
	}
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
		return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, []string{other}, b.url()), fenceLine}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir}, fastEnv())
	a.authN(t, 1)
	a.awaitFence(t) // the push before the fence has been handled, and ignored
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
			pushLine(t, nil, "https://tierplan.example/agent/connect", "not a url", ""),
			pushLine(t, nil),
			`{"t":"urls","urls":"not-a-list"}`,
			fenceLine,
		}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.authN(t, 1)
	a.awaitFence(t) // every push before the fence has been handled
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
		return reply{heartbeatMs: 30, toAgent: []string{pushLine(t, nil, many...), fenceLine}}
	}
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.awaitFence(t) // the push before the fence has been handled
	if got := urlsOnDisk(t, dir); len(got) == 0 || len(got) > state.MaxURLs || got[0].URL != many[0] {
		t.Fatalf("kept %d addresses starting with %s, want at most %d starting with the server's first", len(got), got[0].URL, state.MaxURLs)
	}
}

// --- a changed TIERPLAN_AGENT_URL (G-02-4b) ---

// strandedAgent is an enrolled agent whose stored list holds the refusing server
// B, started with the accepting server C on a volume that records an older
// TIERPLAN_AGENT_URL.
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
	// B refuses, and the agent goes round again: had it meant to dial C, it would have
	// done so between the two attempts at B.
	b.authN(t, 2)
	if n := c.attempts(); n != 0 {
		t.Errorf("TIERPLAN_AGENT_URL was dialed %d times although it did not change and an address pushed by TierPlan is stored", n)
	}
	if got := urlsOnDisk(t, dir); len(got) != 1 || got[0].URL != b.url() {
		t.Errorf("urls.json = %+v, want it untouched", got)
	}
}

func TestAnAgentWithNoRecordOfItsLastEnvironmentURLKeepsTheStoredOrderAndRecordsTheURL(t *testing.T) {
	dir, b, c, pins := strandedAgent(t, "")
	startAgent(t, Config{URL: c.url(), StateDir: dir, InstallPins: pins}, fastEnv())
	b.authN(t, 2) // see the test above: C is not dialed between two attempts at B
	if n := c.attempts(); n != 0 {
		t.Errorf("TIERPLAN_AGENT_URL was dialed %d times, want the stored list in charge on the start that has no record", n)
	}
	// The start address is recorded before the first dial.
	if rec, ok, _ := state.LoadEnvURL(dir); !ok || rec != c.url() {
		t.Errorf("env-url.json = %q, %v; want %q recorded", rec, ok, c.url())
	}
}

func TestAFirstStartRecordsTheEnvironmentURL(t *testing.T) {
	dir := t.TempDir()
	a := newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}}, fastEnv())
	a.heartbeatsAfterWelcome(t, 2) // the start address is recorded before the first dial, the welcome handled before the first heartbeat
	if rec, ok, _ := state.LoadEnvURL(dir); !ok || rec != a.url() {
		t.Errorf("env-url.json = %q, %v; want %q recorded", rec, ok, a.url())
	}
	if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
		t.Error("urls.json was written although nothing changed")
	}
}

// --- plain ws:// addresses the agent stored or learned (T-02-31) ---

const plainElsewhere = "ws://plain.example/agent/connect"

// hostRecorder passes every request on to a real transport, and records the
// requests made to a host whose address must never be dialed. It answers requests
// for that host itself, with an error, so the test depends on no DNS.
type hostRecorder struct {
	host string
	next http.RoundTripper
	rt   recordingTransport
}

func newHostRecorder(host string) *hostRecorder {
	return &hostRecorder{host: host, next: &http.Transport{}}
}

func (h *hostRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() == h.host {
		return h.rt.RoundTrip(r)
	}
	return h.next.RoundTrip(r)
}

func (h *hostRecorder) client() *http.Client { return &http.Client{Transport: h} }

func TestAStoredPlainAddressForAnotherHostIsNeverDialedWithoutTheSwitch(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	if err := state.SaveURLs(dir, []state.URLEntry{{URL: plainElsewhere}, {URL: srv.url()}}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveEnvURL(dir, srv.url()); err != nil {
		t.Fatal(err)
	}
	rec := newHostRecorder("plain.example")
	logs := &lockedBuffer{}
	startAgent(t, Config{URL: srv.url(), StateDir: dir, HTTPClient: rec.client(), Log: slog.New(slog.NewTextHandler(logs, nil))}, fastEnv())
	srv.authN(t, 1)

	if got := rec.rt.seen(); len(got) != 0 {
		t.Errorf("the agent made requests %v to a stored plain address for another host", got)
	}
	if n := strings.Count(logs.String(), "ignored stored addresses"); n != 1 {
		t.Errorf("the agent logged the ignored stored address %d times, want once: %s", n, logs.String())
	}
	if strings.Contains(logs.String(), "skipped an address it cannot dial") {
		t.Errorf("the stored plain address reached the dial list: %s", logs.String())
	}
}

func TestAStoredPlainAddressIsTriedWithTheSwitch(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	if err := state.SaveURLs(dir, []state.URLEntry{{URL: plainElsewhere}, {URL: srv.url()}}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveEnvURL(dir, srv.url()); err != nil {
		t.Fatal(err)
	}
	rec := newHostRecorder("plain.example")
	startAgent(t, Config{URL: srv.url(), StateDir: dir, HTTPClient: rec.client(), AllowInsecure: true}, fastEnv())
	srv.authN(t, 1) // dialed after the plain address, which came first
	if len(rec.rt.seen()) == 0 {
		t.Error("with the switch the stored plain address was never tried")
	}
	if got := urlsOnDisk(t, dir); len(got) != 2 || got[0].URL != plainElsewhere {
		t.Errorf("urls.json = %+v, want both stored addresses untouched", got)
	}
}

func TestTheRecordedStartAddressIsNeverDialed(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	if err := state.SaveURLs(dir, []state.URLEntry{{URL: srv.url()}}); err != nil {
		t.Fatal(err)
	}
	// An older TIERPLAN_AGENT_URL, plain and for another host, is only compared.
	if err := state.SaveEnvURL(dir, plainElsewhere); err != nil {
		t.Fatal(err)
	}
	rec := newHostRecorder("plain.example")
	startAgent(t, Config{URL: srv.url(), StateDir: dir, HTTPClient: rec.client()}, fastEnv())
	srv.authN(t, 1)
	// env-url.json is rewritten before the first dial.
	if got, ok, err := state.LoadEnvURL(dir); err != nil || !ok || got != srv.url() {
		t.Fatalf("env-url.json = %q, %v, %v; want it rewritten to %q", got, ok, err, srv.url())
	}
	if got := rec.rt.seen(); len(got) != 0 {
		t.Errorf("the agent made requests %v to the address it last started with", got)
	}
	for _, e := range urlsOnDisk(t, dir) {
		if e.URL == plainElsewhere {
			t.Errorf("the recorded plain address was added to the dial list: %+v", urlsOnDisk(t, dir))
		}
	}
}

func TestNewTargetRefusesWhatDialableURLRefuses(t *testing.T) {
	for _, tc := range []struct {
		url        string
		allowPlain bool
		ok         bool
	}{
		{"wss://tierplan.example/agent/connect", false, true},
		{"ws://127.0.0.1:7480/agent/connect", false, true},
		{plainElsewhere, false, false},
		{plainElsewhere, true, true},
		{"wss://user:pw@tierplan.example/agent/connect", true, false},
		{"https://tierplan.example/agent/connect", true, false},
	} {
		_, err := newTarget(tc.url, tc.allowPlain)
		if (err == nil) != tc.ok {
			t.Errorf("newTarget(%q, %v) = %v, want ok %v", tc.url, tc.allowPlain, err, tc.ok)
		}
	}
}

func TestAPushedPlainAddressIsIgnoredEvenWithTheSwitch(t *testing.T) {
	for _, allow := range []bool{false, true} {
		dir := t.TempDir()
		a := newFakeServerTLS(t, nil)
		a.script = func(int, wire.Auth) reply {
			return reply{heartbeatMs: 30, toAgent: []string{
				pushLine(t, nil, "ws://insecure.example/agent/connect", "ws://127.0.0.1:1/agent/connect"),
				fenceLine,
			}}
		}
		startAgent(t, Config{URL: a.url(), JoinToken: "cmjt_x", StateDir: dir, InstallPins: []string{serverPin(a)}, AllowInsecure: allow}, fastEnv())
		a.authN(t, 1)
		a.awaitFence(t) // the push before the fence has been handled
		if _, err := os.Stat(filepath.Join(dir, state.URLsFile)); err == nil {
			t.Errorf("switch %v: urls.json was written from a push of plain addresses: %+v", allow, urlsOnDisk(t, dir))
		}
	}
}
