// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The helper and image regression of the virtual IP (D-29, plan 26-25): the S3 spike's cases, run through the
// production service spec (ingressdeploy.KeepalivedServiceSpec) instead of a test configuration. The real
// tierplan-keepalived:dev image (`make keepalived-image`) runs its own entrypoint, discovers the interface,
// renders keepalived's configuration, asks the probe on the readiness port and writes the event lines the server
// reads. A global whoami service published host-mode on the readiness port stands in for the proxy, and a node
// label decides whether it runs on a node, so a test can fail one node's probe. The rig is copied from the S3
// harness (internal/dockerx), which lives in another package's tests.
const (
	// vhImage is what `make keepalived-image` builds on this machine's engine.
	vhImage = "tierplan-keepalived:dev"
	// vhReadyService is the readiness stand-in and vhReadyLabel the node label it is constrained to: vhReadyLabel=yes on a
	// node means the node's probe passes.
	vhReadyService = "vh-ready"
	vhReadyLabel   = "vhready"
	// vhRouterID is the router ID of the main service.
	vhRouterID = 51
	// vhSettle bounds the setup waits that follow a pull or a service start, vhFirstHolder the wait for the first holder
	// once every task runs (about 5.5 s measured by S3).
	vhSettle      = 3 * time.Minute
	vhFirstHolder = 60 * time.Second
	// vhLogBound bounds the wait for a line a helper writes soon after what caused it.
	vhLogBound = 60 * time.Second
)

// vhService is a keepalived service of the rig, built from KeepalivedServiceSpec.
type vhService struct {
	name     string
	id       string
	secretID string
	addr     netip.Prefix
	routerID int
}

// vipHelperRig is a three-node dind swarm (one manager, two workers) with the readiness stand-in on every node, the
// address the virtual IP would hold, and the services the tests start.
type vipHelperRig struct {
	t    testing.TB
	sw   *dockertest.Swarm
	host *client.Client
	mgr  *client.Client
	// vip is .250 of the nodes' network, which no node holds (the harness hands out .10 up).
	vip netip.Prefix
	// readyID is the readiness stand-in service.
	readyID string
	// nodeID is each node's swarm node ID by host name.
	nodeID map[string]string
}

