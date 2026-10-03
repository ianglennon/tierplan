// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package tunneltest is the server end of the agent tunnel, in process, for
// tests: a real tunnel.Hub behind a TLS test server, a real migrated store, the
// real agents.Service and a recording Observer, plus a scriptable agent that
// performs enrolment and hello with a knob for everything a handshake can get
// wrong. Tests of the tunnel, of revocation and of identity checks all use it, so
// they exercise the same code that production runs.
package tunneltest

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Clock is a clock a test can move. It is the server's clock: certificates are
// issued and verified against it, and the hub's timers read it.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock set to t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now is the current time of the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Options configure Start. The zero value is a working server.
type Options struct {
	// Clock is the server's clock. Nil means a clock that starts at the real time
	// and is moved only by the test.
	Clock *Clock
	// Wrap lets a test put middleware around the hub's handler.
	Wrap func(http.Handler) http.Handler
	// Configure runs on the hub before it serves, after the defaults a test
	// server gets (the agents service records last-seen), so it may override them.
	Configure func(*tunnel.Hub) *tunnel.Hub
	// Server replaces the http.Server the hub is served by, for a test of how the
	// real server settings treat a tunnel. It is given the hub's handler.
	Server func(http.Handler) *http.Server
	// AuditRefusals records every refused handshake as agent.enrol_refused with the
	// real audit writer, unthrottled, so a test can read the reason from the
	// database. Off by default, so tests that count audit rows are unaffected.
	AuditRefusals bool
	// Store is an already migrated store to use instead of a new one, for a test
	// that runs other services over the same database (the environment registry
	// reads the agent rows the agents service writes). Its data directory is the
	// one the master key is kept in.
	Store *store.Store
	// Observer is told about every session after the hub's own recorder has been
	// told, in the same order. Events still records them, so a test can wait on
	// Events.WaitLen to know the Observer has run.
	Observer tunnel.Observer
	// Notifier is told what the hub's Notifier is told: connections, disconnections
	// and refused handshakes.
	Notifier tunnel.Notifier
	// Verifier wraps the real agents service as the hub's Verifier, so a test can
	// hold a handshake between its verification and the registration of the session.
	// The wrapper must pass StillActive through.
	Verifier func(real tunnel.Verifier) tunnel.Verifier
}

// Server is a running in-process Bayplan tunnel endpoint.
type Server struct {
	// Hub is the real hub. HTTP is the TLS test server in front of it.
	Hub  *tunnel.Hub
	HTTP *httptest.Server
	// Store is the real, migrated store the agents service uses.
	Store *store.Store
	// Svc is the real agents service, the hub's Verifier.
	Svc *agents.Service
	// Events records the session lifecycle the hub reports.
	Events *Recorder
	// Clock is the server's clock.
	Clock *Clock
	// Vault is the master key holder the service seals CA keys with. Its key file
	// is in DataDir.
	Vault *vault.Vault
	// DataDir is the directory that holds the database, its WAL and the key file.
	DataDir string
}

// Recorder is an Observer that records every lifecycle call in order.
type Recorder struct {
	mu       sync.Mutex
	events   []string
	sessions []*tunnel.Session
	changed  chan struct{}
	// next is the Observer the recorder forwards to, or nil.
	next tunnel.Observer
}

func newRecorder(next tunnel.Observer) *Recorder {
	return &Recorder{changed: make(chan struct{}, 1), next: next}
}

func (r *Recorder) note() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// Connected implements tunnel.Observer.
func (r *Recorder) Connected(s *tunnel.Session) {
	// The Observer runs first: a test that waits for the log entry then knows the
	// Observer has finished.
	if r.next != nil {
		r.next.Connected(s)
	}
	r.mu.Lock()
	r.events = append(r.events, "connected:"+s.AgentID)
	r.sessions = append(r.sessions, s)
	r.mu.Unlock()
	r.note()
}

