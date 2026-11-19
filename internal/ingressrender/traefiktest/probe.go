// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package traefiktest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// probeTimeout bounds one connection or request of a probe. A poll repeats a probe, so this only has
// to be longer than a loaded runner needs for one.
const probeTimeout = 30 * time.Second

// Handshake completes a TLS handshake with Traefik's websecure entrypoint, sending serverName as the
// SNI and verifying the certificate against roots. Verification is never switched off: a certificate
// that does not verify is an error, and PresentedSubject reads who it claimed to be.
//
// An empty serverName sends no SNI at all. Go refuses an empty name outright and never sends an IP
// literal as SNI, so the config names Traefik's own address, which gives a hello without the
// extension while the certificate is still verified.
func (tr *Traefik) Handshake(serverName string, roots *x509.CertPool) (tls.ConnectionState, error) {
	if serverName == "" {
		serverName = tr.ip
	}
	conn, err := dial(tr.ip, httpsPort)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	c := tls.Client(conn, &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12})
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	err = c.HandshakeContext(ctx)
	return c.ConnectionState(), err
}

// HandshakeIPSNI sends the IP literal ip as the SNI, which Go's client never does by itself. The
// config names a placeholder as long as ip, and the connection swaps it for ip in the ClientHello,
// the first thing written. It is valid for handshakes that must FAIL only: a rewritten hello breaks
// the Finished check of a handshake that would otherwise succeed, so a positive case must use
// Handshake with a real name. The pool is empty, so verification stays on.
func (tr *Traefik) HandshakeIPSNI(ip string) error {
	if ip == "" {
		return errors.New("traefiktest: HandshakeIPSNI needs an IP literal")
	}
	conn, err := dial(tr.ip, httpsPort)
	if err != nil {
		return err
	}
	placeholder := strings.Repeat("x", len(ip))
	rc := &rewriteConn{Conn: conn, placeholder: []byte(placeholder), real: []byte(ip)}
	c := tls.Client(rc, &tls.Config{ServerName: placeholder, RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12})
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return c.HandshakeContext(ctx)
}

// rewriteConn swaps a same-length placeholder for the real server name in the first write.
type rewriteConn struct {
	net.Conn
	placeholder, real []byte
	done              bool
}

func (c *rewriteConn) Write(b []byte) (int, error) {
	if c.done {
		return c.Conn.Write(b)
	}
	c.done = true
	out := bytes.Replace(b, c.placeholder, c.real, 1)
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	// The caller wrote len(b) bytes; the rewritten hello has the same length.
	return len(b), nil
}

// PresentedSubject is the common name of the certificate a server presented that did not verify, from
// the error of a handshake, or "" when the error carries none: the server sent no certificate (it
// refused with an alert), or the failure was not a verification failure.
func PresentedSubject(err error) string {
	var verr *tls.CertificateVerificationError
	if !errors.As(err, &verr) || len(verr.UnverifiedCertificates) == 0 {
		return ""
	}
	return verr.UnverifiedCertificates[0].Subject.CommonName
}

// TryGet requests https://host<pathAndQuery> from Traefik's websecure entrypoint and returns the
// status, the body and the headers, or the transport error. The connection always goes to Traefik,
// and the host gives both the SNI and the Host header; the certificate is verified against roots.
func (tr *Traefik) TryGet(roots *x509.CertPool, host, pathAndQuery string, header http.Header) (int, string, http.Header, error) {
	status, body, h, _, err := tr.TryGetFromIP(roots, host, pathAndQuery, header)
	return status, body, h, err
}

// TryGetFromIP is TryGet that also returns the IP address the connection was made from, which is the
// address Traefik sees as the client's. A test of forwarded headers compares them with it; it is "" when
// no connection was made.
func (tr *Traefik) TryGetFromIP(roots *x509.CertPool, host, pathAndQuery string, header http.Header) (status int, body string, h http.Header, clientIP string, err error) {
	tp := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(tr.ip, httpsPort))
			if err == nil {
				if ta, ok := conn.LocalAddr().(*net.TCPAddr); ok {
					clientIP = ta.IP.String()
				}
			}
			return conn, err
		},
		TLSClientConfig:   &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	status, body, h, err = do(tp, "https://"+host+pathAndQuery, header, true)
	return status, body, h, clientIP, err
}

// Get is TryGet that fails the test on a transport error.
func (tr *Traefik) Get(t testing.TB, roots *x509.CertPool, host, pathAndQuery string, header http.Header) (status int, body string, h http.Header) {
	t.Helper()
	status, body, h, err := tr.TryGet(roots, host, pathAndQuery, header)
	if err != nil {
		t.Fatalf("traefiktest: GET https://%s%s: %v", host, pathAndQuery, err)
	}
	return status, body, h
}

// HTTPGet requests http://host<pathAndQuery> from Traefik's web entrypoint without following a
// redirect, and returns the status and the Location header. host may carry a port, which then reaches
// Traefik in the Host header only.
func (tr *Traefik) HTTPGet(t testing.TB, host, pathAndQuery string) (status int, location string) {
	t.Helper()
	tp := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(tr.ip, httpPort))
		},
		DisableKeepAlives: true,
	}
	status, _, h, err := do(tp, "http://"+host+pathAndQuery, nil, false)
	if err != nil {
		t.Fatalf("traefiktest: GET http://%s%s: %v", host, pathAndQuery, err)
	}
	return status, h.Get("Location")
}

func do(tp *http.Transport, url string, header http.Header, followRedirects bool) (int, string, http.Header, error) {
	hc := &http.Client{Transport: tp, Timeout: probeTimeout}
	if !followRedirects {
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, "", nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, string(b), resp.Header, nil
}

func dial(ip, port string) (net.Conn, error) {
	d := net.Dialer{Timeout: probeTimeout}
	return d.Dial("tcp", net.JoinHostPort(ip, port))
}
