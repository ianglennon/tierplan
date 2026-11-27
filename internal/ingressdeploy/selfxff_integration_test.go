// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

const (
	// forgedXFF is the address a client or a routed service forges in X-Forwarded-For, hoping TierPlan records it
	// (`X-Forwarded-For: 6.6.6.6`).
	forgedXFF = "6.6.6.6"
	// stubTrustedEnv is the stub's variable for its trusted proxy ranges (selfstub's TIERPLAN_STUB_TRUSTED).
	stubTrustedEnv = "TIERPLAN_STUB_TRUSTED"
	// stubServerName is the name on the stub's self-signed certificate (writeStubTLS), which the proxy verifies it by.
	stubServerName = "tierplan"
	// stubPeerLine starts the stub's log line of a /client-ip request, which names the request's TCP peer.
	stubPeerLine = "client-ip peer="
)

// xffEnv is a swarm with ingress on, TierPlan's own route claimed, and the stub standing in for TierPlan on tierplan-self,
// trusting exactly ingress.SelfTrustedPrefixes of the observed self network, as the real server does (D-06).
type xffEnv struct {
	*swarmEnv
	image   string
	obs     ingressdeploy.SelfObservation
	trusted []netip.Prefix
	// stubCert is the certificate the stub serves, which the proxy trusts it by.
	stubCert []byte
}

// startXFFEnv builds the xffEnv: the stub service on the manager, ingress on, the route's certificate, the claim, the self
// network the ensure loop makes, then the stub's own service update that attaches it with its trusted ranges set from the
// observation.
func startXFFEnv(t *testing.T) *xffEnv {
	t.Helper()
	e := &xffEnv{swarmEnv: startSwarmEnv(t, withSwarmSelf)}
	e.image = traefiktest.SelfStubImage(t)
	dockertest.LoadImage(t, e.image, e.mgr, e.wrk)
	writeStubTLS(t, e.mgr)
	e.stubCert = []byte(e.sh(e.mgr, "cat "+stubTLSDir+"/tls.crt"))
	e.sh(e.mgr, "docker service create --detach=false --quiet --no-resolve-image --name "+stubService+
		" --label "+stacks.LabelSwarmNamespace+"="+stubNamespace+" --container-label "+stacks.LabelSwarmNamespace+"="+stubNamespace+
		" --constraint node.role==manager --mount type=bind,src="+stubTLSDir+",dst=/tls,readonly "+e.image+" "+stubCommand())
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, _ := e.mgr.Sh(ctx, "docker service ps --no-trunc "+stubService+" 2>&1; docker service logs --raw --tail 40 "+stubService+" 2>&1")
		t.Logf("the stub service:\n%s", strings.TrimSpace(out))
		for _, n := range e.sw.Nodes() {
			out, _ = n.Sh(ctx, "docker network inspect --verbose "+ingress.SelfNetworkName+" 2>&1")
			t.Logf("%s: %s, verbose:\n%s", n.Hostname, ingress.SelfNetworkName, strings.TrimSpace(out))
		}
	})

	requireJobDone(t, e.enable())
	// The route's certificate is uploaded before the claim: once the system holds the name, an upload for it is refused.
	e.uploadCertificate(selfHost)
	claimSelf(t, e.istore, e.envID, stubService)
	e.passUntil("the ensure loop to make "+ingress.SelfNetworkName+", attach the proxy and observe the subnet", func() (bool, string) {
		net, ok := selfNetworkOn(t, e.mgr)
		if !ok {
			return false, "no network yet"
		}
		o, _ := e.deploy.SelfObservationOf(e.envID)
		return e.proxyOnSelf(net.ID) && o.Exists && o.Subnet.IsValid(), fmt.Sprintf("proxy on it %v, observation %+v", e.proxyOnSelf(net.ID), o)
	})
	e.obs, _ = e.deploy.SelfObservationOf(e.envID)
	e.trusted = ingress.SelfTrustedPrefixes(e.obs.Facts())
	if len(e.trusted) == 0 {
		t.Fatalf("no trusted ranges from the observation %+v", e.obs)
	}
	for _, p := range e.trusted {
		if !e.obs.Subnet.Overlaps(p) {
			t.Fatalf("trusted range %s is outside the self subnet %s", p, e.obs.Subnet)
		}
	}

	// TierPlan's own service update (D-03), which also gives the stub the trusted ranges the server would derive.
	ranges := make([]string, 0, len(e.trusted))
	for _, p := range e.trusted {
		ranges = append(ranges, p.String())
	}
	e.sh(e.mgr, "docker service update --detach=false --quiet --network-add "+ingress.SelfNetworkName+
		" --env-add "+stubTrustedEnv+"="+strings.Join(ranges, ",")+" "+stubService+" 2>&1")
	e.passUntil("the observation to say the stub is on "+ingress.SelfNetworkName, func() (bool, string) {
		o, ok := e.deploy.SelfObservationOf(e.envID)
		return ok && o.Exists && o.Attached, fmt.Sprintf("%+v", o)
	})
	if o, _ := e.deploy.SelfObservationOf(e.envID); o.Subnet != e.obs.Subnet {
		t.Fatalf("the self subnet moved from %s to %s while the stub joined it", e.obs.Subnet, o.Subnet)
	}
	return e
}

