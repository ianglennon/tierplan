// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/ingressrecon"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

const (
	// devIngressImage is what `make ingress-image` builds; the harness loads it on every node under the name a
	// development server's proxy service names.
	devIngressImage = "tierplan-ingress:dev"
	// testDomain is the one domain registered for the environment, and routeHost the hostname the stacks route.
	testDomain = "test.local"
	routeHost  = "app.test.local"

	// The bounds of the waits for what the engines and the proxies do on their own schedule. Each is a failure
	// deadline, never a pause: every wait returns the moment its condition holds.
	enrolBound  = 6 * time.Minute
	rollBound   = 10 * time.Minute
	serveBound  = 4 * time.Minute
	wipeBound   = 4 * time.Minute
	deployBound = 8 * time.Minute
)

// agentConn is one node's agent: its tunnel to the in-process hub, proxying Docker calls to the node's real engine.
type agentConn struct {
	node     *dockertest.Node
	sock     string
	facts    *wire.Facts
	identity tunneltest.Identity
	conn     *tunneltest.Conn
}

// swarmEnv is one dind swarm of a manager and a worker with TierPlan's server side in process: the tunnel hub the node
// agents dial, the ingress hub the proxies dial, the real stores, the reconciler and the ingress deployer. Only the
// agents (which are tunnel test connections proxying to the node's engine) and the HTTP layer are not production code.
type swarmEnv struct {
	t        *testing.T
	sw       *dockertest.Swarm
	mgr, wrk *dockertest.Node
	envID    string
	joinTok  string

	clock *tunneltest.Clock
	tun   *tunneltest.Server
	srv   *ingresstest.Server
	mon   *envs.Monitor
	esvc  *envs.Service
	agg   *envs.Aggregator
	clus  *envs.Clusters
	addr  *envs.AddressSpaces
	brk   *envs.Broker

	st       *store.Store
	vlt      *vault.Vault
	aud      *audit.Writer
	istore   *ingressstore.Store
	certs    *ingresscert.Store
	recon    *ingressrecon.Reconciler
	notifier *ingress.Notifier
	jobs     *jobs.Hub
	locks    *stackops.Locks
	repo     stackops.Repo
	ca       *traefiktest.CA
	log      *slog.Logger

	agents map[*dockertest.Node]*agentConn

	// registry and webImage are where the stacks' backend image is served from: the swarm's fixture registry.
	registry string
	webImage string

	// deploy is the ingress deployer, and stopDeploy ends its two loops and waits for them.
	deploy     *ingressdeploy.Service
	stopDeploy func()
	version    string

	ops  *stackOps
	deps stackops.Deps
}

