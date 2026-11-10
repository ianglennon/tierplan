// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

// resolverServers are custom resolvers at the responders' addresses.
func resolverServers(servers ...*acmetest.DNSServer) []Server {
	var entries []string
	for _, s := range servers {
		entries = append(entries, s.Addr())
	}
	return Resolvers(entries)
}

// result runs fn on its own goroutine and delivers what it returns.
func result[T any](fn func() T) <-chan T {
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	return ch
}

func TestPropagationCustomResolversReplaceTheDefault(t *testing.T) {
	c, _, nameservers := nsPair(t)
	resolver := acmetest.StartDNSServer(t)
	resolver.AddTXT(testFQDN, testValue) // only the resolver holds the record

	if err := c.WaitVisible(t.Context(), testFQDN, testValue, resolverServers(resolver), PropagationTimeout); err != nil {
		t.Fatalf("WaitVisible through the custom resolver: %v", err)
	}
	for i, ns := range nameservers {
		if qs := ns.Queries(); len(qs) != 0 {
			t.Errorf("nameserver %d was asked %d times although custom resolvers are set", i+1, len(qs))
		}
	}
	qs := resolver.Queries()
	if len(qs) != 1 || !qs[0].Recursive {
		t.Errorf("the custom resolver was asked %+v, want one question with recursion on", qs)
	}
}

func TestPropagationNeverUsesTheSystemResolverForTheVerdict(t *testing.T) {
	c, _, servers := nsPair(t)
	for _, s := range servers {
		s.AddTXT(testFQDN, testValue)
	}
	c.ResolvConf = func() ([]string, error) {
		t.Error("the system resolver list was read to judge propagation")
		return nil, nil
	}
	if err := c.WaitVisible(t.Context(), testFQDN, testValue, twoNameservers, PropagationTimeout); err != nil {
		t.Fatalf("WaitVisible: %v", err)
	}
}

func TestPropagationNeedsEveryNameserver(t *testing.T) {
	c, clock, servers := nsPair(t)
	for _, s := range servers {
		s.AddTXT(testFQDN, testValue)
	}
	servers[1].Hold(testFQDN)

	done := result(func() error {
		return c.WaitVisible(t.Context(), testFQDN, testValue, twoNameservers, PropagationTimeout)
	})
	clock.WaitArmed(t) // the first look found one nameserver without the record, so the check waits
	servers[1].Release(testFQDN)
	clock.Advance(PropagationInterval)

	if err := testsupport.Await(t, "WaitVisible to see the record on both nameservers", done); err != nil {
		t.Fatalf("WaitVisible after the record appeared on the second nameserver: %v", err)
	}
}

func TestPropagationTimeoutNamesTheResolvers(t *testing.T) {
	c, clock, _ := nsPair(t)
	first, second, third := acmetest.StartDNSServer(t), acmetest.StartDNSServer(t), acmetest.StartDNSServer(t)
	second.AddTXT(testFQDN, testValue)
	servers := resolverServers(first, second, third)

	done := result(func() error {
		return c.WaitVisible(t.Context(), testFQDN, testValue, servers, PropagationTimeout)
	})
	for range int(PropagationTimeout / PropagationInterval) {
		clock.Tick(t, PropagationInterval)
	}

	err := testsupport.Await(t, "WaitVisible to give up", done)
	var nv *NotVisibleError
	if !errors.As(err, &nv) {
		t.Fatalf("WaitVisible = %v, want a *NotVisibleError", err)
	}
	want := []string{first.Addr(), third.Addr()}
	if !slices.Equal(nv.Servers, want) {
		t.Errorf("the error names %v, want %v in the order asked", nv.Servers, want)
	}
	if !strings.Contains(nv.Error(), first.Addr()) {
		t.Errorf("the error text %q does not name %s", nv.Error(), first.Addr())
	}
}

func TestPropagationContextEndsTheWait(t *testing.T) {
	c, clock, _ := nsPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := result(func() error {
		return c.WaitVisible(ctx, testFQDN, testValue, twoNameservers, PropagationTimeout)
	})
	clock.WaitArmed(t)
	cancel()
	if err := testsupport.Await(t, "WaitVisible to stop", done); !errors.Is(err, context.Canceled) {
		t.Errorf("WaitVisible after cancel = %v, want context.Canceled", err)
	}
}