// vhHostClient is a moby client to this machine's own engine.
func vhHostClient(t testing.TB) *client.Client {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatalf("connect to this machine's Docker engine: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// newVIPHelperRig starts the swarm, loads the two images into every node, labels every node ready and starts the readiness
// stand-in on the published readiness port. No keepalived service is started: start does that.
func newVIPHelperRig(t *testing.T) *vipHelperRig {
	t.Helper()
	host := vhHostClient(t)
	ictx, icancel := context.WithTimeout(context.Background(), time.Minute)
	defer icancel()
	if _, err := host.ImageInspect(ictx, vhImage); err != nil {
		t.Skipf("the keepalived image %s is not on this machine's engine (%v): run `make keepalived-image` first", vhImage, err)
	}
	sw := dockertest.StartSwarm(t, dockertest.EngineFromEnv(), 1, 2)
	dockertest.LoadImage(t, vhImage, sw.Nodes()...)
	dockertest.LoadImage(t, traefiktest.WhoamiImage, sw.Nodes()...)
	r := &vipHelperRig{t: t, sw: sw, host: host, mgr: sw.Managers[0].Client(t), nodeID: map[string]string{}}
	t.Cleanup(func() {
		if t.Failed() {
			r.dump()
		}
	})

	nres, err := host.NetworkInspect(ictx, sw.Network, client.NetworkInspectOptions{})
	if err != nil {
		t.Fatalf("inspect the nodes' network %s: %v", sw.Network, err)
	}
	for _, c := range nres.Network.IPAM.Config {
		if c.Subnet.IsValid() && c.Subnet.Addr().Is4() {
			a := c.Subnet.Addr().As4()
			a[3] = 250
			r.vip = netip.PrefixFrom(netip.AddrFrom4(a), c.Subnet.Bits())
			break
		}
	}
	if !r.vip.IsValid() {
		t.Fatalf("the nodes' network %s has no IPv4 subnet: %+v", sw.Network, nres.Network.IPAM)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, n := range sw.Nodes() {
		info, err := n.Info(ctx, t)
		if err != nil || info.NodeID == "" {
			t.Fatalf("read the swarm node ID of %s: %v", n.Hostname, err)
		}
		r.nodeID[n.Hostname] = info.NodeID
		if out, err := sw.Managers[0].Exec(ctx, "docker", "node", "update", "--label-add", vhReadyLabel+"=yes", n.Hostname); err != nil {
			t.Fatalf("label %s ready: %v\n%s", n.Hostname, err, out)
		}
	}
	grace := time.Second
	port := ingressbundle.ReadyPublishedPort
	ready, err := r.mgr.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: vhReadyService},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:           traefiktest.WhoamiImage,
				Args:            []string{"--port", strconv.Itoa(port)},
				StopGracePeriod: &grace,
			},
			Placement: &swarm.Placement{Constraints: []string{"node.labels." + vhReadyLabel + "==yes"}},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{Protocol: network.TCP, TargetPort: uint32(port), PublishedPort: uint32(port), PublishMode: swarm.PortConfigPublishModeHost},
		}},
	}})
	if err != nil {
		t.Fatalf("create the readiness stand-in: %v", err)
	}
	r.readyID = ready.ID
	dockertest.WaitRunningTasks(t, sw.Managers[0], ready.ID, len(sw.Nodes()), vhSettle)
	return r
}

// start creates the VRRP key secret and the keepalived service named name for addr and routerID, both from the production
// builders: the secret from KeepalivedSecretContent of 32 random bytes and the service from KeepalivedServiceSpec with the
// image of this machine. Only the name is changed after the spec is built, so a second service can run beside the first.
func (r *vipHelperRig) start(name string, addr netip.Prefix, routerID int) *vhService {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		r.t.Fatalf("make the key: %v", err)
	}
	content, err := ingressdeploy.KeepalivedSecretContent(raw)
	if err != nil {
		r.t.Fatalf("build the key secret: %v", err)
	}
	// Every service has an environment ID of its own, so its secret has a name of its own.
	secretName := ingressdeploy.KeepalivedSecretName(fmt.Sprintf("vh-%x", raw[:6]), 1)
	sec, err := r.mgr.SecretCreate(ctx, client.SecretCreateOptions{Spec: swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: secretName},
		Data:        content,
	}})
	if err != nil {
		r.t.Fatalf("create the secret %s: %v", secretName, err)
	}
	spec := ingressdeploy.KeepalivedServiceSpec(ingressdeploy.KeepalivedSpecInput{
		Image: vhImage, Address: addr, RouterID: routerID, SecretName: secretName,
	})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = sec.ID
	spec.Annotations.Name = name
	res, err := r.mgr.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec})
	if err != nil {
		r.t.Fatalf("create the keepalived service %s on engine %s: %v", name, r.sw.Engine, err)
	}
	return &vhService{name: name, id: res.ID, secretID: sec.ID, addr: addr, routerID: routerID}
}

// tasks waits until every node runs a task of the service.
func (r *vipHelperRig) tasks(svc *vhService) {
	r.t.Helper()
	dockertest.WaitRunningTasks(r.t, r.sw.Managers[0], svc.id, len(r.sw.Nodes()), vhSettle)
}

// has reports whether the node lists addr on any interface.
func (r *vipHelperRig) has(n *dockertest.Node, addr netip.Addr) bool {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := n.Exec(ctx, "ip", "-4", "-o", "addr", "show")
	if err != nil {
		return false
	}
	return strings.Contains(out, "inet "+addr.String()+"/")
}

