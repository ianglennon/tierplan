// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
)

// The gate tests in httpsgate_test.go call the handler with a chosen RemoteAddr. These go over real
// sockets through srv.serve, so the sniffing listener, the http.Server and the gate are the
// production ones. A wrapper on the listener makes the peer non-loopback, which no Go test client
// can be (RESEARCH Pitfall 4).

const refusalBody = "This TierPlan serves HTTPS on this port. Use an https:// address.\n"

// wireServer is a Build'd server being served on a loopback port.
type wireServer struct {
	srv  *Server
	cfg  config.Config
	addr string
}

// startWire builds a server from env and serves it on 127.0.0.1:0. When peer is not empty, every
// connection reports it as its remote address. It is stopped when the test ends.
func startWire(t *testing.T, env map[string]string, peer string) *wireServer {
	t.Helper()
	all := map[string]string{"TIERPLAN_DATA_DIR": t.TempDir() + "/data"}
	for k, v := range env {
		all[k] = v
	}
	cfg := testConfig(t, all)
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serving := ln
	if peer != "" {
		serving = newFakeAddrListener(t, ln, peer)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, serving) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &wireServer{srv: srv, cfg: cfg, addr: ln.Addr().String()}
}

// plainClient sends plain HTTP to the listener and does not follow redirects.
func (w *wireServer) plainClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// tlsClient speaks TLS to the listener whatever name the URL carries, trusting exactly the
// certificate TierPlan serves. Certificate verification stays on.
func (w *wireServer) tlsClient(t *testing.T) *http.Client {
	t.Helper()
	cfg, err := ownCertificateTrust(HealthcheckCertFile(w.cfg))
	if err != nil {
		t.Fatal(err)
	}
	// The name comes from the URL being fetched, so a Location naming a host the certificate does
	// not carry fails here.
	cfg.ServerName = ""
	tr := &http.Transport{
		TLSClientConfig: cfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, w.addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// wireReply is what a client saw of a response whose body it has read and closed.
type wireReply struct {
	status int
	header http.Header
	body   string
}

// wireGet sends a GET with an optional Host override and returns what came back.
func wireGet(t *testing.T, c *http.Client, url, host string) wireReply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return wireReply{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func TestGateWireRedirectsANonLoopbackPlainClient(t *testing.T) {
	w := startWire(t, map[string]string{
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://tierplan.example.com:8443",
	}, defaultFakePeer)
	// The Host header is the caller's to choose and must not reach the Location.
	resp := wireGet(t, w.plainClient(t), "http://"+w.addr+"/api/version?x=1", "evil.test")
	if resp.status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.status)
	}
	if got, want := resp.header.Get("Location"), "https://tierplan.example.com:8443/api/version?x=1"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := resp.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want none on the redirect", got)
	}
}

func TestGateWireFollowingTheLocationIsNotRedirectedAgain(t *testing.T) {
	w := startWire(t, map[string]string{
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://tierplan.example.com:8443",
	}, defaultFakePeer)
	first := wireGet(t, w.plainClient(t), "http://"+w.addr+"/api/version?x=1", "")
	loc := first.header.Get("Location")
	if first.status != http.StatusTemporaryRedirect || loc == "" {
		t.Fatalf("plain request: status = %d, Location = %q, want a 307 with a Location", first.status, loc)
	}
	// The same connection, now over TLS, from the same non-loopback peer.
	resp := wireGet(t, w.tlsClient(t), loc, "")
	if resp.status != http.StatusOK {
		t.Fatalf("GET %s over TLS: status = %d, Location = %q, want 200 and no second redirect", loc, resp.status, resp.header.Get("Location"))
	}
}

func TestGateWireServesALoopbackPlainClient(t *testing.T) {
	w := startWire(t, map[string]string{
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://tierplan.example.com:8443",
	}, "")
	resp := wireGet(t, w.plainClient(t), "http://"+w.addr+"/api/version", "")
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, Location = %q, want 200: a loopback peer is exempt", resp.status, resp.header.Get("Location"))
	}
}

func TestGateWireRefusesPlainHTTPOnAnEnvTLSInstallWithoutAnHTTPSPublicURL(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no public URL":   {"TIERPLAN_TLS_SELF_SIGNED": "true"},
		"http public URL": {"TIERPLAN_TLS_SELF_SIGNED": "true", "TIERPLAN_PUBLIC_URL": "http://tierplan.example.com:7480"},
	} {
		t.Run(name, func(t *testing.T) {
			w := startWire(t, env, defaultFakePeer)
			plain := w.plainClient(t)
			// Sign-in, the setup wizard and the API are not reachable in clear.
			for _, target := range []string{"/", "/api/version", "/api/auth/login", "/api/setup/status", "/setup"} {
				resp := wireGet(t, plain, "http://"+w.addr+target, "")
				if resp.status != http.StatusBadRequest || resp.body != refusalBody {
					t.Errorf("plain GET %s: status = %d, body = %q, want 400 and the refusal", target, resp.status, resp.body)
				}
				if loc := resp.header.Get("Location"); loc != "" {
					t.Errorf("plain GET %s: Location = %q, want none", target, loc)
				}
			}
			for _, probe := range []string{"/healthz", "/readyz"} {
				if resp := wireGet(t, plain, "http://"+w.addr+probe, ""); resp.status != http.StatusOK {
					t.Errorf("plain GET %s: status = %d, want 200", probe, resp.status)
				}
			}
			resp := wireGet(t, w.tlsClient(t), "https://"+w.addr+"/api/version", "")
			if resp.status != http.StatusOK {
				t.Errorf("https GET /api/version: status = %d, want 200", resp.status)
			}
		})
	}
}
