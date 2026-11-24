// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
)

// Spike S3, leg 1a (D-01): what the virtual IP's design assumes about failover, measured on a real dind swarm with the
// real keepalived image (tierplan-keepalived:dev, `make keepalived-image`). keepalived runs here with a test configuration
// (the entrypoint overridden), so nothing depends on the helper that later plans add. The check script is the existing
// `tierplan-ingress healthcheck`, which asks 127.0.0.1:8083/readyz; a global whoami service stands in for the proxy's
// readiness endpoint and a node's label decides whether it runs there, so a test can fail one node's check.
const (
	// s3Image is what `make keepalived-image` builds on this machine's engine.
	s3Image = "tierplan-keepalived:dev"
	// s3Service is the keepalived service, s3Ready the readiness stand-in.
	s3Service = "s3-keepalived"
	s3Ready   = "s3-ready"
	// s3ReadyLabel is the node label the stand-in is constrained to: s3ReadyLabel=yes on a node means its check passes.
	s3ReadyLabel = "s3ready"
	// s3ReadyPort is the port tierplan-ingress healthcheck asks.
	s3ReadyPort = 8083
	// s3Settle bounds the setup waits that follow a pull or a service start.
	s3Settle = 3 * time.Minute
	// s3FirstHolder bounds the wait for the first holder once every task runs (about 5.5 s measured), s3Moved the wait for
	// another node to take the address of a killed holder.
	s3FirstHolder = 60 * time.Second
	s3Moved       = 30 * time.Second
)

// s3Seq makes the names of the swarm configs and secrets of one test unique.
var s3Seq atomic.Int64

// s3KeyError matches the lines keepalived logs when it cannot use the auth_hmac key.
var s3KeyError = regexp.MustCompile(`auth_hmac[^\n]*(cannot|readable by|too large|not in range|needs an id|must be|duplicate)`)

// s3Opts is what a test changes in the keepalived service. The zero value is the production shape: interface eth0, the
// key as a swarm secret at its default target under /run/secrets, the swarm's own restart delay.
type s3Opts struct {
	iface string
	// secretFile is the secret's File.Name, keyPath the path the configuration reads the key from.
	secretFile, keyPath string
	// restartDelay is the restart policy's delay, so a test can keep a killed node's replacement from starting.
	restartDelay time.Duration
}

func (o s3Opts) withDefaults() s3Opts {
	if o.iface == "" {
		o.iface = "eth0"
	}
	if o.secretFile == "" {
		o.secretFile = "s3-key"
	}
	if o.keyPath == "" {
		o.keyPath = "/run/secrets/s3-key"
	}
	return o
}

// s3Rig is a three-node dind swarm (one manager, two workers) with the readiness stand-in on every node, the address the
// virtual IP would hold, and the keepalived service once a test starts it.
type s3Rig struct {
	t     testing.TB
	sw    *dockertest.Swarm
	host  *client.Client
	mgr   *client.Client
	vip   netip.Prefix
	svcID string
	// key is the secret's content: "hex:" and 64 hex digits, which keepalived reads as 32 bytes (Pitfall 2).
	key string
	// secret is the swarm secret reference of the running service.
	secret *swarm.SecretReference
}

// with is the rig for a subtest: the same swarm and service, failing the subtest's t.
func (r *s3Rig) with(t testing.TB) *s3Rig {
	c := *r
	c.t = t
	return &c
}

// s3HostClient is a moby client to this machine's own engine.
func s3HostClient(t testing.TB) *client.Client {
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

// newS3Swarm starts the swarm, loads the two images into every node, labels every node ready and starts the readiness
// stand-in. The keepalived service is not started: start does that.
func newS3Swarm(t *testing.T) *s3Rig {
	t.Helper()
	host := s3HostClient(t)
	ictx, icancel := context.WithTimeout(context.Background(), time.Minute)
	defer icancel()
	if _, err := host.ImageInspect(ictx, s3Image); err != nil {
		t.Skipf("the keepalived image %s is not on this machine's engine (%v): run `make keepalived-image` first", s3Image, err)
	}
	sw := dockertest.StartSwarm(t, dockertest.EngineFromEnv(), 1, 2)
	dockertest.LoadImage(t, s3Image, sw.Nodes()...)
	dockertest.LoadImage(t, traefiktest.WhoamiImage, sw.Nodes()...)
	r := &s3Rig{t: t, sw: sw, host: host, mgr: sw.Managers[0].Client(t)}
	t.Cleanup(func() {
		if t.Failed() {
			r.dump()
		}
	})

	// The address: .250 of the nodes' network, which no node holds (the harness hands out .10 up).
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

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("make the key: %v", err)
	}
	r.key = "hex:" + hex.EncodeToString(raw)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, n := range sw.Nodes() {
		if out, err := sw.Managers[0].Exec(ctx, "docker", "node", "update", "--label-add", s3ReadyLabel+"=yes", n.Hostname); err != nil {
			t.Fatalf("label %s ready: %v\n%s", n.Hostname, err, out)
		}
	}
	grace := time.Second
	ready, err := r.mgr.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: s3Ready},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:           traefiktest.WhoamiImage,
				Args:            []string{"--port", strconv.Itoa(s3ReadyPort)},
				StopGracePeriod: &grace,
			},
			Placement: &swarm.Placement{Constraints: []string{"node.labels." + s3ReadyLabel + "==yes"}},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{Protocol: network.TCP, TargetPort: s3ReadyPort, PublishedPort: s3ReadyPort, PublishMode: swarm.PortConfigPublishModeHost},
		}},
	}})
	if err != nil {
		t.Fatalf("create the readiness stand-in: %v", err)
	}
	dockertest.WaitRunningTasks(t, sw.Managers[0], ready.ID, len(sw.Nodes()), s3Settle)
	return r
}