// Disconnected implements tunnel.Observer.
func (r *Recorder) Disconnected(s *tunnel.Session, code string) {
	if r.next != nil {
		r.next.Disconnected(s, code)
	}
	r.mu.Lock()
	r.events = append(r.events, "disconnected:"+s.AgentID+":"+code)
	r.mu.Unlock()
	r.note()
}

// Log is the lifecycle so far, as "connected:<agent>" and
// "disconnected:<agent>:<code>".
func (r *Recorder) Log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// Sessions are the sessions reported connected, oldest first.
func (r *Recorder) Sessions() []*tunnel.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*tunnel.Session(nil), r.sessions...)
}

// WaitLen waits until the log has at least n entries and returns it.
func (r *Recorder) WaitLen(t testing.TB, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l := r.Log(); len(l) >= n {
			return l
		}
		select {
		case <-r.changed:
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunneltest: timed out waiting for %d lifecycle events, have %v", n, r.Log())
		}
	}
}

// Start serves a hub over TLS with a real store and agents service. Everything is
// shut down when the test ends.
func Start(t *testing.T, opts Options) *Server {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	clock := opts.Clock
	if clock == nil {
		clock = NewClock(time.Now())
	}
	st := opts.Store
	if st == nil {
		st = storetest.New(t)
	}
	// The master key sits in the data directory next to the database, as it does in
	// production, so a test can search the whole directory for secrets and exempt
	// only the key file.
	dataDir := filepath.Dir(st.Path)
	vlt, err := vault.LoadOrCreate("", dataDir, log)
	if err != nil {
		t.Fatalf("tunneltest: vault: %v", err)
	}
	aud := audit.NewWriter(st, vlt, log, clock.Now)
	svc := agents.NewService(st, vlt, aud, clock.Now)
	rec := newRecorder(opts.Observer)

	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	var verifier tunnel.Verifier = svc
	if opts.Verifier != nil {
		verifier = opts.Verifier(svc)
	}
	hub := tunnel.NewHub(verifier, rec, log, clock.Now).WithPublicURL(func() string { return "https://" + addr }).WithLastSeen(svc)
	if opts.Notifier != nil {
		hub = hub.WithNotifier(opts.Notifier)
	}
	if opts.AuditRefusals {
		hub = hub.WithRefusalAudit(aud)
	}
	if opts.Configure != nil {
		hub = opts.Configure(hub)
	}
	// A revoke closes the agent's live session through the hub, as in production.
	svc.WithKicker(hub)
	h := hub.Handler()
	if opts.Wrap != nil {
		h = opts.Wrap(h)
	}
	if opts.Server != nil {
		ts.Config = opts.Server(h)
	} else {
		ts.Config.Handler = h
	}
	ts.StartTLS()
	t.Cleanup(func() {
		hub.Close()
		ts.CloseClientConnections()
		ts.Close()
	})
	return &Server{Hub: hub, HTTP: ts, Store: st, Svc: svc, Events: rec, Clock: clock, Vault: vlt, DataDir: dataDir}
}

// URL is the address an agent dials.
func (s *Server) URL() string {
	return "ws" + strings.TrimPrefix(s.HTTP.URL, "http") + "/agent/connect"
}

// Audience is the host:port the agent must address its handshake to.
func (s *Server) Audience() string { return s.HTTP.Listener.Addr().String() }

// Environment is an agent environment with its join token.
type Environment struct {
	ID, Token string
}

// NewEnvironment creates an agent environment and its first join token.
func (s *Server) NewEnvironment(t testing.TB, name string) Environment {
	t.Helper()
	c, err := s.Svc.CreateEnvironment(context.Background(), audit.SystemCLI, name, "203.0.113.9", "tunneltest")
	if err != nil {
		t.Fatalf("tunneltest: create environment: %v", err)
	}
	return Environment{ID: c.EnvID, Token: c.JoinToken}
}

