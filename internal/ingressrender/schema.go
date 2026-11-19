// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"slices"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Latest is the newest bundle schema this renderer produces.
const Latest = ingressbundle.Schema1

// Schemas lists the bundle schemas this renderer can produce, oldest first. The slice is a copy.
func Schemas() []int {
	return []int{ingressbundle.Schema1}
}

// RenderSchema renders the input as a bundle of the given schema. A schema this renderer does not
// produce is an *UnsupportedSchemaError and no bundle.
//
// The supervisor on each node reports the schemas it can apply when it connects (Phase 22). TierPlan
// renders the highest schema both sides support (PickSchema) and asks for exactly that one here. A node
// whose supervisor shares no schema with TierPlan keeps the last good bundle it has: it is frozen, not
// broken, and the environment says so rather than pushing a bundle the node cannot read.
func RenderSchema(schema int, in Input) (Result, error) {
	switch schema {
	case ingressbundle.Schema1:
		return renderSchema1(in)
	default:
		return Result{}, &UnsupportedSchemaError{Offered: []int{schema}, Supported: Schemas()}
	}
}

// PickSchema is the highest schema that is in both offered and Schemas. When there is none it returns
// an *UnsupportedSchemaError carrying what was offered.
func PickSchema(offered []int) (int, error) {
	supported := Schemas()
	best, found := 0, false
	for _, s := range offered {
		if slices.Contains(supported, s) && (!found || s > best) {
			best, found = s, true
		}
	}
	if !found {
		return 0, &UnsupportedSchemaError{Offered: slices.Clone(offered), Supported: supported}
	}
	return best, nil
}
