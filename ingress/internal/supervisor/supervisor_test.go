// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package supervisor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/ingress/internal/cache"
	"github.com/ianglennon/tierplan/ingress/internal/channel"
	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/ingress/internal/identity"
	"github.com/ianglennon/tierplan/ingress/internal/ready"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// testInstance is the instance ID every test node is pinned to.
const testInstance = "inst-1"

// fixedNotBefore is the start of every test certificate's validity. No test asserts anything about the
// current time, so the certificates carry a fixed, long window instead of one read from a clock.
var fixedNotBefore = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// --- bundles -----------------------------------------------------------------------------------------------

// leaf is one certificate and key as bundle files, named by content as the renderer names them.
type leaf struct {
	certPath, keyPath string
	cert, key         []byte
}

func newLeaf(t *testing.T, host string) leaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the key for %s: %v", host, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("make a serial number: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             fixedNotBefore,
		NotAfter:              fixedNotBefore.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign the certificate for %s: %v", host, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key for %s: %v", host, err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return leaf{
		certPath: ingressbundle.CertsDir + "/" + ingressbundle.Checksum(chain) + ".crt",
		keyPath:  ingressbundle.CertsDir + "/" + ingressbundle.Checksum(keyPEM) + ".key",
		cert:     chain,
		key:      keyPEM,
	}
}

// dynamicFor renders a dynamic file shaped like the renderer's: tls.options.default with sniStrict and
// minVersion, one certificates pair per leaf by absolute path, and one websecure router per host.
func dynamicFor(sniStrict bool, hosts []string, leaves []leaf) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tls:\n  options:\n    default:\n      sniStrict: %t\n      minVersion: VersionTLS12\n", sniStrict)
	if len(leaves) > 0 {
		sb.WriteString("  certificates:\n")
		for _, l := range leaves {
			fmt.Fprintf(&sb, "    - certFile: %s/%s\n      keyFile: %s/%s\n",
				ingressbundle.NodeRoot, l.certPath, ingressbundle.NodeRoot, l.keyPath)
		}
	}
	if len(hosts) > 0 {
		sb.WriteString("http:\n  routers:\n")
		for i, h := range hosts {
			fmt.Fprintf(&sb, "    r-%d:\n      rule: Host(`%s`)\n      entryPoints:\n        - websecure\n      service: s-%d\n      priority: 1\n      tls: {}\n", i, h, i)
		}
		sb.WriteString("  services:\n")
		for i := range hosts {
			fmt.Fprintf(&sb, "    s-%d:\n      loadBalancer:\n        servers:\n          - url: http://web:8080\n", i)
		}
	}
	return []byte(sb.String())
}

// bundleFor builds a bundle over the given leaves and dynamic file.
func bundleFor(t *testing.T, config int64, dynamic []byte, leaves ...leaf) ingressbundle.Bundle {
	t.Helper()
	var files []ingressbundle.File
	for _, l := range leaves {
		files = append(files,
			ingressbundle.File{Path: l.certPath, Data: l.cert},
			ingressbundle.File{Path: l.keyPath, Data: l.key})
	}
	files = append(files, ingressbundle.File{Path: ingressbundle.DynamicFile, Data: dynamic})
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: config}, files, nil)
	if err != nil {
		t.Fatalf("build a bundle: %v", err)
	}
	return b
}

// validBundle is a bundle that passes Check, with one certificate and router for host.
func validBundle(t *testing.T, config int64, host string) ingressbundle.Bundle {
	t.Helper()
	l := newLeaf(t, host)
	return bundleFor(t, config, dynamicFor(true, []string{host}, []leaf{l}), l)
}

// refusedBundle is a bundle that decodes and verifies but fails Check (tls_options), and whose files hold a
// real certificate key.
func refusedBundle(t *testing.T, config int64, host string) ingressbundle.Bundle {
	t.Helper()
	l := newLeaf(t, host)
	return bundleFor(t, config, dynamicFor(false, []string{host}, []leaf{l}), l)
}

func frameFor(t *testing.T, instance string, b ingressbundle.Bundle) []byte {
	t.Helper()
	raw, err := json.Marshal(ingresswire.BundleEnvelope{T: ingresswire.TypeBundle, InstanceID: instance, Bundle: b})
	if err != nil {
		t.Fatalf("marshal the envelope: %v", err)
	}
	return raw
}

// --- fakes -------------------------------------------------------------------------------------------------

// events is an ordered record of what the supervisor did.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, kind)
}

// once records kind the first time only.
func (e *events) once(kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !slices.Contains(e.log, kind) {
		e.log = append(e.log, kind)
	}
}

func (e *events) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// eventFS is the real filesystem that notes the first of each step the supervisor takes on the volume.
type eventFS struct {
	fsx.FS
	ev *events
}

func (f eventFS) MkdirAll(path string, perm fs.FileMode) error {
	f.ev.once("prepare")
	return f.FS.MkdirAll(path, perm)
}

func (f eventFS) Rename(oldpath, newpath string) error {
	switch {
	case strings.HasSuffix(newpath, "/static/traefik.yml"):
		f.ev.once("static")
	case strings.HasSuffix(newpath, "/state/node.key"):
		f.ev.once("key")
	}
	return f.FS.Rename(oldpath, newpath)
}

func (f eventFS) ReadFile(name string) ([]byte, error) {
	if strings.HasSuffix(name, "/state/pending.json") {
		f.ev.once("load")
	}
	return f.FS.ReadFile(name)
}

type fakeTraefik struct {
	ev      *events
	starts  atomic.Int32
	stops   atomic.Int32
	running atomic.Bool
	onStop  func()
}

