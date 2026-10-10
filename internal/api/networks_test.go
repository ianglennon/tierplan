// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Network create (plan 04-12, RES-06, D-39, D-40, D-51, P2 D-42). Docker is a fake
// daemon behind the real monitor, snapshot cache and aggregator; the router, policy,
// CSRF guard and audit writer are the real ones.

func networkEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
		},
	}
}

type networkCreated struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// createNet posts a create as the signed-in admin. A write goes only to a node the
// monitor has seen online, so it waits for the first probe like act and remove do.
func (f *envFix) createNet(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks", body)
}

func (f *envFix) networkAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'network.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCreateNetworkAsAnEnvAdminAnswers201SendsTheFieldsOnceAndAuditsOnce(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	f.addUser(t, "netadmin", false)
	f.addEnvRole(t, "netadmin", e.ID, "admin")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "netadmin", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/networks",
		body:   `{"name":"app-net","driver":"bridge","labels":{"team":"a"}}`,
		cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got networkCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID == "" || got.Name != "app-net" {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /networks/create"}) {
		t.Fatalf("writes = %v, want one create", w)
	}
	creates := f.fake.NetworkCreates()
	if len(creates) != 1 || creates[0].Name != "app-net" || creates[0].Driver != "bridge" || creates[0].Labels["team"] != "a" {
		t.Fatalf("the engine saw %+v", creates)
	}

	rows := f.networkAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionNetworkCreated || r.Result != string(audit.ResultSuccess) || r.TargetType != "network" || r.TargetID != got.ID || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "app-net", "node": e.ID, "driver": "bridge", "scope": "local", "code": "created"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}

	// The list reads the network the write made.
	list := decode[listBody[networkItem]](t, f.get(t, e, "networks"))
	if !slices.ContainsFunc(list.Items, func(n networkItem) bool { return n.Name == "app-net" && n.Driver == "bridge" }) {
		t.Fatalf("networks after the create = %+v", list.Items)
	}
}

