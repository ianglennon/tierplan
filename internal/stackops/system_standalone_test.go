// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The system project is TierPlan's own: the ingress proxy of a standalone host. It goes through the tail of the
// standalone pipeline and leaves no stack state behind (D-29). The Compose SDK is replaced by the stand-in the
// standalone tests use.

const systemImage = "ghcr.io/example/tierplan-ingress@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// systemToken is the value of the join token the project carries in its environment.
const systemToken = "join-token-value-9f3b7c1e5a"

// systemProject is a small typed project named as the proxy's: one service, a bridge network and a volume, each with the
// labels every object of the ingress carries.
func systemProject() *types.Project {
	return &types.Project{
		Name: ingress.ProjectName,
		Services: types.Services{ingress.ProxyComposeService: {
			Name: ingress.ProxyComposeService, Image: systemImage, Labels: types.Labels(ingress.ObjectLabels()),
			Networks: map[string]*types.ServiceNetworkConfig{"ingress": nil},
			Volumes:  []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "cache", Target: "/var/lib/tierplan-ingress"}},
		}},
		Networks: types.Networks{"ingress": {Name: ingress.NetworkName, Driver: "bridge", Labels: types.Labels(ingress.ObjectLabels())}},
		Volumes:  types.Volumes{"cache": {Name: ingress.VolumeName, Labels: types.Labels(ingress.ObjectLabels())}},
		Environment: types.Mapping{
			"TIERPLAN_INGRESS_TOKEN": systemToken,
		},
	}
}

// deploySystem runs DeploySystem as a job and returns its result and the job as it ended.
func (r *swarmRig) deploySystem(t *testing.T, in stackops.SystemDeployInput) (stackops.DeployResult, jobs.Job) {
	t.Helper()
	in.EnvID = r.envID
	if in.NodeID == "" {
		in.NodeID = r.envID
	}
	if in.Project == nil {
		in.Project = systemProject()
	}
	var res stackops.DeployResult
	done := make(chan struct{})
	job, err := r.hub.Start(jobs.Spec{
		Kind: "ingress_enable", EnvID: r.envID, Owner: "user:test", Items: stackops.SystemItemSpecs(),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		defer close(done)
		res = r.deps.DeploySystem(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := r.hub.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the system deploy to return", done)
	return res, ended
}

func TestDeploySystemDeploysTheTypedProject(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	up := &upStandIn{}
	rig.deps.Up = up.up

	res, job := rig.deploySystem(t, stackops.SystemDeployInput{NoWait: true})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result %+v, job %s %v", res, job.State, itemStates(job))
	}
	if up.calls != 1 || !up.opts.NoWait {
		t.Errorf("the up ran %d times with NoWait %v, want once with NoWait", up.calls, up.opts.NoWait)
	}
	if !res.Wrote {
		t.Error("the result does not say the host was written")
	}
	var order []string
	for _, it := range job.Items {
		if it.Parent == "" {
			order = append(order, it.ID)
			if it.State != jobs.ItemDone {
				t.Errorf("item %s ended %s, want done", it.ID, it.State)
			}
		}
	}
	if got, want := strings.Join(order, ","), "check,lookup,networks,volumes,services,cleanup"; got != want {
		t.Errorf("items in order = %s, want %s", got, want)
	}

	// What the SDK was handed carries no stack's identity, and the labels the loader would have set.
	p := up.project
	if p == nil {
		t.Fatal("the SDK was never called")
	}
	svc := p.Services[ingress.ProxyComposeService]
	if _, has := svc.CustomLabels[stacks.LabelStackID]; has {
		t.Error("the service carries a stack ID label")
	}
	for kind, labels := range map[string]types.Labels{
		"service labels": svc.Labels, "network labels": p.Networks["ingress"].Labels, "volume labels": p.Volumes["cache"].Labels,
	} {
		if _, has := labels[stacks.LabelStackID]; has {
			t.Errorf("the project's %s carry a stack ID", kind)
		}
	}
	if svc.CustomLabels[stacks.LabelStackImage] != systemImage {
		t.Errorf("image label = %q, want the image as written", svc.CustomLabels[stacks.LabelStackImage])
	}
	if svc.CustomLabels[stacks.LabelComposeProject] != ingress.ProjectName || svc.CustomLabels[stacks.LabelComposeService] != ingress.ProxyComposeService {
		t.Errorf("compose labels = %v", svc.CustomLabels)
	}

	// Nothing of a stack is stored.
	stored, err := subnetRepo(t, rig).Stacks(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("%d stacks stored, want none", len(stored))
	}
}

// downStandIn plays the SDK's down and records what it was asked.
type downStandIn struct {
	mu       sync.Mutex
	projects []string
	clients  []*dockerx.Client
}

func (d *downStandIn) down(_ context.Context, c *dockerx.Client, project string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.projects, d.clients = append(d.projects, project), append(d.clients, c)
	return nil
}

func TestDownSystemRunsDownOnTheNode(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &downStandIn{}
	rig.deps.DownProject = stand.down

	if err := rig.deps.DownSystem(testsupport.Context(t), rig.envID, rig.envID); err != nil {
		t.Fatal(err)
	}

	if len(stand.projects) != 1 || stand.projects[0] != ingress.ProjectName {
		t.Fatalf("down ran for %v, want only %s", stand.projects, ingress.ProjectName)
	}
	if stand.clients[0] == nil {
		t.Error("down was not handed the node's client")
	}
}

// systemRig is a standalone host whose daemon hands networks out of 10.50.0.0/16 in /24 blocks, whose default bridge holds
// 10.50.0.0/24, and that holds what tune adds.
func systemRig(t *testing.T, tune func(*dockertest.FakeConfig)) *swarmRig {
	t.Helper()
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.DefaultAddressPools = []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 24}}
		c.Networks = []dockertest.FakeNetwork{{ID: "br-0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.0/24"}}}
		if tune != nil {
			tune(c)
		}
	})
}

