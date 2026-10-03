// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package connect is the agent's one connection to Bayplan: dial out over
// WebSocket, authenticate inside it, then serve the server's Docker calls over
// one multiplexed session. No port on the host is opened (D-18).
//
// This is the tracer slice. The agent generates its key in memory and enrols
// with the join token every time it starts; the state volume, the hello path,
// renewal, heartbeats and pinning arrive in later plans.
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
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/agent/internal/proxy"
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

// Run connects, enrols and serves until the session ends or ctx does. It
// returns nil only when ctx ended; a refused handshake is a *RefusedError.
func Run(ctx context.Context, cfg Config) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = defaultSocket
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

	// The engine is described before the dial, so the handshake itself never
	// waits on it. An engine that does not answer is reported, not fatal: the
	// server shows engine_unreachable (D-40).
	facts := readFacts(ctx, cfg.SocketPath, log)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate the agent key: %w", err)
	}

	c, resp, err := websocket.Dial(ctx, cfg.URL, &websocket.DialOptions{ //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
		HTTPClient:   cfg.HTTPClient,
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		return fmt.Errorf("connect to %s: %w", aud, err)
	}
	defer func() { _ = c.CloseNow() }()
	if c.Subprotocol() != wire.Subprotocol {
		return fmt.Errorf("%s does not speak %s", aud, wire.Subprotocol)
	}
	// Until the handshake is over the raw connection keeps its read limit; the
	// net.Conn wrapper removes it (Pitfall 4), so it is made only afterwards.
	c.SetReadLimit(wire.MaxHandshakeBytes)

	hctx, cancel := context.WithTimeout(ctx, wire.HandshakeTimeout)
	welcome, err := handshake(hctx, c, resp, cfg, aud, pub, priv, facts)
	cancel()
	if err != nil {
		return err
	}
	log.Info("enrolled with Bayplan", "agent_id", welcome.AgentID, "environment_id", welcome.EnvID, "server", aud)

	return serve(ctx, c, cfg.SocketPath, log)
}

// handshake runs the enrol exchange on the raw connection.
func handshake(ctx context.Context, c *websocket.Conn, resp *http.Response, cfg Config, aud string,
	pub ed25519.PublicKey, priv ed25519.PrivateKey, facts wire.Facts) (wire.Welcome, error) {
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
	tr := authn.Transcript(wire.ModeEnrol, wire.ProtoVersion, ch.Nonce, nonce, aud, pub, ekm)
	auth := wire.Auth{
		T: wire.TypeAuth, Mode: wire.ModeEnrol, Proto: wire.ProtoVersion, AgentVersion: cfg.Version,
		Aud: aud, Nonce: nonce, TS: time.Now().UnixMilli(),
		Pub: pub, Token: cfg.JoinToken, EKM: ekm, Sig: authn.Sign(priv, tr), Facts: facts,
	}
	if err := writeJSON(ctx, c, auth); err != nil {
		return wire.Welcome{}, fmt.Errorf("send the handshake: %w", err)
	}

	var raw json.RawMessage
	if err := readJSON(ctx, c, &raw); err != nil {
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

// serve multiplexes the connection and answers the server's streams until the
// session ends.
func serve(ctx context.Context, c *websocket.Conn, socketPath string, log *slog.Logger) error {
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
	for {
		st, err := sess.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
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
