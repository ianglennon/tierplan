// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/resolvers"
)

// These tests keep docs/https.md true. Each check is a pure function over strings. A subtest feeds it a
// snippet that breaks the rule (it must report a problem) and then the real file (it must report none),
// so a check that never bites cannot pass. The guide quotes names, numbers and labels; every one of them
// is tied to the code or the UI strings here, so the guide cannot drift from the product (Phase 17, D-19).

// repoText reads a file of the repository by its path from the repository root. The tests of this
// package run in internal/acme, so the root is two levels up.
func repoText(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel) //nolint:gosec // G304: a repository path chosen by this test
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// expectBad fails the test when a check accepts a snippet that breaks its rule.
func expectBad(t *testing.T, name string, check func(string) []string, bad string) {
	t.Helper()
	t.Run(name+"/known-bad", func(t *testing.T) {
		if got := check(bad); len(got) == 0 {
			t.Fatalf("the check accepted a snippet that breaks the rule:\n%s", bad)
		}
	})
}

// expectClean fails the test for every problem a check finds on the real text.
func expectClean(t *testing.T, name string, check func(string) []string, real string) {
	t.Helper()
	t.Run(name+"/real", func(t *testing.T) {
		for _, p := range check(real) {
			t.Error(p)
		}
	})
}

// mdProse is the lines of a markdown text outside fenced code blocks. Headings and bold spans are read from these so a "# comment" in a compose example is not a heading.
func mdProse(doc string) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			out = append(out, line)
		}
	}
	return out
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

// mdHeadings are the heading texts of a markdown document.
func mdHeadings(doc string) []string {
	var out []string
	for _, line := range mdProse(doc) {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[2])
		}
	}
	return out
}

var slugDrop = regexp.MustCompile(`[^\p{L}\p{N} _-]`)

// mdSlug is the anchor GitHub gives a heading: lower case, punctuation dropped, spaces to hyphens.
func mdSlug(heading string) string {
	return strings.ReplaceAll(slugDrop.ReplaceAllString(strings.ToLower(heading), ""), " ", "-")
}

