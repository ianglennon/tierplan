// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package backup

import (
	"bytes"
	"testing"
)

// FuzzOpen feeds mutated backup files to Open. A file must never panic the
// reader, and must never open under a passphrase other than the one it was
// sealed with.
func FuzzOpen(f *testing.F) {
	file, _ := seal(f, testHeader(f), testPass, 3000)
	f.Add(file)
	for _, n := range []int{0, 5, len(Magic), len(Magic) + 6, 150, 300, len(file) / 2, len(file) - 1} {
		if n < len(file) {
			f.Add(file[:n])
		}
	}
	f.Add(append(bytes.Clone(file), 0))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Any outcome is allowed for the right passphrase, but it must not panic.
		_, _ = readAll(data, testPass)

		if got, err := readAll(data, "some other passphrase altogether"); err == nil {
			t.Fatalf("opened under the wrong passphrase, returned %d entries", len(got))
		}
	})
}
