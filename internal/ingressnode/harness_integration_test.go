// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressnode

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

const (
	// ingressImage is what `make ingress-image` builds.
	ingressImage = "tierplan-ingress:dev"
	// ingressName is the name of the ingress container in the node.
	ingressName = "ingress"
	// routeHost is the one hostname the tests route; the backend is stack web's service whoami, which the
	// renderer names web_whoami.
	routeHost = "a.test.local"
	// swarmNodeID is the node ID the hub is told this host has and the supervisor presents.
	swarmNodeID = "node-1"
	// network is the user network the backend and the ingress container share, so the name web_whoami
	// resolves.
	network = "ingress-test"
	// hostTokenPath is where the join token sits in the node; secretPath is where the container reads it, as
	// it would read a Docker secret.
	hostTokenPath = "/run/ingress-join-token"
	secretPath    = "/run/secrets/ingress-join-token"
	// pollFor bounds the waits for something the image does on its own schedule: an enrolment, a Traefik
	// start or reload, a restart.
	pollFor = 2 * time.Minute
)

// nodeEnv is one dind node with a backend, the in-process hub the node's ingress container dials, one
// environment and the CA the leaves come from.
type nodeEnv struct {
	node *dockertest.Node
	srv  *ingresstest.Server
	env  ingresstest.Environment
	ca   *traefiktest.CA
}

// startNodeEnv starts the node, loads the two images, starts the hub on the node's gateway address and
// the backend, and writes the join token to a file in the node. It starts no ingress container: runIngress
// does. The test is skipped off the engine 29 leg.
func startNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()
	traefiktest.SkipUnlessEngineLeg(t)
	node := dockertest.StartHost(t, "")
	dockertest.LoadImage(t, ingressImage, node)
	dockertest.LoadImage(t, traefiktest.WhoamiImage, node)

	e := &nodeEnv{node: node, ca: traefiktest.NewCA(t)}
	// Registered after the node's own cleanup, so it runs first, while the node still answers.
	e.dumpOnFailure(t)

	e.srv = ingresstest.Start(t, ingresstest.Options{Host: node.Gateway})
	e.env = e.srv.NewEnvironment(t, "swarm")
	e.srv.Nodes.Set(e.env.ID, swarmNodeID)

	e.sh(t, "docker network create "+network)
	e.sh(t, "docker run -d --name web_whoami --network "+network+" "+traefiktest.WhoamiImage)
	// The token goes in a file the container's user can read, never on the container's command line or
	// in its environment. WriteFile puts the content in an exec command, so its error text, which would
	// repeat the command, is withheld.
	if err := node.WriteFile(testsupport.Context(t), hostTokenPath, []byte(e.env.Token)); err != nil {
		t.Fatal("write the join token file in the node failed (the error text is withheld: it carries the command)")
	}
	e.sh(t, "chmod 0444 "+hostTokenPath)
	return e
}

// dumpOnFailure prints what the node holds when the test fails: the containers, the ingress container's
// last log lines and health (its log holds no token, which TestNoSecretsInLogs of the supervisor proves),
// and the hub's row for the node. It runs no command that would print a container's environment.
func (e *nodeEnv) dumpOnFailure(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, line := range []string{
			"docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}'",
			"docker inspect -f 'state={{.State.Status}} health={{.State.Health.Status}} restarts={{.RestartCount}}' " + ingressName + " 2>&1",
			"docker logs --tail 60 " + ingressName + " 2>&1",
		} {
			out, _ := e.node.Sh(ctx, line)
			t.Logf("%s: %s\n%s", e.node.Hostname, line, strings.TrimSpace(out))
		}
		if nodes, err := e.srv.Service.ListNodes(ctx, e.env.ID); err == nil {
			for _, n := range nodes {
				t.Logf("hub row of %s: status=%s health=%s applied=%s rejected=%s (%s)", n.ID, n.Status, n.Health, n.AppliedBundleID, n.RejectedBundleID, n.RejectedReason)
			}
		}
	})
}

