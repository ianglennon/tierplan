// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

// Package traefiktest runs a real Traefik under test: a digest-pinned container with the embedded
// static file, a bundle root bind-mounted where the proxy image expects it, and an echo backend, all
// on an --internal Docker network through testcontainers-go. The probes reach Traefik by its container
// IP, so no host port is published and two runners on one VM cannot collide.
//
// It is built for the render-and-serve tests of the ingress phases (21, 23 and 28). Test certificates
// come from a throwaway CA, never from an ACME server.
package traefiktest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

const (
	// TraefikImage is the proxy under test, pinned by its index digest. The tag is the release the
	// static file was written against; the digest is what makes the run repeatable.
	TraefikImage = "traefik:v3.7.19@sha256:aa942a00bce033c909bf1f7e861d93761be4b31a035f10d2410d33e38a8f41cf"
	// WhoamiImage is the echo backend: it prints the request line, the Host and every header it got,
	// and the container's hostname. Pinned by its index digest.
	WhoamiImage = "traefik/whoami:v1.10@sha256:1699d99cb4b9acc17f74ca670b3d8d0b7ba27c948b3445f0593b58ebece92f04"
	// NodeRoot is where the bundle root is mounted in the Traefik container: the node root of
	// schema 1. The static file reads NodeRoot/dynamic, and the YAML names certificates by absolute
	// paths under NodeRoot/certs.
	NodeRoot = "/var/lib/tierplan-ingress"
	// DefaultCertSubject is the common name of the certificate Traefik generates for itself and
	// presents when it has no better answer. A test that sees it has found a hole in strict SNI.
	DefaultCertSubject = "TRAEFIK DEFAULT CERT"

	// EnvEngine and EnvCIOwner are the variables the CI jobs set (dockertest.EnvEngine and
	// scripts/ci-clean.sh name the same ones).
	envEngine  = "TIERPLAN_TEST_ENGINE"
	envCIOwner = "TIERPLAN_CI_OWNER"
	ownerLabel = "tierplan.ci.owner"

	// The ports the static file opens inside the container.
	httpPort  = "8080"
	httpsPort = "8443"
	pingPort  = "8082"

	// startup bounds the pull and the start of a container.
	startup = 5 * time.Minute
)

// SkipUnlessEngineLeg skips the test unless it is the one CI leg that runs the Traefik harness. The
// harness talks to the runner's own Docker, not to the engine under test, so the six engine legs
// (24 to 29) would run identical tests; running it once, on 29 or with no leg set, is enough.
func SkipUnlessEngineLeg(t testing.TB) {
	t.Helper()
	if e := os.Getenv(envEngine); e != "" && e != "29" {
		t.Skipf("traefiktest: the Traefik harness runs on the engine 29 leg only (%s=%s)", envEngine, e)
	}
}

// Env is one test's isolated setup: an --internal network and a bundle root on the host.
type Env struct {
	nw     *testcontainers.DockerNetwork
	root   string
	labels map[string]string
}

