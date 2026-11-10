// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package dnscheck asks DNS servers whether a TXT record is visible, so a DNS-01 order waits for the
// record before it asks the CA to look (Phase 15, D-11 to D-14).
//
// By default the questions go to the zone's own authoritative nameservers, never to the system
// resolver: a split-horizon answer on the LAN cannot make a record look visible to Let's Encrypt.
// An admin's resolver list replaces the nameservers entirely. Every wait runs on the injected
// clock, so a test moves time itself.
//
// The package imports nothing from internal/acme: the solver turns its errors into classified
// values.
package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/internal/stackops"
)

const (
	// PropagationInterval is the wait between two rounds of questions.
	PropagationInterval = 2 * time.Second
	// PropagationTimeout is how long an order waits for a record to show.
	PropagationTimeout = 2 * time.Minute
	// TestObserveLimit is how long the Test token info row waits for a record to show.
	TestObserveLimit = 20 * time.Second
	// MaxCNAMEHops is the longest _acme-challenge CNAME chain that is followed.
	MaxCNAMEHops = 8

	defaultPort = "53"
)

// ErrNoAnswer is returned by FollowCNAME when no server answered the first question, so the caller
// can fall back to other servers.
var ErrNoAnswer = errors.New("no server answered")

// NotVisibleError says the record was not visible in time. It names the servers that had not shown
// it, in the order they were asked, and never an answer's content.
type NotVisibleError struct {
	Servers []string
}

func (e *NotVisibleError) Error() string {
	if len(e.Servers) == 0 {
		return "the TXT record is not visible: there is no DNS server to ask"
	}
	return "the TXT record is not visible at " + strings.Join(e.Servers, ", ")
}

// DelegationError says an _acme-challenge CNAME chain cannot be followed: it loops, is longer than
// MaxCNAMEHops, or points at something that is not a DNS name. Target is the name it stopped at.
type DelegationError struct {
	Target string
}

func (e *DelegationError) Error() string {
	return "the _acme-challenge CNAME chain cannot be followed (stopped at " + e.Target + ")"
}

// Server is one DNS server a question is sent to.
type Server struct {
	// Name is what an error shows: the nameserver host or the resolver entry.
	Name string
	// Host is a DNS name or an IP literal.
	Host string
	// Port is empty for a nameserver (port 53 at every address NameserverAddrs returns).
	Port string
	// Recursive asks the server to find the answer itself (a resolver) rather than answer from what
	// it holds (an authoritative nameserver).
	Recursive bool
}

// Authoritative are the zone's own nameservers, asked without recursion.
func Authoritative(nameservers []string) []Server {
	out := make([]Server, 0, len(nameservers))
	for _, ns := range nameservers {
		ns = canonical(ns)
		if ns == "" {
			continue
		}
		out = append(out, Server{Name: ns, Host: ns})
	}
	return out
}

// Resolvers are the admin's resolvers, asked recursively. entries are already normalised by
// ParseResolvers ("host:port", IPv6 hosts in brackets).
func Resolvers(entries []string) []Server {
	out := make([]Server, 0, len(entries))
	for _, e := range entries {
		host, port, err := net.SplitHostPort(e)
		if err != nil {
			host, port = strings.Trim(e, "[]"), defaultPort
		}
		out = append(out, Server{Name: e, Host: host, Port: port, Recursive: true})
	}
	return out
}

// Checker asks servers for TXT records. Q and Clock are required; the rest default to the system.
type Checker struct {
	Q     Querier
	Clock stackops.Clock
	// LookupIP resolves a server's host name. Nil means the system resolver (it resolves the public
	// names of nameservers, not the zone's data).
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	// NameserverAddrs maps a nameserver host name to the "ip:port" addresses to ask, in order. Nil
	// means LookupIP with port 53, IPv4 first. A test uses it to point names at local responders.
	NameserverAddrs func(ctx context.Context, host string) ([]string, error)
	// ResolvConf lists the system resolvers. Nil means /etc/resolv.conf.
	ResolvConf func() ([]string, error)
}

// WaitVisible returns nil once every server shows value at fqdn. It asks first, then waits
// PropagationInterval on the clock, and at timeout returns a *NotVisibleError naming the servers
// that had not shown it. With no servers it returns that error at once.
func (c *Checker) WaitVisible(ctx context.Context, fqdn, value string, servers []Server, timeout time.Duration) error {
	if len(servers) == 0 {
		return &NotVisibleError{}
	}
	deadline := c.Clock.Now().Add(timeout)
	for {
		missing := c.round(ctx, fqdn, value, servers)
		if len(missing) == 0 {
			return nil
		}
		if !c.Clock.Now().Before(deadline) {
			return &NotVisibleError{Servers: missing}
		}
		if err := c.pause(ctx); err != nil {
			return err
		}
	}
}