// startSwarmEnv starts the swarm and everything around it. It does not enable ingress.
func startSwarmEnv(t *testing.T) *swarmEnv {
	t.Helper()
	e := &swarmEnv{t: t, agents: map[*dockertest.Node]*agentConn{}, version: "dev", log: slog.New(slog.DiscardHandler), ca: traefiktest.NewCA(t)}

	e.sw = dockertest.StartSwarm(t, "", 1, 1)
	e.mgr, e.wrk = e.sw.Managers[0], e.sw.Workers[0]
	for _, n := range e.sw.Nodes() {
		dockertest.LoadImage(t, devIngressImage, n)
		e.sh(n, "docker tag "+devIngressImage+" "+ingressdeploy.ImageRepository+":edge")
	}
	e.clock = tunneltest.NewClock(time.Now())
	e.notifier = ingress.NewNotifier()
	var istoreRef atomic.Pointer[ingressstore.Store]
	e.srv = ingresstest.Start(t, ingresstest.Options{
		Host: e.sw.Gateway,
		Configure: func(h *ingresshub.Hub) *ingresshub.Hub {
			// Bundles are delivered only while the environment's ingress is on, as in the server.
			return h.WithEnvChanges(e.notifier).WithDeliveryGate(func(ctx context.Context, envID string) bool {
				s := istoreRef.Load()
				if s == nil {
					return false
				}
				set, err := s.Settings(ctx, envID)
				return err == nil && set.Enabled
			})
		},
	})
	e.st = e.srv.Store
	var err error
	if e.vlt, err = vault.LoadOrCreate("", filepath.Dir(e.st.Path), e.log); err != nil {
		t.Fatalf("vault: %v", err)
	}
	e.aud = audit.NewWriter(e.st, e.vlt, e.log, nil)

	// The registry of environments, as the server builds it, over the store the tunnel and the ingress hub share.
	e.mon = envs.NewMonitor(e.log, e.clock.Now, nil)
	e.esvc = envs.NewService(e.st, e.aud, e.mon, "", "self-id", e.clock.Now)
	snaps := envs.NewSnapshots(e.mon, 0, e.clock.Now)
	evs := envs.NewHub(e.mon, snaps, e.log, e.clock.Now)
	e.agg = envs.NewAggregator(e.esvc, snaps, evs, e.clock.Now)
	e.brk = envs.NewBroker(e.esvc, e.clock.Now)
	e.clus = envs.NewClusters(e.esvc, snaps, envs.ClusterOptions{Now: e.clock.Now})
	e.esvc.WithClusters(e.clus)
	e.addr = envs.NewAddressSpaces(e.esvc, e.agg, e.clus, envs.AddressSpaceOptions{Now: e.clock.Now})
	t.Cleanup(func() {
		e.brk.Close()
		evs.Close()
		e.mon.StopAll()
		e.agg.Close()
	})
	e.tun = tunneltest.Start(t, tunneltest.Options{Clock: e.clock, Store: e.st, Observer: e.esvc, Notifier: e.brk})
	e.esvc.WithAgents(e.tun.Hub).WithHealth(envs.HealthConfig{ServerStart: e.clock.Now()})
	env := e.tun.NewEnvironment(t, "dind-swarm")
	e.envID, e.joinTok = env.ID, env.Token

	// The ingress model, the certificates and the reconciler that renders the bundle the hub delivers.
	e.istore = ingressstore.New(ingressstore.Deps{St: e.st, Vault: e.vlt, Notifier: e.notifier})
	istoreRef.Store(e.istore)
	e.certs = ingresscert.NewStore(ingresscert.StoreDeps{St: e.st, Vault: e.vlt, Notifier: e.notifier, Audit: e.aud})
	e.recon = ingressrecon.New(ingressrecon.Deps{Model: e.istore, Certs: e.certs, Notifier: e.notifier, Log: e.log})
	e.runReconciler()
	e.srv.Nodes.Set(e.envID, e.swarmNodeID(e.mgr), e.swarmNodeID(e.wrk))

	e.jobs = jobs.New(jobs.Options{})
	t.Cleanup(e.jobs.Close)
	e.locks = stackops.NewLocks()
	e.repo = stackops.Repo{Store: e.st, Vault: e.vlt, Now: time.Now}
	e.ops = &stackOps{st: e.istore}
	e.deps = stackops.Deps{
		Aggregator: e.agg, Clusters: e.clus, Checker: rbac.NewChecker(e.st), AddressSpaces: e.addr, Ingress: e.ops, Log: e.log,
	}

	// The agents dial in only now: the environment is a swarm once its first agent says so.
	for _, n := range e.sw.Nodes() {
		e.connectAgent(n)
	}
	if _, err := e.istore.CreateDomain(testsupport.Context(t), ingressstore.CreateDomainInput{
		EnvID: e.envID, Suffix: testDomain, Issuer: ingress.IssuerStaging, Actor: "user:test",
	}, nil); err != nil {
		t.Fatalf("register the domain %s: %v", testDomain, err)
	}
	e.startDeploy("dev")
	// Registered last, so it runs first, while the nodes still answer and the store is open.
	e.dumpOnFailure()
	return e
}

// sh runs a shell line in a node and fails the test when it exits non-zero.
func (e *swarmEnv) sh(n *dockertest.Node, line string) string {
	e.t.Helper()
	out, err := n.Sh(testsupport.Context(e.t), line)
	if err != nil {
		e.t.Fatalf("%v", err)
	}
	return out
}