// holders is every node that lists addr, including one whose keepalived was killed and left it (A-02).
func (r *vipHelperRig) holders(addr netip.Addr) []*dockertest.Node {
	r.t.Helper()
	var out []*dockertest.Node
	for _, n := range r.sw.Nodes() {
		if r.has(n, addr) {
			out = append(out, n)
		}
	}
	return out
}

func vhNames(ns []*dockertest.Node) string {
	var names []string
	for _, n := range ns {
		names = append(names, n.Hostname)
	}
	return "[" + strings.Join(names, " ") + "]"
}

// waitOneHolder waits until exactly one node other than not holds addr (every node when not is nil) and returns it with the
// seconds this call waited. A killed holder's node is passed as not: it keeps the address until its replacement starts, which
// is a separate observation.
func (r *vipHelperRig) waitOneHolder(addr netip.Addr, bound time.Duration, not *dockertest.Node) (*dockertest.Node, float64) {
	r.t.Helper()
	start := time.Now()
	var holder *dockertest.Node
	dockertest.Wait(r.t, bound, "exactly one node to hold "+addr.String(), func() (bool, string) {
		var got []*dockertest.Node
		for _, h := range r.holders(addr) {
			if h != not {
				got = append(got, h)
			}
		}
		if len(got) == 1 {
			holder = got[0]
			return true, ""
		}
		return false, "holders " + vhNames(got)
	})
	return holder, time.Since(start).Seconds() // wall-clock: a measurement printed for people, nothing is asserted on it
}

// containers is the IDs of the node's containers of the service, newest first. With running true only a running one counts.
func (r *vipHelperRig) containers(n *dockertest.Node, svc string, running bool) []string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{"docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.swarm.service.name=" + svc}
	if running {
		args = append(args, "--filter", "status=running")
	}
	out, err := n.Exec(ctx, args...)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// runningContainer is the ID of the node's running container of the service, "" while there is none.
func (r *vipHelperRig) runningContainer(n *dockertest.Node, svc string) string {
	if ids := r.containers(n, svc, true); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// containerLog is what one container's helper wrote on its standard output (its standard error is read by nobody: the
// event lines are the report), parsed line by line with ingressbundle.ParseVIPLine, which is what the server does with the
// same lines. A line it refuses fails the test, naming only its length: a node's log is untrusted text.
func (r *vipHelperRig) containerLog(n *dockertest.Node, id string) []ingressbundle.VIPEvent {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := n.Sh(ctx, "docker logs "+id+" 2>/dev/null")
	if err != nil {
		return nil // the container is gone
	}
	var events []ingressbundle.VIPEvent
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		ev, err := ingressbundle.ParseVIPLine([]byte(line))
		if err != nil {
			r.t.Fatalf("a log line of container %.12s on %s is not an event line the server accepts (%d bytes): %v", id, n.Hostname, len(line), err)
		}
		events = append(events, ev)
	}
	return events
}

// taskLog is the lines of the node's newest container of the service: the running task's, or the last one's while none runs.
func (r *vipHelperRig) taskLog(n *dockertest.Node, svc string) []ingressbundle.VIPEvent {
	r.t.Helper()
	ids := r.containers(n, svc, false)
	if len(ids) == 0 {
		return nil
	}
	return r.containerLog(n, ids[0])
}

// ifaceOf is the interface the node's own address is on, which is what the helper discovers inside the virtual address's
// prefix.
func (r *vipHelperRig) ifaceOf(n *dockertest.Node) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := n.Exec(ctx, "ip", "-4", "-o", "addr", "show")
	if err != nil {
		r.t.Fatalf("list the addresses of %s: %v", n.Hostname, err)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && strings.HasPrefix(f[3], n.IP+"/") {
			return f[1]
		}
	}
	r.t.Fatalf("%s lists no interface with its own address %s:\n%s", n.Hostname, n.IP, out)
	return ""
}