// mdSection is the text of the "## heading" section: from its heading line to the next "## " heading or
// the end. It is empty when there is no such heading.
func mdSection(doc, heading string) string {
	lines := strings.Split(doc, "\n")
	var out []string
	in, fenced := false, false
	for _, line := range lines {
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

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// relLinks are the relative link targets of a document, split into the file part and the anchor.
type relLink struct{ file, anchor string }

func mdRelLinks(doc string) []relLink {
	var out []relLink
	for _, m := range linkRe.FindAllStringSubmatch(doc, -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		file, anchor, _ := strings.Cut(target, "#")
		out = append(out, relLink{file: file, anchor: anchor})
	}
	return out
}

// checkLinksResolve reports a relative link in the document (which lives in dir, a slash path from the
// repository root) whose file does not exist, or whose anchor matches no heading of the file it points at.
func checkLinksResolve(doc, dir string) []string {
	var bad []string
	for _, l := range mdRelLinks(doc) {
		target := doc
		name := "the document itself"
		if l.file != "" {
			rel := path.Join(dir, l.file)
			b, err := os.ReadFile("../../" + rel) //nolint:gosec // G304: a repository path chosen by this test
			if err != nil {
				bad = append(bad, fmt.Sprintf("link to %s: file not found", rel))
				continue
			}
			target, name = string(b), rel
		}
		if l.anchor == "" || (l.file != "" && !strings.HasSuffix(l.file, ".md")) {
			continue
		}
		found := false
		for _, h := range mdHeadings(target) {
			if mdSlug(h) == l.anchor {
				found = true
				break
			}
		}
		if !found {
			bad = append(bad, fmt.Sprintf("link to %s#%s: no such heading", name, l.anchor))
		}
	}
	return bad
}

// checkNoPlanningLinks reports a relative link whose target, resolved against the repository root, is one
// of the git-ignored planning paths. Those paths exist in a working tree and are dead in the published
// repository, so the check is on the path and not on whether the file is there.
func checkNoPlanningLinks(doc, dir string) []string {
	var bad []string
	for _, l := range mdRelLinks(doc) {
		if l.file == "" {
			continue
		}
		p := path.Clean(path.Join(dir, l.file))
		if p == ".planning" || strings.HasPrefix(p, ".planning/") || p == ".claude" || strings.HasPrefix(p, ".claude/") ||
			p == "CLAUDE.md" || strings.HasPrefix(p, "../") {
			bad = append(bad, fmt.Sprintf("link to %s resolves to %s, which is not published", l.file, p))
		}
	}
	return bad
}

var envNameRe = regexp.MustCompile(`TIERPLAN_[A-Z0-9_]*[A-Z0-9]`)

// checkEnvNames reports a TIERPLAN_ name in the document that is not a quoted literal of config.go.
func checkEnvNames(doc, configSrc string) []string {
	seen := map[string]bool{}
	var bad []string
	for _, n := range envNameRe.FindAllString(doc, -1) {
		if seen[n] {
			continue
		}
		seen[n] = true
		if !strings.Contains(configSrc, `"`+n+`"`) {
			bad = append(bad, fmt.Sprintf("%s is not an environment variable of internal/config/config.go", n))
		}
	}
	return bad
}

var plainTokenRe = regexp.MustCompile(`TIERPLAN_CLOUDFLARE_API_TOKEN[ \t]*[=:][ \t]*\S`)

// checkSafeExamples reports a setting that skips TLS verification, and a value given to the plain
// Cloudflare token variable (the secret belongs in a file: a plain variable shows in docker inspect).
func checkSafeExamples(doc string) []string {
	var bad []string
	for _, s := range []string{"insecureSkipVerify", "tls_insecure_skip_verify", "proxy_ssl_verify off", "--insecure", "curl -k"} {
		if strings.Contains(doc, s) {
			bad = append(bad, "the document shows "+s)
		}
	}
	if plainTokenRe.MatchString(doc) {
		bad = append(bad, "the document gives a value to TIERPLAN_CLOUDFLARE_API_TOKEN; use the _FILE form with a secret")
	}
	return bad
}

// checkReadme reports a README that does not link the guide, or a HTTPS row that lacks the link.
func checkReadme(readme string) []string {
	var bad []string
	if !strings.Contains(readme, "](docs/https.md") {
		bad = append(bad, "README.md does not link docs/https.md")
	}
	for _, env := range []string{"TIERPLAN_ACME", "TIERPLAN_ACME_CHALLENGE", "TIERPLAN_DNS_PROVIDER", "TIERPLAN_CLOUDFLARE_API_TOKEN", "TIERPLAN_HSTS"} {
		found := false
		for _, line := range strings.Split(readme, "\n") {
			if strings.HasPrefix(line, "| `"+env+"` |") {
				found = true
				if !strings.Contains(line, "docs/https.md") {
					bad = append(bad, "the README row of "+env+" does not link docs/https.md")
				}
			}
		}
		if !found {
			bad = append(bad, "the README has no row for "+env)
		}
	}
	return bad
}

// docWords writes a duration the way the guide does: "5 minutes", "24 hours", "10 days", "1 week",
// "1 year". A whole number of minutes that is not a whole number of hours stays in minutes
// ("100 minutes"), so the backoff reads as Let's Encrypt publishes it.
func docWords(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	const day = 24 * time.Hour
	switch {
	case d == 365*day:
		return "1 year"
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

// docNumber is a phrase the guide must contain inside one of its sections.
type docNumber struct{ section, phrase string }

// checkNumbers reports a phrase that is missing from its section, or a section that is missing.
func checkNumbers(doc string, rows []docNumber) []string {
	var bad []string
	for _, r := range rows {
		sec := mdSection(doc, r.section)
		switch {
		case sec == "":
			bad = append(bad, fmt.Sprintf("the guide has no section %q", r.section))
		case !strings.Contains(sec, r.phrase):
			bad = append(bad, fmt.Sprintf("section %q does not say %q", r.section, r.phrase))
		}
	}
	return bad
}

// checkNumbersBite proves every row of a table is checked: with that row's phrase changed in the guide,
// the check must report a problem.
func checkNumbersBite(t *testing.T, name, doc string, rows []docNumber) {
	t.Helper()
	for _, r := range rows {
		bad := strings.ReplaceAll(doc, r.phrase, "9999 nonsense")
		t.Run(name+"/known-bad/"+r.phrase, func(t *testing.T) {
			if got := checkNumbers(bad, []docNumber{r}); len(got) == 0 {
				t.Fatalf("a changed %q was not noticed", r.phrase)
			}
		})
	}
}

// listWords joins the words of a list of durations the way the guide does: "a, b, then c".
func listWords(ds []time.Duration) string {
	var w []string
	for _, d := range ds {
		w = append(w, docWords(d))
	}
	if len(w) < 2 {
		return strings.Join(w, "")
	}
	return strings.Join(w[:len(w)-1], ", ") + ", then " + w[len(w)-1]
}

const (
	secHSTS    = "HSTS"
	secRenewal = "Renewal, backoff, the restore pause and the fallback"
	secBell    = "What each bell warning means"
	secTrouble = "Troubleshooting by error class"
	secLimits  = "Let's Encrypt rate limits"
	secHTTP01  = "Let's Encrypt with HTTP-01"
)

// acmeNumbers are the numbers of internal/acme the guide quotes, written from the constants themselves.
func acmeNumbers() []docNumber {
	failed := fmt.Sprintf("%d failed runs in %s", FailureGuardRuns, docWords(FailureGuardWindow))
	return []docNumber{
		{secRenewal, listWords(backoffSteps[:]) + " for every later failure"},
		{secRenewal, "up to " + docWords(MaxRetryAfter)},
		{secRenewal, failed},
		{secRenewal, "less than " + docWords(ShortLifetime)},
		{secRenewal, "never longer than " + docWords(ShortLifetimeMaxDelay)},
		{secHSTS, "less than " + docWords(ShortLifetime)},
		{secBell, failed},
		{secBell, "more than " + docWords(ClockWarnAbove)},
		{secBell, "older than " + docWords(ClockMeasureTTL)},
		{secBell, "less than " + docWords(ExpiringWarnWithin)},
		{secLimits, failed},
	}
}

// errorClassesOf reads the class names from the ERROR_CLASSES list of the HTTPS page's source.
func errorClassesOf(t *testing.T, src string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)ERROR_CLASSES = \[(.*?)\] as const`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("ERROR_CLASSES not found in web/src/features/https/copy.ts")
	}
	var out []string
	for _, c := range regexp.MustCompile(`"(\w+)"`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, c[1])
	}
	return out
}

// errorCopyText is one class's headline and fix in https.json.
type errorCopyText struct{ Headline, Fix string }

// copyRegexp matches a string of UI copy where the page fills a {{name}} in: the guide writes the
// value in words, so any text without a table cell break stands for it.
func copyRegexp(copy string) *regexp.Regexp {
	parts := regexp.MustCompile(`\{\{\w+\}\}`).Split(copy, -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(strings.Join(parts, `[^|]+?`))
}

// tableRows are the rows of a markdown table in a section that begin with a backticked first cell, by
// that cell's text.
func tableRows(section string) map[string]string {
	rows := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "| `") {
			if cell, _, ok := strings.Cut(line[3:], "`"); ok {
				rows[cell] = line
			}
		}
	}
	return rows
}

// checkClassTable reports a class of ERROR_CLASSES the table misses, a class it names that is not
// there, and a row whose headline or fix is not the page's copy.
func checkClassTable(section string, classes []string, copy map[string]errorCopyText) []string {
	var bad []string
	rows := tableRows(section)
	known := map[string]bool{}
	for _, c := range classes {
		known[c] = true
		row, ok := rows[c]
		if !ok {
			bad = append(bad, "the troubleshooting table has no row for "+c)
			continue
		}
		if !copyRegexp(copy[c].Headline).MatchString(row) {
			bad = append(bad, fmt.Sprintf("%s: the row does not hold the headline %q", c, copy[c].Headline))
		}
		if !copyRegexp(copy[c].Fix).MatchString(row) {
			bad = append(bad, fmt.Sprintf("%s: the row does not hold the fix %q", c, copy[c].Fix))
		}
	}
	for c := range rows {
		if !known[c] {
			bad = append(bad, "the troubleshooting table names "+c+", which is not an error class")
		}
	}
	return bad
}

// bellKeyOf is where bell.json keeps each https_ warning's titles. A warning with no entry here is a
// new one the guide has to learn about.
var bellKeyOf = map[string]string{
	"https_cert_fallback":  "fallback",
	"https_renewal_failed": "renewalFailed",
	"https_acme_paused":    "paused",
	"https_clock_off":      "clock",
	"https_cert_expiring":  "expiring",
	"https_txt_left":       "txtLeft",
}

// httpsWarningIDs are the https_ warning IDs of the WarningBody.ID enum.
func httpsWarningIDs(t *testing.T, src string) []string {
	t.Helper()
	m := regexp.MustCompile(`ID\s+string\s+` + "`" + `json:"id" enum:"([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the WarningBody.ID enum was not found in internal/api/account.go")
	}
	var out []string
	for _, id := range strings.Split(m[1], ",") {
		if strings.HasPrefix(id, "https_") {
			out = append(out, id)
		}
	}
	return out
}

// checkBellSection reports a warning ID with no row, a row for an ID that is not an https_ warning, and
// a row that lacks one of the warning's titles.
func checkBellSection(section string, ids []string, titles map[string][]string) []string {
	var bad []string
	rows := tableRows(section)
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
		row, ok := rows[id]
		if !ok {
			bad = append(bad, "the bell table has no row for "+id)
			continue
		}
		for _, title := range titles[id] {
			if !strings.Contains(row, title) {
				bad = append(bad, fmt.Sprintf("%s: the row does not hold the title %q", id, title))
			}
		}
	}
	for id := range rows {
		if !known[id] {
			bad = append(bad, "the bell table names "+id+", which is not an https_ warning")
		}
	}
	return bad
}

// jsonAt walks a decoded JSON document by keys.
func jsonAt(t *testing.T, doc map[string]any, keys ...string) any {
	t.Helper()
	var cur any = doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("no object at %v", keys)
		}
		if cur, ok = m[k]; !ok {
			t.Fatalf("no key %v", keys)
		}
	}
	return cur
}

// refusalCodes are the preflight refusal codes of the HTTPSACMERefusalBody.Code enum.
func refusalCodes(t *testing.T, src string) []string {
	t.Helper()
	m := regexp.MustCompile(`enum:"([^"]*)" doc:"Why the order is refused"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the refusal code enum was not found in internal/api/https_acme.go")
	}
	return strings.Split(m[1], ",")
}