func TestCreateNetworkTheSameNameAgainIs409NameTakenAndTheFailureIsAudited(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	if rec := f.createNet(t, e, `{"name":"app-net","driver":"bridge"}`); rec.code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.code, rec.body)
	}
	rec := f.createNet(t, e, `{"name":"app-net","driver":"bridge"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" {
		t.Fatalf("second create: %d %q, want 409 name_taken: %s", rec.code, rec.ptyp, rec.body)
	}
	rows := f.networkAudits(t)
	if len(rows) != 2 || rows[1].Result != string(audit.ResultFailure) || rows[1].Reason != "name_taken" || rows[1].Details["code"] != "name_taken" {
		t.Fatalf("audit rows = %+v, want a success then a failure named name_taken", rows)
	}
}

func TestCreateNetworkTheSchemaRefusesBadNamesAndDriversBeforeAnyDockerCall(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for name, body := range map[string]string{
		"a name with a space":         `{"name":"my net","driver":"bridge"}`,
		"an empty name":               `{"name":"","driver":"bridge"}`,
		"a name starting with a dash": `{"name":"-net","driver":"bridge"}`,
		"a name of 129 characters":    fmt.Sprintf(`{"name":%q,"driver":"bridge"}`, strings.Repeat("a", 129)),
		"the host driver":             `{"name":"n","driver":"host"}`,
		"the null driver":             `{"name":"n","driver":"null"}`,
		"a third party driver":        `{"name":"n","driver":"weave"}`,
		"no driver":                   `{"name":"n"}`,
		"an ingress flag":             `{"name":"n","driver":"overlay","ingress":true}`,
		"a config-only flag":          `{"name":"n","driver":"bridge","configOnly":true}`,
		"a config-from reference":     `{"name":"n","driver":"bridge","configFrom":"base"}`,
		"more than 64 labels":         fmt.Sprintf(`{"name":"n","driver":"bridge","labels":%s}`, manyEntries(65)),
		"more than 64 driver options": fmt.Sprintf(`{"name":"n","driver":"bridge","options":%s}`, manyEntries(65)),
	} {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks", body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("a refused body was audited as an attempt: %+v", rows)
	}
}

func manyEntries(n int) string {
	parts := make([]string, 0, n)
	for i := range n {
		parts = append(parts, fmt.Sprintf(`"k%d":"v"`, i))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestCreateNetworkAnOperatorAndAViewerAreRefusedAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })

	for _, who := range []string{"viewer", "operator"} {
		cookie, csrf := f.signIn(t, who, false)
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/networks",
			body: `{"name":"app-net","driver":"bridge"}`, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403: %s", who, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("network audit rows = %+v, want none", rows)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "POST /api/env/{envId}/networks" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}
}

func TestCreateNetworkAnUnknownEnvironmentIs404(t *testing.T) {
	f, _ := dashEnv(t, networkEngine())
	rec := f.req(t, http.MethodPost, "/api/env/nope/networks", `{"name":"n","driver":"bridge"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("%d %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
}

// Where a network is made on a swarm, and the addressing the server checks (plan 04-12
// task 2, D-39, D-40, P2 D-42).

// writesOf lists, per agent, the writes its daemon served, in a swarm fixture.
func (s *swarmFix) createsOn() map[string][]string {
	out := map[string][]string{}
	for name, a := range map[string]*agentConn{"m1": s.m1, "m2": s.m2, "w": s.w} {
		var creates []string
		for _, w := range a.fake.Writes() {
			if strings.HasPrefix(w, "POST /networks/create") {
				creates = append(creates, w)
			}
		}
		out[name] = creates
	}
	return out
}

// only returns the conn that served create, failing unless it was exactly one.
func (s *swarmFix) onlyCreate(t *testing.T) *agentConn {
	t.Helper()
	got := s.createsOn()
	total := len(got["m1"]) + len(got["m2"]) + len(got["w"])
	if total != 1 {
		t.Fatalf("creates served = %v, want exactly one in the whole swarm", got)
	}
	switch {
	case len(got["m1"]) == 1:
		return s.m1
	case len(got["m2"]) == 1:
		return s.m2
	}
	return s.w
}

func (s *swarmFix) post(t *testing.T, body string) *envRec {
	t.Helper()
	return s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/networks", body)
}

func TestCreateNetworkOverlayOnASwarmGoesThroughOneManagerOnceAndIsListedOnceAsCluster(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.post(t, `{"name":"new-ovl","driver":"overlay","attachable":true}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	a := s.onlyCreate(t)
	if a == s.w {
		t.Fatal("the create went to the worker, not to a manager")
	}
	creates := a.fake.NetworkCreates()
	if len(creates) != 1 || creates[0].Driver != "overlay" || creates[0].Scope != "swarm" || !creates[0].Attachable {
		t.Fatalf("the manager saw %+v, want an attachable overlay of scope swarm", creates)
	}

	list := decode[nodeListBody[nodeNetwork]](t, s.get(t, s.env.ID, "networks"))
	var rows []nodeNetwork
	for _, n := range list.Items {
		if n.Name == "new-ovl" {
			rows = append(rows, n)
		}
	}
	if len(rows) != 1 || rows[0].Scope != "swarm" || rows[0].Node != "Cluster" {
		t.Fatalf("the list holds %+v for new-ovl, want one swarm row on Cluster", rows)
	}

	audits := s.networkAudits(t)
	if len(audits) != 1 {
		t.Fatalf("audit rows = %+v, want one", audits)
	}
	d := audits[0].Details
	if d["name"] != "new-ovl" || d["driver"] != "overlay" || d["scope"] != "swarm" || d["attachable"] != "true" || d["code"] != "created" {
		t.Errorf("audit details = %+v", d)
	}
	if _, has := d["node"]; has {
		t.Errorf("a cluster network names no node: %+v", d)
	}
}

func TestCreateNetworkALocalDriverOnASwarmNeedsANodeAndGoesToThatNodeOnly(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.post(t, `{"name":"br-a","driver":"bridge"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "node_required" {
		t.Fatalf("without a node: %d %q, want 422 node_required: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := s.createsOn(); len(got["m1"])+len(got["m2"])+len(got["w"]) != 0 {
		t.Fatalf("a refused create reached an engine: %v", got)
	}

	rec = s.post(t, fmt.Sprintf(`{"name":"br-a","driver":"bridge","nodeId":%q}`, s.w.id))
	if rec.code != http.StatusCreated {
		t.Fatalf("with a node: %d %s", rec.code, rec.body)
	}
	if a := s.onlyCreate(t); a != s.w {
		t.Fatal("the create did not go to the node that was named")
	}
	audits := s.networkAudits(t)
	if len(audits) != 1 || audits[0].Details["node"] != s.w.id || audits[0].Details["scope"] != "local" || audits[0].Details["driver"] != "bridge" {
		t.Fatalf("audit rows = %+v, want the worker's node ID, scope local", audits)
	}
}

func TestCreateNetworkANodeThatIsNotOneOfTheEnvironmentsIs503AndNothingIsSent(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.post(t, `{"name":"br-a","driver":"bridge","nodeId":"not-a-node"}`)
	if rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Fatalf("%d %q, want 503 node_unavailable: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := s.createsOn(); len(got["m1"])+len(got["m2"])+len(got["w"]) != 0 {
		t.Fatalf("a create reached an engine: %v", got)
	}
}

func TestCreateNetworkAttachableIsForAnOverlayOnASwarmOnly(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.post(t, fmt.Sprintf(`{"name":"br-a","driver":"bridge","nodeId":%q,"attachable":true}`, s.w.id))
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "attachable_overlay_only" {
		t.Fatalf("a bridge: %d %q, want 422 attachable_overlay_only: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := s.createsOn(); len(got["m1"])+len(got["m2"])+len(got["w"]) != 0 {
		t.Fatalf("a refused create reached an engine: %v", got)
	}

	f, e := dashEnv(t, networkEngine())
	rec = f.createNet(t, e, `{"name":"n","driver":"bridge","attachable":true}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "attachable_overlay_only" {
		t.Fatalf("off a swarm: %d %q, want 422 attachable_overlay_only: %s", rec.code, rec.ptyp, rec.body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused create reached the engine: %v", w)
	}
}

func TestCreateNetworkAnOverlayOffASwarmIsRefusedBeforeAnyCall(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	rec := f.createNet(t, e, `{"name":"n","driver":"overlay"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "overlay_needs_swarm" {
		t.Fatalf("%d %q, want 422 overlay_needs_swarm: %s", rec.code, rec.ptyp, rec.body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused create reached the engine: %v", w)
	}
}

func TestCreateNetworkAcceptsAddressingInsideTheSubnetAndPassesOptionsAndIPv6Through(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	rec := f.createNet(t, e, `{"name":"addr","driver":"bridge","subnet":"10.20.0.0/24","gateway":"10.20.0.1","ipRange":"10.20.0.128/25","ipv6":true,"options":{"com.docker.network.bridge.name":"br-addr"}}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	creates := f.fake.NetworkCreates()
	if len(creates) != 1 {
		t.Fatalf("creates = %+v", creates)
	}
	req := creates[0]
	if req.IPAM == nil || len(req.IPAM.Config) != 1 ||
		req.IPAM.Config[0].Subnet.String() != "10.20.0.0/24" || req.IPAM.Config[0].Gateway.String() != "10.20.0.1" || req.IPAM.Config[0].IPRange.String() != "10.20.0.128/25" {
		t.Errorf("IPAM = %+v", req.IPAM)
	}
	if req.EnableIPv6 == nil || !*req.EnableIPv6 || req.Options["com.docker.network.bridge.name"] != "br-addr" {
		t.Errorf("body = %+v", req)
	}
	audits := f.networkAudits(t)
	if len(audits) != 1 || audits[0].Details["subnet"] != "10.20.0.0/24" || audits[0].Details["gateway"] != "10.20.0.1" {
		t.Errorf("audit rows = %+v, want the subnet and gateway recorded", audits)
	}

	// An IPv6 subnet is a subnet too.
	if rec := f.createNet(t, e, `{"name":"six","driver":"bridge","subnet":"fd00:20::/64","gateway":"fd00:20::1","ipv6":true}`); rec.code != http.StatusCreated {
		t.Fatalf("an IPv6 subnet: %d %s", rec.code, rec.body)
	}
}

func TestCreateNetworkRefusesBadAddressingWithItsOwnProblemTypeAndSendsNothing(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for name, tc := range map[string]struct{ body, ptyp string }{
		"a subnet with no prefix length":    {`{"name":"n","driver":"bridge","subnet":"10.20.0.0"}`, "subnet_invalid"},
		"a subnet that is not an address":   {`{"name":"n","driver":"bridge","subnet":"banana/24"}`, "subnet_invalid"},
		"a prefix longer than the address":  {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/33"}`, "subnet_invalid"},
		"a gateway with no subnet":          {`{"name":"n","driver":"bridge","gateway":"10.20.0.1"}`, "subnet_invalid"},
		"an IP range with no subnet":        {`{"name":"n","driver":"bridge","ipRange":"10.20.0.0/25"}`, "subnet_invalid"},
		"a gateway outside the subnet":      {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/24","gateway":"10.20.1.1"}`, "gateway_outside_subnet"},
		"a gateway that is not an address":  {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/24","gateway":"gw"}`, "gateway_outside_subnet"},
		"an IPv6 gateway in an IPv4 subnet": {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/24","gateway":"fd00::1"}`, "gateway_outside_subnet"},
		"an IP range outside the subnet":    {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/24","ipRange":"10.20.1.0/25"}`, "iprange_outside_subnet"},
		"an IP range wider than the subnet": {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/24","ipRange":"10.20.0.0/16"}`, "iprange_outside_subnet"},
		"an IP range with no prefix length": {`{"name":"n","driver":"bridge","subnet":"10.20.0.0/24","ipRange":"10.20.0.5"}`, "iprange_outside_subnet"},
	} {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks", tc.body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.ptyp {
			t.Errorf("%s: %d %q, want 422 %s: %s", name, rec.code, rec.ptyp, tc.ptyp, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused create reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("a refused body was audited as an attempt: %+v", rows)
	}
}

func TestCreateNetworkTheGatewayMayBeTheFirstAndLastAddressOfTheSubnetButNotTheNextOne(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	for i, tc := range []struct {
		gateway string
		code    int
	}{
		{"10.20.0.0", http.StatusCreated},
		{"10.20.0.255", http.StatusCreated},
		{"10.20.1.0", http.StatusUnprocessableEntity},
		{"10.19.255.255", http.StatusUnprocessableEntity},
	} {
		body := fmt.Sprintf(`{"name":"edge%d","driver":"bridge","subnet":"10.20.0.0/24","gateway":%q}`, i, tc.gateway)
		if rec := f.createNet(t, e, body); rec.code != tc.code {
			t.Errorf("gateway %s: %d, want %d: %s", tc.gateway, rec.code, tc.code, rec.body)
		}
	}
}

func TestCreateNetworkOnAReadOnlyManagerIsHostReadOnlyAuditedAndNothingIsSent(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Locked swarm")
	cfg := networkEngine()
	cfg.SwarmState, cfg.SwarmControl = "active", true
	m := f.connect(t, env, cfg, &wire.Facts{
		EngineID: "ENG:ro", Hostname: "node-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true},
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-ro", ClusterID: "cluster-1", Control: true},
	})
	f.whenState(t, env.ID, "online")
	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/networks", `{"name":"ovl","driver":"overlay"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "host_read_only" {
		t.Fatalf("%d %q, want 409 host_read_only: %s", rec.code, rec.ptyp, rec.body)
	}
	if w := m.fake.Writes(); len(w) != 0 {
		t.Fatalf("a read-only host received %v", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "host_read_only" {
		t.Fatalf("audit rows = %+v, want one failure host_read_only", rows)
	}
}

func TestCreateNetworkIsNeverRetriedOnAnotherManagerWhenTheAnswerIsLost(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Flaky swarm")
	var conns []*agentConn
	for i := 1; i <= 2; i++ {
		cfg := networkEngine()
		cfg.Hostname = fmt.Sprintf("mgr-%d", i)
		cfg.SwarmState, cfg.SwarmControl, cfg.DropWrites = "active", true, true
		conns = append(conns, f.connect(t, env, cfg, &wire.Facts{
			EngineID: fmt.Sprintf("ENG:m%d", i), Hostname: cfg.Hostname, EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: "active", NodeID: fmt.Sprintf("node-m%d", i), ClusterID: "cluster-1", Control: true},
		}))
	}
	f.whenState(t, env.ID, "online")
	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/networks", `{"name":"ovl","driver":"overlay"}`)
	if rec.code < 400 {
		t.Fatalf("status = %d, want an error for a lost answer: %s", rec.code, rec.body)
	}
	total := 0
	for _, c := range conns {
		total += len(c.fake.Writes())
	}
	if total != 1 {
		t.Fatalf("creates attempted = %d, want exactly one across both managers", total)
	}
	rows := f.networkAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) {
		t.Fatalf("audit rows = %+v, want one failure", rows)
	}
}

// Network delete (plan 04-13, RES-06, D-41, D-51, P2 D-42). Same harness as the create.

func deleteEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "ho0", Name: "host", Driver: "host", Scope: "local"},
			{ID: "mesh0", Name: "my-mesh", Driver: "overlay", Scope: "local", Ingress: true},
			{ID: "app0", Name: "app-net", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24"}},
		},
	}
}

// deleteNet sends a delete as the signed-in admin once the monitor has seen the node.
func (f *envFix) deleteNet(t *testing.T, e envBody, id string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodDelete, "/api/env/"+e.ID+"/networks/"+id, "")
}

func TestDeleteNetworkAnUnusedOneAsAnEnvAdminAnswers204AuditsOnceAndASecondIs404(t *testing.T) {
	f, e := dashEnv(t, deleteEngine())
	f.addUser(t, "netadmin", false)
	f.addEnvRole(t, "netadmin", e.ID, "admin")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "netadmin", false)
	rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/networks/app0", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /networks/app0"}) {
		t.Fatalf("writes = %v, want one delete", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionNetworkRemoved || r.Result != string(audit.ResultSuccess) || r.TargetType != "network" || r.TargetID != "app0" || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "app-net", "node": e.ID, "driver": "bridge", "scope": "local", "code": "removed"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}
	list := decode[listBody[networkItem]](t, f.get(t, e, "networks"))
	if slices.ContainsFunc(list.Items, func(n networkItem) bool { return n.Name == "app-net" }) {
		t.Fatalf("the list still holds app-net: %+v", list.Items)
	}

	again := f.deleteNet(t, e, "app0")
	if again.code != http.StatusNotFound || again.ptyp != "network_not_found" {
		t.Fatalf("a second delete: %d %q, want 404 network_not_found: %s", again.code, again.ptyp, again.body)
	}
	if w := f.fake.Writes(); len(w) != 1 {
		t.Fatalf("a delete of a network that is gone reached the engine: %v", w)
	}
}

func TestDeleteNetworkABuiltInOneIs409NetworkBuiltinSendsNothingAndIsAudited(t *testing.T) {
	f, e := dashEnv(t, deleteEngine())
	for _, id := range []string{"br0", "ho0", "mesh0"} {
		rec := f.deleteNet(t, e, id)
		if rec.code != http.StatusConflict || rec.ptyp != "network_builtin" {
			t.Fatalf("%s: %d %q, want 409 network_builtin: %s", id, rec.code, rec.ptyp, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a built-in network reached the engine: %v", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 3 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "network_builtin" || rows[0].Details["name"] != "bridge" {
		t.Fatalf("audit rows = %+v, want three failures named network_builtin", rows)
	}
}

func TestDeleteNetworkAnOperatorAndAViewerAreRefusedAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, deleteEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for _, who := range []string{"viewer", "operator"} {
		cookie, csrf := f.signIn(t, who, false)
		rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/networks/app0", cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403: %s", who, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("network audit rows = %+v, want none", rows)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "DELETE /api/env/{envId}/networks/{networkId}" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}
}

func TestDeleteNetworkAnUnknownEnvironmentIs404(t *testing.T) {
	f, _ := dashEnv(t, deleteEngine())
	rec := f.req(t, http.MethodDelete, "/api/env/nope/networks/app0", "")
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("%d %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
}

// The networks list says what uses each network, who owns it and whether it can go (plan
// 04-13 task 2, D-40, D-41).

type networkListItem struct {
	Node               string   `json:"node"`
	NodeID             string   `json:"nodeId"`
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Driver             string   `json:"driver"`
	Scope              string   `json:"scope"`
	Subnets            []string `json:"subnets"`
	AttachedContainers int      `json:"attachedContainers"`
	AttachedServices   int      `json:"attachedServices"`
	Owner              string   `json:"owner"`
	OwnerKind          string   `json:"ownerKind"`
	BuiltIn            bool     `json:"builtIn"`
	Internal           bool     `json:"internal"`
	Attachable         bool     `json:"attachable"`
	IPv6               bool     `json:"ipv6"`
}

func listEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "app0", Name: "app-net", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24"}, Internal: true,
				Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "prj0", Name: "blog_default", Driver: "macvlan", Scope: "local", Subnets: []string{"192.168.5.0/24", "192.168.6.0/24"}, Attachable: true,
				Labels: map[string]string{"com.docker.compose.project": "blog"}},
			{ID: "lone0", Name: "lonely", Driver: "bridge", Scope: "local"},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}}},
			{ID: "c2", Names: []string{"/db"}, State: "exited", Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}}},
			{ID: "c3", Names: []string{"/blog"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"blog_default": {NetworkID: "prj0"}, "bridge": {NetworkID: "br0"}}},
		},
	}
}

