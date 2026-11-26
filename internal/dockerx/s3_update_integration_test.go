// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

const (
	// s3UpdateBound bounds one rolling update of three nodes at a Delay of 10 s, with its start, tasks and settling.
	s3UpdateBound = 6 * time.Minute
	// s3StableSamples is how many consecutive samples with exactly one holder, after the update says completed, end a watch.
	s3StableSamples = 4
	// s3EnvMaxGap is the longest gap the environment-only change may show at a Delay of 10 s (the master-down interval is
	// about 3.6 s, and a stopped holder says goodbye, so a gap above 5 s means the roll is not safe).
	s3EnvMaxGap = 5.0
)

// holdersOfAny reads every node's addresses at once and counts the nodes that list at least one of addrs, so the sample is
// one look at the segment and not three looks spread over a second.
func (r *s3Rig) holdersOfAny(addrs ...netip.Addr) int {
	r.t.Helper()
	nodes := r.sw.Nodes()
	got := make([]bool, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, err := n.Exec(ctx, "ip", "-4", "-o", "addr", "show")
			if err != nil {
				return
			}
			for _, a := range addrs {
				if strings.Contains(out, "inet "+a.String()+"/") {
					got[i] = true
				}
			}
		}()
	}
	wg.Wait()
	count := 0
	for _, g := range got {
		if g {
			count++
		}
	}
	return count
}

// s3Watch folds a series of samples of the segment into the two numbers the update decision needs: the most nodes that
// listed an address at once and the longest time no node did. A gap is measured from the last sample that had a holder to
// the first one that had a holder again, so it is an upper bound by up to two sample widths (a few hundred milliseconds).
type s3Watch struct {
	samples    int
	maxHolders int
	gap        time.Duration
	lastHeld   time.Time
	sawZero    bool
}

func (w *s3Watch) add(now time.Time, holders int) {
	if w.lastHeld.IsZero() {
		w.lastHeld = now
	}
	w.samples++
	w.maxHolders = max(w.maxHolders, holders)
	if holders == 0 {
		w.sawZero = true
		return
	}
	if w.sawZero {
		w.gap = max(w.gap, now.Sub(w.lastHeld))
	}
	w.sawZero = false
	w.lastHeld = now
}

// rollUpdate updates the keepalived service with parallelism 1, stop-first, the given Delay and the failure action rollback,
// after mutate has changed the spec, and samples the segment until the update has completed and exactly one node holds an
// address of addrs for s3StableSamples samples in a row. A rollback fails the test.
func (r *s3Rig) rollUpdate(delay time.Duration, addrs []netip.Addr, mutate func(*swarm.ServiceSpec)) s3Watch {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cur, err := r.mgr.ServiceInspect(ctx, r.svcID, client.ServiceInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect the keepalived service: %v", err)
	}
	var prev *time.Time
	if u := cur.Service.UpdateStatus; u != nil {
		prev = u.StartedAt
	}
	spec := cur.Service.Spec
	mutate(&spec)
	spec.UpdateConfig = &swarm.UpdateConfig{
		Parallelism:   1,
		Delay:         delay,
		FailureAction: swarm.UpdateFailureActionRollback,
		Order:         swarm.UpdateOrderStopFirst,
	}
	if _, err := r.mgr.ServiceUpdate(ctx, r.svcID, client.ServiceUpdateOptions{Version: cur.Service.Version, Spec: spec}); err != nil {
		r.t.Fatalf("update the keepalived service: %v", err)
	}

	var w s3Watch
	stable := 0
	dockertest.Wait(r.t, s3UpdateBound, "the update to complete with exactly one holder", func() (bool, string) {
		holders := r.holdersOfAny(addrs...)
		now := time.Now()
		w.add(now, holders)
		ictx, icancel := context.WithTimeout(context.Background(), time.Minute)
		defer icancel()
		res, err := r.mgr.ServiceInspect(ictx, r.svcID, client.ServiceInspectOptions{})
		if err != nil {
			return false, err.Error()
		}
		u := res.Service.UpdateStatus
		if u == nil {
			return false, fmt.Sprintf("no UpdateStatus yet, holders %d", holders)
		}
		if strings.HasPrefix(string(u.State), "rollback") || u.State == swarm.UpdateStatePaused {
			r.t.Fatalf("the update did not complete: %s: %s", u.State, u.Message)
		}
		fresh := prev == nil || u.StartedAt == nil || !u.StartedAt.Equal(*prev)
		if u.State != swarm.UpdateStateCompleted || !fresh {
			stable = 0
			return false, fmt.Sprintf("%s, holders %d", u.State, holders)
		}
		if holders == 1 {
			stable++
		} else {
			stable = 0
		}
		return stable >= s3StableSamples, fmt.Sprintf("completed, holders %d, stable %d", holders, stable)
	})
	return w
}