func TestPropagationTruncatedUDPIsRetriedOverTCP(t *testing.T) {
	c, _, servers := nsPair(t)
	for _, s := range servers {
		s.AddTXT(testFQDN, testValue)
		s.TruncateUDP(true)
	}
	if err := c.WaitVisible(t.Context(), testFQDN, testValue, twoNameservers, PropagationTimeout); err != nil {
		t.Fatalf("WaitVisible with truncated UDP answers: %v", err)
	}
	var tcp int
	for _, q := range servers[0].Queries() {
		if q.Net == "tcp" {
			tcp++
		}
	}
	if tcp != 1 {
		t.Errorf("the first nameserver saw %d questions over tcp, want the one retry", tcp)
	}
}

func TestPropagationWithNoServerFailsAtOnce(t *testing.T) {
	c, clock, _ := nsPair(t)
	err := c.WaitVisible(t.Context(), testFQDN, testValue, nil, PropagationTimeout)
	var nv *NotVisibleError
	if !errors.As(err, &nv) || len(nv.Servers) != 0 {
		t.Fatalf("WaitVisible with no servers = %v, want an empty *NotVisibleError", err)
	}
	if n := clock.armed.Load(); n != 0 {
		t.Errorf("an empty server list armed %d timers, want none", n)
	}
}

// answers is a Querier that replies per server address, for the cases that need a server to fail,
// refuse or say something a responder would not.
type answers map[string]func(name string, qtype uint16) (*mdns.Msg, error)

func (a answers) Exchange(_ context.Context, server, name string, qtype uint16, _ bool) (*mdns.Msg, error) {
	fn, ok := a[server]
	if !ok {
		return nil, errors.New("no route to " + server)
	}
	return fn(name, qtype)
}

func reply(rrs ...mdns.RR) *mdns.Msg {
	m := new(mdns.Msg)
	m.Rcode = mdns.RcodeSuccess
	m.Answer = rrs
	return m
}

func txtRR(owner string, parts ...string) mdns.RR {
	return &mdns.TXT{Hdr: mdns.RR_Header{Name: owner, Rrtype: mdns.TypeTXT, Class: mdns.ClassINET}, Txt: parts}
}

func cnameRR(owner, target string) mdns.RR {
	return &mdns.CNAME{Hdr: mdns.RR_Header{Name: owner, Rrtype: mdns.TypeCNAME, Class: mdns.ClassINET}, Target: target}
}

func ipServer(name, addr string) Server {
	return Server{Name: name, Host: addr, Port: "53"}
}

func TestPropagationComparesTheValueByteForByte(t *testing.T) {
	cases := []struct {
		name string
		rrs  []mdns.RR
		want bool
	}{
		{"exact", []mdns.RR{txtRR(testFQDN+".", testValue)}, true},
		{"owner in another case and with a dot", []mdns.RR{txtRR("_ACME-Challenge.App.Example.Test.", testValue)}, true},
		{"value split over several strings", []mdns.RR{txtRR(testFQDN+".", testValue[:5], testValue[5:])}, true},
		{"value in another case", []mdns.RR{txtRR(testFQDN+".", strings.ToUpper(testValue))}, false},
		{"value with a trailing space", []mdns.RR{txtRR(testFQDN+".", testValue+" ")}, false},
		{"another record beside the right one", []mdns.RR{txtRR(testFQDN+".", "other"), txtRR(testFQDN+".", testValue)}, true},
		{"the right value at another name", []mdns.RR{txtRR("elsewhere.example.test.", testValue)}, false},
		{"no record", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			c := &Checker{Clock: clock, Q: answers{"192.0.2.1:53": func(string, uint16) (*mdns.Msg, error) {
				return reply(tc.rrs...), nil
			}}}
			// A timeout of one interval: one look, one wait, one more look.
			done := result(func() error {
				return c.WaitVisible(t.Context(), testFQDN, testValue, []Server{ipServer("ns", "192.0.2.1")}, PropagationInterval)
			})
			if !tc.want {
				clock.Tick(t, PropagationInterval)
			}
			err := testsupport.Await(t, "WaitVisible to finish", done)
			if (err == nil) != tc.want {
				t.Errorf("WaitVisible = %v, want visible %v", err, tc.want)
			}
		})
	}
}

