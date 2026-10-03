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
// jittered exponential backoff and never gives up. Renewal and pinning arrive in
// later plans.
package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	// HTTPClient dials the server. Nil means the default client, which trusts
	// the system roots and honours SSL_CERT_FILE.
	HTTPClient *http.Client
	// StateDir holds the agent's identity (key, certificate, IDs). Empty keeps
	// the identity in memory only, so a restart enrols again.
	StateDir string
	// BackoffMin and BackoffMax bound the reconnect delay. Zero means 1 s and 60 s.
	BackoffMin, BackoffMax time.Duration
}

// runEnv is what a test replaces: the wait between attempts, the jitter and the
// clock the heartbeat reports.
type runEnv struct {
	sleep func(context.Context, time.Duration) error
	rnd   func() float64
	now   func() time.Time
}

// RefusedError is returned when the server answers the handshake with a refusal.
type RefusedError struct {
	Code, Detail string
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
	// factsTimeout bounds the two reads of the engine's own description.
	factsTimeout = 5 * time.Second
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

	r := &runner{cfg: cfg, env: env, log: log, aud: aud}
	if cfg.StateDir != "" {
		cur, ok, err := state.Load(cfg.StateDir)
		if err != nil {
			return err
		}
		r.cur, r.have = cur, ok
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
	return backoff(*failures, r.cfg.BackoffMin, r.cfg.BackoffMax, r.env.rnd)
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
	}
	return ""
}

// once makes one connection: dial, handshake, serve. welcomed reports whether
// the server accepted the agent, so the caller can reset its failure count.
func (r *runner) once(ctx context.Context) (welcomed bool, err error) {
	// The engine is described before the dial, so the handshake itself never
	// waits on it. An engine that does not answer is reported, not fatal: the
	// server shows engine_unreachable (D-40).
	facts := readFacts(ctx, r.cfg.SocketPath, r.log)

	cr, err := r.credentials()
	if err != nil {
		return false, err
	}

	c, resp, err := websocket.Dial(ctx, r.cfg.URL, &websocket.DialOptions{ //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
		HTTPClient:   r.cfg.HTTPClient,
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
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
	welcome, err := handshake(hctx, c, resp, r.cfg.Version, r.aud, cr, facts)
	cancel()
	if err != nil {
		return false, err
	}
	if err := r.accept(cr, welcome); err != nil {
		return false, err
	}
	r.forceEnrol = false

	every := time.Duration(welcome.HeartbeatMs) * time.Millisecond
	if welcome.HeartbeatMs <= 0 {
		every = wire.DefaultHeartbeatMs * time.Millisecond
	}
	// A server cannot make the agent spin: the floor is far below any real setting.
	every = max(every, minHeartbeat)
	return true, serve(ctx, c, r.cfg.SocketPath, every, r.env.now, r.log)
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
// memory and saved, and every welcome marks the agent as connected.
func (r *runner) accept(cr credentials, w wire.Welcome) error {
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
	if r.cfg.StateDir != "" {
		if err := state.MarkConnected(r.cfg.StateDir); err != nil {
			r.log.Warn("the agent could not write its connected marker", "error", err)
		}
	}
	return nil
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
	cr credentials, facts wire.Facts) (wire.Welcome, error) {
	var ch wire.Challenge
	if err := readJSON(ctx, c, &ch); err != nil {
		return wire.Welcome{}, fmt.Errorf("read the server challenge: %w", err)
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
	var ekm []byte
	if resp != nil {
		ekm = authn.EKM(resp.TLS)
	}
	auth := wire.Auth{
		T: wire.TypeAuth, Mode: cr.mode, Proto: wire.ProtoVersion, AgentVersion: version,
		Aud: aud, Nonce: nonce, TS: time.Now().UnixMilli(), EKM: ekm, Facts: facts,
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
		var r wire.Refuse
		if err := json.Unmarshal(raw, &r); err != nil {
			return wire.Welcome{}, errors.New("the server refusal is malformed")
		}
		return wire.Welcome{}, &RefusedError{Code: r.Code, Detail: r.Detail}
	default:
		return wire.Welcome{}, fmt.Errorf("unexpected message %q from the server", head.T)
	}
}

// serve multiplexes the connection, opens the control stream and answers the
// server's Docker streams until the session ends. It ends when the multiplexer
// does, and when the control stream breaks, so a server that stopped reading
// heartbeats is not served as if all were well.
func serve(ctx context.Context, c *websocket.Conn, socketPath string, every time.Duration, now func() time.Time, log *slog.Logger) error {
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	sess, err := yamux.Server(conn, muxConfig())
	if err != nil {
		return fmt.Errorf("start the multiplexer: %w", err)
	}
	defer func() { _ = sess.Close() }()

	handler := proxy.Handler(socketPath)
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
		err := runControl(ctx, ctl, socketPath, every, now, log)
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

// readFacts asks the engine, through its socket, who it is. A failure leaves
// EngineReachable false.
func readFacts(ctx context.Context, socketPath string, log *slog.Logger) wire.Facts {
	f := wire.Facts{Policy: wire.PolicyFacts{SelfUpdate: false}}
	hc := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}},
		Timeout: factsTimeout,
	}
	defer hc.CloseIdleConnections()

	var info struct {
		ID, Name      string
		ServerVersion string
		Swarm         struct {
			LocalNodeState   string
			NodeID           string
			ControlAvailable bool
			Cluster          *struct{ ID string }
			RemoteManagers   []struct{ NodeID string }
		}
	}
	if err := getJSON(ctx, hc, "/info", &info); err != nil {
		log.Warn("the Docker engine did not answer GET /info", "socket", socketPath, "error", err)
		return f
	}
	f.EngineReachable = true
	f.EngineID, f.Hostname, f.EngineVersion = info.ID, info.Name, info.ServerVersion
	if st := info.Swarm.LocalNodeState; st != "" && st != "inactive" {
		sf := &wire.SwarmFacts{State: st, NodeID: info.Swarm.NodeID, Control: info.Swarm.ControlAvailable}
		if info.Swarm.Cluster != nil {
			sf.ClusterID = info.Swarm.Cluster.ID
		}
		for _, m := range info.Swarm.RemoteManagers {
			sf.ManagerIDs = append(sf.ManagerIDs, m.NodeID)
		}
		f.Swarm = sf
	}

	var ver struct {
		Version    string
		APIVersion string `json:"ApiVersion"`
	}
	if err := getJSON(ctx, hc, "/version", &ver); err != nil {
		log.Warn("the Docker engine did not answer GET /version", "socket", socketPath, "error", err)
		return f
	}
	f.APIVersion = ver.APIVersion
	if ver.Version != "" {
		f.EngineVersion = ver.Version
	}
	return f
}

func getJSON(ctx context.Context, hc *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}
