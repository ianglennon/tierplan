// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// EngineFromEnv is the dind major version the tests run against:
// TIERPLAN_TEST_ENGINE, or 29.
func EngineFromEnv() string {
	if v := os.Getenv(EnvEngine); v != "" {
		return v
	}
	return defaultEngine
}

// Node is one privileged docker:<engine>-dind container, a Docker host of its
// own. Everything the agent tests run "on a host" runs inside one.
type Node struct {
	// Hostname is what the engine inside reports as its name (Info.Name).
	Hostname string
	// IP is the address on the network the nodes share. It is fixed, so a node
	// that is stopped and started again keeps the address its swarm knows.
	IP string
	// Gateway is the network's gateway: the address of this machine as the
	// nodes see it, which is where an in-process TierPlan is reachable from them.
	Gateway string
	// Manager is true for a swarm manager.
	Manager bool

	ctr testcontainers.Container

	mu   sync.Mutex
	host string // tcp://localhost:<published port>
}

// Swarm is a set of nodes that are one Docker Swarm.
type Swarm struct {
	Managers, Workers []*Node
	// Network is the name of the Docker network the nodes share.
	Network string
	// Gateway is the network's gateway address.
	Gateway string
	// Engine is the dind major version.
	Engine string
}

// Nodes is every node, managers first.
func (s *Swarm) Nodes() []*Node {
	return append(append([]*Node{}, s.Managers...), s.Workers...)
}

// netPlan is a network the harness created and the addresses it hands out.
type netPlan struct {
	name    string
	gateway netip.Addr
	subnet  netip.Prefix
	next    byte
}

// ip hands out the next fixed address, from .10 up.
func (p *netPlan) ip() netip.Addr {
	a := p.subnet.Addr().As4()
	a[3] = 10 + p.next
	p.next++
	return netip.AddrFrom4(a)
}

// newNetwork creates a bridge network on the machine's own engine with a /24
// the harness picked, so every node can have a fixed address. The subnet is a
// random one inside 10.128.0.0/9; a clash with an existing network makes the
// create fail and another is tried.
func newNetwork(t testing.TB) *netPlan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var lastErr error
	for range 8 {
		hi, err := rand.Int(rand.Reader, big.NewInt(1<<15))
		if err != nil {
			t.Fatalf("pick a subnet: %v", err)
		}
		n := int(hi.Int64())
		subnet := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(128 + n>>8&0x7f), byte(n & 0xff), 0}), 24)
		gw := netip.AddrFrom4([4]byte{10, byte(128 + n>>8&0x7f), byte(n & 0xff), 1})
		nw, err := tcnetwork.New(ctx, tcnetwork.CustomizeNetworkOption(func(o *client.NetworkCreateOptions) error {
			o.IPAM = &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet, Gateway: gw}}}
			return nil
		}))
		if err != nil {
			lastErr = err
			continue
		}
		testcontainers.CleanupNetwork(t, nw)
		return &netPlan{name: nw.Name, gateway: gw, subnet: subnet}
	}
	t.Fatalf("create a test network: %v", lastErr)
	return nil
}

// StartSwarm starts managers + workers dind nodes on a fresh network, makes the
// first manager `docker swarm init` on its own address, joins the rest with the
// tokens it prints, and returns once a leader is elected and every node is
// ready (polling for up to a minute each, never sleeping a fixed time). Engine
// "" means TIERPLAN_TEST_ENGINE. Everything is removed when the test ends.
func StartSwarm(t testing.TB, engine string, managers, workers int) *Swarm {
	t.Helper()
	if managers < 1 {
		t.Fatal("a swarm needs a manager")
	}
	if engine == "" {
		engine = EngineFromEnv()
	}
	plan := newNetwork(t)
	sw := &Swarm{Network: plan.name, Gateway: plan.gateway.String(), Engine: engine}

	total := managers + workers
	nodes := make([]*Node, total)
	ips := make([]netip.Addr, total)
	for i := range ips {
		ips[i] = plan.ip()
	}
	var wg sync.WaitGroup
	errs := make([]error, total)
	for i := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			role, idx := "m", i+1
			if i >= managers {
				role, idx = "w", i-managers+1
			}
			nodes[i], errs[i] = startNode(t, engine, plan, ips[i], fmt.Sprintf("cm-%s%d-%s", role, idx, randHex(2)), i < managers)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("start the dind nodes: %v", err)
	}
	sw.Managers, sw.Workers = nodes[:managers], nodes[managers:]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	first := sw.Managers[0]
	if out, err := first.Exec(ctx, "docker", "swarm", "init", "--advertise-addr", first.IP); err != nil {
		t.Fatalf("swarm init on %s: %v\n%s", first.Hostname, err, out)
	}
	tokens := map[bool]string{}
	for _, manager := range []bool{true, false} {
		role := "worker"
		if manager {
			role = "manager"
		}
		out, err := first.Exec(ctx, "docker", "swarm", "join-token", "-q", role)
		if err != nil {
			t.Fatalf("read the %s join token: %v\n%s", role, err, out)
		}
		tokens[manager] = strings.TrimSpace(out)
	}
	for _, n := range sw.Nodes()[1:] {
		out, err := n.Exec(ctx, "docker", "swarm", "join", "--token", tokens[n.Manager], "--advertise-addr", n.IP, first.IP+":2377")
		if err != nil {
			t.Fatalf("swarm join on %s: %v\n%s", n.Hostname, err, out)
		}
	}
	if err := sw.WaitForLeader(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sw.WaitForQuorum(ctx); err != nil {
		t.Fatal(err)
	}
	return sw
}