func listNets(t *testing.T, f *envFix, e envBody, query string) listBody[networkListItem] {
	t.Helper()
	rec := f.get(t, e, "networks"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET networks%s: %d %s", query, rec.code, rec.body)
	}
	return decode[listBody[networkListItem]](t, rec)
}

func netByName(t *testing.T, items []networkListItem, name string) networkListItem {
	t.Helper()
	for _, n := range items {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no network %q in %+v", name, items)
	return networkListItem{}
}

func TestNetworkListCountsContainersOnAnyStateNamesTheOwnerAndFlagsTheBuiltIns(t *testing.T) {
	f, e := dashEnv(t, listEngine())
	list := listNets(t, f, e, "")
	if list.Total != 4 {
		t.Fatalf("total = %d: %+v", list.Total, list.Items)
	}
	app := netByName(t, list.Items, "app-net")
	if app.AttachedContainers != 2 || app.AttachedServices != 0 || app.Owner != "shop" || app.OwnerKind != "stack" || !app.Internal || app.BuiltIn || app.Attachable {
		t.Errorf("app-net = %+v, want a stopped container counted too, stack shop, internal", app)
	}
	blog := netByName(t, list.Items, "blog_default")
	if blog.AttachedContainers != 1 || blog.Owner != "blog" || blog.OwnerKind != "project" || !blog.Attachable || len(blog.Subnets) != 2 {
		t.Errorf("blog_default = %+v", blog)
	}
	if br := netByName(t, list.Items, "bridge"); !br.BuiltIn || br.AttachedContainers != 1 || br.Owner != "" {
		t.Errorf("bridge = %+v, want built in with the one container on it", br)
	}
	if lone := netByName(t, list.Items, "lonely"); lone.AttachedContainers != 0 || lone.AttachedServices != 0 || lone.BuiltIn {
		t.Errorf("lonely = %+v, want nothing attached", lone)
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.Name)
	}
	if !slices.IsSorted(names) {
		t.Errorf("order = %v, want by name", names)
	}
}

