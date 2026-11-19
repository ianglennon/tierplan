// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Spike S1, second half, reload cases (D-01, D-02): what the pinned Traefik does when its dynamic
// directory changes. The fixtures are built from constants; the only formatted values are loop indexes.
// Each test applies files the way Phase 23's supervisor must (Env.Apply: certificates first, the YAML
// last by rename from tmp/) and polls the asynchronous reload with a bound, never a sleep.

const (
	// s1ReloadOptions is the TLS options block every fixture of this file carries.
	s1ReloadOptions = "tls:\n  options:\n    default:\n      sniStrict: true\n      minVersion: VersionTLS12\n"
	// s1ReloadService is the one backend the reload fixtures route to.
	s1ReloadService = "  services:\n    s-a:\n      loadBalancer:\n        servers:\n          - url: http://shop_web:80\n"
)

// s1Rule is the rule form the renderer emits for a path: the exact path or anything under it.
func s1Rule(host, p string) string {
	return "Host(`" + host + "`) && (Path(`" + p + "`) || PathPrefix(`" + p + "/`))"
}

// s1Router is one router entry under http.routers, on websecure with TLS, to service svc.
func s1Router(name, rule, svc string) string {
	return "    " + name + ":\n      rule: " + rule + "\n      entryPoints:\n        - websecure\n      service: " + svc + "\n      tls: {}\n"
}

// s1Doc is a whole dynamic file: strict SNI, the certificate named certName (absolute paths under the
// node root; empty writes none), the routers and the one service.
func s1Doc(certName string, routers ...string) string {
	var sb strings.Builder
	sb.WriteString(s1ReloadOptions)
	if certName != "" {
		sb.WriteString("  certificates:\n    - certFile: " + traefiktest.NodeRoot + "/certs/" + certName + ".crt\n      keyFile: " +
			traefiktest.NodeRoot + "/certs/" + certName + ".key\n")
	}
	sb.WriteString("http:\n  routers:\n")
	for _, r := range routers {
		sb.WriteString(r)
	}
	sb.WriteString(s1ReloadService)
	return sb.String()
}

// s1CertFiles are the certificate files of a bundle, to be applied before its YAML.
func s1CertFiles(name string, chain, key []byte) []traefiktest.FileSpec {
	return []traefiktest.FileSpec{
		{Path: "certs/" + name + ".crt", Data: chain},
		{Path: "certs/" + name + ".key", Data: key, Mode: 0o600},
	}
}

func s1Dynamic(doc string) traefiktest.FileSpec {
	return traefiktest.FileSpec{Path: "dynamic/tierplan.yml", Data: []byte(doc)}
}

// statusOf is the answer of a request, or 0 with the transport error.
func (s *s1Site) statusOf(host, pathAndQuery string) (int, error) {
	status, _, _, err := s.tr.TryGet(s.ca.Pool(), host, pathAndQuery, nil)
	return status, err
}

// awaitStatus polls until the request answers want.
func (s *s1Site) awaitStatus(t *testing.T, host, pathAndQuery string, want int) {
	t.Helper()
	testsupport.Poll(t, testsupport.Deadline, "GET "+host+pathAndQuery+" to answer "+strconv.Itoa(want), func() (bool, string) { // testsupport: external, Traefik reloads on its own schedule
		status, err := s.statusOf(host, pathAndQuery)
		if err != nil {
			return false, err.Error()
		}
		return status == want, "status " + strconv.Itoa(status)
	})
}

