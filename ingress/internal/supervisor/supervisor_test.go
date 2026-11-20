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
	mu      sync.Mutex
	log     []string
	waiters map[string]chan struct{}
}

func (e *events) add(kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, kind)
	e.notify(kind)
}

// notify closes the channel a waiter for kind holds. The caller holds e.mu.
func (e *events) notify(kind string) {
	if ch, ok := e.waiters[kind]; ok {
		close(ch)
		delete(e.waiters, kind)
	}
}

// wait blocks until kind has been recorded. It waits on a channel the recorder closes, not on the clock.
func (e *events) wait(t *testing.T, kind string) {
	t.Helper()
	e.mu.Lock()
	if slices.Contains(e.log, kind) {
		e.mu.Unlock()
		return
	}
	if e.waiters == nil {
		e.waiters = map[string]chan struct{}{}
	}
	ch, ok := e.waiters[kind]
	if !ok {
		ch = make(chan struct{})
		e.waiters[kind] = ch
	}
	e.mu.Unlock()
	<-ch
}

// once records kind the first time only.
func (e *events) once(kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !slices.Contains(e.log, kind) {
		e.log = append(e.log, kind)
		e.notify(kind)
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

// --- cache-first start, rejections, the token, shutdown ----------------------------------------------------

// cachedVolume makes a volume that already serves b, as an earlier run of the node would have left it.
func cachedVolume(t *testing.T, b ingressbundle.Bundle) string {
	t.Helper()
	root := t.TempDir()
	s := cache.NewStore(fsx.OS(), root)
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if id, reason := s.Accept(frameFor(t, testInstance, b), testInstance); id != b.ID || reason != "" {
		t.Fatalf("Accept = (%q, %q), want (%q, \"\")", id, reason, b.ID)
	}
	return root
}

// snapshot reads every file under the given subdirectories of root, keyed by its path relative to root.
func snapshot(t *testing.T, root string, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				return nil
			}
			data, err := os.ReadFile(p) //nolint:gosec // G304: a file in this test's own temporary volume
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(data)
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot %s: %v", d, err)
		}
	}
	return out
}

// errNoDial is what a fake Dial returns: the supervisor treats it as an attempt that failed.
var errNoDial = fmt.Errorf("no TierPlan in this test")

// dialRecord is what one call of the fake Dial saw.
type dialRecord struct {
	appliedID string
	creds     channel.Credentials
}

