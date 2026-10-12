// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import "strings"

// CompareTags orders two tag names for the tag list: it returns a negative number when a is
// listed before b, positive when after, and zero only for the same name.
func CompareTags(a, b string) int { return strings.Compare(a, b) }
