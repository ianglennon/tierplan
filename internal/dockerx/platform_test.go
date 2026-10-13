// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// An engine says x86_64 where the registry says amd64 (plan 05-03, IMG-02): the node
// chooser shows the registry's names so an image's platforms can be compared with it.
func TestOCIArchNamesAnArchitectureTheWayARegistryDoes(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "amd64", "amd64": "amd64", "AMD64": "amd64", "X86_64": "amd64",
		"aarch64": "arm64", "arm64": "arm64", "aarch64_be": "arm64",
		"armv7l": "arm/v7", "armv7": "arm/v7", "arm": "arm/v7",
		"armv6l": "arm/v6", "armv6": "arm/v6",
		"i386": "386", "i686": "386", "386": "386",
		"ppc64le": "ppc64le", "s390x": "s390x", "riscv64": "riscv64",
		"": "", "  ": "", "Sparc64": "sparc64",
	} {
		if got := dockerx.OCIArch(in); got != want {
			t.Errorf("OCIArch(%q) = %q, want %q", in, got, want)
		}
	}
}