// sentSubnet is the subnet the project the SDK was handed gives the network, "" for none.
func sentSubnet(t *testing.T, up *upStandIn) string {
	t.Helper()
	if up.project == nil {
		t.Fatal("the SDK was never called")
	}
	cfg := up.project.Networks["ingress"].Ipam.Config
	if len(cfg) == 0 || cfg[0] == nil {
		return ""
	}
	return cfg[0].Subnet
}

// ownedIngress are the labels of an object TierPlan made for the proxy's project.
func ownedIngress() map[string]string {
	labels := ingress.ObjectLabels()
	labels[stacks.LabelComposeProject] = ingress.ProjectName
	return labels
}

func TestDeploySystemReusesTheLiveSubnet(t *testing.T) {
	t.Parallel()
	rig := systemRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{
			ID: "ing-1", Name: ingress.NetworkName, Driver: "bridge", Scope: "local",
			Subnets: []string{"10.77.0.0/24"}, Labels: ownedIngress(),
		})
	})
	up := &upStandIn{}
	rig.deps.Up = up.up

	res, job := rig.deploySystem(t, stackops.SystemDeployInput{Network: "ingress", Prefix: 24})

	if !res.OK() {
		t.Fatalf("result %+v %v", res, itemStates(job))
	}
	if got := sentSubnet(t, up); got != "10.77.0.0/24" {
		t.Errorf("the SDK was handed subnet %q, want the live network's 10.77.0.0/24 and not the lowest free block", got)
	}
}

