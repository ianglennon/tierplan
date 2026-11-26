// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// The virtual IP through the typed API on the shared swarm, with real agents on every node (Phase 26, D-29, VIP-01 to VIP-05). The
// in-process harness of internal/ingressdeploy reports every manager as allowing, so only here does the agents' own
// privileged-services policy meet the API, the reader and the drain route. The nodes of a dind swarm share one L2 segment, so
// the address really moves between them; a real router and the S3 leg 2 run stay the UAT's. Every bound below is a failure
// deadline: each wait returns the moment its condition holds.
const (
	// vipJobWait is how long an enable or a switch-off job gets to end: the images are on the nodes, so it is the services'
	// start, and a proxy's health wait on four nodes.
	vipJobWait = 6 * time.Minute
	// vipProxiesWait is how long the four proxies get to dial the server, enrol and apply the current bundle once the enable
	// job ended.
	vipProxiesWait = 5 * time.Minute
	// vipHolderWait is how long the first holder may take once the job is done (S3 measured about 5.5 s for the election).
	vipHolderWait = 60 * time.Second
	// vipMoveWait is how long the address may take to move after the holder's proxy stops, and the reader to list the move
	// (the reader passes every two seconds).
	vipMoveWait = 20 * time.Second
	// vipSettleWatch is how long an activated node is watched for taking the address back.
	vipSettleWatch = 20 * time.Second
	// vipSettleWindow is how long a holder must stay the holder to count as settled. Every node has the same priority, so when
	// the holder goes, the nodes that were BACKUP can all claim the address within the same second and the address may pass
	// through one or two of them, stored as moves of reason unknown, before the highest-addressed claimant keeps it (D-04).
	vipSettleWindow = 8 * time.Second
	// vipGoneWait is how long a switch-off's last objects (a network waits for the tasks that used it) may take to go.
	vipGoneWait = 3 * time.Minute

	// vipIngressImage and vipKeepalivedImage are what `make ingress-image` and `make keepalived-image` build.
	vipIngressImage    = "tierplan-ingress:dev"
	vipKeepalivedImage = "tierplan-keepalived:dev"
)

// vipJobBody is the part of an ingress job these tests read.
type vipJobBody struct {
	State string `json:"state"`
	Items []struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		State   string `json:"state"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"items"`
}

// vipPreviewBody is the part of the preview's answer these tests read.
type vipPreviewBody struct {
	Findings []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
	} `json:"findings"`
	Managers []struct {
		Hostname string `json:"hostname"`
		State    string `json:"state"`
	} `json:"managers"`
}

// vipProblemBody is the part of a refusal these tests read: the type, the detail and the findings as the errors list them.
type vipProblemBody struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string   `json:"location"`
		Message  string   `json:"message"`
		Value    []string `json:"value"`
	} `json:"errors"`
}

// vipStatusBody is the part of the virtual IP's status these tests read.
type vipStatusBody struct {
	Address string `json:"address"`
	Enabled bool   `json:"enabled"`
	Holder  struct {
		State string   `json:"state"`
		Nodes []string `json:"nodes"`
	} `json:"holder"`
	Candidates []struct {
		NodeID     string `json:"nodeId"`
		Hostname   string `json:"hostname"`
		VRRPState  string `json:"vrrpState"`
		ProxyReady bool   `json:"proxyReady"`
	} `json:"candidates"`
}

// vipMove is one row of the move history.
type vipMove struct {
	FromNode string `json:"fromNode"`
	ToNode   string `json:"toNode"`
	Reason   string `json:"reason"`
}

func (f *swarmFix) ingressPath(suffix string) string {
	return "/api/env/" + f.env.ID + "/ingress" + suffix
}

// vipJob starts the job at method and path, which has to answer 202, and waits until it ends. It returns the job as it ended.
func (f *swarmFix) vipJob(t *testing.T, method, path string, body any) vipJobBody {
	t.Helper()
	code, raw := f.srv.Do(method, f.ingressPath(path), body)
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(raw, &started) != nil || started.JobID == "" {
		t.Fatalf("%s ingress%s = %d %s, want 202 with a job", method, path, code, raw)
	}
	var job vipJobBody
	f.srv.WaitFor("the ingress job "+started.JobID+" to end", vipJobWait, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodGet, f.ingressPath("/jobs/"+started.JobID), nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, raw)
		}
		job = vipJobBody{}
		if err := json.Unmarshal(raw, &job); err != nil {
			return false, string(raw)
		}
		return job.State != "running", string(raw)
	})
	return job
}

