// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/tierplan/agent/internal/state"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

func fakeEngine(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tpag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path
}

func TestRunRefusesAnAddressThatIsNotAWebSocketURL(t *testing.T) {
	for _, bad := range []string{"", "https://tierplan.example/agent/connect", "not a url", "wss://"} {
		err := Run(context.Background(), Config{URL: bad, JoinToken: "x"})
		if err == nil || strings.Contains(err.Error(), "tpjt") {
			t.Errorf("Run(%q) = %v, want an error about the address", bad, err)
		}
	}
}

// recordingTransport counts every request it is given and answers none of them.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []string
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, r.URL.String())
	rt.mu.Unlock()
	return nil, errors.New("the test transport dials nothing")
}

func (rt *recordingTransport) seen() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.reqs...)
}

// A plain ws:// address for a host that is not loopback sends the join token to a
// peer the agent never authenticated, so Run refuses it at once and never dials
// unless the agent itself carries TIERPLAN_AGENT_ALLOW_INSECURE (T-02-31).
func TestRunRefusesPlainWSToAnotherHostWithoutTheSwitchAndNeverDials(t *testing.T) {
	rt := &recordingTransport{}
	ctx := testsupport.Context(t)
	err := Run(ctx, Config{
		URL: "ws://tierplan.example/agent/connect", JoinToken: "tpjt_tok",
		HTTPClient: &http.Client{Transport: rt}, SocketPath: filepath.Join(t.TempDir(), "none.sock"),
	})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_ALLOW_INSECURE") || !strings.Contains(err.Error(), "TIERPLAN_AGENT_URL") {
		t.Fatalf("Run = %v, want an error naming TIERPLAN_AGENT_URL and TIERPLAN_AGENT_ALLOW_INSECURE", err)
	}
	if strings.Contains(err.Error(), "tpjt_tok") {
		t.Errorf("the error carries the token: %v", err)
	}
	if got := rt.seen(); len(got) != 0 {
		t.Errorf("the agent made requests %v, want none", got)
	}
}

func TestARefusalNamesItsCode(t *testing.T) {
	err := error(&RefusedError{Code: "auth_rejected", Detail: "the join token was not accepted"})
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "auth_rejected") || !strings.Contains(err.Error(), "join token") {
		t.Fatalf("error = %v", err)
	}
	if got := (&RefusedError{Code: "revoked"}).Error(); !strings.Contains(got, "revoked") {
		t.Errorf("error = %q", got)
	}
}

// reply is what the fake server does with one connection.
type reply struct {
	refuse      string   // non-empty: refuse with this code
	cert        []byte   // the certificate in an enrol welcome
	heartbeatMs int      // zero: 40 ms, so a test sees heartbeats quickly
	drop        bool     // close the connection right after the welcome
	pins        []string // the pins in the welcome
	urls        []string // the addresses in the welcome
	// hangup ends the connection a moment after the control stream has opened and
	// toAgent has been written, so the agent reconnects.
	hangup  bool
	toAgent []string
	// onRenew answers a renew message from the agent with the message it returns;
	// nil answers nothing.
	onRenew func() any
	// dockerRequests are raw HTTP requests the server sends on streams it opens
	// after the welcome, the way its Docker client would.
	dockerRequests []string
	// fenceAfterRenew makes the server send a fence (see fenceLine) right after it
	// answers a renew, so a test can wait for the agent to have handled the answer.
	fenceAfterRenew bool
}

// fenceID is the id of the update the fake server sends as a fence.
const fenceID = "fence"

// fenceLine is an update request that follows other control lines. The agent reads
// control lines one at a time and handles the pushes, renewals and pins among them
// before it reads the next, so the update_result it answers a fence with proves that
// every line sent before the fence was handled. With no self-maintenance on the agent
// the answer is an error and nothing runs; a session that was not verified refuses
// it. Either way the result reaches fakeServer.results.
var fenceLine = `{"t":"update","id":"` + fenceID + `","image":"fence/fence:0"}`

// fakeServer is the server end of the agent's connection: it runs the real
// handshake messages and the control stream, and records what the agent sent.
type fakeServer struct {
	ts     *httptest.Server
	script func(n int, a wire.Auth) reply

	mu       sync.Mutex
	auths    []wire.Auth
	nonces   [][]byte
	scripted int           // handshakes whose script has run
	changed  chan struct{} // closed and replaced, under mu, each time scripted grows
	kinds    []byte
	beats    chan wire.Heartbeat
	denied   chan wire.Denied
	results  chan wire.UpdateResult // every update_result the agent wrote
	started  chan int               // connection number, after the welcome
	// answered gets one value each time the agent answers a Docker request the
	// server sent (see reply.dockerRequests).
	answered chan struct{}
}

// newFakeCore is a fake server that is not started.
func newFakeCore(script func(n int, a wire.Auth) reply) *fakeServer {
	return &fakeServer{script: script, changed: make(chan struct{}), beats: make(chan wire.Heartbeat, 256), denied: make(chan wire.Denied, 256), results: make(chan wire.UpdateResult, 64), started: make(chan int, 64), answered: make(chan struct{}, 64)}
}

func newFakeServer(t *testing.T, script func(n int, a wire.Auth) reply) *fakeServer {
	t.Helper()
	f := newFakeCore(script)
	f.ts = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() {
		f.ts.CloseClientConnections()
		f.ts.Close()
	})
	return f
}

