// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The ingress preview and enable routes (Phase 24, DEP-01, D-06). The swarm is a fake daemon; the deployer is the real one
// over the fixture's store, audit writer and operation lock.

const deployServerVersion = "1.2.3"

// deployEngine runs every call against the one fake swarm manager.
type deployEngine struct{ cli *dockerx.Client }

func (e deployEngine) WriteOnManager(_ context.Context, _ string, fn func(*dockerx.Client) error) error {
	return fn(e.cli)
}

func (e deployEngine) WriteOnNode(_ context.Context, _, _ string, fn func(*dockerx.Client) error) error {
	return fn(e.cli)
}

func (e deployEngine) ClusterRead(_ context.Context, _ string, fn func(*dockerx.Client) error) error {
	return fn(e.cli)
}

func (e deployEngine) OnNode(_ context.Context, _, _ string, fn func(*dockerx.Client) error) error {
	return fn(e.cli)
}

// deployEnvs says the environment is a swarm with a connected manager, or a standalone host.
type deployEnvs struct{ standalone bool }

func (d deployEnvs) Info(_ context.Context, envID string) (ingressdeploy.EnvInfo, error) {
	return ingressdeploy.EnvInfo{
		ID: envID, Name: "prod", Swarm: !d.standalone, ClusterID: "cluster-1", ManagerConnected: true,
		Nodes: []ingressdeploy.NodeInfo{{SwarmNodeID: "node-1", AgentID: "agent-1", Name: "manager-1", Role: "manager", Connected: true}},
	}, nil
}

// deployAddressing hands out blocks of 10.0.0.0/8 and counts the networks of the fake swarm as occupied.
type deployAddressing struct {
	cli *dockerx.Client
	mu  sync.Mutex
}

func (a *deployAddressing) LockAllocation(string) func() {
	a.mu.Lock()
	return a.mu.Unlock
}

func (a *deployAddressing) AllocationInputs(ctx context.Context, _ string, _ bool) (envs.AllocationInputs, error) {
	rows, err := a.cli.Networks(ctx)
	if err != nil {
		return envs.AllocationInputs{}, err
	}
	in := envs.AllocationInputs{Pools: []netspace.Pool{{Base: netip.MustParsePrefix("10.0.0.0/8"), Size: 24}}}
	for _, r := range rows {
		for _, s := range r.Subnets {
			if p, err := netip.ParsePrefix(s); err == nil {
				in.Occupied = append(in.Occupied, p)
			}
		}
	}
	return in, nil
}

// deployFix is the ingress fixture with a deployer over a fake swarm behind its routes.
type deployFix struct {
	*ingressFix
	fake *dockertest.Fake
	cli  *dockerx.Client
	hub  *jobs.Hub
}

// newDeployFix builds the fixture and the routes over one fake swarm whose ingress network holds 10.0.0.0/24.
func newDeployFix(t *testing.T, standalone bool) *deployFix {
	t.Helper()
	return newDeployFixWith(t, standalone, nil)
}

// newDeployFixWith is newDeployFix with the fake swarm adjusted by config, for a test that needs something running on it.
func newDeployFixWith(t *testing.T, standalone bool, config func(*dockertest.FakeConfig)) *deployFix {
	t.Helper()
	return newDeployFixDeps(t, standalone, config, nil)
}

