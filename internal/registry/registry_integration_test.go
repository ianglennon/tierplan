// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package registry_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/registry"
)

// The credential test against a real registry (plan 05-12, D-12, D-14): registry:2 with an
// htpasswd file on the host's engine, once over plain HTTP and once over TLS with a
// self-signed certificate. The registry is published on 127.0.0.1, which the egress policy
// allows, and the test process talks to it exactly as the server does.

const (
	itUser = "bot"
	itPass = "registry-pass-0123456789"
	itRepo = "acme/app"
)

var allPassed = registry.CheckResult{
	Reachable: registry.StepResult{State: registry.StepPassed},
	SignedIn:  registry.StepResult{State: registry.StepPassed},
	CanPull:   registry.StepResult{State: registry.StepPassed},
}

// selfSigned makes a certificate for 127.0.0.1 and localhost and returns it with its key, in
// PEM. It is its own authority, which is what a person pastes as the custom CA.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "bayplan test registry"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// startRegistry runs registry:2 with the htpasswd file (and the certificate, when given) on the
// host engine and returns its host:port on 127.0.0.1.
func startRegistry(t *testing.T, certPEM, keyPEM []byte) string {
	t.Helper()
	env := map[string]string{ //nolint:gosec // G101: names of the registry's own settings, not credentials
		"REGISTRY_AUTH":                "htpasswd",
		"REGISTRY_AUTH_HTPASSWD_REALM": "bayplan-test",
		"REGISTRY_AUTH_HTPASSWD_PATH":  "/auth/htpasswd",
	}
	files := []testcontainers.ContainerFile{{
		Reader: bytes.NewReader(dockertest.HtpasswdFor(itUser, itPass)), ContainerFilePath: "/auth/htpasswd", FileMode: 0o644,
	}}
	if certPEM != nil {
		env["REGISTRY_HTTP_TLS_CERTIFICATE"] = "/certs/tls.crt"
		env["REGISTRY_HTTP_TLS_KEY"] = "/certs/tls.key"
		files = append(files,
			testcontainers.ContainerFile{Reader: bytes.NewReader(certPEM), ContainerFilePath: "/certs/tls.crt", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: bytes.NewReader(keyPEM), ContainerFilePath: "/certs/tls.key", FileMode: 0o600},
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        dockertest.RegistryImage,
			ExposedPorts: []string{"5000/tcp"},
			Env:          env,
			Files:        files,
			WaitingFor:   wait.ForListeningPort("5000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start %s: %v", dockertest.RegistryImage, err)
	}
	port, err := ctr.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("find the registry port: %v", err)
	}
	return "127.0.0.1:" + port.Port()
}