// taskAddrs are the addresses on the network of the service's tasks, read from the tasks' network attachments; filter is a
// `docker service ps` filter, or "" for every task.
func (e *xffEnv) taskAddrs(service, network, filter string) []netip.Addr {
	e.t.Helper()
	cmd := "docker service ps -q --no-trunc "
	if filter != "" {
		cmd += "--filter " + filter + " "
	}
	var out []netip.Addr
	for _, id := range strings.Fields(e.sh(e.mgr, cmd+service)) {
		raw := e.sh(e.mgr, "docker inspect --format '{{json .NetworksAttachments}}' "+id)
		var atts []struct {
			Network struct {
				Spec struct{ Name string }
			}
			Addresses []string
		}
		if err := json.Unmarshal([]byte(raw), &atts); err != nil {
			e.t.Fatalf("read the network attachments of task %s: %v (%s)", id, err, strings.TrimSpace(raw))
		}
		for _, a := range atts {
			if a.Network.Spec.Name != network {
				continue
			}
			for _, cidr := range a.Addresses {
				p, err := netip.ParsePrefix(cidr)
				if err != nil {
					e.t.Fatalf("task %s has address %q on %s: %v", id, cidr, network, err)
				}
				out = append(out, p.Addr())
			}
		}
	}
	return out
}

// stubPeers are the TCP peers of the /client-ip requests the stub logged, oldest first.
func (e *xffEnv) stubPeers() []netip.Addr {
	e.t.Helper()
	var out []netip.Addr
	for _, line := range strings.Split(e.sh(e.mgr, "docker service logs --raw "+stubService+" 2>&1"), "\n") {
		_, rest, ok := strings.Cut(line, stubPeerLine)
		if !ok {
			continue
		}
		peer, _, _ := strings.Cut(rest, " ")
		if ap, err := netip.ParseAddrPort(peer); err == nil {
			out = append(out, ap.Addr().Unmap())
		}
	}
	return out
}

// clientAnswer is the stub's /client-ip answer: the client address it derived and the X-Forwarded-For it was given.
type clientAnswer struct {
	client, xff string
}

// parseClientAnswer reads the stub's /client-ip body; ok is false when the body is not one.
func parseClientAnswer(body string) (clientAnswer, bool) {
	var a clientAnswer
	seen := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "client="); ok {
			a.client, seen = v, true
		}
		if v, ok := strings.CutPrefix(line, "xff="); ok {
			a.xff = v
		}
	}
	return a, seen
}