func (f *fakeTraefik) Start(context.Context) error {
	f.ev.add("traefik-start")
	f.starts.Add(1)
	f.running.Store(true)
	return nil
}

func (f *fakeTraefik) Stop() error {
	f.ev.add("traefik-stop")
	if f.onStop != nil {
		f.onStop()
	}
	f.stops.Add(1)
	f.running.Store(false)
	return nil
}

func (f *fakeTraefik) Running() bool { return f.running.Load() }

// syncBuffer is a bytes.Buffer a logger and a test can use from different goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a supervisor over a real volume in a temporary directory, with a real readiness listener on a
// loopback port and fakes for everything that would leave the process.
type harness struct {
	t       *testing.T
	root    string
	events  *events
	traefik *fakeTraefik
	state   *ready.State
	ping    atomic.Bool
	logs    *syncBuffer
	deps    Deps
	client  *http.Client

	addrMu sync.Mutex
	addr   string
}

func newHarness(t *testing.T, root string) *harness {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	h := &harness{t: t, root: root, events: &events{}, state: &ready.State{}, logs: &syncBuffer{}}
	h.traefik = &fakeTraefik{ev: h.events}
	fsys := eventFS{FS: fsx.OS(), ev: h.events}
	cfg, err := config.Load(func(k string) string {
		if k == config.EnvURL {
			return "wss://tierplan.example.test/api/ingress"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	h.client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(h.client.CloseIdleConnections)
	h.deps = Deps{
		Config:   cfg,
		Store:    cache.NewStore(fsys, root),
		Identity: identity.New(fsys, root),
		Traefik:  h.traefik,
		Ready:    h.state,
		Ping:     func(context.Context) bool { return h.ping.Load() },
		Listen: func(network, addr string) (net.Listener, error) {
			h.events.add("listen")
			ln, err := net.Listen(network, "127.0.0.1:0")
			if err == nil {
				h.addrMu.Lock()
				h.addr = ln.Addr().String()
				h.addrMu.Unlock()
			}
			return ln, err
		},
		ReadFile: func(name string) ([]byte, error) { return nil, fmt.Errorf("unexpected read of %s", name) },
		Hostname: func() (string, error) { return "node-a", nil },
		Version:  "v0.0.0-test",
		Log:      slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return h
}

// run starts Run in a goroutine and returns the channel its result arrives on.
func (h *harness) run(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h.deps) }()
	return done
}

// status is the readiness endpoint's status code, or the error of the request.
func (h *harness) status() (int, error) {
	h.addrMu.Lock()
	addr := h.addr
	h.addrMu.Unlock()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, "http://"+addr+ingressbundle.ReadyPath, nil)
	if err != nil {
		return 0, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func (h *harness) mustStatus(want int) {
	h.t.Helper()
	got, err := h.status()
	if err != nil {
		h.t.Fatalf("GET %s: %v", ingressbundle.ReadyPath, err)
	}
	if got != want {
		h.t.Fatalf("GET %s = %d, want %d", ingressbundle.ReadyPath, got, want)
	}
}

// waitChannel runs Run until the channel loop is entered and returns the options it was given.
func (h *harness) runUntilChannel(ctx context.Context) (channel.LoopOptions, <-chan error) {
	h.t.Helper()
	entered := make(chan channel.LoopOptions, 1)
	h.deps.RunChannel = func(ctx context.Context, lo channel.LoopOptions) error {
		h.events.add("channel")
		entered <- lo
		<-ctx.Done()
		return nil
	}
	done := h.run(ctx)
	select {
	case lo := <-entered:
		return lo, done
	case err := <-done:
		h.t.Fatalf("Run returned before the channel loop was entered: %v", err)
		return channel.LoopOptions{}, nil
	}
}

// --- tests -------------------------------------------------------------------------------------------------

func TestStartNoCacheThenFirstBundle(t *testing.T) {
	h := newHarness(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)

	// Nothing cached: the readiness listener answers 503, the layout and the static file are there, the
	// node has its key, and Traefik is not running.
	h.mustStatus(http.StatusServiceUnavailable)
	static, err := os.ReadFile(filepath.Join(h.root, "static", "traefik.yml"))
	if err != nil {
		t.Fatalf("read the static file: %v", err)
	}
	if !bytes.Equal(static, ingressbundle.StaticTraefik()) {
		t.Fatalf("static/traefik.yml is not ingressbundle.StaticTraefik()")
	}
	if _, err := os.Stat(filepath.Join(h.root, "state", "node.key")); err != nil {
		t.Fatalf("the node key was not made before the channel: %v", err)
	}
	if got := h.traefik.starts.Load(); got != 0 {
		t.Fatalf("Traefik started %d times with nothing cached", got)
	}
	if want, got := []string{"listen", "prepare", "static", "key", "load", "channel"}, h.events.snapshot(); !slices.Equal(want, got) {
		t.Fatalf("effects before the channel = %v, want %v", got, want)
	}

	// The first bundle from TierPlan: applied, Traefik started once, readiness follows the ping.
	b := validBundle(t, 1, "app.example.test")
	id, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, b), testInstance)
	if id != b.ID || reason != "" {
		t.Fatalf("Handle = (%q, %q), want (%q, \"\")", id, reason, b.ID)
	}
	if got := h.traefik.starts.Load(); got != 1 {
		t.Fatalf("Traefik started %d times after the first bundle, want 1", got)
	}
	if !h.state.Applied() {
		t.Fatalf("readiness is not marked applied after the first bundle")
	}
	h.mustStatus(http.StatusServiceUnavailable) // applied, but Traefik's ping does not answer yet
	h.ping.Store(true)
	h.mustStatus(http.StatusOK)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
