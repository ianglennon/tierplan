// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests tie the numbers docs/https.md quotes about HSTS to the constants of this package. The
// matching checks for internal/acme live in internal/acme/docs_https_test.go. The two test packages
// cannot share unexported code, so the small helpers are repeated here.

const guideHSTSSection = "HSTS"

// guideText reads docs/https.md. The tests of this package run in internal/server.
func guideText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/https.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// guideSection is the text of the "## heading" section: from its heading line to the next "## "
// heading outside a code fence, or the end. It is empty when there is no such heading.
func guideSection(doc, heading string) string {
	var out []string
	in, fenced := false, false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "## ") {
			if in {
				break
			}
			in = strings.TrimSpace(strings.TrimPrefix(line, "## ")) == heading
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// guideWords writes a duration the way the guide does: "5 minutes", "24 hours", "2 days", "1 week",
// "1 year", "10 years" (a whole number of 365-day years).
func guideWords(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	const day = 24 * time.Hour
	switch {
	case d >= 365*day && d%(365*day) == 0:
		return plural(int64(d/(365*day)), "year")
	case d == 7*day:
		return "1 week"
	case d >= 2*day && d%day == 0:
		return plural(int64(d/day), "day")
	case d%time.Hour == 0:
		return plural(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return plural(int64(d/time.Minute), "minute")
	default:
		return plural(int64(d/time.Second), "second")
	}
}

// guidePhrase is a phrase the guide must hold inside one of its sections.
type guidePhrase struct{ section, phrase string }

// checkGuidePhrases reports a phrase missing from its section, or a section the guide lacks.
func checkGuidePhrases(doc string, rows []guidePhrase) []string {
	var bad []string
	for _, r := range rows {
		sec := guideSection(doc, r.section)
		switch {
		case sec == "":
			bad = append(bad, fmt.Sprintf("the guide has no section %q", r.section))
		case !strings.Contains(sec, r.phrase):
			bad = append(bad, fmt.Sprintf("section %q does not say %q", r.section, r.phrase))
		}
	}
	return bad
}

// guidePhrasesBite proves every row is checked: with its phrase changed in the guide, the check fails.
func guidePhrasesBite(t *testing.T, doc string, rows []guidePhrase) {
	t.Helper()
	for _, r := range rows {
		bad := strings.ReplaceAll(doc, r.phrase, "9999 nonsense")
		t.Run("known-bad/"+r.phrase, func(t *testing.T) {
			if got := checkGuidePhrases(bad, []guidePhrase{r}); len(got) == 0 {
				t.Fatalf("a changed %q was not noticed", r.phrase)
			}
		})
	}
}

// TestHTTPSDocsHSTSNumbers: the HSTS ramp, the dwell before each step and the withdrawal window the
// guide quotes are the constants of hsts.go.
func TestHTTPSDocsHSTSNumbers(t *testing.T) {
	guide := guideText(t)

	var steps []string
	for _, s := range hstsSteps {
		steps = append(steps, guideWords(time.Duration(s)*time.Second))
	}
	rows := []guidePhrase{{guideHSTSSection, strings.Join(steps[:len(steps)-1], ", then ") + ", then " + steps[len(steps)-1]}}
	for i, dwell := range hstsDwell {
		rows = append(rows, guidePhrase{guideHSTSSection, guideWords(dwell) + " at " + steps[i]})
	}
	// The withdrawal window depends on the certificate's lifetime: a week for a 90-day certificate, a
	// third of the lifetime for a short one. The two must differ or the second row proves nothing.
	long, short := 90*24*time.Hour, 6*24*time.Hour
	if withdrawWindow(long) == withdrawWindow(short) {
		t.Fatal("the long and the short certificate have the same withdrawal window")
	}
	rows = append(rows,
		guidePhrase{guideHSTSSection, guideWords(withdrawWindow(long)) + " before a 90-day certificate ends"},
		guidePhrase{guideHSTSSection, guideWords(withdrawWindow(short)) + " before a 6-day certificate ends"},
	)

	t.Run("real", func(t *testing.T) {
		for _, p := range checkGuidePhrases(guide, rows) {
			t.Error(p)
		}
	})
	t.Run("missing section", func(t *testing.T) {
		if got := checkGuidePhrases("# nothing", rows); len(got) == 0 {
			t.Fatal("a guide without the HSTS section was accepted")
		}
	})
	guidePhrasesBite(t, guide, rows)
}

// TestHTTPSDocsServerDefaults: the self-signed certificate's lifetime, the interval at which supplied
// certificate files are read again, and the TLS floor and HTTP/2 the listener offers, as the guide quotes
// them.
func TestHTTPSDocsServerDefaults(t *testing.T) {
	guide := guideText(t)
	const (
		secDefault = "How TierPlan serves HTTPS by default"
		secMode    = "Choosing a mode"
	)
	cfg := newHTTPSRuntime().cfg
	rows := []guidePhrase{
		{secDefault, "valid for " + guideWords(selfSignedValidity)},
		{secMode, "within " + guideWords(suppliedRecheck)},
		{secDefault, tls.VersionName(cfg.MinVersion) + " or newer"},
	}
	// The one path the plain listener answers.
	rows = append(rows, guidePhrase{"Let's Encrypt with HTTP-01", "`" + acmeChallengePrefix + "`"})
	for _, proto := range cfg.NextProtos {
		if proto == "h2" {
			rows = append(rows, guidePhrase{secDefault, "HTTP/2"})
		}
	}
	// The key the guide calls ECDSA P-256 and the copy of the certificate it points at.
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	rows = append(rows,
		guidePhrase{secDefault, "ECDSA " + key.Curve.Params().Name},
		guidePhrase{secDefault, "`" + filepath.Join("/data", selfSignedDir, mirrorCertFile) + "`"},
	)
	if len(rows) != 7 {
		t.Fatalf("the listener no longer offers h2, so the guide's HTTP/2 claim has nothing to be checked against: %v", cfg.NextProtos)
	}
	t.Run("real", func(t *testing.T) {
		for _, p := range checkGuidePhrases(guide, rows) {
			t.Error(p)
		}
	})
	guidePhrasesBite(t, guide, rows)
}
