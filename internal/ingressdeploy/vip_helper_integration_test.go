// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client" //nolint:depguard // the dind rig talks to its own engines, as the dockerx integration tests do

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
				Image:           dockertest.NodeImage(traefiktest.WhoamiImage),
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
	spec.Name = name
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

// The bounds of the failover cases. vhMoved is VIP-02's "within seconds" (S3 measured 3.0 to 3.8 s for a failing probe), the
// others are failure deadlines for what the helper does on its own schedule: the statistics are read every 12 s and a
// foreign router is cleared by three quiet reads.
const (
	vhMoved        = 10 * time.Second
	vhKillMoved    = 30 * time.Second
	vhCleared      = 90 * time.Second
	vhForeignRaise = 60 * time.Second
	vhForeignClear = 90 * time.Second
	// vhHold is how long a case that must not change is watched.
	vhHold = 20 * time.Second
)

// vhFind returns the first event pred accepts.
func vhFind(events []ingressbundle.VIPEvent, pred func(ingressbundle.VIPEvent) bool) (ingressbundle.VIPEvent, bool) {
	i := slices.IndexFunc(events, pred)
	if i < 0 {
		return ingressbundle.VIPEvent{}, false
	}
	return events[i], true
}

// vhState is the predicate for a state line, with reason when it is not empty.
func vhState(state, reason string) func(ingressbundle.VIPEvent) bool {
	return func(e ingressbundle.VIPEvent) bool {
		return e.Event == ingressbundle.VIPEventState && e.State == state && (reason == "" || e.Reason == reason)
	}
}

// vhWatch polls violation every half second for window and returns the first violation, "" when there was none. It is for a
// case that must keep holding for a stretch of time, which the waits (they fail when the window ends) cannot express.
func vhWatch(window time.Duration, violation func() string) string {
	expired := time.NewTimer(window) // wall-clock: a bounded observation window of a case that must not change; it can only false-pass, never fail a correct run
	defer expired.Stop()
	tick := time.NewTicker(500 * time.Millisecond) // wall-clock: the pace of that observation
	defer tick.Stop()
	for {
		if v := violation(); v != "" {
			return v
		}
		select {
		case <-expired.C:
			return violation()
		case <-tick.C:
		}
	}
}

// setReady puts the readiness stand-in back on the node (its label) or takes it away, which fails the node's probe.
func (r *vipHelperRig) setReady(n *dockertest.Node, ready bool) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	flag := "--label-rm"
	label := vhReadyLabel
	if ready {
		flag, label = "--label-add", vhReadyLabel+"=yes"
	}
	if out, err := r.sw.Managers[0].Exec(ctx, "docker", "node", "update", flag, label, n.Hostname); err != nil {
		r.t.Fatalf("set %s ready=%v: %v\n%s", n.Hostname, ready, err, out)
	}
}

// kill sends SIGKILL to the container, which is what a crashed holder looks like: no goodbye advert, no cleanup.
func (r *vipHelperRig) kill(n *dockertest.Node, id string) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := n.Exec(ctx, "docker", "kill", "-s", "KILL", id); err != nil {
		r.t.Fatalf("kill %s on %s: %v\n%s", id, n.Hostname, err, out)
	}
}

// replacement waits until the node runs a container of the service other than old: swarm's replacement of a killed task.
func (r *vipHelperRig) replacement(n *dockertest.Node, svc, old string) string {
	r.t.Helper()
	var repl string
	dockertest.Wait(r.t, vhSettle, "a replacement keepalived task on "+n.Hostname, func() (bool, string) {
		repl = r.runningContainer(n, svc)
		return repl != "" && repl != old, "container " + repl
	})
	return repl
}