// StartHost starts one dind node on a network of its own, for a standalone
// host. It ignores TIERPLAN_TEST_DOCKER_HOST: an agent test needs a network its
// in-process TierPlan is reachable on, which the CI dind service is not.
func StartHost(t testing.TB, engine string) *Node {
	t.Helper()
	if engine == "" {
		engine = EngineFromEnv()
	}
	plan := newNetwork(t)
	n, err := startNode(t, engine, plan, plan.ip(), "cm-h1-"+randHex(2), false)
	if err != nil {
		t.Fatalf("start the dind host: %v", err)
	}
	return n
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// startNode starts one dind container with a fixed address and waits for its
// daemon. Registering the cleanup here lets concurrent starts clean up after
// each other. ghcr.io resolves to the node itself, so a swarm service that
// names the release image fails to resolve its digest at once instead of
// reaching out to a registry: the harness loads the image the tests run.
func startNode(t testing.TB, engine string, plan *netPlan, ip netip.Addr, hostname string, manager bool) (*Node, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "docker:" + engine + "-dind",
			ExposedPorts: []string{dindPort},
			Env:          map[string]string{"DOCKER_TLS_CERTDIR": ""},
			Networks:     []string{plan.name},
			ConfigModifier: func(c *container.Config) {
				c.Hostname = hostname
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Privileged = true
				hc.ExtraHosts = append(hc.ExtraHosts, "ghcr.io:127.0.0.1")
			},
			EndpointSettingsModifier: func(m map[string]*network.EndpointSettings) {
				if es := m[plan.name]; es != nil {
					es.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: ip}
				}
			},
			WaitingFor: wait.ForHTTP("/_ping").WithPort(dindPort).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		return nil, fmt.Errorf("start docker:%s-dind %s: %w", engine, hostname, err)
	}
	host, err := ctr.PortEndpoint(ctx, dindPort, "tcp")
	if err != nil {
		return nil, fmt.Errorf("find the dind port of %s: %w", hostname, err)
	}
	return &Node{Hostname: hostname, IP: ip.String(), Gateway: plan.gateway.String(), Manager: manager, ctr: ctr, host: host}, nil
}

// Host is the node's engine address on this machine, such as
// tcp://localhost:32771, for RawClient. It changes when the node is restarted.
func (n *Node) Host() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.host
}

// Client is a moby client to the node's engine, for test setup and for
// checking what the engine says.
func (n *Node) Client(t testing.TB) *client.Client {
	t.Helper()
	return RawClient(t, n.Host())
}

// EngineInfo is the part of a node's /info the identity checks read.
type EngineInfo struct {
	// ID is the engine ID, Name the engine's hostname.
	ID, Name string
	// NodeID is the swarm node ID, empty outside a swarm.
	NodeID string
	// State is Swarm.LocalNodeState and Control Swarm.ControlAvailable.
	State   string
	Control bool
	// ClusterID is Swarm.Cluster.ID, which only a manager reports.
	ClusterID string
}

// Info reads the node's /info.
func (n *Node) Info(ctx context.Context, t testing.TB) (EngineInfo, error) {
	in, err := n.Client(t).Info(ctx, client.InfoOptions{})
	if err != nil {
		return EngineInfo{}, fmt.Errorf("/info of %s: %w", n.Hostname, err)
	}
	out := EngineInfo{
		ID: in.Info.ID, Name: in.Info.Name, NodeID: in.Info.Swarm.NodeID,
		State: string(in.Info.Swarm.LocalNodeState), Control: in.Info.Swarm.ControlAvailable,
	}
	if in.Info.Swarm.Cluster != nil {
		out.ClusterID = in.Info.Swarm.Cluster.ID
	}
	return out, nil
}