// push puts a one-layer image at repo:latest into the registry with the registry's own HTTP
// API, signed in with the fixture's login. The layer is a few bytes: a registry stores blobs
// without reading them, and nothing here pulls the image.
func push(t *testing.T, hc *http.Client, base, repo string) {
	t.Helper()
	blob := func(content []byte) (digest string, size int) {
		sum := sha256.Sum256(content)
		digest = "sha256:" + hex.EncodeToString(sum[:])
		do := func(method, url string, body []byte) *http.Response {
			req, err := http.NewRequest(method, url, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.SetBasicAuth(itUser, itPass)
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, url, err)
			}
			return resp
		}
		resp := do(http.MethodPost, base+"/v2/"+repo+"/blobs/uploads/", nil)
		_ = resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusAccepted || loc == "" {
			t.Fatalf("start a blob upload: %d", resp.StatusCode)
		}
		if strings.HasPrefix(loc, "/") {
			loc = base + loc
		}
		sep := "?"
		if strings.Contains(loc, "?") {
			sep = "&"
		}
		resp = do(http.MethodPut, loc+sep+"digest="+digest, content)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("finish the blob upload: %d %s", resp.StatusCode, b)
		}
		return digest, len(content)
	}
	cfgDigest, cfgSize := blob([]byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
	layerDigest, layerSize := blob([]byte("bayplan test layer"))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]}`,
		cfgDigest, cfgSize, layerDigest, layerSize)
	req, err := http.NewRequest(http.MethodPut, base+"/v2/"+repo+"/manifests/latest", strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	req.SetBasicAuth(itUser, itPass)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("push the manifest: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push the manifest: %d %s", resp.StatusCode, b)
	}
}

// checker runs the same call the credential test routes make, with the options of one test.
type checker struct {
	host, caPEM string
	insecure    bool
}

func (c checker) check(user, pass, repo string) registry.CheckResult {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eg := registry.NewEgressPolicy(nil)
	signed := registry.HostOptions{Host: c.host, Username: user, Password: pass, CAPEM: c.caPEM, Insecure: c.insecure}
	anon := registry.HostOptions{Host: c.host, CAPEM: c.caPEM, Insecure: c.insecure}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return registry.Check(ctx, registry.NewClient(signed, eg, "integration", log), registry.NewClient(anon, eg, "integration", log),
		registry.CheckInput{HostOptions: signed, Type: registry.TypeGeneric, TestRepository: repo})
}

// waitUp waits until the registry answers a test at all.
func waitUp(t *testing.T, c checker) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if r := c.check(itUser, itPass, ""); r.Reachable.State == registry.StepPassed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry at %s never answered", c.host)
		}
		time.Sleep(time.Second)
	}
}

func expect(t *testing.T, what string, got registry.CheckResult, want registry.CheckResult) {
	t.Helper()
	if got != want {
		t.Errorf("%s: result = %+v, want %+v", what, got, want)
	}
}

func TestCredentialTestAgainstRegistry(t *testing.T) {
	step := func(state, code string) registry.StepResult { return registry.StepResult{State: state, Code: code} }

	t.Run("plain HTTP", func(t *testing.T) {
		host := startRegistry(t, nil, nil)
		off := checker{host: host}
		on := checker{host: host, insecure: true}
		waitUp(t, on)
		push(t, &http.Client{Timeout: time.Minute}, "http://"+host, itRepo)

		// The switch is off: a registry that answers plain HTTP to a TLS request is registry_http.
		got := off.check(itUser, itPass, itRepo)
		expect(t, "switch off", got, registry.CheckResult{
			Reachable: step(registry.StepFailed, registry.CodeRegistryHTTP),
			SignedIn:  step(registry.StepNotChecked, ""),
			CanPull:   step(registry.StepNotChecked, ""),
		})

		// The switch is on: HTTPS without verification is tried, then plain HTTP, and every step
		// passes for the right password and a repository that was pushed.
		expect(t, "switch on, right password, pushed repository", on.check(itUser, itPass, itRepo), allPassed)

		// A wrong password is refused at the sign-in.
		got = on.check(itUser, "not-the-password", itRepo)
		expect(t, "switch on, wrong password", got, registry.CheckResult{
			Reachable: step(registry.StepPassed, ""),
			SignedIn:  step(registry.StepFailed, registry.CodeSignInRefused),
			CanPull:   step(registry.StepNotChecked, ""),
		})

		// A repository nobody pushed is not found; the sign-in still passed.
		got = on.check(itUser, itPass, "acme/missing")
		expect(t, "switch on, missing repository", got, registry.CheckResult{
			Reachable: step(registry.StepPassed, ""),
			SignedIn:  step(registry.StepPassed, ""),
			CanPull:   step(registry.StepFailed, registry.CodeRepositoryNotFound),
		})
	})

	t.Run("TLS with a self-signed CA", func(t *testing.T) {
		certPEM, keyPEM := selfSigned(t)
		host := startRegistry(t, certPEM, keyPEM)
		withCA := checker{host: host, caPEM: string(certPEM)}
		waitUp(t, withCA)

		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(certPEM)
		push(t, &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}},
			"https://"+host, itRepo)

		// Without the CA the server does not trust the certificate.
		got := checker{host: host}.check(itUser, itPass, itRepo)
		expect(t, "no CA", got, registry.CheckResult{
			Reachable: step(registry.StepFailed, registry.CodeRegistryTLS),
			SignedIn:  step(registry.StepNotChecked, ""),
			CanPull:   step(registry.StepNotChecked, ""),
		})

		// With the credential's CA every step passes.
		expect(t, "with the CA", withCA.check(itUser, itPass, itRepo), allPassed)

		got = withCA.check(itUser, "not-the-password", itRepo)
		expect(t, "with the CA, wrong password", got, registry.CheckResult{
			Reachable: step(registry.StepPassed, ""),
			SignedIn:  step(registry.StepFailed, registry.CodeSignInRefused),
			CanPull:   step(registry.StepNotChecked, ""),
		})

		got = withCA.check(itUser, itPass, "acme/missing")
		expect(t, "with the CA, missing repository", got, registry.CheckResult{
			Reachable: step(registry.StepPassed, ""),
			SignedIn:  step(registry.StepPassed, ""),
			CanPull:   step(registry.StepFailed, registry.CodeRepositoryNotFound),
		})

		// The switch alone reaches it too, without the CA.
		expect(t, "the switch without the CA", checker{host: host, insecure: true}.check(itUser, itPass, itRepo), allPassed)
	})
}