func TestPropagationAnUnreachableServerIsNotVisible(t *testing.T) {
	clock := newClock()
	c := &Checker{Clock: clock, Q: answers{
		"192.0.2.1:53": func(string, uint16) (*mdns.Msg, error) { return reply(txtRR(testFQDN+".", testValue)), nil },
	}}
	servers := []Server{ipServer("up", "192.0.2.1"), ipServer("down", "192.0.2.2")}
	done := result(func() error {
		return c.WaitVisible(t.Context(), testFQDN, testValue, servers, PropagationInterval)
	})
	clock.Tick(t, PropagationInterval)
	var nv *NotVisibleError
	if err := testsupport.Await(t, "WaitVisible to finish", done); !errors.As(err, &nv) || !slices.Equal(nv.Servers, []string{"down"}) {
		t.Errorf("WaitVisible = %v, want a *NotVisibleError naming only down", err)
	}
}

func TestPropagationARefusingServerIsNotVisible(t *testing.T) {
	refused := func(string, uint16) (*mdns.Msg, error) {
		m := reply()
		m.Rcode = mdns.RcodeRefused
		return m, nil
	}
	clock := newClock()
	c := &Checker{Clock: clock, Q: answers{"192.0.2.1:53": refused}}
	done := result(func() error {
		return c.WaitVisible(t.Context(), testFQDN, testValue, []Server{ipServer("ns", "192.0.2.1")}, PropagationInterval)
	})
	clock.Tick(t, PropagationInterval)
	var nv *NotVisibleError
	if err := testsupport.Await(t, "WaitVisible to finish", done); !errors.As(err, &nv) {
		t.Errorf("WaitVisible = %v, want a *NotVisibleError", err)
	}
}

func TestObserveReportsSeenAfter(t *testing.T) {
	c, clock, servers := nsPair(t)
	for _, s := range servers {
		s.AddTXT(testFQDN, testValue)
	}
	servers[1].Hold(testFQDN)

	done := result(func() Observation {
		return c.Observe(t.Context(), testFQDN, testValue, twoNameservers, TestObserveLimit)
	})
	clock.WaitArmed(t)
	servers[1].Release(testFQDN)
	clock.Advance(2 * PropagationInterval)

	got := testsupport.Await(t, "Observe to see the record", done)
	want := Observation{Seen: true, Servers: []string{"ns1.example.test", "ns2.example.test"}, After: 2 * PropagationInterval}
	if !got.Seen || got.After != want.After || !slices.Equal(got.Servers, want.Servers) {
		t.Errorf("Observe = %+v, want %+v", got, want)
	}
}

func TestObserveReportsNotSeen(t *testing.T) {
	c, clock, servers := nsPair(t)
	servers[0].AddTXT(testFQDN, testValue)

	done := result(func() Observation {
		return c.Observe(t.Context(), testFQDN, testValue, twoNameservers, TestObserveLimit)
	})
	for range int(TestObserveLimit / PropagationInterval) {
		clock.Tick(t, PropagationInterval)
	}

	got := testsupport.Await(t, "Observe to give up", done)
	if got.Seen || got.After != TestObserveLimit || !slices.Equal(got.Servers, []string{"ns2.example.test"}) {
		t.Errorf("Observe = %+v, want not seen after %s, naming ns2.example.test", got, TestObserveLimit)
	}
}

func TestObserveNeverReturnsAnErrorForAnUnreachableServer(t *testing.T) {
	clock := newClock()
	c := &Checker{Clock: clock, Q: answers{}}
	done := result(func() Observation {
		return c.Observe(t.Context(), testFQDN, testValue, []Server{ipServer("down", "192.0.2.9")}, PropagationInterval)
	})
	clock.Tick(t, PropagationInterval)
	if got := testsupport.Await(t, "Observe to finish", done); got.Seen || !slices.Equal(got.Servers, []string{"down"}) {
		t.Errorf("Observe = %+v, want not seen, naming down", got)
	}
}

