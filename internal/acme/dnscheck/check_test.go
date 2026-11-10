// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnscheck

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
)

const (
	testFQDN  = "_acme-challenge.app.example.test"
	testValue = "gfj9Xq...Rg85nM"
)

// countingClock is the test clock that counts the timers the code under test armed.
type countingClock struct {
	*acmetest.Clock
	armed atomic.Int32
}

func newClock() *countingClock {
	return &countingClock{Clock: acmetest.NewClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))}
}

func (c *countingClock) After(d time.Duration) <-chan time.Time {
	c.armed.Add(1)
	return c.Clock.After(d)
}

// nsPair starts two responders and a checker whose nameserver names map to them.
func nsPair(t *testing.T) (*Checker, *countingClock, [2]*acmetest.DNSServer) {
	t.Helper()
	servers := [2]*acmetest.DNSServer{acmetest.StartDNSServer(t), acmetest.StartDNSServer(t)}
	clock := newClock()
	addrs := map[string]string{"ns1.example.test": servers[0].Addr(), "ns2.example.test": servers[1].Addr()}
	c := &Checker{
		Q:     NewQuerier(),
		Clock: clock,
		NameserverAddrs: func(_ context.Context, host string) ([]string, error) {
			return []string{addrs[host]}, nil
		},
	}
	return c, clock, servers
}

var twoNameservers = Authoritative([]string{"ns1.example.test", "ns2.example.test."})

func TestPropagationSeenOnEveryNameserver(t *testing.T) {
	c, clock, servers := nsPair(t)
	for _, s := range servers {
		s.AddTXT(testFQDN, testValue)
	}

	if err := c.WaitVisible(t.Context(), testFQDN, testValue, twoNameservers, PropagationTimeout); err != nil {
		t.Fatalf("WaitVisible: %v", err)
	}
	if n := clock.armed.Load(); n != 0 {
		t.Errorf("a record visible at the first look armed %d timers, want none", n)
	}
	for i, s := range servers {
		qs := s.Queries()
		if len(qs) != 1 {
			t.Fatalf("nameserver %d was asked %d times, want once", i+1, len(qs))
		}
		if qs[0].Recursive {
			t.Errorf("nameserver %d was asked with recursion on", i+1)
		}
		if qs[0].Type != mdns.TypeTXT || qs[0].Name != testFQDN {
			t.Errorf("nameserver %d was asked %+v, want a TXT question for %s", i+1, qs[0], testFQDN)
		}
	}
}