// sh runs a shell line in the node and fails the test when it exits non-zero.
func (e *nodeEnv) sh(t *testing.T, line string) string {
	t.Helper()
	out, err := e.node.Sh(testsupport.Context(t), line)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// runIngress starts the ingress container on volume. The hub's address and pin, the node ID and the token
// file are all it is given; the token's value is not on the command line.
func (e *nodeEnv) runIngress(t *testing.T, volume string) {
	t.Helper()
	line := fmt.Sprintf("docker run -d --name %s --network %s -p 8080:8080 -p 8443:8443 -p 8083:8083 "+
		"-v %s:/var/lib/tierplan-ingress --mount type=bind,src=%s,dst=%s,readonly "+
		"-e TIERPLAN_INGRESS_URL='%s' -e TIERPLAN_INGRESS_PIN='%s' -e TIERPLAN_INGRESS_JOIN_TOKEN_FILE=%s -e TIERPLAN_INGRESS_NODE_ID=%s %s",
		ingressName, network, volume, hostTokenPath, secretPath, e.srv.URL(), e.srv.Pin(), secretPath, swarmNodeID, ingressImage)
	e.sh(t, line)
}

// renderBundle renders what the reconciler would: the route routeHost to stack web's service whoami on
// port 80, served with the given certificate. version is the certificate set version.
func renderBundle(t *testing.T, version int64, chainPEM, keyPEM []byte) ingressbundle.Bundle {
	t.Helper()
	in := ingressrender.Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
			Claims: []ingress.SnapshotClaim{{
				Claim:     ingress.Claim{Hostname: routeHost, OwnerKind: ingress.OwnerStack},
				StackName: "web", Target: ingress.TargetSwarm,
			}},
			Routes: []ingress.SnapshotRoute{{
				Route:     ingress.Route{Hostname: routeHost, PathPrefix: "/", Service: "whoami", Port: 80, Scheme: ingress.SchemeHTTP},
				OwnerKind: ingress.OwnerStack, StackName: "web", Target: ingress.TargetSwarm,
			}},
		},
		Certs:        []ingressrender.HostCert{{Hostname: routeHost, ChainPEM: chainPEM, KeyPEM: keyPEM}},
		CertsVersion: version,
	}
	res, err := ingressrender.Render(in)
	if err != nil {
		t.Fatalf("render the bundle: %v", err)
	}
	return res.Bundle
}

// serialOf is the serial number of the first certificate of a PEM chain, as the handshake reports it.
func serialOf(t *testing.T, chainPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		t.Fatal("serialOf: no PEM block in the chain")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("serialOf: %v", err)
	}
	return c.SerialNumber.String()
}

// secureConn is one HTTP/1.1 keep-alive connection to the node's 8443, verified against the test CA for
// routeHost. It is how a test sees which certificate a handshake got and whether a connection opened
// before a change still works after it.
type secureConn struct {
	conn *tls.Conn
	br   *bufio.Reader
}

// open dials the node's 8443 with the SNI routeHost and completes the handshake.
func (e *nodeEnv) open() (*secureConn, error) {
	d := &net.Dialer{Timeout: testsupport.Deadline}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(e.node.IP, "8443"),
		&tls.Config{ServerName: routeHost, RootCAs: e.ca.Pool(), MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	return &secureConn{conn: conn, br: bufio.NewReader(conn)}, nil
}

// serial is the serial number of the certificate the server presented.
func (c *secureConn) serial() string {
	return c.conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
}

// get requests / for routeHost on the connection and returns the status and the body, leaving the
// connection open for another request.
func (c *secureConn) get() (int, string, error) {
	if err := c.conn.SetDeadline(time.Now().Add(testsupport.Deadline)); err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+routeHost+"/", nil)
	if err != nil {
		return 0, "", err
	}
	if err := req.Write(c.conn); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), err
}

func (c *secureConn) close() { _ = c.conn.Close() }

