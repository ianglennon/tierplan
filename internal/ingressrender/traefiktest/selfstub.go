// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package traefiktest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

const (
	// StubPort is the port the stub TierPlan listens on: TierPlan's default TIERPLAN_LISTEN port.
	StubPort = "7480"
	// StubAlias is the DNS name the stub answers to on the test network.
	StubAlias = "selfstub"
	// stubBase is the base of the stub's container and of the image SelfStubImage builds.
	stubBase = "alpine:3"
	// StubImageTag is the local image tag SelfStubImage builds.
	StubImageTag = "tierplan-selfstub:test"
)

var (
	buildOnce sync.Once
	builtPath string
	buildErr  error
)

// BuildSelfStub builds the stub binary for linux once per test process and returns its path. It is a
// static binary (CGO off) for the host's architecture, which is the architecture of the Docker engine
// the tests run containers on. The file lives under the system temp directory in a per-user folder and
// is replaced by each process's build, so it never accumulates; the Go build cache makes a rebuild
// quick. Nothing else uses it: it is a test helper and is never copied into a shipped image.
func BuildSelfStub(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() { builtPath, buildErr = buildSelfStub() })
	if buildErr != nil {
		t.Fatalf("traefiktest: build the stub TierPlan: %v", buildErr)
	}
	return builtPath
}

func buildSelfStub() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot find the source directory")
	}
	srcDir := filepath.Dir(thisFile)
	outDir := filepath.Join(os.TempDir(), "tierplan-selfstub-"+strconv.Itoa(os.Getuid()), runtime.GOARCH)
	if err := os.MkdirAll(outDir, 0o755); err != nil { //nolint:gosec // a build output folder under the temp directory
		return "", err
	}
	tmp, err := os.CreateTemp(outDir, "selfstub-*.tmp")
	if err != nil {
		return "", err
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", tmp.Name(), "./selfstub") //nolint:gosec // a fixed package of this repository
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./selfstub: %w\n%s", err, out)
	}
	final := filepath.Join(outDir, "selfstub")
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // the binary is executed by the container's user
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// SelfStubImage builds the local image tierplan-selfstub:test (alpine:3 with the stub binary at
// /selfstub) and returns its tag, for a test that loads it into a Docker-in-Docker engine. The
// certificate and key are not in it: the test supplies them, as the server's own image does not hold
// them either.
func SelfStubImage(t testing.TB) string {
	t.Helper()
	bin := BuildSelfStub(t)
	dir := t.TempDir()
	data, err := os.ReadFile(bin) //nolint:gosec // the binary BuildSelfStub just wrote
	if err != nil {
		t.Fatalf("traefiktest: read the stub binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "selfstub"), data, 0o755); err != nil { //nolint:gosec // the executable the image runs
		t.Fatalf("traefiktest: stage the stub binary: %v", err)
	}
	dockerfile := "FROM " + stubBase + "\nCOPY selfstub /selfstub\nENTRYPOINT [\"/selfstub\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil { //nolint:gosec // a build file in the test's own temp directory
		t.Fatalf("traefiktest: write the stub Dockerfile: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startup)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-q", "-t", StubImageTag, dir) //nolint:gosec // fixed arguments and a test-owned directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("traefiktest: docker build %s: %v\n%s", StubImageTag, err, out)
	}
	return StubImageTag
}

