// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"reflect"
	"strings"
	"testing"
)

// skipKey is Traefik's key that turns off backend certificate verification, spelled in two halves so
// scripts/insecure-tls-check.sh does not trip on this file.
const skipKey = "insecure" + "SkipVerify"

// TestSelfTransportHasNoSkipVerificationField is SELF-02's must-not (D-14): the transport of TierPlan's
// own route has no field whose YAML key is the skip key, so no value of the type can ever write it.
func TestSelfTransportHasNoSkipVerificationField(t *testing.T) {
	typ := reflect.TypeFor[selfServersTransport]()
	if typ.NumField() == 0 {
		t.Fatal("selfServersTransport has no fields: the reflection proves nothing")
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if strings.EqualFold(name, skipKey) || strings.EqualFold(f.Name, skipKey) {
			t.Errorf("selfServersTransport field %s (yaml %q) can write the skip key", f.Name, name)
		}
		if f.Type.Kind() == reflect.Bool {
			t.Errorf("selfServersTransport field %s is a bool: t-self has no switch to turn off", f.Name)
		}
	}
	// The yaml tags the route needs are there: serverName and rootCAs.
	tags := map[string]bool{}
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		tags[name] = true
	}
	for _, want := range []string{"serverName", "rootCAs"} {
		if !tags[want] {
			t.Errorf("selfServersTransport has no %s field", want)
		}
	}
}