func (f *fakeServer) url() string {
	return "ws" + strings.TrimPrefix(f.ts.URL, "http") + "/agent/connect"
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	b, _ := json.Marshal(wire.Challenge{T: wire.TypeChallenge, Proto: wire.ProtoVersion, Nonce: nonce, TS: time.Now().UnixMilli()})
	if c.Write(ctx, websocket.MessageText, b) != nil {
		return
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var a wire.Auth
	if json.Unmarshal(data, &a) != nil {
		return
	}
	f.mu.Lock()
	f.auths = append(f.auths, a)
	f.nonces = append(f.nonces, nonce)
	n := len(f.auths)
	f.mu.Unlock()

	rep := f.script(n, a)
	// Only now is the handshake seen to a test: whatever the script recorded is there.
	f.mu.Lock()
	f.scripted++
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
	if rep.refuse != "" {
		b, _ := json.Marshal(wire.Refuse{T: wire.TypeRefuse, Code: rep.refuse, Detail: "refused by the test"})
		_ = c.Write(ctx, websocket.MessageText, b)
		_ = c.Close(websocket.StatusPolicyViolation, rep.refuse)
		return
	}
	if a.Mode == wire.ModeEnrol && rep.cert == nil {
		rep.cert = []byte("default-cert-der")
	}
	hb := rep.heartbeatMs
	if hb == 0 {
		hb = 40
	}
	b, _ = json.Marshal(wire.Welcome{T: wire.TypeWelcome, AgentID: "agent-7", EnvID: "env-7", Cert: rep.cert, HeartbeatMs: hb, Pins: rep.pins, URLs: rep.urls, ServerTime: time.Now().UnixMilli()})
	if c.Write(ctx, websocket.MessageText, b) != nil {
		return
	}
	if rep.drop {
		_ = c.CloseNow()
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	ym, err := yamux.Client(websocket.NetConn(ctx, c, websocket.MessageBinary), cfg)
	if err != nil {
		return
	}
	defer func() { _ = ym.Close() }()
	f.started <- n
	st, err := ym.AcceptStream() // the agent opens the control stream
	if err != nil {
		return
	}
	br := bufio.NewReader(st)
	kind, err := br.ReadByte()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.kinds = append(f.kinds, kind)
	f.mu.Unlock()
	for _, line := range rep.toAgent {
		_, _ = st.Write([]byte(line + "\n"))
	}
	if rep.hangup {
		// Hang up once the agent has handled everything sent so far: its answer to a
		// fence says so.
		_, _ = st.Write([]byte(fenceLine + "\n"))
		for sc := bufio.NewScanner(br); sc.Scan(); {
			var ur wire.UpdateResult
			if json.Unmarshal(sc.Bytes(), &ur) == nil && ur.T == wire.TypeUpdateResult && ur.ID == fenceID {
				break
			}
		}
		return
	}
	for _, raw := range rep.dockerRequests {
		go func() {
			ds, err := ym.OpenStream()
			if err != nil {
				return
			}
			defer func() { _ = ds.Close() }()
			_, _ = ds.Write(append([]byte{wire.StreamKindDockerHTTP}, raw...))
			var first [1]byte
			if n, _ := ds.Read(first[:]); n > 0 {
				select {
				case f.answered <- struct{}{}:
				default:
				}
			}
			_, _ = io.Copy(io.Discard, ds)
		}()
	}
	sc := bufio.NewScanner(br)
	for sc.Scan() {
		var h wire.Heartbeat
		if json.Unmarshal(sc.Bytes(), &h) == nil && h.T == wire.TypeHeartbeat {
			select {
			case f.beats <- h:
			default:
			}
		}
		var rn wire.Renew
		if json.Unmarshal(sc.Bytes(), &rn) == nil && rn.T == wire.TypeRenew && rep.onRenew != nil {
			if v := rep.onRenew(); v != nil {
				b, _ := json.Marshal(v)
				_, _ = st.Write(append(b, '\n'))
				if rep.fenceAfterRenew {
					_, _ = st.Write([]byte(fenceLine + "\n"))
				}
			}
		}
		var ur wire.UpdateResult
		if json.Unmarshal(sc.Bytes(), &ur) == nil && ur.T == wire.TypeUpdateResult {
			select {
			case f.results <- ur:
			default:
			}
		}
		var d wire.Denied
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.T == wire.TypeDenied {
			select {
			case f.denied <- d:
			default:
			}
		}
	}
}

// authN returns the nth handshake the server recorded, waiting for it to arrive and
// for the script that decides its answer to have run.
func (f *fakeServer) authN(t *testing.T, n int) (wire.Auth, []byte) {
	t.Helper()
	ctx := testsupport.Context(t)
	for {
		f.mu.Lock()
		if f.scripted >= n {
			a, nonce := f.auths[n-1], f.nonces[n-1]
			f.mu.Unlock()
			return a, nonce
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("testsupport: timed out waiting for the server to see handshake %d", n)
		}
	}
}

// heartbeatsAfterWelcome reads n heartbeats the agent sent. The agent heartbeats
// only once it has handled the welcome (saved its identity, its pins and its
// addresses, and marked itself connected), so a state change a welcome causes has
// happened by the first heartbeat, and its absence is settled by the second.
func (f *fakeServer) heartbeatsAfterWelcome(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		testsupport.Await(t, fmt.Sprintf("heartbeat %d of the agent", i+1), f.beats)
	}
}

// awaitFence waits for the agent's answer to a fence, which proves that it handled
// every control line the server sent before the fence.
func (f *fakeServer) awaitFence(t *testing.T) {
	t.Helper()
	for {
		if res := testsupport.Await(t, "the agent's answer to the fence", f.results); res.ID == fenceID {
			return
		}
	}
}

func (f *fakeServer) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auths)
}

// startAgent runs the agent until the test ends. The returned channel holds its
// result.
func startAgent(t *testing.T, cfg Config, env runEnv) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(t.TempDir(), "none.sock") // never the real Docker socket
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- runWith(ctx, cfg, env) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ch:
		case <-time.After(testsupport.Deadline):
			t.Error("the agent did not stop when its context ended")
		}
	})
	return cancel, ch
}

func fastEnv() runEnv {
	return runEnv{
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(min(d, 5*time.Millisecond)): // wall-clock: paces a fake sleeper; no assertion depends on it
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// countingEnv is fastEnv with a signal: the channel gets a value each time the agent
// waits before another round of attempts, which is how a test knows that a round of
// dials has ended.
func countingEnv() (runEnv, <-chan struct{}) {
	env := fastEnv()
	inner := env.sleep
	rounds := make(chan struct{}, 1024)
	env.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case rounds <- struct{}{}:
		default:
		}
		return inner(ctx, d)
	}
	return env, rounds
}