// checkRefusals reports a refusal code the section does not name in code formatting.
func checkRefusals(section string, codes []string) []string {
	var bad []string
	for _, c := range codes {
		if !strings.Contains(section, "`"+c+"`") {
			bad = append(bad, "the HTTP-01 section does not explain the refusal "+c)
		}
	}
	return bad
}

// checkProxyPage reports a reverse-proxy page without the "Proxy or TierPlan's own HTTPS?" section above
// "The short version", or one that does not link the guide.
func checkProxyPage(proxy string) []string {
	var bad []string
	text := "\n" + proxy
	at := strings.Index(text, "\n## Proxy or TierPlan's own HTTPS?\n")
	short := strings.Index(text, "\n## The short version\n")
	if at < 0 {
		bad = append(bad, `docs/reverse-proxy.md has no "## Proxy or TierPlan's own HTTPS?" section`)
	} else if short >= 0 && at > short {
		bad = append(bad, `the "Proxy or TierPlan's own HTTPS?" section is below "The short version"`)
	}
	sec := mdSection(proxy, "Proxy or TierPlan's own HTTPS?")
	if sec != "" && !strings.Contains(sec, "](https.md") {
		bad = append(bad, "the proxy section does not link https.md")
	}
	return bad
}

// checkGuideLinksProxy reports a guide that does not link the proxy page.
func checkGuideLinksProxy(guide string) []string {
	if !strings.Contains(guide, "](reverse-proxy.md") {
		return []string{"docs/https.md does not link reverse-proxy.md"}
	}
	return nil
}

