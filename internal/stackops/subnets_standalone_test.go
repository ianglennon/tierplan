// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/netspace"
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

// ownedNetwork is a network the stack's earlier deploy left on the host, with the labels the SDK and Coaming put on it.
func ownedNetwork(st stackops.Stack, name, subnet string, extra map[string]string) dockerx.NetworkCreateSpec {
	labels := map[string]string{stacks.LabelComposeProject: st.Name, stacks.LabelStackID: st.ID}
	for k, v := range extra {
		labels[k] = v
	}
	return dockerx.NetworkCreateSpec{Name: name, Driver: "bridge", Subnet: subnet, Labels: labels}
}

func TestStandaloneSubnetRecordAlwaysReapplied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mask int
	}{{"setting on", 26}, {"setting off", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := standaloneSubnetRig(t, tc.mask)
			d := storedStandalone(t, rig, "shop", standaloneBackend)
			// The network exists on a block an earlier deploy chose, and that revision recorded it.
			seedNetwork(t, rig, ownedNetwork(d.st, "shop_backend", "10.50.7.0/26", nil))
			d.setRecord(t, map[string]string{"shop_backend": "10.50.7.0/26"})
			d.good(t)
			d.newRevision(t)

			res, _ := rig.deployStandalone(t, d.in)

			if !res.OK() || res.Networks != 0 {
				t.Fatalf("result = %+v, want no network created", res)
			}
			if got := d.sent(t); got["backend"] != "10.50.7.0/26" {
				t.Errorf("the SDK was handed %v, want exactly the recorded 10.50.7.0/26, never a new pick: the network's hash must not change", got)
			}
			if got := d.recorded(t); got["shop_backend"] != "10.50.7.0/26" {
				t.Errorf("recorded subnets = %v, want the record carried to the new revision", got)
			}
		})
	}
}

func TestStandaloneSubnetNeverForExistingUnrecorded(t *testing.T) {
	t.Parallel()
	for _, mask := range []int{26, 0} {
		rig := standaloneSubnetRig(t, mask)
		d := storedStandalone(t, rig, "shop", standaloneBackend)
		// Deployed before the setting existed: the network has whatever subnet the daemon gave it, and a hash made without one.
		seedNetwork(t, rig, ownedNetwork(d.st, "shop_backend", "10.50.3.0/24", nil))

		res, _ := rig.deployStandalone(t, d.in)

		if !res.OK() || res.Networks != 0 {
			t.Fatalf("mask %d: result = %+v, want no network created", mask, res)
		}
		if got := d.sent(t); got["backend"] != "" {
			t.Errorf("mask %d: the SDK was handed %v, want no subnet for a network that exists with no record", mask, got)
		}
		if got := d.recorded(t); len(got) != 0 {
			t.Errorf("mask %d: recorded subnets = %v, want none", mask, got)
		}
	}
}

