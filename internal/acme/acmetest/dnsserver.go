// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acmetest

import (
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// DNSQuery is one question the responder received.
type DNSQuery struct {
	// Name is the name asked about, lowercase, without the trailing dot.
	Name string
	// Type is the record type asked for (miekg/dns TypeTXT, TypeCNAME, ...).
	Type uint16
	// Recursive is the "recursion desired" bit.
	Recursive bool
	// Net is "udp" or "tcp".
	Net string
}

// DNSServer is an in-process DNS responder on 127.0.0.1 (UDP and TCP on one port) that serves the
// TXT and CNAME records a test gives it. A query without recursion desired is answered as an
// authoritative nameserver would (AA set, one CNAME only); with recursion desired the answer
// carries the whole CNAME chain and the record at its end, as a resolver's does.
type DNSServer struct {
	addr string

	mu       sync.Mutex
	txt      map[string][]string
	cname    map[string]string
	held     map[string]bool
	truncate bool
	queries  []DNSQuery
}

// StartDNSServer starts a responder and stops it when the test ends.
func StartDNSServer(tb testing.TB) *DNSServer {
	tb.Helper()
	s := &DNSServer{txt: map[string][]string{}, cname: map[string]string{}, held: map[string]bool{}}

	var pc net.PacketConn
	var ln net.Listener
	for range 20 {
		var err error
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			tb.Fatalf("listen for the test DNS server over tcp: %v", err)
		}
		pc, err = net.ListenPacket("udp", ln.Addr().String())
		if err == nil {
			break
		}
		_ = ln.Close() // the udp port is taken: try another tcp port
		ln = nil
	}
	if ln == nil {
		tb.Fatalf("no free port for the test DNS server on both udp and tcp")
	}
	s.addr = ln.Addr().String()

	var started sync.WaitGroup
	started.Add(2)
	notify := func() { started.Done() }
	udp := &mdns.Server{PacketConn: pc, Handler: mdns.HandlerFunc(s.serve), NotifyStartedFunc: notify}
	tcp := &mdns.Server{Listener: ln, Handler: mdns.HandlerFunc(s.serve), NotifyStartedFunc: notify}
	done := make(chan struct{}, 2)
	go func() { _ = udp.ActivateAndServe(); done <- struct{}{} }()
	go func() { _ = tcp.ActivateAndServe(); done <- struct{}{} }()
	started.Wait()
	tb.Cleanup(func() {
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		testsupport.Await(tb, "the first test DNS listener to stop", done)
		testsupport.Await(tb, "the second test DNS listener to stop", done)
	})
	return s
}

// Addr is the "127.0.0.1:port" the responder listens on (UDP and TCP).
func (s *DNSServer) Addr() string { return s.addr }

// AddTXT serves value as a TXT record at fqdn.
func (s *DNSServer) AddTXT(fqdn, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := dnsKey(fqdn)
	s.txt[n] = append(s.txt[n], value)
}

// RemoveTXT stops serving value at fqdn.
func (s *DNSServer) RemoveTXT(fqdn, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := dnsKey(fqdn)
	s.txt[n] = slices.DeleteFunc(s.txt[n], func(v string) bool { return v == value })
	if len(s.txt[n]) == 0 {
		delete(s.txt, n)
	}
}

// SetCNAME serves a CNAME record from name to target.
func (s *DNSServer) SetCNAME(name, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cname[dnsKey(name)] = dnsKey(target)
}

// Hold makes fqdn answer as if its TXT records were not there yet.
func (s *DNSServer) Hold(fqdn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[dnsKey(fqdn)] = true
}

// Release ends Hold: the records held back are served again.
func (s *DNSServer) Release(fqdn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, dnsKey(fqdn))
}

// TruncateUDP makes every answer over UDP an empty truncated one, so a client has to ask again
// over TCP.
func (s *DNSServer) TruncateUDP(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncate = on
}

// Queries lists the questions received so far, in order.
func (s *DNSServer) Queries() []DNSQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries)
}

// dnsKey is the form names are kept in: lowercase, no trailing dot.
func dnsKey(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

const maxChain = 32

func (s *DNSServer) serve(w mdns.ResponseWriter, r *mdns.Msg) {
	resp := new(mdns.Msg)
	resp.SetReply(r)
	if len(r.Question) != 1 {
		resp.Rcode = mdns.RcodeFormatError
		_ = w.WriteMsg(resp)
		return
	}
	q := r.Question[0]
	name := dnsKey(q.Name)
	netw := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		netw = "tcp"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, DNSQuery{Name: name, Type: q.Qtype, Recursive: r.RecursionDesired, Net: netw})

	resp.Authoritative = !r.RecursionDesired
	resp.RecursionAvailable = r.RecursionDesired
	if s.truncate && netw == "udp" {
		resp.Truncated = true
		_ = w.WriteMsg(resp)
		return
	}
	cur := name
	for range maxChain {
		if target, ok := s.cname[cur]; ok {
			resp.Answer = append(resp.Answer, &mdns.CNAME{
				Hdr:    mdns.RR_Header{Name: mdns.Fqdn(cur), Rrtype: mdns.TypeCNAME, Class: mdns.ClassINET, Ttl: 30},
				Target: mdns.Fqdn(target),
			})
			if !r.RecursionDesired {
				break
			}
			cur = target
			continue
		}
		if q.Qtype == mdns.TypeTXT && !s.held[cur] {
			for _, v := range s.txt[cur] {
				resp.Answer = append(resp.Answer, &mdns.TXT{
					Hdr: mdns.RR_Header{Name: mdns.Fqdn(cur), Rrtype: mdns.TypeTXT, Class: mdns.ClassINET, Ttl: 30},
					Txt: txtChunks(v),
				})
			}
		}
		break
	}
	_ = w.WriteMsg(resp)
}

// txtChunks splits a value into the 255-byte strings a TXT record holds.
func txtChunks(v string) []string {
	var out []string
	for len(v) > 255 {
		out = append(out, v[:255])
		v = v[255:]
	}
	return append(out, v)
}