// SelfSignedLeaf makes a certificate in the shape of the one TierPlan generates for itself
// (internal/server signCertificate): ECDSA P-256, signed by its own key, CA:FALSE, digitalSignature and
// serverAuth only. The first name is the common name and all are DNS names. A proxy that verifies a
// backend must accept it as its own root.
func SelfSignedLeaf(t testing.TB, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("traefiktest: SelfSignedLeaf needs a name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("traefiktest: generate a key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"TierPlan"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("traefiktest: create the self-signed certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("traefiktest: marshal the key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// StubOptions configure StartSelfStub.
type StubOptions struct {
	// CertPEM and KeyPEM are the stub's TLS pair; the certificate's first DNS name is what a client
	// verifies it against.
	CertPEM, KeyPEM []byte
	// SSEEvery and PingEvery shorten the keepalives of /events and /ws; zero keeps the real ones
	// (15 s and 20 s).
	SSEEvery, PingEvery time.Duration
	// Trusted are the CIDR prefixes the stub's /client-ip treats as trusted proxies.
	Trusted []string
}

// Stub is a running stub TierPlan.
type Stub struct {
	ctr *testcontainers.DockerContainer
	ip  string
}

// StartSelfStub runs the stub on e's network with the alias "selfstub" and the
// environment's labels, and returns when /proto answers over verified TLS. Verification uses the stub's
// own certificate as the only root.
func StartSelfStub(t testing.TB, e *Env, o StubOptions) *Stub {
	t.Helper()
	if len(o.CertPEM) == 0 || len(o.KeyPEM) == 0 {
		t.Fatal("traefiktest: StartSelfStub needs a certificate and a key")
	}
	bin := BuildSelfStub(t)
	cmd := []string{"/selfstub", "-listen", ":" + StubPort, "-cert", "/tls/tls.crt", "-key", "/tls/tls.key"}
	if o.SSEEvery > 0 {
		cmd = append(cmd, "-sse-every", o.SSEEvery.String())
	}
	if o.PingEvery > 0 {
		cmd = append(cmd, "-ping-every", o.PingEvery.String())
	}
	req := testcontainers.ContainerRequest{
		Image:          stubBase,
		Cmd:            cmd,
		Networks:       []string{e.nw.Name},
		NetworkAliases: map[string][]string{e.nw.Name: {StubAlias}},
		Labels:         e.containerLabels(),
		Files: []testcontainers.ContainerFile{
			{HostFilePath: bin, ContainerFilePath: "/selfstub", FileMode: 0o755},
			{Reader: bytes.NewReader(o.CertPEM), ContainerFilePath: "/tls/tls.crt", FileMode: 0o644},
			{Reader: bytes.NewReader(o.KeyPEM), ContainerFilePath: "/tls/tls.key", FileMode: 0o644},
		},
	}
	if len(o.Trusted) > 0 {
		req.Env = map[string]string{"TIERPLAN_STUB_TRUSTED": strings.Join(o.Trusted, ",")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), startup)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("traefiktest: start the stub TierPlan: %v", err)
	}
	dc, ok := ctr.(*testcontainers.DockerContainer)
	if !ok || dc == nil {
		t.Fatalf("traefiktest: unexpected container type %T", ctr)
	}
	s := &Stub{ctr: dc}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("traefiktest: the stub's log:\n%s", s.rawLogs())
		}
	})
	ip, err := dc.ContainerIP(ctx)
	if err != nil || ip == "" {
		t.Fatalf("traefiktest: find the stub's address: %q %v", ip, err)
	}
	s.ip = ip

	leaf := firstCert(t, o.CertPEM)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	if len(leaf.DNSNames) == 0 {
		t.Fatal("traefiktest: the stub's certificate has no DNS name to verify")
	}
	testsupport.Poll(t, testsupport.Deadline, "the stub's /proto to answer", func() (bool, string) { // testsupport: external, a real container starting
		status, body, err := s.get(pool, leaf.DNSNames[0], "/proto")
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK, fmt.Sprintf("status %d %q", status, body)
	})
	return s
}

func firstCert(t testing.TB, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("traefiktest: no certificate in the PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("traefiktest: parse the certificate: %v", err)
	}
	return c
}

// get is one verified request straight to the stub, which is on the test network and reachable from
// the host by its container address.
func (s *Stub) get(roots *x509.CertPool, serverName, path string) (int, string, error) {
	tp := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(s.ip, StubPort))
		},
		TLSClientConfig:   &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
	}
	status, body, _, err := do(tp, "https://"+serverName+path, nil, false)
	return status, body, err
}

// Get requests path from the stub directly, verifying its certificate against certPEM (the stub's own),
// and fails the test on a transport error. It is how a test sees what the stub answers without the
// proxy in between.
func (s *Stub) Get(t testing.TB, certPEM []byte, path string) (status int, body string) {
	t.Helper()
	leaf := firstCert(t, certPEM)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	status, body, err := s.get(pool, leaf.DNSNames[0], path)
	if err != nil {
		t.Fatalf("traefiktest: GET https://%s%s: %v", s.ip, path, err)
	}
	return status, body
}

// IP is the stub's address on the environment's network.
func (s *Stub) IP() string { return s.ip }

// Container is the underlying container.
func (s *Stub) Container() *testcontainers.DockerContainer { return s.ctr }

// Logs is everything the stub has logged so far.
func (s *Stub) Logs(t testing.TB) string {
	t.Helper()
	out, err := s.readLogs()
	if err != nil {
		t.Fatalf("traefiktest: read the stub's log: %v", err)
	}
	return out
}

func (s *Stub) rawLogs() string {
	out, err := s.readLogs()
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	return out
}

func (s *Stub) readLogs() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rd, err := s.ctr.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rd.Close() }()
	b, err := io.ReadAll(rd)
	return string(b), err
}

// StreamClient is an HTTP client for requests that stay open (an event feed, a WebSocket): its
// connections go to Traefik's websecure entry point, the certificate is verified against roots for
// host, and it has no overall timeout, so the caller bounds a request with its context. It speaks
// HTTP/1.1, which a WebSocket upgrade needs.
func (tr *Traefik) StreamClient(roots *x509.CertPool, host string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(tr.ip, httpsPort))
		},
		TLSClientConfig:   &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}}
}