func TestNetworkListSearchMatchesNameDriverSubnetAndOwnerBeforeTheTotal(t *testing.T) {
	f, e := dashEnv(t, listEngine())
	for q, want := range map[string][]string{
		"app":         {"app-net"},
		"MACVLAN":     {"blog_default"},
		"192.168.6":   {"blog_default"},
		"10.20.0.0/2": {"app-net"},
		"shop":        {"app-net"},
		"blog":        {"blog_default"},
		"bridge":      {"app-net", "bridge", "lonely"},
		"no-such":     nil,
	} {
		list := listNets(t, f, e, "?q="+q)
		var got []string
		for _, n := range list.Items {
			got = append(got, n.Name)
		}
		if !slices.Equal(got, want) || list.Total != len(want) {
			t.Errorf("q=%s: %v (total %d), want %v", q, got, list.Total, want)
		}
	}
	if rec := f.get(t, e, "networks?q="+strings.Repeat("a", 129)); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a 129 character search: %d, want 422", rec.code)
	}
}

func TestNetworkListOnASwarmCountsTheServicesThatMentionAnOverlayEvenWithNoTasks(t *testing.T) {
	s := newSwarmFix(t)
	list := decode[listBody[networkListItem]](t, s.get(t, s.env.ID, "networks"))
	ovl := netByName(t, list.Items, "ovl")
	if ovl.AttachedServices != 2 || ovl.Node != "Cluster" || ovl.Scope != "swarm" {
		t.Errorf("ovl = %+v, want both services counted, the one with 0 replicas too", ovl)
	}
	for _, n := range list.Items {
		if n.Name == "bridge" && n.AttachedServices != 0 {
			t.Errorf("a local bridge on %s counts services: %+v", n.Node, n)
		}
	}
	keys := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		keys = append(keys, n.Name+"\x00"+n.ID+"\x00"+n.Node)
	}
	if !slices.IsSorted(keys) {
		t.Errorf("order = %q, want name, then ID, then node", keys)
	}
}

// The in-use check before every delete (plan 04-13 task 3, D-41, T-04-45, Pitfall 7).

// deletesOn lists, per agent, the network deletes its daemon served, in a swarm fixture.
func (s *swarmFix) deletesOn() map[string][]string {
	out := map[string][]string{}
	for name, a := range map[string]*agentConn{"m1": s.m1, "m2": s.m2, "w": s.w} {
		var dels []string
		for _, w := range a.fake.Writes() {
			if strings.HasPrefix(w, "DELETE /networks/") {
				dels = append(dels, w)
			}
		}
		out[name] = dels
	}
	return out
}

func (s *swarmFix) noDeletes(t *testing.T) {
	t.Helper()
	if got := s.deletesOn(); len(got["m1"])+len(got["m2"])+len(got["w"]) != 0 {
		t.Fatalf("a refused delete reached an engine: %v", got)
	}
}

