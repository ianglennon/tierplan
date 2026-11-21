// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestRoutedStackServedOn443 is SC 2 on the data plane: a stack deployed through stackops.DeploySwarm with a route for
// app.test.local writes its claim and joins the ingress network with no alias; its deploy succeeds with no certificate
// and the route reads waiting_certificate; once a certificate from the test CA is uploaded, every node of the swarm answers
// HTTPS on port 443 for that hostname with the backend's own page.
func TestRoutedStackServedOn443(t *testing.T) {
	e := startSwarmEnv(t)
	e.startRegistry()
	requireJobDone(t, e.enable())
	e.awaitReady()

	st, res, job := e.deployStack("web", e.routedCompose(routeHost))
	if !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the deploy ended %+v, want it to succeed before any certificate exists", res)
	}
	if held := e.claimsOf(st); !slices.Equal(held, []string{routeHost}) {
		t.Fatalf("the stack holds %v, want %s", held, routeHost)
	}

	// No certificate yet: the deploy worked and the route is left out of the bundle until one exists.
	e.awaitRouteState(st, stackops.RouteWaitingCertificate)

	e.uploadCertificate(routeHost)
	for _, n := range e.sw.Nodes() {
		e.awaitServed(n, routeHost)
	}
	e.awaitRouteState(st, stackops.RouteServing)

	// The routed service is on the ingress network with no alias (LBL-06): the proxy reaches it by the service's own name.
	netID := e.ingressNetworkID()
	var spec swarm.ServiceSpec
	d := e.service("web_web")
	if err := jsonUnmarshal(d.Spec, &spec); err != nil {
		t.Fatalf("read the service's spec: %v", err)
	}
	attached := false
	for _, a := range spec.TaskTemplate.Networks {
		if a.Target == netID || a.Target == ingress.NetworkName {
			attached = true
			if len(a.Aliases) != 0 {
				t.Errorf("the ingress attachment of web_web has aliases %v, want none", a.Aliases)
			}
		}
	}
	if !attached {
		t.Errorf("web_web is on networks %+v, want the ingress network %s", spec.TaskTemplate.Networks, netID)
	}
}

// TestRefusedStopRemoveOnSwarm is SC 2 and SC 3: a second stack asking for a hostname another holds is refused with nothing
// changed on the swarm; stopping the holder keeps its claim and its route reads backend_not_running; starting it serves
// again; removing it releases the claim and the hostname stops being served.
func TestRefusedStopRemoveOnSwarm(t *testing.T) {
	e := startSwarmEnv(t)
	e.startRegistry()
	requireJobDone(t, e.enable())
	e.awaitReady()

	first, res, job := e.deployStack("web", e.routedCompose(routeHost))
	if !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the first deploy ended %+v", res)
	}
	e.uploadCertificate(routeHost)
	for _, n := range e.sw.Nodes() {
		e.awaitServed(n, routeHost)
	}

	// A second stack asking for the same hostname is refused at the check, and the swarm is as it was.
	servicesBefore, networksBefore := e.swarmNames()
	second, res2, job2 := e.deployStack("other", e.routedCompose(routeHost))
	if res2.OK() || res2.Code != stackops.CodeIngressRefused || res2.Wrote {
		t.Fatalf("the second deploy ended %+v, want ingress_refused with nothing written", res2)
	}
	if got := itemStateOf(job2, stackops.ItemCheck); got != "failed:"+stackops.CodeIngressRefused {
		t.Errorf("the check item of the refused deploy is %q, want failed:%s", got, stackops.CodeIngressRefused)
	}
	servicesAfter, networksAfter := e.swarmNames()
	if !slices.Equal(servicesBefore, servicesAfter) || !slices.Equal(networksBefore, networksAfter) {
		t.Errorf("a refused deploy changed the swarm: services %v -> %v, networks %v -> %v", servicesBefore, servicesAfter, networksBefore, networksAfter)
	}
	if held := e.claimsOf(second); len(held) != 0 {
		t.Errorf("the refused stack holds %v, want nothing", held)
	}
	if held := e.claimsOf(first); !slices.Equal(held, []string{routeHost}) {
		t.Errorf("the holder holds %v after a refusal, want %s still", held, routeHost)
	}

	// Stopping keeps the claim: the route reads backend_not_running, stopped.
	if r := e.lifecycle(stackops.LifecycleStop, first); !r.OK() {
		t.Fatalf("stop = %+v", r)
	}
	if held := e.claimsOf(first); !slices.Equal(held, []string{routeHost}) {
		t.Errorf("a stopped stack holds %v, want %s kept", held, routeHost)
	}
	states := e.routeStates(first)
	if len(states) != 1 || states[0].State != stackops.RouteBackendNotRunning || !states[0].Stopped {
		t.Errorf("route states of the stopped stack = %+v, want one route backend_not_running with stopped set", states)
	}

	// Starting it serves it again.
	if r := e.lifecycle(stackops.LifecycleStart, first); !r.OK() {
		t.Fatalf("start = %+v", r)
	}
	for _, n := range e.sw.Nodes() {
		e.awaitServed(n, routeHost)
	}
	e.awaitRouteState(first, stackops.RouteServing)

	// Removing it releases the claim, and the hostname is no longer served by the backend on any node.
	if r := e.remove(first); !r.OK() {
		t.Fatalf("remove = %+v", r)
	}
	if held := e.claimsOf(first); len(held) != 0 {
		t.Errorf("a removed stack holds %v, want nothing", held)
	}
	for _, n := range e.sw.Nodes() {
		e.awaitNotServed(n, routeHost)
	}
	if got := e.service("web_web"); got.ID != "" {
		t.Errorf("the removed stack's service is still there: %+v", got)
	}
}

