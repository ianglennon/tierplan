// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx

import "net/http"

// StripBasePath is a stub until the GREEN step.
func StripBasePath(base func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}