func enrolledState(t *testing.T, dir string) state.State {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := state.State{AgentID: "agent-7", EnvID: "env-7", Key: priv, Cert: []byte("saved-cert-der")}
	if err := state.Save(dir, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFirstStartEnrolsWithTheTokenAndKeepsItsIdentityOnTheVolume(t *testing.T) {
	dir := t.TempDir()
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{cert: []byte("issued-cert-der")} })
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

	a, _ := srv.authN(t, 1)
	if a.Mode != wire.ModeEnrol || a.Token != "tpjt_tok" || len(a.Pub) != ed25519.PublicKeySize || len(a.Cert) != 0 {
		t.Fatalf("first handshake = mode %q token %q pub %d cert %d; want enrol with the token and a key", a.Mode, a.Token, len(a.Pub), len(a.Cert))
	}
	srv.heartbeatsAfterWelcome(t, 1) // the identity and the connected marker are written before the first heartbeat
	got, _, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "agent-7" || got.EnvID != "env-7" || string(got.Cert) != "issued-cert-der" || !bytes.Equal(publicOf(got.Key), a.Pub) {
		t.Fatalf("saved state = %+v; want the welcome's IDs and certificate and the key the handshake proved", got)
	}
	// The marker is written after the identity is saved, and before the first heartbeat.
	if _, err := os.Stat(filepath.Join(dir, state.ConnectedFile)); err != nil {
		t.Errorf("the connected marker is missing after the first heartbeat: %v", err)
	}
}

func TestARestartReconnectsInHelloModeWithItsCertificateAndNoToken(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
	startAgent(t, Config{URL: srv.url(), StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

	a, serverNonce := srv.authN(t, 1)
	if a.Mode != wire.ModeHello || a.AgentID != "agent-7" || a.Token != "" || len(a.Pub) != 0 || string(a.Cert) != "saved-cert-der" {
		t.Fatalf("handshake = mode %q agent %q token %q cert %q; want hello with the saved certificate and no token", a.Mode, a.AgentID, a.Token, a.Cert)
	}
	aud, _ := authn.Audience(srv.url())
	tr := authn.Transcript(wire.ModeHello, wire.ProtoVersion, serverNonce, a.Nonce, aud, a.Cert, a.EKM)
	if !authn.Verify(publicOf(saved.Key), tr, a.Sig) {
		t.Fatal("the hello signature does not verify under the saved key over the transcript with the certificate")
	}
	if a.Facts.EngineReachable {
		t.Error("the engine socket does not exist, yet the facts say it is reachable")
	}
}

func TestAnAgentThatLosesItsConnectionReconnectsWithoutTheTokenOrANewKey(t *testing.T) {
	dir := t.TempDir()
	srv := newFakeServer(t, func(n int, _ wire.Auth) reply {
		if n == 1 {
			return reply{cert: []byte("issued-cert-der"), drop: true}
		}
		return reply{}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

	first, _ := srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if first.Mode != wire.ModeEnrol || second.Mode != wire.ModeHello || second.Token != "" || string(second.Cert) != "issued-cert-der" {
		t.Fatalf("modes = %q then %q (token %q, cert %q); want enrol, then hello with the issued certificate", first.Mode, second.Mode, second.Token, second.Cert)
	}
}

func TestWithNoStateDirectoryTheIdentityStaysInMemoryForTheLifeOfTheProcess(t *testing.T) {
	srv := newFakeServer(t, func(n int, _ wire.Auth) reply {
		if n == 1 {
			return reply{cert: []byte("issued-cert-der"), drop: true}
		}
		return reply{}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok"}, fastEnv())
	srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if second.Mode != wire.ModeHello {
		t.Fatalf("second handshake mode %q; an in-process reconnect must not enrol a second agent", second.Mode)
	}
}

func TestAfterTheWelcomeTheAgentOpensOneControlStreamAndHeartbeats(t *testing.T) {
	dir := t.TempDir()
	sock := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		http.NotFound(w, r)
	})
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: sock}, runEnv{
		now: func() time.Time { return time.UnixMilli(1_700_000_000_123) },
	})
	var got []wire.Heartbeat
	for len(got) < 3 {
		got = append(got, testsupport.Await(t, fmt.Sprintf("heartbeat %d", len(got)+1), srv.beats))
	}
	for _, h := range got {
		if h.T != wire.TypeHeartbeat || !h.EngineReachable || h.ClockMs != 1_700_000_000_123 {
			t.Fatalf("heartbeat = %+v; want engineReachable true and the agent's clock", h)
		}
	}
	srv.mu.Lock()
	kinds := append([]byte(nil), srv.kinds...)
	srv.mu.Unlock()
	if len(kinds) != 1 || kinds[0] != wire.StreamKindControl {
		t.Fatalf("control stream kinds = %v, want exactly one stream of kind %#x", kinds, wire.StreamKindControl)
	}
}

func TestAHeartbeatSaysTheEngineIsUnreachableWhenPingFails(t *testing.T) {
	dir := t.TempDir()
	broken := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	for name, sock := range map[string]string{"no socket": filepath.Join(dir, "none.sock"), "ping answers 500": broken} {
		srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{heartbeatMs: 30} })
		startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", SocketPath: sock}, runEnv{})
		if h := testsupport.Await(t, name+": a heartbeat", srv.beats); h.EngineReachable {
			t.Errorf("%s: heartbeat says the engine is reachable", name)
		}
	}
}