// swarmNodeID is the node's ID in the swarm.
func (e *swarmEnv) swarmNodeID(n *dockertest.Node) string {
	e.t.Helper()
	info, err := n.Info(testsupport.Context(e.t), e.t)
	if err != nil {
		e.t.Fatalf("read the engine info of %s: %v", n.Hostname, err)
	}
	return info.NodeID
}

// dumpOnFailure prints what the swarm holds when the test fails: the services and tasks, the proxy's last log
// lines and the hub's rows. The join token lives in a swarm secret, whose value no command here prints, and the
// proxy's log carries none (the supervisor's own test proves it).
func (e *swarmEnv) dumpOnFailure() {
	e.t.Cleanup(func() {
		if !e.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		for _, line := range []string{
			"docker service ls 2>&1",
			"docker service ps --no-trunc " + ingress.ServiceName + " 2>&1",
			"docker service logs --tail 40 " + ingress.ServiceName + " 2>&1",
			"docker network ls 2>&1",
			"docker secret ls 2>&1",
		} {
			out, _ := e.mgr.Sh(ctx, line)
			e.t.Logf("%s: %s\n%s", e.mgr.Hostname, line, strings.TrimSpace(out))
		}
		for _, n := range e.sw.Nodes() {
			out, _ := n.Sh(ctx, "docker volume ls --format '{{.Name}}' 2>&1; docker ps -a --format '{{.Names}} {{.Status}}' 2>&1")
			e.t.Logf("%s: volumes and containers\n%s", n.Hostname, strings.TrimSpace(out))
		}
		if b, ok := e.recon.Current(e.envID); ok {
			e.t.Logf("the reconciler's current bundle %s holds %d route(s); skipped %v; render failures %v", b.ID, len(b.Manifest.Routes), e.recon.Skipped(e.envID), e.recon.Failures())
		}
		if nodes, err := e.srv.Service.ListNodes(ctx, e.envID); err == nil {
			for _, n := range nodes {
				e.t.Logf("hub row of %s: status=%s health=%s applied=%s rejected=%s (%s)", n.ID, n.Status, n.Health, n.AppliedBundleID, n.RejectedBundleID, n.RejectedReason)
			}
		}
	})
}

// runReconciler runs the reconciler and carries each new bundle it renders to the in-process hub, as the server hands
// the reconciler to the hub as its bundle source.
func (e *swarmEnv) runReconciler() {
	t := e.t
	sub := e.recon.Changes().Subscribe()
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.recon.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.C():
				for _, id := range sub.Pending() {
					if b, ok := e.recon.Current(id); ok {
						e.srv.Bundles.Set(id, b)
					} else {
						e.srv.Bundles.Remove(id)
					}
				}
			}
		}
	}()
	t.Cleanup(func() {
		stop()
		wg.Wait()
		sub.Close()
	})
}

// connectAgent dials the node's agent into the tunnel hub, with the facts the node's own engine reports, and waits until
// the monitor has probed the engine through the tunnel.
func (e *swarmEnv) connectAgent(n *dockertest.Node) {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	a := e.agents[n]
	if a == nil {
		info, err := n.Info(ctx, t)
		if err != nil {
			t.Fatalf("read the engine info of %s: %v", n.Hostname, err)
		}
		a = &agentConn{node: n, sock: dockertest.UnixProxy(t, n.Host()), facts: &wire.Facts{
			EngineID: info.ID, Hostname: info.Name, EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: info.State, NodeID: info.NodeID, ClusterID: info.ClusterID, Control: info.Control},
		}}
		e.agents[n] = a
	}
	spec := tunneltest.Agent{Facts: a.facts, LongLived: true}
	if a.conn == nil {
		spec.Token = e.joinTok
	} else {
		id := a.identity
		spec.Identity = &id
	}
	c := e.tun.Dial(t, spec)
	if c.Welcome == nil {
		t.Fatalf("the agent of %s was refused: %+v (err %v)", n.Hostname, c.Refuse, c.Err)
	}
	a.conn, a.identity = c, c.Identity
	c.ProxyDocker(t, a.sock)
	tunneltest.SendHeartbeat(t, c.OpenControl(t), true, e.clock.Now().UnixMilli())
	if _, err := e.mon.AwaitStatus(ctx, envs.AgentTarget(c.Identity.AgentID), func(s envs.Status) bool { return s.State != envs.StateUnknown }); err != nil {
		t.Fatalf("the monitor never probed the engine of %s through its agent: %v", n.Hostname, err)
	}
}

