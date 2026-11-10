// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel_test

import (
	"net"
	"testing"

	"github.com/ianglennon/tierplan/internal/tunnel"
)

func TestDialHostStripsPortAndBrackets(t *testing.T) {
	tests := []struct{ aud, want string }{
		{"tierplan.example.com", "tierplan.example.com"},
		{"Tierplan.Example.com:7480", "tierplan.example.com"},
		{"[2001:db8::1]:7480", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"192.0.2.1:8443", "192.0.2.1"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := tunnel.DialHostOf(tc.aud); got != tc.want {
			t.Errorf("dialHost(%q) = %q, want %q", tc.aud, got, tc.want)
		}
	}
}

// The handshake already refused any audience other than the public URL's, so the session records
// the host of that URL.
func TestSessionRecordsTheDialHost(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	f.dial(t, hs{})
	<-f.o.conn
	host, _, err := net.SplitHostPort(f.ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	got := f.hub.Sessions()
	if len(got) != 1 || got[0].DialHost != host {
		t.Fatalf("Sessions = %+v, want one with DialHost %q", got, host)
	}
}
