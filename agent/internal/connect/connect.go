// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package connect is the agent's one connection to Bayplan: dial out over
// WebSocket, authenticate inside it, then serve the server's Docker calls over
// one multiplexed session. No port on the host is opened (D-18).
//
// The agent enrols once, with the join token, and keeps the key it generated and
// the certificate it was issued in its state directory (package state). Every
// later connection, after a restart or a dropped link, says hello with that
// certificate and signs the server's challenge with that key; the token is not
// needed again (D-01, D-03). After the welcome the agent opens a control stream
// and heartbeats on it (D-41). When the connection ends it reconnects with
// jittered exponential backoff and never gives up. The server's certificate is
// verified by the system roots or by a pin (tls.go). Renewal arrives in a later
// plan.
package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/agent/internal/proxy"
	"github.com/ianglennon/bayplan/agent/internal/facts"
	"github.com/ianglennon/bayplan/agent/internal/state"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Config is what Run needs.
type Config struct {
	// URL is the server's agent endpoint, for example wss://bayplan.example/agent/connect.
	URL string
	// JoinToken is presented once, in the handshake. It is never logged.
	JoinToken string
	// SocketPath is the Docker socket. Empty means /var/run/docker.sock.
	SocketPath string
	// Version is the agent's version, reported in the handshake.
	Version string
	// Log receives one line per event. Nil means slog.Default().
	Log *slog.Logger
	// HTTPClient dials the server. Nil means the agent's own client: system
	// roots (honouring SSL_CERT_FILE), the pins below and those stored from
	// earlier welcomes, and the proxy from the environment (tls.go). A caller
	// that supplies one takes over verification; the agent binary never does.
	HTTPClient *http.Client
	// InstallPins are the server certificate pins of BAYPLAN_AGENT_PIN: the
	// pin of Bayplan's own certificate, or of a certificate authority in the
	// chain of a private-CA proxy.
	InstallPins []string
	// StateDir holds the agent's identity (key, certificate, IDs). Empty keeps
	// the identity in memory only, so a restart enrols again.
	StateDir string
	// BackoffMin and BackoffMax bound the reconnect delay. Zero means 1 s and 60 s.
	BackoffMin, BackoffMax time.Duration
	// ReadOnly makes the agent refuse every request that is not GET or HEAD, and
	// SelfUpdate says whether the host lets the agent update itself. Both are
	// reported in the handshake so the UI can show them (D-28).
	ReadOnly, SelfUpdate bool
}

// runEnv is what a test replaces: the wait between attempts, the jitter and the
// clock the heartbeat reports.
type runEnv struct {
	sleep func(context.Context, time.Duration) error
	rnd   func() float64
	now   func() time.Time
	// roots replaces the system roots in a test; nil in production.
	roots *x509.CertPool
}

// RefusedError is returned when the server answers the handshake with a refusal.
type RefusedError struct {
	Code, Detail string
	// RetryAfter is how long the server asked the agent to wait, when it did.
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	if e.Detail == "" {
		return "the server refused this agent: " + e.Code
	}
	return "the server refused this agent: " + e.Code + ": " + e.Detail
}

const (
	defaultSocket = "/var/run/docker.sock"
	// kindTimeout bounds the wait for the kind byte of a new stream.
	kindTimeout = 10 * time.Second
	// maxRetryAfter bounds how long the agent will wait because a server said so.
	maxRetryAfter = 5 * time.Minute
)

// muxConfig is the yamux configuration of RESEARCH Pattern 1: detection of a
// silent black hole in at most about 20 s, inside the D-41 window of 30 s.
func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 10 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamCloseTimeout = 30 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.AcceptBacklog = 64
	c.LogOutput = io.Discard
	return c
}

// Run connects and serves until ctx ends. It returns nil only when ctx ended: a
// dropped connection or a refusal is retried with backoff, so a revoked agent
// that an administrator later unblocks comes back by itself. It returns an error
// for a configuration the agent cannot work with, and for a refusal of the very
// first enrolment (a *RefusedError), because a wrong token does not fix itself.
func Run(ctx context.Context, cfg Config) error {
	return runWith(ctx, cfg, runEnv{})
}