// TestS1BurstOf50RoutesInOneReload is the tracer of the second half (D-01, O11): a dynamic file with
// 50 routers renamed over a file with one is served in full by one reload. One certificate carries the
// 50 hostnames as SANs, so the fixture is one certificate, one backend and two files.
func TestS1BurstOf50RoutesInOneReload(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const n = 50
	hosts := make([]string, n)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%02d.burst.test", i)
	}
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	chain, key := ca.Leaf(t, hosts...)
	routers := func(count int) string {
		var sb strings.Builder
		for i := range count {
			sb.WriteString(s1Router(fmt.Sprintf("r-h%02d", i), s1Rule(hosts[i], "/shop"), "s-a"))
		}
		return s1Doc("burst", sb.String())
	}

	s := startS1Files(t, env, ca, append(s1CertFiles("burst", chain, key), s1Dynamic(routers(1))))
	s.awaitGet(t, hosts[0], "/shop")
	// Before the burst only the first hostname has a route.
	if status, err := s.statusOf(hosts[1], "/shop"); err != nil || status != http.StatusNotFound {
		t.Fatalf("before the burst %s answered %d (error %v), want 404", hosts[1], status, err)
	}

	env.Apply(t, []traefiktest.FileSpec{s1Dynamic(routers(n))})
	polls := 0
	testsupport.Poll(t, testsupport.Deadline, "all 50 routes to answer 200 after one rename", func() (bool, string) { // testsupport: external, Traefik reloads on its own schedule
		polls++
		for _, h := range hosts {
			status, err := s.statusOf(h, "/shop")
			if err != nil {
				return false, h + ": " + err.Error()
			}
			if status != http.StatusOK {
				return false, h + ": status " + strconv.Itoa(status)
			}
		}
		return true, ""
	})
	t.Logf("the 50-router file was served in full after %d poll(s) of all 50 hostnames (O11)", polls)

	// The final state is 50 of 50, asserted once more outside the poll.
	served := 0
	for _, h := range hosts {
		status, body, _ := s.tr.Get(t, ca.Pool(), h, "/shop", nil)
		if status == http.StatusOK && strings.Contains(body, "Hostname: shop-web") {
			served++
		}
	}
	if served != n {
		t.Errorf("%d of %d routes served after the reload", served, n)
	}
}

// s1Hammer is a client that keeps requesting one route for as long as a test runs, and counts what
// went wrong. It runs in a goroutine, so it records failures and never calls t.Fatal.
type s1Hammer struct {
	stop     chan struct{}
	done     chan struct{}
	ticks    chan struct{}
	requests atomic.Int64
	bad      atomic.Int64
	once     sync.Once
	mu       sync.Mutex
	firstBad string
}

