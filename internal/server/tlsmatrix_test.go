// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The ROADMAP research flag for phase 13: one port must answer https:// and http:// per connection
// with HTTP/2, HTTP/1.1, WebSocket and SSE all working. These tests go through newHTTPServer and the
// sniffing listener, the same pair serve() uses, so they pin the production serving plane and not a
// copy of it.

// matrixEnv is a server on a loopback port whose runtime holds a certificate, with every armed peek
// timer under the test's control.
type matrixEnv struct {
	addr    string
	rt      *httpsRuntime
	timers  *manualTimers
	pool    *x509.CertPool
	release chan struct{}
}

func newMatrixEnv(t *testing.T) *matrixEnv {
	t.Helper()
	cert, leaf := genCert(t, "")
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	rt := newHTTPSRuntime()
	rt.Install(tlsKindSupplied, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil })
	env := &matrixEnv{rt: rt, timers: newManualTimers(), pool: pool, release: make(chan struct{})}

	mux := http.NewServeMux()
	mux.HandleFunc("/proto", func(w http.ResponseWriter, r *http.Request) {
		alpn := ""
		if r.TLS != nil {
			alpn = r.TLS.NegotiatedProtocol
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%s tls=%t alpn=%s", r.Proto, r.TLS != nil, alpn) //nolint:gosec // a test handler echoing the protocol Go reports, as text/plain
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		typ, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		if err := c.Write(r.Context(), typ, msg); err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: 1\n\n")
		fl.Flush()
		select {
		case <-env.release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: 2\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: 3\n\n")
		fl.Flush()
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env.addr = ln.Addr().String()
	srv := newHTTPServer(mux, nil, defaultReadTimeout)
	go func() { _ = srv.Serve(newSniffListener(ln, rt, env.timers.after)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return env
}

func (e *matrixEnv) tlsConfig() *tls.Config {
	return &tls.Config{RootCAs: e.pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
}

func newClient(t *testing.T, tr *http.Transport) *http.Client {
	t.Helper()
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// h2Client offers h2 and http/1.1, as a browser does.
func (e *matrixEnv) h2Client(t *testing.T) *http.Client {
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	return newClient(t, &http.Transport{TLSClientConfig: e.tlsConfig(), Protocols: &p})
}

// h1TLSClient offers http/1.1 only, and says so in ALPN.
func (e *matrixEnv) h1TLSClient(t *testing.T) *http.Client {
	var p http.Protocols
	p.SetHTTP1(true)
	cfg := e.tlsConfig()
	cfg.NextProtos = []string{"http/1.1"}
	return newClient(t, &http.Transport{TLSClientConfig: cfg, Protocols: &p})
}

func (e *matrixEnv) plainClient(t *testing.T) *http.Client {
	var p http.Protocols
	p.SetHTTP1(true)
	return newClient(t, &http.Transport{Protocols: &p})
}

// h2cClient speaks HTTP/2 over a plain connection with prior knowledge.
func (e *matrixEnv) h2cClient(t *testing.T) *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return newClient(t, &http.Transport{Protocols: &p})
}

// reply is what a client saw of a response whose body it has read and closed.
type reply struct {
	status int
	major  int
	proto  string
	body   string
}

func get(t *testing.T, c *http.Client, url string) (reply, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, major: resp.ProtoMajor, proto: resp.Proto, body: string(body)}, err
}

func TestTLSMatrixServesBothBranchesOnOnePort(t *testing.T) {
	env := newMatrixEnv(t)
	cases := []struct {
		name      string
		client    *http.Client
		scheme    string
		wantMajor int
		wantBody  string
	}{
		{"h2 over TLS", env.h2Client(t), "https", 2, "HTTP/2.0 tls=true alpn=h2"},
		{"HTTP/1.1 over TLS", env.h1TLSClient(t), "https", 1, "HTTP/1.1 tls=true alpn=http/1.1"},
		{"HTTP/1.1 plain", env.plainClient(t), "http", 1, "HTTP/1.1 tls=false alpn="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := get(t, tc.client, tc.scheme+"://"+env.addr+"/proto")
			if err != nil {
				t.Fatal(err)
			}
			if got.status != http.StatusOK || got.major != tc.wantMajor {
				t.Fatalf("status %d, ProtoMajor %d, want 200 and %d", got.status, got.major, tc.wantMajor)
			}
			if got.body != tc.wantBody {
				t.Fatalf("the handler saw %q, want %q", got.body, tc.wantBody)
			}
		})
	}
}

func TestTLSMatrixWebSocketOnBothBranches(t *testing.T) {
	env := newMatrixEnv(t)
	for _, tc := range []struct {
		name   string
		scheme string
		client *http.Client
	}{
		{"wss", "wss", env.h1TLSClient(t)},
		{"ws", "ws", env.plainClient(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testsupport.Context(t)
			c, _, err := websocket.Dial(ctx, tc.scheme+"://"+env.addr+"/ws", &websocket.DialOptions{HTTPClient: tc.client}) //nolint:bodyclose // a WebSocket upgrade response has no body to close
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.CloseNow() }()
			if err := c.Write(ctx, websocket.MessageText, []byte("hello over "+tc.name)); err != nil {
				t.Fatal(err)
			}
			_, got, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "hello over "+tc.name {
				t.Fatalf("echo = %q", got)
			}
		})
	}
}

func TestTLSMatrixSSEOutlivesThePeekTimer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scheme string
		client func(*matrixEnv, *testing.T) *http.Client
	}{
		{"h2 over TLS", "https", (*matrixEnv).h2Client},
		{"HTTP/1.1 over TLS", "https", (*matrixEnv).h1TLSClient},
		{"plain", "http", (*matrixEnv).plainClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newMatrixEnv(t)
			req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodGet, tc.scheme+"://"+env.addr+"/sse", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := tc.client(env, t).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			r := bufio.NewReader(resp.Body)
			want := func(event string) {
				t.Helper()
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						t.Fatalf("waiting for %q: %v", event, err)
					}
					if strings.TrimSpace(line) == event {
						return
					}
				}
			}
			want("data: 1")
			// The first event is out, so the peek is over and its timer was stopped before the
			// connection was handed on. Firing every timer that is still armed must not touch it.
			if n := env.timers.Pending(); n != 0 {
				t.Fatalf("%d peek timers are still armed after the stream began", n)
			}
			env.timers.FireAll()
			close(env.release)
			want("data: 2")
			want("data: 3")
		})
	}
}