// errNoCredentials means the agent has neither an identity nor a join token.
var errNoCredentials = errors.New("the agent has no identity yet and no join token: set BAYPLAN_AGENT_JOIN_TOKEN_FILE or BAYPLAN_AGENT_JOIN_TOKEN")

// runner is the agent's state across connection attempts.
type runner struct {
	cfg Config
	env runEnv
	log *slog.Logger
	aud string
	// host is the name or address of the server, which its certificate must be
	// valid for.
	host string

	// cur is the identity, once the agent has one. It is also kept in memory when
	// there is no state directory, so a reconnect in the same process never
	// enrols a second agent.
	cur  state.State
	have bool
	// forceEnrol makes the next attempt enrol although an identity exists: the
	// certificate expired and a join token is at hand.
	forceEnrol bool
	// lastCode is the refusal code last logged, so a refusal that repeats every
	// minute is said once.
	lastCode string
	// pins are the server pins kept from welcomes, with their overlap deadlines.
	pins []state.PinEntry
}

func runWith(ctx context.Context, cfg Config, env runEnv) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = defaultSocket
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = defaultBackoffMin
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = defaultBackoffMax
	}
	if env.sleep == nil {
		env.sleep = sleepCtx
	}
	if env.now == nil {
		env.now = time.Now
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") {
		return fmt.Errorf("BAYPLAN_AGENT_URL must be a wss:// (or ws://) address: %q", cfg.URL)
	}
	aud, err := authn.Audience(cfg.URL)
	if err != nil {
		return fmt.Errorf("BAYPLAN_AGENT_URL: %w", err)
	}
	if u.Scheme == "ws" {
		log.Warn("the agent endpoint is plain ws://: the join token and every Docker answer travel unencrypted")
	}

	r := &runner{cfg: cfg, env: env, log: log, aud: aud, host: u.Hostname()}
	if cfg.StateDir != "" {
		cur, ok, err := state.Load(cfg.StateDir)
		if err != nil {
			return err
		}
		r.cur, r.have = cur, ok
		// A pins file that cannot be read leaves the agent with fewer pins, which
		// can only make it refuse more, never trust more.
		if r.pins, err = state.LoadPins(cfg.StateDir); err != nil {
			log.Warn("the agent could not read its stored server pins; it will trust only the install pin and the system roots", "error", err)
			r.pins = nil
		}
	}
	if !r.have && cfg.JoinToken == "" {
		return errNoCredentials
	}

	failures := 0
	for {
		welcomed, err := r.once(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errNoCredentials) {
			return err
		}
		if welcomed {
			// The connection worked: whatever ended it is a first failure, and a
			// refusal after it is news again.
			failures = 0
			r.lastCode = ""
		}
		var wait time.Duration
		var refused *RefusedError
		if errors.As(err, &refused) {
			if !r.have && !transient(refused.Code) {
				// The very first enrolment was refused for a reason that will not
				// pass by itself, most often a wrong or disabled join token. Say so
				// and stop, so the operator sees the failure where they installed
				// the agent. An enrolled agent never stops for a refusal.
				return refused
			}
			wait = r.refused(refused, &failures)
		} else if untrusted := asUntrusted(err); untrusted != nil {
			// The server could not be verified, so nothing was sent to it. Said
			// once, with the way out; a server that stays untrusted is retried
			// quietly with the usual backoff until its certificate changes.
			failures++
			wait = backoff(failures, cfg.BackoffMin, cfg.BackoffMax, env.rnd)
			if r.lastCode != untrustedCode {
				r.lastCode = untrustedCode
				log.Warn("the server's certificate could not be verified, so the agent did not connect",
					"code", untrustedCode, "detail", untrusted.Detail, "hint", untrustedHint, "retry_in", wait.Round(time.Millisecond).String())
			}
		} else {
			failures++
			wait = backoff(failures, cfg.BackoffMin, cfg.BackoffMax, env.rnd)
			log.Warn("the connection to Bayplan ended; will reconnect", "error", err, "retry_in", wait.Round(time.Millisecond).String())
		}
		if err := env.sleep(ctx, wait); err != nil {
			return nil
		}
	}
}