// Exec runs a statement on the store's writer, to put a test in a state the
// service would not reach on its own (a revoked agent, a changed key).
func (s *Server) Exec(t testing.TB, query string, args ...any) {
	t.Helper()
	if _, err := s.Store.Write.Exec(query, args...); err != nil {
		t.Fatalf("tunneltest: %s: %v", query, err)
	}
}

// Identity is what an enrolled agent holds: the IDs, the key and the certificate.
type Identity struct {
	AgentID, EnvID string
	Key            ed25519.PrivateKey
	Cert           []byte
}

// Public is the public key of the identity's private key.
func (i Identity) Public() ed25519.PublicKey { return ed25519.PublicKey(i.Key[ed25519.SeedSize:]) }

// Agent describes one handshake the test agent makes. The zero value enrols with
// Token. Every field is a way to make the handshake wrong.
type Agent struct {
	// Mode is wire.ModeEnrol or wire.ModeHello. Empty means enrol, or hello when
	// Identity is set.
	Mode string
	// Token is the join token to enrol with.
	Token string
	// Identity is the identity to say hello with.
	Identity *Identity
	// Aud is the audience sent and signed. Empty means the server's address.
	Aud string
	// SignedAud, when set, is signed instead of Aud, so the signature is for
	// another audience than the one sent.
	SignedAud string
	// SigningKey signs the transcript instead of the identity's key.
	SigningKey ed25519.PrivateKey
	// Cert is presented instead of the identity's certificate.
	Cert []byte
	// Proto is the protocol version sent. Zero means wire.ProtoVersion.
	Proto int
	// Facts are the facts reported. Nil means a reachable standalone engine whose ID
	// is the environment's: see defaultHost.
	Facts *wire.Facts
	// NoMux leaves the WebSocket unwrapped after the welcome, so a test can read
	// the server's close frame itself.
	NoMux bool
	// Version is the agent version sent. Empty means "0.2.0".
	Version string
	// ClockSkew is how far the agent's clock is ahead of the server's, in the ts
	// it sends. Negative is behind.
	ClockSkew time.Duration
	// EKM replaces the TLS exporter value sent and signed. NoEKM sends none.
	EKM   []byte
	NoEKM bool
}

// Conn is a connection after the handshake.
type Conn struct {
	WS      *websocket.Conn
	Welcome *wire.Welcome
	Refuse  *wire.Refuse
	// Mux is the agent's end of the tunnel: the agent is the yamux server, and the
	// hub opens a stream for every Docker call.
	Mux *yamux.Session
	// Identity is what this connection enrolled with or said hello with.
	Identity Identity
	// Err is the error that ended the exchange when the server closed the
	// connection instead of answering.
	Err error

	ctx context.Context
}