// getHTTPS makes one request on a new connection: the status, the body and the serial of the certificate
// the handshake got, or the error.
func (e *nodeEnv) getHTTPS() (status int, body, serial string, err error) {
	c, err := e.open()
	if err != nil {
		return 0, "", "", err
	}
	defer c.close()
	status, body, err = c.get()
	return status, body, c.serial(), err
}

// awaitServing polls until a new connection to 8443 is answered 200 by the backend. A non-empty serial also
// requires that connection to have been served that certificate.
func (e *nodeEnv) awaitServing(t *testing.T, serial string) {
	t.Helper()
	testsupport.Poll(t, pollFor, "https://"+routeHost+" to answer 200 from the backend", func() (bool, string) { // testsupport: external, the image enrols, starts and reloads on its own schedule
		status, body, got, err := e.getHTTPS()
		if err != nil {
			return false, err.Error()
		}
		if status != http.StatusOK || !strings.Contains(body, "Host: "+routeHost) {
			return false, fmt.Sprintf("status %d", status)
		}
		if serial != "" && got != serial {
			return false, "serving certificate " + got
		}
		return true, ""
	})
}

// requireServing requires, once and with no waiting, a 200 from the backend on a new connection that was
// served the certificate with the given serial.
func (e *nodeEnv) requireServing(t *testing.T, serial string) {
	t.Helper()
	status, body, got, err := e.getHTTPS()
	if err != nil {
		t.Fatalf("GET https://%s: %v", routeHost, err)
	}
	if status != http.StatusOK || !strings.Contains(body, "Host: "+routeHost) {
		t.Fatalf("GET https://%s answered %d, want 200 from the backend", routeHost, status)
	}
	if got != serial {
		t.Fatalf("the certificate served has serial %s, want %s", got, serial)
	}
}

// readyz is the status and the body of the supervisor's readiness endpoint on 8083, or the error.
func (e *nodeEnv) readyz() (int, string, error) {
	hc := &http.Client{Timeout: testsupport.Deadline}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+net.JoinHostPort(e.node.IP, "8083")+"/readyz", nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), err
}

// awaitReady polls until readiness answers 200.
func (e *nodeEnv) awaitReady(t *testing.T) {
	t.Helper()
	testsupport.Poll(t, pollFor, "/readyz to answer 200", func() (bool, string) { // testsupport: external, the image starts on its own schedule
		status, _, err := e.readyz()
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK, fmt.Sprintf("status %d", status)
	})
}

// requireReady requires readiness to answer 200 now.
func (e *nodeEnv) requireReady(t *testing.T) {
	t.Helper()
	status, _, err := e.readyz()
	if err != nil || status != http.StatusOK {
		t.Fatalf("/readyz: status %d, error %v; want 200", status, err)
	}
}

// process is one line of docker top: a process of the ingress container.
type process struct{ pid, comm string }

// processes lists the processes of the ingress container as the node sees them (the node's own PID
// namespace, so a pid is one a command run in the node can signal and read in /proc).
func (e *nodeEnv) processes(t *testing.T) []process {
	t.Helper()
	out := e.sh(t, "docker top "+ingressName+" -eo pid,comm")
	var ps []process
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 2 { // the header
			continue
		}
		ps = append(ps, process{pid: f[0], comm: f[1]})
	}
	return ps
}

// traefikPID is the pid of the container's Traefik, or "" when it has none.
func (e *nodeEnv) traefikPID(t *testing.T) string {
	t.Helper()
	for _, p := range e.processes(t) {
		if p.comm == "traefik" {
			return p.pid
		}
	}
	return ""
}

// logs is everything the ingress container has logged since it was created.
func (e *nodeEnv) logs(t *testing.T) string {
	t.Helper()
	return e.sh(t, "docker logs "+ingressName+" 2>&1")
}

// nodeRow is the hub's row for the one node of the environment.
func (e *nodeEnv) nodeRow(t *testing.T) ingresshub.Node {
	t.Helper()
	nodes, err := e.srv.Service.ListNodes(testsupport.Context(t), e.env.ID)
	if err != nil {
		t.Fatalf("list the nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("the environment has %d nodes, want 1", len(nodes))
	}
	return nodes[0]
}