// dropAgent ends a node's tunnel from the agent's side and waits until the hub no longer holds its session, so the
// node reads as not connected to the deployer.
func (e *swarmEnv) dropAgent(n *dockertest.Node) {
	t := e.t
	t.Helper()
	a := e.agents[n]
	_ = a.conn.WS.CloseNow()
	if _, err := e.tun.Hub.AwaitSession(testsupport.Context(t), a.identity.AgentID, func(s *tunnel.Session) bool { return s == nil }); err != nil {
		t.Fatalf("the hub kept the session of %s after its agent left: %v", n.Hostname, err)
	}
}

// startDeploy builds the ingress deployer for a server of the given version and runs its two loops.
func (e *swarmEnv) startDeploy(version string) {
	t := e.t
	if e.stopDeploy != nil {
		e.stopDeploy()
	}
	e.version = version
	e.deploy = ingressdeploy.New(ingressdeploy.Deps{
		Engine: swarmEngine{agg: e.agg, clusters: e.clus}, Envs: envReader{st: e.st, svc: e.esvc},
		Store: e.istore, Notifier: e.notifier, Tokens: e.srv.Service, NodeRows: e.srv.Service, Nodes: e.srv.Service, Kicker: e.srv.Hub,
		CurrentBundle: func(envID string) (string, bool) {
			b, ok := e.recon.Current(envID)
			return b.ID, ok
		},
		Events: func(ctx context.Context) (<-chan envs.HealthEvent, func()) {
			_, ch, cancel := e.brk.Subscribe(ctx)
			return ch, cancel
		},
		Jobs: e.jobs, Locks: e.locks, Audit: e.aud, AddressSpaces: e.addr,
		ServerVersion: version, URL: e.srv.URL, Pins: func() []string { return []string{e.srv.Pin()} }, Log: e.log,
	})
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.deploy.Run(ctx) }()
	go func() { defer wg.Done(); e.deploy.RunWipes(ctx) }()
	var once sync.Once
	e.stopDeploy = func() { once.Do(func() { stop(); wg.Wait() }) }
	t.Cleanup(e.stopDeploy)
}

// enable switches ingress on and waits for the job.
func (e *swarmEnv) enable() jobs.Job {
	t := e.t
	t.Helper()
	id, err := e.deploy.Enable(testsupport.Context(t), testActor, testOwner, e.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	return e.waitJob(id)
}

// disable switches ingress off and waits for the job.
func (e *swarmEnv) disable() jobs.Job {
	t := e.t
	t.Helper()
	id, err := e.deploy.Disable(testsupport.Context(t), testActor, testOwner, e.envID, func(string) bool { return true })
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	return e.waitJob(id)
}

// waitJob waits for a job by the hub's own signal.
func (e *swarmEnv) waitJob(id string) jobs.Job {
	t := e.t
	t.Helper()
	job, err := e.jobs.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatalf("waiting for job %s: %v", id, err)
	}
	return job
}

// requireJobDone fails the test unless every item of the job is done, naming the item that was not.
func requireJobDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State == jobs.JobDone {
		return
	}
	var items []string
	for _, it := range job.Items {
		items = append(items, fmt.Sprintf("%s=%s:%s %s", it.ID, it.State, it.Code, it.Detail))
	}
	t.Fatalf("job %s ended %s: %s", job.Kind, job.State, strings.Join(items, "; "))
}

// awaitReady waits until the status reads every node expected, enrolled and ready.
func (e *swarmEnv) awaitReady() ingressdeploy.Status {
	t := e.t
	t.Helper()
	var last ingressdeploy.Status
	dockertest.Wait(t, enrolBound, "every proxy to enrol and apply the current bundle (nodes expected 2, enrolled 2, ready 2)", func() (bool, string) {
		st, err := e.deploy.Status(testsupport.Context(t), e.envID)
		if err != nil {
			return false, err.Error()
		}
		last = st
		return st.Nodes.Expected == 2 && st.Nodes.Enrolled == 2 && st.Nodes.Ready == 2, fmt.Sprintf("%+v", st.Nodes)
	})
	return last
}

