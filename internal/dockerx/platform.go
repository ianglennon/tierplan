// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx

import "strings"

// OCIArch is the OCI name of a CPU architecture, given the name an engine or uname
// reported: x86_64 is amd64, aarch64 is arm64, armv7l is arm/v7. A name it does not know
// is passed through in lower case, so a new architecture still shows as itself.
func OCIArch(engineArch string) string {
	a := strings.ToLower(strings.TrimSpace(engineArch))
	switch a {
	case "x86_64", "amd64", "x64":
		return "amd64"
	case "aarch64", "aarch64_be", "arm64", "armv8", "armv8l":
		return "arm64"
	case "armv7l", "armv7", "arm":
		return "arm/v7"
	case "armv6l", "armv6":
		return "arm/v6"
	case "i386", "i686", "i486", "i586", "x86", "386":
		return "386"
	}
	return a
}
