// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package keepalived_test pins the shape of the keepalived image (D-26, D-27, D-28):
// keepalived/Dockerfile is the only Dockerfile that may carry GPL code, and the image
// built from it has no shell, no busybox and no package manager. The directory holds
// no Go code of its own, so the tests are a test-only package of the root module.
package keepalived_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	// keepalivedSHA256 is the D-26 hash of keepalived-2.4.3.tar.gz.
	keepalivedSHA256 = "a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632"
	// alpineBase is the pinned Alpine 3.24 multi-arch index the C stage builds on.
	alpineBase = "alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf"
	// keepalivedSource is where the tarball comes from; the image labels name it.
	keepalivedSource = "https://www.keepalived.org/software/keepalived-2.4.3.tar.gz"
)

// stage is one FROM of a Dockerfile: its base reference and its instructions with the
// comments dropped and continuation lines joined, one string per instruction.
type stage struct {
	base  string
	lines []string
}

// readFile reads a repository file relative to this directory.
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a fixed repository file named by this test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// parseStages splits a Dockerfile at each FROM. Comment lines are dropped (they talk about
// shells and packages in prose) and a trailing backslash joins a line to the next one.
func parseStages(t *testing.T, text string) []stage {
	t.Helper()
	var stages []stage
	var joined string
	flush := func() {
		line := strings.TrimSpace(joined)
		joined = ""
		if line == "" {
			return
		}
		if rest, ok := strings.CutPrefix(line, "FROM "); ok {
			base := strings.Fields(rest)[0]
			stages = append(stages, stage{base: base})
			return
		}
		if len(stages) == 0 {
			// ARG before the first FROM, or similar: not part of a stage.
			return
		}
		s := &stages[len(stages)-1]
		s.lines = append(s.lines, line)
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if joined == "" && strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			joined += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		joined += line
		flush()
	}
	flush()
	if len(stages) == 0 {
		t.Fatal("the Dockerfile has no FROM line")
	}
	return stages
}

// finalBase returns the base reference of the last stage of the Dockerfile at path.
func finalBase(t *testing.T, path string) string {
	t.Helper()
	stages := parseStages(t, readFile(t, path))
	return stages[len(stages)-1].base
}

