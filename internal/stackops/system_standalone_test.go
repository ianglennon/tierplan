// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/dockerx"
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
