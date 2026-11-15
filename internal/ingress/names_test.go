// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  App.Example.COM. ", "app.example.com"},
		{"app.example.com", "app.example.com"},
		// Only one trailing dot goes; the second stays and is refused by ValidateHost.
		{"app.example.com..", "app.example.com."},
		{"", ""},
	} {
		if got := NormalizeHost(tc.in); got != tc.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateHost(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	label64 := strings.Repeat("a", 64)
	// 253 bytes: three 63-byte labels, one 61-byte label and the dots between them.
	name253 := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("a", 61)
	if len(name253) != 253 {
		t.Fatalf("test setup: name253 is %d bytes", len(name253))
	}

	valid := []string{
		"app.example.com",
		"a-b.apps.example.co.uk",
		"xn--bcher-kva.example.com",
		"1app.example.com",
		label63 + ".example.com",
		name253,
	}
	for _, h := range valid {
		if err := ValidateHost(h); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want nil", h, err)
		}
	}

	invalid := []struct {
		name string
		host string
	}{
		{"empty", ""},
		{"single label", "localhost"},
		{"wildcard", "*.example.com"},
		{"underscore", "app_1.example.com"},
		{"leading hyphen", "-app.example.com"},
		{"trailing hyphen", "app-.example.com"},
		{"empty label", "app..example.com"},
		{"ip address", "192.168.1.10"},
		{"slash", "app.example.com/x"},
		{"label of 64 bytes", label64 + ".example.com"},
		{"name of 254 bytes", name253 + "a"},
		{"upper case (not normalized)", "App.example.com"},
		{"space", "app example.com"},
	}
	for _, tc := range invalid {
		if err := ValidateHost(tc.host); err == nil {
			t.Errorf("%s: ValidateHost(%q) = nil, want an error", tc.name, tc.host)
		}
	}

	t.Run("non-ASCII names its punycode form", func(t *testing.T) {
		err := ValidateHost("bücher.example.com")
		if err == nil {
			t.Fatal("ValidateHost(bücher.example.com) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "xn--bcher-kva.example.com") {
			t.Errorf("error %q does not name xn--bcher-kva.example.com", err)
		}
	})

	t.Run("non-ASCII with no valid international form still refuses", func(t *testing.T) {
		// A lone combining mark cannot start a label, so ToASCII fails.
		err := ValidateHost("́.example.com")
		if err == nil {
			t.Fatal("ValidateHost = nil, want an error")
		}
		if !strings.Contains(err.Error(), "international") {
			t.Errorf("error %q does not say the name is not a valid international name", err)
		}
	})

	t.Run("lengths are counted in bytes", func(t *testing.T) {
		// 32 two-byte runes make a 64-byte label that is only 32 characters long.
		err := ValidateHost(strings.Repeat("ü", 32) + ".example.com")
		if err == nil {
			t.Fatal("ValidateHost = nil, want an error")
		}
	})
}

func TestValidateSuffix(t *testing.T) {
	for _, s := range []string{"example.co.uk", "foo.github.io", "apps.example.com", "internal.corp"} {
		if err := ValidateSuffix(s); err != nil {
			t.Errorf("ValidateSuffix(%q) = %v, want nil", s, err)
		}
	}

	// D-07: a suffix that is itself a public suffix is refused, and the single-label com is answered
	// with the public-suffix code because that step runs before ValidateHost.
	for _, s := range []string{"co.uk", "com", "github.io"} {
		assertNameCode(t, s, CodeDomainPublicSuffix)
	}

	for _, s := range []string{"", "   ", "*.example.com", "example..com", "app_1.example.com", "*", "bücher"} {
		assertNameCode(t, s, CodeDomainInvalid)
	}

	t.Run("raw non-ASCII below a registrable name names its punycode form", func(t *testing.T) {
		err := ValidateSuffix("bücher.example.com")
		var ne *NameError
		if !errors.As(err, &ne) || ne.Code != CodeDomainInvalid {
			t.Fatalf("ValidateSuffix = %v, want a *NameError with %s", err, CodeDomainInvalid)
		}
		if !strings.Contains(ne.Message, "xn--bcher-kva.example.com") {
			t.Errorf("message %q does not name the punycode form", ne.Message)
		}
	})

	t.Run("upper case is normalized, not refused", func(t *testing.T) {
		if err := ValidateSuffix("Apps.Example.COM."); err != nil {
			t.Errorf("ValidateSuffix = %v, want nil", err)
		}
		assertNameCode(t, "CO.UK", CodeDomainPublicSuffix)
	})
}

// assertNameCode fails unless ValidateSuffix(suffix) returns a *NameError with code.
func assertNameCode(t *testing.T, suffix, code string) {
	t.Helper()
	err := ValidateSuffix(suffix)
	var ne *NameError
	if !errors.As(err, &ne) {
		t.Errorf("ValidateSuffix(%q) = %v, want a *NameError with %s", suffix, err, code)
		return
	}
	if ne.Code != code {
		t.Errorf("ValidateSuffix(%q) code = %s, want %s (%s)", suffix, ne.Code, code, ne.Message)
	}
	if ne.Message == "" {
		t.Errorf("ValidateSuffix(%q) has an empty message", suffix)
	}
}
