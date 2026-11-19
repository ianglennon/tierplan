// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"maps"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
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
	}
	redirect := section(t, section(t, section(t, section(t, eps, "web"), "http"), "redirections"), "entryPoint")
	if got := sortedKeys(redirect); !slices.Equal(got, []string{"permanent", "scheme", "to"}) {
		t.Errorf("redirect keys = %v", got)
	}
	expect(t, "redirect.to", redirect["to"], ":443")
	expect(t, "redirect.scheme", redirect["scheme"], "https")
	expect(t, "redirect.permanent", redirect["permanent"], true)
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
