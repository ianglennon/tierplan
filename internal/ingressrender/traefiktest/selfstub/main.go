// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Command selfstub is a stand-in for TierPlan's own listener, for the tests of the self route (Phase 28).
// It is a test helper: it is built at test time by internal/ingressrender/traefiktest, run in a container
// on an --internal test network, and is never part of a shipped image or a release target.
//
// It answers the way the real server does where the proxy cares: one port that speaks TLS with
// HTTP/2 (certificate and key from -cert and -key) and, for a connection that opens in plain text,
// answers only the ACME challenge path (the shape of internal/server/plainchallenge.go). The TLS routes
// are the ones a route for TierPlan must carry or refuse:
//
//	/ and /proto      the protocol the request arrived on, so a test sees that HTTP/2 was negotiated
//	/events           a text/event-stream that writes a comment line every -sse-every (the real feed: 15 s)
//	/ws               a WebSocket echo that pings every -ping-every (the real shell bridge: 20 s)
//	/agent/connect    "reached", so a test sees whether the proxy let it through
//	/ingress/connect  the same
//	/client-ip        the client address httpx.ClientIPMiddleware derives, with the trusted ranges taken from
//	                  TIERPLAN_STUB_TRUSTED, and the X-Forwarded-For header it saw
//	/serial           the serial number (hex) of the certificate the stub serves now
//
// On SIGHUP the stub reads -cert and -key again and serves the new pair to every new handshake. It then
// drains the connections it already has (HTTP/2 GOAWAY, in-flight requests finish), so a proxy in front
// has to open new connections and verify the new certificate: the strictest form of TierPlan changing
// its certificate under a running route (Phase 28, A-02, A-03). A pair that does not load is logged and
// the old one kept.
//
// "selfstub hold" is the client of the long idle hold: it holds an event feed and a WebSocket open for a
// given time and says which one was cut first.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/httpx"
)

const (
	// trustedEnv is a comma-separated list of CIDR prefixes the stub treats as trusted proxies on
	// /client-ip, the way the real server treats its trusted proxy ranges.
	trustedEnv = "TIERPLAN_STUB_TRUSTED"

	// acmeChallengePrefix is where an ACME HTTP-01 challenge is fetched (internal/server).
	acmeChallengePrefix = "/.well-known/acme-challenge/"

	// tlsFirstByte is the record type of a TLS handshake: the first byte of a ClientHello.
	tlsFirstByte = 0x16
	// peekTimeout bounds the wait for a connection's first byte.
	peekTimeout = 10 * time.Second
	// drainTimeout bounds how long a replaced server waits for its connections to finish after a
	// reload; a long-lived stream still open then is cut.
	drainTimeout = time.Minute
)

// certStore is the TLS pair the stub serves, read from its files at start and again on each reload.
type certStore struct {
	certFile, keyFile string
	cur               atomic.Pointer[tls.Certificate]
}

// load reads the pair from the files and serves it from the next handshake on. On an error the pair
// served so far stays.
func (c *certStore) load() error {
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	if pair.Leaf == nil {
		if pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return err
		}
	}
	c.cur.Store(&pair)
	return nil
}

// get is tls.Config.GetCertificate: the pair served now.
func (c *certStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if p := c.cur.Load(); p != nil {
		return p, nil
	}
	return nil, errors.New("no certificate loaded")
}

// serial is the served leaf's serial number in hex, or "" before a pair is loaded.
func (c *certStore) serial() string {
	if p := c.cur.Load(); p != nil && p.Leaf != nil {
		return p.Leaf.SerialNumber.Text(16)
	}
	return ""
}

// stubConfig is what the handlers need.
type stubConfig struct {
	// sseEvery and pingEvery are the keepalive periods of /events and /ws.
	sseEvery, pingEvery time.Duration
	// trusted are the proxy ranges /client-ip honours X-Forwarded-For from.
	trusted []netip.Prefix
	// serial is the served certificate's serial for /serial; nil answers "".
	serial func() string
}

// parseTrusted reads a comma-separated list of prefixes; an empty string is none.
func parseTrusted(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a CIDR prefix: %w", trustedEnv, part, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// trustedFromEnv is the trusted ranges named by TIERPLAN_STUB_TRUSTED.
func trustedFromEnv() ([]netip.Prefix, error) { return parseTrusted(os.Getenv(trustedEnv)) }

// newTLSMux is the handler of the TLS side.
func newTLSMux(cfg stubConfig) http.Handler {
	mux := http.NewServeMux()
	proto := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, r.Proto) //nolint:gosec // G705: text/plain from a test stub, never rendered as HTML
	}
	mux.HandleFunc("/proto", proto)
	mux.HandleFunc("/{$}", proto)
	mux.HandleFunc("/events", cfg.events)
	mux.HandleFunc("/ws", cfg.ws)
	reached := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "reached")
	}
	mux.HandleFunc("/agent/connect", reached)
	mux.HandleFunc("/ingress/connect", reached)
	mux.Handle("/client-ip", httpx.ClientIPMiddleware(cfg.trusted)(http.HandlerFunc(clientIP)))
	mux.HandleFunc("/serial", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if cfg.serial != nil {
			_, _ = io.WriteString(w, cfg.serial())
		}
	})
	return mux
}