// s3Conf is keepalived's configuration for the test: RESEARCH Pattern 1 without the notify FIFO. The instance is named TP,
// as the helper will name it, the key is read from a file, and the check is the existing healthcheck.
func s3Conf(iface string, vip netip.Prefix, keyPath string) string {
	return fmt.Sprintf(`global_defs {
    enable_script_security
    script_user root
    vrrp_garp_master_refresh 60
}
vrrp_script chk_proxy {
    script "/usr/local/bin/tierplan-ingress healthcheck"
    interval 1
    timeout 1
    fall 2
    rise 2
}
vrrp_instance TP {
    state BACKUP
    interface %[1]s
    virtual_router_id 51
    priority 100
    advert_int 1
    nopreempt
    auth_hmac {
        key 1 file:%[3]s
        active_key 1
        mode enforce
    }
    virtual_ipaddress {
        %[2]s dev %[1]s
    }
    track_script { chk_proxy }
}
`, iface, vip, keyPath)
}

// config creates a swarm config holding the configuration for o and returns the reference that mounts it where
// keepalived reads it.
func (r *s3Rig) config(ctx context.Context, o s3Opts) *swarm.ConfigReference {
	r.t.Helper()
	name := fmt.Sprintf("s3-conf-%d", s3Seq.Add(1))
	res, err := r.mgr.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name},
		Data:        []byte(s3Conf(o.iface, r.vip, o.keyPath)),
	}})
	if err != nil {
		r.t.Fatalf("create the config %s: %v", name, err)
	}
	return &swarm.ConfigReference{
		ConfigID: res.ID, ConfigName: name,
		File: &swarm.ConfigReferenceFileTarget{Name: "/etc/keepalived/keepalived.conf", UID: "0", GID: "0", Mode: 0o444},
	}
}

// spec is the keepalived service as the design will run it: global, on the host network, every capability dropped but
// the four it needs, a read-only root with tmpfs on /run and /tmp, the key a secret of mode 0400. The command overrides
// the image's entrypoint to run keepalived itself.
func (r *s3Rig) spec(o s3Opts, cfg *swarm.ConfigReference) swarm.ServiceSpec {
	grace := 10 * time.Second
	var restart *swarm.RestartPolicy
	if o.restartDelay > 0 {
		d := o.restartDelay
		restart = &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, Delay: &d}
	}
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: s3Service},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:          s3Image,
				Command:        []string{"/usr/sbin/keepalived"},
				Args:           []string{"--dont-fork", "--log-console", "--log-detail", "--use-file", "/etc/keepalived/keepalived.conf"},
				CapabilityDrop: []string{"ALL"},
				CapabilityAdd:  []string{"NET_ADMIN", "NET_RAW", "SETUID", "SETGID"},
				ReadOnly:       true,
				Mounts: []mount.Mount{
					{Type: mount.TypeTmpfs, Target: "/run"},
					{Type: mount.TypeTmpfs, Target: "/tmp"},
				},
				Secrets:         []*swarm.SecretReference{r.secret},
				Configs:         []*swarm.ConfigReference{cfg},
				StopGracePeriod: &grace,
			},
			Networks:      []swarm.NetworkAttachmentConfig{{Target: "host"}},
			RestartPolicy: restart,
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
	}
}