// chain serves CNAME records from names[i] to names[i+1] on one responder.
func chain(s *acmetest.DNSServer, names ...string) {
	for i := 0; i+1 < len(names); i++ {
		s.SetCNAME(names[i], names[i+1])
	}
}

func TestCNAMEChainAndLoops(t *testing.T) {
	hops := func(n int) []string {
		var out []string
		for i := 0; i <= n; i++ {
			out = append(out, fmt.Sprintf("n%d.example.test", i))
		}
		return out
	}
	ns := func(t *testing.T) (*Checker, *acmetest.DNSServer, []Server) {
		c, _, servers := nsPair(t)
		return c, servers[0], Authoritative([]string{"ns1.example.test"})
	}

	t.Run("no CNAME leaves the name as it is", func(t *testing.T) {
		c, _, servers := ns(t)
		final, delegated, err := c.FollowCNAME(t.Context(), "_ACME-Challenge.App.Example.Test.", servers)
		if err != nil || delegated || final != testFQDN {
			t.Errorf("FollowCNAME = %q, %v, %v; want %q, false, nil", final, delegated, err, testFQDN)
		}
	})
	t.Run("one hop", func(t *testing.T) {
		c, s, servers := ns(t)
		s.SetCNAME(testFQDN, "_acme-challenge.Target.Example.Test.")
		final, delegated, err := c.FollowCNAME(t.Context(), testFQDN, servers)
		if err != nil || !delegated || final != "_acme-challenge.target.example.test" {
			t.Errorf("FollowCNAME = %q, %v, %v", final, delegated, err)
		}
	})
	t.Run("a chain inside one recursive answer", func(t *testing.T) {
		c, s, _ := ns(t)
		names := append([]string{testFQDN}, hops(3)...)
		chain(s, names...)
		final, delegated, err := c.FollowCNAME(t.Context(), testFQDN, resolverServers(s))
		if err != nil || !delegated || final != "n3.example.test" {
			t.Errorf("FollowCNAME = %q, %v, %v", final, delegated, err)
		}
	})
	t.Run("eight hops are followed", func(t *testing.T) {
		c, s, servers := ns(t)
		chain(s, append([]string{testFQDN}, hops(7)...)...) // fqdn -> n0 ... n7: eight records
		final, delegated, err := c.FollowCNAME(t.Context(), testFQDN, servers)
		if err != nil || !delegated || final != "n7.example.test" {
			t.Errorf("FollowCNAME = %q, %v, %v; want n7.example.test", final, delegated, err)
		}
	})
	t.Run("a ninth hop is refused", func(t *testing.T) {
		c, s, servers := ns(t)
		chain(s, append([]string{testFQDN}, hops(8)...)...) // nine records
		_, _, err := c.FollowCNAME(t.Context(), testFQDN, servers)
		var de *DelegationError
		if !errors.As(err, &de) || de.Target != "n8.example.test" {
			t.Errorf("FollowCNAME = %v, want a *DelegationError for n8.example.test", err)
		}
	})
	t.Run("a CNAME that points at itself", func(t *testing.T) {
		c, s, servers := ns(t)
		s.SetCNAME(testFQDN, testFQDN)
		_, _, err := c.FollowCNAME(t.Context(), testFQDN, servers)
		var de *DelegationError
		if !errors.As(err, &de) || de.Target != testFQDN {
			t.Errorf("FollowCNAME = %v, want a *DelegationError for %s", err, testFQDN)
		}
	})
	t.Run("a loop of two names", func(t *testing.T) {
		c, s, servers := ns(t)
		s.SetCNAME(testFQDN, "a.example.test")
		s.SetCNAME("a.example.test", testFQDN)
		_, _, err := c.FollowCNAME(t.Context(), testFQDN, servers)
		var de *DelegationError
		if !errors.As(err, &de) || de.Target != testFQDN {
			t.Errorf("FollowCNAME = %v, want a *DelegationError for %s", err, testFQDN)
		}
	})
	t.Run("a loop inside one recursive answer", func(t *testing.T) {
		c, s, _ := ns(t)
		s.SetCNAME(testFQDN, "a.example.test")
		s.SetCNAME("a.example.test", testFQDN)
		_, _, err := c.FollowCNAME(t.Context(), testFQDN, resolverServers(s))
		var de *DelegationError
		if !errors.As(err, &de) {
			t.Errorf("FollowCNAME = %v, want a *DelegationError", err)
		}
	})
	t.Run("a target that is not a DNS name", func(t *testing.T) {
		c := &Checker{Clock: newClock(), Q: answers{"192.0.2.1:53": func(string, uint16) (*mdns.Msg, error) {
			return reply(cnameRR(testFQDN+".", "a..b.example.test.")), nil
		}}}
		_, _, err := c.FollowCNAME(t.Context(), testFQDN, []Server{ipServer("ns", "192.0.2.1")})
		var de *DelegationError
		if !errors.As(err, &de) {
			t.Errorf("FollowCNAME = %v, want a *DelegationError", err)
		}
	})
	t.Run("no server answering the first hop", func(t *testing.T) {
		c := &Checker{Clock: newClock(), Q: answers{}}
		_, _, err := c.FollowCNAME(t.Context(), testFQDN, []Server{ipServer("a", "192.0.2.1"), ipServer("b", "192.0.2.2")})
		if !errors.Is(err, ErrNoAnswer) {
			t.Errorf("FollowCNAME = %v, want ErrNoAnswer", err)
		}
	})
	t.Run("the first server that answers wins", func(t *testing.T) {
		c := &Checker{Clock: newClock(), Q: answers{"192.0.2.2:53": func(string, uint16) (*mdns.Msg, error) {
			return reply(cnameRR(testFQDN+".", "t.example.test.")), nil
		}}}
		final, delegated, err := c.FollowCNAME(t.Context(), testFQDN, []Server{ipServer("down", "192.0.2.1"), ipServer("up", "192.0.2.2")})
		if err != nil || !delegated || final != "t.example.test" {
			t.Errorf("FollowCNAME = %q, %v, %v", final, delegated, err)
		}
	})
	t.Run("a server that cannot answer a later hop ends the chain there", func(t *testing.T) {
		c := &Checker{Clock: newClock(), Q: answers{"192.0.2.1:53": func(name string, _ uint16) (*mdns.Msg, error) {
			if name == testFQDN {
				return reply(cnameRR(testFQDN+".", "t.example.test.")), nil
			}
			m := reply()
			m.Rcode = mdns.RcodeRefused
			return m, nil
		}}}
		final, delegated, err := c.FollowCNAME(t.Context(), testFQDN, []Server{ipServer("ns", "192.0.2.1")})
		if err != nil || !delegated || final != "t.example.test" {
			t.Errorf("FollowCNAME = %q, %v, %v", final, delegated, err)
		}
	})
}

func TestSystemServersAreTheResolvConfEntries(t *testing.T) {
	c := &Checker{ResolvConf: func() ([]string, error) { return []string{"10.0.0.1:53", "[fd00::1]:5353"}, nil }}
	got, err := c.SystemServers()
	if err != nil || len(got) != 2 {
		t.Fatalf("SystemServers = %+v, %v", got, err)
	}
	for _, s := range got {
		if !s.Recursive {
			t.Errorf("system server %q is not asked recursively", s.Name)
		}
	}
	if got[1].Name != "[fd00::1]:5353" || got[1].Host != "fd00::1" || got[1].Port != "5353" {
		t.Errorf("second system server = %+v", got[1])
	}
}

func TestSystemServersRefusesAnEmptyList(t *testing.T) {
	c := &Checker{ResolvConf: func() ([]string, error) { return nil, nil }}
	if got, err := c.SystemServers(); err == nil {
		t.Errorf("SystemServers with no resolvers = %+v, nil; want an error", got)
	}
}
