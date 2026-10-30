// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"slices"
	"testing"

	"github.com/ianglennon/coaming/internal/registry"
)

// The missing-platform flag (plan 05-20 task 2, IMG-03, D-23): the architectures an
// environment's nodes run that a tag has no platform for.

func linux(arch, variant string) registry.Platform {
	return registry.Platform{OS: "linux", Arch: arch, Variant: variant, Digest: "sha256:" + arch + variant}
}

func TestMissingPlatformsNamesTheNodeArchitecturesNoPlatformCovers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		nodeArchs []string
		platforms []registry.Platform
		want      []string
	}{
		{"an arm64 node and an amd64-only tag", []string{"arm64", "amd64"}, []registry.Platform{linux("amd64", "")}, []string{"arm64"}},
		{"every architecture covered", []string{"arm64", "amd64"}, []registry.Platform{linux("amd64", ""), linux("arm64", "v8")}, nil},
		{"arm/v7 nodes and an arm/v7 platform", []string{"arm/v7"}, []registry.Platform{linux("arm", "v7")}, nil},
		{"an arm/v6 image runs on an arm/v7 node", []string{"arm/v7"}, []registry.Platform{linux("arm", "v6")}, nil},
		{"an arm/v7 image does not run on an arm/v6 node", []string{"arm/v6"}, []registry.Platform{linux("arm", "v7")}, []string{"arm/v6"}},
		{"an arm platform with no variant is read as v7", []string{"arm/v7"}, []registry.Platform{linux("arm", "")}, nil},
		{"arm64 is not arm", []string{"arm/v7"}, []registry.Platform{linux("arm64", "v8")}, []string{"arm/v7"}},
		{"an unknown architecture is ignored", []string{"sparc", "", "amd64"}, []registry.Platform{linux("amd64", "")}, nil},
		{"a tag whose platforms are not known says nothing", []string{"arm64"}, nil, nil},
		{"each missing architecture once, sorted", []string{"arm64", "386", "arm64"}, []registry.Platform{linux("amd64", "")}, []string{"386", "arm64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingPlatforms(tc.nodeArchs, tc.platforms); !slices.Equal(got, tc.want) {
				t.Errorf("missingPlatforms(%v, %v) = %v, want %v", tc.nodeArchs, tc.platforms, got, tc.want)
			}
		})
	}
}
