// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"bytes"
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Small stack subnets (Phase 10, D-20, NET-06) on the swarm engine: a deploy gives the networks it is about to
// create a block of the environment's chosen size, never one that exists, never one the file addressed.

const backendCompose = `
services:
  web:
    image: nginx:1.27-alpine
    networks: [backend]
networks:
  backend: {}
`

// subnetRig is a swarm whose pool is 10.0.0.0/16 and whose overlays 10.0.0.0/24 and 10.0.1.0/24 exist. mask 0 leaves
// the small subnet setting off.
func subnetRig(t *testing.T, mask int, extra ...dockertest.FakeNetwork) *swarmRig {
	t.Helper()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.SwarmClusterID = "cluster-1"
		c.SwarmDefaultAddrPool = []string{"10.0.0.0/16"}
		c.SwarmSubnetSize = 24
		c.Networks = append([]dockertest.FakeNetwork{
			{ID: "ovl-0", Name: "other_a", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.0.0/24"}},
			{ID: "ovl-1", Name: "other_b", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
		}, extra...)
	})
	if mask != 0 {
		rig.setSmallSubnets(t, true, mask)
	}
	return rig
}

// setSmallSubnets saves the environment's small stack subnet setting the way the settings route does.
func (r *swarmRig) setSmallSubnets(t *testing.T, on bool, mask int) {
	t.Helper()
	ctx := testsupport.Context(t)
	row, err := r.store.Q.GetAddressSettings(ctx, r.envID)
	if err != nil {
		t.Fatal(err)
	}
	enabled := int64(0)
	if on {
		enabled = 1
	}
	n, err := r.store.Q.SetSmallSubnets(ctx, sqlc.SetSmallSubnetsParams{
		SmallSubnets: enabled, SmallSubnetMask: int64(mask), UpdatedAt: time.Now().UnixNano(), ID: r.envID,
		AddressSettingsVersion: row.AddressSettingsVersion,
	})
	if err != nil || n != 1 {
		t.Fatalf("SetSmallSubnets: %d rows, %v", n, err)
	}
}

func subnetRepo(t *testing.T, rig *swarmRig) stackops.Repo {
	t.Helper()
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	return stackops.Repo{Store: rig.store, Vault: v, Now: time.Now}
}

// storedDeploy is a stack in the store with a revision a deploy of it runs on.
type storedDeploy struct {
	repo stackops.Repo
	st   stackops.Stack
	rev  stackops.Snapshot
	in   stackops.SwarmDeployInput
}

// storedStack creates the stack and a pending revision of it and returns the input of its deploy, which records
// pins and subnets on that revision.
func storedStack(t *testing.T, rig *swarmRig, name, compose string) storedDeploy {
	t.Helper()
	ctx := testsupport.Context(t)
	repo := subnetRepo(t, rig)
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}}
	st, _, err := repo.CreateStack(ctx, rig.envID, name, stacks.TargetSwarm, "root", copy)
	if err != nil {
		t.Fatal(err)
	}
	_, in := stackInput(t, name, compose, nil)
	in.Stack = st
	in.Pins, in.Subnets = repo, repo
	d := storedDeploy{repo: repo, st: st, in: in}
	d.newRevision(t)
	return d
}