// start creates the secret, the configuration and the keepalived service for o. An engine that refuses the spec is
// recorded as `S3 engine<N>: refused <reason>` and fails the test.
func (r *s3Rig) start(o s3Opts) {
	r.t.Helper()
	o = o.withDefaults()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("s3-key-%d", s3Seq.Add(1))
	sec, err := r.mgr.SecretCreate(ctx, client.SecretCreateOptions{Spec: swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name},
		Data:        []byte(r.key),
	}})
	if err != nil {
		r.t.Fatalf("create the secret %s: %v", name, err)
	}
	r.secret = &swarm.SecretReference{
		SecretID: sec.ID, SecretName: name,
		File: &swarm.SecretReferenceFileTarget{Name: o.secretFile, UID: "0", GID: "0", Mode: 0o400},
	}
	res, err := r.mgr.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: r.spec(o, r.config(ctx, o))})
	if err != nil {
		r.t.Logf("S3 engine%s: refused %v", r.sw.Engine, err)
		r.t.Fatalf("create the keepalived service on engine %s: %v", r.sw.Engine, err)
	}
	r.svcID = res.ID
}

// updateConfig rolls the service over to a new configuration for o and waits for the update to complete.
func (r *s3Rig) updateConfig(o s3Opts) {
	r.t.Helper()
	o = o.withDefaults()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := r.config(ctx, o)
	cur, err := r.mgr.ServiceInspect(ctx, r.svcID, client.ServiceInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect the keepalived service: %v", err)
	}
	spec := cur.Service.Spec
	spec.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{cfg}
	if _, err := r.mgr.ServiceUpdate(ctx, r.svcID, client.ServiceUpdateOptions{Version: cur.Service.Version, Spec: spec}); err != nil {
		r.t.Fatalf("update the keepalived service: %v", err)
	}
	dockertest.WaitServiceUpdateState(r.t, r.sw.Managers[0], r.svcID, "completed", s3Settle)
}

// remove takes the keepalived service and its secret away and waits until no node has a keepalived container or the
// address, so the next service on the same swarm starts from a clean host.
func (r *s3Rig) remove() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := r.mgr.ServiceRemove(ctx, r.svcID, client.ServiceRemoveOptions{}); err != nil {
		r.t.Fatalf("remove the keepalived service: %v", err)
	}
	dockertest.Wait(r.t, s3Settle, "every keepalived container to be gone and the address released", func() (bool, string) {
		var left []string
		for _, n := range r.sw.Nodes() {
			if id := r.container(n, false); id != "" {
				left = append(left, n.Hostname+" container "+id)
			}
			if r.has(n, "") {
				left = append(left, n.Hostname+" holds the address")
			}
		}
		return len(left) == 0, strings.Join(left, "; ")
	})
	if _, err := r.mgr.SecretRemove(ctx, r.secret.SecretID, client.SecretRemoveOptions{}); err != nil {
		r.t.Fatalf("remove the secret: %v", err)
	}
}

// has reports whether the node lists the address on dev ("" for every interface).
func (r *s3Rig) has(n *dockertest.Node, dev string) bool {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{"ip", "-4", "-o", "addr", "show"}
	if dev != "" {
		args = append(args, "dev", dev)
	}
	out, err := n.Exec(ctx, args...)
	if err != nil {
		return false
	}
	return strings.Contains(out, "inet "+r.vip.Addr().String()+"/")
}

// holders is every node that lists the address on any interface, including one whose keepalived was killed and left it.
func (r *s3Rig) holders() []*dockertest.Node {
	r.t.Helper()
	var out []*dockertest.Node
	for _, n := range r.sw.Nodes() {
		if r.has(n, "") {
			out = append(out, n)
		}
	}
	return out
}

func s3Names(ns []*dockertest.Node) string {
	var names []string
	for _, n := range ns {
		names = append(names, n.Hostname)
	}
	return "[" + strings.Join(names, " ") + "]"
}

// waitOneHolder waits until exactly one node other than not holds the address (every node when not is nil) and returns it
// with the seconds this call waited. A killed holder's node is passed as not: it keeps the address until its replacement
// starts (A-02), which is a separate observation. The seconds come from a poll a few hundred milliseconds wide.
func (r *s3Rig) waitOneHolder(bound time.Duration, not *dockertest.Node) (*dockertest.Node, float64) {
	r.t.Helper()
	start := time.Now()
	var holder *dockertest.Node
	dockertest.Wait(r.t, bound, "exactly one node to hold "+r.vip.Addr().String(), func() (bool, string) {
		var got []*dockertest.Node
		for _, h := range r.holders() {
			if h != not {
				got = append(got, h)
			}
		}
		if len(got) == 1 {
			holder = got[0]
			return true, ""
		}
		return false, "holders " + s3Names(got)
	})
	return holder, time.Since(start).Seconds() // wall-clock: a measurement printed for people, nothing is asserted on it
}