// newDeployFixDeps is newDeployFixWith with the deployer's dependencies adjusted by deps, which sees the fixture.
func newDeployFixDeps(t *testing.T, standalone bool, config func(*dockertest.FakeConfig), deps func(*deployFix, *ingressdeploy.Deps)) *deployFix {
	t.Helper()
	f := newIngressFix(t)
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1",
		Networks: []dockertest.FakeNetwork{
			{ID: "ingress-net", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
		},
	}
	if config != nil {
		config(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	cli, err := dockerx.New(fake)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	hub := jobs.New(jobs.Options{})
	t.Cleanup(hub.Close)
	f.stackLocks = stackops.NewLocks()
	f.ingressDeployJobs = hub
	df := &deployFix{ingressFix: f, fake: fake, cli: cli, hub: hub}
	d := ingressdeploy.Deps{
		Engine: deployEngine{cli: cli}, Envs: deployEnvs{standalone: standalone}, Store: f.ingressStore, Tokens: f.ingressNodes, NodeRows: f.ingressNodes, Nodes: f.ingressNodes,
		Jobs: hub, Locks: f.stackLocks, Audit: f.aud, AddressSpaces: &deployAddressing{cli: cli},
		AllocationWait: dockerx.AllocationWait{After: func(time.Duration) <-chan time.Time {
			c := make(chan time.Time, 1)
			c <- time.Time{}
			return c
		}},
		ServerVersion: deployServerVersion, URL: f.inst.IngressEndpoint, Pins: func() []string { return []string{"sha256/AAAA"} },
		Clock: fixtureClock{f.clk},
	}
	if deps != nil {
		deps(df, &d)
	}
	f.ingressDeploy = ingressdeploy.New(d)
	f.h = f.build(t)
	return df
}

// problemOf decodes the type and detail of a problem answer.
func deployProblem(t *testing.T, body []byte) (typ, detail string) {
	t.Helper()
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	return p.Type, p.Detail
}

type previewResp struct {
	Findings []struct {
		Code, Severity string
	} `json:"findings"`
	Creates struct {
		Network struct{ Name, Subnet string } `json:"network"`
		Service struct{ Name, Image string }  `json:"service"`
		Secret  struct {
			Name string
			New  bool
		} `json:"secret"`
		Volume struct{ Name string } `json:"volume"`
	} `json:"creates"`
	Redeploy []struct{ StackID, StackName string } `json:"redeploy"`
}

type jobResp struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Items []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Code  string `json:"code"`
	} `json:"items"`
}

// TestIngressEnableRoute is the tracer: an Admin previews what an enable would create (and nothing is written), switches
// ingress on and follows the job to done, with the objects on the fake swarm and the proxy dialling the configured address.
func TestIngressEnableRoute(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	base := "/api/env/" + f.envA + "/ingress"

	rec := f.credCall(t, f.adminA, http.MethodPost, base+"/enable/preview", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("preview Cache-Control = %q, want no-store", cc)
	}
	pv := decodeBody[previewResp](t, rec)
	if pv.Creates.Network.Name != ingress.NetworkName || pv.Creates.Network.Subnet != "10.0.4.0/22" ||
		pv.Creates.Service.Name != ingress.ServiceName || pv.Creates.Service.Image != ingressdeploy.IngressImage(deployServerVersion) ||
		!pv.Creates.Secret.New || pv.Creates.Volume.Name != ingress.VolumeName {
		t.Errorf("preview creates = %+v", pv.Creates)
	}
	for _, fd := range pv.Findings {
		if fd.Severity == "error" {
			t.Errorf("preview finds a refusal: %+v", fd)
		}
	}
	if n := len(f.fake.ServiceCreates()) + len(f.fake.SecretCreates()); n != 0 {
		t.Fatalf("the preview wrote %d objects", n)
	}
	if rows, err := f.cli.Networks(testsupport.Context(t)); err != nil || len(rows) != 1 {
		t.Fatalf("networks after the preview = %v, %v; want only the swarm's own", rows, err)
	}

	rec = f.credCall(t, f.adminA, http.MethodPost, base+"/enable", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if start.JobID == "" {
		t.Fatalf("enable answered no job: %s", rec.Body)
	}
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the job: %v", err)
	}

	rec = f.credCall(t, f.adminA, http.MethodGet, base+"/jobs/"+start.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read the job: %d %s", rec.Code, rec.Body)
	}
	job := decodeBody[jobResp](t, rec)
	if job.Kind != ingressdeploy.JobKindEnable || job.State != jobs.JobDone || len(job.Items) != 4 {
		t.Fatalf("job = %+v, want a done ingress_enable with four items", job)
	}
	for _, it := range job.Items {
		if it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", it.ID, it.State, it.Code)
		}
	}

	// The objects are on the fake swarm.
	if secrets, err := f.cli.SecretList(testsupport.Context(t)); err != nil || len(secrets) != 1 {
		t.Errorf("secrets = %v, %v; want one", secrets, err)
	}
	if _, _, err := f.cli.ServiceInspect(testsupport.Context(t), ingress.ServiceName); err != nil {
		t.Errorf("the proxy service is not on the swarm: %v", err)
	}
	nets, _ := f.cli.Networks(testsupport.Context(t))
	found := false
	for _, n := range nets {
		found = found || n.Name == ingress.NetworkName
	}
	if !found {
		t.Error("the ingress network is not on the swarm")
	}
	// The proxy dials the address the Admin configured, with the pin list the server holds.
	creates := f.fake.ServiceCreates()
	if len(creates) != 1 {
		t.Fatalf("service creates = %d, want 1", len(creates))
	}
	env := map[string]string{}
	for _, kv := range creates[0].Spec.TaskTemplate.ContainerSpec.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env[ingressbundle.EnvURL] != "wss://example.com/tierplan/ingress/connect" || env[ingressbundle.EnvPin] != "sha256/AAAA" {
		t.Errorf("proxy env url %q pin %q", env[ingressbundle.EnvURL], env[ingressbundle.EnvPin])
	}

	// A job is read only through the environment it ran on: another environment's route finds none.
	rec = f.credCall(t, f.admin, http.MethodGet, "/api/env/"+f.envB+"/ingress/jobs/"+start.JobID, "")
	if typ, _ := deployProblem(t, rec.Body.Bytes()); rec.Code != http.StatusNotFound || typ != "ingress_job_not_found" {
		t.Errorf("the job through another environment: %d %s", rec.Code, rec.Body)
	}

	// Its stream of an ended job is the job as it stands and the done frame.
	rec = f.credCall(t, f.adminA, http.MethodGet, base+"/jobs/"+start.JobID+"/stream", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: done") {
		t.Errorf("stream: %d %s", rec.Code, rec.Body)
	}
}

