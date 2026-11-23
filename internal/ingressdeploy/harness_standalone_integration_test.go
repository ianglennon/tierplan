// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
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
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

const (
	// releaseRegistry is the registry the proxy image is published to, and releaseFixture the host's container that plays it.
	releaseRegistry = "ghcr.io"
	releaseFixture  = "tp-release-registry"
	releaseDir      = "/tmp/tp-release"
)

// hostEnv is one dind host with TierPlan's server side in process, as swarmEnv is for a swarm: the ingress hub the proxy
// dials, the real stores, the reconciler, the stack pipeline and the ingress deployer. The host is reached either through a
// tunnel test agent (an agent standalone environment) or through the server's own local socket (a proxy to the host's
// engine). Only the agent and the HTTP layer are not production code.
type hostEnv struct {
	t     *testing.T
	local bool
	host  *dockertest.Node
	envID string
	// nodeID is the ID writes to the host are routed by: the agent's ID, or the environment's own for the local socket.
	nodeID  string
	joinTok string

	now   func() time.Time
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

	// registry and webImage are where the stacks' backend image is served from: a registry on the host itself.
	registry string
	webImage string

	deploy     *ingressdeploy.Service
	stopDeploy func()

	ops  *hostStackOps
	deps stackops.Deps
}

// startHostEnv starts the host and everything around it. It does not enable ingress. With local set the environment is
// TierPlan's own local socket, pointed at the host's engine; otherwise it is an agent environment whose one agent proxies to it.
func startHostEnv(t *testing.T, local bool) *hostEnv {
	t.Helper()
	e := &hostEnv{t: t, local: local, now: time.Now, log: slog.New(slog.DiscardHandler), ca: traefiktest.NewCA(t)}
	// The Compose SDK's process-wide state: its private configuration directory and its logging.
	if err := dockerx.InitCompose(t.TempDir(), e.log); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	e.host = dockertest.StartHost(t, "")
	e.serveReleaseImage()
	if !local {
		e.clock = tunneltest.NewClock(time.Now())
		e.now = e.clock.Now
	}
	e.notifier = ingress.NewNotifier()
	var istoreRef atomic.Pointer[ingressstore.Store]
	e.srv = ingresstest.Start(t, ingresstest.Options{
		Host: e.host.Gateway,
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

	dockerHost := ""
	if local {
		dockerHost = "unix://" + dockertest.UnixProxy(t, e.host.Host())
	}
	e.mon = envs.NewMonitor(e.log, e.now, nil)
	e.esvc = envs.NewService(e.st, e.aud, e.mon, dockerHost, "self-id", e.now)
	snaps := envs.NewSnapshots(e.mon, 0, e.now)
	evs := envs.NewHub(e.mon, snaps, e.log, e.now)
	e.agg = envs.NewAggregator(e.esvc, snaps, evs, e.now)
	e.brk = envs.NewBroker(e.esvc, e.now)
	e.clus = envs.NewClusters(e.esvc, snaps, envs.ClusterOptions{Now: e.now})
	e.esvc.WithClusters(e.clus)
	e.addr = envs.NewAddressSpaces(e.esvc, e.agg, e.clus, envs.AddressSpaceOptions{Now: e.now})
	t.Cleanup(func() {
		e.brk.Close()
		evs.Close()
		e.mon.StopAll()
		e.agg.Close()
	})
	if local {
		env, err := e.esvc.AddLocal(testsupport.Context(t), audit.SystemCLI, "", false, "", "")
		if err != nil {
			t.Fatalf("AddLocal: %v", err)
		}
		e.envID, e.nodeID = env.ID, env.ID
		if _, err := e.mon.AwaitStatus(testsupport.Context(t), env.ID, func(s envs.Status) bool { return s.State == envs.StateOnline }); err != nil {
			t.Fatalf("waiting for the local environment to come online: %v", err)
		}
	} else {
		e.tun = tunneltest.Start(t, tunneltest.Options{Clock: e.clock, Store: e.st, Observer: e.esvc, Notifier: e.brk})
		e.esvc.WithAgents(e.tun.Hub).WithHealth(envs.HealthConfig{ServerStart: e.clock.Now()})
		env := e.tun.NewEnvironment(t, "dind-host")
		e.envID, e.joinTok = env.ID, env.Token
	}

	// The ingress model, the certificates and the reconciler that renders the bundle the hub delivers.
	e.istore = ingressstore.New(ingressstore.Deps{St: e.st, Vault: e.vlt, Notifier: e.notifier})
	istoreRef.Store(e.istore)
	e.certs = ingresscert.NewStore(ingresscert.StoreDeps{St: e.st, Vault: e.vlt, Notifier: e.notifier, Audit: e.aud})
	e.recon = ingressrecon.New(ingressrecon.Deps{Model: e.istore, Certs: e.certs, Notifier: e.notifier, Log: e.log})
	e.runReconciler()

	e.jobs = jobs.New(jobs.Options{})
	t.Cleanup(e.jobs.Close)
	e.locks = stackops.NewLocks()
	e.repo = stackops.Repo{Store: e.st, Vault: e.vlt, Now: time.Now}
	e.ops = &hostStackOps{st: e.istore}
	e.deps = stackops.Deps{
		Aggregator: e.agg, Clusters: e.clus, Checker: rbac.NewChecker(e.st), AddressSpaces: e.addr, Ingress: e.ops, Log: e.log,
	}

	// The agent dials in only now: the environment is a standalone host once its first agent says so.
	if !local {
		e.connectAgent()
	}
	if _, err := e.istore.CreateDomain(testsupport.Context(t), ingressstore.CreateDomainInput{
		EnvID: e.envID, Suffix: testDomain, Issuer: ingress.IssuerStaging, Actor: "user:test",
	}, nil); err != nil {
		t.Fatalf("register the domain %s: %v", testDomain, err)
	}
	e.startDeploy()
	// Registered last, so it runs first, while the host still answers and the store is open.
	e.dumpOnFailure()
	return e
}

// serveReleaseImage makes the host's engine find the proxy image where a real one would: in the release registry. The
// registry is a container of the host on its default bridge, listening on 443 over TLS and not published; the engine trusts
// the test CA for the release registry's name and its own name for it points at the container, so the lookup of the image's digest, the pre-pull and the up all go to it and never
// leave the host. It is not on the loopback, which the proxy's published ports would collide with.
func (e *hostEnv) serveReleaseImage() {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	dockertest.LoadImage(t, devIngressImage, e.host)
	dockertest.LoadImage(t, dockertest.RegistryImage, e.host)
	image := ingressdeploy.ImageRepository + ":edge"
	e.sh("docker tag " + devIngressImage + " " + image)
	// The registry serves a certificate of the test CA for the release registry's name, which the engine is told to trust.
	chain, key := e.ca.Leaf(t, releaseRegistry)
	e.sh("mkdir -p " + releaseDir + " /etc/docker/certs.d/" + releaseRegistry)
	for path, data := range map[string][]byte{
		releaseDir + "/tls.crt": chain, releaseDir + "/tls.key": key, "/etc/docker/certs.d/" + releaseRegistry + "/ca.crt": e.ca.PEM(),
	} {
		if err := e.host.WriteFile(ctx, path, data); err != nil {
			t.Fatalf("write %s on the host: %v", path, err)
		}
	}
	e.sh("docker run -d --name " + releaseFixture + " -e REGISTRY_HTTP_ADDR=0.0.0.0:443 " +
		"-e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key " +
		"-v " + releaseDir + "/tls.crt:/certs/tls.crt:ro -v " + releaseDir + "/tls.key:/certs/tls.key:ro " + dockertest.RegistryImage)
	ip := strings.TrimSpace(e.sh("docker inspect -f '{{(index .NetworkSettings.Networks \"bridge\").IPAddress}}' " + releaseFixture))
	dockertest.Wait(t, 90*time.Second, "the release registry fixture to listen on 443", func() (bool, string) {
		out, err := e.host.Sh(ctx, "nc -z "+ip+" 443")
		return err == nil, strings.TrimSpace(out)
	})
	// The container's hosts file is the one the engine inside reads; the release registry's name was set to the loopback.
	e.sh("grep -v '" + releaseRegistry + "' /etc/hosts > /tmp/hosts.new; echo '" + ip + " " + releaseRegistry + "' >> /tmp/hosts.new; cat /tmp/hosts.new > /etc/hosts")
	// The engine re-reads its hosts file on its own schedule, so the first push may still go to the old address.
	dockertest.Wait(t, 2*time.Minute, "the engine to push the proxy image to the release registry fixture", func() (bool, string) {
		out, err := e.host.Sh(ctx, "docker push "+image)
		return err == nil, lastLine(out)
	})
}

// lastLine is the last non-empty line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// sh runs a shell line on the host and fails the test when it exits non-zero.
func (e *hostEnv) sh(line string) string {
	e.t.Helper()
	out, err := e.host.Sh(testsupport.Context(e.t), line)
	if err != nil {
		e.t.Fatalf("%v", err)
	}
	return out
}

// dumpOnFailure prints what the host holds when the test fails: its containers, networks and volumes, the proxy's last log
// lines and the hub's rows. The join token lives in a mounted secret file, which no command here prints, and the proxy's log
// carries none (the supervisor's own test proves it).
func (e *hostEnv) dumpOnFailure() {
	e.t.Cleanup(func() {
		if !e.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		for _, line := range []string{
			"docker ps -a --format '{{.Names}} {{.Status}} {{.Image}}' 2>&1",
			"docker network ls 2>&1",
			"docker volume ls --format '{{.Name}}' 2>&1",
			"docker logs --tail 40 " + ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1 2>&1",
		} {
			out, _ := e.host.Sh(ctx, line)
			e.t.Logf("%s: %s\n%s", e.host.Hostname, line, strings.TrimSpace(out))
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
func (e *hostEnv) runReconciler() {
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
	e.t.Cleanup(func() {
		stop()
		wg.Wait()
		sub.Close()
	})
}

// connectAgent dials the host's agent into the tunnel hub, with the facts the host's own engine reports (no swarm), and waits
// until the monitor has probed the engine through the tunnel.
func (e *hostEnv) connectAgent() {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	info, err := e.host.Info(ctx, t)
	if err != nil {
		t.Fatalf("read the engine info of %s: %v", e.host.Hostname, err)
	}
	sock := dockertest.UnixProxy(t, e.host.Host())
	c := e.tun.Dial(t, tunneltest.Agent{
		Facts: &wire.Facts{EngineID: info.ID, Hostname: info.Name, EngineReachable: true}, LongLived: true, Token: e.joinTok,
	})
	if c.Welcome == nil {
		t.Fatalf("the agent of %s was refused: %+v (err %v)", e.host.Hostname, c.Refuse, c.Err)
	}
	e.nodeID = c.Identity.AgentID
	c.ProxyDocker(t, sock)
	tunneltest.SendHeartbeat(t, c.OpenControl(t), true, e.clock.Now().UnixMilli())
	if _, err := e.mon.AwaitStatus(ctx, envs.AgentTarget(c.Identity.AgentID), func(s envs.Status) bool { return s.State != envs.StateUnknown }); err != nil {
		t.Fatalf("the monitor never probed the engine of %s through its agent: %v", e.host.Hostname, err)
	}
}

// startDeploy builds the ingress deployer and runs its two loops.
func (e *hostEnv) startDeploy() {
	t := e.t
	e.deploy = ingressdeploy.New(ingressdeploy.Deps{
		Engine: swarmEngine{agg: e.agg, clusters: e.clus}, Envs: hostEnvReader{st: e.st, svc: e.esvc},
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
		// The deployer's own stack pipeline for the proxy's project: no Ingress, so the project is never attached or claimed.
		System:        stackops.Deps{Aggregator: e.agg, AddressSpaces: e.addr, Log: e.log},
		ServerVersion: "dev", URL: e.srv.URL, Pins: func() []string { return []string{e.srv.Pin()} }, Log: e.log,
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
func (e *hostEnv) enable() jobs.Job {
	t := e.t
	t.Helper()
	id, err := e.deploy.Enable(testsupport.Context(t), testActor, testOwner, e.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	return e.waitJob(id)
}

// disable switches ingress off and waits for the job.
func (e *hostEnv) disable() jobs.Job {
	t := e.t
	t.Helper()
	id, err := e.deploy.Disable(testsupport.Context(t), testActor, testOwner, e.envID, func(string) bool { return true })
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	return e.waitJob(id)
}

// waitJob waits for a job by the hub's own signal.
func (e *hostEnv) waitJob(id string) jobs.Job {
	t := e.t
	t.Helper()
	job, err := e.jobs.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatalf("waiting for job %s: %v", id, err)
	}
	return job
}

// awaitReady waits until the status reads the host's one proxy enrolled and ready.
func (e *hostEnv) awaitReady() ingressdeploy.Status {
	t := e.t
	t.Helper()
	var last ingressdeploy.Status
	dockertest.Wait(t, enrolBound, "the proxy to enrol and apply the current bundle (nodes expected 1, enrolled 1, ready 1)", func() (bool, string) {
		st, err := e.deploy.Status(testsupport.Context(t), e.envID)
		if err != nil {
			return false, err.Error()
		}
		last = st
		return st.Nodes.Expected == 1 && st.Nodes.Enrolled == 1 && st.Nodes.Ready == 1, fmt.Sprintf("%+v", st.Nodes)
	})
	return last
}

// proxyContainer is the part of the proxy container's inspect the tests assert on.
type proxyContainer struct {
	Config struct {
		User string
	}
	HostConfig struct {
		RestartPolicy struct{ Name string }
		PortBindings  map[string][]struct{ HostPort string }
	}
	NetworkSettings struct {
		Networks map[string]json.RawMessage
	}
}

// proxy inspects the proxy container of the host.
func (e *hostEnv) proxy() proxyContainer {
	e.t.Helper()
	out := e.sh("docker inspect --format '{{json .}}' " + ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1")
	var c proxyContainer
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		e.t.Fatalf("read the proxy container's inspect: %v", err)
	}
	return c
}

// hostEnvReader reads the facts of the environment the deployer decides on, as the server's reader does: an agent host is
// its one agent, the local socket one node whose ID is the environment's.
type hostEnvReader struct {
	st  *store.Store
	svc *envs.Service
}

func (r hostEnvReader) Info(ctx context.Context, envID string) (ingressdeploy.EnvInfo, error) {
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
	info := ingressdeploy.EnvInfo{ID: envID, Name: row.Name, ClusterID: row.SwarmClusterID.String}
	if in.Kind != envs.KindAgent {
		info.Local = true
		info.EngineID = in.Local.Probe.EngineID
		info.Nodes = []ingressdeploy.NodeInfo{{AgentID: envID, Name: in.Local.Probe.Hostname, Connected: in.Local.State == envs.StateOnline}}
		return info, nil
	}
	info.Swarm = in.EnvType == "swarm"
	info.EngineID = row.EngineID.String
	for _, ag := range in.Agents {
		if ag.Revoked {
			continue
		}
		info.Nodes = append(info.Nodes, ingressdeploy.NodeInfo{AgentID: ag.ID, Name: ag.Node, Role: ag.Role, Connected: ag.Connected})
		break // a standalone host has the one agent
	}
	return info, nil
}

// hostStackOps adapts the ingress store to stackops.Ingress, as the API's adaptor does, minus its audit rows. Unlike the
// swarm harness's, it attaches for either target: a standalone stack joins the shared bridge too.
type hostStackOps struct{ st *ingressstore.Store }

func (o *hostStackOps) AttachFor(ctx context.Context, envID string, _ stacks.Target) (*stacks.IngressAttach, error) {
	set, err := o.st.Settings(ctx, envID)
	if err != nil {
		return nil, err
	}
	if !set.Enabled {
		return nil, nil
	}
	return &stacks.IngressAttach{Network: ingress.NetworkName}, nil
}

func (o *hostStackOps) World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error) {
	return o.st.World(ctx, envID, canView)
}

func (o *hostStackOps) ReplaceStackRoutes(ctx context.Context, in ingressstore.ReplaceInput, _ audit.Actor) (ingress.Plan, error) {
	return o.st.ReplaceStackRoutes(ctx, in)
}

func (o *hostStackOps) ReleaseStack(ctx context.Context, envID, stackID string, actor audit.Actor) error {
	return o.st.ReleaseStack(ctx, envID, stackID, actor.ID, nil)
}

func (o *hostStackOps) Notify(envID string) { o.st.Notify(envID) }

func (o *hostStackOps) HeldRoutes(ctx context.Context, envID, stackID string) ([]ingress.Route, error) {
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

func (o *hostStackOps) CertStates(context.Context, string, []string) (map[string]stackops.CertState, error) {
	return map[string]stackops.CertState{}, nil
}

var _ stackops.Ingress = (*hostStackOps)(nil)

// rootCaller is an instance admin, who may deploy anything.
func (e *hostEnv) rootCaller() stackops.Caller {
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

// startRegistry runs a registry on the host and pushes the backend image into it, so a stack names it by digest.
func (e *hostEnv) startRegistry() {
	t := e.t
	t.Helper()
	e.registry = dockertest.StartRegistry(t, e.host, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, e.host, dockertest.ServiceTestImage, e.registry+"/ingress/backend:1", nil)
	e.webImage = e.registry + "/ingress/backend@" + digest
}

// backendCommand is the shell line a backend runs: it answers body on port 80, so a reply names the container that gave it.
func backendCommand(body string) string {
	return `["sh", "-c", "mkdir -p /www && echo ` + body + ` > /www/index.html && exec httpd -f -p 80 -h /www"]`
}

// webBody is what the web service of a stack answers, and apiBody what its api service does.
func webBody(stack string) string { return stack + "-web" }
func apiBody(stack string) string { return stack + "-api" }

// routedService is a service of a standalone stack file that answers body on port 80 and asks for host.
func (e *hostEnv) routedService(name, body, host string) string {
	return "  " + name + ":\n    image: " + e.webImage + "\n    command: " + backendCommand(body) + "\n" +
		"    labels:\n      tierplan.ingress.host: " + host + "\n      tierplan.ingress.port: \"80\"\n"
}

// deployStack stores a standalone stack of the environment and deploys compose through stackops.DeployStandalone with a claim
// write, as the API's deploy does, ingress attachment included. It returns the result and the job, and marks the revision good
// when the deploy succeeded.
func (e *hostEnv) deployStack(name, compose string) (*stack, stackops.DeployResult, jobs.Job) {
	t := e.t
	t.Helper()
	ctx := testsupport.Context(t)
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}}
	st, _, err := e.repo.CreateStack(ctx, e.envID, name, stacks.TargetStandalone, "user:test", copy)
	if err != nil {
		t.Fatalf("CreateStack %s: %v", name, err)
	}
	rev, err := e.repo.NewRevision(ctx, st.ID, "user:test", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	attach, err := e.ops.AttachFor(ctx, e.envID, stacks.TargetStandalone)
	if err != nil {
		t.Fatalf("AttachFor: %v", err)
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: name, Target: stacks.TargetStandalone, Copy: copy, Ingress: attach})
	if err != nil {
		t.Fatalf("the stack is not deployable: %v", err)
	}
	in := stackops.StandaloneDeployInput{
		EnvID: e.envID, NodeID: e.nodeID, Stack: st, Copy: copy, Pins: e.repo, Subnets: e.repo, RevisionID: rev.ID, Caller: e.rootCaller(),
		Ingress: &stackops.ClaimWrite{Decls: stacks.IngressDeclarations(loaded.Project, stacks.TargetStandalone), Actor: audit.Actor{Type: audit.ActorUser, ID: "user:test", Name: "Test User"}},
	}
	var res stackops.DeployResult
	done := make(chan struct{})
	job, err := e.jobs.Start(jobs.Spec{Kind: "stack_deploy", EnvID: e.envID, Owner: "user:test", Items: stackops.StandaloneItemSpecs()},
		func(jctx context.Context, rep *jobs.Reporter) {
			defer close(done)
			res = e.deps.DeployStandalone(jctx, in, rep)
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

// uploadCertificate stores a leaf for host from the test CA as the live certificate of the hostname, as the upload route does:
// the proxy serves what the reconciler renders from it.
func (e *hostEnv) uploadCertificate(host string) {
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

// httpsGet asks the host's port 443 for the page of host, with the test CA as the only root, and returns the status and the body.
func (e *hostEnv) httpsGet(host string) (int, string, error) {
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{ServerName: host, RootCAs: e.ca.Pool(), MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: testsupport.Deadline}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+net.JoinHostPort(e.host.IP, "443")+"/", nil)
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

// awaitServed waits until the host's port 443 answers host over a certificate the test CA issued, and returns the page.
func (e *hostEnv) awaitServed(host string) string {
	t := e.t
	t.Helper()
	var page string
	dockertest.Wait(t, serveBound, "https://"+host+" on "+e.host.Hostname+":443 to answer", func() (bool, string) {
		status, body, err := e.httpsGet(host)
		if err != nil {
			return false, err.Error()
		}
		page = body
		return status == http.StatusOK, fmt.Sprintf("status %d: %.80s", status, strings.TrimSpace(body))
	})
	return page
}

// aliasesOn are the aliases the container's endpoint on the network lists, without the container's own short ID, which the
// engine adds to every endpoint.
func (e *hostEnv) aliasesOn(container, network string) []string {
	e.t.Helper()
	out := strings.TrimSpace(e.sh("docker inspect --format '{{json (index .NetworkSettings.Networks \"" + network + "\").Aliases}}' " + container))
	var all []string
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		e.t.Fatalf("read the aliases of %s on %s from %q: %v", container, network, out, err)
	}
	id := strings.TrimSpace(e.sh("docker inspect --format '{{.Id}}' " + container))
	var aliases []string
	for _, a := range all {
		if !strings.HasPrefix(id, a) {
			aliases = append(aliases, a)
		}
	}
	return aliases
}

// networksOf are the names of the networks the container is on, sorted.
func (e *hostEnv) networksOf(container string) []string {
	e.t.Helper()
	out := strings.TrimSpace(e.sh("docker inspect --format '{{json .NetworkSettings.Networks}}' " + container))
	var nets map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &nets); err != nil {
		e.t.Fatalf("read the networks of %s: %v", container, err)
	}
	names := keysOf(nets)
	slices.Sort(names)
	return names
}

// claimsOf are the hostnames the stack holds, sorted.
func (e *hostEnv) claimsOf(st *stack) []string {
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
