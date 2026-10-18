// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// The swarm suites (D-57, D-58) run against the engine BAYPLAN_TEST_ENGINE names,
// one real dind swarm per test function. Every command an admin would paste is run
// exactly as the server generated it; the harness only loads the agent image on
// the nodes, so no registry is contacted.

const (
	installWait = 90 * time.Second
	agentName   = "bayplan-agent"
)

// swarmFix is one swarm, one in-process Bayplan reachable from it, and one agent
// environment that nothing has been installed for yet.
type swarmFix struct {
	sw  *dockertest.Swarm
	srv *servertest.Server
	env servertest.Created
}

func startSwarmFix(t *testing.T, managers, workers int) *swarmFix {
	t.Helper()
	sw := dockertest.StartSwarm(t, "", managers, workers)
	dockertest.LoadAgentImage(t, sw.Nodes()...)
	dumpOnFailure(t, sw.Nodes()...)
	srv := servertest.Start(t, servertest.Options{Host: sw.Gateway})
	return &swarmFix{sw: sw, srv: srv, env: srv.CreateAgentEnvironment("")}
}

// install runs the generated swarm command on the first manager.
func (f *swarmFix) install(t *testing.T) {
	t.Helper()
	t.Logf("swarm install on %s:\n%s", f.sw.Managers[0].Hostname, quiet(sh(t, f.sw.Managers[0], f.env.Install.Swarm.Command)))
}

// installPrivileged runs the generated swarm command with the one setting a host owner adds to let the manager's
// agent forward services that give tasks host access (BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES, D-36). The
// setting is put in the text the server generated, before the first update flag of the service, so everything
// else in the command runs exactly as an admin would paste it.
func (f *swarmFix) installPrivileged(t *testing.T) {
	t.Helper()
	const anchor = "--update-parallelism 1"
	cmd := f.env.Install.Swarm.Command
	if !strings.Contains(cmd, anchor) {
		t.Fatalf("the generated swarm command has no %q to put the setting before:\n%s", anchor, cmd)
	}
	cmd = strings.Replace(cmd, anchor, "--env BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true "+anchor, 1)
	t.Logf("swarm install with the privileged-services opt-in on %s:\n%s", f.sw.Managers[0].Hostname, quiet(sh(t, f.sw.Managers[0], cmd)))
}

// svcItem is the part of a row of the services list these tests read.
type svcItem struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Mode            string `json:"mode"`
	Running         uint64 `json:"running"`
	Desired         uint64 `json:"desired"`
	UpdateState     string `json:"updateState"`
	Image           string `json:"image"`
	HasPreviousSpec bool   `json:"hasPreviousSpec"`
	RestartLoop     *struct {
		State string `json:"state"`
		Count int    `json:"count"`
	} `json:"restartLoop"`
}