func (s *swarmFix) del(t *testing.T, id, nodeID string) *envRec {
	t.Helper()
	path := "/api/env/" + s.env.ID + "/networks/" + id
	if nodeID != "" {
		path += "?nodeId=" + nodeID
	}
	return s.req(t, http.MethodDelete, path, "")
}

func TestDeleteNetworkWithAStoppedContainerAttachedIs409InUseNamingItAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, listEngine())
	rec := f.deleteNet(t, e, "app0")
	if rec.code != http.StatusConflict || rec.ptyp != "network_in_use" {
		t.Fatalf("%d %q, want 409 network_in_use: %s", rec.code, rec.ptyp, rec.body)
	}
	for _, name := range []string{"web", "db"} {
		if !strings.Contains(string(rec.body), name) {
			t.Errorf("the detail does not name %s: %s", name, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("an in-use network reached the engine: %v", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "network_in_use" || rows[0].Details["name"] != "app-net" {
		t.Fatalf("audit rows = %+v, want one failure named network_in_use", rows)
	}
}

func TestDeleteNetworkReadsTheContainersLiveAndNotTheCachedList(t *testing.T) {
	cfg := listEngine()
	cfg.Containers = nil
	f, e := dashEnv(t, cfg)
	// The list is read and cached while nothing is attached.
	if list := listNets(t, f, e, ""); netByName(t, list.Items, "app-net").AttachedContainers != 0 {
		t.Fatalf("list = %+v", list.Items)
	}
	// A container joins; the cached list does not know yet.
	cfg.Containers = listEngine().Containers
	f.fake.Reconfigure(cfg)
	rec := f.deleteNet(t, e, "app0")
	if rec.code != http.StatusConflict || rec.ptyp != "network_in_use" {
		t.Fatalf("%d %q, want 409 network_in_use: %s", rec.code, rec.ptyp, rec.body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("the delete trusted a stale list and reached the engine: %v", w)
	}
}

func TestDeleteNetworkNamesAtMostFiveUsers(t *testing.T) {
	cfg := listEngine()
	cfg.Containers = nil
	for i := range 8 {
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
			ID: fmt.Sprintf("u%d", i), Names: []string{fmt.Sprintf("/user-%d", i)}, State: "exited",
			Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}},
		})
	}
	f, e := dashEnv(t, cfg)
	rec := f.deleteNet(t, e, "app0")
	if rec.code != http.StatusConflict || rec.ptyp != "network_in_use" {
		t.Fatalf("%d %q: %s", rec.code, rec.ptyp, rec.body)
	}
	named := 0
	for i := range 8 {
		if strings.Contains(string(rec.body), fmt.Sprintf("user-%d", i)) {
			named++
		}
	}
	if named != 5 || !strings.Contains(string(rec.body), "3 more") {
		t.Fatalf("named %d users, want 5 and '3 more': %s", named, rec.body)
	}
}

