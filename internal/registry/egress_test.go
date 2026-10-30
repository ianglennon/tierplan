// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestEgressRefusesLinkLocalUnspecifiedAndMulticastAddresses(t *testing.T) {
	p := NewEgressPolicy(nil)
	for _, a := range []string{"169.254.169.254", "169.254.0.1", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "239.1.2.3", "ff02::1", "::ffff:169.254.169.254"} {
		if !p.Blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s was allowed, want refused", a)
		}
	}
}

func TestEgressAllowsPrivateAndLoopbackRangesForSelfHostedRegistries(t *testing.T) {
	p := NewEgressPolicy(nil)
	for _, a := range []string{"10.0.0.5", "192.168.1.10", "172.16.0.9", "127.0.0.1", "::1", "fd00::5", "93.184.216.34"} {
		if p.Blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s was refused, want allowed", a)
		}
	}
}

func TestEgressRefusesTheOperatorsExtraRanges(t *testing.T) {
	p := NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")})
	if !p.Blocked(netip.MustParseAddr("10.9.1.1")) || p.Blocked(netip.MustParseAddr("10.8.1.1")) {
		t.Error("the extra range is not honoured exactly")
	}
}

func TestEgressTransportRefusesToDialABlockedAddressBeforeConnecting(t *testing.T) {
	tr := NewEgressPolicy(nil).Transport()
	for _, addr := range []string{"169.254.169.254:80", "[fe80::1]:443", "0.0.0.0:80", "[::]:80", "224.0.0.1:80", "[ff02::1]:80"} {
		conn, err := tr.DialContext(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrEgressBlocked) {
			t.Errorf("dial %s: err = %v, want ErrEgressBlocked", addr, err)
		}
	}
}

func TestEgressTransportDialsLoopbackWhereASelfHostedRegistryLives(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		if c, err := l.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	conn, err := NewEgressPolicy(nil).Transport().DialContext(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial loopback: %v", err)
	}
	_ = conn.Close()
}

func TestEgressTransportUsesNoProxyAndTheDocumentedTimeouts(t *testing.T) {
	tr := NewEgressPolicy(nil).Transport()
	if tr.Proxy != nil {
		t.Error("the transport takes a proxy from the environment, which would be dialled instead of the registry")
	}
	if tr.TLSHandshakeTimeout.Seconds() != 10 || tr.ResponseHeaderTimeout.Seconds() != 30 {
		t.Errorf("timeouts = %v / %v, want 10s handshake and 30s to the first byte", tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
}

func TestTheZeroEgressPolicyStillRefusesTheMetadataAddress(t *testing.T) {
	var p EgressPolicy
	if !p.Blocked(netip.MustParseAddr("169.254.169.254")) {
		t.Error("a policy that was never constructed lets the metadata address through")
	}
}

// RefusesHost applies the policy to a host the server does not dial itself (WR-B02).
func TestRefusesHostJudgesLiteralAddressesAndResolvedNamesAndLeavesUnresolvableNamesToTheNode(t *testing.T) {
	p := NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")})
	for host, want := range map[string]bool{
		"169.254.169.254": true, "169.254.169.254:5000": true, "[fe80::1]:5000": true, "[fd00:ec2::254]": true,
		"100.100.100.200": true, "192.0.0.192": true, "10.9.1.1:443": true,
		"10.8.1.1": false, "127.0.0.1:5000": false, "192.168.1.5": false, "registry.invalid": false,
	} {
		if got := p.RefusesHost(context.Background(), host); got != want {
			t.Errorf("RefusesHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// The git transport shares the dial hook of the registry transport, with a larger read bound and
// no switch that turns certificate checks off (plan 09-01, T-09-01, T-09-04).
func TestGitTransportRefusesToDialABlockedAddressBeforeConnecting(t *testing.T) {
	tr := NewEgressPolicy(nil).GitTransport()
	for _, addr := range []string{"169.254.169.254:443", "[fe80::1]:443", "0.0.0.0:443", "224.0.0.1:443"} {
		conn, err := tr.DialContext(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrEgressBlocked) {
			t.Errorf("dial %s: err = %v, want ErrEgressBlocked", addr, err)
		}
	}
}

func TestGitTransportUsesNoProxyAndKeepsCertificateChecksOn(t *testing.T) {
	tr := NewEgressPolicy(nil).GitTransport()
	if tr.Proxy != nil {
		t.Error("the git transport takes a proxy from the environment, which would be dialled instead of the host")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.TLSClientConfig.VerifyPeerCertificate != nil {
		t.Errorf("TLS config = %+v, want TLS 1.2 minimum with the default verification", tr.TLSClientConfig)
	}
}

func TestGitTransportDialsThroughThePolicyOfTheCallWhenTheContextCarriesOne(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	tr := NewEgressPolicy(nil).GitTransport()
	conn, err := tr.DialContext(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial loopback with the transport's own policy: %v", err)
	}
	_ = conn.Close()

	blockLoopback := NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	conn, err = tr.DialContext(WithEgressPolicy(context.Background(), blockLoopback), "tcp", l.Addr().String())
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("dial with a call policy that blocks loopback: err = %v, want ErrEgressBlocked", err)
	}
}

// A listing is read through a transport with a smaller bound than the one that carries a packfile (WR-A06).
func TestGitTransportBoundedStopsAReadAtItsLimit(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write(make([]byte, 64))
	}()
	conn, err := NewEgressPolicy(nil).GitTransportBounded(16).DialContext(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var got int
	buf := make([]byte, 64)
	for {
		n, err := conn.Read(buf)
		got += n
		if err != nil {
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("read past the limit = %v, want ErrResponseTooLarge", err)
			}
			break
		}
	}
	if got != 16 {
		t.Errorf("read %d bytes before the refusal, want exactly the limit of 16", got)
	}
}

func TestDialControlRefusesWhatThePolicyBlocks(t *testing.T) {
	ctl := NewEgressPolicy(nil).DialControl()
	if err := ctl("tcp", "169.254.169.254:22", nil); !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("control(metadata) = %v, want ErrEgressBlocked", err)
	}
	if err := ctl("tcp", "10.0.0.5:22", nil); err != nil {
		t.Errorf("control(private) = %v, want nil", err)
	}
}

func TestABoundedConnFailsTheReadThatPassesItsLimitAndStartsAgainAtTheNextWrite(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
			if _, err := server.Write([]byte("0123456789")); err != nil {
				return
			}
		}
	}()
	c := &boundedConn{Conn: client, limit: 10}
	buf := make([]byte, 64)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Read(buf); err != nil || n != 10 {
		t.Fatalf("first read = %d, %v, want 10 bytes", n, err)
	}
	if _, err := c.Read(buf); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("read past the limit = %v, want ErrResponseTooLarge", err)
	}
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Read(buf); err != nil || n != 10 {
		t.Fatalf("read after the next write = %d, %v, want 10 bytes", n, err)
	}
	if gitResponseBytesMax <= responseBytesMax {
		t.Errorf("gitResponseBytesMax = %d, want it above the registry's %d", gitResponseBytesMax, responseBytesMax)
	}
}
