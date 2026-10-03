// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/agent/internal/state"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

func fakeEngine(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpag")
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

func TestReadFactsDescribesAStandaloneEngine(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			_, _ = w.Write([]byte(`{"ID":"ENG:1","Name":"node-1","ServerVersion":"29.1.0","Swarm":{"LocalNodeState":"inactive"}}`))
		case "/version":
			_, _ = w.Write([]byte(`{"Version":"29.1.0","ApiVersion":"1.52"}`))
		default:
			http.NotFound(w, r)
		}
	})
	f := readFacts(context.Background(), sock, slog.New(slog.DiscardHandler))
	if !f.EngineReachable || f.EngineID != "ENG:1" || f.Hostname != "node-1" || f.EngineVersion != "29.1.0" || f.APIVersion != "1.52" {
		t.Fatalf("facts = %+v", f)
	}
	if f.Swarm != nil {
		t.Fatalf("an engine outside a swarm reported swarm facts: %+v", f.Swarm)
	}
}

func TestReadFactsDescribesASwarmManager(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			_, _ = w.Write([]byte(`{"ID":"E","Name":"m1","ServerVersion":"29.1.0","Swarm":{"LocalNodeState":"active","NodeID":"n1","ControlAvailable":true,"Cluster":{"ID":"c1"},"RemoteManagers":[{"NodeID":"n2"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"Version":"29.1.0","ApiVersion":"1.52"}`))
	})
	f := readFacts(context.Background(), sock, slog.New(slog.DiscardHandler))
	if f.Swarm == nil || f.Swarm.State != "active" || f.Swarm.NodeID != "n1" || !f.Swarm.Control || f.Swarm.ClusterID != "c1" ||
		len(f.Swarm.ManagerIDs) != 1 || f.Swarm.ManagerIDs[0] != "n2" {
		t.Fatalf("swarm facts = %+v", f.Swarm)
	}
}

func TestReadFactsReportsAnEngineThatDoesNotAnswerAsUnreachable(t *testing.T) {
	f := readFacts(context.Background(), filepath.Join(t.TempDir(), "none.sock"), slog.New(slog.DiscardHandler))
	if f.EngineReachable {
		t.Fatalf("facts = %+v, want the engine unreachable", f)
	}
	sock := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if f := readFacts(context.Background(), sock, slog.New(slog.DiscardHandler)); f.EngineReachable {
		t.Fatalf("an engine answering 500 was reported reachable: %+v", f)
	}
}

func TestRunRefusesAnAddressThatIsNotAWebSocketURL(t *testing.T) {
	for _, bad := range []string{"", "https://bayplan.example/agent/connect", "not a url", "wss://"} {
		err := Run(context.Background(), Config{URL: bad, JoinToken: "x"})
		if err == nil || strings.Contains(err.Error(), "bpjt") {
			t.Errorf("Run(%q) = %v, want an error about the address", bad, err)
		}
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
	refuse      string // non-empty: refuse with this code
	cert        []byte // the certificate in an enrol welcome
	heartbeatMs int    // zero: 40 ms, so a test sees heartbeats quickly
	drop        bool   // close the connection right after the welcome
	toAgent     []string
	// dockerRequests are raw HTTP requests the server sends on streams it opens
	// after the welcome, the way its Docker client would.
	dockerRequests []string
}

// fakeServer is the server end of the agent's connection: it runs the real
// handshake messages and the control stream, and records what the agent sent.
type fakeServer struct {
	ts     *httptest.Server
	script func(n int, a wire.Auth) reply

	mu      sync.Mutex
	auths   []wire.Auth
	nonces  [][]byte
	kinds   []byte
	beats   chan wire.Heartbeat
	denied  chan wire.Denied
	started chan int // connection number, after the welcome
}

func newFakeServer(t *testing.T, script func(n int, a wire.Auth) reply) *fakeServer {
	t.Helper()
	f := &fakeServer{script: script, beats: make(chan wire.Heartbeat, 256), denied: make(chan wire.Denied, 256), started: make(chan int, 64)}
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
	b, _ = json.Marshal(wire.Welcome{T: wire.TypeWelcome, AgentID: "agent-7", EnvID: "env-7", Cert: rep.cert, HeartbeatMs: hb, ServerTime: time.Now().UnixMilli()})
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
	for _, raw := range rep.dockerRequests {
		go func() {
			ds, err := ym.OpenStream()
			if err != nil {
				return
			}
			defer func() { _ = ds.Close() }()
			_, _ = ds.Write(append([]byte{wire.StreamKindDockerHTTP}, raw...))
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
		var d wire.Denied
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.T == wire.TypeDenied {
			select {
			case f.denied <- d:
			default:
			}
		}
	}
}

func (f *fakeServer) authN(t *testing.T, n int) (wire.Auth, []byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		if len(f.auths) >= n {
			a, nonce := f.auths[n-1], f.nonces[n-1]
			f.mu.Unlock()
			return a, nonce
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("the server never saw handshake %d", n)
		}
		time.Sleep(5 * time.Millisecond)
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
		case <-time.After(5 * time.Second):
			t.Error("the agent did not stop when its context ended")
		}
	})
	return cancel, ch
}

