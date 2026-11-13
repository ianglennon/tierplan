// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

// These tests keep docs/https.md true. Each check is a pure function over strings. A subtest feeds it a
// snippet that breaks the rule (it must report a problem) and then the real file (it must report none),
// so a check that never bites cannot pass. The guide quotes names, numbers and labels; every one of them
// is tied to the code or the UI strings here, so the guide cannot drift from the product (Phase 17, D-19).

// repoText reads a file of the repository by its path from the repository root. The tests of this
// package run in internal/acme, so the root is two levels up.
func repoText(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
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
			b, err := os.ReadFile("../../" + rel)
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
}