// checkReadmeTruth reports the stale sentence about the challenge path and a missing row for
// TIERPLAN_REGISTRY_BLOCK_CIDRS.
func checkReadmeTruth(readme string) []string {
	var bad []string
	if strings.Contains(readme, "until Let's Encrypt support lands") {
		bad = append(bad, "README.md still says the challenge path answers 404 until Let's Encrypt support lands")
	}
	if !strings.Contains(readme, "| `TIERPLAN_REGISTRY_BLOCK_CIDRS` |") {
		bad = append(bad, "README.md has no row for TIERPLAN_REGISTRY_BLOCK_CIDRS")
	}
	return bad
}

// stringValues collects every string value of a decoded JSON document.
func stringValues(v any, into map[string]bool) {
	switch x := v.(type) {
	case string:
		into[x] = true
	case map[string]any:
		for _, e := range x {
			stringValues(e, into)
		}
	}
}

var boldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)

// checkBoldLabels reports a bold span of the guide that is not a string of the UI. A span with " > " is a
// navigation path, and each of its segments must be one.
func checkBoldLabels(doc string, ui map[string]bool) []string {
	var bad []string
	for _, line := range mdProse(doc) {
		for _, m := range boldRe.FindAllStringSubmatch(line, -1) {
			span := m[1]
			if strings.Contains(span, " > ") {
				for _, seg := range strings.Split(span, " > ") {
					if !ui[strings.TrimSpace(seg)] {
						bad = append(bad, fmt.Sprintf("**%s**: %q is not a label in the UI strings", span, strings.TrimSpace(seg)))
					}
				}
				continue
			}
			if !ui[span] {
				bad = append(bad, fmt.Sprintf("**%s** is not a label in the UI strings", span))
			}
		}
	}
	return bad
}

