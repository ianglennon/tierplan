// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import "strings"

// tagVersion is the version a tag name starts with.
type tagVersion struct {
	// parts are major, minor and patch as digit strings without leading zeros; a part the
	// name does not have is "0". They are strings so that a build stamp longer than any
	// integer still compares.
	parts [3]string
	// plain is true when the name is the version and nothing else.
	plain bool
}

// parseTagVersion reads a leading v?MAJOR[.MINOR[.PATCH]] off a tag name. ok is false for a
// name that does not start with one. Anything after the version, like -trixie-perl, is left
// out of the version and only makes the name not plain.
func parseTagVersion(name string) (v tagVersion, ok bool) {
	s := strings.TrimPrefix(name, "v")
	v.parts = [3]string{"0", "0", "0"}
	i := 0
	for p := range v.parts {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return tagVersion{}, false
		}
		v.parts[p] = strings.TrimLeft(s[i:j], "0")
		if v.parts[p] == "" {
			v.parts[p] = "0"
		}
		i = j
		if p == len(v.parts)-1 || i+1 >= len(s) || s[i] != '.' || s[i+1] < '0' || s[i+1] > '9' {
			break
		}
		i++ // the dot of a further part
	}
	v.plain = i == len(s)
	return v, true
}

// cmpNumber compares two numbers written as digit strings without leading zeros.
func cmpNumber(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// CompareTags orders two tag names for the tag list when no dates are known (D-21): a negative
// number when a is listed before b, a positive one when after, zero only for the same name.
// Names that start with a version (v1.10.0, 1.9.2, 2, 1.30.5-trixie-perl) come first, the
// newest version first; a missing minor or patch counts as zero and a leading v is ignored. Of
// two names with one version the plain one (1.30.5) comes before its variants (1.30.5-rc1,
// 1.30.5-alpine), the way a release follows its pre-releases. Ties, and every name without a
// version, then go by name, descending.
func CompareTags(a, b string) int {
	va, okA := parseTagVersion(a)
	vb, okB := parseTagVersion(b)
	switch {
	case okA && !okB:
		return -1
	case !okA && okB:
		return 1
	case okA && okB:
		for i := range va.parts {
			if c := cmpNumber(vb.parts[i], va.parts[i]); c != 0 {
				return c
			}
		}
		if va.plain != vb.plain {
			if va.plain {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(b, a)
}
