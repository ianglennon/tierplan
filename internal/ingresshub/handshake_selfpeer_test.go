// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

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

// A proxy connect relayed through TierPlan's own ingress route comes from the self network. It is refused with
// its own code before anything is verified, so a peer the live set trusts never skips the channel binding
// (SELF-05, A-05). Each case below would get another answer from verifyAuth or the service (a welcome, an
// audience refusal, wrong_token_kind), so self_network_peer for all of them shows neither was reached.
func TestIngressHandshakeRefusesASelfNetworkPeer(t *testing.T) {
	logs := &syncBuffer{}
	srv := ingresstest.Start(t, ingresstest.Options{
		Wrap:          fromPeer("10.9.0.5:40000", nil),
		AuditRefusals: true,
		Log:           slog.New(slog.NewJSONHandler(logs, nil)),
	})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "swarmnode1")
	// Any token with the agent prefix is answered wrong_token_kind by the service; this one need not be real.
	agentToken := ingresswire.AgentTokenPrefix + env.ID + "_notasecret"

	cases := []struct {
		name string
		d    ingresstest.Dial
	}{
		{"a valid enrolment", ingresstest.Dial{Mode: ingresswire.ModeEnrol, Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1"}},
		{"an enrolment for another audience", ingresstest.Dial{Mode: ingresswire.ModeEnrol, Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1", Aud: "elsewhere.example:443"}},
		{"an agent join token", ingresstest.Dial{Mode: ingresswire.ModeEnrol, WireToken: agentToken, Name: "node-1", SwarmNodeID: "swarmnode1"}},
	}
	for _, tc := range cases {
		c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), tc.d)
		if c.Welcome != nil {
			t.Fatalf("%s: a peer on the self network was welcomed", tc.name)
		}
		if c.Refuse == nil || c.Refuse.Code != ingresswire.RefuseSelfNetworkPeer {
			t.Fatalf("%s: refuse %+v err %v, want %s", tc.name, c.Refuse, c.Err, ingresswire.RefuseSelfNetworkPeer)
		}
		if c.Refuse.Detail == "" {
			t.Errorf("%s: the refusal says nothing about why", tc.name)
		}
	}

	if n := nodeCount(t, srv); n != 0 {
		t.Errorf("%d ingress_nodes rows, want 0", n)
	}
	rows := refusedRows(t, srv)
	if len(rows) != len(cases) {
		t.Fatalf("%d refusal rows, want %d", len(rows), len(cases))
	}
	for _, r := range rows {
		if r.Reason != ingresswire.RefuseSelfNetworkPeer || r.IP != "10.9.0.5" {
			t.Errorf("refusal row %+v, want reason %s from 10.9.0.5", r, ingresswire.RefuseSelfNetworkPeer)
		}
	}

	refused := 0
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		if line["msg"] != "ingress handshake refused" {
			continue
		}
		refused++
		if line["code"] != ingresswire.RefuseSelfNetworkPeer || line["ip"] != "10.9.0.5" {
			t.Errorf("log line %v, want code %s and ip 10.9.0.5", line, ingresswire.RefuseSelfNetworkPeer)
		}
	}
	if refused != len(cases) {
		t.Errorf("%d \"ingress handshake refused\" lines, want %d", refused, len(cases))
	}
}

// A peer trusted only through TIERPLAN_TRUSTED_PROXIES, an operator's own TLS terminator, is not on the self
// network: it enrols, its exporter value left uncompared, exactly as before.
func TestIngressHandshakeTrustedProxyPathUnchanged(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{
		Wrap: fromPeer("10.8.0.5:40000", []netip.Prefix{netip.MustParsePrefix("10.8.0.0/24")}),
	})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "swarmnode1")
	c := srv.Enrol(t, ingresstest.NewIdentity(), ingresstest.Dial{Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1", NoEKM: true})
	if c.Welcome == nil {
		t.Fatal("a peer in the static trusted list was not welcomed")
	}
}