// has reports whether some instruction of s satisfies re.
func (s stage) has(re *regexp.Regexp) bool {
	for _, l := range s.lines {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// TestKeepalivedDockerfileShape fails unless the keepalived Dockerfile has the shape D-26 gives it: a
// pinned Alpine build stage that checks the tarball's hash, and a final stage that is the same distroless
// static digest as the other three images, with no RUN, no package manager and no shell, carrying the
// licence text, the labels that name the source, and the helper as its entrypoint.
func TestKeepalivedDockerfileShape(t *testing.T) {
	stages := parseStages(t, readFile(t, "Dockerfile"))
	if len(stages) != 2 {
		t.Fatalf("the Dockerfile has %d stages, want a build stage and a final stage", len(stages))
	}
	build, final := stages[0], stages[1]

	// The final stage is the digest the other images use, read from the ingress Dockerfile so the four stay equal.
	want := finalBase(t, "../ingress/Dockerfile")
	if !strings.HasPrefix(want, "gcr.io/distroless/static-debian13@sha256:") {
		t.Fatalf("ingress/Dockerfile's final base is %q, not the distroless static digest", want)
	}
	if final.base != want {
		t.Errorf("the final stage is FROM %s, the other images use %s", final.base, want)
	}
	for _, line := range final.lines {
		switch {
		case strings.HasPrefix(line, "RUN "):
			t.Errorf("the final stage has a RUN: %s", line)
		case regexp.MustCompile(`\bapk\b`).MatchString(line):
			t.Errorf("the final stage names apk: %s", line)
		case strings.Contains(line, "busybox"), strings.Contains(line, "/bin/sh"), strings.Contains(line, "/bin/ash"), strings.Contains(line, "/bin/bash"):
			t.Errorf("the final stage copies in a shell: %s", line)
		}
	}

	// The build stage is the pinned Alpine and checks the tarball against the D-26 hash.
	if build.base != alpineBase {
		t.Errorf("the build stage is FROM %s, want %s", build.base, alpineBase)
	}
	if !build.has(regexp.MustCompile(`sha256sum -c`)) || !build.has(regexp.MustCompile(regexp.QuoteMeta(keepalivedSHA256)+`\s+/src/ka\.tgz`)) {
		t.Errorf("the build stage does not run sha256sum -c with the D-26 hash on the copied tarball")
	}

	// The licence text of keepalived crosses into the final stage.
	if !final.has(regexp.MustCompile(`^COPY --from=build /src/keepalived-2\.4\.3/COPYING /licenses/keepalived/COPYING$`)) {
		t.Errorf("the final stage does not copy COPYING to /licenses/keepalived/COPYING")
	}

	// The OCI labels (D-26): the source, the two licences and the two keepalived source labels.
	var labels string
	for _, l := range final.lines {
		if strings.HasPrefix(l, "LABEL ") {
			labels += " " + l
		}
	}
	for _, w := range []string{
		`org.opencontainers.image.source="https://github.com/ianglennon/tierplan"`,
		`org.opencontainers.image.licenses="AGPL-3.0-or-later AND GPL-2.0-or-later"`,
		`org.tierplan.keepalived.source="` + keepalivedSource + `"`,
		`org.tierplan.keepalived.sha256="` + keepalivedSHA256 + `"`,
	} {
		if !strings.Contains(labels, w) {
			t.Errorf("the final stage has no label %s", w)
		}
	}

	// The evidence the image carries (D-27): the filled SBOM entry and the notice.
	if !final.has(regexp.MustCompile(`^COPY --from=build /out/sbom/keepalived\.spdx\.json /usr/share/tierplan/sbom/keepalived\.spdx\.json$`)) {
		t.Errorf("the final stage does not copy the filled SBOM to /usr/share/tierplan/sbom/keepalived.spdx.json")
	}
	if !final.has(regexp.MustCompile(`^COPY keepalived/image/doc/NOTICE\.md /usr/share/doc/tierplan-keepalived/NOTICE\.md$`)) {
		t.Errorf("the final stage does not copy the notice to /usr/share/doc/tierplan-keepalived/")
	}
	// The build stage fills the template and fails if a placeholder is left.
	if !build.has(regexp.MustCompile(`COPY keepalived/image/sbom/keepalived\.spdx\.json\.in `)) {
		t.Errorf("the build stage does not copy the SBOM template")
	}
	if !build.has(regexp.MustCompile(`apk list -I openssl-libs-static`)) || !build.has(regexp.MustCompile(`@OPENSSL_VERSION@`)) {
		t.Errorf("the build stage does not read OpenSSL's version from the installed openssl-libs-static package")
	}
	if !build.has(regexp.MustCompile(`apk list -I musl-dev`)) || !build.has(regexp.MustCompile(`@MUSL_VERSION@`)) {
		t.Errorf("the build stage does not read musl's version from the installed musl-dev package")
	}
	if !build.has(regexp.MustCompile(`@CREATED@`)) || !build.has(regexp.MustCompile(`SOURCE_DATE_EPOCH`)) {
		t.Errorf("the build stage does not fill the created time from SOURCE_DATE_EPOCH")
	}
	if !build.has(regexp.MustCompile(`grep -Eq '@\[A-Z_\]\+@'`)) {
		t.Errorf("the build stage does not fail when a placeholder is left in the SBOM")
	}

	// The helper is PID 1, there is no USER (VRRP needs root and raw sockets) and no HEALTHCHECK (A-08).
	if !final.has(regexp.MustCompile(`^ENTRYPOINT \["/usr/local/bin/tierplan-ingress", "vip-entrypoint"\]$`)) {
		t.Errorf(`the final stage's ENTRYPOINT is not ["/usr/local/bin/tierplan-ingress", "vip-entrypoint"]`)
	}
	if final.has(regexp.MustCompile(`^USER `)) {
		t.Errorf("the final stage sets a USER; VRRP needs root")
	}
	if final.has(regexp.MustCompile(`^HEALTHCHECK `)) {
		t.Errorf("the final stage sets a HEALTHCHECK; nothing in the image can run one but the helper")
	}
}

// gplName matches gpl and lgpl in lower-case text, and not the "agpl" of AGPL-3.0-or-later.
var gplName = regexp.MustCompile(`(^|[^a])gpl`)

// TestOnlyTheKeepalivedImageCarriesGPL fails if the server, agent or ingress Dockerfile names keepalived,
// the keepalived licence directory or the GPL (VIP-06, PROH-26-05). The go-licenses gate scans Go and npm
// only, and keepalived is neither, so it needs no GPL allowance: this test is what keeps the GPL out.
func TestOnlyTheKeepalivedImageCarriesGPL(t *testing.T) {
	for _, path := range []string{"../Dockerfile", "../agent/Dockerfile", "../ingress/Dockerfile"} {
		text := strings.ToLower(readFile(t, path))
		for _, w := range []string{"keepalived", "/licenses/keepalived"} {
			if strings.Contains(text, w) {
				t.Errorf("%s names %q; GPL code belongs in keepalived/Dockerfile alone", path, w)
			}
		}
		// "gpl" as a licence name (GPL, LGPL) but not inside AGPL, the licence of every header here.
		if m := gplName.FindString(text); m != "" {
			t.Errorf("%s names the GPL (%q); GPL code belongs in keepalived/Dockerfile alone", path, strings.TrimSpace(m))
		}
	}
}

// spdxDoc is the part of an SPDX 2.3 JSON document the template test reads.
type spdxDoc struct {
	SPDXVersion string `json:"spdxVersion"`
	Packages    []struct {
		Name             string `json:"name"`
		VersionInfo      string `json:"versionInfo"`
		DownloadLocation string `json:"downloadLocation"`
		LicenseConcluded string `json:"licenseConcluded"`
		LicenseDeclared  string `json:"licenseDeclared"`
		Checksums        []struct {
			Algorithm string `json:"algorithm"`
			Value     string `json:"checksumValue"`
		} `json:"checksums"`
	} `json:"packages"`
	Relationships []struct {
		Element string `json:"spdxElementId"`
		Type    string `json:"relationshipType"`
		Related string `json:"relatedSpdxElement"`
	} `json:"relationships"`
}

// TestKeepalivedSBOMTemplate fills the two placeholders of the SBOM template as the build stage does and
// checks the result is SPDX 2.3 JSON that lists keepalived 2.4.3 under GPL-2.0-or-later with the tarball's
// URL and hash, and OpenSSL under Apache-2.0 (D-27).
func TestKeepalivedSBOMTemplate(t *testing.T) {
	text := readFile(t, "image/sbom/keepalived.spdx.json.in")
	for _, p := range []string{"@OPENSSL_VERSION@", "@MUSL_VERSION@", "@CREATED@"} {
		if !strings.Contains(text, p) {
			t.Errorf("the template has no %s placeholder", p)
		}
	}
	filled := strings.NewReplacer("@OPENSSL_VERSION@", "3.5.4-r0", "@MUSL_VERSION@", "1.2.6-r3", "@CREATED@", "2026-07-14T00:00:00Z").Replace(text)
	if m := regexp.MustCompile(`@[A-Z_]+@`).FindString(filled); m != "" {
		t.Errorf("a placeholder %s is left after the three are replaced", m)
	}

	var doc spdxDoc
	if err := json.Unmarshal([]byte(filled), &doc); err != nil {
		t.Fatalf("the filled template is not JSON: %v", err)
	}
	if doc.SPDXVersion != "SPDX-2.3" {
		t.Errorf("spdxVersion is %q, want SPDX-2.3", doc.SPDXVersion)
	}

	var sawKeepalived, sawOpenSSL, sawMusl bool
	for _, p := range doc.Packages {
		switch p.Name {
		case "keepalived":
			sawKeepalived = true
			if p.VersionInfo != "2.4.3" {
				t.Errorf("keepalived versionInfo is %q, want 2.4.3", p.VersionInfo)
			}
			if p.LicenseConcluded != "GPL-2.0-or-later" || p.LicenseDeclared != "GPL-2.0-or-later" {
				t.Errorf("keepalived licences are %q and %q, want GPL-2.0-or-later", p.LicenseConcluded, p.LicenseDeclared)
			}
			if p.DownloadLocation != keepalivedSource {
				t.Errorf("keepalived downloadLocation is %q, want %s", p.DownloadLocation, keepalivedSource)
			}
			var hash bool
			for _, c := range p.Checksums {
				if c.Algorithm == "SHA256" && c.Value == keepalivedSHA256 {
					hash = true
				}
			}
			if !hash {
				t.Errorf("keepalived has no SHA256 checksum equal to the D-26 hash")
			}
		case "openssl":
			sawOpenSSL = true
			if p.LicenseConcluded != "Apache-2.0" || p.LicenseDeclared != "Apache-2.0" {
				t.Errorf("openssl licences are %q and %q, want Apache-2.0", p.LicenseConcluded, p.LicenseDeclared)
			}
			if p.VersionInfo != "3.5.4-r0" {
				t.Errorf("openssl versionInfo is %q, want the filled version", p.VersionInfo)
			}
		case "musl":
			sawMusl = true
			if p.LicenseConcluded != "MIT" || p.LicenseDeclared != "MIT" {
				t.Errorf("musl licences are %q and %q, want MIT", p.LicenseConcluded, p.LicenseDeclared)
			}
			if p.VersionInfo != "1.2.6-r3" {
				t.Errorf("musl versionInfo is %q, want the filled version", p.VersionInfo)
			}
		}
	}
	if !sawKeepalived {
		t.Error("the template lists no keepalived package")
	}
	if !sawOpenSSL {
		t.Error("the template lists no openssl package")
	}
	if !sawMusl {
		t.Error("the template lists no musl package: it is linked statically into the binary")
	}
	// Both libraries are linked into keepalived, and the SBOM says how.
	linked := map[string]bool{}
	for _, r := range doc.Relationships {
		if r.Element == "SPDXRef-Package-keepalived" && r.Type == "STATIC_LINK" {
			linked[r.Related] = true
		}
	}
	for _, want := range []string{"SPDXRef-Package-openssl", "SPDXRef-Package-musl"} {
		if !linked[want] {
			t.Errorf("no STATIC_LINK relationship from keepalived to %s", want)
		}
	}
}
