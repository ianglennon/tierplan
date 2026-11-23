// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"crypto/sha256"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// testAgentID is the ID the standalone host's one engine is routed by.
const testAgentID = "agent-sa"

// hostFixture is what the fake standalone host holds of the ingress proxy's objects.
type hostFixture struct {
	// Proxy is the proxy container, nil for none.
	Proxy *proxyFixture
	// Network and Volume are the owned bridge network (with its subnet) and the cache volume; an empty Network.Subnet
	// leaves the network out.
	NetworkSubnet string
	Volume        bool
}

// proxyFixture is one proxy container of the project.
type proxyFixture struct {
	Image string
	// State is the engine's state word; "" is running.
	State     string
	AccessLog bool
	// TokenLabel is the ID of the join token the container was created with, "" for none.
	TokenLabel string
	// URL and Pin are the server address and pin the container was created with; "" is what the rig's deployer wants.
	URL, Pin string
}

// The address and pin the rig's deployer wants of a proxy (newRig's Deps).
const (
	rigIngressURL = "wss://tierplan.example:7480/ingress/connect"
	rigIngressPin = "sha256/AAAA,sha256/BBBB"
)

// fakeSystem is the standalone deployer under test: it records every deploy and every down, answers the result a
// test sets, finishes the items of the job as the real one does, and plays the host the SDK would have left.
type fakeSystem struct {
	mu sync.Mutex
	// result is what DeploySystem answers; its Code, when set, fails the deploy.
	result stackops.DeployResult
	// deploys and downs are every call, in order. tokens are the clear text each deploy was given, "" for none.
	deploys []stackops.SystemDeployInput
	tokens  []string
	downs   []string
	// gate, when set, holds the first deploy until the test lets it go.
	gate *gate

	rig  *rig
	base dockertest.FakeConfig
	host hostFixture
	// nodes lists the ingress nodes through the rig's token service, set once the rig is built.
	nodes ingressdeploy.NodeLister
}

var _ ingressdeploy.SystemDeployer = (*fakeSystem)(nil)

// DeploySystem records the call and, when it is to succeed, leaves the host holding what the project asks for.
func (f *fakeSystem) DeploySystem(ctx context.Context, in stackops.SystemDeployInput, r *jobs.Reporter) stackops.DeployResult {
	if g := f.gate; g != nil {
		g.once.Do(func() {
			close(g.entered)
			select {
			case <-g.proceed:
			case <-ctx.Done():
			}
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys = append(f.deploys, in)
	tok := ""
	if in.Project != nil {
		tok = in.Project.Environment["TIERPLAN_SECRET_token"]
	}
	f.tokens = append(f.tokens, tok)
	specs := stackops.SystemItemSpecs()
	if f.result.Code != "" {
		for i, sp := range specs {
			if i == 0 {
				r.Finish(sp.ID, jobs.ItemFailed, f.result.Code, "", nil)
				continue
			}
			r.Finish(sp.ID, jobs.ItemSkipped, "not_run", "", nil)
		}
		return f.result
	}
	f.play(in)
	for _, sp := range specs {
		r.Finish(sp.ID, jobs.ItemDone, "", "", nil)
	}
	return f.result
}

// play leaves the host as the SDK would: the proxy running with the labels and the variables of the project, the owned
// network with its subnet (chosen as the real deploy does when the project has none) and the cache volume.
func (f *fakeSystem) play(in stackops.SystemDeployInput) {
	svc := in.Project.Services[ingress.ProxyComposeService]
	var env []string
	for k, v := range svc.Environment {
		if v != nil {
			env = append(env, k+"="+*v)
		}
	}
	sort.Strings(env)
	n := in.Project.Networks[in.Network]
	if len(n.Ipam.Config) == 0 || n.Ipam.Config[0] == nil || n.Ipam.Config[0].Subnet == "" {
		n.Ipam.Config = nil
		sub := in.Subnet
		if sub == "" {
			sub = "10.0.8.0/24"
		}
		n.Ipam.Config = append(n.Ipam.Config, &types.IPAMPool{Subnet: sub})
		in.Project.Networks[in.Network] = n
	}
	state := hostFixture{
		Proxy: &proxyFixture{
			Image: svc.Image, TokenLabel: svc.CustomLabels[ingressdeploy.TokenLabel], AccessLog: hasEnv(env, ingressbundle.EnvAccessLog+"=1"),
			URL: envValue(env, ingressbundle.EnvURL), Pin: envValue(env, ingressbundle.EnvPin),
		},
		NetworkSubnet: n.Ipam.Config[0].Subnet, Volume: true,
	}
	f.host = state
	f.reconfigure()
}

// DownSystem records the call and removes the container and the network; the volume stays.
func (f *fakeSystem) DownSystem(_ context.Context, envID, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downs = append(f.downs, envID+"/"+nodeID)
	f.host.Proxy, f.host.NetworkSubnet = nil, ""
	f.reconfigure()
	return nil
}

// setHost replaces what the fake host holds.
func (f *fakeSystem) setHost(h hostFixture) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.host = h
	f.reconfigure()
}

// reconfigure gives the fake daemon the base configuration with the host's proxy objects added.
func (f *fakeSystem) reconfigure() {
	cfg := f.base
	cfg.Containers = append([]dockertest.FakeContainer(nil), f.base.Containers...)
	cfg.Networks = append([]dockertest.FakeNetwork(nil), f.base.Networks...)
	cfg.Volumes = append([]dockertest.FakeVolume(nil), f.base.Volumes...)
	if p := f.host.Proxy; p != nil {
		labels := ingress.ObjectLabels()
		labels[stacks.LabelComposeProject] = ingress.ProjectName
		labels[stacks.LabelComposeService] = ingress.ProxyComposeService
		labels[stacks.LabelComposeOneoff] = "False"
		labels[stacks.LabelStackImage] = p.Image
		if p.TokenLabel != "" {
			labels[ingressdeploy.TokenLabel] = p.TokenLabel
		}
		url, pin := p.URL, p.Pin
		if url == "" {
			url = rigIngressURL
		}
		if pin == "" {
			pin = rigIngressPin
		}
		env := []string{ingressbundle.EnvURL + "=" + url, ingressbundle.EnvPin + "=" + pin}
		if p.AccessLog {
			env = append(env, ingressbundle.EnvAccessLog+"=1")
		}
		state := p.State
		if state == "" {
			state = "running"
		}
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
			ID: "proxy-1", Names: []string{"/tierplan-ingress-proxy-1"}, Image: p.Image, State: state, Labels: labels,
			Inspect: dockertest.FakeInspect{Env: env, StartedAt: "2026-11-21T12:00:00Z"},
		})
	}
	if f.host.NetworkSubnet != "" {
		cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{
			ID: "net-ingress", Name: ingress.NetworkName, Driver: "bridge", Scope: "local",
			Subnets: []string{f.host.NetworkSubnet}, Labels: ingress.ObjectLabels(),
		})
	}
	if f.host.Volume {
		cfg.Volumes = append(cfg.Volumes, dockertest.FakeVolume{Name: ingress.VolumeName, Driver: "local", Labels: ingress.ObjectLabels()})
	}
	f.rig.fake.Reconfigure(cfg)
}

