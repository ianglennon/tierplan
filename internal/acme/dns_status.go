// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// DNSView is the DNS-01 settings as the page shows them. The token is never in it: TokenSaved says only
// whether one is saved (CHAL-01, D-03).
type DNSView struct {
	Provider   string
	TokenSaved bool
	// Resolvers are the normalised resolver list as saved; empty, never nil, when the system's are used.
	Resolvers []string
	// LeftOver lists every TXT record TierPlan created and could not yet delete, oldest first (D-18).
	LeftOver []LeftOverView
}

// LeftOverView is one TXT record still at the DNS provider: what it is called, the provider's ID for it,
// the class of its last delete error and when it was tried and will be tried again.
type LeftOverView struct {
	Name, RecordID, Class string
	CreatedAt             time.Time
	LastTryAt, NextTryAt  time.Time
	// RetryPaused is true while no delete is tried, and PausedReason says why: PausedRestore or
	// PausedCertFiles (D-23). NextTryAt is then zero: no try is promised.
	RetryPaused  bool
	PausedReason string
}

// dnsView reads the DNS settings and the record ledger for the status. paused is why deletes are not
// being tried ("" when they are).
func (m *Manager) dnsView(ctx context.Context, st State, paused string) (DNSView, error) {
	provider := st.DNSProvider
	if provider == "" {
		provider = defaultDNSProvider
	}
	saved, err := m.d.Store.DNSTokenSaved(ctx, provider)
	if err != nil {
		return DNSView{}, err
	}
	rows, err := m.d.Store.DNSRecords(ctx)
	if err != nil {
		return DNSView{}, err
	}
	out := DNSView{
		Provider: provider, TokenSaved: saved,
		Resolvers: slices.Clone(st.DNSResolvers), LeftOver: make([]LeftOverView, 0, len(rows)),
	}
	if out.Resolvers == nil {
		out.Resolvers = []string{}
	}
	for _, r := range rows {
		v := LeftOverView{
			Name: r.Name, RecordID: r.RecordID, Class: r.LastErrorClass,
			CreatedAt: r.CreatedAt, LastTryAt: r.LastTryAt, NextTryAt: r.NextTryAt,
			RetryPaused: paused != "", PausedReason: paused,
		}
		if paused != "" {
			v.NextTryAt = time.Time{}
		}
		out.LeftOver = append(out.LeftOver, v)
	}
	return out, nil
}

// Suggestion says the public host resolves, as this server sees it, to an address Let's Encrypt can
// hardly reach, so DNS-01 may suit the install better (D-02).
type Suggestion struct {
	Host, Address string
}

const (
	// suggestLookupTimeout bounds the one DNS lookup a status read can make.
	suggestLookupTimeout = 2 * time.Second
	// suggestCacheTTL is how long an answer, a failure included, is kept: the page polls the status
	// every minute and must stay cheap.
	suggestCacheTTL = 5 * time.Minute
)

// cgnat is 100.64.0.0/10, the shared address space of RFC 6598, which IsPrivate does not cover.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isPrivateLike reports whether a CA could not be expected to reach addr: a private, loopback or
// link-local address, or one in the carrier-grade NAT range.
func isPrivateLike(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || cgnat.Contains(addr)
}

// suggestCache is the last lookup the suggestion made. It has its own lock, so a status read never
// waits on the manager's while a resolver answers.
type suggestCache struct {
	mu        sync.Mutex
	host      string
	answer    *Suggestion
	checkedAt time.Time
	set       bool
}

func (c *suggestCache) get(host string, now time.Time) (*Suggestion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.set || c.host != host || now.Sub(c.checkedAt) >= suggestCacheTTL {
		return nil, false
	}
	return c.answer, true
}

func (c *suggestCache) put(host string, answer *Suggestion, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.host, c.answer, c.checkedAt, c.set = host, answer, now, true
}

// suggestDNS01 returns the DNS-01 suggestion for host, or nil. It is made only when HTTP-01 is the saved
// challenge, host is a name (not an IP address) and the directory is not overridden for a test. The
// name is looked up with the system resolver, as this server sees it, under a 2 s deadline, and the
// answer (or the failure to get one) is kept for 5 minutes on the manager's clock.
func (m *Manager) suggestDNS01(ctx context.Context, st State, host string, overridden bool) *Suggestion {
	if overridden || challengeOf(st) != ChallengeHTTP01 || host == "" || net.ParseIP(host) != nil {
		return nil
	}
	now := m.clock.Now()
	if answer, ok := m.suggest.get(host, now); ok {
		return answer
	}
	lookup := m.d.DNS.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	lctx, cancel := context.WithTimeout(ctx, suggestLookupTimeout)
	defer cancel()
	var answer *Suggestion
	if addrs, err := lookup(lctx, host); err == nil {
		for _, a := range addrs {
			if isPrivateLike(a) {
				answer = &Suggestion{Host: host, Address: a.Unmap().String()}
				break
			}
		}
	}
	m.suggest.put(host, answer, now)
	return answer
}