// service reads a service of the swarm by name; the zero value when there is none.
func (e *swarmEnv) service(name string) dockerx.ServiceDetail {
	e.t.Helper()
	var d dockerx.ServiceDetail
	err := e.cluster(func(ctx context.Context, c *dockerx.Client) error {
		var err error
		d, _, err = c.ServiceInspect(ctx, name)
		if errors.Is(err, dockerx.ErrServiceNotFound) {
			d, err = dockerx.ServiceDetail{}, nil
		}
		return err
	})
	if err != nil {
		e.t.Fatalf("inspect %s: %v", name, err)
	}
	return d
}

// ingressNetworkID is the ID of the ingress network.
func (e *swarmEnv) ingressNetworkID() string {
	e.t.Helper()
	id := ""
	if err := e.cluster(func(ctx context.Context, c *dockerx.Client) error {
		rows, err := c.Networks(ctx)
		for _, r := range rows {
			if r.Name == ingress.NetworkName {
				id = r.ID
			}
		}
		return err
	}); err != nil {
		e.t.Fatalf("read the networks: %v", err)
	}
	return id
}

// swarmNames lists the names of the swarm's services and networks, sorted.
func (e *swarmEnv) swarmNames() (services, networks []string) {
	e.t.Helper()
	lines := func(cmd string) []string {
		out := strings.Fields(e.sh(e.mgr, cmd))
		slices.Sort(out)
		return out
	}
	return lines("docker service ls --format '{{.Name}}'"), lines("docker network ls --format '{{.Name}}'")
}

// itemStateOf is the state and code of one item of a job, as "state:code".
func itemStateOf(job jobs.Job, id string) string {
	for _, it := range job.Items {
		if it.ID == id {
			return it.State + ":" + it.Code
		}
	}
	return "missing"
}

// claimsOf are the hostnames the stack holds, sorted.
func (e *swarmEnv) claimsOf(st *stack) []string {
	e.t.Helper()
	all, err := e.istore.InstallClaims(testsupport.Context(e.t))
	if err != nil {
		e.t.Fatalf("InstallClaims: %v", err)
	}
	out := []string{}
	for _, c := range all {
		if c.StackID == st.ID {
			out = append(out, c.Hostname)
		}
	}
	slices.Sort(out)
	return out
}

// uploadCertificate stores a leaf for host from the test CA as the live certificate of the hostname, as the upload
// route does: the proxy serves what the reconciler renders from it.
func (e *swarmEnv) uploadCertificate(host string) {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	// More than the day a certificate must have left to be served.
	chain, key := e.ca.LeafValidFor(t, 5*24*time.Hour, host)
	world, err := e.istore.World(ctx, e.envID, nil)
	if err != nil {
		t.Fatalf("read the ingress world: %v", err)
	}
	if _, _, err := e.certs.Upload(ctx, ingresscert.UploadInput{
		EnvID: e.envID, Hostname: host, Actor: "user:test", ChainPEM: chain, KeyPEM: key, World: world,
	}, e.ca.Pool(), nil); err != nil {
		t.Fatalf("upload the certificate of %s: %v", host, err)
	}
}

