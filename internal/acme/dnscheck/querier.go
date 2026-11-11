// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnscheck

import (
	"context"
	"fmt"
	"time"

	mdns "github.com/miekg/dns"
)

const (
	// exchangeTimeout is the I/O deadline of one question to one server.
	exchangeTimeout = 5 * time.Second
	// ednsBufferSize is the UDP payload size offered to servers. 1232 bytes avoids IP fragmentation.
	ednsBufferSize = 1232
)

// Querier sends one DNS question to one server and returns the reply.
type Querier interface {
	// Exchange asks server ("host:port", IPv6 hosts in brackets) for the records of qtype at name.
	// recursive sets the "recursion desired" bit: false asks an authoritative server for what it
	// holds itself, true asks a resolver to find the answer.
	Exchange(ctx context.Context, server, name string, qtype uint16, recursive bool) (*mdns.Msg, error)
}

// NewQuerier is the production Querier: UDP with a TCP retry when the reply is truncated.
func NewQuerier() Querier { return wireQuerier{} }

type wireQuerier struct{}

func (wireQuerier) Exchange(ctx context.Context, server, name string, qtype uint16, recursive bool) (*mdns.Msg, error) {
	m := new(mdns.Msg)
	m.SetQuestion(mdns.Fqdn(name), qtype)
	m.RecursionDesired = recursive
	m.SetEdns0(ednsBufferSize, false)

	udp := &mdns.Client{Net: "udp", Timeout: exchangeTimeout}
	r, _, err := udp.ExchangeContext(ctx, m, server)
	if err != nil {
		return nil, fmt.Errorf("ask %s over udp: %w", server, err)
	}
	if !r.Truncated {
		return r, nil
	}
	tcp := &mdns.Client{Net: "tcp", Timeout: exchangeTimeout}
	r, _, err = tcp.ExchangeContext(ctx, m, server)
	if err != nil {
		return nil, fmt.Errorf("ask %s over tcp: %w", server, err)
	}
	return r, nil
}
