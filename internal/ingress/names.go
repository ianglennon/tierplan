// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"errors"
	"fmt"
	"strings"
)

// The limits of a DNS name (RFC 1035).
const (
	maxHostLen  = 253
	maxLabelLen = 63
)

// NormalizeHost lowers s and drops surrounding space and one trailing dot. Every hostname and
// suffix is stored and compared in this form.
func NormalizeHost(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

// ValidateHost checks a normalized hostname. It accepts what a certificate for one exact name can
// carry and nothing else: at least two labels of letters, digits and hyphens. A wildcard, an
// underscore, a non-ASCII name (write it in its xn-- form) and an IP address are all refused; a
// name whose last label is all digits is refused because it would read as an address.
func ValidateHost(host string) error {
	if host == "" {
		return errors.New("the hostname is empty")
	}
	if len(host) > maxHostLen {
		return fmt.Errorf("the hostname is longer than %d characters", maxHostLen)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return errors.New("the hostname needs at least two labels, such as app.example.com")
	}
	for _, l := range labels {
		if err := validateLabel(l); err != nil {
			return err
		}
	}
	if allDigits(labels[len(labels)-1]) {
		return errors.New("the hostname must be a name, not an IP address")
	}
	return nil
}

// validateLabel checks one label of a normalized hostname.
func validateLabel(l string) error {
	switch {
	case l == "":
		return errors.New("the hostname has an empty label")
	case l == "*":
		return errors.New("a wildcard is not a hostname: every route names one exact host")
	case len(l) > maxLabelLen:
		return fmt.Errorf("the label %q is longer than %d characters", l, maxLabelLen)
	case l[0] == '-' || l[len(l)-1] == '-':
		return fmt.Errorf("the label %q starts or ends with a hyphen", l)
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("the label %q has a character that is not a letter, a digit or a hyphen", l)
		}
	}
	return nil
}

// allDigits reports whether s is made of digits only.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// ValidateSuffix checks a domain suffix. It normalizes the suffix, then refuses an empty one, then
// refuses any suffix that is not a valid hostname. Both refusals are a *NameError with
// CodeDomainInvalid. The two steps are separate on purpose: the public-suffix refusal sits between
// them, because ValidateHost refuses every single-label name and would otherwise answer a bare top
// level such as com with the invalid code instead of the public-suffix one.
func ValidateSuffix(suffix string) error {
	suffix = NormalizeHost(suffix)
	if suffix == "" {
		return &NameError{Code: CodeDomainInvalid, Message: "The domain is empty. Enter a suffix such as “apps.example.com”."}
	}
	if err := ValidateHost(suffix); err != nil {
		return &NameError{Code: CodeDomainInvalid, Message: "“" + suffix + "” is not a valid domain: " + err.Error() + "."}
	}
	return nil
}

// Under reports whether host is a name strictly below suffix: app.apps.example.com is under
// apps.example.com, and apps.example.com itself is not. Both must be normalized. The dot in the
// comparison is what stops badapps.example.com from passing.
func Under(host, suffix string) bool {
	return len(host) > len(suffix)+1 && strings.HasSuffix(host, "."+suffix)
}

// MatchDomain returns the domain host belongs to: the one with the longest suffix host is under,
// so a more specific registration wins over a broader one. ok is false when no domain allows host.
func MatchDomain(host string, domains []Domain) (d Domain, ok bool) {
	for _, c := range domains {
		if Under(host, c.Suffix) && (!ok || len(c.Suffix) > len(d.Suffix)) {
			d, ok = c, true
		}
	}
	return d, ok
}