// container is the ID of the node's running keepalived task container, or "" when there is none. With running false it
// also counts a stopped one that has not been removed yet.
func (r *s3Rig) container(n *dockertest.Node, running bool) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{"docker", "ps", "-q", "--no-trunc", "--filter", "label=com.docker.swarm.service.name=" + s3Service}
	if running {
		args = append(args, "--filter", "status=running")
	}
	out, err := n.Exec(ctx, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
}

// taskContainer is the ID of the node's running keepalived task container, "" while there is none.
func (r *s3Rig) taskContainer(n *dockertest.Node) string {
	return r.container(n, true)
}

// kill sends SIGKILL to the container, which is what a crashed holder looks like: no goodbye advert, no cleanup.
func (r *s3Rig) kill(n *dockertest.Node, id string) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := n.Exec(ctx, "docker", "kill", "-s", "KILL", id); err != nil {
		r.t.Fatalf("kill %s on %s: %v\n%s", id, n.Hostname, err, out)
	}
}

// logs is what the container wrote, for a state line or a key error.
func (r *s3Rig) logs(n *dockertest.Node, id string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, _ := n.Exec(ctx, "docker", "logs", id)
	return out
}

// until polls cond until it is true or the window ends and reports which. It is for a case that is recorded rather than
// asserted, which testsupport's waits cannot express because they fail the test when the window ends.
func s3Until(window time.Duration, cond func() bool) bool {
	expired := time.NewTimer(window) // wall-clock: a bounded observation window, not a wait a correct run depends on
	defer expired.Stop()
	tick := time.NewTicker(500 * time.Millisecond) // wall-clock: the pace of that observation
	defer tick.Stop()
	for {
		if cond() {
			return true
		}
		select {
		case <-expired.C:
			return cond()
		case <-tick.C:
		}
	}
}

// secondNetwork makes a second bridge network on this machine's engine, a fresh /24 of 10.128.0.0/9 as the harness does for
// the first, and connects every node to it, so each node's engine sees an eth1. It disconnects and removes the network at
// cleanup, before the nodes go.
func (r *s3Rig) secondNetwork() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var id string
	var lastErr error
	for range 8 {
		hi, err := rand.Int(rand.Reader, big.NewInt(1<<15))
		if err != nil {
			r.t.Fatalf("pick a subnet: %v", err)
		}
		n := int(hi.Int64())
		subnet := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(128 + n>>8&0x7f), byte(n & 0xff), 0}), 24)
		gw := netip.AddrFrom4([4]byte{10, byte(128 + n>>8&0x7f), byte(n & 0xff), 1})
		res, err := r.host.NetworkCreate(ctx, "s3-second-"+hex.EncodeToString(raw2(2)), client.NetworkCreateOptions{
			Driver: "bridge",
			IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet, Gateway: gw}}},
		})
		if err == nil {
			id = res.ID
			break
		}
		lastErr = err
	}
	if id == "" {
		r.t.Fatalf("create the second network: %v", lastErr)
	}
	// The node containers are the endpoints of the first network, found by the address each node was given.
	first, err := r.host.NetworkInspect(ctx, r.sw.Network, client.NetworkInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect %s: %v", r.sw.Network, err)
	}
	var ctrs []string
	for _, n := range r.sw.Nodes() {
		found := ""
		for cid, ep := range first.Network.Containers {
			if ep.IPv4Address.Addr().String() == n.IP {
				found = cid
			}
		}
		if found == "" {
			r.t.Fatalf("no container of %s (%s) on %s", n.Hostname, n.IP, r.sw.Network)
		}
		ctrs = append(ctrs, found)
	}
	r.t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		for _, c := range ctrs {
			_, _ = r.host.NetworkDisconnect(cctx, id, client.NetworkDisconnectOptions{Container: c, Force: true})
		}
		_, _ = r.host.NetworkRemove(cctx, id, client.NetworkRemoveOptions{})
	})
	for _, c := range ctrs {
		if _, err := r.host.NetworkConnect(ctx, id, client.NetworkConnectOptions{Container: c}); err != nil {
			r.t.Fatalf("connect %s to the second network: %v", c, err)
		}
	}
}

func raw2(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// dump is the failure report: the service's tasks, each node's addresses and each keepalived container's log tail.
func (r *s3Rig) dump() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if r.svcID != "" {
		out, err := r.sw.Managers[0].Exec(ctx, "docker", "service", "ps", "--no-trunc", s3Service)
		r.t.Logf("tasks of %s (err %v):\n%s", s3Service, err, out)
	}
	for _, n := range r.sw.Nodes() {
		out, err := n.Sh(ctx, "ip -4 -o addr; for c in $(docker ps -aq --filter label=com.docker.swarm.service.name="+s3Service+"); do echo \"== container $c\"; docker logs --tail 40 $c 2>&1; done")
		r.t.Logf("%s (err %v):\n%s", n.Hostname, err, out)
	}
}