func fastEnv() runEnv {
	return runEnv{
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(min(d, 5*time.Millisecond)):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

	a, _ := srv.authN(t, 1)
	if a.Mode != wire.ModeEnrol || a.Token != "bpjt_tok" || len(a.Pub) != ed25519.PublicKeySize || len(a.Cert) != 0 {
		t.Fatalf("first handshake = mode %q token %q pub %d cert %d; want enrol with the token and a key", a.Mode, a.Token, len(a.Pub), len(a.Cert))
	}
	waitUntil(t, "the identity to be saved", func() bool { _, ok, _ := state.Load(dir); return ok })
	got, _, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "agent-7" || got.EnvID != "env-7" || string(got.Cert) != "issued-cert-der" || !bytes.Equal(publicOf(got.Key), a.Pub) {
		t.Fatalf("saved state = %+v; want the welcome's IDs and certificate and the key the handshake proved", got)
	}
	if _, err := os.Stat(filepath.Join(dir, state.ConnectedFile)); err != nil {
		t.Errorf("no connected marker after the handshake: %v", err)
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
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

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
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok"}, fastEnv())
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
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", StateDir: dir, SocketPath: sock}, runEnv{
		now: func() time.Time { return time.UnixMilli(1_700_000_000_123) },
	})
	var got []wire.Heartbeat
	for len(got) < 3 {
		select {
		case h := <-srv.beats:
			got = append(got, h)
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d heartbeats arrived", len(got))
		}
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
		startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", SocketPath: sock}, runEnv{})
		select {
		case h := <-srv.beats:
			if h.EngineReachable {
				t.Errorf("%s: heartbeat says the engine is reachable", name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no heartbeat", name)
		}
	}
}

func TestTheAgentIgnoresControlMessagesItDoesNotKnow(t *testing.T) {
	srv := newFakeServer(t, func(int, wire.Auth) reply {
		return reply{heartbeatMs: 30, toAgent: []string{`{"t":"from-the-future","x":1}`, `not json at all`, `{"t":"heartbeat"}`}}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok"}, runEnv{})
	for range 3 {
		select {
		case <-srv.beats:
		case <-time.After(3 * time.Second):
			t.Fatal("the agent stopped heartbeating after an unknown control message")
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sl := &sleeps{stopAt: stopAt}
	ctx, sl.cancel = context.WithCancel(ctx)
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	err := runWith(ctx, cfg, runEnv{sleep: sl.sleep, rnd: func() float64 { return 0.5 }})
	return sl, err
}

func TestARevokedOrBlockedAgentWaitsTheMaximumLogsTheCodeOnceAndKeepsItsIdentity(t *testing.T) {
	for _, code := range []string{wire.ReasonRevoked, wire.ReasonBlocked} {
		t.Run(code, func(t *testing.T) {
			dir := t.TempDir()
			saved := enrolledState(t, dir)
			srv := newFakeServer(t, func(int, wire.Auth) reply { return reply{refuse: code} })
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			sl, err := runUntilSleeps(t, Config{
				URL: srv.url(), StateDir: dir, Log: log, SocketPath: filepath.Join(dir, "none.sock"),
				BackoffMin: time.Second, BackoffMax: 60 * time.Second,
			}, 3)
			if err != nil {
				t.Fatalf("Run = %v; a refusal never ends the agent", err)
			}
			for i, d := range sl.all() {
				if d != 60*time.Second {
					t.Errorf("wait %d = %v, want the 60s maximum", i+1, d)
				}
			}
			if n := strings.Count(logs.String(), "code="+code); n != 1 {
				t.Errorf("the code %s was logged %d times, want once:\n%s", code, n, logs.String())
			}
			got, ok, err := state.Load(dir)
			if err != nil || !ok || !got.Key.Equal(saved.Key) || string(got.Cert) != "saved-cert-der" {
				t.Fatalf("the identity changed or went away: %+v %v %v", got, ok, err)
			}
			if srv.attempts() < 3 {
				t.Errorf("the agent tried %d times, want it to keep retrying", srv.attempts())
			}
		})
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
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", StateDir: dir, SocketPath: filepath.Join(dir, "none.sock")}, fastEnv())

	first, _ := srv.authN(t, 1)
	second, _ := srv.authN(t, 2)
	if first.Mode != wire.ModeHello || second.Mode != wire.ModeEnrol || second.Token != "bpjt_tok" {
		t.Fatalf("modes = %q then %q (token %q); want hello, then enrol with the token", first.Mode, second.Mode, second.Token)
	}
	waitUntil(t, "the new certificate to be saved", func() bool {
		s, ok, _ := state.Load(dir)
		return ok && string(s.Cert) == "renewed-cert-der"
	})
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runWith(ctx, Config{
		URL: srv.url(), JoinToken: "bpjt_wrong", SocketPath: filepath.Join(t.TempDir(), "none.sock"), Log: slog.New(slog.DiscardHandler),
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
		startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok"}, fastEnv())
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
		URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/agent/connect", JoinToken: "bpjt_tok",
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
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok"}, env)
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
		startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", ReadOnly: tc.readOnly, SelfUpdate: tc.selfUpdate}, fastEnv())
		a, _ := srv.authN(t, 1)
		if a.Facts.Policy.ReadOnly != tc.readOnly || a.Facts.Policy.SelfUpdate != tc.selfUpdate {
			t.Errorf("%s: facts.policy = %+v", name, a.Facts.Policy)
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
			startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", SocketPath: sock, ReadOnly: tc.readOnly}, fastEnv())
			select {
			case got := <-srv.denied:
				if got != tc.want {
					t.Fatalf("denied = %+v, want %+v", got, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no denied message reached the server")
			}
			select {
			case extra := <-srv.denied:
				t.Fatalf("a second denied message for one refusal: %+v", extra)
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

func TestAnAllowedRequestSendsNoDeniedMessage(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })
	srv := newFakeServer(t, func(int, wire.Auth) reply {
		return reply{dockerRequests: []string{"GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"}}
	})
	startAgent(t, Config{URL: srv.url(), JoinToken: "bpjt_tok", SocketPath: sock, ReadOnly: true}, fastEnv())
	select {
	case d := <-srv.denied:
		t.Fatalf("a read on a read-only agent was reported as denied: %+v", d)
	case <-time.After(300 * time.Millisecond):
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
