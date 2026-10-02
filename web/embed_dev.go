//go:build devspa

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package web

import (
	"io/fs"
	"os"
)

// FS serves web/dist from disk (relative to the working directory, which is
// the repository root under `make dev`), so a rebuilt SPA is picked up without
// recompiling Go (D-77). Only built with `-tags devspa`; release builds never
// set the tag and use the embedded copy in embed.go.
func FS() fs.FS {
	return os.DirFS("web/dist")
}
