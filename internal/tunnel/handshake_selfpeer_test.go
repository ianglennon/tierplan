// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel_test

import (
	"context"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// countingVerifier counts the enrolments and hellos the hub asks the real verifier about, so a test can
// tell whether a handshake reached verification at all.
type countingVerifier struct {
	tunnel.Verifier
	calls atomic.Int32
}

func (v *countingVerifier) Enrol(ctx context.Context, req tunnel.EnrolRequest) (tunnel.EnrolResult, error) {
	v.calls.Add(1)
	return v.Verifier.Enrol(ctx, req)
}

func (v *countingVerifier) Hello(ctx context.Context, req tunnel.HelloRequest) (tunnel.HelloResult, error) {
	v.calls.Add(1)
	return v.Verifier.Hello(ctx, req)
}

// selfNet is the live trusted set: TierPlan's own self network.
var selfNet = []netip.Prefix{netip.MustParsePrefix("10.9.0.0/24")}

// fromPeer puts the hub behind the client-address middleware with the static list given and selfNet as the
// live set, and makes every connection look as if its TCP peer were peer, as it would be on the overlay.
func fromPeer(peer string, static []netip.Prefix) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		inner := httpx.ClientIPMiddlewareLive(static, func() []netip.Prefix { return selfNet })(h)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = peer
			inner.ServeHTTP(w, r)
		})
	}
}

// A connect relayed by TierPlan's own ingress proxy comes from the self network. It is refused with its own
// code before verification, so a peer the live set trusts never skips the channel binding (SELF-05, A-05).
func TestAgentHandshakeRefusesASelfNetworkPeer(t *testing.T) {
	log, rec := newRecordingLogger()
	v := &countingVerifier{}
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap: fromPeer("10.9.0.5:40000", nil),
		Log:  log,
		Verifier: func(real tunnel.Verifier) tunnel.Verifier {
			v.Verifier = real
			return v
		},
	})
	env := srv.NewEnvironment(t, "Production")

	for name, a := range map[string]tunneltest.Agent{
		"an enrolment bound to its TLS session": {Token: env.Token},
		"an enrolment with no exporter value":   {Token: env.Token, NoEKM: true},
	} {
		c := srv.Dial(t, a)
		if c.Welcome != nil {
			t.Fatalf("%s: a peer on the self network was welcomed", name)
		}
		if c.Refuse == nil || c.Refuse.Code != wire.ReasonSelfNetworkPeer {
			t.Fatalf("%s: refusal = %+v (err %v), want %s", name, c.Refuse, c.Err, wire.ReasonSelfNetworkPeer)
		}
		if c.Refuse.Detail == "" {
			t.Errorf("%s: the refusal says nothing about why", name)
		}
	}
	if n := v.calls.Load(); n != 0 {
		t.Fatalf("the verifier was asked %d times, want never", n)
	}
	got := rec.containing("agent handshake refused")
	if len(got) != 2 {
		t.Fatalf("%d \"agent handshake refused\" records, want 2", len(got))
	}
	for _, r := range got {
		if r.attrs["code"] != wire.ReasonSelfNetworkPeer || r.attrs["ip"] != "10.9.0.5" {
			t.Errorf("log record %+v, want code %s and ip 10.9.0.5", r.attrs, wire.ReasonSelfNetworkPeer)
		}
	}
}

// A peer trusted only through TIERPLAN_TRUSTED_PROXIES, an operator's own TLS terminator, is not on the self
// network: it is verified, and its exporter value left uncompared, exactly as before.
func TestHandshakeTrustedProxyPathUnchanged(t *testing.T) {
	v := &countingVerifier{}
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap: fromPeer("10.8.0.5:40000", []netip.Prefix{netip.MustParsePrefix("10.8.0.0/24")}),
		Verifier: func(real tunnel.Verifier) tunnel.Verifier {
			v.Verifier = real
			return v
		},
	})
	env := srv.NewEnvironment(t, "Production")

	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, NoEKM: true})
	if c.Welcome == nil {
		t.Fatalf("a peer in the static trusted list was refused: %+v (err %v)", c.Refuse, c.Err)
	}
	if n := v.calls.Load(); n != 1 {
		t.Fatalf("the verifier was asked %d times, want once", n)
	}
	leave(t, srv, c)
}