// requireJobDone fails unless the job ended done, with the job as it ended.
func requireJobDone(t *testing.T, what string, job vipJobBody) {
	t.Helper()
	if job.State != "done" {
		out, _ := json.Marshal(job)
		t.Fatalf("the %s job ended %s, want done: %s", what, job.State, out)
	}
}

// vipStatus reads the virtual IP's status; ok is false for any answer but 200.
func (f *swarmFix) vipStatus(t *testing.T) (vipStatusBody, bool, int) {
	t.Helper()
	code, raw := f.srv.Do(http.MethodGet, f.ingressPath("/vip"), nil)
	var st vipStatusBody
	if code != http.StatusOK {
		return st, false, code
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("GET the virtual IP status = %d %s: %v", code, raw, err)
	}
	return st, true, code
}

// vipMoves reads the move history, newest first.
func (f *swarmFix) vipMoves(t *testing.T) []vipMove {
	t.Helper()
	code, raw := f.srv.Do(http.MethodGet, f.ingressPath("/vip/moves?limit=100"), nil)
	var page struct {
		Items []vipMove `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &page) != nil {
		t.Fatalf("GET the virtual IP moves = %d %s", code, raw)
	}
	return page.Items
}

// vipNodeIDs maps the swarm node ID of every node of the swarm to the node, read once.
func vipNodeIDs(t *testing.T, f *swarmFix) map[string]*dockertest.Node {
	t.Helper()
	ids := map[string]*dockertest.Node{}
	for _, n := range f.sw.Nodes() {
		ids[strings.TrimSpace(sh(t, n, "docker info -f '{{.Swarm.NodeID}}'"))] = n
	}
	return ids
}

// vipHolder waits until the status names exactly one holder that accept takes (any when accept is nil) and returns that node
// and the status.
func (f *swarmFix) vipHolder(t *testing.T, ids map[string]*dockertest.Node, what string, d time.Duration, accept func(*dockertest.Node) bool) (vipStatusBody, *dockertest.Node) {
	t.Helper()
	var st vipStatusBody
	var holder *dockertest.Node
	f.srv.WaitFor(what, d, func() (bool, string) {
		var ok bool
		var code int
		if st, ok, code = f.vipStatus(t); !ok {
			return false, fmt.Sprintf("GET the status = %d", code)
		}
		if st.Holder.State != "one" || len(st.Holder.Nodes) != 1 {
			return false, fmt.Sprintf("holder %s %v", st.Holder.State, st.Holder.Nodes)
		}
		if holder = ids[st.Holder.Nodes[0]]; holder == nil {
			return false, "the holder " + st.Holder.Nodes[0] + " is not a node of the swarm"
		}
		if accept != nil && !accept(holder) {
			return false, "held by " + holder.Hostname
		}
		return true, ""
	})
	return st, holder
}

// vipSettled waits for a holder that accept takes and that then stays the only holder for vipSettleWindow, and returns it.
func (f *swarmFix) vipSettled(t *testing.T, ids map[string]*dockertest.Node, what string, accept func(*dockertest.Node) bool) *dockertest.Node {
	t.Helper()
	for range 6 {
		_, h := f.vipHolder(t, ids, what, vipJobWait, accept)
		if vipWatch(vipSettleWindow, func() string {
			st, ok, _ := f.vipStatus(t)
			if !ok || len(st.Holder.Nodes) != 1 || ids[st.Holder.Nodes[0]] != h {
				return fmt.Sprintf("holder %+v, want %s alone", st.Holder, h.Hostname)
			}
			return ""
		}) == "" {
			return h
		}
	}
	t.Fatalf("%s: the address kept moving between the nodes", what)
	return nil
}

// nodeIDOf is the swarm node ID of n.
func nodeIDOf(ids map[string]*dockertest.Node, n *dockertest.Node) string {
	for id, m := range ids {
		if m == n {
			return id
		}
	}
	return ""
}

// vipProxyContainer is the ID of n's running proxy container, "" while it has none.
func vipProxyContainer(t *testing.T, n *dockertest.Node) string {
	t.Helper()
	out := sh(t, n, "docker ps -q --no-trunc --filter status=running --filter label=com.docker.swarm.service.name="+ingress.ServiceName)
	if ids := strings.Fields(out); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// vipLeft lists what of the virtual IP and the ingress remains on the swarm: services, secrets and networks under their names.
func vipLeft(t *testing.T, mgr *dockertest.Node) []string {
	t.Helper()
	var left []string
	for kind, line := range map[string]string{ //nolint:gosec // G101: shell lines that list names, not credentials
		"service": "docker service ls --format '{{.Name}}'",
		"secret":  "docker secret ls --format '{{.Name}}'",
		"network": "docker network ls --format '{{.Name}}'",
	} {
		for _, name := range strings.Fields(sh(t, mgr, line)) {
			if strings.HasPrefix(name, ingress.ServiceNameKeepalived) || strings.HasPrefix(name, ingress.ServiceName) || strings.HasPrefix(name, ingress.NetworkName) {
				left = append(left, kind+" "+name)
			}
		}
	}
	slices.Sort(left)
	return left
}

// vipAddress is .250 of the network the nodes share, with the prefix length of that network (the harness gives the nodes .10
// up on a /24 of its own, so .250 is no node's).
func vipAddress(t *testing.T, n *dockertest.Node) netip.Prefix {
	t.Helper()
	a, err := netip.ParseAddr(n.IP)
	if err != nil || !a.Is4() {
		t.Fatalf("the node's address %q is not an IPv4 address: %v", n.IP, err)
	}
	b := a.As4()
	b[3] = 250
	return netip.PrefixFrom(netip.AddrFrom4(b), 24)
}

// holdsAddress lists the nodes whose interfaces carry the address.
func holdsAddress(t *testing.T, f *swarmFix, addr netip.Addr) []string {
	t.Helper()
	var held []string
	for _, n := range f.sw.Nodes() {
		if strings.Contains(sh(t, n, "ip -o -4 addr show"), addr.String()+"/") {
			held = append(held, n.Hostname)
		}
	}
	return held
}

// sharedVirtualIP is Phase 26 through the typed API on the shared swarm of three managers and a worker with a real agent on
// each (D-29, VIP-01 to VIP-05, PROH-26-01, PROH-26-04):
//
//  1. Agents installed so that the third manager's agent refuses privileged services and the others allow it: the preview
//     and the enable are refused with vip_privileged_not_allowed, naming the setting and each manager, and nothing is made.
//  2. Every agent allowing: the virtual IP's job runs to done through the agents' policy, and one holder shows.
//  3. The holder's proxy stops: a probe_failing move is listed and another node holds the address.
//  4. The new holder is drained: 200 with a moved handoff, an audit row, a drain_handoff move, and the node, activated again,
//     stays BACKUP.
//  5. The virtual IP and ingress are switched off, the agents uninstalled, and nothing of either remains.
func sharedVirtualIP(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	refusing := f.sw.Managers[2]
	addr := vipAddress(t, mgr)

	// The ingress and keepalived images are on every node as the names the deployer asks for, so no registry is contacted.
	dockertest.LoadImage(t, vipIngressImage, f.sw.Nodes()...)
	dockertest.LoadImage(t, vipKeepalivedImage, f.sw.Nodes()...)
	for _, n := range f.sw.Nodes() {
		sh(t, n, "docker tag "+vipIngressImage+" "+ingressdeploy.ImageRepository+":edge")
		sh(t, n, "docker tag "+vipKeepalivedImage+" "+ingressdeploy.KeepalivedRepository+":edge")
	}

	ids := vipNodeIDs(t, f)

	// A node this case drained is made active again, and a virtual IP or ingress it left on is switched off, however it ends.
	var finished bool
	t.Cleanup(func() {
		if finished {
			return
		}
		for _, n := range f.sw.Nodes() {
			_, _ = mgr.Sh(execCtx(t, time.Minute), "docker node update --availability active "+n.Hostname)
		}
		f.srv.Do(http.MethodDelete, f.ingressPath("/vip"), nil)
		f.srv.Do(http.MethodPost, f.ingressPath("/disable"), nil)
	})

	// One global service, one value per node: a swarm service template gives the third manager's agent false and every other
	// agent true. The opt-in goes in the text the server generated, before the first update flag, so everything else runs as
	// an admin would paste it.
	const anchor = "--update-parallelism 1"
	cmd := f.env.Install.Swarm.Command
	if !strings.Contains(cmd, anchor) {
		t.Fatalf("the generated swarm command has no %q to put the setting before:\n%s", anchor, cmd)
	}
	optIn := `--env 'TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES={{if eq .Node.Hostname "` + refusing.Hostname + `"}}false{{else}}true{{end}}' `
	t.Logf("swarm install with the opt-in by node template (the form that ran: one global service, %s refuses) on %s:\n%s",
		refusing.Hostname, mgr.Hostname, quiet(sh(t, mgr, strings.Replace(cmd, anchor, optIn+anchor, 1))))
	f.waitSwarmOnline(t, 4)

	// Ingress on through the API, the proxies on every node enrolled and ready: the virtual IP is refused without it. The proxy
	// needs no opt-in of any agent (D-03, D-24).
	code, raw := f.srv.Do(http.MethodPost, f.ingressPath("/enable/preview"), map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("POST the ingress enable preview = %d %s", code, raw)
	}
	requireJobDone(t, "ingress enable", f.vipJob(t, http.MethodPost, "/enable", map[string]any{}))
	f.srv.WaitFor("the four proxies to be enrolled and ready", vipProxiesWait, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodGet, f.ingressPath("/status"), nil)
		var st struct {
			Nodes struct {
				Expected int `json:"expected"`
				Enrolled int `json:"enrolled"`
				Ready    int `json:"ready"`
			} `json:"nodes"`
		}
		if code != http.StatusOK || json.Unmarshal(raw, &st) != nil {
			return false, fmt.Sprintf("GET ingress status = %d %s", code, raw)
		}
		return st.Nodes.Expected == 4 && st.Nodes.Enrolled == 4 && st.Nodes.Ready == 4, fmt.Sprintf("%+v", st.Nodes)
	})

	// 1. One manager's agent refuses: the preview and the enable are refused and name every manager (PROH-26-01).
	body := map[string]any{"address": addr.String()}
	code, raw = f.srv.Do(http.MethodPost, f.ingressPath("/vip/preview"), body)
	var pv vipPreviewBody
	if code != http.StatusOK || json.Unmarshal(raw, &pv) != nil {
		t.Fatalf("POST the virtual IP preview = %d %s, want 200 with findings", code, raw)
	}
	refused := false
	for _, fd := range pv.Findings {
		refused = refused || (fd.Severity == "error" && fd.Code == ingressdeploy.CodeVIPPrivilegedNotAllowed)
	}
	if !refused {
		t.Errorf("the preview has no error finding %s: %s", ingressdeploy.CodeVIPPrivilegedNotAllowed, raw)
	}
	if len(pv.Managers) != 3 {
		t.Errorf("the preview lists %d managers, want 3: %s", len(pv.Managers), raw)
	}
	for _, m := range pv.Managers {
		want := "allows"
		if m.Hostname == refusing.Hostname {
			want = "refuses"
		}
		if m.State != want {
			t.Errorf("the preview says %s %s, want %s", m.Hostname, m.State, want)
		}
	}
	code, raw = f.srv.Do(http.MethodPost, f.ingressPath("/vip"), body)
	var problem vipProblemBody
	if code != http.StatusConflict || json.Unmarshal(raw, &problem) != nil || !strings.HasSuffix(problem.Type, ingressdeploy.CodeVIPPrivilegedNotAllowed) {
		t.Fatalf("POST the virtual IP with %s refusing = %d %s, want 409 %s", refusing.Hostname, code, raw, ingressdeploy.CodeVIPPrivilegedNotAllowed)
	}
	if !strings.Contains(problem.Detail, "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true") {
		t.Errorf("the refusal's detail does not name the setting: %s", problem.Detail)
	}
	var items []string
	for _, e := range problem.Errors {
		if e.Location == ingressdeploy.CodeVIPPrivilegedNotAllowed {
			items = e.Value
		}
	}
	for _, m := range f.sw.Managers {
		want := m.Hostname + ": allows"
		if m == refusing {
			want = m.Hostname + ": refuses"
		}
		if !slices.Contains(items, want) {
			t.Errorf("the refusal's items %q lack %q", items, want)
		}
	}
	if svc := strings.TrimSpace(sh(t, mgr, "docker service ls --filter name="+ingress.ServiceNameKeepalived+" -q")); svc != "" {
		t.Fatalf("a keepalived service exists after the refusal: %s", svc)
	}
	if _, ok, code := f.vipStatus(t); ok || code != http.StatusNotFound {
		t.Errorf("GET the virtual IP status after the refusal = ok %v, %d, want 404 vip_not_enabled", ok, code)
	}

	// 2. Every manager allowing: the opt-in is set on the whole service, as a host owner would, and the agents roll.
	sh(t, mgr, "docker service update --env-add TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true "+agentName+" >/dev/null")
	waitServiceUpdated(t, mgr)
	f.waitSwarmOnline(t, 4)
	f.srv.WaitFor("every manager to read allows", installWait, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodPost, f.ingressPath("/vip/preview"), body)
		var pv vipPreviewBody
		if code != http.StatusOK || json.Unmarshal(raw, &pv) != nil {
			return false, fmt.Sprintf("%d %s", code, raw)
		}
		for _, fd := range pv.Findings {
			if fd.Severity == "error" {
				return false, string(raw)
			}
		}
		return true, ""
	})
	requireJobDone(t, "virtual IP enable", f.vipJob(t, http.MethodPost, "/vip", body))
	if svc := strings.TrimSpace(sh(t, mgr, "docker service ls --filter name="+ingress.ServiceNameKeepalived+" -q")); svc == "" {
		t.Fatal("the enable job is done but there is no keepalived service")
	}
	// A holder that loses the address before it settled was never the holder the history moves from, so wait until it has.
	f.vipHolder(t, ids, "one holder of "+addr.String(), vipHolderWait, nil)
	f.vipSettled(t, ids, "the first holder of "+addr.String()+" to settle", nil)
	st, holder := f.vipHolder(t, ids, "one holder of "+addr.String(), vipHolderWait, nil)
	t.Logf("the virtual IP %s is held by %s: %+v", addr, holder.Hostname, st.Candidates)
	if !st.Enabled || st.Address != addr.String() {
		t.Errorf("status = enabled %v address %s, want on and %s", st.Enabled, st.Address, addr)
	}
	if held := holdsAddress(t, f, addr.Addr()); !slices.Equal(held, []string{holder.Hostname}) {
		t.Errorf("the nodes carrying %s are %v, the status says %s alone", addr.Addr(), held, holder.Hostname)
	}

	// 3. The holder's proxy stops: the address moves and the reader lists a probe_failing move.
	proxy := vipProxyContainer(t, holder)
	if proxy == "" {
		t.Fatalf("%s runs no proxy container", holder.Hostname)
	}
	sh(t, holder, "docker stop "+proxy)
	f.srv.WaitFor("a probe_failing move away from "+holder.Hostname, vipMoveWait, func() (bool, string) {
		// The history is searched for who and why, wherever the move sits.
		all := f.vipMoves(t)
		for _, m := range all {
			if m.Reason == "probe_failing" && m.FromNode == holder.Hostname && m.ToNode != "" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d moves: %+v", len(all), all)
	})
	second := f.vipSettled(t, ids, "another node to hold the address after the proxy on "+holder.Hostname+" stopped",
		func(n *dockertest.Node) bool { return n != holder })

	// 4. The new holder is drained: the address moves first, the drain is made, and the activated node stays BACKUP.
	code, raw = f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/nodes/"+nodeIDOf(ids, second), nil)
	var node struct {
		Version uint64 `json:"version"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &node) != nil {
		t.Fatalf("GET the node %s = %d %s", second.Hostname, code, raw)
	}
	code, raw = f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/nodes/"+nodeIDOf(ids, second)+"/availability",
		map[string]any{"availability": "drain", "version": node.Version, "confirm": second.Hostname})
	var drain struct {
		Handoff struct {
			Outcome string `json:"outcome"`
			Holder  string `json:"holder"`
		} `json:"vipHandoff"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &drain) != nil || drain.Handoff.Outcome != "moved" || drain.Handoff.Holder == "" {
		t.Fatalf("POST drain of the holder %s = %d %s, want 200 with vipHandoff moved and a holder", second.Hostname, code, raw)
	}
	if drain.Handoff.Holder == second.Hostname {
		t.Errorf("the handoff names the drained node %s as the new holder", second.Hostname)
	}
	if nodeByHostname(f, drain.Handoff.Holder) == nil {
		t.Fatalf("the handoff's holder %q is not a node of the swarm", drain.Handoff.Holder)
	}
	// The handoff names the first node to report MASTER; the address may pass on to another before it settles.
	third := f.vipSettled(t, ids, "a node other than the drained "+second.Hostname+" to hold the address", func(n *dockertest.Node) bool { return n != second })

	code, raw = f.srv.Do(http.MethodGet, "/api/audit?action=ingress.vip_handoff", nil)
	var audit struct {
		Items []struct {
			Action  string            `json:"action"`
			Result  string            `json:"result"`
			Details map[string]string `json:"details"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &audit) != nil || len(audit.Items) != 1 {
		t.Fatalf("GET the audit log for ingress.vip_handoff = %d %s, want one entry", code, raw)
	}
	if d := audit.Items[0]; d.Result != "success" || d.Details["node"] != second.Hostname || d.Details["outcome"] != "moved" || d.Details["holder"] != drain.Handoff.Holder {
		t.Errorf("the handoff's audit entry = %+v, want success, node %s, outcome moved, holder %s", d, second.Hostname, drain.Handoff.Holder)
	}
	f.srv.WaitFor("a drain_handoff move away from "+second.Hostname, vipMoveWait, func() (bool, string) {
		all := f.vipMoves(t)
		for _, m := range all {
			if m.Reason == "drain_handoff" && m.FromNode == second.Hostname && m.ToNode != "" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d moves: %+v", len(all), all)
	})

	// The node is active again: it joins as a BACKUP and never takes the address back while the holder is healthy.
	sh(t, mgr, "docker node update --availability active "+second.Hostname)
	f.srv.WaitFor(second.Hostname+" to report BACKUP", vipJobWait, func() (bool, string) {
		st, ok, code := f.vipStatus(t)
		if !ok {
			return false, fmt.Sprintf("GET the status = %d", code)
		}
		for _, c := range st.Candidates {
			if c.Hostname == second.Hostname {
				return c.VRRPState == "BACKUP", "state " + c.VRRPState
			}
		}
		return false, "not yet a candidate"
	})
	if v := vipWatch(vipSettleWatch, func() string {
		st, ok, _ := f.vipStatus(t)
		if !ok || len(st.Holder.Nodes) != 1 || ids[st.Holder.Nodes[0]] != third {
			return fmt.Sprintf("holder %+v, want %s alone", st.Holder, third.Hostname)
		}
		return ""
	}); v != "" {
		t.Fatalf("while %s was active again, the holder changed: %s", second.Hostname, v)
	}

	// 5. Everything off through the API, the agents uninstalled, nothing left behind.
	requireJobDone(t, "virtual IP switch-off", f.vipJob(t, http.MethodDelete, "/vip", nil))
	requireJobDone(t, "ingress switch-off", f.vipJob(t, http.MethodPost, "/disable", nil))
	var left []string
	f.srv.WaitFor("no tierplan-keepalived or tierplan-ingress object to remain", vipGoneWait, func() (bool, string) {
		left = vipLeft(t, mgr)
		return len(left) == 0, fmt.Sprint(left)
	})
	if held := holdsAddress(t, f, addr.Addr()); len(held) != 0 {
		t.Errorf("the address is still carried by %v after the switch-off", held)
	}
	for _, n := range f.sw.Nodes() {
		if got := strings.TrimSpace(sh(t, mgr, "docker node inspect -f '{{.Spec.Availability}}' "+n.Hostname)); got != "active" {
			t.Errorf("%s is %s, want active", n.Hostname, got)
		}
	}
	finished = true

	code, raw = f.srv.Do(http.MethodDelete, "/api/environments/"+f.env.ID+"?uninstallAgents=true", nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE the environment with uninstallAgents = %d %s, want 204", code, raw)
	}
	f.srv.WaitFor("the agent service to be gone", vipGoneWait, func() (bool, string) {
		out, _ := mgr.Sh(execCtx(t, time.Minute), "docker service ls --format '{{.Name}}'")
		return !strings.Contains(out, agentName), strings.TrimSpace(out)
	})
	if left := vipLeft(t, mgr); len(left) != 0 {
		t.Errorf("after the agents were uninstalled the swarm still holds %v", left)
	}
}

// nodeByHostname is the node of the swarm with the host name, nil when there is none.
func nodeByHostname(f *swarmFix, hostname string) *dockertest.Node {
	for _, n := range f.sw.Nodes() {
		if n.Hostname == hostname {
			return n
		}
	}
	return nil
}

// vipWatch polls violation every two seconds for window and returns the first violation, "" when there was none. It is for a
// case that must keep holding for a stretch of time, which the waits (they fail when the window ends) cannot express.
func vipWatch(window time.Duration, violation func() string) string {
	expired := time.NewTimer(window) // wall-clock: a bounded observation window of a case that must not change; it can only false-pass, never fail a correct run
	defer expired.Stop()
	tick := time.NewTicker(2 * time.Second) // wall-clock: the pace of that observation
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