// TestVIPHelperFailover is D-29 and VIP-02 through the production spec. Removing the holder's readiness stand-in moves the
// address within vhMoved and the holder's log says FAULT with the reason probe_failing; the node that recovers stays BACKUP
// (nopreempt); a SIGKILL of the new holder's container moves the address again, and the node swarm replaces comes back as a
// BACKUP and stays one for a watch.
func TestVIPHelperFailover(t *testing.T) {
	r := newVIPHelperRig(t)
	svc := r.start(ingress.ServiceNameKeepalived, r.vip, vhRouterID)
	r.tasks(svc)
	addr := r.vip.Addr()

	a, _ := r.waitOneHolder(addr, vhFirstHolder, nil)
	aCtr := r.runningContainer(a, svc.name)

	// A failing probe on the holder.
	r.setReady(a, false)
	b, secs := r.waitOneHolder(addr, vhMoved, a)
	t.Logf("S3 helper-failing-probe: engine=%s seconds=%.1f from=%s to=%s", r.sw.Engine, secs, a.Hostname, b.Hostname)
	dockertest.Wait(t, vhLogBound, a.Hostname+" to say FAULT probe_failing and to drop the address", func() (bool, string) {
		_, said := vhFind(r.containerLog(a, aCtr), vhState(ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing))
		return said && !r.has(a, addr), fmt.Sprintf("said=%v listed=%v", said, r.has(a, addr))
	})

	// The probe passes again: the node comes back as a BACKUP and does not take the address back.
	r.setReady(a, true)
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.readyID, len(r.sw.Nodes()), vhSettle)
	dockertest.Wait(t, vhLogBound, a.Hostname+" to leave FAULT for BACKUP", func() (bool, string) {
		ev := r.containerLog(a, aCtr)
		return vhLastState(ev) == ingressbundle.VIPStateBackup, "last state " + vhLastState(ev)
	})
	if v := vhWatch(vhHold, func() string {
		switch {
		case r.has(a, addr):
			return a.Hostname + " took the address back"
		case !r.has(b, addr):
			return b.Hostname + " lost the address"
		}
		return ""
	}); v != "" {
		t.Fatalf("S3 helper-nopreempt: engine=%s violated: %s", r.sw.Engine, v)
	}
	t.Logf("S3 helper-nopreempt: engine=%s held", r.sw.Engine)

	// A crashed holder.
	bCtr := r.runningContainer(b, svc.name)
	r.kill(b, bCtr)
	c, secs := r.waitOneHolder(addr, vhKillMoved, b)
	t.Logf("S3 helper-killed-holder: engine=%s seconds=%.1f from=%s to=%s", r.sw.Engine, secs, b.Hostname, c.Hostname)

	// The killed node's replacement clears what the kill left (keepalived does, A-02), is a BACKUP and stays one.
	repl := r.replacement(b, svc.name, bCtr)
	dockertest.Wait(t, vhCleared, b.Hostname+" to lose the address its killed task left and "+c.Hostname+" to hold it alone", func() (bool, string) {
		got := r.holders(addr)
		return len(got) == 1 && got[0] == c, "holders " + vhNames(got)
	})
	dockertest.Wait(t, vhCleared, "the replacement on "+b.Hostname+" to report BACKUP", func() (bool, string) {
		ev := r.containerLog(b, repl)
		return vhLastState(ev) == ingressbundle.VIPStateBackup, "last state " + vhLastState(ev)
	})
	if v := vhWatch(vhHold, func() string {
		switch {
		case r.has(b, addr):
			return b.Hostname + " took the address back"
		case !r.has(c, addr):
			return c.Hostname + " lost the address"
		}
		return ""
	}); v != "" {
		t.Fatalf("S3 helper-nopreempt-after-kill: engine=%s violated: %s", r.sw.Engine, v)
	}
	if _, master := vhFind(r.containerLog(b, repl), vhState(ingressbundle.VIPStateMaster, "")); master {
		t.Fatalf("the replacement on %s became MASTER", b.Hostname)
	}
	t.Logf("S3 helper-nopreempt-after-kill: engine=%s held", r.sw.Engine)
}

// vhForeignConf is keepalived's configuration for the foreign router: the same router ID, its own key and another address,
// and no check (a check would fail there and keep the router in FAULT, silent).
func vhForeignConf(addr netip.Prefix, keyPath string, routerID int) string {
	return fmt.Sprintf(`vrrp_instance FOREIGN {
    state BACKUP
    interface eth0
    virtual_router_id %d
    priority 100
    advert_int 1
    auth_hmac {
        key 1 file:%s
        active_key 1
        mode enforce
    }
    virtual_ipaddress {
        %s dev eth0
    }
}
`, routerID, keyPath, addr)
}

