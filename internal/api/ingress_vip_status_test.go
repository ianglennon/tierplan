// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The virtual IP status and move history routes (Phase 26, VIP-03, D-22, D-23). The deployer is the real one over a fake swarm
// whose one connected node's keepalived log is played through the real reader, so nothing is written into its memory by hand.

var (
	// The swarm node IDs of the two nodes: 25 lower-case alphanumerics, as the helper writes them.
	vipNodeA = strings.Repeat("a", 25)
	vipNodeB = strings.Repeat("b", 25)

	// vipNow is the instant the fixture's clock stands at.
	vipNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
)

const (
	vipBundle = "bundle-1"
	// vipKeySentinel is the sealed key's stored text; no answer may carry any of it (PROH-26-03).
	vipKeySentinel = "SEALED-KEY-SENTINEL-d41d8cd98f"
)

// vipClock stands at vipNow. After never fires but says a round of the reader has ended, so a test waits on that and not on
// time.
type vipClock struct{ rounds chan struct{} }

func (c *vipClock) Now() time.Time { return vipNow }

func (c *vipClock) After(time.Duration) <-chan time.Time {
	select {
	case c.rounds <- struct{}{}:
	default:
	}
	return nil
}

// vipManagers answers the manager table a test sets.
type vipManagers struct {
	rows []envs.ManagerPolicyRow
}

func (m *vipManagers) ManagerPolicies(context.Context, string) ([]envs.ManagerPolicyRow, error) {
	return m.rows, nil
}

// vipProxyNodes answers the proxy node rows a test sets.
type vipProxyNodes struct {
	mu   sync.Mutex
	rows []ingresshub.Node
}

func (n *vipProxyNodes) ListNodes(context.Context, string) ([]ingresshub.Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]ingresshub.Node(nil), n.rows...), nil
}

// vipEnvs is a swarm of two nodes, of which only node A is connected, so only its keepalived log is read.
type vipEnvs struct{}

func (vipEnvs) Info(_ context.Context, envID string) (ingressdeploy.EnvInfo, error) {
	return ingressdeploy.EnvInfo{
		ID: envID, Name: "prod", Swarm: true, ClusterID: "cluster-1", ManagerConnected: true,
		Nodes: []ingressdeploy.NodeInfo{
			{SwarmNodeID: vipNodeA, AgentID: "agent-a", Name: "manager-1", Role: "manager", Connected: true},
			{SwarmNodeID: vipNodeB, AgentID: "agent-b", Name: "manager-2", Role: "manager", Connected: false},
		},
	}, nil
}

// vipFix is the deployer fixture with a virtual IP row and a way to play the reader.
type vipFix struct {
	*deployFix
	clock    *vipClock
	managers *vipManagers
	proxies  *vipProxyNodes
}

// newVIPFix builds the fixture over a swarm whose keepalived service has a task on each node in the state given and whose
// node A has a running keepalived container, "ka-a". A virtual IP row is stored unless noRow.
func newVIPFix(t *testing.T, noRow bool, stateA, stateB string) *vipFix {
	t.Helper()
	v := &vipFix{clock: &vipClock{rounds: make(chan struct{}, 1)}, managers: &vipManagers{}, proxies: &vipProxyNodes{}}
	task := func(id, node, state string) swarm.Task {
		tk := swarm.Task{ID: id, NodeID: node, ServiceID: "ka-svc", DesiredState: swarm.TaskStateRunning}
		tk.CreatedAt = vipNow.Add(-time.Hour)
		tk.Status = swarm.TaskStatus{State: swarm.TaskState(state), Timestamp: vipNow.Add(-time.Hour)}
		return tk
	}
	v.deployFix = newDeployFixDeps(t, false, func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{{ID: "ka-svc", Name: ingress.ServiceNameKeepalived, Mode: "global"}}
		c.Tasks = []swarm.Task{task("task-a", vipNodeA, stateA), task("task-b", vipNodeB, stateB)}
		c.Containers = []dockertest.FakeContainer{{ID: "ka-a", Names: []string{"/ka-a"}, State: "running", Labels: ingress.KeepalivedLabels()}}
	}, func(_ *deployFix, d *ingressdeploy.Deps) {
		d.Envs = vipEnvs{}
		d.Clock = v.clock
		d.Managers = v.managers
		d.Nodes = v.proxies
		d.CurrentBundle = func(string) (string, bool) { return vipBundle, true }
	})
	if !noRow {
		err := v.ingressStore.PutVIP(testsupport.Context(t), ingressstore.VIPConfig{
			EnvID: v.envA, Address: netip.MustParseAddr("192.168.10.250"), PrefixLen: 24, RouterID: 51,
			RouterDerived: true, KeySealed: vipKeySentinel, KeyGeneration: 2, Enabled: true,
		})
		if err != nil {
			t.Fatalf("PutVIP: %v", err)
		}
	}
	return v
}