func TestDeploySystemHoldsTheAllocationLockUntilUp(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		requested string
		want      string
	}{
		"the lowest free block": {"", "10.50.1.0/24"},
		"a requested block":     {"10.60.4.0/24", "10.60.4.0/24"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := systemRig(t, nil)
			var mu sync.Mutex
			var order []string
			note := func(s string) {
				mu.Lock()
				defer mu.Unlock()
				order = append(order, s)
			}
			// The second allocation starts from the hook, where the deploy holds the lock and has not yet called the SDK. It
			// can only take the lock once the deploy lets go, and the stand-in for the SDK writes down that it returned
			// before then.
			contender := make(chan struct{})
			contended := make(chan struct{})
			rig.deps = stackops.SetSubnetPlanHook(rig.deps, func() {
				go func() {
					defer close(contended)
					close(contender)
					defer rig.addr.LockAllocation(rig.envID)()
					note("second allocation")
				}()
				<-contender
			})
			up := &upStandIn{}
			rig.deps.Up = func(ctx context.Context, c *dockerx.Client, p *types.Project, o dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error {
				// Give the second allocation every chance to run before the SDK returns: it must still be waiting.
				for range 200 {
					runtime.Gosched()
				}
				note("up returned")
				return up.up(ctx, c, p, o, on)
			}

			res, job := rig.deploySystem(t, stackops.SystemDeployInput{Network: "ingress", Prefix: 24, Subnet: tc.requested})
			testsupport.Await(t, "the second allocation to take the lock", contended)

			if !res.OK() {
				t.Fatalf("result %+v %v", res, itemStates(job))
			}
			if got := sentSubnet(t, up); got != tc.want {
				t.Errorf("the SDK was handed subnet %q, want %q", got, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if got := strings.Join(order, ", "); got != "up returned, second allocation" {
				t.Errorf("order = %q, want the second allocation to wait for the SDK's up", got)
			}
		})
	}
}

func TestDeploySystemRefusesAForeignObject(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]func(*dockertest.FakeConfig){
		"a network of the reserved name without the component label": func(c *dockertest.FakeConfig) {
			c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "n1", Name: ingress.NetworkName, Driver: "bridge", Scope: "local", Subnets: []string{"10.99.0.0/24"}})
		},
		"a container of the project without the component label": func(c *dockertest.FakeConfig) {
			c.Containers = append(c.Containers, dockertest.FakeContainer{
				ID: "c1", Names: []string{"/" + ingress.ProjectName + "-proxy-1"}, Image: "other", State: "running",
				Labels: map[string]string{stacks.LabelComposeProject: ingress.ProjectName, stacks.LabelComposeService: ingress.ProxyComposeService},
			})
		},
		"a volume of the reserved name without the component label": func(c *dockertest.FakeConfig) {
			c.Volumes = append(c.Volumes, dockertest.FakeVolume{Name: ingress.VolumeName, Driver: "local"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := systemRig(t, tc)
			up := &upStandIn{}
			rig.deps.Up = up.up

			res, job := rig.deploySystem(t, stackops.SystemDeployInput{Network: "ingress", Prefix: 24})

			if res.OK() || res.Code != stackops.CodeNameClash {
				t.Fatalf("result %+v, want the name clash", res)
			}
			if it := itemByID(t, job, stackops.ItemCheck); it.State != jobs.ItemFailed || it.Code != stackops.CodeNameClash {
				t.Errorf("check = %s %s, want failed with the name clash", it.State, it.Code)
			}
			if up.calls != 0 || res.Wrote {
				t.Errorf("the SDK ran %d times and wrote %v, want nothing written", up.calls, res.Wrote)
			}
		})
	}
}

func TestDeploySystemNeverShowsTheToken(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	up := &upStandIn{err: errors.New("create container: invalid environment TIERPLAN_INGRESS_TOKEN=" + systemToken + " in the request")}
	rig.deps.Up = up.up

	res, job := rig.deploySystem(t, stackops.SystemDeployInput{})

	if res.OK() {
		t.Fatalf("the deploy worked although the up failed: %+v", res)
	}
	shown := false
	for _, it := range job.Items {
		for _, text := range []string{it.Label, it.Message, it.Detail} {
			if strings.Contains(text, systemToken) {
				t.Errorf("item %s shows the token: %q", it.ID, text)
			}
			shown = shown || strings.Contains(text, stacks.Hidden)
		}
	}
	lines, _ := rig.hub.Output(job.ID)
	for _, l := range lines {
		if strings.Contains(l.Text, systemToken) {
			t.Errorf("an output line shows the token: %q", l.Text)
		}
		shown = shown || strings.Contains(l.Text, stacks.Hidden)
	}
	if !shown {
		t.Error("the engine's words were dropped instead of masked: nothing in the job says what the engine answered")
	}
}