// Dial runs the agent side of the handshake. It returns after the welcome or the
// refusal. A connection that welcomed has a Mux, and Identity carries the
// certificate the welcome issued when the agent enrolled.
func (s *Server) Dial(t testing.TB, a Agent) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	ws, resp, err := websocket.Dial(ctx, s.URL(), &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient: s.HTTP.Client(), Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		t.Fatalf("tunneltest: dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	c := &Conn{WS: ws, ctx: ctx}

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("tunneltest: read the challenge: %v", err)
	}
	var ch wire.Challenge
	if err := json.Unmarshal(data, &ch); err != nil || ch.T != wire.TypeChallenge {
		t.Fatalf("tunneltest: challenge = %s, %v", data, err)
	}

	mode := a.Mode
	if mode == "" {
		mode = wire.ModeEnrol
		if a.Identity != nil {
			mode = wire.ModeHello
		}
	}
	aud := a.Aud
	if aud == "" {
		aud = s.Audience()
	}
	signedAud := a.SignedAud
	if signedAud == "" {
		signedAud = aud
	}
	proto := a.Proto
	if proto == 0 {
		proto = wire.ProtoVersion
	}
	// A host that says nothing about itself is one standalone host per environment:
	// every default agent that enrols with the same token is that host again, which
	// is how a test enrols several agents into one environment, and two
	// environments never share one (an engine ID belongs to one environment).
	facts := wire.Facts{EngineID: "ENG:" + defaultHost(a), Hostname: "test-host", EngineReachable: true}
	if a.Facts != nil {
		facts = *a.Facts
	}

	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	ekm := authn.EKM(resp.TLS)
	switch {
	case a.NoEKM:
		ekm = nil
	case a.EKM != nil:
		ekm = a.EKM
	}
	version := a.Version
	if version == "" {
		version = "0.2.0"
	}
	auth := wire.Auth{
		T: wire.TypeAuth, Mode: mode, Proto: proto, AgentVersion: version, Aud: aud, Nonce: nonce,
		TS: s.Clock.Now().Add(a.ClockSkew).UnixMilli(), EKM: ekm, Facts: facts,
	}
	var signer ed25519.PrivateKey
	var signed []byte
	if mode == wire.ModeHello {
		id := a.Identity
		if id == nil {
			t.Fatal("tunneltest: hello needs an Identity")
		}
		c.Identity = *id
		cert := id.Cert
		if a.Cert != nil {
			cert = a.Cert
		}
		auth.AgentID, auth.Cert = id.AgentID, cert
		signer, signed = id.Key, cert
	} else {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		auth.Pub, auth.Token = pub, a.Token
		signer, signed = priv, pub
		c.Identity.Key = priv
	}
	if a.SigningKey != nil {
		signer = a.SigningKey
	}
	auth.Sig = authn.Sign(signer, authn.Transcript(mode, proto, ch.Nonce, nonce, signedAud, signed, ekm))
	b, _ := json.Marshal(auth)
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("tunneltest: send auth: %v", err)
	}

	_, data, err = ws.Read(ctx)
	if err != nil {
		c.Err = err
		return c
	}
	var head struct{ T string }
	_ = json.Unmarshal(data, &head)
	switch head.T {
	case wire.TypeWelcome:
		var w wire.Welcome
		_ = json.Unmarshal(data, &w)
		c.Welcome = &w
		c.Identity.AgentID, c.Identity.EnvID = w.AgentID, w.EnvID
		if mode == wire.ModeEnrol {
			c.Identity.Cert = w.Cert
		}
		if a.NoMux {
			break
		}
		cfg := yamux.DefaultConfig()
		cfg.LogOutput = io.Discard
		mux, err := yamux.Server(websocket.NetConn(ctx, ws, websocket.MessageBinary), cfg)
		if err != nil {
			t.Fatalf("tunneltest: start the multiplexer: %v", err)
		}
		t.Cleanup(func() { _ = mux.Close() })
		c.Mux = mux
	case wire.TypeRefuse:
		var r wire.Refuse
		_ = json.Unmarshal(data, &r)
		c.Refuse = &r
	default:
		t.Fatalf("tunneltest: unexpected answer %s", data)
	}
	return c
}

// Enrol enrols a new agent in env and returns its connection.
func (s *Server) Enrol(t testing.TB, env Environment) *Conn {
	t.Helper()
	c := s.Dial(t, Agent{Token: env.Token})
	if c.Welcome == nil {
		t.Fatalf("tunneltest: enrol was refused: %+v (err %v)", c.Refuse, c.Err)
	}
	return c
}

// Hello says hello with an identity.
func (s *Server) Hello(t testing.TB, id Identity) *Conn {
	t.Helper()
	return s.Dial(t, Agent{Identity: &id})
}