func TestTLSMatrixRefusesH2CAndAnswersAClientHelloWhenNotLive(t *testing.T) {
	env := newMatrixEnv(t)

	// D-17: HTTP/2 is served over TLS only. A client that speaks it in clear gets no HTTP/2.
	if got, err := get(t, env.h2cClient(t), "http://"+env.addr+"/proto"); err == nil {
		t.Fatalf("an h2c prior-knowledge request was answered: %v %q", got.proto, got.body)
	}

	// With TLS removed a ClientHello is answered as plain HTTP rather than left hanging, and a plain
	// client is still served.
	env.rt.Remove()
	if _, err := get(t, env.h2Client(t), "https://"+env.addr+"/proto"); err == nil {
		t.Fatal("a TLS request succeeded while TLS was not live")
	}
	got, err := get(t, env.plainClient(t), "http://"+env.addr+"/proto")
	if err != nil || got.status != http.StatusOK || got.body != "HTTP/1.1 tls=false alpn=" {
		t.Fatalf("plain request after Remove: %+v %v", got, err)
	}
}

func TestTLSMatrixServeInstallsAndRemovesTLSLive(t *testing.T) {
	cfg := testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": t.TempDir() + "/data"})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	cert, leaf := genCert(t, "")
	env := &matrixEnv{pool: x509.NewCertPool()}
	env.pool.AddCert(leaf)

	healthz := func(scheme string, c *http.Client) (reply, error) {
		t.Helper()
		return get(t, c, scheme+"://"+addr+"/healthz")
	}
	if _, err := healthz("https", env.h2Client(t)); err == nil {
		t.Fatal("https answered on a server started without TLS")
	}
	if got, err := healthz("http", env.plainClient(t)); err != nil || got.status != http.StatusOK {
		t.Fatalf("plain /healthz before the install: %+v %v", got, err)
	}

	srv.https.Install(tlsKindSupplied, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil })
	got, err := healthz("https", env.h2Client(t))
	if err != nil || got.status != http.StatusOK || got.major != 2 {
		t.Fatalf("https /healthz after the install: %+v %v, want 200 over HTTP/2", got, err)
	}

	srv.https.Remove()
	if _, err := healthz("https", env.h2Client(t)); err == nil {
		t.Fatal("https still answered after the certificate was removed")
	}
	if got, err := healthz("http", env.plainClient(t)); err != nil || got.status != http.StatusOK {
		t.Fatalf("plain /healthz after the removal: %+v %v", got, err)
	}
}