func TestTheAgentIgnoresControlMessagesItDoesNotKnow(t *testing.T) {
	srv := newFakeServer(t, func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{`{"t":"from-the-future","x":1}`, `not json at all`, `{"t":"heartbeat"}`}}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok"}, runEnv{})
	srv.heartbeatsAfterWelcome(t, 3) // the agent goes on heartbeating after the unknown messages
	if srv.attempts() != 1 {
		t.Errorf("the agent reconnected %d times; an unknown control message is not an error", srv.attempts())
	}
}

// sleeps records the waits the agent asked for and stops it after n of them.
type sleeps struct {
	mu     sync.Mutex
	waited []time.Duration
	stopAt int
	cancel context.CancelFunc
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waited = append(s.waited, d)
	n := len(s.waited)
	s.mu.Unlock()
	if n >= s.stopAt {
		s.cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waited...)
}

func runUntilSleeps(t *testing.T, cfg Config, stopAt int) (*sleeps, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Deadline)
	defer cancel()
	sl := &sleeps{stopAt: stopAt}
	ctx, sl.cancel = context.WithCancel(ctx)
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	err := runWith(ctx, cfg, runEnv{sleep: sl.sleep, rnd: func() float64 { return 0.5 }})
	return sl, err
}

// identityFiles are the files that make up an agent's identity on its volume.
var identityFiles = []string{state.IdentityFile, state.KeyFile, state.CertFile}

// noIdentityOnDisk reports whether none of the identity files exists in dir.
func noIdentityOnDisk(dir string) bool {
	for _, name := range identityFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// D-10: a host that rejoins its swarm gets a new task that finds the identity of
// the agent the reaper revoked. With a join token at hand the agent drops that
// identity and enrols again, as a new agent, by itself.
func TestARevokedAgentWithAJoinTokenDiscardsItsIdentityAndEnrolsAgain(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	var mu sync.Mutex
	var onDiskAtEnrol []bool
	srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonRevoked}
		}
		mu.Lock()
		onDiskAtEnrol = append(onDiskAtEnrol, !noIdentityOnDisk(dir))
		mu.Unlock()
		return reply{cert: []byte("rejoined-cert-der")}
	})
	var logs lockedBuffer
	startAgent(t, Config{
		URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}, fastEnv())

	first, _ := srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if first.Mode != wire.ModeHello || second.Mode != wire.ModeEnrol || second.Token != "tpjt_tok" {
		t.Fatalf("modes = %q then %q (token %q); want hello, refused revoked, then enrol with the token", first.Mode, second.Mode, second.Token)
	}
	if bytes.Equal(second.Pub, publicOf(saved.Key)) {
		t.Error("the re-enrolment presented the old key; it must be a fresh one")
	}
	mu.Lock()
	if len(onDiskAtEnrol) == 0 || onDiskAtEnrol[0] {
		t.Errorf("identity files still on disk when the agent enrolled again (%v); the revoked identity must be gone first", onDiskAtEnrol)
	}
	mu.Unlock()
	srv.heartbeatsAfterWelcome(t, 1)
	if s, ok, _ := state.Load(dir); !ok || string(s.Cert) != "rejoined-cert-der" {
		t.Fatalf("the new identity was not saved: %+v %v", s, ok)
	}
	got, _, _ := state.Load(dir)
	if got.Key.Equal(saved.Key) {
		t.Error("the saved identity still has the revoked agent's key")
	}
	if !strings.Contains(logs.String(), "code=revoked") {
		t.Errorf("the refusal was not logged:\n%s", logs.String())
	}
	if n := srv.attempts(); n != 2 {
		t.Errorf("the agent made %d attempts, want 2 (hello, then one enrolment)", n)
	}
}

// Without a join token nothing the agent can do changes the answer, so it keeps
// its identity, waits the maximum and says once what it needs.
func TestARevokedAgentWithNoJoinTokenWaitsTheMaximumAndSaysOnceThatItNeedsAToken(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonRevoked} })
	var logs lockedBuffer
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)), SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 3)
	if err != nil {
		t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
	}
	for i, d := range sl.all() {
		if d != 60*time.Second {
			t.Errorf("wait %d = %v, want the 60s maximum", i+1, d)
		}
	}
	if n := strings.Count(logs.String(), "code=revoked"); n != 1 {
		t.Errorf("the code revoked was logged %d times, want once:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "no join token to enrol again with"); n != 1 || strings.Contains(logs.String(), "drops the revoked identity") {
		t.Errorf("want one hint that a join token is needed to enrol again, and no talk of discarding:\n%s", logs.String())
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || !got.Key.Equal(saved.Key) || string(got.Cert) != "saved-cert-der" {
		t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
	}
	for n := 1; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
			t.Errorf("attempt %d used mode %q, want hello: with no token there is nothing to enrol with", n, a.Mode)
		}
	}
	if srv.attempts() < 3 {
		t.Errorf("the agent tried %d times, want it to keep retrying", srv.attempts())
	}
}

// D-31: after TierPlan is restored from a backup, an agent that was installed after
// the backup holds a certificate the restored environment issued but does not
// know, and is refused unknown_agent. With a join token the agent drops that
// identity and enrols again, exactly as for revoked.
func TestAnUnknownAgentRefusalWithAJoinTokenDiscardsTheIdentityAndEnrolsAgain(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	var mu sync.Mutex
	var onDiskAtEnrol []bool
	srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonUnknownAgent}
		}
		mu.Lock()
		onDiskAtEnrol = append(onDiskAtEnrol, !noIdentityOnDisk(dir))
		mu.Unlock()
		return reply{cert: []byte("reinstalled-cert-der")}
	})
	var logs lockedBuffer
	startAgent(t, Config{
		URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}, fastEnv())

	first, _ := srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if first.Mode != wire.ModeHello || second.Mode != wire.ModeEnrol || second.Token != "tpjt_tok" {
		t.Fatalf("modes = %q then %q (token %q); want hello, refused unknown_agent, then enrol with the token", first.Mode, second.Mode, second.Token)
	}
	if bytes.Equal(second.Pub, publicOf(saved.Key)) {
		t.Error("the re-enrolment presented the old key; it must be a fresh one")
	}
	mu.Lock()
	if len(onDiskAtEnrol) == 0 || onDiskAtEnrol[0] {
		t.Errorf("identity files still on disk when the agent enrolled again (%v)", onDiskAtEnrol)
	}
	mu.Unlock()
	srv.heartbeatsAfterWelcome(t, 1)
	if s, ok, _ := state.Load(dir); !ok || string(s.Cert) != "reinstalled-cert-der" {
		t.Fatalf("the new identity was not saved: %+v %v", s, ok)
	}
	if !strings.Contains(logs.String(), "code=unknown_agent") {
		t.Errorf("the refusal was not logged:\n%s", logs.String())
	}
	if n := srv.attempts(); n != 2 {
		t.Errorf("the agent made %d attempts, want 2 (hello, then one enrolment)", n)
	}
}

