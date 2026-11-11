// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package acmetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ChallTestSrvImage is the challenge test server that belongs to PebbleImage (the same release train).
const ChallTestSrvImage = "ghcr.io/letsencrypt/pebble-challtestsrv:2.10.1"

const (
	challAlias   = "challtestsrv"
	challDNSPort = "8053"
	challMgmt    = "8055/tcp"
	challUDP     = challDNSPort + "/udp"
	challTCP     = challDNSPort + "/tcp"
)

// ChallTestSrv is a running pebble-challtestsrv. It answers DNS for the TXT and CNAME records a test
// sets through its management API, and it is the DNS server a Pebble container resolves challenges
// through.
type ChallTestSrv struct {
	nw       *testcontainers.DockerNetwork
	mgmtURL  string
	dnsAddr  string
	httpc    *http.Client
	mu       sync.Mutex
	txtByKey map[string][]string
}

// StartChallTestSrv runs the challenge test server on a new network, under the alias "challtestsrv",
// and removes both when the test ends. Its HTTP-01, HTTPS, TLS-ALPN-01 and DoH servers are switched off:
// only DNS and the management interface run.
func StartChallTestSrv(t testing.TB) *ChallTestSrv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pebbleStartup)
	defer cancel()

	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("acmetest: create the network for the challenge test server: %v", err)
	}
	testcontainers.CleanupNetwork(t, nw)

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: ChallTestSrvImage,
			Cmd: []string{
				"-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", "",
				"-dnsserver", ":" + challDNSPort, "-management", ":8055",
			},
			ExposedPorts:   []string{challMgmt, challUDP, challTCP},
			Networks:       []string{nw.Name},
			NetworkAliases: map[string][]string{nw.Name: {challAlias}},
			// The image has no shell, so the wait is a connection from here.
			WaitingFor: wait.ForListeningPort(challMgmt).SkipInternalCheck().WithStartupTimeout(pebbleStartup),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("acmetest: start %s: %v", ChallTestSrvImage, err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		rd, err := ctr.Logs(logCtx)
		if err != nil {
			t.Logf("acmetest: read the challenge test server's log: %v", err)
			return
		}
		defer func() { _ = rd.Close() }()
		out, _ := io.ReadAll(rd)
		t.Logf("acmetest: challenge test server's log:\n%s", out)
	})

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("acmetest: find the challenge test server's host: %v", err)
	}
	mgmt, err := ctr.MappedPort(ctx, challMgmt)
	if err != nil {
		t.Fatalf("acmetest: find the challenge test server's management port: %v", err)
	}
	dns, err := ctr.MappedPort(ctx, challUDP)
	if err != nil {
		t.Fatalf("acmetest: find the challenge test server's DNS port: %v", err)
	}
	s := &ChallTestSrv{
		nw:       nw,
		mgmtURL:  "http://" + net.JoinHostPort(host, mgmt.Port()),
		dnsAddr:  net.JoinHostPort("127.0.0.1", dns.Port()),
		httpc:    &http.Client{Timeout: 30 * time.Second},
		txtByKey: map[string][]string{},
	}
	t.Cleanup(s.httpc.CloseIdleConnections)
	return s
}

// Network is the network the server is on, for PebbleOptions.Network.
func (s *ChallTestSrv) Network() *testcontainers.DockerNetwork { return s.nw }

// InNetworkDNS is the address of its DNS server from a container on Network(), for
// PebbleOptions.DNSServer.
func (s *ChallTestSrv) InNetworkDNS() string { return challAlias + ":" + challDNSPort }

// DNSAddr is the address of its DNS server from the test process: the mapped UDP port on 127.0.0.1.
func (s *ChallTestSrv) DNSAddr() string { return s.dnsAddr }

// AddTXT serves value as a TXT record at fqdn. It satisfies dnstest.TXTSink. A failure to reach the
// management interface panics, because the sink has no way to return it and a test that cannot set a
// record has nothing left to check.
func (s *ChallTestSrv) AddTXT(fqdn, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := dotted(fqdn)
	s.post("set-txt", map[string]string{"host": k, "value": value})
	s.txtByKey[k] = append(s.txtByKey[k], value)
}

// RemoveTXT stops serving value at fqdn. The server clears every TXT record at a name at once, so the
// other values this helper set there are set again. It satisfies dnstest.TXTSink.
func (s *ChallTestSrv) RemoveTXT(fqdn, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := dotted(fqdn)
	keep := slices.DeleteFunc(slices.Clone(s.txtByKey[k]), func(v string) bool { return v == value })
	s.post("clear-txt", map[string]string{"host": k})
	for _, v := range keep {
		s.post("set-txt", map[string]string{"host": k, "value": v})
	}
	if len(keep) == 0 {
		delete(s.txtByKey, k)
		return
	}
	s.txtByKey[k] = keep
}

// SetCNAME serves a CNAME record from name to target.
func (s *ChallTestSrv) SetCNAME(name, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.post("set-cname", map[string]string{"host": dotted(name), "target": dotted(target)})
}

// dotted is name in lowercase with one trailing dot, the form the management interface wants.
func dotted(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".") + "."
}

// post sends one management request. The caller holds s.mu.
func (s *ChallTestSrv) post(op string, body map[string]string) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(fmt.Errorf("acmetest: encode the %s request: %w", op, err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.mgmtURL+"/"+op, bytes.NewReader(raw))
	if err != nil {
		panic(fmt.Errorf("acmetest: build the %s request: %w", op, err))
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		panic(fmt.Errorf("acmetest: challenge test server %s: %w", op, err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		panic(fmt.Errorf("acmetest: challenge test server %s answered %d: %s", op, resp.StatusCode, msg))
	}
}