// networkHashWith is the hash the SDK stamps on the project's network backend of a stack called name, given a subnet
// ("" for none).
func networkHashWith(t *testing.T, name, compose, subnet string) string {
	t.Helper()
	checked := stacks.Check(context.Background(), stacks.CheckInput{
		Name: name, Target: stacks.TargetStandalone,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}},
	})
	if checked.Loaded == nil {
		t.Fatalf("check: %v", checked.Problems)
	}
	n := checked.Loaded.Project.Networks["backend"]
	if subnet != "" {
		n.Ipam.Config = []*types.IPAMPool{{Subnet: subnet}}
	}
	h, err := dockerx.ComposeNetworkHash(&n)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A network an earlier deploy made with a subnet that no good revision recorded (that deploy failed after the
// network was created) is told apart by its hash label: giving it the subnet is what keeps the SDK from recreating it.
func TestStandaloneSubnetKeptForANetworkWhoseHashShowsIt(t *testing.T) {
	t.Parallel()
	rig := standaloneSubnetRig(t, 26)
	d := storedStandalone(t, rig, "shop", standaloneBackend)
	seedNetwork(t, rig, ownedNetwork(d.st, "shop_backend", "10.50.7.0/26", map[string]string{
		stacks.LabelComposeConfigHash: networkHashWith(t, "shop", standaloneBackend, "10.50.7.0/26"),
	}))

	res, _ := rig.deployStandalone(t, d.in)

	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if got := d.sent(t); got["backend"] != "10.50.7.0/26" {
		t.Errorf("the SDK was handed %v, want the subnet the network's hash was made with", got)
	}

	// The same network whose label is the hash of the plain one is left alone.
	rig = standaloneSubnetRig(t, 26)
	d = storedStandalone(t, rig, "shop", standaloneBackend)
	seedNetwork(t, rig, ownedNetwork(d.st, "shop_backend", "10.50.7.0/26", map[string]string{
		stacks.LabelComposeConfigHash: networkHashWith(t, "shop", standaloneBackend, ""),
	}))
	if res, _ = rig.deployStandalone(t, d.in); !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if got := d.sent(t); got["backend"] != "" {
		t.Errorf("the SDK was handed %v, want none for a network whose hash was made without a subnet", got)
	}
}

func TestStandaloneSubnetSkipsExplicitAndExternal(t *testing.T) {
	t.Parallel()
	const compose = `
services:
  web:
    image: nginx:1.27-alpine
    networks: [fixed, outside, fresh, macv]
networks:
  fixed:
    ipam:
      config:
        - subnet: 10.60.0.0/24
  outside:
    external: true
  macv:
    driver: macvlan
  fresh: {}
`
	rig := standaloneSubnetRig(t, 26)
	d := storedStandalone(t, rig, "shop", compose)

	res, _ := rig.deployStandalone(t, d.in)

	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	got := d.sent(t)
	if got["fixed"] != "10.60.0.0/24" || got["outside"] != "" || got["macv"] != "" || got["fresh"] != "10.50.1.0/26" {
		t.Errorf("the SDK was handed %v, want the file's own subnet, none for the external and macvlan networks, and a block for fresh alone", got)
	}
	if rec := d.recorded(t); len(rec) != 1 || rec["shop_fresh"] != "10.50.1.0/26" {
		t.Errorf("recorded subnets = %v, want only the block the deploy chose", rec)
	}
}

func TestStandaloneSubnetKeyOrderAvoidsEarlierPicks(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27-alpine\n    networks: [b, a]\nnetworks:\n  b: {}\n  a: {}\n"
	rig := standaloneSubnetRig(t, 27)
	d := storedStandalone(t, rig, "shop", compose)
	if res, _ := rig.deployStandalone(t, d.in); !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if got := d.sent(t); got["a"] != "10.50.1.0/27" || got["b"] != "10.50.1.32/27" {
		t.Errorf("the SDK was handed %v, want a on 10.50.1.0/27 and b on 10.50.1.32/27, in key order", got)
	}
}

func TestStandaloneSubnetRollback(t *testing.T) {
	t.Parallel()
	older := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name     string
		squatter string
		mask     int
		want     string
	}{
		{"its block is free", "", 26, "10.50.2.0/26"},
		{"its block is free with the setting off", "", 0, "10.50.2.0/26"},
		{"its block was taken meanwhile", "10.50.2.0/25", 26, "10.50.1.0/26"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := standaloneSubnetRig(t, tc.mask)
			d := storedStandalone(t, rig, "shop", standaloneBackend)
			if tc.squatter != "" {
				seedNetwork(t, rig, dockerx.NetworkCreateSpec{Name: "squatter", Driver: "bridge", Subnet: tc.squatter})
			}
			d.in.Rollback, d.in.Reviewed = true, map[string]string{"web": older}
			// The revision to restore recorded the block; the network went away since.
			d.in.RestoredSubnets = map[string]string{"shop_backend": "10.50.2.0/26"}

			res, _ := rig.deployStandalone(t, d.in)

			if !res.OK() || res.Networks != 1 {
				t.Fatalf("result = %+v, want the network created again", res)
			}
			if got := d.sent(t); got["backend"] != tc.want {
				t.Errorf("the SDK was handed %v, want backend on %s", got, tc.want)
			}
		})
	}
}