// startForeign runs a second keepalived in a plain container on the nodes' bridge: the router ID of the swarm's service,
// another 32-byte key and another address. The configuration and key are put in with CopyToContainer before the start. The
// container is removed at cleanup. It returns the container ID and the address it was given on the bridge, which is what the
// holder's report should name as the sender.
func (r *vipHelperRig) startForeign(addr netip.Prefix, routerID int) (string, netip.Addr) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		r.t.Fatalf("make the foreign key: %v", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode int64
		data string
	}{
		{"etc/keepalived/foreign.key", 0o400, "hex:" + hex.EncodeToString(raw)},
		{"etc/keepalived/keepalived.conf", 0o444, vhForeignConf(addr, "/etc/keepalived/foreign.key", routerID)},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			r.t.Fatalf("tar %s: %v", f.name, err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			r.t.Fatalf("tar %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		r.t.Fatalf("close the tar: %v", err)
	}
	res, err := r.host.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "vh-foreign-" + hex.EncodeToString(raw[:4]),
		Config: &container.Config{
			Image:      vhImage,
			Entrypoint: []string{"/usr/sbin/keepalived"},
			Cmd:        []string{"--dont-fork", "--log-console", "--log-detail", "--use-file", "/etc/keepalived/keepalived.conf"},
			Tty:        true,
		},
		HostConfig:       &container.HostConfig{NetworkMode: container.NetworkMode(r.sw.Network), CapDrop: []string{"ALL"}, CapAdd: []string{"NET_ADMIN", "NET_RAW"}},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{r.sw.Network: {}}},
	})
	if err != nil {
		r.t.Fatalf("create the foreign router: %v", err)
	}
	id := res.ID
	r.removeAtCleanup(id)
	if _, err := r.host.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		r.t.Fatalf("put the foreign configuration in: %v", err)
	}
	if _, err := r.host.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		r.t.Fatalf("start the foreign router: %v", err)
	}
	insp, err := r.host.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect the foreign router: %v", err)
	}
	ep := insp.Container.NetworkSettings.Networks[r.sw.Network]
	if ep == nil || !ep.IPAddress.IsValid() {
		r.t.Fatalf("the foreign router has no address on %s: %+v", r.sw.Network, insp.Container.NetworkSettings.Networks)
	}
	return id, ep.IPAddress
}

// removeAtCleanup removes a container of this machine's engine when the test ends, whatever state it is in.
func (r *vipHelperRig) removeAtCleanup(id string) {
	r.t.Cleanup(func() { r.removeContainer(id) })
}

// removeContainer force-removes a container of this machine's engine.
func (r *vipHelperRig) removeContainer(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = r.host.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
}

// TestVIPHelperForeignRouter is A-04 through the production spec: a keepalived on the nodes' bridge with another key and the
// same router ID makes the holder's helper write foreign_router active true, naming the counter and the sender; removing it
// makes the helper write active false again.
func TestVIPHelperForeignRouter(t *testing.T) {
	r := newVIPHelperRig(t)
	svc := r.start(ingress.ServiceNameKeepalived, r.vip, vhRouterID)
	r.tasks(svc)
	holder, _ := r.waitOneHolder(r.vip.Addr(), vhFirstHolder, nil)
	ctr := r.runningContainer(holder, svc.name)
	if _, quiet := vhFind(r.containerLog(holder, ctr), func(e ingressbundle.VIPEvent) bool { return e.Event == ingressbundle.VIPEventForeignRouter }); quiet {
		t.Fatalf("%s reported a foreign router before there was one", holder.Hostname)
	}

	a := r.vip.Addr().As4()
	a[3] = 249
	foreignID, foreignIP := r.startForeign(netip.PrefixFrom(netip.AddrFrom4(a), r.vip.Bits()), vhRouterID)
	t.Logf("the foreign router %.12s runs at %s with router ID %d and its own key", foreignID, foreignIP, vhRouterID)

	var raised ingressbundle.VIPEvent
	start := time.Now()
	dockertest.Wait(t, vhForeignRaise, holder.Hostname+" to write foreign_router active", func() (bool, string) {
		ev, ok := vhFind(r.containerLog(holder, ctr), func(e ingressbundle.VIPEvent) bool {
			return e.Event == ingressbundle.VIPEventForeignRouter && e.Active
		})
		raised = ev
		return ok, "no active foreign_router line yet"
	})
	secs := time.Since(start).Seconds() // wall-clock: a measurement printed for people, nothing is asserted on it
	if raised.Counter != ingressbundle.VIPCounterInvalidHMAC {
		t.Errorf("the foreign router was reported with counter %q, want %q", raised.Counter, ingressbundle.VIPCounterInvalidHMAC)
	}
	sender := "ok"
	if raised.Addr != foreignIP.String() {
		sender = "absent"
		if raised.Addr != "" {
			t.Errorf("the foreign router was reported at %q, want %s", raised.Addr, foreignIP)
		}
	}
	t.Logf("S3 helper-foreign-router: engine=%s active seconds=%.1f counter=%s sender=%s", r.sw.Engine, secs, raised.Counter, sender)
	if !r.has(holder, r.vip.Addr()) {
		t.Errorf("%s lost the address to a foreign router", holder.Hostname)
	}

	r.removeContainer(foreignID)
	start = time.Now()
	dockertest.Wait(t, vhForeignClear, holder.Hostname+" to write foreign_router cleared", func() (bool, string) {
		_, ok := vhFind(r.containerLog(holder, ctr), func(e ingressbundle.VIPEvent) bool {
			return e.Event == ingressbundle.VIPEventForeignRouter && !e.Active && e.Seq > raised.Seq
		})
		return ok, "no cleared foreign_router line yet"
	})
	secs = time.Since(start).Seconds() // wall-clock: a measurement printed for people, nothing is asserted on it
	t.Logf("S3 helper-foreign-router: engine=%s cleared seconds=%.1f", r.sw.Engine, secs)
}