// clientIP answers with the address the middleware derived and the forwarding header it was given.
func clientIP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintf(w, "client=%s\nxff=%s\n", httpx.ClientIP(r.Context()), strings.Join(r.Header.Values("X-Forwarded-For"), ", ")) //nolint:gosec // G705: text/plain from a test stub, never rendered as HTML
}

// events is the event feed: headers at once, then a comment line every sseEvery, each flushed.
func (c stubConfig) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	tick := time.NewTicker(c.sseEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

// ws is a WebSocket echo. The server pings every pingEvery, as the shell bridge does, and logs each
// pong it gets back, so a test can count them in the container's log.
func (c stubConfig) ws(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var pongs atomic.Int64
	go func() {
		tick := time.NewTicker(c.pingEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				// Ping returns when the pong arrives, which the read loop below makes possible.
				if err := conn.Ping(ctx); err != nil {
					return
				}
				log.Printf("ws pong %d", pongs.Add(1))
			}
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// plainChallenge is what a plain-text request reaches: the challenge path answers "<token>.stub", and
// any other request has its connection closed with nothing written.
func plainChallenge(w http.ResponseWriter, r *http.Request) {
	if tok, ok := strings.CutPrefix(r.URL.Path, acmeChallengePrefix); ok && tok != "" && !strings.Contains(tok, "/") {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, tok+".stub") //nolint:gosec // G705: text/plain from a test stub, never rendered as HTML
		return
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// peekedConn is a connection whose first byte was read to choose a side; Read serves it back.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// chanListener is a listener fed by the dispatcher.
type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
	once sync.Once
	done chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn), addr: addr, done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

// requeue hands a connection back to whichever server accepts next, or closes it once the listener is.
func (l *chanListener) requeue(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		_ = c.Close()
	}
}

// generation is one server's view of a chanListener: closing it stops that server accepting while the
// listener goes on feeding the next one, so a reload replaces the server with no gap.
type generation struct {
	parent *chanListener
	once   sync.Once
	done   chan struct{}
}

func newGeneration(parent *chanListener) *generation {
	return &generation{parent: parent, done: make(chan struct{})}
}

func (g *generation) Accept() (net.Conn, error) {
	select {
	case c := <-g.parent.ch:
		select {
		case <-g.done:
			// Closed while this connection arrived: it belongs to the next server.
			go g.parent.requeue(c)
			return nil, net.ErrClosed
		default:
			return c, nil
		}
	case <-g.done:
		return nil, net.ErrClosed
	case <-g.parent.done:
		return nil, net.ErrClosed
	}
}

func (g *generation) Close() error {
	g.once.Do(func() { close(g.done) })
	return nil
}

func (g *generation) Addr() net.Addr { return g.parent.addr }

// dispatch hands a connection to the TLS or the plain side by its first byte.
func dispatch(conn net.Conn, tlsSide, plainSide *chanListener) {
	_ = conn.SetReadDeadline(time.Now().Add(peekTimeout))
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	pc := &peekedConn{Conn: conn, r: br}
	side := plainSide
	if first[0] == tlsFirstByte {
		side = tlsSide
	}
	select {
	case side.ch <- pc:
	case <-side.done:
		_ = conn.Close()
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("selfstub", flag.ContinueOnError)
	listen := fs.String("listen", ":7480", "address to listen on")
	certFile := fs.String("cert", "", "TLS certificate file")
	keyFile := fs.String("key", "", "TLS key file")
	sseEvery := fs.Duration("sse-every", 15*time.Second, "period of the comment line on /events")
	pingEvery := fs.Duration("ping-every", 20*time.Second, "period of the server ping on /ws")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *certFile == "" || *keyFile == "" {
		return errors.New("-cert and -key are required")
	}
	trusted, err := trustedFromEnv()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	tlsSide, plainSide := newChanListener(ln.Addr()), newChanListener(ln.Addr())
	go func() {
		plain := &http.Server{Handler: http.HandlerFunc(plainChallenge), ReadHeaderTimeout: peekTimeout}
		log.Printf("plain side stopped: %v", plain.Serve(plainSide))
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("accept: %v", err)
				_ = tlsSide.Close()
				_ = plainSide.Close()
				return
			}
			go dispatch(conn, tlsSide, plainSide)
		}
	}()
	store := &certStore{certFile: *certFile, keyFile: *keyFile}
	if err := store.load(); err != nil {
		return err
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cfg := stubConfig{sseEvery: *sseEvery, pingEvery: *pingEvery, trusted: trusted, serial: store.serial}
	log.Printf("selfstub listening on %s, certificate serial %s", ln.Addr(), store.serial())
	return serveTLS(tlsSide, cfg, store, hup)
}

// serveTLS runs the TLS side until its listener fails. Each reload signal loads the pair again and, when
// it loads, starts a new server on the listener and shuts the previous one down gracefully, which sends
// GOAWAY on its HTTP/2 connections and lets their requests finish.
func serveTLS(side *chanListener, cfg stubConfig, store *certStore, reload <-chan os.Signal) error {
	failed := make(chan error, 1)
	start := func() *http.Server {
		srv := &http.Server{
			Handler:           newTLSMux(cfg),
			ReadHeaderTimeout: peekTimeout,
			TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: store.get},
		}
		gen := newGeneration(side)
		go func() {
			if err := srv.ServeTLS(gen, "", ""); !errors.Is(err, http.ErrServerClosed) {
				select {
				case failed <- err:
				default:
				}
			}
		}()
		return srv
	}
	cur := start()
	for {
		select {
		case err := <-failed:
			return err
		case <-reload:
			if err := store.load(); err != nil {
				log.Printf("reload: keeping the certificate served so far: %v", err)
				continue
			}
			prev := cur
			cur = start()
			log.Printf("reloaded: certificate serial %s", store.serial())
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
				defer cancel()
				if err := prev.Shutdown(ctx); err != nil {
					log.Printf("reload: the previous server's connections did not drain: %v", err)
					_ = prev.Close()
					return
				}
				log.Printf("reload: the previous server's connections drained")
			}()
		}
	}
}