// cluster is a manager's read of the swarm.
func (e *swarmEnv) cluster(fn func(ctx context.Context, c *dockerx.Client) error) error {
	ctx := testsupport.Context(e.t)
	return e.clus.ClusterRead(ctx, e.envID, func(c *dockerx.Client) error { return fn(ctx, c) })
}

// proxyService is the proxy service as the swarm holds it.
func (e *swarmEnv) proxyService() (dockerx.ServiceDetail, bool) {
	e.t.Helper()
	var d dockerx.ServiceDetail
	found := false
	err := e.cluster(func(ctx context.Context, c *dockerx.Client) error {
		var err error
		d, _, err = c.ServiceInspect(ctx, ingress.ServiceName)
		if errors.Is(err, dockerx.ErrServiceNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		e.t.Fatalf("inspect the proxy service: %v", err)
	}
	return d, found
}

// hasCacheVolume says whether the node holds the certificate cache volume, and whether TierPlan's component label is on it.
// It is one listing, so a wipe that runs while a test polls cannot fall between a question and its follow-up.
func (e *swarmEnv) hasCacheVolume(n *dockertest.Node) (present, labelled bool) {
	e.t.Helper()
	out, err := n.Sh(testsupport.Context(e.t), "docker volume ls --filter name=^"+ingress.VolumeName+"$ --format '{{.Name}} {{.Label \""+ingress.LabelComponent+"\"}}'")
	if err != nil {
		e.t.Fatalf("list the volumes of %s: %v", n.Hostname, err)
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return false, false
	}
	return true, len(f) == 2 && f[1] == ingress.ComponentIngress
}

// objects counts, on the swarm, what ingress leaves behind: the proxy service, the network and the join token secrets.
func (e *swarmEnv) objects() (svc, net, secrets int) {
	e.t.Helper()
	if _, ok := e.proxyService(); ok {
		svc = 1
	}
	if err := e.cluster(func(ctx context.Context, c *dockerx.Client) error {
		rows, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Name == ingress.NetworkName {
				net++
			}
		}
		secs, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, s := range secs {
			if ingress.OwnsSecret(s.Name, s.Labels) {
				secrets++
			}
		}
		return nil
	}); err != nil {
		e.t.Fatalf("read the swarm's objects: %v", err)
	}
	return svc, net, secrets
}

// swarmEngine reaches the swarm's engines through the real aggregator and cluster reader, as the server's adapter does.
type swarmEngine struct {
	agg      *envs.Aggregator
	clusters *envs.Clusters
}

func (e swarmEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	return e.agg.WriteOnManager(ctx, envID, fn)
}

func (e swarmEngine) WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	return e.agg.WriteOnNode(ctx, envID, nodeID, fn)
}

func (e swarmEngine) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	return e.clusters.ClusterRead(ctx, envID, fn)
}

func (e swarmEngine) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	return e.agg.OnNode(ctx, envID, nodeID, fn)
}

// envReader reads the facts of the environment the deployer decides on, from the registry of environments, as the
// server's reader does for an agent environment.
type envReader struct {
	st  *store.Store
	svc *envs.Service
}

func (r envReader) Info(ctx context.Context, envID string) (ingressdeploy.EnvInfo, error) {
	row, err := r.st.RQ.GetEnvironment(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return ingressdeploy.EnvInfo{}, envs.ErrNotFound
	}
	if err != nil {
		return ingressdeploy.EnvInfo{}, err
	}
	in, err := r.svc.Input(ctx, envID, time.Now())
	if err != nil {
		return ingressdeploy.EnvInfo{}, err
	}
	info := ingressdeploy.EnvInfo{ID: envID, Name: row.Name, ClusterID: row.SwarmClusterID.String, Swarm: in.EnvType == "swarm"}
	for _, ag := range in.Agents {
		if ag.Revoked {
			continue
		}
		info.Nodes = append(info.Nodes, ingressdeploy.NodeInfo{
			SwarmNodeID: ag.SwarmNodeID, AgentID: ag.ID, Name: ag.Node, Role: ag.Role, Connected: ag.Connected,
		})
		if ag.Role == "manager" && ag.Connected {
			info.ManagerConnected = true
		}
	}
	return info, nil
}