// refused decides how long to wait after a refusal, and says why once.
//
// A revoked or blocked agent waits the maximum: nothing the agent can do changes
// the answer, but an administrator may. It keeps its identity so that an
// unblock needs no new enrolment. An expired certificate with a join token at
// hand enrols again at once; without one only an administrator can help, so it
// waits the maximum too. Anything else backs off as an ordinary failure.
func (r *runner) refused(re *RefusedError, failures *int) time.Duration {
	if re.Code != r.lastCode {
		r.lastCode = re.Code
		r.log.Warn("the server refused this agent", "code", re.Code, "detail", re.Detail, "hint", hint(re.Code))
	}
	switch re.Code {
	case wire.ReasonRevoked, wire.ReasonBlocked:
		return r.cfg.BackoffMax
	case wire.ReasonCertExpired:
		if r.cfg.JoinToken == "" {
			return r.cfg.BackoffMax
		}
		r.forceEnrol = true
	}
	*failures++
	wait := backoff(*failures, r.cfg.BackoffMin, r.cfg.BackoffMax, r.env.rnd)
	// A server that is busy says when to come back; never sooner than that.
	return max(wait, min(re.RetryAfter, maxRetryAfter))
}

// transient reports whether a refusal is the server's trouble rather than the
// agent's: it will pass, so the agent retries instead of ending.
func transient(code string) bool {
	return code == wire.RefuseRateLimited || code == wire.RefuseInternal
}

// hint is what an operator should do about a refusal code.
func hint(code string) string {
	switch code {
	case wire.ReasonRevoked:
		return "an administrator revoked this agent in Bayplan; remove it or unblock it there, and enrol again with a join token if it was removed"
	case wire.ReasonBlocked:
		return "this host is on Bayplan's block list; an administrator can unblock it in the environment's settings"
	case wire.ReasonCertExpired:
		return "the agent's certificate expired; start the agent with a join token (BAYPLAN_AGENT_JOIN_TOKEN_FILE or BAYPLAN_AGENT_JOIN_TOKEN) to enrol again"
	case wire.ReasonAuthRejected:
		return "Bayplan did not accept the agent's credentials; check the join token and BAYPLAN_AGENT_URL"
	case wire.ReasonVersionIncompatible:
		return "upgrade the agent to the version Bayplan runs"
	case wire.ReasonClockSkew:
		return "set this host's clock, for example with NTP"
	case wire.ReasonClusterChanged:
		return "the swarm behind this environment was re-initialised; an administrator adopts the new cluster in the environment's settings in Bayplan, and the agent connects on its next try"
	case wire.ReasonEngineUnreachable:
		return "the agent could not read Docker's description of this host; check that /var/run/docker.sock is mounted into the agent and that Docker is running"
	}
	return ""
}