// Exec runs a command inside the node (as root, so `docker ...` talks to the
// engine in the node) and returns its combined output. A non-zero exit is an
// error that carries the output.
func (n *Node) Exec(ctx context.Context, cmd ...string) (string, error) {
	code, r, err := n.ctr.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("exec %q on %s: %w", strings.Join(cmd, " "), n.Hostname, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read the output of %q on %s: %w", strings.Join(cmd, " "), n.Hostname, err)
	}
	if code != 0 {
		return string(b), fmt.Errorf("%q on %s exited %d: %s", strings.Join(cmd, " "), n.Hostname, code, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

// Sh runs a shell command line inside the node, as the generated install
// commands are meant to be pasted.
func (n *Node) Sh(ctx context.Context, line string) (string, error) {
	return n.Exec(ctx, "sh", "-c", line)
}

// WriteFile puts a file in the node, for a stack file or a compose file the
// install generates. The content travels base64-encoded inside the command, so
// no quoting of it can go wrong; keep it small.
func (n *Node) WriteFile(ctx context.Context, path string, content []byte) error {
	enc := base64.StdEncoding.EncodeToString(content)
	if _, err := n.Exec(ctx, "sh", "-c", "echo "+enc+" | base64 -d > "+path); err != nil {
		return fmt.Errorf("write %s on %s: %w", path, n.Hostname, err)
	}
	return nil
}

// Logs is the dind container's own log, for a failure report.
func (n *Node) Logs(ctx context.Context) string {
	r, err := n.ctr.Logs(ctx)
	if err != nil {
		return fmt.Sprintf("(no logs: %v)", err)
	}
	defer func() { _ = r.Close() }()
	b, _ := io.ReadAll(r)
	return string(b)
}

// StopNode stops the node's container, which takes its engine, and with it
// every container and swarm task in it, down.
func StopNode(ctx context.Context, n *Node) error {
	timeout := 15 * time.Second
	if err := n.ctr.Stop(ctx, &timeout); err != nil {
		return fmt.Errorf("stop %s: %w", n.Hostname, err)
	}
	return nil
}

// StartNode starts a stopped node and returns when its engine answers again. It
// keeps its address on the network, so a swarm that knew it takes it back.
func StartNode(ctx context.Context, n *Node) error {
	if err := n.ctr.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", n.Hostname, err)
	}
	host, err := n.ctr.PortEndpoint(ctx, dindPort, "tcp")
	if err != nil {
		return fmt.Errorf("find the dind port of %s: %w", n.Hostname, err)
	}
	n.mu.Lock()
	n.host = host
	n.mu.Unlock()
	return poll(ctx, time.Minute, n.Hostname+"'s engine to answer /_ping", func() (bool, string) {
		return pingOK(ctx, host), ""
	})
}

func pingOK(ctx context.Context, host string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+strings.TrimPrefix(host, "tcp://")+"/_ping", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// nodeLS reads `docker node ls` from the first manager that answers: one line
// per node, "<hostname>|<status>|<availability>|<manager status>".
func (s *Swarm) nodeLS(ctx context.Context) (string, error) {
	var last error
	for _, m := range s.Managers {
		out, err := m.Exec(ctx, "docker", "node", "ls", "--format", "{{.Hostname}}|{{.Status}}|{{.Availability}}|{{.ManagerStatus}}")
		if err == nil {
			return out, nil
		}
		last = err
	}
	return "", last
}

// WaitForLeader polls `docker node ls` once a second, for up to a minute, until
// one manager is the Leader.
func (s *Swarm) WaitForLeader(ctx context.Context) error {
	return poll(ctx, time.Minute, "the swarm to elect a leader", func() (bool, string) {
		out, err := s.nodeLS(ctx)
		if err != nil {
			return false, err.Error()
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.HasSuffix(line, "|Leader") {
				return true, ""
			}
		}
		return false, out
	})
}

// WaitForQuorum polls until every node the swarm knows is Ready and every
// manager is the Leader or Reachable, for up to a minute. A node that was
// stopped makes it wait; start it first, or do not call this.
func (s *Swarm) WaitForQuorum(ctx context.Context) error {
	want := len(s.Managers) + len(s.Workers)
	return poll(ctx, time.Minute, "every node to be ready and every manager reachable", func() (bool, string) {
		out, err := s.nodeLS(ctx)
		if err != nil {
			return false, err.Error()
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != want {
			return false, out
		}
		managers := 0
		for _, line := range lines {
			f := strings.Split(line, "|")
			if len(f) != 4 || f[1] != "Ready" {
				return false, out
			}
			if f[3] != "" {
				if f[3] != "Leader" && f[3] != "Reachable" {
					return false, out
				}
				managers++
			}
		}
		return managers == len(s.Managers), out
	})
}

// poll calls fn until it reports true or timeout (or ctx) ends, on testsupport.Poll, so
// there is one polling implementation and one failure text. The string is the latest
// observation, shown when it times out. It returns the failure rather than failing a
// test, for the setup paths that report it themselves.
func poll(ctx context.Context, timeout time.Duration, what string, fn func() (bool, string)) error {
	rec := &pollFailure{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testsupport.Poll(rec, timeout, what, func() (bool, string) {
			if err := ctx.Err(); err != nil {
				rec.fail(fmt.Sprintf("gave up waiting for %s: %v", what, err))
			}
			return fn()
		})
	}()
	<-done
	return rec.err
}

// pollFailure is the testing.TB testsupport.Poll fails, so poll can hand the failure
// back as an error. Fatalf ends the goroutine Poll runs in, as a real test's would.
type pollFailure struct {
	testing.TB
	err error
}

func (p *pollFailure) Helper() {}

func (p *pollFailure) Fatalf(format string, args ...any) { p.fail(fmt.Sprintf(format, args...)) }

func (p *pollFailure) fail(msg string) {
	p.err = errors.New(msg)
	runtime.Goexit()
}

// Wait is poll for the tests: it fails the test with the last observation when
// cond is still false after timeout.
func Wait(t testing.TB, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	testsupport.Poll(t, timeout, what, cond)
}