// TestIngressEnableNeedsManage: an Operator may neither preview nor enable, and nothing runs (T-24-22).
func TestIngressEnableNeedsManage(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	base := "/api/env/" + f.envA + "/ingress"
	for _, path := range []string{base + "/enable/preview", base + "/enable"} {
		if rec := f.credCall(t, f.operatorA, http.MethodPost, path, `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("Operator POST %s: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	// An Admin of the other environment is a stranger here.
	if rec := f.credCall(t, f.adminB, http.MethodPost, base+"/enable", `{}`); rec.Code == http.StatusAccepted {
		t.Errorf("an Admin of another environment enabled ingress: %s", rec.Body)
	}
	if n := len(f.fake.ServiceCreates()) + len(f.fake.SecretCreates()); n != 0 {
		t.Errorf("a refused caller caused %d writes", n)
	}
	if list := f.hub.List(func(jobs.Job) bool { return true }); len(list) != 0 {
		t.Errorf("a refused caller started %d jobs", len(list))
	}
}

// TestIngressEnableStandalone409: a standalone environment answers 409 ingress_standalone_unavailable on both routes (D-34).
func TestIngressEnableStandalone409(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, true)
	base := "/api/env/" + f.envA + "/ingress"
	for _, path := range []string{base + "/enable/preview", base + "/enable"} {
		rec := f.credCall(t, f.adminA, http.MethodPost, path, `{}`)
		typ, detail := deployProblem(t, rec.Body.Bytes())
		if rec.Code != http.StatusConflict || typ != "ingress_standalone_unavailable" || !strings.Contains(detail, "not yet available") {
			t.Errorf("POST %s: %d type %q detail %q", path, rec.Code, typ, detail)
		}
	}
	if n := len(f.fake.ServiceCreates()) + len(f.fake.SecretCreates()); n != 0 {
		t.Errorf("a standalone refusal caused %d writes", n)
	}
}

// TestIngressEnableRefusedPreflight409: a swarm where a service publishes port 80 is refused with 409
// ingress_preflight_refused on the enable route, listing each refusal by its code with the services in the way as the
// value; the preview of the same swarm answers 200 with the finding, and neither writes nor starts a job (D-07).
func TestIngressEnableRefusedPreflight409(t *testing.T) {
	t.Parallel()
	f := newDeployFixWith(t, false, func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{{
			ID: "web", Name: "shop_web", Mode: "replicated", Desired: 1,
			Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 8080, PublishedPort: 80, PublishMode: "ingress"}},
		}}
	})
	base := "/api/env/" + f.envA + "/ingress"

	rec := f.credCall(t, f.adminA, http.MethodPost, base+"/enable/preview", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	pv := decodeBody[previewResp](t, rec)
	if len(pv.Findings) != 1 || pv.Findings[0].Code != ingressdeploy.CodePortInUse || pv.Findings[0].Severity != "error" {
		t.Errorf("preview findings = %+v, want ingress_port_in_use as an error", pv.Findings)
	}

	rec = f.credCall(t, f.adminA, http.MethodPost, base+"/enable", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("enable: %d %s, want 409", rec.Code, rec.Body)
	}
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
		Errors []struct {
			Location string   `json:"location"`
			Message  string   `json:"message"`
			Value    []string `json:"value"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if p.Type != "ingress_preflight_refused" || !strings.Contains(p.Detail, "Ports 80 and 443 must be free") {
		t.Errorf("type %q detail %q", p.Type, p.Detail)
	}
	if len(p.Errors) != 1 || p.Errors[0].Location != ingressdeploy.CodePortInUse ||
		len(p.Errors[0].Value) != 1 || p.Errors[0].Value[0] != "shop_web publishes 80 (ingress mode)" {
		t.Errorf("errors = %+v, want ingress_port_in_use naming the service", p.Errors)
	}
	if n := len(f.fake.ServiceCreates()) + len(f.fake.SecretCreates()) + len(f.fake.NetworkCreates()); n != 0 {
		t.Errorf("a refused enable wrote %d objects", n)
	}
	if list := f.hub.List(func(jobs.Job) bool { return true }); len(list) != 0 {
		t.Errorf("a refused enable started %d jobs", len(list))
	}
	if _, held := f.stackLocks.Held(f.envA); held {
		t.Error("a refused enable left the environment's lock held")
	}
}

// TestIngressEnableLocked409: an enable asked for while another operation holds the environment is refused with 409
// operation_locked, naming who holds it, and is not queued; the preview, which takes no lock, still answers (D-11).
func TestIngressEnableLocked409(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		holder stackops.Holder
		detail string
		errors []string
	}{
		{
			name: "a stack operation",
			holder: stackops.Holder{StackID: "s1", StackName: "shop", JobID: "job-1", By: "ada",
				Started: time.Date(2026, 1, 1, 8, 30, 0, 0, time.UTC)},
			detail: "Another stack operation is running on ",
			errors: []string{"stack=shop", "job=job-1", "started=2026-01-01T08:30:00Z", "stackId=s1"},
		},
		{
			name: "an ingress job",
			holder: stackops.Holder{Kind: stackops.HolderKindIngress, JobID: "job-2", Workload: "ingress", By: "ada",
				Started: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)},
			detail: "TierPlan is changing ingress on ",
			errors: []string{"kind=ingress", "job=job-2", "started=2026-01-01T09:00:00Z"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDeployFix(t, false)
			base := "/api/env/" + f.envA + "/ingress"
			release, _, ok := f.stackLocks.TryAcquire(f.envA, tc.holder)
			if !ok {
				t.Fatal("the lock was not free")
			}
			t.Cleanup(release)

			rec := f.credCall(t, f.adminA, http.MethodPost, base+"/enable", `{}`)
			var p struct {
				Type   string                               `json:"type"`
				Detail string                               `json:"detail"`
				Errors []struct{ Location, Message string } `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("decode %s: %v", rec.Body, err)
			}
			if rec.Code != http.StatusConflict || p.Type != "operation_locked" || !strings.HasPrefix(p.Detail, tc.detail) ||
				!strings.HasSuffix(p.Detail, "Try again when it finishes.") {
				t.Fatalf("enable: %d type %q detail %q, want 409 operation_locked: %s", rec.Code, p.Type, p.Detail, tc.detail)
			}
			var got []string
			for _, e := range p.Errors {
				got = append(got, e.Location+"="+e.Message)
			}
			for _, want := range tc.errors {
				if !slices.Contains(got, want) {
					t.Errorf("errors = %v, want %s", got, want)
				}
			}
			if n := len(f.fake.ServiceCreates()) + len(f.fake.SecretCreates()) + len(f.fake.NetworkCreates()); n != 0 {
				t.Errorf("a locked enable wrote %d objects", n)
			}
			if list := f.hub.List(func(jobs.Job) bool { return true }); len(list) != 0 {
				t.Errorf("a locked enable started %d jobs", len(list))
			}
			if rec := f.credCall(t, f.adminA, http.MethodPost, base+"/enable/preview", `{}`); rec.Code != http.StatusOK {
				t.Errorf("preview under the lock: %d %s, want 200", rec.Code, rec.Body)
			}
		})
	}
}