// A rollback to a revision whose network still exists sends the subnet that revision recorded, so the network is kept.
func TestStandaloneSubnetRollbackKeepsAnExistingNetwork(t *testing.T) {
	t.Parallel()
	older := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	rig := standaloneSubnetRig(t, 0)
	d := storedStandalone(t, rig, "shop", standaloneBackend)
	seedNetwork(t, rig, ownedNetwork(d.st, "shop_backend", "10.50.2.0/26", nil))
	d.in.Rollback, d.in.Reviewed = true, map[string]string{"web": older}
	d.in.RestoredSubnets = map[string]string{"shop_backend": "10.50.2.0/26"}

	res, _ := rig.deployStandalone(t, d.in)

	if !res.OK() || res.Networks != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := d.sent(t); got["backend"] != "10.50.2.0/26" {
		t.Errorf("the SDK was handed %v, want the recorded subnet with the setting off", got)
	}
}

// The deploy holds the allocation lock from planning until the SDK's up has returned: a second allocation (the
// network dialog) made in between waits and takes the next block (NET-06).
func TestSmallSubnetStandaloneRace(t *testing.T) {
	t.Parallel()
	rig := standaloneSubnetRig(t, 26)
	planned, release := make(chan struct{}), make(chan struct{})
	rig.deps = stackops.SetSubnetPlanHook(rig.deps, func() {
		close(planned)
		<-release
	})
	d := storedStandalone(t, rig, "shop", standaloneBackend)
	_, done := rig.startStandalone(t, d.in)
	testsupport.Await(t, "the deploy to have planned its subnets", planned)

	started, picked := make(chan struct{}), make(chan netip.Prefix, 1)
	go func() {
		ctx := context.Background()
		close(started)
		defer rig.addr.LockAllocation(rig.envID)()
		in, err := rig.addr.AllocationInputs(ctx, rig.envID, true)
		if err != nil {
			t.Errorf("AllocationInputs: %v", err)
			picked <- netip.Prefix{}
			return
		}
		block, _, err := netspace.LowestFree(in.Pools, in.Occupied, in.Avoid, 26)
		if err != nil {
			t.Errorf("LowestFree: %v", err)
		}
		picked <- block
	}()
	testsupport.Await(t, "the dialog to ask for the lock", started)
	close(release)

	var res stackops.DeployResult
	select {
	case res = <-done:
	case <-testsupport.Context(t).Done():
		t.Fatal("the deploy never finished")
	}
	if !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	var dialog netip.Prefix
	select {
	case dialog = <-picked:
	case <-testsupport.Context(t).Done():
		t.Fatal("the dialog never finished")
	}
	// The deploy's network is not on the host (the SDK is a stand-in), so only the order of the two is checked: the
	// dialog got the lock after the deploy let go of it, having planned 10.50.1.0/26 under it.
	if got := d.sent(t); got["backend"] != "10.50.1.0/26" {
		t.Errorf("the SDK was handed %v, want backend on 10.50.1.0/26", got)
	}
	if !dialog.IsValid() {
		t.Error("the dialog picked no block")
	}
}

// memSubnets is a SubnetStore that holds a record in memory.
type memSubnets map[string]string

func (m memSubnets) GoodSubnets(context.Context, string) (map[string]string, error) { return m, nil }
func (m memSubnets) SetSubnets(context.Context, string, map[string]string) error    { return nil }

