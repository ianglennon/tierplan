// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The container HEALTHCHECK runs `tierplan healthcheck`, which is Healthcheck with the certificate
// file HealthcheckCertFile names. A failing check restarts the container, so it must stay green in
// every TLS mode, with every public URL, and while either is switched at runtime (TLS-07).

const (
	matrixHTTPSURL      = "https://tierplan.example.com:8443"
	matrixOtherHTTPSURL = "https://other.example.com:9443"
)

// port is the port the server listens on, as the ":port" form the healthcheck reads from the
// listen address.
func (w *wireServer) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(w.addr)
	if err != nil {
		t.Fatal(err)
	}
	return ":" + port
}

// probe runs the healthcheck as the Docker HEALTHCHECK does for this install.
func (w *wireServer) probe(t *testing.T) error {
	t.Helper()
	return Healthcheck(t.Context(), w.port(t), HealthcheckCertFile(w.cfg))
}

// plainProbe runs the healthcheck over plain HTTP, which /healthz answers on every install.
func (w *wireServer) plainProbe(t *testing.T) error {
	t.Helper()
	return Healthcheck(t.Context(), w.port(t), "")
}

// modeEnv is the environment for one TLS mode. The certificate-file pair is written for host.
func modeEnv(t *testing.T, mode, host string) map[string]string {
	t.Helper()
	switch mode {
	case "self-signed":
		return map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true"}
	case "cert-file":
		dir := t.TempDir()
		crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
		writePair(t, crt, key, host)
		return map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key}
	}
	return map[string]string{}
}

func TestHealthcheckMatrix(t *testing.T) {
	for _, mode := range []string{"default", "self-signed", "cert-file"} {
		for name, pub := range map[string]string{"unset": "", "https": matrixHTTPSURL} {
			t.Run(mode+"/"+name+" public URL", func(t *testing.T) {
				env := modeEnv(t, mode, "tierplan.example.com")
				if pub != "" {
					env["TIERPLAN_PUBLIC_URL"] = pub
				}
				w := startWire(t, env, "")
				if err := w.probe(t); err != nil {
					t.Errorf("the container healthcheck: %v", err)
				}
				// /healthz is exempt from the gate, so a plain probe passes against every install.
				if err := w.plainProbe(t); err != nil {
					t.Errorf("a plain-HTTP probe: %v", err)
				}
			})
		}
	}
}

func TestHealthcheckMatrixIsGreenOverTLSFromFirstStart(t *testing.T) {
	w := startWire(t, nil, "")
	if err := w.probe(t); err != nil {
		t.Fatalf("a fresh install with no TLS variable: %v", err)
	}
	// The same port answers TLS, and /healthz is served over it to a client that trusts only the mirror.
	mirror, err := os.ReadFile(HealthcheckCertFile(w.cfg)) //nolint:gosec // the mirror under the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(mirror) {
		t.Fatal("the mirror holds no certificate")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}}
	t.Cleanup(tr.CloseIdleConnections)
	resp := wireGet(t, &http.Client{Transport: tr}, "https://"+w.addr+"/healthz", "")
	if resp.status != http.StatusOK {
		t.Errorf("https GET /healthz trusting only the mirror: status = %d, want 200", resp.status)
	}
}

func TestHealthcheckMatrixStaysGreenWhileThePublicURLFlips(t *testing.T) {
	w := startWire(t, map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true", "TIERPLAN_PUBLIC_URL": matrixHTTPSURL}, "")
	for _, pub := range []string{matrixHTTPSURL, matrixOtherHTTPSURL, "", matrixHTTPSURL} {
		w.srv.Instance().Apply("TierPlan", pub)
		if err := w.probe(t); err != nil {
			t.Errorf("public URL %q: the TLS probe: %v", pub, err)
		}
		if err := w.plainProbe(t); err != nil {
			t.Errorf("public URL %q: the plain probe: %v", pub, err)
		}
	}
}

func TestHealthcheckMatrixStillVerifiesTheCertificate(t *testing.T) {
	w := startWire(t, modeEnv(t, "cert-file", "tierplan.example.com"), "")
	if err := w.probe(t); err != nil {
		t.Fatalf("the install's own certificate: %v", err)
	}
	// Another certificate in the file is not the one the listener serves.
	dir := t.TempDir()
	other := filepath.Join(dir, "other.pem")
	writePair(t, other, filepath.Join(dir, "other.key"), "tierplan.example.com")
	if err := Healthcheck(t.Context(), w.port(t), other); err == nil {
		t.Error("a healthcheck trusting a different certificate passed")
	}
	// A file that cannot be read is an error that names it.
	missing := filepath.Join(t.TempDir(), "none.crt")
	if err := Healthcheck(context.Background(), w.port(t), missing); err == nil || !strings.Contains(err.Error(), "none.crt") {
		t.Errorf("a missing certificate file gave %v, want an error naming it", err)
	}
}