func TestStartGoodCache(t *testing.T) {
	b := validBundle(t, 1, "app.example.test")
	h := newHarness(t, cachedVolume(t, b))
	h.ping.Store(true)

	type seen struct {
		startsAtEntry int32
		status        int
		dial          dialRecord
	}
	got := make(chan seen, 1)
	h.deps.Dial = func(_ context.Context, o channel.Options, cr channel.Credentials) (*channel.Conn, error) {
		got <- seen{dial: dialRecord{appliedID: o.AppliedID(), creds: cr}}
		return nil, errNoDial
	}
	var entry seen
	h.deps.RunChannel = func(ctx context.Context, lo channel.LoopOptions) error {
		h.events.add("channel")
		entry.startsAtEntry = h.traefik.starts.Load()
		entry.status, _ = h.status()
		if _, err := lo.Dial(ctx); err != errNoDial {
			return fmt.Errorf("the dial closure returned %v, want the fake's error", err)
		}
		entry.dial = (<-got).dial
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.run(ctx)
	waitUntil(t, h.events, "channel")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if entry.startsAtEntry != 1 {
		t.Fatalf("Traefik had been started %d times when the channel loop was entered, want 1 (cache first)", entry.startsAtEntry)
	}
	if entry.status != http.StatusOK {
		t.Fatalf("readiness before any dial = %d, want 200", entry.status)
	}
	if entry.dial.appliedID != b.ID {
		t.Fatalf("hello's applied ID = %q, want the cached bundle's %q", entry.dial.appliedID, b.ID)
	}
	if want := []string{"traefik-start", "channel"}; !slices.Equal(tail(h.events.snapshot(), 2, "traefik-start"), want) {
		t.Fatalf("Traefik did not start before the channel: %v", h.events.snapshot())
	}
}

func TestStartCorruptCache(t *testing.T) {
	b := validBundle(t, 1, "app.example.test")
	root := cachedVolume(t, b)

	// One byte of the certificate on the volume changes: the record no longer verifies.
	var crt string
	for p := range snapshot(t, root, "certs") {
		if strings.HasSuffix(p, ".crt") {
			crt = filepath.Join(root, p)
		}
	}
	data, err := os.ReadFile(crt) //nolint:gosec // G304: a file in this test's own temporary volume
	if err != nil {
		t.Fatalf("read the cached certificate: %v", err)
	}
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(crt, data, 0o600); err != nil { //nolint:gosec // G703: a file in this test's own temporary volume
		t.Fatalf("damage the cached certificate: %v", err)
	}
	before := snapshot(t, root, "certs", "dynamic", "state", "tmp")
	delete(before, filepath.Join("state", "node.key"))

	h := newHarness(t, root)
	h.ping.Store(true)
	dialled := make(chan struct{}, 1)
	h.deps.Dial = func(context.Context, channel.Options, channel.Credentials) (*channel.Conn, error) {
		dialled <- struct{}{}
		return nil, errNoDial
	}
	var status int
	h.deps.RunChannel = func(ctx context.Context, lo channel.LoopOptions) error {
		h.events.add("channel")
		status, _ = h.status()
		_, _ = lo.Dial(ctx)
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.run(ctx)
	waitUntil(t, h.events, "channel")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := h.traefik.starts.Load(); got != 0 {
		t.Fatalf("Traefik started %d times from a cache that does not verify", got)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("readiness with a corrupt cache = %d, want 503", status)
	}
	select {
	case <-dialled:
	default:
		t.Fatalf("the supervisor did not dial TierPlan with a corrupt cache")
	}
	after := snapshot(t, root, "certs", "dynamic", "state", "tmp")
	delete(after, filepath.Join("state", "node.key"))
	if len(before) != len(after) {
		t.Fatalf("files under the root: %d before, %d after", len(before), len(after))
	}
	for p, was := range before {
		if now, ok := after[p]; !ok || now != was {
			t.Fatalf("%s changed (present after: %t); a cache that fails verification is left byte-identical", p, ok)
		}
	}
}

func TestRejectedBundleKeepsServing(t *testing.T) {
	good := validBundle(t, 1, "app.example.test")
	root := cachedVolume(t, good)
	h := newHarness(t, root)
	h.ping.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)

	before := snapshot(t, root, "certs", "dynamic")
	bad := refusedBundle(t, 2, "other.example.test")
	id, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, bad), testInstance)
	if id != bad.ID || reason != ingressbundle.ReasonTLSOptions {
		t.Fatalf("Handle = (%q, %q), want (%q, %q)", id, reason, bad.ID, ingressbundle.ReasonTLSOptions)
	}

	if starts, stops := h.traefik.starts.Load(), h.traefik.stops.Load(); starts != 1 || stops != 0 {
		t.Fatalf("Traefik starts=%d stops=%d after a rejected bundle, want 1 and 0 (neither touched)", starts, stops)
	}
	if got := h.deps.Store.AppliedID(); got != good.ID {
		t.Fatalf("applied ID = %q after a rejection, want the unchanged %q", got, good.ID)
	}
	h.mustStatus(http.StatusOK)
	after := snapshot(t, root, "certs", "dynamic")
	if len(before) != len(after) {
		t.Fatalf("certs/ and dynamic/ hold %d files before, %d after a rejection", len(before), len(after))
	}
	for p, was := range before {
		if after[p] != was {
			t.Fatalf("%s changed on a rejected bundle", p)
		}
	}
	logged := h.logs.String()
	if !strings.Contains(logged, `"msg":"bundle rejected"`) || !strings.Contains(logged, `"level":"WARN"`) ||
		!strings.Contains(logged, `"reason":"`+ingressbundle.ReasonTLSOptions+`"`) {
		t.Fatalf("no warn line `bundle rejected` with the reason in the log:\n%s", logged)
	}
	if strings.Contains(logged, `"msg":"bundle applied"`) {
		t.Fatalf("a rejected bundle was logged as applied")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAppliedBundleIsLoggedWithItsIDOnly(t *testing.T) {
	h := newHarness(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)
	b := validBundle(t, 1, "app.example.test")
	if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, b), testInstance); reason != "" {
		t.Fatalf("Handle: %q", reason)
	}
	logged := h.logs.String()
	if !strings.Contains(logged, `"msg":"bundle applied"`) || !strings.Contains(logged, `"id":"`+b.ID+`"`) {
		t.Fatalf("no `bundle applied` line with the ID in the log:\n%s", logged)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestTokenReadOnlyWhenNotEnrolled(t *testing.T) {
	const tokenPath = "/run/secrets/ingress-join-token"
	const token = "tpit_e-1_notarealtoken" //nolint:gosec // G101: a fake token the test searches the logs for

	setup := func(t *testing.T, root string) (h *harness, reads *[]string, tokenCalls *int, creds *[]channel.Credentials) {
		h = newHarness(t, root)
		cfg, err := config.Load(func(k string) string {
			switch k {
			case config.EnvURL:
				return "wss://tierplan.example.test/api/ingress"
			case config.EnvJoinTokenFile:
				return tokenPath
			}
			return ""
		})
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		h.deps.Config = cfg
		reads, tokenCalls, creds = new([]string), new(int), new([]channel.Credentials)
		h.deps.ReadFile = func(name string) ([]byte, error) {
			*reads = append(*reads, name)
			return []byte(token + "\n"), nil
		}
		// The fake does what the real Dial does: it asks for the token in enrol mode only, and reports the
		// welcome to OnEnrolled before it fails.
		h.deps.Dial = func(_ context.Context, o channel.Options, cr channel.Credentials) (*channel.Conn, error) {
			*creds = append(*creds, cr)
			if cr.NodeID == "" {
				*tokenCalls++
				if got, err := cr.Token(); err != nil || got != token {
					t.Errorf("Token = (%q, %v), want the token from the file", got, err)
				}
				w := ingresswire.Welcome{NodeID: "n-1", EnvID: "e-1", InstanceID: testInstance}
				if err := o.OnEnrolled(w); err != nil {
					t.Errorf("OnEnrolled: %v", err)
				}
			}
			return nil, errNoDial
		}
		return h, reads, tokenCalls, creds
	}
	dialTwice := func(h *harness) {
		h.deps.RunChannel = func(ctx context.Context, lo channel.LoopOptions) error {
			h.events.add("channel")
			_, _ = lo.Dial(ctx)
			_, _ = lo.Dial(ctx)
			<-ctx.Done()
			return nil
		}
	}
	runIt := func(t *testing.T, h *harness) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.run(ctx)
		waitUntil(t, h.events, "channel")
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	t.Run("enrolled", func(t *testing.T) {
		root := t.TempDir()
		if err := cache.NewStore(fsx.OS(), root).Prepare(); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		rec := identity.Record{NodeID: "n-1", EnvID: "e-1", InstanceID: testInstance}
		if err := identity.New(fsx.OS(), root).Save(rec); err != nil {
			t.Fatalf("Save: %v", err)
		}
		h, reads, tokenCalls, creds := setup(t, root)
		dialTwice(h)
		runIt(t, h)
		if *tokenCalls != 0 || len(*reads) != 0 {
			t.Fatalf("an enrolled node asked for the token %d times and read %v", *tokenCalls, *reads)
		}
		if len(*creds) != 2 {
			t.Fatalf("dials = %d, want 2", len(*creds))
		}
		for _, cr := range *creds {
			if cr.NodeID != rec.NodeID || cr.InstanceID != rec.InstanceID {
				t.Fatalf("an enrolled node dialled as (%q, %q), want its saved (%q, %q)", cr.NodeID, cr.InstanceID, rec.NodeID, rec.InstanceID)
			}
			if len(cr.Key) == 0 {
				t.Fatalf("an enrolled node dialled without its key")
			}
		}
	})

	t.Run("not enrolled", func(t *testing.T) {
		h, reads, tokenCalls, creds := setup(t, "")
		dialTwice(h)
		runIt(t, h)
		if *tokenCalls != 1 || len(*reads) != 1 || (*reads)[0] != tokenPath {
			t.Fatalf("token asked for %d times, files read %v; want once, from %s", *tokenCalls, *reads, tokenPath)
		}
		rec, ok, err := h.deps.Identity.Load()
		if err != nil || !ok || rec != (identity.Record{NodeID: "n-1", EnvID: "e-1", InstanceID: testInstance}) {
			t.Fatalf("saved identity = (%+v, %t, %v), want the welcome's record", rec, ok, err)
		}
		if len(*creds) != 2 {
			t.Fatalf("dials = %d, want 2", len(*creds))
		}
		if first := (*creds)[0]; first.NodeID != "" || first.Name != "node-a" {
			t.Fatalf("the first dial was (node %q, name %q), want an enrolment named node-a", first.NodeID, first.Name)
		}
		if second := (*creds)[1]; second.NodeID != "n-1" || second.InstanceID != testInstance {
			t.Fatalf("the dial after enrolment was (%q, %q), want connect mode with the saved identity", second.NodeID, second.InstanceID)
		}
	})
}

func TestShutdownOrder(t *testing.T) {
	good := validBundle(t, 1, "app.example.test")
	h := newHarness(t, cachedVolume(t, good))
	h.ping.Store(true)
	var listenerOpenAtStop atomic.Bool
	h.traefik.onStop = func() {
		// The channel has ended and Traefik is stopping; the readiness listener must still answer.
		if code, err := h.status(); err == nil && code == http.StatusOK {
			listenerOpenAtStop.Store(true)
		}
	}
	h.deps.RunChannel = func(ctx context.Context, _ channel.LoopOptions) error {
		h.events.add("channel")
		<-ctx.Done()
		h.events.add("channel-returned")
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.run(ctx)
	waitUntil(t, h.events, "channel")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil after an orderly stop", err)
	}

	ev := h.events.snapshot()
	if want := []string{"channel", "channel-returned", "traefik-stop"}; !slices.Equal(tail(ev, 3, "channel"), want) {
		t.Fatalf("shutdown order = %v, want %v", ev, want)
	}
	if !listenerOpenAtStop.Load() {
		t.Fatalf("the readiness listener was already closed when Traefik was stopped")
	}
	if _, err := h.status(); err == nil {
		t.Fatalf("the readiness listener still answers after Run returned")
	}
}

func TestHealthReportsPingAndAppliedID(t *testing.T) {
	good := validBundle(t, 1, "app.example.test")
	h := newHarness(t, cachedVolume(t, good))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)

	for _, ping := range []bool{false, true} {
		h.ping.Store(ping)
		ok, id := lo.Session.Health(ctx)
		if ok != ping || id != good.ID {
			t.Fatalf("Health with ping=%t = (%t, %q), want (%t, %q)", ping, ok, id, ping, good.ID)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// tail returns the last n events from the first occurrence of from on, so a test can look at the part of the
// log it is about without the listen, prepare, static, key and load entries before it.
func tail(ev []string, n int, from string) []string {
	i := slices.Index(ev, from)
	if i < 0 {
		return nil
	}
	ev = ev[i:]
	if len(ev) > n {
		ev = ev[:n]
	}
	return ev
}

// waitUntil blocks until the supervisor has recorded kind (a signal, never the clock).
func waitUntil(t *testing.T, ev *events, kind string) {
	t.Helper()
	ev.wait(t, kind)
}