// taskCount is how many tasks the service has, running or not: a task that exited and was replaced is one more.
func (r *vipHelperRig) taskCount(svc *vhService) int {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	filters := make(client.Filters)
	filters.Add("service", svc.id)
	res, err := r.mgr.TaskList(ctx, client.TaskListOptions{Filters: filters})
	if err != nil {
		r.t.Fatalf("list the tasks of %s: %v", svc.name, err)
	}
	return len(res.Items)
}

// TestVIPHelperDiscoveryFault is D-09 through the production spec: a service whose address is outside every node's interfaces
// keeps its tasks running, and each writes one fault no_interface line and no MASTER state; the service beside it, whose
// address is on the nodes' network, is not disturbed.
func TestVIPHelperDiscoveryFault(t *testing.T) {
	r := newVIPHelperRig(t)
	good := r.start(ingress.ServiceNameKeepalived, r.vip, vhRouterID)
	lost := netip.MustParsePrefix("192.0.2.10/24")
	odd := r.start("tierplan-keepalived-nowhere", lost, vhRouterID+1)
	r.tasks(good)
	r.tasks(odd)
	r.waitOneHolder(r.vip.Addr(), vhFirstHolder, nil)

	for _, n := range r.sw.Nodes() {
		dockertest.Wait(t, vhLogBound, n.Hostname+" to write fault no_interface", func() (bool, string) {
			_, ok := vhFind(r.taskLog(n, odd.name), func(e ingressbundle.VIPEvent) bool {
				return e.Event == ingressbundle.VIPEventFault && e.Reason == ingressbundle.VIPReasonNoInterface && e.Active
			})
			return ok, "no fault line yet"
		})
	}
	// The helper looks again every ten seconds: watch past one look, and then count the lines.
	if v := vhWatch(15*time.Second, func() string { // wall-clock: a window that must be longer than the helper's rescan, so a repeated line can show
		if got := len(r.holders(lost.Addr())); got != 0 {
			return fmt.Sprintf("%d nodes list %s", got, lost.Addr())
		}
		return ""
	}); v != "" {
		t.Fatalf("S3 helper-discovery-fault: engine=%s violated: %s", r.sw.Engine, v)
	}
	for _, n := range r.sw.Nodes() {
		faults := 0
		for _, e := range r.taskLog(n, odd.name) {
			switch e.Event {
			case ingressbundle.VIPEventFault:
				faults++
				if e.Reason != ingressbundle.VIPReasonNoInterface || !e.Active {
					t.Errorf("%s wrote fault %q active=%v, want only no_interface active", n.Hostname, e.Reason, e.Active)
				}
			case ingressbundle.VIPEventState:
				t.Errorf("%s wrote state %s with no interface to hold the address on", n.Hostname, e.State)
			case ingressbundle.VIPEventStart:
				t.Errorf("%s wrote a start line with no interface", n.Hostname)
			}
		}
		if faults != 1 {
			t.Errorf("%s wrote %d fault lines, want exactly one", n.Hostname, faults)
		}
	}
	if got, want := r.taskCount(odd), len(r.sw.Nodes()); got != want {
		t.Errorf("the service with no interface has %d tasks, want %d: a task exited and was replaced", got, want)
	}
	t.Logf("S3 helper-discovery-fault: engine=%s tasks=%d fault=no_interface", r.sw.Engine, len(r.sw.Nodes()))
}