// Without a join token the agent keeps its identity, retries at the 60 second
// maximum and says once what to do.
func TestAnUnknownAgentRefusalWithNoJoinTokenWaitsTheMaximumAndSaysOnceWhatToDo(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonUnknownAgent} })
	var logs lockedBuffer
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)), SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 3)
	if err != nil {
		t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
	}
	for i, d := range sl.all() {
		if d != 60*time.Second {
			t.Errorf("wait %d = %v, want the 60s maximum", i+1, d)
		}
	}
	const hint = "this agent was enrolled after the backup this TierPlan was restored from; reinstall it with the environment's install command"
	if n := strings.Count(logs.String(), "code=unknown_agent"); n != 1 {
		t.Errorf("the code unknown_agent was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), hint) {
		t.Errorf("the hint is missing from the log:\n%s", logs.String())
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || !got.Key.Equal(saved.Key) || string(got.Cert) != "saved-cert-der" {
		t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
	}
	for n := 1; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
			t.Errorf("attempt %d used mode %q, want hello: with no token there is nothing to enrol with", n, a.Mode)
		}
	}
}

func TestTheHintForUnknownAgentNamesTheBackupAndTheInstallCommand(t *testing.T) {
	r := &runner{}
	got := r.hint(wire.ReasonUnknownAgent)
	const want = "this agent was enrolled after the backup this TierPlan was restored from; reinstall it with the environment's install command"
	if got != want {
		t.Errorf("hint = %q, want %q", got, want)
	}
}

// An enrolled agent that TierPlan refuses auth_rejected is most often one whose
// address changed while it was away; the hint says how it is brought back with
// its identity (G-02-4b).
func TestAnEnrolledAgentRefusedAuthRejectedIsToldToStartWithTheNewAddressAndKeepItsIdentity(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} })
	var logs lockedBuffer
	if _, err := runUntilSleeps(t, Config{
		URL: srv.url(), StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)), SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 2); err != nil {
		t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
	}
	if n := strings.Count(logs.String(), "code=auth_rejected"); n != 1 {
		t.Fatalf("the code auth_rejected was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "TIERPLAN_AGENT_URL") || !strings.Contains(logs.String(), "keeps its identity") {
		t.Errorf("the hint should name TIERPLAN_AGENT_URL and say the agent keeps its identity:\n%s", logs.String())
	}
}

// A blocked host is on an administrator's list: its identity is still good and a
// join token does not help, so the agent keeps both and waits.
func TestABlockedAgentWaitsTheMaximumLogsTheCodeOnceAndKeepsItsIdentity(t *testing.T) {
	for _, token := range []string{"", "tpjt_tok"} {
		t.Run("token="+token, func(t *testing.T) {
			dir := t.TempDir()
			saved := enrolledState(t, dir)
			srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonBlocked} })
			var logs lockedBuffer
			sl, err := runUntilSleeps(t, Config{
				URL: srv.url(), JoinToken: token, StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
				SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
			}, 3)
			if err != nil {
				t.Fatalf("Run = %v; a refusal never ends an enrolled agent", err)
			}
			for i, d := range sl.all() {
				if d != 60*time.Second {
					t.Errorf("wait %d = %v, want the 60s maximum", i+1, d)
				}
			}
			if n := strings.Count(logs.String(), "code=blocked"); n != 1 {
				t.Errorf("the code blocked was logged %d times, want once:\n%s", n, logs.String())
			}
			got, ok, err := state.Load(dir)
			if err != nil || !ok || !got.Key.Equal(saved.Key) || string(got.Cert) != "saved-cert-der" {
				t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
			}
			for n := 1; n <= srv.attempts(); n++ {
				if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
					t.Errorf("attempt %d used mode %q, want hello", n, a.Mode)
				}
			}
		})
	}
}

// A manually revoked host is also blocked. The agent drops the revoked identity
// and enrols once; the block refuses it, and from there it waits the maximum
// between tries: no loop of enrolments faster than the backoff, and no exit that
// a restart policy would turn into one.
func TestARevokedAgentWhoseHostIsBlockedEnrolsOnceThenWaitsTheMaximum(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(_ int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonRevoked}
		}
		return reply{refuse: wire.ReasonBlocked}
	})
	var logs lockedBuffer
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		SocketPath: filepath.Join(dir, "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 4)
	if err != nil {
		t.Fatalf("Run = %v; a blocked enrolment after a revoke must wait, not end the agent", err)
	}
	waits := sl.all()
	if len(waits) != 4 {
		t.Fatalf("waits = %v, want 4", waits)
	}
	// After the hello is refused revoked the agent enrols at once (the wait is
	// whatever the rejoin needs); every wait after a blocked refusal is the maximum.
	for i, d := range waits[1:] {
		if d != 60*time.Second {
			t.Errorf("wait %d = %v, want the 60s maximum after a blocked refusal", i+2, d)
		}
	}
	if n := srv.attempts(); n != 4 {
		t.Errorf("the agent made %d attempts for 4 waits; it must try once per wait, never faster", n)
	}
	if a, _ := srv.authN(t, 1); a.Mode != wire.ModeHello {
		t.Errorf("attempt 1 used mode %q, want hello", a.Mode)
	}
	for n := 2; n <= 4; n++ {
		if a, _ := srv.authN(t, n); a.Mode != wire.ModeEnrol || a.Token != "tpjt_tok" {
			t.Errorf("attempt %d used mode %q, want enrol with the token", n, a.Mode)
		}
	}
	if n := strings.Count(logs.String(), "code=blocked"); n != 1 {
		t.Errorf("the code blocked was logged %d times, want once:\n%s", n, logs.String())
	}
	if !noIdentityOnDisk(dir) {
		t.Error("the revoked identity is still on disk")
	}
}