func (s *s1Site) hammer(t *testing.T, host, pathAndQuery string) *s1Hammer {
	t.Helper()
	h := &s1Hammer{stop: make(chan struct{}), done: make(chan struct{}), ticks: make(chan struct{}, 1)}
	go func() {
		defer close(h.done)
		for {
			select {
			case <-h.stop:
				return
			default:
			}
			status, err := s.statusOf(host, pathAndQuery)
			h.requests.Add(1)
			if err != nil || status != http.StatusOK {
				h.bad.Add(1)
				h.mu.Lock()
				if h.firstBad == "" {
					h.firstBad = fmt.Sprintf("status %d, error %v", status, err)
				}
				h.mu.Unlock()
			}
			select {
			case h.ticks <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(h.close)
	return h
}

func (h *s1Hammer) close() {
	h.once.Do(func() { close(h.stop) })
	<-h.done
}

// afterNow returns once the client has completed a request that began after the call: the first tick
// may come from a request already in flight, the second cannot.
func (h *s1Hammer) afterNow(t *testing.T) {
	t.Helper()
	select {
	case <-h.ticks:
	default:
	}
	testsupport.Await(t, "the client's request in flight to finish", h.ticks)
	testsupport.Await(t, "the client's next request to finish", h.ticks)
}

// TestS1RenameOverUnderLoad proves a reload never flaps a route that stays (D-01, O12, edge
// RTE-03/concurrency): while a client keeps requesting /stable, which is in both of two bundles, the
// files are renamed over each other 40 times and the client sees no answer but 200. The load is
// bounded by the rename count; after each rename the test waits until the client has completed a
// request that began after it, so every rename is overlapped by traffic without a sleep.
func TestS1RenameOverUnderLoad(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const (
		host    = "stable.test"
		renames = 40
		extras  = 29
	)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	chain, key := ca.Leaf(t, host)
	stable := s1Router("r-stable", s1Rule(host, "/stable"), "s-a")
	bundleA := s1Doc("stable", stable)
	bundleB := func(marker bool) string {
		var sb strings.Builder
		sb.WriteString(stable)
		for i := range extras {
			sb.WriteString(s1Router(fmt.Sprintf("r-x%02d", i), s1Rule(host, fmt.Sprintf("/x%02d", i)), "s-a"))
		}
		if marker {
			sb.WriteString(s1Router("r-final", s1Rule(host, "/final"), "s-a"))
		}
		return s1Doc("stable", sb.String())
	}

	s := startS1Files(t, env, ca, append(s1CertFiles("stable", chain, key), s1Dynamic(bundleA)))
	s.awaitGet(t, host, "/stable")
	h := s.hammer(t, host, "/stable")
	h.afterNow(t)

	for i := 1; i <= renames; i++ {
		doc := bundleA
		switch {
		case i == renames:
			// The last rename carries a router no earlier bundle had, so the test can tell when the
			// final reload has happened and not an earlier bundle B.
			doc = bundleB(true)
		case i%2 == 1:
			doc = bundleB(false)
		}
		env.Apply(t, []traefiktest.FileSpec{s1Dynamic(doc)})
		h.afterNow(t)
	}
	s.awaitStatus(t, host, "/final", http.StatusOK)
	h.afterNow(t)
	h.close()

	requests, bad := h.requests.Load(), h.bad.Load()
	t.Logf("%d renames, %d requests to /stable, %d not 200 (O12)", renames, requests, bad)
	if requests < renames {
		t.Errorf("only %d requests were made across %d renames", requests, renames)
	}
	if bad != 0 {
		h.mu.Lock()
		first := h.firstBad
		h.mu.Unlock()
		t.Errorf("%d of %d requests to /stable were not 200 during the renames (the first: %s)", bad, requests, first)
	}
}

// TestS1LeftoverTempFile proves why a temporary file must live outside the watched directory (D-02,
// T-21-11, O13). Written straight into dynamic/ the way a crashed writer would leave them:
//   - x.tmp.yml defines a new router and is parsed and served, because the file provider reads every
//     .yml file in the directory;
//   - y.tmp.yml redefines an existing router name: the first definition wins (files read in name
//     order, tierplan.yml first) and the log says it skipped the second;
//   - zz.part is ignored: its extension is not one the provider reads, and Traefik keeps serving.
//
// D-02's conclusion: the supervisor writes its temporary file under tmp/ with a name that does not end
// in .yml, .yaml or .toml, then renames it into dynamic/. A leftover in dynamic/ would be configuration.
func TestS1LeftoverTempFile(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const host = "leftover.test"
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	chain, key := ca.Leaf(t, host)
	base := s1Router("r-base", s1Rule(host, "/base"), "s-a")
	baseDoc := s1Doc("left", base)

	s := startS1Files(t, env, ca, append(s1CertFiles("left", chain, key), s1Dynamic(baseDoc)))
	s.awaitGet(t, host, "/base")
	s.awaitStatus(t, host, "/leftover", http.StatusNotFound)

	direct := func(name, body string) {
		t.Helper()
		p := filepath.Join(env.Root(), "dynamic", name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil { //nolint:gosec // a file under the test's own temp directory, read by the container's root user
			t.Fatalf("write %s into the watched directory: %v", name, err)
		}
	}
	httpDoc := func(routers string) string { return "http:\n  routers:\n" + routers }

	// A new router name in a leftover file is served.
	direct("x.tmp.yml", httpDoc(s1Router("r-leftover", s1Rule(host, "/leftover"), "s-a")))
	s.awaitGet(t, host, "/leftover")

	// A redefined router name is skipped: the original rule keeps serving, the other never does.
	direct("y.tmp.yml", httpDoc(s1Router("r-base", s1Rule(host, "/hijack"), "s-a")))
	testsupport.Poll(t, testsupport.Deadline, "Traefik to log that it skipped the redefined router", func() (bool, string) { // testsupport: external, a real container's log
		return strings.Contains(s.tr.Logs(t), "already configured"), "no such line yet"
	})
	s.awaitGet(t, host, "/base")
	if status, err := s.statusOf(host, "/hijack"); err != nil || status != http.StatusNotFound {
		t.Errorf("/hijack, the rule of the redefined router, answered %d (error %v), want 404: the original definition must win", status, err)
	}

	// A .part file is not read. A later real change shows Traefik still reloads with it present.
	direct("zz.part", "this is : not [ yaml {\n")
	env.Apply(t, []traefiktest.FileSpec{s1Dynamic(s1Doc("left", base, s1Router("r-after", s1Rule(host, "/after"), "s-a")))})
	s.awaitGet(t, host, "/after")
	s.awaitGet(t, host, "/leftover")
	if logs := s.tr.Logs(t); strings.Contains(logs, "zz.part") {
		t.Errorf("Traefik's log mentions zz.part, so the file provider read it:\n%s", logs)
	}
}