// stackOps adapts the ingress store to stackops.Ingress, as the API's adaptor does, minus its audit rows.
type stackOps struct{ st *ingressstore.Store }

func (o *stackOps) AttachFor(ctx context.Context, envID string, target stacks.Target) (*stacks.IngressAttach, error) {
	if target != stacks.TargetSwarm {
		return nil, nil
	}
	set, err := o.st.Settings(ctx, envID)
	if err != nil {
		return nil, err
	}
	if !set.Enabled {
		return nil, nil
	}
	return &stacks.IngressAttach{Network: ingress.NetworkName}, nil
}

func (o *stackOps) World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error) {
	return o.st.World(ctx, envID, canView)
}

func (o *stackOps) ReplaceStackRoutes(ctx context.Context, in ingressstore.ReplaceInput, _ audit.Actor) (ingress.Plan, error) {
	return o.st.ReplaceStackRoutes(ctx, in)
}

func (o *stackOps) ReleaseStack(ctx context.Context, envID, stackID string, actor audit.Actor) error {
	return o.st.ReleaseStack(ctx, envID, stackID, actor.ID, nil)
}

func (o *stackOps) Notify(envID string) { o.st.Notify(envID) }

func (o *stackOps) HeldRoutes(ctx context.Context, envID, stackID string) ([]ingress.Route, error) {
	snap, err := o.st.Snapshot(ctx, envID)
	if err != nil {
		return nil, err
	}
	var out []ingress.Route
	for _, r := range snap.Routes {
		if r.OwnerKind == ingress.OwnerStack && r.StackID == stackID {
			out = append(out, r.Route)
		}
	}
	return out, nil
}

func (o *stackOps) CertStates(context.Context, string, []string) (map[string]stackops.CertState, error) {
	return map[string]stackops.CertState{}, nil
}

var _ stackops.Ingress = (*stackOps)(nil)

