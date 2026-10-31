// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// Small stack subnets (Phase 10, D-20, D-22, NET-06) on a standalone host: the subnet a network is first created
// with is given to it again on every later deploy, so the Compose SDK never sees the network change and never
// recreates it (RESEARCH F5, Pitfall 1). The SDK itself is replaced by a stand-in that keeps the project it was handed.

const standaloneBackend = "services:\n  web:\n    image: nginx:1.27-alpine\n    networks: [backend]\nnetworks:\n  backend: {}\n"

// standaloneSubnetRig is a standalone host whose daemon hands networks out of 10.50.0.0/16 in /24 blocks and
// whose default bridge holds 10.50.0.0/24. mask 0 leaves the small subnet setting off.
func standaloneSubnetRig(t *testing.T, mask int, pools ...dockertest.FakeAddressPool) *swarmRig {
	t.Helper()
	if len(pools) == 0 {
		pools = []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 24}}
	}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.DefaultAddressPools = pools
		c.Networks = []dockertest.FakeNetwork{{ID: "br-0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.0/24"}}}
	})
	if mask != 0 {
		rig.setSmallSubnets(t, true, mask)
	}
	return rig
}

// seedNetwork creates a local network on the host, as an earlier deploy of the SDK left it.
func seedNetwork(t *testing.T, rig *swarmRig, spec dockerx.NetworkCreateSpec) {
	t.Helper()
	ctx := testsupport.Context(t)
	if err := rig.deps.Aggregator.WriteOnNode(ctx, rig.envID, rig.envID, func(c *dockerx.Client) error {
		_, _, err := c.NetworkCreate(ctx, spec)
		return err
	}); err != nil {
		t.Fatalf("create %s: %v", spec.Name, err)
	}
}

// storedStandaloneDeploy is a standalone stack in the store with a revision a deploy of it runs on.
type storedStandaloneDeploy struct {
	repo stackops.Repo
	st   stackops.Stack
	rev  stackops.Snapshot
	in   stackops.StandaloneDeployInput
	up   *upStandIn
}

func storedStandalone(t *testing.T, rig *swarmRig, name, compose string) *storedStandaloneDeploy {
	t.Helper()
	repo := subnetRepo(t, rig)
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}}
	st, _, err := repo.CreateStack(testsupport.Context(t), rig.envID, name, stacks.TargetStandalone, "root", copy)
	if err != nil {
		t.Fatal(err)
	}
	d := &storedStandaloneDeploy{repo: repo, st: st, up: &upStandIn{}}
	d.in = stackops.StandaloneDeployInput{Stack: st, Copy: copy, Pins: repo, Subnets: repo}
	rig.deps.Up = d.up.up
	d.newRevision(t)
	return d
}

func (d *storedStandaloneDeploy) newRevision(t *testing.T) {
	t.Helper()
	rev, err := d.repo.NewRevision(testsupport.Context(t), d.st.ID, "root", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	d.rev, d.in.RevisionID = rev, rev.ID
}

func (d *storedStandaloneDeploy) good(t *testing.T) {
	t.Helper()
	if err := d.repo.SetOutcome(testsupport.Context(t), d.rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
}

// setRecord stores what the revision recorded for its networks.
func (d *storedStandaloneDeploy) setRecord(t *testing.T, rec map[string]string) {
	t.Helper()
	if err := d.repo.SetSubnets(testsupport.Context(t), d.rev.ID, rec); err != nil {
		t.Fatal(err)
	}
}

func (d *storedStandaloneDeploy) recorded(t *testing.T) map[string]string {
	t.Helper()
	rev, err := d.repo.Revision(testsupport.Context(t), d.st.ID, d.rev.Seq)
	if err != nil {
		t.Fatal(err)
	}
	return rev.Subnets
}

// sent is the subnets of the networks of the project the SDK was handed, by network key; "" for a network with none.
func (d *storedStandaloneDeploy) sent(t *testing.T) map[string]string {
	t.Helper()
	if d.up.project == nil {
		t.Fatal("the SDK was never called")
	}
	out := map[string]string{}
	for key, n := range d.up.project.Networks {
		out[key] = ""
		if len(n.Ipam.Config) > 0 {
			out[key] = n.Ipam.Config[0].Subnet
		}
	}
	return out
}

func TestSmallSubnetStandaloneNewNetwork(t *testing.T) {
	t.Parallel()
	rig := standaloneSubnetRig(t, 26)
	d := storedStandalone(t, rig, "shop", standaloneBackend)

	res, _ := rig.deployStandalone(t, d.in)

	if !res.OK() || res.Networks != 1 {
		t.Fatalf("result = %+v, want one network created", res)
	}
	if got := d.sent(t); len(got) != 1 || got["backend"] != "10.50.1.0/26" {
		t.Errorf("the SDK was handed the subnets %v, want backend on 10.50.1.0/26, the lowest free /26 clear of the bridge", got)
	}
	if got := d.recorded(t); len(got) != 1 || got["shop_backend"] != "10.50.1.0/26" {
		t.Errorf("recorded subnets = %v, want shop_backend on 10.50.1.0/26", got)
	}
	if !strings.Contains(string(d.in.Copy.Files[0].Content), "backend: {}") || strings.Contains(string(d.in.Copy.Files[0].Content), "10.50") {
		t.Errorf("the compose file was rewritten: %s", d.in.Copy.Files[0].Content)
	}
}

func TestSmallSubnetStandaloneOffChangesNothing(t *testing.T) {
	t.Parallel()
	rig := standaloneSubnetRig(t, 0)
	d := storedStandalone(t, rig, "shop", standaloneBackend)

	res, _ := rig.deployStandalone(t, d.in)

	if !res.OK() || res.Networks != 1 {
		t.Fatalf("result = %+v, want one network created", res)
	}
	if got := d.sent(t); got["backend"] != "" {
		t.Errorf("the SDK was handed the subnets %v, want none with the setting off", got)
	}
	if got := d.recorded(t); len(got) != 0 {
		t.Errorf("recorded subnets = %v, want none", got)
	}
}

func TestSmallSubnetStandaloneNoFreeBlock(t *testing.T) {
	t.Parallel()
	// The only pool is the /26 the bridge holds: no /26 is left.
	rig := standaloneSubnetRig(t, 26, dockertest.FakeAddressPool{Base: "10.50.0.0/24", Size: 24})
	d := storedStandalone(t, rig, "shop", standaloneBackend)

	res, _ := rig.deployStandalone(t, d.in)

	if res.OK() || res.Code != stackops.CodeNoFreeSubnet || d.up.calls != 0 {
		t.Fatalf("result %+v after %d runs, want small_subnet_no_free_block before the SDK is called", res, d.up.calls)
	}
}