// pause waits PropagationInterval on the clock, or until ctx ends.
func (c *Checker) pause(ctx context.Context) error {
	wait := c.Clock.After(PropagationInterval)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
		return nil
	}
}

// round asks every server once and returns the names of those that did not show the value, in the
// order they were asked.
func (c *Checker) round(ctx context.Context, fqdn, value string, servers []Server) []string {
	var missing []string
	for _, s := range servers {
		msg, err := c.ask(ctx, s, fqdn, mdns.TypeTXT)
		if err != nil || !hasTXT(msg, fqdn, value, s.Recursive) {
			missing = append(missing, s.Name)
		}
	}
	return missing
}

// ask sends one question to a server, trying its addresses in order until one answers.
func (c *Checker) ask(ctx context.Context, s Server, name string, qtype uint16) (*mdns.Msg, error) {
	addrs, err := c.addrs(ctx, s)
	if err != nil {
		return nil, err
	}
	var last error
	for _, addr := range addrs {
		msg, err := c.Q.Exchange(ctx, addr, name, qtype, s.Recursive)
		if err != nil {
			last = err
			continue
		}
		if msg.Rcode != mdns.RcodeSuccess && msg.Rcode != mdns.RcodeNameError {
			last = fmt.Errorf("%s answered %s", addr, mdns.RcodeToString[msg.Rcode])
			continue
		}
		return msg, nil
	}
	if last == nil {
		last = fmt.Errorf("%s has no address", s.Name)
	}
	return nil, last
}

// addrs are the "ip:port" addresses of a server.
func (c *Checker) addrs(ctx context.Context, s Server) ([]string, error) {
	if s.Port == "" {
		if c.NameserverAddrs != nil {
			return c.NameserverAddrs(ctx, s.Host)
		}
		return c.hostAddrs(ctx, s.Host, defaultPort)
	}
	return c.hostAddrs(ctx, s.Host, s.Port)
}

// hostAddrs resolves host and joins each address with port, IPv4 first.
func (c *Checker) hostAddrs(ctx context.Context, host, port string) ([]string, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []string{net.JoinHostPort(ip.String(), port)}, nil
	}
	lookup := c.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("find the address of %s: %w", host, err)
	}
	ips = slices.Clone(ips)
	slices.SortStableFunc(ips, func(a, b netip.Addr) int {
		switch {
		case a.Is4() && !b.Is4():
			return -1
		case !a.Is4() && b.Is4():
			return 1
		}
		return 0
	})
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip.Unmap().String(), port))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	return out, nil
}

// hasTXT reports whether the reply holds a TXT record with exactly value. A resolver's answer may
// carry the record under the name a CNAME chain ended at, so only a recursive answer is accepted
// under another owner name.
func hasTXT(msg *mdns.Msg, fqdn, value string, recursive bool) bool {
	for _, rr := range msg.Answer {
		txt, ok := rr.(*mdns.TXT)
		if !ok {
			continue
		}
		if !recursive && canonical(txt.Hdr.Name) != canonical(fqdn) {
			continue
		}
		if strings.Join(txt.Txt, "") == value {
			return true
		}
	}
	return false
}

// canonical lowercases a DNS name and drops the trailing dot.
func canonical(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// Observation is what Observe saw.
type Observation struct {
	// Seen is true when every server showed the record.
	Seen bool
	// Servers are the servers asked when Seen, otherwise the ones that had not shown the record.
	Servers []string
	// After is the clock time that passed before the verdict.
	After time.Duration
}

// Observe is WaitVisible for information only: it reports whether, and after how long, every
// server showed the record within limit, and never returns an error.
func (c *Checker) Observe(ctx context.Context, fqdn, value string, servers []Server, limit time.Duration) Observation {
	for range 10 {
		_ = c.Clock.After(0) // declaration only: the RED commit's stand-in
	}
	return Observation{}
}

// FollowCNAME follows the _acme-challenge CNAME chain at fqdn.
func (c *Checker) FollowCNAME(ctx context.Context, fqdn string, servers []Server) (final string, delegated bool, err error) {
	return "", false, nil
}

// SystemServers are the system resolvers.
func (c *Checker) SystemServers() ([]Server, error) { return nil, nil }
