// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"context"
	"net/netip"
	"os"
	"testing"
	"time"
)

// TestSystemProberLive runs the real prober against a real segment. It needs CAP_NET_RAW and a second machine
// on the interface (a network namespace on a veth pair is enough), so it runs only when the environment names
// them:
//
//	TIERPLAN_TEST_PROBE_IFACE  the interface the prober uses
//	TIERPLAN_TEST_PROBE_HELD   an address another machine answers ARP for
//	TIERPLAN_TEST_PROBE_FREE   an address nobody answers for
//	TIERPLAN_TEST_PROBE_ROUTER a router ID that machine advertises on 224.0.0.18 every second
//
// 26-25 judges the prober's reliability on dind; this is the quick check that the sockets work at all.
func TestSystemProberLive(t *testing.T) {
	iface := os.Getenv("TIERPLAN_TEST_PROBE_IFACE")
	if iface == "" {
		t.Skip("set TIERPLAN_TEST_PROBE_IFACE to run the prober against a real segment")
	}
	held := netip.MustParseAddr(os.Getenv("TIERPLAN_TEST_PROBE_HELD"))
	free := netip.MustParseAddr(os.Getenv("TIERPLAN_TEST_PROBE_FREE"))
	router := 0
	for _, c := range os.Getenv("TIERPLAN_TEST_PROBE_ROUTER") {
		router = router*10 + int(c-'0')
	}
	ctx := context.Background()
	var p SystemProber

	if got, err := p.ARPProbe(ctx, iface, held, arpProbes); err != nil || !got {
		t.Errorf("ARPProbe(held) = %v, %v, want true", got, err)
	}
	if got, err := p.ARPProbe(ctx, iface, free, arpProbes); err != nil || got {
		t.Errorf("ARPProbe(free) = %v, %v, want false", got, err)
	}
	if router != 0 {
		if got, err := p.VRRPListen(ctx, iface, router, vrrpListen); err != nil || !got {
			t.Errorf("VRRPListen(%d) = %v, %v, want true", router, got, err)
		}
		start := time.Now()
		if got, err := p.VRRPListen(ctx, iface, router+1, 2*time.Second); err != nil || got {
			t.Errorf("VRRPListen(%d) = %v, %v, want false", router+1, got, err)
		}
		if took := time.Since(start); took < 1500*time.Millisecond || took > 4*time.Second { // wall-clock: the listener's own deadline
			t.Errorf("VRRPListen of another router ID took %s, want about its 2 s deadline", took)
		}
	}
	// A stop ends a listen at once.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.VRRPListen(cctx, iface, router, vrrpListen); err == nil {
		t.Error("VRRPListen ignored a stopped context")
	}
	// An interface that does not exist is a closed word, not the system's text.
	if _, err := p.ARPProbe(ctx, "tp-nonexistent0", held, 1); err != errProbeInterface {
		t.Errorf("ARPProbe on a missing interface = %v, want %v", err, errProbeInterface)
	}
}