// OpenControl opens the control stream: a stream from the agent whose first byte
// is wire.StreamKindControl. Lines written to it are the agent's messages.
func (c *Conn) OpenControl(t testing.TB) io.ReadWriteCloser {
	t.Helper()
	if c.Mux == nil {
		t.Fatal("tunneltest: no multiplexer on this connection")
	}
	st, err := c.Mux.OpenStream()
	if err != nil {
		t.Fatalf("tunneltest: open the control stream: %v", err)
	}
	if _, err := st.Write([]byte{wire.StreamKindControl}); err != nil {
		t.Fatalf("tunneltest: write the control stream kind: %v", err)
	}
	return st
}

// SendHeartbeat writes one heartbeat line to the control stream.
func SendHeartbeat(t testing.TB, control io.Writer, engineReachable bool, clockMs int64) {
	t.Helper()
	b, _ := json.Marshal(wire.Heartbeat{T: wire.TypeHeartbeat, EngineReachable: engineReachable, ClockMs: clockMs})
	if _, err := control.Write(append(b, '\n')); err != nil {
		t.Fatalf("tunneltest: send a heartbeat: %v", err)
	}
}

// AnswerDocker serves the Docker streams the hub opens on this connection: for
// each it reads the kind byte and one HTTP request and answers it with status
// and body. It runs until the connection ends. A stream of another kind is closed.
func (c *Conn) AnswerDocker(t testing.TB, status int, body string) {
	t.Helper()
	if c.Mux == nil {
		t.Fatal("tunneltest: no multiplexer on this connection")
	}
	go func() {
		for {
			st, err := c.Mux.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = st.Close() }()
				br := bufio.NewReader(st)
				kind, err := br.ReadByte()
				if err != nil || kind != wire.StreamKindDockerHTTP {
					return
				}
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				_ = req.Body.Close()
				_, _ = fmt.Fprintf(st, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
					status, http.StatusText(status), len(body), body)
			}()
		}
	}()
}

// ProxyDocker serves the Docker streams the hub opens on this connection by
// piping each to the unix socket at socketPath, a fake daemon's for instance, so
// the real client, probe and snapshot run through the real tunnel. It runs until
// the connection ends. A stream of another kind is closed.
func (c *Conn) ProxyDocker(t testing.TB, socketPath string) {
	t.Helper()
	if c.Mux == nil {
		t.Fatal("tunneltest: no multiplexer on this connection")
	}
	go func() {
		for {
			st, err := c.Mux.Accept()
			if err != nil {
				return
			}
			go pipeDocker(st, socketPath)
		}
	}()
}

// pipeDocker copies one Docker stream to and from the daemon until either side
// ends.
func pipeDocker(st net.Conn, socketPath string) {
	defer func() { _ = st.Close() }()
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil || kind[0] != wire.StreamKindDockerHTTP {
		return
	}
	up, err := net.Dial("unix", socketPath)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, st); done <- struct{}{} }()
	go func() { _, _ = io.Copy(st, up); done <- struct{}{} }()
	<-done // whichever side ends first ends the exchange; the deferred closes release the other copy
}

// ForeignCert issues a client certificate shaped like Bayplan's (CN the agent ID,
// OU the environment ID, client authentication) from a CA the server has never
// heard of. With envID of a real environment it is a certificate "signed by
// another environment's CA"; with an unknown one it names an environment that
// does not exist.
func ForeignCert(t testing.TB, envID, agentID string, pub ed25519.PublicKey, now time.Time) []byte {
	t.Helper()
	caPub, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "foreign CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: agentID, OrganizationalUnit: []string{envID}},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(30 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, pub, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// NewKey returns a fresh Ed25519 key pair.
func NewKey(t testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 2*n)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0xf])
	}
	return string(out)
}

// defaultHost is the part of the default engine ID that makes it the environment's
// own: the environment ID inside the join token or, for a hello, in the identity.
// A token that does not parse gets a random one.
func defaultHost(a Agent) string {
	if a.Identity != nil && a.Identity.EnvID != "" {
		return a.Identity.EnvID
	}
	if envID, _, ok := agents.ParseToken(a.Token); ok {
		return envID
	}
	return randomHex(4)
}