// newRevision takes the next revision of the stack, as a deploy does, and points the deploy at it.
func (d *storedDeploy) newRevision(t *testing.T) {
	t.Helper()
	rev, err := d.repo.NewRevision(testsupport.Context(t), d.st.ID, "root", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	d.rev, d.in.RevisionID = rev, rev.ID
}

// good marks the revision good, as a deploy that ends well does.
func (d storedDeploy) good(t *testing.T) {
	t.Helper()
	if err := d.repo.SetOutcome(testsupport.Context(t), d.rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
}

// recorded is what the revision recorded for its networks.
func (d storedDeploy) recorded(t *testing.T) map[string]string {
	t.Helper()
	rev, err := d.repo.Revision(testsupport.Context(t), d.st.ID, d.rev.Seq)
	if err != nil {
		t.Fatal(err)
	}
	return rev.Subnets
}

// created is the subnet of each network create the swarm accepted, by network name.
func created(rig *swarmRig) map[string]string {
	out := map[string]string{}
	for _, c := range rig.fake.NetworkCreates() {
		out[c.Name] = ""
		if c.IPAM != nil && len(c.IPAM.Config) > 0 {
			out[c.Name] = c.IPAM.Config[0].Subnet.String()
		}
	}
	return out
}

func TestSmallSubnetSwarmNewNetwork(t *testing.T) {
	t.Parallel()
	rig := subnetRig(t, 26)
	d := storedStack(t, rig, "shop", backendCompose)
	res, job := rig.deploy(t, d.in)
	if !res.OK() || res.Networks != 1 {
		t.Fatalf("result = %+v, want one network created", res)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s %v", job.State, itemStates(job))
	}
	if got := created(rig); len(got) != 1 || got["shop_backend"] != "10.0.2.0/26" {
		t.Errorf("network creates = %v, want shop_backend on 10.0.2.0/26, the lowest free /26 clear of both overlays", got)
	}
	if got := d.recorded(t); len(got) != 1 || got["shop_backend"] != "10.0.2.0/26" {
		t.Errorf("recorded subnets = %v, want shop_backend on 10.0.2.0/26", got)
	}
}

func TestSmallSubnetOffChangesNothing(t *testing.T) {
	t.Parallel()
	rig := subnetRig(t, 0)
	d := storedStack(t, rig, "shop", backendCompose)
	res, _ := rig.deploy(t, d.in)
	if !res.OK() || res.Networks != 1 {
		t.Fatalf("result = %+v, want one network created", res)
	}
	creates := rig.fake.NetworkCreates()
	if len(creates) != 1 || (creates[0].IPAM != nil && len(creates[0].IPAM.Config) != 0) {
		t.Errorf("network creates = %+v, want one with no address pool, as before the setting existed", creates)
	}
	if got := d.recorded(t); len(got) != 0 {
		t.Errorf("recorded subnets = %v, want none with the setting off", got)
	}
}

func TestSmallSubnetSkipsExisting(t *testing.T) {
	t.Parallel()
	rig := subnetRig(t, 26)
	d := storedStack(t, rig, "shop", backendCompose)
	// The network exists already and belongs to this stack by its labels.
	ctx := testsupport.Context(t)
	if err := rig.deps.Aggregator.WriteOnManager(ctx, rig.envID, func(c *dockerx.Client) error {
		_, _, e := c.NetworkCreate(ctx, dockerx.NetworkCreateSpec{
			Name: "shop_backend", Driver: "overlay", Scope: "swarm", Subnet: "10.0.7.0/24",
			Labels: map[string]string{"com.docker.stack.namespace": "shop", "tierplan.stack.id": d.st.ID},
		})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	before := len(rig.fake.NetworkCreates())
	res, _ := rig.deploy(t, d.in)
	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if got := rig.fake.NetworkCreates()[before:]; len(got) != 0 {
		t.Errorf("network creates = %+v, want none: the network exists", got)
	}
	if got := d.recorded(t); len(got) != 0 {
		t.Errorf("recorded subnets = %v, want none: nothing was given a subnet", got)
	}

	// Turned off afterwards, the next deploy of the same stack changes nothing either.
	rig.setSmallSubnets(t, false, 26)
	d.good(t)
	d.newRevision(t)
	if res, _ := rig.deploy(t, d.in); !res.OK() {
		t.Fatalf("redeploy = %+v", res)
	}
	if got := rig.fake.NetworkCreates()[before:]; len(got) != 0 {
		t.Errorf("network creates after turning the setting off = %+v, want none", got)
	}
}

func TestSmallSubnetSkipsExplicitAndExternal(t *testing.T) {
	t.Parallel()
	const compose = `
services:
  web:
    image: nginx:1.27-alpine
    networks: [fixed, outside, fresh]
networks:
  fixed:
    ipam:
      config:
        - subnet: 10.0.9.0/24
  outside:
    external: true
  fresh: {}
`
	rig := subnetRig(t, 26, dockertest.FakeNetwork{ID: "n-out", Name: "outside", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.8.0/24"}})
	d := storedStack(t, rig, "shop", compose)
	res, _ := rig.deploy(t, d.in)
	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	want := map[string]string{"shop_fixed": "10.0.9.0/24", "shop_fresh": "10.0.2.0/26"}
	if got := created(rig); len(got) != 2 || got["shop_fixed"] != want["shop_fixed"] || got["shop_fresh"] != want["shop_fresh"] {
		t.Errorf("network creates = %v, want %v and the external network left alone", got, want)
	}
	if got := d.recorded(t); len(got) != 1 || got["shop_fresh"] != "10.0.2.0/26" {
		t.Errorf("recorded subnets = %v, want only the block the deploy chose", got)
	}
}

func TestSmallSubnetKeyOrder(t *testing.T) {
	t.Parallel()
	const compose = `
services:
  web:
    image: nginx:1.27-alpine
    networks: [b, a]
networks:
  b: {}
  a: {}
`
	rig := subnetRig(t, 26)
	d := storedStack(t, rig, "shop", compose)
	if res, _ := rig.deploy(t, d.in); !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	got := created(rig)
	if got["shop_a"] != "10.0.2.0/26" || got["shop_b"] != "10.0.2.64/26" {
		t.Errorf("network creates = %v, want a on 10.0.2.0/26 and b on 10.0.2.64/26, lowest first in key order", got)
	}
}

// removeNetwork removes a network of the fake swarm by name through a manager.
func removeNetwork(t *testing.T, rig *swarmRig, name string) {
	t.Helper()
	ctx := testsupport.Context(t)
	agg, _, err := rig.deps.Aggregator.FreshSnapshot(ctx, rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range agg.Networks {
		if n.Name == name {
			if err := rig.deps.Aggregator.WriteOnManager(ctx, rig.envID, func(c *dockerx.Client) error { return c.NetworkRemove(ctx, n.ID) }); err != nil {
				t.Fatalf("remove %s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("no network %s to remove", name)
}

func TestSmallSubnetRollbackReusesRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		taken bool
		want  string
	}{
		{"free", false, "10.0.2.0/26"},
		{"taken meanwhile", true, "10.0.2.64/26"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := subnetRig(t, 26)
			d := storedStack(t, rig, "shop", backendCompose)
			res, _ := rig.deploy(t, d.in)
			if !res.OK() || created(rig)["shop_backend"] != "10.0.2.0/26" {
				t.Fatalf("first deploy = %+v, creates %v", res, created(rig))
			}
			d.good(t)
			first := d.rev
			first.Subnets = d.recorded(t)

			// The network goes away; someone else may take its block.
			removeNetwork(t, rig, "shop_backend")
			if tc.taken {
				ctx := testsupport.Context(t)
				err := rig.deps.Aggregator.WriteOnManager(ctx, rig.envID, func(c *dockerx.Client) error {
					_, _, e := c.NetworkCreate(ctx, dockerx.NetworkCreateSpec{Name: "squatter", Driver: "overlay", Scope: "swarm", Subnet: "10.0.2.0/26"})
					return e
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := len(rig.fake.NetworkCreates())

			// The way back: the revision's own record goes with its digests.
			rb, err := d.repo.CopyRevision(testsupport.Context(t), first, "root", stackops.OriginRollback)
			if err != nil {
				t.Fatal(err)
			}
			in := d.in
			in.RevisionID, in.Rollback = rb.ID, true
			in.Reviewed = map[string]string{"shop_web": res.Pins["shop_web"].Ref}
			in.RestoredSubnets = first.Subnets
			if res, _ := rig.deploy(t, in); !res.OK() {
				t.Fatalf("rollback = %+v", res)
			}
			creates := rig.fake.NetworkCreates()[before:]
			var got string
			for _, c := range creates {
				if c.Name == "shop_backend" && c.IPAM != nil && len(c.IPAM.Config) > 0 {
					got = c.IPAM.Config[0].Subnet.String()
				}
			}
			if got != tc.want {
				t.Errorf("the rollback created shop_backend on %q, want %q", got, tc.want)
			}
			rev, err := d.repo.Revision(testsupport.Context(t), d.st.ID, rb.Seq)
			if err != nil {
				t.Fatal(err)
			}
			if rev.Subnets["shop_backend"] != tc.want {
				t.Errorf("the rollback's revision recorded %v, want shop_backend on %s", rev.Subnets, tc.want)
			}
		})
	}
}

func TestSmallSubnetNoFreeBlock(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.SwarmClusterID = "cluster-1"
		c.SwarmDefaultAddrPool = []string{"10.0.0.0/24"}
		c.SwarmSubnetSize = 24
		c.Networks = []dockertest.FakeNetwork{{ID: "ovl-0", Name: "other_a", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.0.0/24"}}}
	})
	rig.setSmallSubnets(t, true, 26)
	d := storedStack(t, rig, "shop", backendCompose)
	res, job := rig.deploy(t, d.in)
	if res.Code != stackops.CodeNoFreeSubnet || res.FailedService != "shop_backend" {
		t.Fatalf("result = %+v, want small_subnet_no_free_block at shop_backend", res)
	}
	if got := itemStates(job); got[stackops.ItemCheck] != "failed:small_subnet_no_free_block" || got[stackops.ItemNetworks] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
	for _, it := range job.Items {
		if it.ID == stackops.ItemCheck && (!strings.Contains(it.Message, "shop_backend") || !strings.Contains(it.Message, "/26") || !strings.Contains(it.Message, "10.0.0.0/24")) {
			t.Errorf("check message = %q, want the network, the mask and the pool", it.Message)
		}
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none before the refusal", w)
	}
}

// A deploy and a network made from the dialog at the same time never take the same block: the dialog waits for the
// allocation lock the deploy holds from planning to the last create, then sees the deploy's network.
func TestSmallSubnetRace(t *testing.T) {
	t.Parallel()
	rig := subnetRig(t, 26)
	planned, release := make(chan struct{}), make(chan struct{})
	rig.deps = stackops.SetSubnetPlanHook(rig.deps, func() {
		close(planned)
		<-release
	})
	d := storedStack(t, rig, "shop", backendCompose)
	running := rig.startDeploy(t, d.in)
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
			picked <- netip.Prefix{}
			return
		}
		err = rig.deps.Aggregator.WriteOnManager(ctx, rig.envID, func(c *dockerx.Client) error {
			_, _, e := c.NetworkCreate(ctx, dockerx.NetworkCreateSpec{Name: "dialog", Driver: "overlay", Scope: "swarm", Subnet: block.String()})
			return e
		})
		if err != nil {
			t.Errorf("create: %v", err)
		}
		picked <- block
	}()
	testsupport.Await(t, "the dialog to ask for the lock", started)
	close(release)

	res, _ := running.finish(t)
	if !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	var dialog netip.Prefix
	select {
	case dialog = <-picked:
	case <-testsupport.Context(t).Done():
		t.Fatal("the dialog never finished")
	}
	if got := created(rig); got["shop_backend"] != "10.0.2.0/26" || got["dialog"] != "10.0.2.64/26" || dialog.String() != "10.0.2.64/26" {
		t.Errorf("creates = %v, dialog picked %s: want the deploy on 10.0.2.0/26 and the dialog on the next block", got, dialog)
	}
}

func TestSmallSubnetEngineOverlapRecomputesOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refuse  []string
		wantOK  bool
		subnet  string
		creates int
	}{
		{"refused once", []string{"10.0.0.0/26"}, true, "10.0.0.64/26", 1},
		{"refused twice", []string{"10.0.0.0/26", "10.0.0.64/26"}, false, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
				c.SwarmClusterID = "cluster-1"
				c.SwarmDefaultAddrPool = []string{"10.0.0.0/16"}
				c.SwarmSubnetSize = 24
				c.OverlapSubnets = tc.refuse
			})
			rig.setSmallSubnets(t, true, 26)
			d := storedStack(t, rig, "shop", backendCompose)
			res, _ := rig.deploy(t, d.in)
			if res.OK() != tc.wantOK {
				t.Fatalf("result = %+v, want ok = %v", res, tc.wantOK)
			}
			creates := rig.fake.NetworkCreates()
			if len(creates) != tc.creates {
				t.Errorf("accepted creates = %d, want %d", len(creates), tc.creates)
			}
			if tc.wantOK {
				if got := d.recorded(t); got["shop_backend"] != tc.subnet {
					t.Errorf("recorded = %v, want shop_backend on %s, the block the second try was given", got, tc.subnet)
				}
			}
			// Two refused writes at most: the first choice and the one recompute.
			attempts := 0
			for _, w := range rig.fake.Writes() {
				if w == "POST /networks/create" {
					attempts++
				}
			}
			if attempts > 2 {
				t.Errorf("network create attempts = %d, want at most 2 (the choice and one recompute)", attempts)
			}
			if !tc.wantOK && res.Code != stackops.CodeNetworkFailed {
				t.Errorf("code = %q, want %s after the second refusal", res.Code, stackops.CodeNetworkFailed)
			}
		})
	}
}

// TestSmallSubnetSwarmUnallocatedRecomputesOnce is the same for a block the swarm accepts and never allocates: the stub is
// removed again, the block is chosen again once, and a second failure fails the deploy.
func TestSmallSubnetSwarmUnallocatedRecomputesOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		refuse []string
		wantOK bool
		subnet string
	}{
		{"unallocated once", []string{"10.0.0.0/26"}, true, "10.0.0.64/26"},
		{"unallocated twice", []string{"10.0.0.0/26", "10.0.0.64/26"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
				c.SwarmClusterID = "cluster-1"
				c.SwarmDefaultAddrPool = []string{"10.0.0.0/16"}
				c.SwarmSubnetSize = 24
				c.SwarmUnallocated = map[string]int{}
				for _, s := range tc.refuse {
					c.SwarmUnallocated[s] = -1
				}
			})
			rig.setSmallSubnets(t, true, 26)
			d := storedStack(t, rig, "shop", backendCompose)
			res, _ := rig.deploy(t, d.in)
			if res.OK() != tc.wantOK {
				t.Fatalf("result = %+v, want ok = %v", res, tc.wantOK)
			}
			attempts := countWrites(rig.fake.Writes(), "POST /networks/create")
			if attempts > 2 {
				t.Errorf("network create attempts = %d, want at most 2 (the choice and one recompute)", attempts)
			}
			if tc.wantOK {
				if got := d.recorded(t); got["shop_backend"] != tc.subnet {
					t.Errorf("recorded = %v, want shop_backend on %s, the block the second try was given", got, tc.subnet)
				}
				if n := rig.stackNetworks(t, "shop"); !slices.Equal(n, []string{"shop_backend"}) {
					t.Errorf("networks = %v, want only the allocated one", n)
				}
				return
			}
			if attempts != 2 {
				t.Errorf("network create attempts = %d, want 2", attempts)
			}
			if res.Code != stackops.CodeNetworkNotAllocated {
				t.Errorf("code = %q, want %s after the second failure", res.Code, stackops.CodeNetworkNotAllocated)
			}
			if n := rig.stackNetworks(t, "shop"); len(n) != 0 {
				t.Errorf("networks = %v, want none left", n)
			}
			if got := rig.fake.ServiceCreates(); len(got) != 0 {
				t.Errorf("service creates = %d, want none", len(got))
			}
		})
	}
}