// An agent that was just re-enrolled and is revoked again at once does not enrol a
// third agent: it waits the maximum first, so a host cannot spin up a new agent
// row every second.
func TestAnAgentRevokedRightAfterRejoiningWaitsBeforeEnrollingAgain(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(n int, a wire.Auth) reply {
		if a.Mode == wire.ModeEnrol {
			return reply{cert: []byte("second-cert-der"), drop: true}
		}
		return reply{refuse: wire.ReasonRevoked}
	})
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	enrols := 0
	for n := 1; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode == wire.ModeEnrol {
			enrols++
		}
	}
	if enrols != 1 {
		t.Errorf("the agent enrolled %d times in %v of waiting, want once", enrols, sl.all())
	}
	got, ok, err := state.Load(dir)
	if err != nil || !ok || string(got.Cert) != "second-cert-der" {
		t.Errorf("the identity on disk = %+v %v %v, want the one the re-enrolment saved", got, ok, err)
	}
}

func TestAnExpiredCertificateEnrolsAgainWhenAJoinTokenIsAvailable(t *testing.T) {
	dir := t.TempDir()
	old := enrolledState(t, dir)
	srv := newFakeServer(t, func(n int, a wire.Auth) reply {
		if a.Mode == wire.ModeHello {
			return reply{refuse: wire.ReasonCertExpired}
		}
		return reply{cert: []byte("renewed-cert-der")}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

	first, _ := srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if first.Mode != wire.ModeHello || second.Mode != wire.ModeEnrol || second.Token != "tpjt_tok" {
		t.Fatalf("modes = %q then %q (token %q); want hello, then enrol with the token", first.Mode, second.Mode, second.Token)
	}
	srv.heartbeatsAfterWelcome(t, 1)
	if s, ok, _ := state.Load(dir); !ok || string(s.Cert) != "renewed-cert-der" {
		t.Fatalf("the new certificate was not saved: %+v %v", s, ok)
	}
	got, _, _ := state.Load(dir)
	if got.Key.Equal(old.Key) {
		t.Error("re-enrolment reused the old key; the join token enrols a fresh one")
	}
}

func TestAnExpiredCertificateWithNoJoinTokenWaitsTheMaximumAndKeepsTheIdentity(t *testing.T) {
	dir := t.TempDir()
	saved := enrolledState(t, dir)
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonCertExpired} })
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range sl.all() {
		if d != 60*time.Second {
			t.Errorf("wait = %v, want the maximum", d)
		}
	}
	for n := 1; n <= srv.attempts(); n++ {
		if a, _ := srv.authN(t, n); a.Mode != wire.ModeHello {
			t.Errorf("attempt %d used mode %q; without a token the agent can only say hello", n, a.Mode)
		}
	}
	if got, ok, _ := state.Load(dir); !ok || !got.Key.Equal(saved.Key) {
		t.Error("the identity was removed")
	}
}

func TestOtherFailuresBackOffExponentiallyAndAWelcomeResetsTheCount(t *testing.T) {
	dir := t.TempDir()
	enrolledState(t, dir)
	srv := newFakeServer(t, func(n int, _ wire.Auth) reply {
		switch n {
		case 1, 2, 3:
			return reply{refuse: wire.ReasonAuthRejected}
		case 4:
			return reply{drop: true}
		default:
			return reply{refuse: wire.ReasonAuthRejected}
		}
	})
	sl, err := runUntilSleeps(t, Config{
		URL: srv.url(), StateDir: dir, SocketPath: filepath.Join(dir, "none.sock"),
		BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	w := sl.all()
	// rnd is fixed at 0.5, so the wait is three quarters of the step: 0.75, 1.5, 3, then
	// a welcome resets the count, so the next wait is 0.75 again (the drop after the
	// welcome counts as the first failure) and the one after doubles.
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 750 * time.Millisecond, 1500 * time.Millisecond}
	if len(w) != len(want) {
		t.Fatalf("waits = %v, want %v", w, want)
	}
	for i := range want {
		if w[i] != want[i] {
			t.Errorf("wait %d = %v, want %v (all: %v)", i+1, w[i], want[i], w)
		}
	}
}

func TestBackoffStartsAtAboutASecondAndNeverExceedsTheMaximum(t *testing.T) {
	lo, hi := time.Second, 60*time.Second
	if d := backoff(0, lo, hi, func() float64 { return 0 }); d != 500*time.Millisecond {
		t.Errorf("backoff(0) with no jitter = %v, want half of the first step", d)
	}
	if d := backoff(0, lo, hi, func() float64 { return 0.999999 }); d <= 900*time.Millisecond || d > time.Second {
		t.Errorf("backoff(0) at the top of the jitter = %v, want about 1s", d)
	}
	for n := range 200 {
		for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
			if d := backoff(n, lo, hi, func() float64 { return r }); d > hi || d <= 0 {
				t.Fatalf("backoff(%d) = %v with jitter %v, outside (0, %v]", n, d, r, hi)
			}
		}
	}
}