// play writes the events to node A's keepalived log and has the real reader take its first round over the environment.
func (v *vipFix) play(t *testing.T, events ...ingressbundle.VIPEvent) {
	t.Helper()
	lines := make([]dockertest.FakeLogLine, 0, len(events))
	for i, ev := range events {
		b, err := ev.MarshalLine()
		if err != nil {
			t.Fatalf("MarshalLine: %v", err)
		}
		lines = append(lines, dockertest.FakeLogLine{Time: vipNow.Add(time.Duration(i-len(events)) * time.Second), Text: string(b)})
	}
	v.fake.AppendLogs("ka-a", lines...)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.ingressDeploy.RunVIPReader(ctx)
	}()
	<-v.clock.rounds
	cancel()
	<-done
}

// vipEvent is an event of node A's helper.
func vipEvent(seq uint64, event string) ingressbundle.VIPEvent {
	return ingressbundle.VIPEvent{Event: event, Node: vipNodeA, Name: "manager-1", Seq: seq}
}

// vipState is a VRRP state event of node A.
func vipState(seq uint64, state string) ingressbundle.VIPEvent {
	e := vipEvent(seq, ingressbundle.VIPEventState)
	e.State = state
	return e
}

// vipCandidate is the shape of a candidate in the answer.
type vipCandidate struct {
	NodeID     string `json:"nodeId"`
	Hostname   string `json:"hostname"`
	VRRPState  string `json:"vrrpState"`
	ProxyReady bool   `json:"proxyReady"`
	TaskState  string `json:"taskState"`
	LastFault  string `json:"lastFault"`
	LastHeard  string `json:"lastHeard"`
}

// vipStatusResp is the shape of the status answer.
type vipStatusResp struct {
	Address       string `json:"address"`
	RouterID      int    `json:"routerId"`
	RouterDerived bool   `json:"routerDerived"`
	KeyGeneration int    `json:"keyGeneration"`
	Enabled       bool   `json:"enabled"`
	Holder        struct {
		State string   `json:"state"`
		Nodes []string `json:"nodes"`
	} `json:"holder"`
	HolderlessSince string         `json:"holderlessSince"`
	Candidates      []vipCandidate `json:"candidates"`
	Managers        []struct {
		NodeID, Hostname, State string
		LocalSocket             bool
	} `json:"managers"`
	ManagersReadable bool `json:"managersReadable"`
	ForeignRouter    struct {
		Detected bool   `json:"detected"`
		Sender   string `json:"sender"`
	} `json:"foreignRouter"`
	ClockSkewNodes    []string `json:"clockSkewNodes"`
	AuthWindowSeconds int      `json:"authWindowSeconds"`
	LastEnsure        struct {
		At, Error string
	} `json:"lastEnsure"`
}