// TestSelfXFFThroughTheProxyIsTheRealClient is D-08 at the proxy's end on a real swarm: a client sends a forged
// `X-Forwarded-For: 6.6.6.6` to TierPlan's own route on a node's port 443. The proxy writes the header itself (an entry point with no
// trusted IPs), the stub trusts only the self network's ranges, and the address it derives is the client's own, the one
// the client's connection came from, not the forged one. The stub's TCP peer is the proxy's side of tierplan-self (A1): the
// node's load-balancer endpoint there, which the service's virtual IP masquerades the proxy to.
func TestSelfXFFThroughTheProxyIsTheRealClient(t *testing.T) {
	e := startXFFEnv(t)

	// The reconciler renders TierPlan's own route to the stub, which the proxy verifies by the stub's certificate.
	self := selfInputFunc(func(_ context.Context, envID string) (ingressrender.SelfInput, int64, bool) {
		if envID != e.envID {
			return ingressrender.SelfInput{}, 0, false
		}
		return ingressrender.SelfInput{
			Host: selfHost, BackendHost: stubService, BackendPort: 7480, ServerName: stubServerName,
			TrustPEM: e.stubCert, Attached: true,
		}, 1, true
	})
	e.selfIn.Store(&self)

	// The request goes from this process to the manager's port 443; the address the node sees is the source of the TCP
	// connection, which the dialer records.
	var local netip.Addr
	var answer clientAnswer
	dockertest.Wait(t, serveBound, "https://"+selfHost+"/client-ip on "+e.mgr.Hostname+":443 to reach the stub", func() (bool, string) {
		status, body, from, err := e.forgedGet(e.mgr, selfHost, "/client-ip")
		if err != nil {
			return false, err.Error()
		}
		a, ok := parseClientAnswer(body)
		if status != http.StatusOK || !ok {
			return false, fmt.Sprintf("status %d", status)
		}
		local, answer = from, a
		return true, ""
	})
	t.Logf("the stub answered client=%s xff=%q for a request from %s", answer.client, answer.xff, local)
	if answer.client == forgedXFF {
		t.Fatalf("the stub believed the forged X-Forwarded-For: client=%s", answer.client)
	}
	if answer.client != local.String() {
		t.Fatalf("the stub derived client=%s, want the address the request came from, %s", answer.client, local)
	}

	// A1: the stub's TCP peer is on tierplan-self, inside the trusted ranges, and is the proxy's own side of the network on
	// the node that served the request: the service's virtual IP masquerades the proxy task to the node's load-balancer
	// endpoint on the network (lb-tierplan-self), or, on an engine that keeps the source, the proxy task's own address.
	// Either is reachable only by what is on tierplan-self, the proxy and TierPlan.
	proxySide := e.taskAddrs(ingress.ServiceName, ingress.SelfNetworkName, "desired-state=running")
	if lb, ok := e.lbAddr(e.mgr, ingress.SelfNetworkName); ok {
		proxySide = append(proxySide, lb)
	}
	var peers []netip.Addr
	dockertest.Wait(t, serveBound, "the stub to log the peer of the request", func() (bool, string) {
		peers = e.stubPeers()
		return len(peers) > 0, "no peer line yet"
	})
	t.Logf("the stub's peers %v; the proxy's side of %s on %s %v", peers, ingress.SelfNetworkName, e.mgr.Hostname, proxySide)
	for _, peer := range peers {
		if !e.obs.Subnet.Contains(peer) {
			t.Fatalf("the stub's peer %s is outside the self subnet %s", peer, e.obs.Subnet)
		}
		if !slices.ContainsFunc(e.trusted, func(p netip.Prefix) bool { return p.Contains(peer) }) {
			t.Fatalf("the stub's peer %s is outside the trusted ranges %v", peer, e.trusted)
		}
		if !slices.Contains(proxySide, peer) {
			t.Fatalf("the stub's peer %s is neither a proxy task's address on %s nor %s's load-balancer endpoint on it (%v)", peer, ingress.SelfNetworkName, e.mgr.Hostname, proxySide)
		}
	}
}

// lbAddr is the node's load-balancer endpoint address on the overlay network (the lb-<network> entry of its inspect); ok is
// false when the node holds none.
func (e *xffEnv) lbAddr(n *dockertest.Node, network string) (netip.Addr, bool) {
	e.t.Helper()
	raw := e.sh(n, "docker network inspect --format '{{json .Containers}}' "+network)
	var ctrs map[string]struct{ IPv4Address string }
	if err := json.Unmarshal([]byte(raw), &ctrs); err != nil {
		e.t.Fatalf("read the endpoints of %s on %s: %v (%s)", network, n.Hostname, err, strings.TrimSpace(raw))
	}
	c, ok := ctrs["lb-"+network]
	if !ok {
		return netip.Addr{}, false
	}
	p, err := netip.ParsePrefix(c.IPv4Address)
	if err != nil {
		e.t.Fatalf("the load-balancer endpoint of %s on %s has address %q: %v", network, n.Hostname, c.IPv4Address, err)
	}
	return p.Addr(), true
}

// forgedGet asks a node's port 443 for path on host with X-Forwarded-For set to forgedXFF, the test CA as the only root,
// and returns the status, the body and the local address the connection came from.
func (e *xffEnv) forgedGet(n *dockertest.Node, host, path string) (int, string, netip.Addr, error) {
	var from netip.Addr
	var d net.Dialer
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				if ap, perr := netip.ParseAddrPort(c.LocalAddr().String()); perr == nil {
					from = ap.Addr().Unmap()
				}
			}
			return c, err
		},
		TLSClientConfig:   &tls.Config{ServerName: host, RootCAs: e.ca.Pool(), MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: testsupport.Deadline}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+net.JoinHostPort(n.IP, "443")+path, nil)
	if err != nil {
		return 0, "", from, err
	}
	req.Host = host
	req.Header.Set("X-Forwarded-For", forgedXFF)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", from, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(body), from, err
}