func TestBackoffDoublesPerFailureAndIsJitteredByAtMostHalfAStep(t *testing.T) {
	lo, hi := time.Second, 60*time.Second
	steps := map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 32 * time.Second, 7: 60 * time.Second, 30: 60 * time.Second}
	for n, step := range steps {
		low := backoff(n, lo, hi, func() float64 { return 0 })
		high := backoff(n, lo, hi, func() float64 { return 0.999999999 })
		if low != step/2 {
			t.Errorf("backoff(%d) at no jitter = %v, want %v", n, low, step/2)
		}
		if high > step || high < step-step/100 {
			t.Errorf("backoff(%d) at full jitter = %v, want just under %v", n, high, step)
		}
	}
	// A different minimum scales the whole ladder.
	if d := backoff(1, 250*time.Millisecond, 5*time.Second, func() float64 { return 0 }); d != 125*time.Millisecond {
		t.Errorf("backoff with a 250ms minimum = %v, want 125ms", d)
	}
	if d := backoff(9, 250*time.Millisecond, 5*time.Second, func() float64 { return 0.999999999 }); d > 5*time.Second {
		t.Errorf("backoff above a 5s maximum: %v", d)
	}
}

func TestBackoffUsesRealRandomnessByDefault(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 20 {
		seen[backoff(5, time.Second, time.Minute, nil)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("20 delays with the default jitter were all equal: %v", seen)
	}
}

func TestARefusalOfTheVeryFirstEnrolmentEndsTheAgentSoAWrongTokenIsVisible(t *testing.T) {
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: wire.ReasonAuthRejected} })
	ctx := testsupport.Context(t)
	err := runWith(ctx, Config{
		URL: srv.url(), JoinToken: "tpjt_wrong", SocketPath: filepath.Join(t.TempDir(), "none.sock"), Log: slog.New(slog.DiscardHandler),
	}, fastEnv())
	var re *RefusedError
	if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected {
		t.Fatalf("Run = %v, want the auth_rejected refusal; a token that is wrong stays wrong", err)
	}
	if srv.attempts() != 1 {
		t.Errorf("the agent tried %d times", srv.attempts())
	}
}

func TestATransientRefusalOfTheFirstEnrolmentIsRetried(t *testing.T) {
	for _, code := range []string{wire.RefuseRateLimited, wire.RefuseInternal} {
		srv := newFakeServer(t, func(n int, _ wire.Auth) reply {
			if n < 3 {
				return reply{refuse: code}
			}
			return reply{}
		})
		startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok"}, fastEnv())
		if a, _ := srv.authN(t, 3); a.Mode != wire.ModeEnrol {
			t.Errorf("%s: third attempt used mode %q", code, a.Mode)
		}
	}
}

// A server that is busy answers with a refusal in place of the challenge. The agent
// reads it as the refusal it is, and waits at least as long as the server asked.
func TestARefusalInPlaceOfTheChallengeIsReadAndItsRetryAfterIsHonoured(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
		if err != nil {
			return
		}
		mu.Lock()
		attempts++
		mu.Unlock()
		b, _ := json.Marshal(wire.Refuse{T: wire.TypeRefuse, Code: wire.RefuseRateLimited, RetryAfterMs: 90_000})
		_ = c.Write(r.Context(), websocket.MessageText, b)
		_ = c.Close(websocket.StatusPolicyViolation, wire.RefuseRateLimited)
	}))
	defer ts.Close()
	sl, err := runUntilSleeps(t, Config{
		URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/agent/connect", JoinToken: "tpjt_tok",
		SocketPath: filepath.Join(t.TempDir(), "none.sock"), BackoffMin: time.Second, BackoffMax: 60 * time.Second,
	}, 2)
	if err != nil {
		t.Fatalf("Run = %v; a rate-limited first enrolment is retried", err)
	}
	for i, d := range sl.all() {
		if d < 90*time.Second {
			t.Errorf("wait %d = %v, want at least the 90s the server asked for", i+1, d)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Errorf("the agent tried %d times, want it to retry", attempts)
	}
}

// The server measures the agent's clock from the ts of the auth message, so it is
// the agent's own wall clock and nothing adjusted.
func TestTheHandshakeCarriesTheAgentsOwnClockAndTheExporterOfItsDial(t *testing.T) {
	srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
	fixed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	env := fastEnv()
	env.now = func() time.Time { return fixed }
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok"}, env)
	a, _ := srv.authN(t, 1)
	if a.TS != fixed.UnixMilli() {
		t.Errorf("ts = %d, want the agent's clock %d", a.TS, fixed.UnixMilli())
	}
	if len(a.EKM) != 0 {
		t.Errorf("a plain ws:// dial carried an exporter value: %x", a.EKM)
	}
}

func TestTheHandshakeReportsTheHostPolicyInTheFacts(t *testing.T) {
	for name, tc := range map[string]struct{ readOnly, selfUpdate bool }{
		"read-only, no self update": {true, false},
		"read-write, self update":   {false, true},
	} {
		srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
		startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", ReadOnly: tc.readOnly, SelfUpdate: tc.selfUpdate}, fastEnv())
		a, _ := srv.authN(t, 1)
		if a.Facts.Policy.ReadOnly != tc.readOnly || a.Facts.Policy.SelfUpdate != tc.selfUpdate {
			t.Errorf("%s: facts.policy = %+v", name, a.Facts.Policy)
		}
	}
}

func TestTheHandshakeReportsThePrivilegedServicesOptIn(t *testing.T) {
	for name, allow := range map[string]bool{"opted in": true, "not opted in": false} {
		srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
		startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", AllowPrivilegedServices: allow}, fastEnv())
		a, _ := srv.authN(t, 1)
		if a.Facts.Policy.PrivilegedServices != allow {
			t.Errorf("%s: facts.policy = %+v", name, a.Facts.Policy)
		}
	}
}

func TestHandshakeReportsHostNet(t *testing.T) {
	for name, tc := range map[string]struct{ path, want string }{
		"the route file reads":      {hostRouteFixture(t), wire.HostNetOK},
		"the route file is missing": {filepath.Join(t.TempDir(), "absent"), wire.HostNetUnmounted},
		"the route file is a dir":   {t.TempDir(), wire.HostNetUnmounted},
	} {
		srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{} })
		startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", HostRouteFile: tc.path}, fastEnv())
		a, _ := srv.authN(t, 1)
		if a.Facts.HostNet != tc.want {
			t.Errorf("%s: facts.hostNet = %q, want %q", name, a.Facts.HostNet, tc.want)
		}
	}
}