// hold opens an event feed and a WebSocket and keeps both for -for. It exits 0 and prints "held <time>"
// when neither was cut, and exits 1 naming the first one that was.
func hold(args []string) int {
	fs := flag.NewFlagSet("hold", flag.ContinueOnError)
	eventsURL := fs.String("url", "", "event feed URL (https)")
	wsURL := fs.String("ws", "", "WebSocket URL (wss)")
	caFile := fs.String("ca", "", "PEM file with the certificate to trust")
	resolve := fs.String("resolve", "", "host:ip, connect to ip when dialling host")
	holdFor := fs.Duration("for", 21*time.Minute, "how long to hold both streams")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *eventsURL == "" || *wsURL == "" || *caFile == "" {
		fmt.Fprintln(os.Stderr, "hold: -url, -ws and -ca are required")
		return 2
	}
	pemBytes, err := os.ReadFile(*caFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hold:", err)
		return 2
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		fmt.Fprintln(os.Stderr, "hold: no certificate in", *caFile)
		return 2
	}
	rHost, rIP, _ := strings.Cut(*resolve, ":")
	var d net.Dialer
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && rHost != "" && host == rHost {
				addr = net.JoinHostPort(rIP, port)
			}
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	hc := &http.Client{Transport: tr}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), *holdFor)
	defer cancel()
	cuts := make(chan string, 2)
	report := func(what string, err error) {
		if ctx.Err() != nil {
			return // the hold ended; a stream closing then is not a cut
		}
		cuts <- fmt.Sprintf("%s cut after %s: %v", what, time.Since(start).Round(time.Second), err)
	}
	go func() { report("event feed", holdEvents(ctx, hc, *eventsURL)) }()
	go func() { report("websocket", holdWS(ctx, hc, *wsURL)) }()

	select {
	case msg := <-cuts:
		fmt.Println(msg)
		return 1
	case <-ctx.Done():
		fmt.Printf("held %s\n", time.Since(start).Round(time.Second))
		return 0
	}
}

func holdEvents(ctx context.Context, hc *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

func holdWS(ctx context.Context, hc *http.Client, url string) error {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc}) //nolint:bodyclose // a WebSocket upgrade response has no body to close
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		// Read answers the server's pings; nothing else arrives on an idle echo.
		if _, _, err := conn.Read(ctx); err != nil {
			return err
		}
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		os.Exit(hold(os.Args[2:]))
	}
	if err := serve(os.Args[1:]); err != nil {
		log.Fatalf("selfstub: %v", err)
	}
}
