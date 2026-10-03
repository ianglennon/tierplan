// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ianglennon/bayplan/internal/webui"
)

const fixtureIndex = `<!doctype html><html><head><meta charset="utf-8">` +
	`<script type="module" src="./theme-init.js"></script>` +
	`<script type="module" src="./assets/app.js"></script>` +
	`<link rel="stylesheet" href="./assets/app.css"></head><body><div id="root"></div></body></html>`

func fixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte(fixtureIndex)},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
}

func fetch(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestIndexForBasePathRewritesAssetsAndAddsTheMeta(t *testing.T) {
	h := webui.Handler(fixture(), func() string { return "/bayplan" })
	rec := fetch(t, h, "/")
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="bayplan-base" content="/bayplan">`,
		`src="/bayplan/assets/app.js"`,
		`src="/bayplan/theme-init.js"`,
		`href="/bayplan/assets/app.css"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index at /bayplan is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"./`) {
		t.Errorf("a relative reference survived:\n%s", body)
	}
	if strings.Contains(body, "<base") {
		t.Errorf("a <base> element was added (the CSP base-uri 'none' would ignore it):\n%s", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if !strings.HasPrefix(body, `<!doctype html><html><head><meta name="bayplan-base"`) {
		t.Errorf("the meta must come immediately after <head>:\n%s", body)
	}
}

func TestIndexForBasePathEmptyBaseUsesRootURLs(t *testing.T) {
	h := webui.Handler(fixture(), func() string { return "" })
	body := fetch(t, h, "/").Body.String()
	for _, want := range []string{
		`<meta name="bayplan-base" content="">`,
		`src="/assets/app.js"`,
		`src="/theme-init.js"`,
		`href="/assets/app.css"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index at base \"\" is missing %q:\n%s", want, body)
		}
	}
}

func TestIndexForBasePathEscapesTheBase(t *testing.T) {
	h := webui.Handler(fixture(), func() string { return `/a"><script>x</script>` })
	body := fetch(t, h, "/").Body.String()
	if strings.Contains(body, `<script>x</script>`) || strings.Contains(body, `a">`) {
		t.Fatalf("the base was inserted unescaped:\n%s", body)
	}
	if !strings.Contains(body, `content="/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;"`) {
		t.Fatalf("the escaped base is missing:\n%s", body)
	}
}

func TestIndexForBasePathServesSPARoutesAndAssetsAsBefore(t *testing.T) {
	h := webui.Handler(fixture(), func() string { return "/bayplan" })
	if body := fetch(t, h, "/environments").Body.String(); !strings.Contains(body, "bayplan-base") {
		t.Errorf("a client-side route must get the rendered index:\n%s", body)
	}
	rec := fetch(t, h, "/assets/app.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Errorf("a real asset is served untouched: %d %q", rec.Code, rec.Body.String())
	}
	if rec := fetch(t, h, "/assets/missing.js"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing asset = %d, want 404", rec.Code)
	}
}

func TestIndexForBasePathFollowsTheBaseAndReusesTheRenderedCopy(t *testing.T) {
	base := "/one"
	h := webui.Handler(fixture(), func() string { return base })
	first := fetch(t, h, "/").Body.String()
	again := fetch(t, h, "/").Body.String()
	if first != again || !strings.Contains(first, `src="/one/assets/app.js"`) {
		t.Fatalf("same base must render the same bytes:\n%s\n%s", first, again)
	}
	base = "/two"
	if body := fetch(t, h, "/").Body.String(); !strings.Contains(body, `src="/two/assets/app.js"`) {
		t.Fatalf("a changed base must be re-rendered:\n%s", body)
	}
	base = "/one"
	if body := fetch(t, h, "/").Body.String(); body != first {
		t.Fatalf("returning to the first base must give the first rendering")
	}
}

func TestIndexForBasePathBoundsTheCache(t *testing.T) {
	base := ""
	h := webui.Handler(fixture(), func() string { return base })
	for _, b := range []string{"/a", "/b", "/c", "/d", "/e", "/f"} {
		base = b
		if body := fetch(t, h, "/").Body.String(); !strings.Contains(body, `src="`+b+`/assets/app.js"`) {
			t.Fatalf("base %s: wrong rendering:\n%s", b, body)
		}
	}
	base = "/a"
	if body := fetch(t, h, "/").Body.String(); !strings.Contains(body, `src="/a/assets/app.js"`) {
		t.Fatalf("an evicted base must be re-rendered correctly:\n%s", body)
	}
}

func TestIndexForBasePathNilBaseMeansRoot(t *testing.T) {
	h := webui.Handler(fixture(), nil)
	if body := fetch(t, h, "/").Body.String(); !strings.Contains(body, `src="/assets/app.js"`) {
		t.Fatalf("a nil base func must behave as the empty base:\n%s", body)
	}
}
