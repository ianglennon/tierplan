// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
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
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/registry"
	"github.com/ianglennon/coaming/proto/testsupport"
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
		Subject:               pkix.Name{CommonName: "coaming test registry"},
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
		"REGISTRY_AUTH_HTPASSWD_REALM": "coaming-test",
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

// push puts a one-layer image at repo:latest, or at each of tags, into the registry with the
// registry's own HTTP API, signed in with the fixture's login. The layer is a few bytes: a
// registry stores blobs without reading them, and nothing here pulls the image.
func push(t *testing.T, hc *http.Client, base, repo string, tags ...string) {
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
	cfgDigest, cfgSize := blob([]byte(`{"architecture":"amd64","os":"linux","created":"2026-01-02T03:04:05Z","rootfs":{"type":"layers","diff_ids":[]}}`))
	layerDigest, layerSize := blob([]byte("coaming test layer"))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]}`,
		cfgDigest, cfgSize, layerDigest, layerSize)
	if len(tags) == 0 {
		tags = []string{"latest"}
	}
	put := func(tag string) error {
		req, err := http.NewRequest(http.MethodPut, base+"/v2/"+repo+"/manifests/"+tag, strings.NewReader(manifest))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		req.SetBasicAuth(itUser, itPass)
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("%d %s", resp.StatusCode, b)
		}
		return nil
	}
	var g errgroup.Group
	g.SetLimit(8)
	for _, tag := range tags {
		g.Go(func() error { return put(tag) })
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("push the manifest: %v", err)
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

// waitUp waits until the registry answers a test at all. The registry runs in a real
// container, so this is the one wait that polls; a failure names the last step it saw.
func waitUp(t *testing.T, c checker) {
	t.Helper()
	testsupport.Poll(t, time.Minute, "the registry at "+c.host+" to answer (never answered)", func() (bool, string) {
		r := c.check(itUser, itPass, "")
		return r.Reachable.State == registry.StepPassed, fmt.Sprintf("reachable step %q, code %q", r.Reachable.State, r.Reachable.Code)
	})
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

// countingProxy is a TLS front for the registry that counts what reaches it, so the test can
// say how many requests the server had in flight at once.
type countingProxy struct {
	srv *httptest.Server

	inflight, maxInflight atomic.Int32
	requests              atomic.Int64
	manifestGets          atomic.Int64
}

func newCountingProxy(t *testing.T, upstream string) *countingProxy {
	t.Helper()
	target, err := url.Parse("http://" + upstream)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	p := &countingProxy{}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := p.inflight.Add(1)
		defer p.inflight.Add(-1)
		for {
			m := p.maxInflight.Load()
			if n <= m || p.maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		p.requests.Add(1)
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
			p.manifestGets.Add(1)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *countingProxy) host() string { return strings.TrimPrefix(p.srv.URL, "https://") }

func (p *countingProxy) egress() registry.EgressPolicy {
	pool := x509.NewCertPool()
	pool.AddCert(p.srv.Certificate())
	return registry.NewEgressPolicy(nil).WithRootCAs(pool)
}

// TestTagsAgainstRegistry lists and reads the details of the tags of a registry:2 that holds
// 2,000 of them (plan 05-19, IMG-03, D-20, D-21, T-05-75): the list comes in pages of 100 in the
// right order, and a request for the 25 tags on screen never has more than three requests in
// flight to the registry.
func TestTagsAgainstRegistry(t *testing.T) {
	const total = 2000
	host := startRegistry(t, nil, nil)
	waitUp(t, checker{host: host, insecure: true})
	tags := make([]string, total)
	for i := range tags {
		tags[i] = fmt.Sprintf("1.%d.0", i)
	}
	push(t, &http.Client{Timeout: time.Minute}, "http://"+host, itRepo, tags...)

	proxy := newCountingProxy(t, host)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := registry.NewTagService(nil, proxy.egress(), "integration", log)
	cred := &registry.ResolvedCredential{
		Candidate:   registry.Candidate{ID: "c1", Name: "integration"},
		HostOptions: registry.HostOptions{Host: proxy.host(), Username: itUser, Password: itPass},
		UpdatedAt:   time.Now(),
	}
	ref, err := registry.ParseRef(proxy.host() + "/" + itRepo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Paging: 20 pages of 100, newest version first, each tag once.
	var seen []string
	for page := range 20 {
		p, err := svc.List(ctx, cred, ref, "", true, page)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(p.Tags) != registry.TagPageSize || p.Total != total || p.More != (page < 19) || p.Capped {
			t.Fatalf("page %d: %d tags, total %d, more %v, capped %v", page, len(p.Tags), p.Total, p.More, p.Capped)
		}
		for _, tg := range p.Tags {
			seen = append(seen, tg.Name)
		}
	}
	for i, name := range seen {
		if want := fmt.Sprintf("1.%d.0", total-1-i); name != want {
			t.Fatalf("tag %d = %q, want %q: newest version first", i, name, want)
		}
	}
	if len(seen) != total {
		t.Fatalf("listed %d tags, want %d", len(seen), total)
	}

	// Details of the 25 rows on screen: every one is read, and the registry never sees more
	// than three requests at once.
	visible := seen[:25]
	before := proxy.requests.Load()
	got := svc.Details(ctx, cred, ref, visible, "amd64")
	if len(got) != 25 {
		t.Fatalf("details = %d, want 25", len(got))
	}
	for i, d := range got {
		if d.Tag != visible[i] || d.Code != "" || d.Digest == "" || len(d.Platforms) != 1 || d.Platforms[0].Arch != "amd64" ||
			d.Created == nil || d.Created.Format(time.RFC3339) != "2026-01-02T03:04:05Z" {
			t.Fatalf("detail %d = %+v", i, d)
		}
	}
	if proxy.requests.Load() == before {
		t.Fatal("details made no request to the registry")
	}
	if m := proxy.maxInflight.Load(); m > 3 {
		t.Errorf("most requests in flight = %d, want at most 3", m)
	}
	if !slices.Contains(visible, "1.1999.0") {
		t.Error("the newest tag is not among the rows on screen")
	}
}