// services is the services list of the environment as the API answers it now.
func (f *swarmFix) services(t *testing.T) []svcItem {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/services", nil)
	var list struct {
		Items []svcItem `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET services = %d %s", code, body)
	}
	return list.Items
}

// service is the row of one service of the list, by name.
func (f *swarmFix) service(t *testing.T, name string) (svcItem, bool) {
	t.Helper()
	for _, it := range f.services(t) {
		if it.Name == name {
			return it, true
		}
	}
	return svcItem{}, false
}

// waitService polls the services list, never sleeping a fixed time, until cond is true of the service's row,
// and fails with the row (or its absence) as last seen. The cluster is read again each time round, so the
// answer is the swarm's now and not the server's last periodic read.
func (f *swarmFix) waitService(t *testing.T, name, what string, d time.Duration, cond func(svcItem) bool) svcItem {
	t.Helper()
	var got svcItem
	f.srv.WaitFor("service "+name+" "+what, d, func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		it, ok := f.service(t, name)
		got = it
		if !ok {
			return false, "no such service in the list"
		}
		b, _ := json.Marshal(it)
		return cond(it), string(b)
	})
	return got
}

// rolloutBody is the answer of the rollout route.
type rolloutBody struct {
	State           string `json:"state"`
	Message         string `json:"message"`
	Updated         int    `json:"updated"`
	Total           int    `json:"total"`
	LatestTaskError string `json:"latestTaskError"`
}

// serviceRollout reads how far the service's last rolling update or rollback has got.
func (f *swarmFix) serviceRollout(t *testing.T, id string) rolloutBody {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/services/"+id+"/rollout", nil)
	var out rolloutBody
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("GET rollout of %s = %d %s", id, code, body)
	}
	return out
}

// waitRollout polls the rollout route until the update is in state.
func (f *swarmFix) waitRollout(t *testing.T, id, state string, d time.Duration) rolloutBody {
	t.Helper()
	var got rolloutBody
	f.srv.WaitFor("the rollout of "+id+" to be "+state, d, func() (bool, string) {
		got = f.serviceRollout(t, id)
		b, _ := json.Marshal(got)
		return got.State == state, string(b)
	})
	return got
}

// createdService is the answer of the create route.
type createdService struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
	Pinned bool   `json:"pinned"`
}

// createService posts the spec to the create route and answers the status and the body.
func (f *swarmFix) createService(t *testing.T, spec any) (int, []byte) {
	t.Helper()
	return f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/services", map[string]any{"spec": spec})
}

// problemType is the machine-readable type of an error body.
func problemType(body []byte) string {
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Type
}

// quiet drops the progress lines `docker service create` prints while it waits
// for the tasks to converge, which repeat a hundred times in a log.
func quiet(out string) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(l, "overall progress:") && !strings.HasPrefix(l, "verify: Waiting") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// hostnames is the engine hostname of every node, in the order of Nodes().
func (f *swarmFix) hostnames() []string {
	var out []string
	for _, n := range f.sw.Nodes() {
		out = append(out, n.Hostname)
	}
	return out
}

// waitSwarmOnline waits until the environment is one online swarm with every node
// connected: nodes and agents both at want of want.
func (f *swarmFix) waitSwarmOnline(t *testing.T, want int) servertest.Environment {
	t.Helper()
	var got servertest.Environment
	f.srv.WaitFor("the swarm environment to be online with every agent", installWait, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		got = e
		ready := ok && e.Status.State == "online" && e.Engine.Version != "" &&
			e.Nodes != nil && e.Nodes.Ready == want && e.Nodes.Total == want &&
			e.Agents != nil && e.Agents.Connected == want && e.Agents.Expected == want
		return ready, summarise(e)
	})
	return got
}

// serviceSpec is the part of `docker service inspect` the install flags decide.
type serviceSpec struct {
	Name   string
	Labels map[string]string
	Mode   struct {
		Global     *struct{}
		Replicated *struct{}
	}
	UpdateConfig struct {
		Parallelism int
		Delay       time.Duration
	}
	TaskTemplate struct {
		Placement     struct{ Constraints []string }
		ContainerSpec struct {
			Image          string
			Labels         map[string]string
			Env            []string
			ReadOnly       bool
			CapabilityAdd  []string
			CapabilityDrop []string
			Mounts         []struct{ Type, Source, Target string }
			Secrets        []struct {
				File struct {
					Name string
					Mode uint32
				}
				SecretName string
			}
		}
	}
	EndpointSpec *struct{ Ports []any }
}

func inspectService(t *testing.T, n *dockertest.Node, name string) serviceSpec {
	t.Helper()
	var spec serviceSpec
	out := sh(t, n, "docker service inspect --format '{{json .Spec}}' "+name)
	if err := json.Unmarshal([]byte(out), &spec); err != nil {
		t.Fatalf("parse the spec of service %s: %v\n%s", name, err, out)
	}
	return spec
}

// sharedEnrol runs the generated swarm command on one manager of a swarm of
// three managers and a worker: the global service starts one agent per node, all
// four enrol, and the environment is one swarm with its cluster ID bound, four
// of four nodes and agents, and container rows that name the node they run on
// (D-57, D-58, ENV-02, ENV-03). The same run is the first real-swarm use of the
// install flags (--constraint, --update-delay, --container-label, the secret
// mount) and of the hardening options on a service.
func sharedEnrol(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	f.install(t)
	got := f.waitSwarmOnline(t, 4)

	// One swarm environment, bound to the cluster a manager reports.
	if got.Kind != "agent" || got.EnvType != "swarm" {
		t.Errorf("environment = kind %q envType %q, want agent and swarm", got.Kind, got.EnvType)
	}
	clusterID := strings.TrimSpace(sh(t, mgr, "docker info -f '{{.Swarm.Cluster.ID}}'"))
	if clusterID == "" {
		t.Fatal("the manager reports no cluster ID")
	}
	if row := f.srv.Row(f.env.ID); !row.SwarmClusterID.Valid || row.SwarmClusterID.String != clusterID {
		t.Errorf("bound cluster ID = %+v, the manager's docker info says %q", row.SwarmClusterID, clusterID)
	}
	// D-23: with no name given, a manager's hostname and the word swarm.
	wantNames := make([]string, 0, len(f.sw.Managers))
	for _, m := range f.sw.Managers {
		wantNames = append(wantNames, m.Hostname+" swarm")
	}
	if !slices.Contains(wantNames, got.Name) {
		t.Errorf("environment name = %q, want one of %v", got.Name, wantNames)
	}
	if got.Engine.Version != strings.TrimSpace(sh(t, mgr, "docker version --format '{{.Server.Version}}'")) {
		t.Errorf("engine version = %q, not what the manager's docker reports", got.Engine.Version)
	}

	// Every agent row has the right role and node name and a real certificate.
	agents := f.srv.Agents(f.env.ID)
	if len(agents) != 4 {
		t.Fatalf("agents = %d, want 4: %+v", len(agents), agents)
	}
	for _, n := range f.sw.Nodes() {
		wantRole := "worker"
		if n.Manager {
			wantRole = "manager"
		}
		var a *servertest.Agent
		for i := range agents {
			if agents[i].Node == n.Hostname {
				a = &agents[i]
			}
		}
		if a == nil {
			t.Errorf("no agent row for node %s: %+v", n.Hostname, agents)
			continue
		}
		if a.Role != wantRole || !a.Connected || a.Status != "active" {
			t.Errorf("agent of %s = role %q connected %v status %q, want %s, connected, active", n.Hostname, a.Role, a.Connected, a.Status, wantRole)
		}
		if a.CertFingerprint == "" || a.AgentVersion == "" || a.EngineVersion != got.Engine.Version {
			t.Errorf("agent of %s = fingerprint %q agentVersion %q engineVersion %q", n.Hostname, a.CertFingerprint, a.AgentVersion, a.EngineVersion)
		}
		if notAfter, err := time.Parse(time.RFC3339, a.CertNotAfter); err != nil || time.Until(notAfter) < 24*time.Hour {
			t.Errorf("agent of %s: cert not after %q (%v), want a real expiry well ahead", n.Hostname, a.CertNotAfter, err)
		}
		if a.ClockOffsetSeconds < -5 || a.ClockOffsetSeconds > 5 {
			t.Errorf("agent of %s: clock offset %ds on a shared clock", n.Hostname, a.ClockOffsetSeconds)
		}
	}

	// The install flags are what Docker made of them.
	spec := inspectService(t, mgr, agentName)
	ct := spec.TaskTemplate.ContainerSpec
	if spec.Mode.Global == nil || spec.Mode.Replicated != nil {
		t.Errorf("service mode = %+v, want global", spec.Mode)
	}
	if !slices.Contains(spec.TaskTemplate.Placement.Constraints, "node.platform.os==linux") {
		t.Errorf("constraints = %v, want node.platform.os==linux", spec.TaskTemplate.Placement.Constraints)
	}
	if spec.UpdateConfig.Parallelism != 1 || spec.UpdateConfig.Delay != 10*time.Second {
		t.Errorf("update config = %+v, want parallelism 1 and delay 10s", spec.UpdateConfig)
	}
	if spec.Labels["bayplan.agent"] != "true" || ct.Labels["bayplan.agent"] != "true" {
		t.Errorf("labels = service %v container %v, want bayplan.agent=true on both (--label and --container-label)", spec.Labels, ct.Labels)
	}
	if !ct.ReadOnly {
		t.Error("the service's root filesystem is not read-only")
	}
	if !slices.Equal(sorted(ct.CapabilityDrop), []string{"ALL"}) || !slices.Equal(sorted(stripCap(ct.CapabilityAdd)), []string{"SETGID", "SETUID"}) {
		t.Errorf("capabilities = drop %v add %v, want drop ALL and add SETUID, SETGID", ct.CapabilityDrop, ct.CapabilityAdd)
	}
	if len(ct.Secrets) != 1 || ct.Secrets[0].SecretName != f.env.Install.SecretName || ct.Secrets[0].File.Name != "bayplan-agent-token" || ct.Secrets[0].File.Mode != 0o400 {
		t.Errorf("secrets = %+v, want %s mounted as bayplan-agent-token, mode 0400", ct.Secrets, f.env.Install.SecretName)
	}
	if spec.EndpointSpec != nil && len(spec.EndpointSpec.Ports) != 0 {
		t.Errorf("the service publishes ports %v", spec.EndpointSpec.Ports)
	}

	// On every node: one agent container, no published port, UID 1000 with no
	// capability left, and a state volume holding the agent's identity.
	for _, n := range f.sw.Nodes() {
		if ports := strings.TrimSpace(sh(t, n, "docker ps --filter label=bayplan.agent=true --format '{{.Ports}}'")); ports != "" {
			t.Errorf("%s: the agent container publishes %q, want no port (ENV-03)", n.Hostname, ports)
		}
		if names := strings.Fields(sh(t, n, "docker ps --filter label=bayplan.agent=true --format '{{.Names}}'")); len(names) != 1 {
			t.Errorf("%s: agent containers = %v, want one", n.Hostname, names)
		}
		pid := strings.TrimSpace(sh(t, n, "docker inspect -f '{{.State.Pid}}' $(docker ps -q --filter label=bayplan.agent=true)"))
		status := sh(t, n, "cat /proc/"+pid+"/status")
		if uid := fieldOf(status, "Uid:"); uid != "1000 1000 1000 1000" {
			t.Errorf("%s: agent Uid = %q, want 1000", n.Hostname, uid)
		}
		if caps := fieldOf(status, "CapEff:"); strings.Trim(caps, "0") != "" {
			t.Errorf("%s: agent effective capabilities = %s, want none after the privilege drop", n.Hostname, caps)
		}
		if files := sh(t, n, "ls -ln $(docker volume inspect -f '{{.Mountpoint}}' bayplan-agent)"); !strings.Contains(files, " 1000 ") {
			t.Errorf("%s: the state volume holds nothing owned by 1000:\n%s", n.Hostname, files)
		}
	}

	// Container rows carry the node they run on: the four agents, one per node.
	f.srv.WaitFor("the container list to show an agent on every node", 30*time.Second, func() (bool, string) {
		list, code, body := f.srv.Containers(f.env.ID, "")
		if code != http.StatusOK {
			return false, string(body)
		}
		var nodes []string
		for _, c := range list.Items {
			if c.Image == dockertest.ReleaseAgentImage || strings.HasPrefix(c.Name, agentName+".") {
				nodes = append(nodes, c.Node)
			}
		}
		return slices.Equal(sorted(nodes), sorted(f.hostnames())) && len(list.NotReporting) == 0, string(body)
	})

	// The summary adds the four agents up, and knows the node count.
	sum, code, body := f.srv.Summary(f.env.ID)
	if code != http.StatusOK || sum.Counts.Nodes == nil || *sum.Counts.Nodes != 4 || sum.Counts.Containers != 4 || sum.IsWorker {
		t.Errorf("summary = %d %s, want 4 nodes, 4 containers and a manager answering", code, body)
	}
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// stripCap removes the CAP_ prefix newer engines add to a capability name.
func stripCap(in []string) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, strings.TrimPrefix(c, "CAP_"))
	}
	return out
}

// sharedRoutesNodeLocalCalls starts a container on the worker only. Reading
// the environment goes through a manager's agent for cluster questions and
// through every node's own agent for what runs on it: the container shows once,
// under the worker's hostname, whichever way it is asked for (D-11, D-46).
func sharedRoutesNodeLocalCalls(t *testing.T, f *swarmFix) {
	worker, manager := f.sw.Workers[0], f.sw.Managers[0]
	dockertest.LoadImage(t, "busybox", worker)
	f.install(t)
	f.waitSwarmOnline(t, 4)

	sh(t, worker, "docker run -d --name probe-w1 busybox sleep 600")

	find := func(q string) (ok bool, seen string) {
		list, code, body := f.srv.Containers(f.env.ID, q)
		if code != http.StatusOK {
			return false, string(body)
		}
		var on []string
		for _, c := range list.Items {
			if c.Name == "probe-w1" {
				on = append(on, c.Node)
			}
		}
		return slices.Equal(on, []string{worker.Hostname}), string(body)
	}
	f.srv.WaitFor("probe-w1 on the worker, and only there", 30*time.Second, func() (bool, string) { return find("") })

	// The node filter: the worker's rows, not a manager's.
	list, code, body := f.srv.Containers(f.env.ID, "node="+worker.Hostname)
	if code != http.StatusOK || len(list.Items) != 2 { // probe-w1 and the worker's agent
		t.Errorf("node=%s lists %d %s, want the probe and the agent", worker.Hostname, code, body)
	}
	if list, _, body := f.srv.Containers(f.env.ID, "node="+manager.Hostname); len(list.Items) != 1 || list.Items[0].Name == "probe-w1" {
		t.Errorf("node=%s lists %s, want the manager's agent alone", manager.Hostname, body)
	}
	// An unknown node is an empty list, not an error (02-21).
	if list, code, body := f.srv.Containers(f.env.ID, "node=nowhere"); code != http.StatusOK || len(list.Items) != 0 {
		t.Errorf("node=nowhere = %d %s, want 200 and an empty list", code, body)
	}

	// Sixteen reads at once, each fanning out over the four tunnels: every one answers
	// with the same rows.
	type result struct {
		code  int
		count int
		found bool
	}
	results := make(chan result, 16)
	for range 16 {
		go func() {
			l, code, _ := f.srv.Containers(f.env.ID, "")
			r := result{code: code, count: len(l.Items)}
			for _, c := range l.Items {
				r.found = r.found || (c.Name == "probe-w1" && c.Node == worker.Hostname)
			}
			results <- r
		}()
	}
	for range 16 {
		if r := <-results; r.code != http.StatusOK || r.count != 5 || !r.found {
			t.Errorf("a parallel read answered %+v, want 200 and 5 rows including probe-w1 on the worker", r)
		}
	}
}

// TestSwarmWorkerOnly leaves only the worker's agent running. A worker cannot answer
// a cluster question, so the environment is degraded with not_a_manager and the node
// count is unknown, while what the worker holds is still read through its own agent
// (D-14, D-44, Pitfall 1).
func TestSwarmWorkerOnly(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	worker := f.sw.Workers[0]
	dockertest.LoadImage(t, "busybox", worker)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	sh(t, worker, "docker run -d --name probe-w1 busybox sleep 600")

	sh(t, f.sw.Managers[0], "docker service update --constraint-add node.role==worker "+agentName)

	var got servertest.Environment
	f.srv.WaitFor("the environment to be degraded with not_a_manager", installWait, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		got = e
		return ok && e.Status.State == "degraded" && e.HasIssue("not_a_manager"), summarise(e)
	})
	t.Logf("worker-only status: %s", summarise(got))

	// Node-local data still reads: the worker's containers, with the probe.
	f.srv.WaitFor("the worker's rows to be listed", 30*time.Second, func() (bool, string) {
		list, code, body := f.srv.Containers(f.env.ID, "")
		found := false
		for _, c := range list.Items {
			found = found || (c.Name == "probe-w1" && c.Node == worker.Hostname)
		}
		return code == http.StatusOK && found, string(body)
	})
	// The node count is unknown, not wrong: no manager answered.
	sum, code, body := f.srv.Summary(f.env.ID)
	if code != http.StatusOK {
		t.Fatalf("GET summary = %d %s", code, body)
	}
	if sum.Counts.Nodes != nil {
		t.Errorf("summary nodes = %d, want null while no manager answers", *sum.Counts.Nodes)
	}
	if sum.Counts.Containers < 2 {
		t.Errorf("summary containers = %d, want the worker's own (probe and agent)", sum.Counts.Containers)
	}
	if got.Nodes != nil && got.Nodes.Total == 4 && got.Nodes.Ready == 4 {
		t.Errorf("the overview still claims %+v nodes with no manager connected", got.Nodes)
	}
}

// TestSwarmChangedCluster re-initialises the swarm under an enrolled agent: after
// `docker swarm leave --force` and `docker swarm init` the install command is run
// again, the agent (whose identity is on its state volume) is refused with
// cluster_changed, and adopting the new cluster brings the environment online (D-14,
// D-44, ENV-02).
func TestSwarmChangedCluster(t *testing.T) {
	f := startSwarmFix(t, 1, 0)
	mgr := f.sw.Managers[0]
	f.install(t)
	f.waitSwarmOnline(t, 1)
	oldCluster := f.srv.Row(f.env.ID).SwarmClusterID.String
	if oldCluster == "" {
		t.Fatal("the environment was not bound to a cluster")
	}

	sh(t, mgr, "docker swarm leave --force")
	sh(t, mgr, "docker swarm init --advertise-addr "+mgr.IP)
	newCluster := strings.TrimSpace(sh(t, mgr, "docker info -f '{{.Swarm.Cluster.ID}}'"))
	if newCluster == oldCluster {
		t.Fatalf("re-initialising kept the cluster ID %s", oldCluster)
	}
	f.install(t) // the same text: the secret and service are new in the new swarm, the volume is the old one

	var got servertest.Environment
	f.srv.WaitFor("the environment to show cluster_changed", installWait, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		got = e
		return ok && e.Status.State == "cluster_changed" && e.PendingClusterID == newCluster, summarise(e)
	})
	if row := f.srv.Row(f.env.ID); row.SwarmClusterID.String != oldCluster {
		t.Errorf("the bound cluster moved to %q before anyone adopted it", row.SwarmClusterID.String)
	}
	// While refused the agent is not serving requests.
	if _, code, _ := f.srv.Summary(f.env.ID); code != http.StatusServiceUnavailable {
		t.Errorf("summary of a cluster_changed environment = %d, want 503", code)
	}
	t.Logf("cluster_changed status: %s", summarise(got))

	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/adopt-cluster", nil)
	if code != http.StatusOK || !strings.Contains(string(body), newCluster) {
		t.Fatalf("POST adopt-cluster = %d %s, want 200 naming %s", code, body, newCluster)
	}
	f.waitSwarmOnline(t, 1)
	if row := f.srv.Row(f.env.ID); row.SwarmClusterID.String != newCluster || row.PendingClusterID.Valid {
		t.Errorf("after adopting, the row is bound to %q with pending %+v, want %s and nothing pending", row.SwarmClusterID.String, row.PendingClusterID, newCluster)
	}
}