// rootCaller is an instance admin, who may deploy anything.
func (e *swarmEnv) rootCaller() stackops.Caller {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	if _, err := e.st.Write.ExecContext(ctx, `INSERT OR IGNORE INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('root', 'root', 'x', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := e.st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, e.st.Q, "root"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return stackops.Caller{Principal: &rbac.Principal{Type: rbac.PrincipalUser, ID: "root", Name: "root"}, Actor: "root"}
}

// startRegistry runs the swarm's fixture registry and pushes the backend image into it, so a stack names it by digest.
func (e *swarmEnv) startRegistry() {
	t := e.t
	t.Helper()
	e.registry = dockertest.StartSwarmRegistry(t, e.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, e.mgr, dockertest.ServiceTestImage, e.registry+"/ingress/backend:1", nil)
	e.webImage = e.registry + "/ingress/backend@" + digest
}

// backendBody is what the backend answers, so a reply from it cannot be mistaken for the proxy's own.
const backendBody = "tierplan-ingress-backend"

// routedCompose is a one-service stack file whose service answers backendBody on port 80 and asks for host.
func (e *swarmEnv) routedCompose(host string) string {
	return "services:\n  web:\n    image: " + e.webImage + "\n" +
		"    command: [\"sh\", \"-c\", \"mkdir -p /www && echo " + backendBody + " > /www/index.html && exec httpd -f -p 80 -h /www\"]\n" +
		"    deploy:\n      labels:\n        tierplan.ingress.host: " + host + "\n        tierplan.ingress.port: \"80\"\n"
}

// stack is a stack the harness deployed: its record and the revision its deploy was recorded on.
type stack struct {
	stackops.Stack
	rev stackops.Snapshot
}

// deployStack stores a stack of the environment and deploys compose through stackops.DeploySwarm with a claim write, as the
// API's deploy does, ingress attachment included. It returns the result and the job, and marks the revision good when the
// deploy succeeded.
func (e *swarmEnv) deployStack(name, compose string) (*stack, stackops.DeployResult, jobs.Job) {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}}
	st, _, err := e.repo.CreateStack(ctx, e.envID, name, stacks.TargetSwarm, "user:test", copy)
	if err != nil {
		t.Fatalf("CreateStack %s: %v", name, err)
	}
	rev, err := e.repo.NewRevision(ctx, st.ID, "user:test", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	attach, err := e.ops.AttachFor(ctx, e.envID, stacks.TargetSwarm)
	if err != nil {
		t.Fatalf("AttachFor: %v", err)
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: name, Target: stacks.TargetSwarm, Copy: copy, Ingress: attach})
	if err != nil {
		t.Fatalf("the stack is not deployable: %v", err)
	}
	in := stackops.SwarmDeployInput{
		EnvID: e.envID, Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(copy.Files), Vars: map[string]string{}, Copy: copy,
		Caller: e.rootCaller(), Pins: e.repo, RevisionID: rev.ID,
		Ingress: &stackops.ClaimWrite{Decls: stacks.IngressDeclarations(loaded.Project, stacks.TargetSwarm), Actor: audit.Actor{Type: audit.ActorUser, ID: "user:test", Name: "Test User"}},
	}
	var res stackops.DeployResult
	done := make(chan struct{})
	job, err := e.jobs.Start(jobs.Spec{Kind: "stack_deploy", EnvID: e.envID, Owner: "user:test", Items: stackops.DeployItemSpecs()},
		func(jctx context.Context, rep *jobs.Reporter) {
			defer close(done)
			res = e.deps.DeploySwarm(jctx, in, rep)
		})
	if err != nil {
		t.Fatal(err)
	}
	ended := e.waitJob(job.ID)
	testsupport.Await(t, "the deploy function to return", done)
	out := &stack{Stack: st, rev: rev}
	if res.OK() {
		if err := e.repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
			t.Fatalf("SetOutcome: %v", err)
		}
	}
	return out, res, ended
}

// lifecycle runs a stop or a start of the stack as a job and returns its result.
func (e *swarmEnv) lifecycle(mode string, st *stack) stackops.LifecycleResult {
	t := e.t
	t.Helper()
	var res stackops.LifecycleResult
	done := make(chan struct{})
	job, err := e.jobs.Start(jobs.Spec{Kind: "stack_" + mode, EnvID: e.envID, Owner: "user:test", Items: stackops.LifecycleItemSpecs(mode, st.Target)},
		func(jctx context.Context, rep *jobs.Reporter) {
			defer close(done)
			in := stackops.LifecycleInput{EnvID: e.envID, Stack: st.Stack, Caller: e.rootCaller(), Store: e.repo}
			if mode == stackops.LifecycleStop {
				res = e.deps.Stop(jctx, in, rep)
			} else {
				res = e.deps.Start(jctx, in, rep)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	e.waitJob(job.ID)
	testsupport.Await(t, "the "+mode+" function to return", done)
	return res
}

// remove removes the stack, its definition with it, and returns the result.
func (e *swarmEnv) remove(st *stack) stackops.RemoveResult {
	t := e.t
	t.Helper()
	var res stackops.RemoveResult
	done := make(chan struct{})
	job, err := e.jobs.Start(jobs.Spec{Kind: "stack_remove", EnvID: e.envID, Owner: "user:test", Items: stackops.RemoveItemSpecs(st.Target, nil, false)},
		func(jctx context.Context, rep *jobs.Reporter) {
			defer close(done)
			res = e.deps.Remove(jctx, stackops.RemoveInput{EnvID: e.envID, Stack: st.Stack, Caller: e.rootCaller(), Store: e.repo, ClaimActor: audit.Actor{Type: audit.ActorUser, ID: "user:test", Name: "Test User"}}, rep)
		})
	if err != nil {
		t.Fatal(err)
	}
	e.waitJob(job.ID)
	testsupport.Await(t, "the removal function to return", done)
	return res
}

// jsonUnmarshal decodes a service spec read from the engine.
func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
