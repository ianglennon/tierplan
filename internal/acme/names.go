// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"net"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// NameSet is the names a certificate would carry and the reasons it cannot carry them. Names
// holds an IP address or a wildcard too when one was given, so the UI can list it as "Not
// allowed" next to its refusal; it is never sent to a CA while Refusals is not empty.
type NameSet struct {
	Names    []string
	Refusals []Refusal
}

// nameProfile converts a host name to its ASCII (punycode) form. It is a local copy of the
// profile internal/server uses for the self-signed certificate (this package must not import
// that one): it maps case and Unicode compatibility forms but does not apply the STD3 rules.
var nameProfile = idna.New(idna.MapForLookup(), idna.StrictDomainName(false))

// WantedNames is the certificate's names: the host of publicURL and then, when it differs, the
// host of agentURL, each lower-cased and in punycode. Never localhost, loopback or the machine
// host name: those are for the self-signed certificate only (ACME-02).
//
// An IP address, a wildcard and, when checkPublic is set, a name Let's Encrypt can never issue
// for are refused (D-23). checkPublic is false when the directory is overridden for a test, so
// a Pebble run can issue for a .test name. An empty publicURL gives no names and a
// RefuseNamesNotPublic with an empty host, so no order can start with no identifiers.
func WantedNames(publicURL, agentURL string, checkPublic bool) NameSet {
	var ns NameSet
	add := func(host, ipCode string) {
		if slices.Contains(ns.Names, host) {
			return
		}
		ns.Names = append(ns.Names, host)
		switch {
		case net.ParseIP(host) != nil:
			ns.Refusals = append(ns.Refusals, Refusal{Code: ipCode, Host: host})
		case strings.Contains(host, "*"):
			ns.Refusals = append(ns.Refusals, Refusal{Code: RefuseNamesWildcard, Host: host})
		case checkPublic && !isPublicName(host):
			ns.Refusals = append(ns.Refusals, Refusal{Code: RefuseNamesNotPublic, Host: host})
		}
	}

	pub := hostOf(publicURL)
	if pub == "" {
		ns.Refusals = append(ns.Refusals, Refusal{Code: RefuseNamesNotPublic, Host: ""})
	} else {
		add(canonicalHost(pub), RefuseNamesIP)
	}
	if ag := hostOf(agentURL); ag != "" {
		add(canonicalHost(ag), RefuseAgentURLIP)
	}
	return ns
}

// hostOf is the trimmed, lower-cased host of raw, or "" when raw has none.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(u.Hostname()))
}

// canonicalHost is host as the identifier that goes to the CA: an IP in its canonical text form,
// a wildcard as written, any other name as ASCII. A name that cannot be encoded is returned as
// written, and isPublicName then refuses it for having a character no DNS name has.
func canonicalHost(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	if strings.Contains(host, "*") {
		return host
	}
	if ascii, ok := dnsName(host); ok {
		return ascii
	}
	return host
}

// dnsName is host as a DNS name: pure ASCII, a non-ASCII name converted to punycode. ok is false
// for a name that cannot be encoded.
func dnsName(host string) (string, bool) {
	ascii, err := nameProfile.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 {
		return "", false
	}
	for i := 0; i < len(ascii); i++ {
		if c := ascii[i]; c <= ' ' || c >= 0x7f {
			return "", false
		}
	}
	return ascii, true
}

// specialUse are the suffixes that never resolve on the public internet: RFC 6761 and 6762
// special-use names, the home network name of RFC 8375, and the names Let's Encrypt refuses
// outright. The public suffix list does not carry most of them.
var specialUse = []string{
	"localhost", "local", "lan", "home", "internal", "home.arpa",
	"test", "example", "invalid", "onion", "alt",
}

// isPublicName reports whether Let's Encrypt could ever issue for host (D-23, RESEARCH A1). It
// is a cheap local screen, not a guarantee: a name that passes can still be refused by the CA.
// It refuses a single-label name, a special-use suffix, a top-level label that is not an ICANN
// one (.corp, .lan), and a name that is itself a public suffix such as co.uk. A host under a
// private suffix on an ICANN top level (me.duckdns.org) passes: the CA issues for those.
func isPublicName(host string) bool {
	if _, ok := dnsName(host); !ok || !strings.Contains(host, ".") {
		return false
	}
	for _, s := range specialUse {
		if host == s || strings.HasSuffix(host, "."+s) {
			return false
		}
	}
	tld := host[strings.LastIndexByte(host, '.')+1:]
	if _, icann := publicsuffix.PublicSuffix(tld); !icann {
		return false
	}
	if suffix, icann := publicsuffix.PublicSuffix(host); icann && suffix == host {
		return false
	}
	return true
}

// SameNames reports whether a and b hold the same names, in any order. It is how a change of URL
// after an issue is detected (D-05). Neither argument is modified.
func SameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