func TestStandaloneReviewSmallSubnet(t *testing.T) {
	t.Parallel()
	const compose = `
services:
  web:
    image: nginx:1.27-alpine
    networks: [b, a, fixed]
networks:
  a: {}
  b: {}
  fixed:
    ipam:
      config:
        - subnet: 10.60.0.0/24
`
	review := func(rig *swarmRig, record memSubnets, text string) stackops.Review {
		in := rig.standaloneReviewInput(t, "shop", text, nil)
		in.Subnets = record
		rv, err := rig.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		return rv
	}
	rows := func(rv stackops.Review) map[string]stackops.ObjectChange {
		out := map[string]stackops.ObjectChange{}
		for _, o := range rv.Objects {
			if o.Kind == "network" {
				out[o.Name] = o
			}
		}
		return out
	}

	t.Run("new networks show the block the deploy will use", func(t *testing.T) {
		t.Parallel()
		got := rows(review(standaloneSubnetRig(t, 26), nil, compose))
		if a := got["shop_a"]; a.Subnet != "10.50.1.0/26" || !a.SmallSubnetDefault {
			t.Errorf("shop_a = %+v, want 10.50.1.0/26 with the small-subnet default", a)
		}
		if b := got["shop_b"]; b.Subnet != "10.50.1.64/26" || !b.SmallSubnetDefault {
			t.Errorf("shop_b = %+v, want 10.50.1.64/26 with the small-subnet default", b)
		}
		if f := got["shop_fixed"]; f.Subnet != "10.60.0.0/24" || f.SmallSubnetDefault {
			t.Errorf("shop_fixed = %+v, want the file's own subnet and no small-subnet default", f)
		}
	})

	t.Run("a network created again takes its recorded block", func(t *testing.T) {
		t.Parallel()
		got := rows(review(standaloneSubnetRig(t, 26), memSubnets{"shop_a": "10.50.9.0/26"}, compose))
		if a := got["shop_a"]; a.Subnet != "10.50.9.0/26" || !a.SmallSubnetDefault {
			t.Errorf("shop_a = %+v, want the recorded 10.50.9.0/26", a)
		}
	})

	t.Run("a network the host holds is not a row, and not a change", func(t *testing.T) {
		t.Parallel()
		rig := standaloneSubnetRig(t, 26)
		st := standaloneStackOf("shop")
		single := "services:\n  web:\n    image: nginx:1.27-alpine\n    networks: [backend]\nnetworks:\n  backend: {}\n"
		seedNetwork(t, rig, ownedNetwork(st, "shop_backend", "10.50.7.0/26", map[string]string{
			stacks.LabelComposeConfigHash: networkHashWith(t, "shop", single, "10.50.7.0/26"),
		}))
		rv := review(rig, memSubnets{"shop_backend": "10.50.7.0/26"}, single)
		for _, o := range rv.Objects {
			if o.Kind == "network" {
				t.Errorf("object %+v, want no network row: the recorded subnet is sent again, so the SDK keeps the network", o)
			}
		}
	})

	t.Run("nothing free blocks the deploy", func(t *testing.T) {
		t.Parallel()
		rig := standaloneSubnetRig(t, 26, dockertest.FakeAddressPool{Base: "10.50.0.0/24", Size: 24})
		rv := review(rig, nil, standaloneBackend)
		var found *stacks.Problem
		for i, p := range rv.Problems {
			if p.Code == stackops.CodeNoFreeSubnet {
				found = &rv.Problems[i]
			}
		}
		if found == nil || found.Severity != stacks.SeverityError || found.Key != "networks.backend" {
			t.Fatalf("problems = %+v, want a blocking small_subnet_no_free_block keyed networks.backend", rv.Problems)
		}
	})

	t.Run("off shows no subnet", func(t *testing.T) {
		t.Parallel()
		got := rows(review(standaloneSubnetRig(t, 0), nil, compose))
		if a := got["shop_a"]; a.Subnet != "" || a.SmallSubnetDefault {
			t.Errorf("shop_a = %+v, want no subnet with the setting off", a)
		}
	})
}