func TestDeleteNetworkADaemonRefusalForAnAttachmentTheListMissedIsInUseNeverAGenericFailure(t *testing.T) {
	cfg := listEngine()
	// This endpoint is not tied to the network by ID in the list, as when a container
	// joins between the check and the call; the daemon still sees it.
	cfg.Containers = []dockertest.FakeContainer{{ID: "late", Names: []string{"/late"}, State: "running",
		Networks: map[string]dockertest.FakeEndpoint{"app-net": {}}}}
	f, e := dashEnv(t, cfg)
	rec := f.deleteNet(t, e, "app0")
	if rec.code != http.StatusConflict || rec.ptyp != "network_in_use" {
		t.Fatalf("%d %q, want 409 network_in_use: %s", rec.code, rec.ptyp, rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /networks/app0"}) {
		t.Fatalf("writes = %v, want the one delete the daemon refused", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 1 || rows[0].Reason != "network_in_use" || rows[0].Result != string(audit.ResultFailure) {
		t.Fatalf("audit rows = %+v", rows)
	}
}

func TestDeleteNetworkAnOverlayThatOnlyAScaledToZeroServiceMentionsIs409InUseWithoutAnyCachedRead(t *testing.T) {
	s := newSwarmFix(t)
	if _, ok := s.clusters.Cached(s.env.ID); ok {
		t.Fatal("the cluster was read before the test began")
	}
	rec := s.del(t, "ovl", "")
	if rec.code != http.StatusConflict || rec.ptyp != "network_in_use" {
		t.Fatalf("%d %q, want 409 network_in_use: %s", rec.code, rec.ptyp, rec.body)
	}
	for _, name := range []string{"web", "idle"} {
		if !strings.Contains(string(rec.body), name) {
			t.Errorf("the detail does not name service %s: %s", name, rec.body)
		}
	}
	s.noDeletes(t)
}

func TestDeleteNetworkAnOverlayNothingMentionsIsRemovedOnceThroughAManager(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.post(t, `{"name":"spare","driver":"overlay"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.code, rec.body)
	}
	id := decode[networkCreated](t, rec).ID
	rec = s.del(t, id, "")
	if rec.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.code, rec.body)
	}
	got := s.deletesOn()
	if len(got["m1"])+len(got["m2"])+len(got["w"]) != 1 || len(got["w"]) != 0 {
		t.Fatalf("deletes = %v, want exactly one, on a manager", got)
	}
	audits := s.networkAudits(t)
	last := audits[len(audits)-1]
	if last.Action != audit.ActionNetworkRemoved || last.Details["scope"] != "swarm" || last.Details["name"] != "spare" || last.Details["code"] != "removed" {
		t.Fatalf("audit = %+v", last)
	}
	if _, has := last.Details["node"]; has {
		t.Errorf("a cluster network names no node: %+v", last.Details)
	}
}

func TestDeleteNetworkWhenANodeThatCouldHoldAnAttachmentDidNotAnswerIs409NodeUnreachable(t *testing.T) {
	s := newSwarmFix(t)
	list := decode[listBody[networkListItem]](t, s.get(t, s.env.ID, "networks"))
	var brw networkListItem
	for _, n := range list.Items {
		if n.Name == "bridge" && n.ID == "brw" {
			brw = n
		}
	}
	if brw.ID == "" {
		t.Fatalf("the worker's bridge is not listed: %+v", list.Items)
	}
	// The worker stops answering list calls after the list above was cached.
	s.w.fake.Reconfigure(dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active", PolicyDeny: true})

	for name, rec := range map[string]*envRec{
		"a swarm-wide overlay": s.del(t, "ovl", ""),
		"the worker's bridge":  s.del(t, "brw", brw.NodeID),
	} {
		if rec.code != http.StatusConflict || rec.ptyp != "node_unreachable" {
			t.Errorf("%s: %d %q, want 409 node_unreachable: %s", name, rec.code, rec.ptyp, rec.body)
		}
	}
	s.noDeletes(t)
}

func TestDeleteNetworkALocalBridgeOnASwarmNodeGoesToThatNodeOnly(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.post(t, fmt.Sprintf(`{"name":"br-w","driver":"bridge","nodeId":%q}`, s.w.id))
	if rec.code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.code, rec.body)
	}
	id := decode[networkCreated](t, rec).ID
	if rec := s.del(t, id, s.w.id); rec.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.code, rec.body)
	}
	got := s.deletesOn()
	if len(got["w"]) != 1 || len(got["m1"])+len(got["m2"]) != 0 {
		t.Fatalf("deletes = %v, want one, on the worker", got)
	}
}

// Network detail (plan 04-14, RES-06, D-42): what is attached to a network, with the
// addresses and aliases, and its raw inspect.

type attachmentItem struct {
	Kind    string   `json:"kind"`
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Node    string   `json:"node"`
	NodeID  string   `json:"nodeId"`
	State   string   `json:"state"`
	IPv4    string   `json:"ipv4"`
	IPv6    string   `json:"ipv6"`
	MAC     string   `json:"mac"`
	Aliases []string `json:"aliases"`
	Tasks   int      `json:"tasks"`
}

type ipamItem struct {
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
	IPRange string `json:"ipRange"`
}

type networkDetailItem struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Driver             string            `json:"driver"`
	Scope              string            `json:"scope"`
	Node               string            `json:"node"`
	NodeID             string            `json:"nodeId"`
	Created            string            `json:"created"`
	Internal           bool              `json:"internal"`
	Attachable         bool              `json:"attachable"`
	IPv6               bool              `json:"ipv6"`
	BuiltIn            bool              `json:"builtIn"`
	Owner              string            `json:"owner"`
	OwnerKind          string            `json:"ownerKind"`
	Labels             map[string]string `json:"labels"`
	Options            map[string]string `json:"options"`
	IPAM               []ipamItem        `json:"ipam"`
	Attachments        []attachmentItem  `json:"attachments"`
	AttachedContainers int               `json:"attachedContainers"`
	AttachedServices   int               `json:"attachedServices"`
	AliasesTruncated   bool              `json:"aliasesTruncated"`
	NotReporting       []string          `json:"notReporting"`
}

func netDetailEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "app0", Name: "app-net", Driver: "bridge", Scope: "local", Internal: true, IPv6: true,
				Subnets: []string{"10.20.0.0/24", "fd00:20::/64"}, Gateways: []string{"10.20.0.1"},
				Options: map[string]string{"com.docker.network.bridge.name": "br-app"},
				Labels:  map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "lone0", Name: "lonely", Driver: "bridge", Scope: "local"},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{
				"app-net": {NetworkID: "app0", IPv4: "10.20.0.2", IPv6: "fd00:20::2", MAC: "02:42:0a:14:00:02", Aliases: []string{"web", "api"}}}},
			{ID: "c2", Names: []string{"/db"}, State: "exited", Networks: map[string]dockertest.FakeEndpoint{
				"app-net": {NetworkID: "app0", IPv4: "10.20.0.3", MAC: "02:42:0a:14:00:03", Aliases: []string{"db"}}}},
			{ID: "c3", Names: []string{"/blog"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{
				"bridge": {NetworkID: "br0", IPv4: "172.17.0.2"}}},
		},
	}
}

func getNet(t *testing.T, f *envFix, e envBody, id string) networkDetailItem {
	t.Helper()
	rec := f.get(t, e, "networks/"+id)
	if rec.code != http.StatusOK {
		t.Fatalf("GET networks/%s: %d %s", id, rec.code, rec.body)
	}
	return decode[networkDetailItem](t, rec)
}

func TestGetNetworkAnswersTheOverviewAndTheAttachmentsSortedByNameWithAddressesAndAliases(t *testing.T) {
	f, e := dashEnv(t, netDetailEngine())
	n := getNet(t, f, e, "app0")
	if n.ID != "app0" || n.Name != "app-net" || n.Driver != "bridge" || n.Scope != "local" || !n.Internal || !n.IPv6 || n.Attachable || n.BuiltIn {
		t.Errorf("overview = %+v", n)
	}
	if n.Owner != "shop" || n.OwnerKind != "stack" || n.Labels["com.docker.stack.namespace"] != "shop" || n.Options["com.docker.network.bridge.name"] != "br-app" {
		t.Errorf("owner %q/%q, labels %v, options %v", n.Owner, n.OwnerKind, n.Labels, n.Options)
	}
	wantIPAM := []ipamItem{{Subnet: "10.20.0.0/24", Gateway: "10.20.0.1"}, {Subnet: "fd00:20::/64"}}
	if !slices.Equal(n.IPAM, wantIPAM) {
		t.Errorf("ipam = %+v, want %+v", n.IPAM, wantIPAM)
	}
	if n.AttachedContainers != 2 || n.AttachedServices != 0 || n.AliasesTruncated || len(n.Attachments) != 2 {
		t.Fatalf("counts %d/%d, truncated %v, attachments %+v", n.AttachedContainers, n.AttachedServices, n.AliasesTruncated, n.Attachments)
	}
	db, web := n.Attachments[0], n.Attachments[1]
	if db.Name != "db" || db.Kind != "container" || db.State != "exited" || db.IPv4 != "10.20.0.3" || db.MAC != "02:42:0a:14:00:03" || !slices.Equal(db.Aliases, []string{"db"}) {
		t.Errorf("db = %+v", db)
	}
	if web.Name != "web" || web.State != "running" || web.IPv4 != "10.20.0.2" || web.IPv6 != "fd00:20::2" || web.Node != "node-1" || web.NodeID == "" || !slices.Equal(web.Aliases, []string{"web", "api"}) {
		t.Errorf("web = %+v", web)
	}
}

func TestGetNetworkWithNothingAttachedHasAnEmptyListAndABridgeIsBuiltIn(t *testing.T) {
	f, e := dashEnv(t, netDetailEngine())
	lone := getNet(t, f, e, "lone0")
	if lone.Attachments == nil || len(lone.Attachments) != 0 || lone.AttachedContainers != 0 || lone.BuiltIn || lone.IPAM == nil {
		t.Errorf("lonely = %+v, want an empty list (not null) and no IPAM rows", lone)
	}
	br := getNet(t, f, e, "br0")
	if !br.BuiltIn || br.AttachedContainers != 1 || br.Attachments[0].Name != "blog" {
		t.Errorf("bridge = %+v", br)
	}
}

func TestGetNetworkAnUnknownOneIs404NetworkNotFoundAndAViewerMayRead(t *testing.T) {
	f, e := dashEnv(t, netDetailEngine())
	rec := f.get(t, e, "networks/nope")
	if rec.code != http.StatusNotFound || rec.ptyp != "network_not_found" {
		t.Fatalf("%d %q, want 404 network_not_found", rec.code, rec.ptyp)
	}
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "viewer", false)
	ok := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/networks/app0", cookie: cookie, csrf: csrf})
	if ok.Code != http.StatusOK {
		t.Fatalf("a viewer: %d %s", ok.Code, ok.Body)
	}
	if rec := f.req(t, http.MethodGet, "/api/env/nope/networks/app0", ""); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("unknown environment: %d %q", rec.code, rec.ptyp)
	}
}

func TestGetNetworkInspectsAtMost50ContainersForAliasesAndSaysTheRestWereLeftOut(t *testing.T) {
	cfg := netDetailEngine()
	cfg.Containers = nil
	for i := range 53 {
		name := fmt.Sprintf("job-%02d", i)
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
			ID: fmt.Sprintf("j%02d", i), Names: []string{"/" + name}, State: "running",
			Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0", Aliases: []string{"alias-" + name}}},
		})
	}
	f, e := dashEnv(t, cfg)
	n := getNet(t, f, e, "app0")
	if n.AttachedContainers != 53 || len(n.Attachments) != 53 || !n.AliasesTruncated {
		t.Fatalf("counts %d, attachments %d, truncated %v", n.AttachedContainers, len(n.Attachments), n.AliasesTruncated)
	}
	withAliases := 0
	for i, a := range n.Attachments {
		if a.Name != fmt.Sprintf("job-%02d", i) {
			t.Fatalf("attachment %d is %q, want them sorted by name", i, a.Name)
		}
		if i < 50 {
			if !slices.Equal(a.Aliases, []string{"alias-" + a.Name}) {
				t.Errorf("%s aliases = %v, want inspected", a.Name, a.Aliases)
			}
			withAliases++
		} else if a.Aliases != nil {
			t.Errorf("%s aliases = %v, want null past the 50th", a.Name, a.Aliases)
		}
	}
	if withAliases != 50 {
		t.Errorf("%d inspected", withAliases)
	}
	reads := 0
	for _, r := range f.fake.Requests() {
		if strings.HasSuffix(r, "/json") && strings.Contains(r, "/containers/") && !strings.Contains(r, "/containers/json") {
			reads++
		}
	}
	if reads != 50 {
		t.Errorf("%d container inspects, want exactly 50", reads)
	}
}

func TestInspectNetworkAnswersTheEnginesDocumentAndAnUnknownOneIs404(t *testing.T) {
	f, e := dashEnv(t, netDetailEngine())
	rec := f.get(t, e, "networks/app0/inspect")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	got := decode[struct {
		Inspect map[string]any `json:"inspect"`
	}](t, rec)
	if got.Inspect["Name"] != "app-net" || got.Inspect["Driver"] != "bridge" {
		t.Errorf("inspect = %v", got.Inspect)
	}
	if rec := f.get(t, e, "networks/nope/inspect"); rec.code != http.StatusNotFound || rec.ptyp != "network_not_found" {
		t.Errorf("unknown: %d %q", rec.code, rec.ptyp)
	}
}

func TestGetNetworkOnASwarmListsEveryServiceThatMentionsAnOverlayEvenAtZeroReplicasAndReadsItOnAManager(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.get(t, s.env.ID, "networks/ovl")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	n := decode[networkDetailItem](t, rec)
	if n.Scope != "swarm" || n.Node != "Cluster" || n.AttachedServices != 2 || n.AttachedContainers != 0 {
		t.Fatalf("overlay = %+v", n)
	}
	want := map[string]int{"idle": 0, "web": 1}
	if len(n.Attachments) != 2 {
		t.Fatalf("attachments = %+v", n.Attachments)
	}
	for i, a := range n.Attachments {
		if a.Kind != "service" || a.Tasks != want[a.Name] || a.Node != "" || a.Aliases != nil {
			t.Errorf("service %d = %+v", i, a)
		}
	}
	if n.Attachments[0].Name != "idle" || n.Attachments[1].Name != "web" {
		t.Errorf("order = %+v, want by name", n.Attachments)
	}
	if w := s.m1.fake.Writes(); len(w) != 0 {
		t.Errorf("a read wrote: %v", w)
	}
}

// Connecting a container to a network (plan 04-15, RES-06, D-43, D-51, P2 D-42, Pitfall 11):
// the validation, the routing to the container's node, and the audit rows.

func connectEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "app0", Name: "app-net", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24"}},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running"},
			{ID: "c2", Names: []string{"/db"}, State: "exited", Networks: map[string]dockertest.FakeEndpoint{
				"app-net": {NetworkID: "app0", IPv4: "10.20.0.3"}}},
		},
	}
}

// connectNet posts a connect as the signed-in admin, after the monitor's first probe.
func (f *envFix) connectNet(t *testing.T, e envBody, networkID, body string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks/"+networkID+"/connect", body)
}

func TestConnectContainerAsAnOperatorSendsTheEndpointOnceToTheContainersNodeAndAuditsIt(t *testing.T) {
	f, e := dashEnv(t, connectEngine())
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "operator", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/networks/app0/connect",
		body:   `{"containerId":"c1","alias":"api","ipv4":"10.20.0.5","ipv6":"fd00:20::5"}`,
		cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /networks/app0/connect"}) {
		t.Fatalf("writes = %v, want one connect", w)
	}
	got := f.fake.NetworkConnects()
	if len(got) != 1 || got[0].Container != "c1" || got[0].EndpointConfig == nil ||
		!slices.Equal(got[0].EndpointConfig.Aliases, []string{"api"}) ||
		got[0].EndpointConfig.IPAMConfig.IPv4Address.String() != "10.20.0.5" || got[0].EndpointConfig.IPAMConfig.IPv6Address.String() != "fd00:20::5" {
		t.Fatalf("the engine saw %+v", got)
	}

	rows := f.networkAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionNetworkConnected || r.Result != string(audit.ResultSuccess) || r.TargetType != "network" || r.TargetID != "app0" || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"network": "app-net", "container": "web", "node": e.ID, "alias": "api", "ipv4": "10.20.0.5", "ipv6": "fd00:20::5", "code": "connected"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}
}

func TestConnectContainerAViewerIsRefusedAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, connectEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/networks/app0/connect",
		body: `{"containerId":"c1"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("network audit rows = %+v, want none", rows)
	}
}

func TestConnectContainerRefusalsAreNamedAuditedAndNeverReachTheEngine(t *testing.T) {
	f, e := dashEnv(t, connectEngine())
	for _, c := range []struct {
		name, network, body, status string
		code                        int
	}{
		{"an IPv4 that is not one", "app0", `{"containerId":"c1","ipv4":"nope"}`, "address_invalid", 422},
		{"an IPv6 where IPv4 goes", "app0", `{"containerId":"c1","ipv4":"fd00::1"}`, "address_invalid", 422},
		{"an IPv4 where IPv6 goes", "app0", `{"containerId":"c1","ipv6":"10.0.0.1"}`, "address_invalid", 422},
		{"a container already on it", "app0", `{"containerId":"c2"}`, "already_connected", 409},
	} {
		rec := f.connectNet(t, e, c.network, c.body)
		if rec.code != c.code || rec.ptyp != c.status {
			t.Errorf("%s: %d %q, want %d %s: %s", c.name, rec.code, rec.ptyp, c.code, c.status, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refusal reached the engine: %v", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %+v, want one failure per refusal", rows)
	}
	for i, want := range []string{"address_invalid", "address_invalid", "address_invalid", "already_connected"} {
		if rows[i].Result != string(audit.ResultFailure) || rows[i].Reason != want || rows[i].Details["code"] != want {
			t.Errorf("row %d = %+v, want a failure named %s", i, rows[i], want)
		}
	}
}

func TestConnectContainerTheSchemaRefusesABadAliasAndAnEmptyContainerBeforeAnyLookup(t *testing.T) {
	f, e := dashEnv(t, connectEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for name, body := range map[string]string{
		"an alias with a space":   `{"containerId":"c1","alias":"my alias"}`,
		"an alias past 253":       fmt.Sprintf(`{"containerId":"c1","alias":%q}`, strings.Repeat("a", 254)),
		"no container":            `{"alias":"api"}`,
		"an empty container":      `{"containerId":""}`,
		"a field of its own kind": `{"containerId":"c1","links":["x"]}`,
	} {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks/app0/connect", body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body reached the engine: %v", w)
	}
}

func TestConnectContainerAnUnknownNetworkContainerOrEnvironmentIs404(t *testing.T) {
	f, e := dashEnv(t, connectEngine())
	rec := f.connectNet(t, e, "nope", `{"containerId":"c1"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "network_not_found" {
		t.Errorf("network: %d %q, want 404 network_not_found", rec.code, rec.ptyp)
	}
	rec = f.connectNet(t, e, "app0", `{"containerId":"ghost"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "container_not_found" {
		t.Errorf("container: %d %q, want 404 container_not_found", rec.code, rec.ptyp)
	}
	rec = f.req(t, http.MethodPost, "/api/env/nope/networks/app0/connect", `{"containerId":"c1"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("environment: %d %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a lookup that failed reached the engine: %v", w)
	}
}

func TestConnectContainerTheEnginesOwnAlreadyConnectedRefusalIsAlreadyConnectedWhenTheListIsStale(t *testing.T) {
	f, e := dashEnv(t, connectEngine())
	// The list does not know c1 is on app-net yet; the engine does, and says so.
	cfg := connectEngine()
	cfg.Containers[0].Networks = map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	_ = f.get(t, e, "networks") // the list is read and cached while c1 is on nothing
	f.fake.Reconfigure(cfg)
	rec := f.connectNet(t, e, "app0", `{"containerId":"c1"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "already_connected" {
		t.Fatalf("%d %q, want 409 already_connected from the engine's own refusal: %s", rec.code, rec.ptyp, rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /networks/app0/connect"}) {
		t.Fatalf("writes = %v, want the one connect the engine refused (the list was stale)", w)
	}
	rows := f.networkAudits(t)
	if last := rows[len(rows)-1]; last.Result != string(audit.ResultFailure) || last.Reason != "already_connected" {
		t.Fatalf("last audit row = %+v", last)
	}
}

func TestConnectContainerOnASwarmAnOverlayMustBeAttachableAndALocalNetworkTakesOnlyItsOwnNodesContainers(t *testing.T) {
	s := newSwarmFix(t)
	connect := func(network, container, node string) *envRec {
		return s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/networks/"+network+"/connect",
			fmt.Sprintf(`{"containerId":%q,"nodeId":%q}`, container, node))
	}
	// ovl is not attachable.
	if rec := connect("ovl", "w001", s.w.id); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "network_not_attachable" {
		t.Errorf("a non-attachable overlay: %d %q, want 422 network_not_attachable: %s", rec.code, rec.ptyp, rec.body)
	}
	// brw is the worker's bridge; m1 is a container of the first manager.
	if rec := connect("brw", "m1", s.m1.id); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "node_mismatch" {
		t.Errorf("another node's bridge: %d %q, want 422 node_mismatch: %s", rec.code, rec.ptyp, rec.body)
	}
	for name, a := range map[string]*agentConn{"m1": s.m1, "m2": s.m2, "w": s.w} {
		if w := a.fake.Writes(); len(w) != 0 {
			t.Errorf("%s: a refusal reached the engine: %v", name, w)
		}
	}
	// The worker's own bridge takes the worker's container, on the worker only.
	if rec := connect("brw", "w001", s.w.id); rec.code != http.StatusNoContent {
		t.Fatalf("the worker's bridge: %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := s.w.fake.NetworkConnects(); len(got) != 1 || got[0].Container != "w001" {
		t.Errorf("the worker saw %+v", got)
	}
	if len(s.m1.fake.Writes())+len(s.m2.fake.Writes()) != 0 {
		t.Error("the connect reached a manager")
	}
}

func TestConnectContainerAWorkerThatHasNotLearnedTheOverlayAnswersNetworkUnavailableOnNodeWithTheEnginesWords(t *testing.T) {
	s := newSwarmFix(t)
	made := s.post(t, `{"name":"new-ovl","driver":"overlay","attachable":true}`)
	if made.code != http.StatusCreated {
		t.Fatalf("create: %d %s", made.code, made.body)
	}
	ovl := decode[networkCreated](t, made)
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/networks/"+ovl.ID+"/connect",
		fmt.Sprintf(`{"containerId":"w001","nodeId":%q}`, s.w.id))
	if rec.code != http.StatusConflict || rec.ptyp != "network_unavailable_on_node" {
		t.Fatalf("%d %q, want 409 network_unavailable_on_node: %s", rec.code, rec.ptyp, rec.body)
	}
	if !strings.Contains(string(rec.body), "not found") {
		t.Errorf("the problem does not carry the engine's words: %s", rec.body)
	}
	rows := s.networkAudits(t)
	if last := rows[len(rows)-1]; last.Action != audit.ActionNetworkConnected || last.Result != string(audit.ResultFailure) || last.Reason != "network_unavailable_on_node" {
		t.Errorf("last audit row = %+v", last)
	}
	// Nothing was retried on another node.
	if got := len(s.w.fake.Writes()); got != 1 {
		t.Errorf("the worker served %d writes, want the one connect", got)
	}
}
