// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Nullable is an object that may be null in a response body. huma can mark a
// scalar nullable but refuses a field that points at a named object, so this says
// it in the schema directly: the field is either the object or null, which the
// generated TypeScript reads as `T | null`. A nil V is written as null.
type Nullable[T any] struct{ V *T }

// Some wraps v.
func Some[T any](v T) Nullable[T] { return Nullable[T]{V: &v} }

// MarshalJSON writes the object, or null when there is none.
func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if n.V == nil {
		return []byte("null"), nil
	}
	return json.Marshal(n.V)
}

// UnmarshalJSON reads null as no object.
func (n *Nullable[T]) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		n.V = nil
		return nil
	}
	n.V = new(T)
	return json.Unmarshal(b, n.V)
}

// Schema implements huma.SchemaProvider: a reference to T's schema, or null.
func (Nullable[T]) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{
		r.Schema(reflect.TypeFor[T](), true, ""),
		{Type: "null"},
	}}
}
