// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"maps"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// TestStaticConfig pins every key of the fixed static Traefik file (D-03, D-25): the proxy never calls
// home, exposes no management surface, trusts no forwarded headers, redirects port 80 at the
// entrypoint, and reads the dynamic bundle from one directory.
func TestStaticConfig(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(Static(), &doc); err != nil {
		t.Fatalf("the static file does not parse: %v", err)
	}

	// Nothing else: no api, dashboard, metrics, tracing or accessLog.
	if got, want := sortedKeys(doc), []string{"entryPoints", "global", "log", "ping", "providers"}; !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
	assertBaseSections(t, doc)
}

// TestStaticConfigWithAccessLog pins the access-log form (D-31, D-32): every base section is unchanged and
// one accessLog block is added, JSON on stdout with every header dropped, so the value of an Authorization
// or Cookie header can never reach the log (PROH-24-02).
func TestStaticConfigWithAccessLog(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(ingressbundle.StaticTraefikWith(true), &doc); err != nil {
		t.Fatalf("the access-log static file does not parse: %v", err)
	}
	if got, want := sortedKeys(doc), []string{"accessLog", "entryPoints", "global", "log", "ping", "providers"}; !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
	assertBaseSections(t, doc)

	accessLog := section(t, doc, "accessLog")
	if got := sortedKeys(accessLog); !slices.Equal(got, []string{"fields", "format"}) {
		t.Errorf("accessLog keys = %v, want fields and format: no filePath, so the log goes to stdout", got)
	}
	if _, ok := accessLog["filePath"]; ok {
		t.Error("accessLog.filePath is set: the log must go to stdout")
	}
	expect(t, "accessLog.format", accessLog["format"], "json")
	fields := section(t, accessLog, "fields")
	expect(t, "accessLog.fields.defaultMode", fields["defaultMode"], "keep")
	headers := section(t, fields, "headers")
	if got := sortedKeys(headers); !slices.Equal(got, []string{"defaultMode"}) {
		t.Errorf("accessLog.fields.headers keys = %v: no header may be kept by name", got)
	}
	expect(t, "accessLog.fields.headers.defaultMode", headers["defaultMode"], "drop")
}

// assertBaseSections checks every section the base static file has, whether or not an access log is added
// to it. The caller checks the top-level key set, which is what differs between the two forms.
func assertBaseSections(t *testing.T, doc map[string]any) {
	t.Helper()
	global := section(t, doc, "global")
	if got := sortedKeys(global); !slices.Equal(got, []string{"checkNewVersion", "sendAnonymousUsage"}) {
		t.Errorf("global keys = %v", got)
	}
	expect(t, "global.checkNewVersion", global["checkNewVersion"], false)
	expect(t, "global.sendAnonymousUsage", global["sendAnonymousUsage"], false)

	logSec := section(t, doc, "log")
	expect(t, "log.level", logSec["level"], "INFO")
	expect(t, "log.format", logSec["format"], "json")
	if got := sortedKeys(logSec); !slices.Equal(got, []string{"format", "level"}) {
		t.Errorf("log keys = %v (an access log would be a decision of its own)", got)
	}

	eps := section(t, doc, "entryPoints")
	if got := sortedKeys(eps); !slices.Equal(got, []string{"ping", "web", "websecure"}) {
		t.Fatalf("entrypoints = %v, want ping, web, websecure", got)
	}
	for name, addr := range map[string]string{"web": ":8080", "websecure": ":8443", "ping": ":8082"} {
		ep := section(t, eps, name)
		expect(t, "entryPoints."+name+".address", ep["address"], addr)
		if _, ok := ep["forwardedHeaders"]; ok {
			t.Errorf("entryPoints.%s sets forwardedHeaders: the edge trusts no peer (D-14)", name)
		}
		httpSec := section(t, ep, "http")
		if _, ok := httpSec["forwardedHeaders"]; ok {
			t.Errorf("entryPoints.%s.http sets forwardedHeaders: the edge trusts no peer (D-14)", name)
		}
		expect(t, "entryPoints."+name+".http.aliasHeadersStrategy", httpSec["aliasHeadersStrategy"], "delete")

		// The 10 s graceful shutdown is explicit on the two public entrypoints (D-11) and equals the
		// constant the supervisor's stop budget is built on; ping is liveness only and has none.
		if name == "ping" {
			if _, ok := ep["transport"]; ok {
				t.Errorf("entryPoints.ping sets transport: only web and websecure carry a graceful shutdown")
			}
			continue
		}
		lifeCycle := section(t, section(t, ep, "transport"), "lifeCycle")
		expect(t, "entryPoints."+name+".transport.lifeCycle.graceTimeOut", lifeCycle["graceTimeOut"], ingressbundle.TraefikGraceTimeout.String())
	}
	redirect := section(t, section(t, section(t, section(t, eps, "web"), "http"), "redirections"), "entryPoint")
	if got := sortedKeys(redirect); !slices.Equal(got, []string{"permanent", "priority", "scheme", "to"}) {
		t.Errorf("redirect keys = %v", got)
	}
	expect(t, "redirect.to", redirect["to"], ":443")
	expect(t, "redirect.scheme", redirect["scheme"], "https")
	expect(t, "redirect.permanent", redirect["permanent"], true)
	// The redirect is an ordinary router at the lowest priority, so the dynamic carve-out for TierPlan's
	// own challenge path outranks it (Phase 28 S12, A-01). The ACME bypass key the Traefik docs list is
	// not set: 3.7.19 refuses to start with it (F1c).
	expect(t, "redirect.priority", redirect["priority"], 1)
	if got := sortedKeys(section(t, section(t, eps, "web"), "http")); !slices.Equal(got, []string{"aliasHeadersStrategy", "redirections"}) {
		t.Errorf("web http keys = %v", got)
	}

	pingSec := section(t, doc, "ping")
	if got := sortedKeys(pingSec); !slices.Equal(got, []string{"entryPoint"}) {
		t.Errorf("ping keys = %v", got)
	}
	expect(t, "ping.entryPoint", pingSec["entryPoint"], "ping")

	providers := section(t, doc, "providers")
	if got := sortedKeys(providers); !slices.Equal(got, []string{"file", "providersThrottleDuration"}) {
		t.Errorf("providers keys = %v: only the file provider may be on", got)
	}
	expect(t, "providers.providersThrottleDuration", providers["providersThrottleDuration"], "1s")
	file := section(t, providers, "file")
	if got := sortedKeys(file); !slices.Equal(got, []string{"directory"}) {
		t.Errorf("providers.file keys = %v: a single watched directory, no single-file mode", got)
	}
	expect(t, "providers.file.directory", file["directory"], "/var/lib/tierplan-ingress/dynamic")
	// The directory Traefik watches is the one the bundle writes its single dynamic file into.
	expect(t, "providers.file.directory is ingressbundle.DynamicRoot", file["directory"], ingressbundle.DynamicRoot)
}

func TestStaticReturnsACopy(t *testing.T) {
	a := Static()
	a[0] = '!'
	if b := Static(); b[0] == '!' {
		t.Fatal("Static returned the embedded slice: a caller's change reached the next caller")
	}
}

func section(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is missing or not a mapping: %v", key, m[key])
	}
	return v
}

func expect(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v (%T), want %v (%T)", what, got, got, want, want)
	}
}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}
