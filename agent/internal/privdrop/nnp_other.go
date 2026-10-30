// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

//go:build !linux

package privdrop

import "errors"

// noNewPrivs exists so the package builds on a contributor's non-Linux machine.
// The agent only ships for Linux, and Drop only gets here when run as root.
func noNewPrivs() error {
	return errors.New("no-new-privileges is only available on Linux")
}