// NewEnv creates the --internal network (no route out, so a proxy that called home would fail and
// could not be mistaken for one that did not try) and a bundle root with dynamic/, certs/ and tmp/.
// Both are removed when the test ends. When TIERPLAN_CI_OWNER is set the network and every container
// carry tierplan.ci.owner, so make ci-clean removes what a cancelled job left behind.
func NewEnv(t testing.TB) *Env {
	t.Helper()
	e := &Env{root: t.TempDir(), labels: map[string]string{}}
	if o := os.Getenv(envCIOwner); o != "" {
		e.labels[ownerLabel] = o
	}
	for _, d := range []string{"dynamic", "certs", "tmp"} {
		if err := os.Mkdir(filepath.Join(e.root, d), 0o755); err != nil { //nolint:gosec // a directory the container's root user reads; it holds only test material
			t.Fatalf("traefiktest: make %s: %v", d, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), startup)
	defer cancel()
	opts := []network.NetworkCustomizer{network.WithInternal()}
	if len(e.labels) > 0 {
		opts = append(opts, network.WithLabels(e.labels))
	}
	nw, err := network.New(ctx, opts...)
	if err != nil {
		t.Fatalf("traefiktest: create the internal network: %v", err)
	}
	testcontainers.CleanupNetwork(t, nw)
	e.nw = nw
	return e
}

// Root is the bundle root on the host.
func (e *Env) Root() string { return e.root }

// NetworkName is the name of the --internal network, for an inspection.
func (e *Env) NetworkName() string { return e.nw.Name }

// FileSpec is one file of a bundle: a path relative to the root, under certs/ or dynamic/.
type FileSpec struct {
	Path string
	Data []byte
	// Mode is the file's permission bits; zero means 0644.
	Mode os.FileMode
}

// Apply writes files the way a node supervisor must (D-02): every certs/ file first, then each
// dynamic/ file by writing it under tmp/, outside the watched directory and with an extension the
// file provider ignores, flushing it and renaming it into dynamic/. A reader of dynamic/ therefore
// never sees a half-written file, nor a certificate that is not there yet.
func (e *Env) Apply(t testing.TB, files []FileSpec) {
	t.Helper()
	var certs, dynamic []FileSpec
	for _, f := range files {
		clean := path.Clean(f.Path)
		switch {
		case clean != f.Path || strings.HasPrefix(clean, "../") || path.IsAbs(clean):
			t.Fatalf("traefiktest: %q is not a clean relative path", f.Path)
		case strings.HasPrefix(clean, "certs/") && !strings.Contains(strings.TrimPrefix(clean, "certs/"), "/"):
			certs = append(certs, f)
		case strings.HasPrefix(clean, "dynamic/") && !strings.Contains(strings.TrimPrefix(clean, "dynamic/"), "/"):
			dynamic = append(dynamic, f)
		default:
			t.Fatalf("traefiktest: %q is not a file directly under certs/ or dynamic/", f.Path)
		}
	}
	for _, f := range certs {
		if err := os.WriteFile(filepath.Join(e.root, filepath.FromSlash(f.Path)), f.Data, modeOf(f)); err != nil {
			t.Fatalf("traefiktest: write %s: %v", f.Path, err)
		}
	}
	for _, f := range dynamic {
		part := filepath.Join(e.root, "tmp", path.Base(f.Path)+".part")
		writeSynced(t, part, f.Data, modeOf(f))
		if err := os.Rename(part, filepath.Join(e.root, filepath.FromSlash(f.Path))); err != nil {
			t.Fatalf("traefiktest: rename %s into place: %v", f.Path, err)
		}
	}
}

// Remove deletes one file of the bundle by its path relative to the root.
func (e *Env) Remove(t testing.TB, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(e.root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("traefiktest: remove %s: %v", rel, err)
	}
}

func modeOf(f FileSpec) os.FileMode {
	if f.Mode == 0 {
		return 0o644
	}
	return f.Mode
}

func writeSynced(t testing.TB, name string, data []byte, mode os.FileMode) {
	t.Helper()
	fh, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // a path under the test's own temp directory
	if err != nil {
		t.Fatalf("traefiktest: create %s: %v", name, err)
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		t.Fatalf("traefiktest: write %s: %v", name, err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		t.Fatalf("traefiktest: sync %s: %v", name, err)
	}
	if err := fh.Close(); err != nil {
		t.Fatalf("traefiktest: close %s: %v", name, err)
	}
}

// TraefikOptions configure StartTraefik.
type TraefikOptions struct {
	// Static is the static configuration, normally ingressrender.Static(), which returns
	// ingressbundle.StaticTraefik().
	Static []byte
}

// Traefik is a running Traefik container.
type Traefik struct {
	ctr *testcontainers.DockerContainer
	ip  string
}

// StartTraefik runs the pinned Traefik on the environment's network with o.Static as its static file
// and the bundle root bind-mounted read-only at NodeRoot. It publishes no port. It returns when the
// ping entrypoint answers, so the proxy is up; whether it has read the dynamic directory yet is for
// the test to poll, because the file provider reloads asynchronously.
func (e *Env) StartTraefik(t testing.TB, o TraefikOptions) *Traefik {
	t.Helper()
	if len(o.Static) == 0 {
		t.Fatal("traefiktest: StartTraefik needs the static file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), startup)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    TraefikImage,
			Networks: []string{e.nw.Name},
			Labels:   e.containerLabels(),
			Files: []testcontainers.ContainerFile{
				{Reader: bytes.NewReader(o.Static), ContainerFilePath: "/etc/traefik/traefik.yml", FileMode: 0o644},
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = []string{e.root + ":" + NodeRoot + ":ro"}
			},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("traefiktest: start %s: %v", TraefikImage, err)
	}
	dc, ok := ctr.(*testcontainers.DockerContainer)
	if !ok || dc == nil {
		t.Fatalf("traefiktest: unexpected container type %T", ctr)
	}
	tr := &Traefik{ctr: dc}
	// Registered after the container's own clean-up, so it runs before it: a failed test shows
	// what Traefik logged, which is where it says why it refused a file.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("traefiktest: Traefik's log:\n%s", tr.rawLogs())
		}
	})
	ip, err := dc.ContainerIP(ctx)
	if err != nil || ip == "" {
		t.Fatalf("traefiktest: find Traefik's address: %q %v", ip, err)
	}
	tr.ip = ip

	testsupport.Poll(t, testsupport.Deadline, "Traefik's ping entrypoint to answer", func() (bool, string) { // testsupport: external, a real container starting
		status, err := pingStatus(ip)
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK, fmt.Sprintf("status %d", status)
	})
	return tr
}

