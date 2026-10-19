// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert

import (
	"reflect"
	"sort"
	"strings"

	"github.com/ianglennon/bayplan/internal/stacks/fieldclass"
)

// FieldClass says what the converter does with one compose service field on a swarm. The table
// lives in package fieldclass, which lint reads too; these are the converter's names for it.
type FieldClass = fieldclass.Class

// The classes (see package fieldclass).
const (
	Converted      = fieldclass.Converted
	IgnoredOnSwarm = fieldclass.IgnoredOnSwarm
	Blocking       = fieldclass.Blocking
)

// FieldClasses classifies every field of compose-go's types.ServiceConfig, keyed by the compose
// YAML key. TestEveryServiceFieldIsClassified fails when compose-go adds a field that is not
// in it (RESEARCH Pitfall 3), so a new host-access key cannot be dropped without a decision.
var FieldClasses = fieldclass.Table

// serviceFieldKeys returns the compose YAML key of every field of struct type t that is set in v
// (not at its zero value; compose-go fills some blocks with empty maps and slices, which count
// as not set). Embedded structs (ContainerSpec, WorkloadSpec) are walked into. A field with no
// YAML key (yaml:"-") and the extensions catch-all are not compose keys and are left out.
func serviceFieldKeys(v reflect.Value) []string {
	var keys []string
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if f.Anonymous && fv.Kind() == reflect.Struct {
			keys = append(keys, serviceFieldKeys(fv)...)
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" || strings.HasPrefix(tag, "#") || isEmpty(fv) {
			continue
		}
		if tag == "" {
			tag = strings.ToLower(f.Name)
		}
		keys = append(keys, tag)
	}
	sort.Strings(keys)
	return keys
}

// isEmpty reports whether v holds nothing a user set: zero scalars, nil pointers and interfaces,
// empty maps and slices, and structs whose fields are all empty.
func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Map, reflect.Slice:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() && !isEmpty(v.Field(i)) {
				return false
			}
		}
		return true
	default:
		return v.IsZero()
	}
}