// vhLastState is the state of the last state line, "" when there is none.
func vhLastState(events []ingressbundle.VIPEvent) string {
	for _, e := range slices.Backward(events) {
		if e.Event == ingressbundle.VIPEventState {
			return e.State
		}
	}
	return ""
}

// dump is the failure report: the readiness stand-in's and the keepalived services' tasks, each node's addresses and each
// keepalived container's last event lines (the helper's own output, which holds no key).
func (r *vipHelperRig) dump() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := r.sw.Managers[0].Exec(ctx, "docker", "service", "ps", "--no-trunc", "--format", "{{.Name}} {{.Node}} {{.DesiredState}} {{.CurrentState}} {{.Error}}")
	r.t.Logf("tasks (err %v):\n%s", err, out)
	for _, n := range r.sw.Nodes() {
		out, err := n.Sh(ctx, "ip -4 -o addr; for c in $(docker ps -aq --filter label=com.docker.swarm.service.name="+ingress.ServiceNameKeepalived+"); do echo \"== container $c\"; docker logs --tail 30 $c 2>&1; done")
		r.t.Logf("%s (err %v):\n%s", n.Hostname, err, out)
	}
}

// TestVIPHelperElectsAndReports is D-29 and D-15, the tracer: on a three-node swarm the real image, run from the production
// service spec (its entrypoint, the interface discovery, the rendered configuration and the probe on the readiness port
// against the stand-in), elects one holder, and every task's log holds start and state lines that ingressbundle.ParseVIPLine
// accepts, with the node ID and host name the swarm templates expanded.
func TestVIPHelperElectsAndReports(t *testing.T) {
	r := newVIPHelperRig(t)
	svc := r.start(ingress.ServiceNameKeepalived, r.vip, vhRouterID)
	r.tasks(svc)

	holder, secs := r.waitOneHolder(r.vip.Addr(), vhFirstHolder, nil)
	t.Logf("S3 helper-elects: engine=%s seconds=%.1f holder=%s", r.sw.Engine, secs, holder.Hostname)

	for _, n := range r.sw.Nodes() {
		want := ingressbundle.VIPStateBackup
		if n == holder {
			want = ingressbundle.VIPStateMaster
		}
		iface := r.ifaceOf(n)
		var events []ingressbundle.VIPEvent
		dockertest.Wait(t, vhLogBound, n.Hostname+" to report its start and the state "+want, func() (bool, string) {
			events = r.taskLog(n, svc.name)
			started := slices.ContainsFunc(events, func(e ingressbundle.VIPEvent) bool { return e.Event == ingressbundle.VIPEventStart })
			return started && vhLastState(events) == want, fmt.Sprintf("%d lines, last state %q", len(events), vhLastState(events))
		})

		var seq uint64
		for _, e := range events {
			if e.Node != r.nodeID[n.Hostname] || e.Name != n.Hostname || e.Router != vhRouterID {
				t.Errorf("%s wrote a line for node %q name %q router %d, want node %q name %q router %d",
					n.Hostname, e.Node, e.Name, e.Router, r.nodeID[n.Hostname], n.Hostname, vhRouterID)
			}
			if e.Seq <= seq {
				t.Errorf("%s wrote sequence %d after %d", n.Hostname, e.Seq, seq)
			}
			seq = e.Seq
			if e.Event == ingressbundle.VIPEventStart && e.Iface != iface {
				t.Errorf("%s reported interface %q, want %q", n.Hostname, e.Iface, iface)
			}
			if e.Event == ingressbundle.VIPEventFault && e.Active {
				t.Errorf("%s reported a fault %q on a healthy swarm", n.Hostname, e.Reason)
			}
		}
		t.Logf("S3 helper-reports: engine=%s node=%s lines=%d iface=%s last=%s", r.sw.Engine, n.Hostname, len(events), iface, vhLastState(events))
	}
}
