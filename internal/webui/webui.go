// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package webui serves the single-page app out of an fs.FS (the embedded
// web/dist in production).
package webui

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Handler serves files from site, falls back to index.html for any other path
// (client-side routes), and never invents a file under /assets/.
func Handler(site fs.FS, base func() string) http.Handler {
	_ = base
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
		w.Header().Set("Cache-Control", "no-cache")
		serve(w, r, site, "index.html")
	})
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