// routeStates derives the state of the stack's routes the way the stack page reads them: the same reads of the swarm, the
// reconciler and the nodes, through stackops.RouteStates.
func (e *swarmEnv) routeStates(st *stack) []stackops.RouteState {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	row, err := e.repo.Stack(ctx, e.envID, st.ID)
	if err != nil {
		t.Fatalf("read the stack: %v", err)
	}
	snap, err := e.istore.Snapshot(ctx, e.envID)
	if err != nil {
		t.Fatalf("read the ingress snapshot: %v", err)
	}
	in := stackops.RouteStateInput{Enabled: snap.Settings.Enabled, Stopped: row.Stopped}
	for _, r := range snap.Routes {
		if r.OwnerKind == ingress.OwnerStack && r.StackID == st.ID {
			in.Routes = append(in.Routes, r.Route)
		}
	}
	if in.Enabled && !in.Stopped {
		in.Running, in.Attached = map[string]int{}, map[string]bool{}
		if err := e.cluster(func(ctx context.Context, c *dockerx.Client) error {
			cluster, err := c.Cluster(ctx)
			if err != nil {
				return err
			}
			netID := e.ingressNetworkID()
			prefix := st.Name + "_"
			for _, r := range cluster.Services {
				if r.Stack == st.Name && strings.HasPrefix(r.Name, prefix) {
					in.Running[strings.TrimPrefix(r.Name, prefix)] = int(min(r.Running, math.MaxInt32))
				}
			}
			for _, ref := range cluster.ServiceRefs {
				if !strings.HasPrefix(ref.Name, prefix) {
					continue
				}
				for _, n := range ref.Networks {
					if n == ingress.NetworkName || netID != "" && n == netID {
						in.Attached[strings.TrimPrefix(ref.Name, prefix)] = true
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("read the swarm: %v", err)
		}
		path := func(p string) string {
			if p == "" {
				return "/"
			}
			return p
		}
		in.Skipped = map[stackops.RouteKey]string{}
		for _, s := range e.recon.Skipped(e.envID) {
			if s.Reason == ingressrender.SkipNoCertificate || s.Reason == ingressrender.SkipCertificateNotReady {
				in.Skipped[stackops.RouteKey{Hostname: s.Hostname, Path: path(s.PathPrefix)}] = string(s.Reason)
			}
		}
		if bundle, have := e.recon.Current(e.envID); have && bundle.ID != "" {
			in.InBundle = map[stackops.RouteKey]bool{}
			for _, r := range bundle.Manifest.Routes {
				in.InBundle[stackops.RouteKey{Hostname: r.Hostname, Path: path(r.PathPrefix)}] = true
			}
			nodes, err := e.srv.Service.ListNodes(ctx, e.envID)
			if err != nil {
				t.Fatalf("list the proxy nodes: %v", err)
			}
			active := 0
			in.NodesApplied = true
			for _, n := range nodes {
				if n.Status != "active" {
					continue
				}
				active++
				if n.AppliedBundleID != bundle.ID {
					in.NodesApplied = false
				}
			}
			if active == 0 {
				in.NodesApplied = false
			}
		}
	}
	return stackops.RouteStates(in)
}

// awaitRouteState waits until the stack's one route reads the state.
func (e *swarmEnv) awaitRouteState(st *stack, want string) {
	t := e.t
	t.Helper()
	dockertest.Wait(t, serveBound, "the route of stack "+st.Name+" to read "+want, func() (bool, string) {
		states := e.routeStates(st)
		return len(states) == 1 && states[0].State == want, fmt.Sprintf("%+v", states)
	})
}

// httpsGet asks a node's port 443 for the page of host, with the test CA as the only root, and returns the status and
// the body.
func (e *swarmEnv) httpsGet(n *dockertest.Node, host string) (int, string, error) {
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{ServerName: host, RootCAs: e.ca.Pool(), MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: testsupport.Deadline}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+net.JoinHostPort(n.IP, "443")+"/", nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), err
}

// awaitServed waits until the node's port 443 answers host with the backend's page over a certificate the test CA issued.
func (e *swarmEnv) awaitServed(n *dockertest.Node, host string) {
	t := e.t
	t.Helper()
	dockertest.Wait(t, serveBound, "https://"+host+" on "+n.Hostname+":443 to answer with the backend's page", func() (bool, string) {
		status, body, err := e.httpsGet(n, host)
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK && strings.Contains(body, backendBody), fmt.Sprintf("status %d", status)
	})
}

// awaitNotServed waits until the node's port 443 no longer answers host with the backend's page: a refused handshake or
// any other answer than the backend's.
func (e *swarmEnv) awaitNotServed(n *dockertest.Node, host string) {
	t := e.t
	t.Helper()
	dockertest.Wait(t, serveBound, "https://"+host+" on "+n.Hostname+":443 to stop reaching the backend", func() (bool, string) {
		status, body, err := e.httpsGet(n, host)
		if err != nil {
			return true, ""
		}
		return status != http.StatusOK || !strings.Contains(body, backendBody), fmt.Sprintf("still served: status %d", status)
	})
}