// The bounds of the address-in-use case (A-05). vhInUseHold is how long the nodes are watched while another machine holds the
// address, vhInUseReleased the wait for one holder once it is gone (the helper looks again every 12 s).
const (
	vhInUseHold     = 30 * time.Second
	vhInUseReleased = 45 * time.Second
	// vhRestarts is how many times the service is force-updated while a peer holds the address.
	vhRestarts = 3
)

// startSquatter runs a plain container on the nodes' bridge that holds addr as its own address, which is what another
// machine answering for the virtual address looks like. It is removed at cleanup.
func (r *vipHelperRig) startSquatter(addr netip.Addr) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := r.host.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "vh-squatter-" + hex.EncodeToString(randBytes(r.t, 4)),
		Config:     &container.Config{Image: traefiktest.WhoamiImage},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(r.sw.Network)},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			r.sw.Network: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: addr}},
		}},
	})
	if err != nil {
		r.t.Fatalf("create the container that holds %s: %v", addr, err)
	}
	r.removeAtCleanup(res.ID)
	if _, err := r.host.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		r.t.Fatalf("start the container that holds %s: %v", addr, err)
	}
	insp, err := r.host.ContainerInspect(ctx, res.ID, client.ContainerInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect the container that holds %s: %v", addr, err)
	}
	if ep := insp.Container.NetworkSettings.Networks[r.sw.Network]; ep == nil || ep.IPAddress != addr {
		r.t.Fatalf("the container was not given %s on %s: %+v", addr, r.sw.Network, insp.Container.NetworkSettings.Networks)
	}
	return res.ID
}

func randBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return b
}

// forceUpdate rolls the service over to new tasks with the same spec (the update configuration of the production spec: one
// node at a time, the old task first, ten seconds apart) and waits until every node runs a task of the new generation.
func (r *vipHelperRig) forceUpdate(svc *vhService) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cur, err := r.mgr.ServiceInspect(ctx, svc.id, client.ServiceInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect %s: %v", svc.name, err)
	}
	spec := cur.Service.Spec
	spec.TaskTemplate.ForceUpdate++
	want := spec.TaskTemplate.ForceUpdate
	if _, err := r.mgr.ServiceUpdate(ctx, svc.id, client.ServiceUpdateOptions{Version: cur.Service.Version, Spec: spec}); err != nil {
		r.t.Fatalf("update %s: %v", svc.name, err)
	}
	// The status of the update before still says completed for a moment, so the new generation is what is awaited first.
	dockertest.Wait(r.t, vhSettle, "every node to run a task of generation "+strconv.FormatUint(want, 10), func() (bool, string) {
		filters := make(client.Filters)
		filters.Add("service", svc.id)
		res, err := r.mgr.TaskList(ctx, client.TaskListOptions{Filters: filters})
		if err != nil {
			return false, err.Error()
		}
		running := 0
		for _, tk := range res.Items {
			if tk.DesiredState == swarm.TaskStateRunning && tk.Status.State == swarm.TaskStateRunning && tk.Spec.ForceUpdate == want {
				running++
			}
		}
		return running == len(r.sw.Nodes()), fmt.Sprintf("%d tasks of the new generation running", running)
	})
	dockertest.WaitServiceUpdateState(r.t, r.sw.Managers[0], svc.id, "completed", vhSettle)
}