// deployCount is how many times DeploySystem was called.
func (f *fakeSystem) deployCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deploys)
}

// lazyNodes lists the ingress nodes through the rig's token service, which exists only once the rig is built.
type lazyNodes struct{ f *fakeSystem }

func (l lazyNodes) ListNodes(ctx context.Context, envID string) ([]ingresshub.Node, error) {
	return l.f.nodes.ListNodes(ctx, envID)
}

// standaloneRig is a rig whose environment is a standalone host with one connected node, and the fake deployer.
type standaloneRig struct {
	*rig
	sys *fakeSystem
}

// standaloneRigOptions tune a standalone rig.
type standaloneRigOptions struct {
	// host is what the fake host holds to begin with.
	host hostFixture
	// result is what the deploy answers.
	result stackops.DeployResult
	// deps adjusts the deployer's dependencies.
	deps func(*ingressdeploy.Deps)
}

// newStandaloneRig builds a standalone host of one connected node, empty of ingress objects unless the options say
// otherwise, and a deployer over it.
func newStandaloneRig(t *testing.T, o standaloneRigOptions) *standaloneRig {
	t.Helper()
	sys := &fakeSystem{result: o.result}
	r := newRig(t, rigOptions{
		standalone: true,
		config: func(c *dockertest.FakeConfig) {
			c.SwarmState, c.SwarmControl, c.SwarmClusterID, c.Networks = "inactive", false, "", nil
			sys.base = *c
		},
		deps: func(d *ingressdeploy.Deps) {
			d.System = sys
			d.Nodes = lazyNodes{sys}
			if o.deps != nil {
				o.deps(d)
			}
		},
	})
	sys.rig, sys.nodes = r, r.tokens
	r.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.ClusterID, i.ManagerConnected = "", false
		i.Nodes = []ingressdeploy.NodeInfo{{AgentID: testAgentID, Name: "host-1", Connected: true}}
	})
	sys.setHost(o.host)
	return &standaloneRig{rig: r, sys: sys}
}

// addNodeRow enrols a proxy node row as the enrolment would leave it.
func (r *standaloneRig) addNodeRow(status string) {
	r.t.Helper()
	key := sha256.Sum256([]byte("host-1"))
	if _, err := r.st.Write.Exec(`INSERT INTO ingress_nodes
		(id, env_id, name, swarm_node_id, public_key, status, created_at, revoked_at, revoked_by, revoke_reason, reenrol_blocked)
		VALUES (?, ?, ?, ?, ?, ?, 1, 0, '', '', 0)`, uuid.NewString(), r.envID, "host-1", "", key[:], status); err != nil {
		r.t.Fatalf("insert an ingress node: %v", err)
	}
}

// envValue is the value of the variable name in env, "" when it is not there.
func envValue(env []string, name string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