func TestARefusedDockerRequestIsReportedOnTheControlStream(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })
	for name, tc := range map[string]struct {
		readOnly bool
		raw      string
		want     wire.Denied
	}{
		"a write on a read-only agent": {
			readOnly: true, raw: "POST /containers/create HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}",
			want: wire.Denied{T: wire.TypeDenied, Method: "POST", Path: "/containers/create", Reason: wire.DeniedReadOnly},
		},
		"a read the allowlist does not name": {
			raw:  "GET /swarm HTTP/1.1\r\nHost: docker\r\n\r\n",
			want: wire.Denied{T: wire.TypeDenied, Method: "GET", Path: "/swarm", Reason: wire.DeniedAllowlist},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{dockerRequests: []string{tc.raw}} })
			startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", SocketPath: sock, ReadOnly: tc.readOnly}, fastEnv())
			if got := testsupport.Await(t, "a denied message", srv.denied); got != tc.want {
				t.Fatalf("denied = %+v, want %+v", got, tc.want)
			}
			// Two heartbeats later the control loop has had every chance to write a
			// second report of the same refusal.
			srv.heartbeatsAfterWelcome(t, 2)
			select {
			case extra := <-srv.denied:
				t.Fatalf("a second denied message for one refusal: %+v", extra)
			default:
			}
		})
	}
}

func TestAnAllowedRequestSendsNoDeniedMessage(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })
	srv := newFakeServer(t, func(int, wire.Auth) reply {
		return reply{dockerRequests: []string{"GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"}}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "tpjt_tok", SocketPath: sock, ReadOnly: true}, fastEnv())
	testsupport.Await(t, "the agent's answer to the Docker request", srv.answered)
	srv.heartbeatsAfterWelcome(t, 2)
	select {
	case d := <-srv.denied:
		t.Fatalf("a read on a read-only agent was reported as denied: %+v", d)
	default:
	}
}

// The agent reports refusals without ever blocking the request: a full buffer
// drops the message and counts it.
func TestDenialsBufferSixtyFourAndCountTheDrops(t *testing.T) {
	d := newDenials()
	for range 100 {
		d.report("GET", "/x", wire.DeniedAllowlist)
	}
	if got := len(d.ch); got != 64 {
		t.Fatalf("buffered %d, want 64", got)
	}
	if got := d.dropped.Load(); got != 36 {
		t.Fatalf("dropped %d, want 36", got)
	}
	m := <-d.ch
	if m.T != wire.TypeDenied || m.Method != "GET" || m.Path != "/x" || m.Reason != wire.DeniedAllowlist {
		t.Fatalf("message = %+v", m)
	}
}

func TestDenialsCapTheMethodAndThePathTheServerSent(t *testing.T) {
	d := newDenials()
	d.report(strings.Repeat("M", 100), "/"+strings.Repeat("p", 1000), wire.DeniedAllowlist)
	m := <-d.ch
	if len(m.Method) > 16 || len(m.Path) > 256 {
		t.Fatalf("method %d bytes, path %d bytes; want at most 16 and 256", len(m.Method), len(m.Path))
	}
}

// hostRouteFixture copies the shared route-table fixture to a temp file, as the
// bind mount would present it.
func hostRouteFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "proto", "hostroute", "testdata", "route-host.txt"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "route")
	if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // a temp path
		t.Fatal(err)
	}
	return p
}

func TestServeStreamHostNet(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the fixture is a little-endian host's /proc/net/route")
	}
	route := hostRouteFixture(t)
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveStream(srv, http.NotFoundHandler(), route, slog.New(slog.DiscardHandler))
	}()
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := cli.Write([]byte{wire.StreamKindHostNet}); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(cli) // returns at close: the stream ends after the document
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	<-done
	var doc wire.HostNetDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if dec.More() {
		t.Fatalf("more than one document: %q", raw)
	}
	want := []wire.HostRoute{
		{Iface: "eth0", Dst: "192.168.2.0/24"},
		{Iface: "docker0", Dst: "172.17.0.0/16"},
		{Iface: "wg0", Dst: "10.8.0.0/24"},
	}
	if doc.V != 1 || doc.Error != "" || doc.Truncated || !slices.Equal(doc.Routes, want) {
		t.Fatalf("doc = %+v, want routes %v", doc, want)
	}
}

func TestServeStreamHostNetAnswersUnreadableWhenTheFileIsGone(t *testing.T) {
	srv, cli := net.Pipe()
	go serveStream(srv, http.NotFoundHandler(), filepath.Join(t.TempDir(), "absent"), slog.New(slog.DiscardHandler))
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = cli.Write([]byte{wire.StreamKindHostNet})
	var doc wire.HostNetDoc
	if err := json.NewDecoder(cli).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Error != wire.HostNetUnreadable || len(doc.Routes) != 0 {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestServeStreamUnknownKindStillClosed(t *testing.T) {
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveStream(srv, http.NotFoundHandler(), "/nonexistent", slog.New(slog.DiscardHandler))
	}()
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := cli.Write([]byte{0x07}); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(cli)
	if err != nil || len(b) != 0 {
		t.Fatalf("an unknown kind answered %q (%v), want a close with nothing", b, err)
	}
	<-done
}

func TestServeStreamDockerHTTPStillGoesToTheProxy(t *testing.T) {
	srv, cli := net.Pipe()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("proxied")) })
	go serveStream(srv, h, "/nonexistent", slog.New(slog.DiscardHandler))
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := cli.Write([]byte{wire.StreamKindDockerHTTP}); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write([]byte("GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(cli), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "proxied" {
		t.Fatalf("body = %q", body)
	}
}
