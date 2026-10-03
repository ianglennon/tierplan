// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package webui serves the single-page app out of an fs.FS (the embedded
// web/dist in production).
package webui

import (
	"bytes"
	"html"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// maxRenderedBases bounds the per-base index cache. The base is the public
// URL's path, so it changes rarely; a handful of entries is plenty.
const maxRenderedBases = 4

// Handler serves files from site, falls back to index.html for any other path
// (client-side routes), and never invents a file under /assets/.
//
// base returns the base path Bayplan is served under, "" for the root (D-50).
// index.html is rendered once per distinct base: a <meta name="bayplan-base">
// is added after <head>, and the relative ./ asset references the SPA build
// emits are rewritten to {base}/. No <base> element is added, because the CSP's
// base-uri 'none' makes browsers ignore it, and no inline script is added, so
// the CSP is unchanged. A nil base means the root.
func Handler(site fs.FS, base func() string) http.Handler {
	idx := &indexCache{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		if name != "index.html" && isFile(site, name) {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			serve(w, r, site, name)
			return
		}

		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}

		// SPA fallback.
		if !isFile(site, "index.html") {
			http.NotFound(w, r)
			return
		}
		b := ""
		if base != nil {
			b = base()
		}
		data, err := idx.render(site, b)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
	})
}

// indexCache holds the rendered index.html per base value, oldest evicted.
type indexCache struct {
	mu    sync.Mutex
	order []string
	byKey map[string][]byte
}

func (c *indexCache) render(site fs.FS, base string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if data, ok := c.byKey[base]; ok {
		return data, nil
	}
	raw, err := fs.ReadFile(site, "index.html")
	if err != nil {
		return nil, err
	}
	data := renderIndex(raw, base)
	if c.byKey == nil {
		c.byKey = make(map[string][]byte, maxRenderedBases)
	}
	if len(c.order) >= maxRenderedBases {
		delete(c.byKey, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, base)
	c.byKey[base] = data
	return data, nil
}

// renderIndex inserts the bayplan-base meta right after <head> and points every
// relative ./ src and href at the base. The base is HTML-escaped first, so a
// public URL cannot break out of the attribute (T-02-13).
func renderIndex(raw []byte, base string) []byte {
	esc := html.EscapeString(base)
	out := raw
	for _, attr := range []string{`src="./`, `href="./`} {
		out = bytes.ReplaceAll(out, []byte(attr), []byte(strings.TrimSuffix(attr, "./")+esc+"/"))
	}
	meta := []byte(`<meta name="bayplan-base" content="` + esc + `">`)
	if at := headEnd(out); at >= 0 {
		withMeta := make([]byte, 0, len(out)+len(meta))
		withMeta = append(withMeta, out[:at]...)
		withMeta = append(withMeta, meta...)
		withMeta = append(withMeta, out[at:]...)
		return withMeta
	}
	return out
}

// headEnd returns the offset just past the opening <head> tag (which may carry
// attributes), or -1 when there is none. <header> is not matched.
func headEnd(doc []byte) int {
	const open = "<head"
	from := 0
	for {
		i := bytes.Index(doc[from:], []byte(open))
		if i < 0 {
			return -1
		}
		i += from
		next := i + len(open)
		if next < len(doc) && (doc[next] == '>' || doc[next] == ' ' || doc[next] == '\t' || doc[next] == '\n') {
			end := bytes.IndexByte(doc[next:], '>')
			if end < 0 {
				return -1
			}
			return next + end + 1
		}
		from = next
	}
}

func isFile(site fs.FS, name string) bool {
	st, err := fs.Stat(site, name)
	return err == nil && !st.IsDir()
}

func serve(w http.ResponseWriter, r *http.Request, site fs.FS, name string) {
	data, err := fs.ReadFile(site, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// ServeContent sets Content-Type from the extension and handles HEAD and
	// ranges. A zero modtime disables conditional-request handling, which is
	// right for an embedded FS with no meaningful mtimes.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
