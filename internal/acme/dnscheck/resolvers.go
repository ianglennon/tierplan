// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnscheck

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// MaxResolvers is the most resolvers an admin may list.
const MaxResolvers = 5

// Reasons a ResolverError carries.
const (
	// ReasonInvalid is an entry that is not host or host:port.
	ReasonInvalid = "invalid"
	// ReasonTooMany is a list with more than MaxResolvers distinct entries.
	ReasonTooMany = "too_many"
)

// ResolverError names the resolver entry that was refused.
type ResolverError struct {
	// Entry is the entry as the admin wrote it (for "too_many", the first one over the limit).
	Entry string
	// Reason is ReasonInvalid or ReasonTooMany.
	Reason string
}

func (e *ResolverError) Error() string {
	if e.Reason == ReasonTooMany {
		return "too many resolvers: " + e.Entry + " is over the limit of " + strconv.Itoa(MaxResolvers)
	}
	return "resolver " + strconv.Quote(e.Entry) + " is not a host or host:port"
}

// ParseResolvers validates and normalises the admin's resolver list. Entries may also be comma
// separated inside one string; blanks are dropped, so a list of only spaces and commas is empty
// (which means the zone's own nameservers). Each entry is a host or host:port where the host is an
// IP literal or an ASCII DNS name, with no scheme or path and a port from 1 to 65535 (default 53).
// IPv6 literals are bracketed with their port. Names are lowercased, duplicates are removed keeping
// the first, and the admin's order is kept. At most MaxResolvers distinct entries are allowed.
func ParseResolvers(entries []string) ([]string, error) {
	var out []string
	for _, raw := range entries {
		for _, part := range strings.Split(raw, ",") {
			entry := strings.TrimSpace(part)
			if entry == "" {
				continue
			}
			norm, ok := normaliseResolver(entry)
			if !ok {
				return nil, &ResolverError{Entry: entry, Reason: ReasonInvalid}
			}
			dup := false
			for _, have := range out {
				if have == norm {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			if len(out) == MaxResolvers {
				return nil, &ResolverError{Entry: entry, Reason: ReasonTooMany}
			}
			out = append(out, norm)
		}
	}
	return out, nil
}

// normaliseResolver returns "host:port" for one entry, or false if the entry is not valid.
func normaliseResolver(entry string) (string, bool) {
	host, port := entry, defaultPort
	switch {
	case strings.HasPrefix(entry, "["):
		end := strings.Index(entry, "]")
		if end < 0 {
			return "", false
		}
		host = entry[1:end]
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" || addr.Is4In6() {
			return "", false
		}
		switch rest := entry[end+1:]; {
		case rest == "":
		case strings.HasPrefix(rest, ":"):
			port = rest[1:]
		default:
			return "", false
		}
	case strings.Count(entry, ":") == 1:
		host, port, _ = strings.Cut(entry, ":")
	}
	if !validPort(port) {
		return "", false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" || (strings.Contains(host, ":") != addr.Is6()) {
			return "", false
		}
		if addr.Is4In6() {
			return "", false
		}
		return net.JoinHostPort(addr.String(), port), true
	}
	if strings.HasPrefix(entry, "[") || strings.Contains(host, ":") {
		return "", false // brackets hold only an IPv6 literal, and a bare host with colons is not a name
	}
	name, ok := resolverName(host)
	if !ok {
		return "", false
	}
	return net.JoinHostPort(name, port), true
}

// validPort is true for a decimal port from 1 to 65535 and nothing else (no sign, no spaces).
func validPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

// resolverName lowercases an ASCII host name and checks it: labels of 1 to 63 letters, digits and
// hyphens, none starting or ending with a hyphen, at most 253 characters in all, and a last label
// that is not all digits (so a mistyped IPv4 address is not taken for a name).
func resolverName(host string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" || len(name) > 253 {
		return "", false
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}
	last := labels[len(labels)-1]
	allDigits := true
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			allDigits = false
		}
	}
	return name, !allDigits
}