// checkRequiredLabels reports a label the guide must show in bold and does not.
func checkRequiredLabels(doc string, labels []string) []string {
	var bad []string
	for _, l := range labels {
		if !strings.Contains(doc, "**"+l+"**") {
			bad = append(bad, fmt.Sprintf("the guide does not show **%s** in bold", l))
		}
	}
	return bad
}

var (
	listenDefaultRe = regexp.MustCompile(`env: "TIERPLAN_LISTEN"[^\n]*def: ":(\d+)"`)
	dashPortRe      = regexp.MustCompile(`-p (\d+):(\d+)`)
	quotedPortRe    = regexp.MustCompile(`"(\d+):(\d+)"`)
)

// checkPorts reports a port mapping whose container port is not the listen port, and an env section that
// does not name it.
func checkPorts(doc, envSection, listen string) []string {
	var bad []string
	for _, re := range []*regexp.Regexp{dashPortRe, quotedPortRe} {
		for _, m := range re.FindAllStringSubmatch(doc, -1) {
			if m[2] != listen {
				bad = append(bad, fmt.Sprintf("the port mapping %s:%s does not end at TierPlan's port %s", m[1], m[2], listen))
			}
		}
	}
	if !strings.Contains(envSection, listen) {
		bad = append(bad, "the environment section does not name port "+listen)
	}
	return bad
}