// TestVIPHelperAddressInUse is D-11 and A-05 through the production spec, and the judgement of whether the check is reliable
// enough to hold nodes in FAULT (T-26-47). A plain container on the nodes' bridge holds the virtual address before the service
// starts: every node writes fault address_in_use and none holds the address for a watch. Once the container is gone one node
// holds it and the faults clear. Then, with a holder, the service is force-updated three times, so each node restarts while a
// peer holds the address: no restarted node may write address_in_use, because a peer's adverts are on the segment.
func TestVIPHelperAddressInUse(t *testing.T) {
	r := newVIPHelperRig(t)
	addr := r.vip.Addr()
	squatter := r.startSquatter(addr)
	svc := r.start(ingress.ServiceNameKeepalived, r.vip, vhRouterID)
	r.tasks(svc)

	inUse := func(e ingressbundle.VIPEvent) bool {
		return e.Event == ingressbundle.VIPEventFault && e.Reason == ingressbundle.VIPReasonAddressInUse
	}
	raised := map[string]ingressbundle.VIPEvent{}
	for _, n := range r.sw.Nodes() {
		dockertest.Wait(t, vhLogBound, n.Hostname+" to write fault address_in_use", func() (bool, string) {
			ev, ok := vhFind(r.taskLog(n, svc.name), func(e ingressbundle.VIPEvent) bool { return inUse(e) && e.Active })
			raised[n.Hostname] = ev
			return ok, "no active address_in_use line yet"
		})
	}
	if v := vhWatch(vhInUseHold, func() string {
		if got := r.holders(addr); len(got) != 0 {
			return vhNames(got) + " hold the address another machine answers for"
		}
		for _, n := range r.sw.Nodes() {
			if _, master := vhFind(r.taskLog(n, svc.name), vhState(ingressbundle.VIPStateMaster, "")); master {
				return n.Hostname + " became MASTER"
			}
		}
		return ""
	}); v != "" {
		t.Fatalf("S3 helper-address-in-use: engine=%s violated: %s", r.sw.Engine, v)
	}
	t.Logf("S3 helper-address-in-use: engine=%s held-out nodes=%d watch=%s", r.sw.Engine, len(r.sw.Nodes()), vhInUseHold)

	// The other machine goes away: one node holds the address and every fault clears.
	r.removeContainer(squatter)
	holder, secs := r.waitOneHolder(addr, vhInUseReleased, nil)
	for _, n := range r.sw.Nodes() {
		dockertest.Wait(t, vhInUseReleased, n.Hostname+" to clear fault address_in_use", func() (bool, string) {
			_, ok := vhFind(r.taskLog(n, svc.name), func(e ingressbundle.VIPEvent) bool {
				return inUse(e) && !e.Active && e.Seq > raised[n.Hostname].Seq
			})
			return ok, "no cleared address_in_use line yet"
		})
	}
	t.Logf("S3 helper-address-in-use: engine=%s released seconds=%.1f holder=%s", r.sw.Engine, secs, holder.Hostname)

	// A node that restarts while a peer holds the address is never told the address is in use.
	baseline := map[string]bool{}
	for _, n := range r.sw.Nodes() {
		for _, id := range r.containers(n, svc.name, false) {
			baseline[id] = true
		}
	}
	for i := 1; i <= vhRestarts; i++ {
		r.forceUpdate(svc)
		for _, n := range r.sw.Nodes() {
			// A task that reported BACKUP or MASTER has finished its check, which runs before keepalived starts.
			dockertest.Wait(t, vhLogBound, n.Hostname+" to finish the check of restart "+strconv.Itoa(i), func() (bool, string) {
				s := vhLastState(r.taskLog(n, svc.name))
				return s == ingressbundle.VIPStateBackup || s == ingressbundle.VIPStateMaster, "last state " + s
			})
			for _, id := range r.containers(n, svc.name, false) {
				if baseline[id] {
					continue
				}
				if _, flagged := vhFind(r.containerLog(n, id), inUse); flagged {
					t.Fatalf("S3 helper-address-in-use: engine=%s FLAGGED restart=%d node=%s: a restarted node was told the address is in use while a peer held it", r.sw.Engine, i, n.Hostname)
				}
			}
		}
		r.waitOneHolder(addr, vhFirstHolder, nil)
	}
	t.Logf("S3 helper-address-in-use: engine=%s restarts=%d flagged=0", r.sw.Engine, vhRestarts)
}