// once makes one connection: dial, handshake, serve. welcomed reports whether
// the server accepted the agent, so the caller can reset its failure count.
func (r *runner) once(ctx context.Context) (welcomed bool, err error) {
	// The engine is described before the dial, so the handshake itself never
	// waits on it. An engine that does not answer is reported, not fatal: the
	// server shows engine_unreachable (D-40).
	reported := facts.Collect(ctx, r.cfg.SocketPath)
	if !reported.EngineReachable {
		r.log.Warn("the Docker engine did not answer on its socket", "socket", r.cfg.SocketPath)
	}
	// What the host owner set is part of the facts, so the UI can say "read-only
	// (set on host)" and show the manual update commands (D-28).
	reported.Policy = wire.PolicyFacts{ReadOnly: r.cfg.ReadOnly, SelfUpdate: r.cfg.SelfUpdate}

	cr, err := r.credentials()
	if err != nil {
		return false, err
	}

	client := r.cfg.HTTPClient
	if client == nil {
		client = newHTTPClient(state.ActivePins(r.pins, r.env.now()), r.cfg.InstallPins, r.env.roots, r.host)
		defer client.CloseIdleConnections()
	}
	c, resp, err := websocket.Dial(ctx, r.cfg.URL, &websocket.DialOptions{ //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
		HTTPClient:   client,
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		if u := asUntrusted(err); u != nil {
			return false, u
		}
		return false, fmt.Errorf("connect to %s: %w", r.aud, err)
	}
	// CloseNow, never a graceful close: a peer that stopped answering would make
	// a close handshake wait (Pitfall 6).
	defer func() { _ = c.CloseNow() }()
	if c.Subprotocol() != wire.Subprotocol {
		return false, fmt.Errorf("%s does not speak %s", r.aud, wire.Subprotocol)
	}
	// Until the handshake is over the raw connection keeps its read limit; the
	// net.Conn wrapper removes it (Pitfall 4), so it is made only afterwards.
	c.SetReadLimit(wire.MaxHandshakeBytes)

	hctx, cancel := context.WithTimeout(ctx, wire.HandshakeTimeout)
	welcome, err := handshake(hctx, c, resp, r.cfg.Version, r.aud, cr, reported, r.env.now)
	cancel()
	if err != nil {
		return false, err
	}
	if err := r.accept(cr, welcome, resp != nil && resp.TLS != nil); err != nil {
		return false, err
	}
	r.forceEnrol = false

	every := time.Duration(welcome.HeartbeatMs) * time.Millisecond
	if welcome.HeartbeatMs <= 0 {
		every = wire.DefaultHeartbeatMs * time.Millisecond
	}
	// A server cannot make the agent spin: the floor is far below any real setting.
	every = max(every, minHeartbeat)
	return true, serve(ctx, c, r.cfg.SocketPath, every, r.env.now, r.log, proxy.Policy{ReadOnly: r.cfg.ReadOnly})
}

// minHeartbeat is the shortest interval the agent accepts from a server.
const minHeartbeat = 10 * time.Millisecond

// credentials is what this attempt presents: the certificate and key when the
// agent has an identity, else a fresh key and the join token.
func (r *runner) credentials() (credentials, error) {
	if r.have && !r.forceEnrol {
		return credentials{
			mode: wire.ModeHello, agentID: r.cur.AgentID, cert: r.cur.Cert,
			pub: publicOf(r.cur.Key), priv: r.cur.Key,
		}, nil
	}
	if r.cfg.JoinToken == "" {
		return credentials{}, errNoCredentials
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return credentials{}, fmt.Errorf("generate the agent key: %w", err)
	}
	return credentials{mode: wire.ModeEnrol, token: r.cfg.JoinToken, pub: pub, priv: priv}, nil
}

// accept records the outcome of a welcome: an enrolment's identity is kept in
// memory and saved, the server's pins are merged in, and every welcome marks the
// agent as connected. verified says the session ran over TLS that was verified;
// pins from any other session are not taken, so a spoofed server cannot plant one
// (T-02-33).
func (r *runner) accept(cr credentials, w wire.Welcome, verified bool) error {
	switch cr.mode {
	case wire.ModeEnrol:
		if len(w.Cert) == 0 {
			return errors.New("the server welcomed the agent without a certificate")
		}
		r.cur = state.State{AgentID: w.AgentID, EnvID: w.EnvID, Key: cr.priv, Cert: w.Cert}
		r.have = true
		r.log.Info("enrolled with Bayplan", "agent_id", w.AgentID, "environment_id", w.EnvID, "server", r.aud)
		if r.cfg.StateDir != "" {
			if err := state.Save(r.cfg.StateDir, r.cur); err != nil {
				// Staying connected beats stopping: the identity is in memory, and
				// the agent says so loudly because a restart will enrol again.
				r.log.Error("the agent could not save its identity; a restart will enrol it again as a new agent", "dir", r.cfg.StateDir, "error", err)
			}
		}
	default:
		if w.AgentID != r.cur.AgentID {
			return fmt.Errorf("the server welcomed agent %q but this agent is %q", w.AgentID, r.cur.AgentID)
		}
		r.log.Info("connected to Bayplan", "agent_id", w.AgentID, "environment_id", w.EnvID, "server", r.aud)
	}
	if verified {
		r.mergePins(w.Pins)
	}
	if r.cfg.StateDir != "" {
		if err := state.MarkConnected(r.cfg.StateDir); err != nil {
			r.log.Warn("the agent could not write its connected marker", "error", err)
		}
	}
	return nil
}

// mergePins keeps the pins a welcome lists, and for 30 days the ones it no longer
// does (state.MergePins), saving them when they changed.
func (r *runner) mergePins(fromServer []string) {
	merged := state.MergePins(r.pins, fromServer, r.env.now())
	if state.EqualPins(merged, r.pins) {
		return
	}
	r.pins = merged
	if r.cfg.StateDir == "" {
		return
	}
	if err := state.SavePins(r.cfg.StateDir, merged); err != nil {
		r.log.Warn("the agent could not save the server's pins; it will trust the server by the install pin or the system roots after a restart", "dir", r.cfg.StateDir, "error", err)
	}
}

// publicOf is the public half of an Ed25519 private key: the last 32 bytes of it.
func publicOf(k ed25519.PrivateKey) ed25519.PublicKey { return ed25519.PublicKey(k[ed25519.SeedSize:]) }

// credentials is the identity an attempt presents.
type credentials struct {
	mode    string
	agentID string // hello
	cert    []byte // hello
	token   string // enrol
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handshake runs the challenge, auth and welcome exchange on the raw connection.
// In enrol mode the signature proves possession of the new key; in hello mode it
// is made with the key the certificate was issued for, over a transcript that
// carries the certificate.
func handshake(ctx context.Context, c *websocket.Conn, resp *http.Response, version, aud string,
	cr credentials, facts wire.Facts, now func() time.Time) (wire.Welcome, error) {
	var ch wire.Challenge
	var first json.RawMessage
	if err := readJSON(ctx, c, &first); err != nil {
		return wire.Welcome{}, fmt.Errorf("read the server challenge: %w", err)
	}
	if err := json.Unmarshal(first, &ch); err != nil {
		return wire.Welcome{}, fmt.Errorf("the server challenge is not JSON: %w", err)
	}
	if ch.T == wire.TypeRefuse {
		// A server that is busy turns the agent away before the challenge.
		return wire.Welcome{}, refusal(first)
	}
	if ch.T != wire.TypeChallenge {
		return wire.Welcome{}, fmt.Errorf("expected a challenge from the server, got %q", ch.T)
	}
	if ch.Proto != wire.ProtoVersion {
		return wire.Welcome{}, &RefusedError{Code: wire.ReasonVersionIncompatible,
			Detail: fmt.Sprintf("the server speaks protocol %d, this agent speaks %d", ch.Proto, wire.ProtoVersion)}
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return wire.Welcome{}, fmt.Errorf("generate a nonce: %w", err)
	}
	// The channel binding: ExportKeyingMaterial on the TLS state of the dial
	// response. A server that terminates TLS itself computes the same value; behind
	// a TLS-terminating proxy the two differ and the server does not compare them.
	var ekm []byte
	if resp != nil {
		ekm = authn.EKM(resp.TLS)
	}
	auth := wire.Auth{
		T: wire.TypeAuth, Mode: cr.mode, Proto: wire.ProtoVersion, AgentVersion: version,
		Aud: aud, Nonce: nonce, TS: now().UnixMilli(), EKM: ekm, Facts: facts,
	}
	var signed []byte
	if cr.mode == wire.ModeHello {
		auth.AgentID, auth.Cert = cr.agentID, cr.cert
		signed = cr.cert
	} else {
		auth.Pub, auth.Token = cr.pub, cr.token
		signed = cr.pub
	}
	auth.Sig = authn.Sign(cr.priv, authn.Transcript(cr.mode, wire.ProtoVersion, ch.Nonce, nonce, aud, signed, ekm))
	if err := writeJSON(ctx, c, auth); err != nil {
		return wire.Welcome{}, fmt.Errorf("send the handshake: %w", err)
	}

	var raw json.RawMessage
	if err := readJSON(ctx, c, &raw); err != nil {
		// A refusal is a message followed by a close; if the message did not
		// arrive, the close frame's reason is the code.
		if code := websocket.CloseStatus(err); code == websocket.StatusPolicyViolation {
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Reason != "" {
				return wire.Welcome{}, &RefusedError{Code: strings.TrimSpace(ce.Reason)}
			}
		}
		return wire.Welcome{}, fmt.Errorf("read the server answer: %w", err)
	}
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return wire.Welcome{}, fmt.Errorf("the server answer is not JSON: %w", err)
	}
	switch head.T {
	case wire.TypeWelcome:
		var w wire.Welcome
		if err := json.Unmarshal(raw, &w); err != nil || w.AgentID == "" || w.EnvID == "" {
			return wire.Welcome{}, errors.New("the server welcome is incomplete")
		}
		return w, nil
	case wire.TypeRefuse:
		return wire.Welcome{}, refusal(raw)
	default:
		return wire.Welcome{}, fmt.Errorf("unexpected message %q from the server", head.T)
	}
}

// refusal is the error for a refuse message.
func refusal(raw []byte) error {
	var r wire.Refuse
	if err := json.Unmarshal(raw, &r); err != nil {
		return errors.New("the server refusal is malformed")
	}
	return &RefusedError{Code: r.Code, Detail: r.Detail, RetryAfter: time.Duration(r.RetryAfterMs) * time.Millisecond}
}

// serve multiplexes the connection, opens the control stream and answers the
// server's Docker streams until the session ends. It ends when the multiplexer
// does, and when the control stream breaks, so a server that stopped reading
// heartbeats is not served as if all were well.
func serve(ctx context.Context, c *websocket.Conn, socketPath string, every time.Duration, now func() time.Time, log *slog.Logger, policy proxy.Policy) error {
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	sess, err := yamux.Server(conn, muxConfig())
	if err != nil {
		return fmt.Errorf("start the multiplexer: %w", err)
	}
	defer func() { _ = sess.Close() }()

	// Every request the proxy refuses is reported to the server on the control
	// stream, which has one writer: the proxy hands it over without blocking.
	den := newDenials()
	handler := proxy.Handler(socketPath, policy, den.report)
	go func() {
		<-ctx.Done()
		_ = sess.Close()
	}()

	ctl, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("open the control stream: %w", err)
	}
	controlErr := make(chan error, 1)
	go func() {
		err := runControl(ctx, ctl, socketPath, every, now, log, den)
		controlErr <- err
		_ = sess.Close() // ends Accept below
	}()

	for {
		st, err := sess.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			select {
			case cerr := <-controlErr:
				if cerr != nil {
					return fmt.Errorf("the control stream to Bayplan ended: %w", cerr)
				}
			default:
			}
			return fmt.Errorf("the connection to Bayplan ended: %w", err)
		}
		go serveStream(st, handler, log)
	}
}

// serveStream reads the kind byte and, for the one kind that exists, runs the
// allowlisting proxy on the stream. Any other kind is closed.
func serveStream(st net.Conn, h http.Handler, log *slog.Logger) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(kindTimeout))
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		return
	}
	if kind[0] != wire.StreamKindDockerHTTP {
		log.Warn("closed a stream of an unknown kind", "kind", kind[0])
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	proxy.ServeConn(st, h)
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("a handshake message must be a text frame")
	}
	return json.Unmarshal(data, v)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}