func TestHTTPSDocs(t *testing.T) {
	guide := repoText(t, "docs/https.md")
	readme := repoText(t, "README.md")
	proxy := repoText(t, "docs/reverse-proxy.md")
	configSrc := repoText(t, "internal/config/config.go")

	if !strings.HasPrefix(guide, "<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->\n<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->\n") {
		t.Error("docs/https.md does not start with the two SPDX comment lines")
	}

	envCheck := func(s string) []string { return checkEnvNames(s, configSrc) }
	expectBad(t, "env names", envCheck, "Set TIERPLAN_MADE_UP_NAME=on to begin.")
	expectClean(t, "env names", envCheck, guide)

	linkCheck := func(s string) []string { return checkLinksResolve(s, "docs") }
	expectBad(t, "links/missing file", linkCheck, "See [x](no-such-page.md).")
	expectBad(t, "links/missing anchor", linkCheck, "See [x](upgrade.md#no-such-heading).")
	expectBad(t, "links/missing anchor in proxy page", linkCheck, "See [x](reverse-proxy.md#no-such-heading).")
	expectClean(t, "links", linkCheck, guide)

	planning := func(dir string) func(string) []string {
		return func(s string) []string { return checkNoPlanningLinks(s, dir) }
	}
	// ../.planning/STATE.md exists in a working tree; the rule is on the path.
	expectBad(t, "planning links/docs", planning("docs"), "See [state](../.planning/STATE.md).")
	expectBad(t, "planning links/claude dir", planning("docs"), "See [x](../.claude/CLAUDE.md).")
	expectBad(t, "planning links/CLAUDE.md", planning("docs"), "See [x](../CLAUDE.md).")
	expectBad(t, "planning links/readme", planning("."), "See [x](.planning/STATE.md).")
	expectClean(t, "planning links/guide", planning("docs"), guide)
	expectClean(t, "planning links/readme", planning("."), readme)
	expectClean(t, "planning links/proxy", planning("docs"), proxy)

	expectBad(t, "safe examples/skip verify", checkSafeExamples, "tls_insecure_skip_verify")
	expectBad(t, "safe examples/proxy off", checkSafeExamples, "proxy_ssl_verify off;")
	expectBad(t, "safe examples/plain token", checkSafeExamples, "      TIERPLAN_CLOUDFLARE_API_TOKEN: abc123")
	expectBad(t, "safe examples/plain token env", checkSafeExamples, "-e TIERPLAN_CLOUDFLARE_API_TOKEN=abc123")
	expectClean(t, "safe examples", checkSafeExamples, guide)
	t.Run("safe examples/file form is fine", func(t *testing.T) {
		if got := checkSafeExamples("TIERPLAN_CLOUDFLARE_API_TOKEN_FILE: /run/secrets/cloudflare_token"); len(got) != 0 {
			t.Fatalf("the _FILE form was refused: %v", got)
		}
	})

	expectBad(t, "readme", checkReadme, "| `TIERPLAN_ACME` | unset | no | Turns it on. |")
	expectClean(t, "readme", checkReadme, readme)

	// The sections of the guide, in D-15 order.
	t.Run("sections", func(t *testing.T) {
		want := []string{
			"How TierPlan serves HTTPS by default", "Choosing a mode", secHTTP01,
			"Let's Encrypt with DNS-01 through Cloudflare", "Configuring everything with environment variables",
			secHSTS, secRenewal, secBell, secTrouble, secLimits,
		}
		var got []string
		for _, l := range strings.Split(guide, "\n") {
			if strings.HasPrefix(l, "## ") {
				got = append(got, strings.TrimPrefix(l, "## "))
			}
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("the guide's sections are\n%q\nwant\n%q", got, want)
		}
	})

	// The error classes and their copy.
	classes := errorClassesOf(t, repoText(t, "web/src/features/https/copy.ts"))
	if len(classes) != 20 {
		t.Fatalf("ERROR_CLASSES holds %d classes, the guide was written for 20", len(classes))
	}
	var httpsJSON map[string]any
	if err := json.Unmarshal([]byte(repoText(t, "web/src/i18n/en/https.json")), &httpsJSON); err != nil {
		t.Fatal(err)
	}
	copyOf := map[string]errorCopyText{}
	for _, c := range classes {
		e, _ := jsonAt(t, httpsJSON, "https", "error", c).(map[string]any)
		copyOf[c] = errorCopyText{Headline: fmt.Sprint(e["headline"]), Fix: fmt.Sprint(e["fix"])}
	}
	classCheck := func(s string) []string { return checkClassTable(mdSection(s, secTrouble), classes, copyOf) }
	expectBad(t, "class table/missing class", classCheck, "## "+secTrouble+"\n\n| `rate_limited` | x | y |\n")
	expectBad(t, "class table/made-up class", classCheck, strings.Replace(guide, "| `internal` |", "| `made_up_class` |", 1))
	expectBad(t, "class table/changed fix", classCheck, strings.Replace(guide, "Check your firewall and router, then try again.", "Wait.", 1))
	expectBad(t, "class table/changed headline", classCheck, strings.Replace(guide, "Let's Encrypt is limiting requests.", "Slow down.", 1))
	expectClean(t, "class table", classCheck, guide)

	// The bell warnings and their titles.
	ids := httpsWarningIDs(t, repoText(t, "internal/api/account.go"))
	if len(ids) != 6 {
		t.Fatalf("the warnings enum holds %d https_ IDs, the guide was written for 6", len(ids))
	}
	var bellJSON map[string]any
	if err := json.Unmarshal([]byte(repoText(t, "web/src/i18n/en/bell.json")), &bellJSON); err != nil {
		t.Fatal(err)
	}
	titles := map[string][]string{}
	for _, id := range ids {
		key, ok := bellKeyOf[id]
		if !ok {
			t.Fatalf("%s has no entry in bellKeyOf: add its bell.json key and document it in the guide", id)
		}
		entry, _ := jsonAt(t, bellJSON, "bell", "https", key).(map[string]any)
		for k, v := range entry {
			if strings.HasPrefix(k, "title") {
				titles[id] = append(titles[id], fmt.Sprint(v))
			}
		}
		if len(titles[id]) == 0 {
			t.Fatalf("%s has no title in bell.json", id)
		}
	}
	bellCheck := func(s string) []string { return checkBellSection(mdSection(s, secBell), ids, titles) }
	expectBad(t, "bell table/missing id", bellCheck, strings.Replace(guide, "| `https_txt_left` |", "| `https_other` |", 1))
	expectBad(t, "bell table/changed title", bellCheck, strings.Replace(guide, "The server clock is off", "The clock is wrong", 1))
	expectBad(t, "bell table/changed second title", bellCheck, strings.Replace(guide, "The certificate couldn't be issued", "The order failed", 1))
	expectClean(t, "bell table", bellCheck, guide)

	// The preflight refusals.
	codes := refusalCodes(t, repoText(t, "internal/api/https_acme.go"))
	refusalCheck := func(s string) []string { return checkRefusals(mdSection(s, secHTTP01), codes) }
	expectBad(t, "refusals", refusalCheck, strings.Replace(guide, "`base_path`", "base path", 1))
	expectClean(t, "refusals", refusalCheck, guide)

	// The numbers of this package.
	rows := acmeNumbers()
	numberCheck := func(s string) []string { return checkNumbers(s, rows) }
	expectBad(t, "numbers/missing section", numberCheck, "# nothing here")
	expectClean(t, "numbers", numberCheck, guide)
	checkNumbersBite(t, "numbers", guide, rows)

	// The proxy page and the README.
	proxyBelow := "## The short version\n\n## Proxy or TierPlan's own HTTPS?\n\nSee [x](https.md).\n"
	expectBad(t, "proxy section/missing", checkProxyPage, "## The short version\n")
	expectBad(t, "proxy section/below", checkProxyPage, proxyBelow)
	expectBad(t, "proxy section/no link", checkProxyPage, "## Proxy or TierPlan's own HTTPS?\n\nUse a proxy.\n\n## The short version\n")
	expectClean(t, "proxy section", checkProxyPage, proxy)
	expectBad(t, "guide links proxy", checkGuideLinksProxy, "No link here.")
	expectClean(t, "guide links proxy", checkGuideLinksProxy, guide)
	expectBad(t, "readme truth/stale", checkReadmeTruth, "answers 404 until Let's Encrypt support lands | `TIERPLAN_REGISTRY_BLOCK_CIDRS` |")
	expectBad(t, "readme truth/no row", checkReadmeTruth, "nothing")
	expectClean(t, "readme truth", checkReadmeTruth, readme)

	// Every bold span is a UI label, and the labels the guide depends on are there.
	ui := map[string]bool{}
	for _, f := range []string{"https", "settings", "bell"} {
		var doc map[string]any
		if err := json.Unmarshal([]byte(repoText(t, "web/src/i18n/en/"+f+".json")), &doc); err != nil {
			t.Fatal(err)
		}
		stringValues(doc, ui)
	}
	boldCheck := func(s string) []string { return checkBoldLabels(s, ui) }
	expectBad(t, "bold labels/short form", boldCheck, "Press **Resume** to go on.")
	expectBad(t, "bold labels/invented", boldCheck, "Press **Test the token** to go on.")
	expectBad(t, "bold labels/path segment", boldCheck, "Open **Settings > TLS**.")
	expectBad(t, "bold labels/emphasis", boldCheck, "This is **important**.")
	expectClean(t, "bold labels", boldCheck, guide)
	var settingsJSON map[string]any
	if err := json.Unmarshal([]byte(repoText(t, "web/src/i18n/en/settings.json")), &settingsJSON); err != nil {
		t.Fatal(err)
	}
	str := func(doc map[string]any, keys ...string) string {
		v, ok := jsonAt(t, doc, keys...).(string)
		if !ok {
			t.Fatalf("%v is not a string", keys)
		}
		return v
	}
	// The labels the guide depends on, by their key in the UI strings (never as literals).
	var required []string
	for _, path := range []string{
		"https.dns.test.action", "https.issue.issue", "https.issue.renew", "https.alerts.restore.resume", "https.stop.action",
	} {
		required = append(required, str(httpsJSON, strings.Split(path, ".")...))
	}
	required = append(required, str(settingsJSON, "settings", "title")+" > "+str(settingsJSON, "settings", "tabs", "https"))
	requiredCheck := func(s string) []string { return checkRequiredLabels(s, required) }
	expectBad(t, "required labels/not bold", requiredCheck, strings.ReplaceAll(guide, "**"+required[3]+"**", required[3]))
	expectBad(t, "required labels/changed", requiredCheck, strings.ReplaceAll(guide, "**"+required[0]+"**", "**Check token**"))
	expectBad(t, "required labels/path", requiredCheck, strings.ReplaceAll(guide, "**"+required[5]+"**", "**Settings > TLS**"))
	expectClean(t, "required labels", requiredCheck, guide)

	// The listen port and the resolver limit.
	lm := listenDefaultRe.FindStringSubmatch(configSrc)
	if lm == nil {
		t.Fatal("the TIERPLAN_LISTEN default was not found in config.go")
	}
	listen := lm[1]
	portCheck := func(s string) []string {
		return checkPorts(s, mdSection(s, "Configuring everything with environment variables"), listen)
	}
	expectBad(t, "ports/other container port", func(s string) []string { return checkPorts(s, "7480", listen) }, "-p 80:8080")
	expectBad(t, "ports/compose entry", func(s string) []string { return checkPorts(s, "7480", listen) }, `    - "8080:8080"`)
	expectBad(t, "ports/not named", func(s string) []string { return checkPorts(s, "no port here", listen) }, "")
	expectClean(t, "ports", portCheck, guide)
	const secDNS01 = "Let's Encrypt with DNS-01 through Cloudflare"
	// The record name and the comment prefix the guide quotes come from the DNS package.
	commentPrefix, _, _ := strings.Cut(dns.RecordComment("x", "y"), "x")
	resolverRows := []docNumber{
		{secDNS01, fmt.Sprintf("at most %d", resolvers.MaxResolvers)},
		{secDNS01, "`" + dns.ChallengeFQDN("<your host>") + "`"},
		{secDNS01, "starts with `" + commentPrefix + "`"},
		// The two Cloudflare permissions, as the HTTPS page names them.
		{secDNS01, str(httpsJSON, "https", "dns", "setup", "permEdit")},
		{secDNS01, str(httpsJSON, "https", "dns", "setup", "permRead")},
	}
	resolverCheck := func(s string) []string { return checkNumbers(s, resolverRows) }
	expectClean(t, "dns-01 numbers", resolverCheck, guide)
	checkNumbersBite(t, "dns-01 numbers", guide, resolverRows)
}

func TestDocWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Minute: "1 minute", 5 * time.Minute: "5 minutes", 100 * time.Minute: "100 minutes", time.Hour: "1 hour",
		24 * time.Hour: "24 hours", 48 * time.Hour: "2 days", 10 * 24 * time.Hour: "10 days", 7 * 24 * time.Hour: "1 week",
		30 * 24 * time.Hour: "30 days", 365 * 24 * time.Hour: "1 year", 90 * time.Second: "90 seconds",
	} {
		if got := docWords(d); got != want {
			t.Errorf("docWords(%v) = %q, want %q", d, got, want)
		}
	}
}