func (e *Env) containerLabels() map[string]string {
	return maps.Clone(e.labels)
}

func pingStatus(ip string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // wall-clock: one request of a poll, whose own deadline is generous
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ip+":"+pingPort+"/ping", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode, nil
}

// IP is Traefik's address on the environment's network.
func (tr *Traefik) IP() string { return tr.ip }

// Container is the underlying container, for an inspection.
func (tr *Traefik) Container() *testcontainers.DockerContainer { return tr.ctr }

// Logs is everything Traefik has logged so far.
func (tr *Traefik) Logs(t testing.TB) string {
	t.Helper()
	out, err := tr.readLogs()
	if err != nil {
		t.Fatalf("traefiktest: read Traefik's log: %v", err)
	}
	return out
}

func (tr *Traefik) rawLogs() string {
	out, err := tr.readLogs()
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	return out
}

func (tr *Traefik) readLogs() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rd, err := tr.ctr.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rd.Close() }()
	b, err := io.ReadAll(rd)
	return string(b), err
}

// WhoamiOptions configure StartWhoami.
type WhoamiOptions struct {
	// Alias is the DNS name the backend answers to on the network, such as "shop_web".
	Alias string
	// Hostname is the container's own hostname, which whoami reports as "Hostname:". It tells two
	// backends apart.
	Hostname string
	// CertPEM and KeyPEM, when both are set, make the backend speak HTTPS on 443 with that pair.
	// Without them it speaks HTTP on 80.
	CertPEM, KeyPEM []byte
}

// Whoami is a running echo backend.
type Whoami struct {
	ctr *testcontainers.DockerContainer
}

// StartWhoami runs the pinned echo backend on the environment's network.
func (e *Env) StartWhoami(t testing.TB, o WhoamiOptions) *Whoami {
	t.Helper()
	if o.Alias == "" {
		t.Fatal("traefiktest: StartWhoami needs an alias")
	}
	req := testcontainers.ContainerRequest{
		Image:          WhoamiImage,
		Networks:       []string{e.nw.Name},
		NetworkAliases: map[string][]string{e.nw.Name: {o.Alias}},
		Labels:         e.containerLabels(),
	}
	if o.Hostname != "" {
		req.ConfigModifier = func(c *container.Config) { c.Hostname = o.Hostname }
	}
	if len(o.CertPEM) > 0 && len(o.KeyPEM) > 0 {
		req.Cmd = []string{"-port", "443", "-cert", "/tls/tls.crt", "-key", "/tls/tls.key"}
		req.Files = []testcontainers.ContainerFile{
			{Reader: bytes.NewReader(o.CertPEM), ContainerFilePath: "/tls/tls.crt", FileMode: 0o644},
			{Reader: bytes.NewReader(o.KeyPEM), ContainerFilePath: "/tls/tls.key", FileMode: 0o644},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), startup)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("traefiktest: start %s: %v", WhoamiImage, err)
	}
	dc, ok := ctr.(*testcontainers.DockerContainer)
	if !ok || dc == nil {
		t.Fatalf("traefiktest: unexpected container type %T", ctr)
	}
	return &Whoami{ctr: dc}
}

// Container is the underlying container.
func (w *Whoami) Container() *testcontainers.DockerContainer { return w.ctr }