// TestS3RollingUpdate is A-08 and D-16: what a rolling update of the keepalived service does to the holder, for an
// environment-only change, an address change and a router-ID change, each with parallelism 1, stop-first and an update Delay
// of 0 and then 10 s. It records, per case, the most nodes that listed an address at once and the longest time none did. The
// result decides the update Delay and whether the change job may roll each kind of change or must replace the service. The
// test fails only if the environment-only change at a Delay of 10 s ever shows two holders or a gap over 5 s.
func TestS3RollingUpdate(t *testing.T) {
	base := newS3Swarm(t)
	for _, delay := range []time.Duration{0, 10 * time.Second} {
		t.Run("delay-"+delay.String(), func(t *testing.T) {
			r := base.with(t)
			r.start(s3Opts{})
			want := len(r.sw.Nodes())
			dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, want, s3Settle)
			r.waitOneHolder(s3FirstHolder, nil)
			first := r.vip

			report := func(change string, w s3Watch) {
				gap := w.gap.Seconds()
				t.Logf("S3 update-delay: engine=%s delay=%s change=%s max-holders=%d gap=%.1f", r.sw.Engine, delay, change, w.maxHolders, gap)
				if delay > 0 && change != "env" {
					t.Logf("S3 change-%s: engine=%s max-holders=%d gap=%.1f", change, r.sw.Engine, w.maxHolders, gap)
				}
			}
			settle := func() {
				dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, want, s3Settle)
				r.waitOneHolder(s3FirstHolder, nil)
			}

			// An environment-only change: the configuration is the same, so every node keeps its role.
			env := r.rollUpdate(delay, []netip.Addr{r.vip.Addr()}, func(s *swarm.ServiceSpec) {
				s.TaskTemplate.ContainerSpec.Env = append(s.TaskTemplate.ContainerSpec.Env, "S3_DUMMY="+strconv.Itoa(int(delay.Seconds())))
			})
			report("env", env)
			settle()

			// An address change: a new configuration naming .251, the router ID unchanged.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			oldAddr := r.vip.Addr()
			a := oldAddr.As4()
			a[3] = 251
			next := netip.PrefixFrom(netip.AddrFrom4(a), r.vip.Bits())
			cfg := r.config(ctx, s3Opts{vip: next}.withDefaults())
			addr := r.rollUpdate(delay, []netip.Addr{oldAddr, next.Addr()}, func(s *swarm.ServiceSpec) {
				s.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{cfg}
			})
			r.vip = next
			report("address", addr)
			settle()

			// A router-ID change: a new configuration with VRID 52, the address as it now is.
			cfg = r.config(ctx, s3Opts{vip: next, routerID: 52}.withDefaults())
			rid := r.rollUpdate(delay, []netip.Addr{next.Addr()}, func(s *swarm.ServiceSpec) {
				s.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{cfg}
			})
			report("router-id", rid)
			settle()

			if delay == 10*time.Second && (env.maxHolders > 1 || env.gap.Seconds() > s3EnvMaxGap) {
				t.Errorf("an environment-only change at a Delay of 10 s showed max-holders=%d gap=%.1f s (want 1 and at most %.0f s)", env.maxHolders, env.gap.Seconds(), s3EnvMaxGap)
			}

			r.remove()
			dockertest.Wait(t, s3Settle, "no node to list "+first.Addr().String()+" or "+next.Addr().String(), func() (bool, string) {
				n := r.holdersOfAny(first.Addr(), next.Addr())
				return n == 0, strconv.Itoa(n) + " nodes still list it"
			})
		})
	}
}