// TestGetIngressVIP is the tracer of the status route (D-22): an Admin reads the address, who holds it, how each candidate
// stands, the managers, the foreign-router and clock warnings and the last ensure, with no key anywhere in the answer, and an
// Operator is refused.
func TestGetIngressVIP(t *testing.T) {
	t.Parallel()
	v := newVIPFix(t, false, "running", "starting")
	v.managers.rows = []envs.ManagerPolicyRow{
		{NodeID: vipNodeA, Hostname: "manager-1", State: envs.ManagerAllows},
		{NodeID: vipNodeB, Hostname: "manager-2", State: envs.ManagerRefuses},
	}
	v.proxies.rows = []ingresshub.Node{{SwarmNodeID: vipNodeA, Status: "active", Health: "ok", AppliedBundleID: vipBundle}}
	ensureAt := vipNow.Add(-time.Minute)
	if err := v.ingressStore.RecordVIPEnsure(testsupport.Context(t), v.envA, ensureAt, "unreachable"); err != nil {
		t.Fatalf("RecordVIPEnsure: %v", err)
	}
	foreign := vipEvent(2, ingressbundle.VIPEventForeignRouter)
	foreign.Active, foreign.Counter, foreign.Addr = true, ingressbundle.VIPCounterInvalidHMAC, "192.168.10.77"
	skew := vipEvent(3, ingressbundle.VIPEventClockSkew)
	skew.Active = true
	v.play(t, vipState(1, ingressbundle.VIPStateMaster), foreign, skew)

	path := "/api/env/" + v.envA + "/ingress/vip"
	rec := v.credCall(t, v.adminA, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if strings.Contains(rec.Body.String(), vipKeySentinel) || strings.Contains(strings.ToLower(rec.Body.String()), "sealed") {
		t.Errorf("the answer carries the key: %s", rec.Body)
	}
	st := decodeBody[vipStatusResp](t, rec)
	if st.Address != "192.168.10.250/24" || st.RouterID != 51 || !st.RouterDerived || st.KeyGeneration != 2 || !st.Enabled {
		t.Errorf("configuration = %+v", st)
	}
	if st.Holder.State != "one" || len(st.Holder.Nodes) != 1 || st.Holder.Nodes[0] != vipNodeA || st.HolderlessSince != "" {
		t.Errorf("holder = %+v, holderlessSince = %q, want node A alone and no holderlessSince", st.Holder, st.HolderlessSince)
	}
	if len(st.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want two", st.Candidates)
	}
	a, b := st.Candidates[0], st.Candidates[1]
	if a.NodeID != vipNodeA || a.Hostname != "manager-1" || a.VRRPState != "MASTER" || !a.ProxyReady || a.TaskState != "running" || a.LastFault != "" || a.LastHeard != vipNow.Format(time.RFC3339) {
		t.Errorf("candidate A = %+v", a)
	}
	if b.NodeID != vipNodeB || b.Hostname != "manager-2" || b.VRRPState != "" || b.ProxyReady || b.TaskState != "starting" || b.LastHeard != "" {
		t.Errorf("candidate B = %+v, want no state and never heard", b)
	}
	if len(st.Managers) != 2 || !st.ManagersReadable || st.Managers[0].State != "allows" || st.Managers[1].State != "refuses" {
		t.Errorf("managers = %+v readable %v", st.Managers, st.ManagersReadable)
	}
	if !st.ForeignRouter.Detected || st.ForeignRouter.Sender != "192.168.10.77" {
		t.Errorf("foreignRouter = %+v", st.ForeignRouter)
	}
	if len(st.ClockSkewNodes) != 1 || st.ClockSkewNodes[0] != vipNodeA || st.AuthWindowSeconds != 30 {
		t.Errorf("clock skew nodes = %v, auth window = %d, want node A and 30", st.ClockSkewNodes, st.AuthWindowSeconds)
	}
	if st.LastEnsure.At != ensureAt.Format(time.RFC3339) || st.LastEnsure.Error != "unreachable" {
		t.Errorf("lastEnsure = %+v", st.LastEnsure)
	}

	if rec := v.credCall(t, v.operatorA, http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
		t.Errorf("an Operator reading the status: %d, want 403", rec.Code)
	}
}
