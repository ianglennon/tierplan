// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/swarmops"
)

// The code of a scale result is an enum in the contract, so every code scaleError can answer must be listed in it: a
// strict client built from the spec rejects a value outside it.
func TestScaleResultCodeEnumHoldsEveryCodeScaleErrorReturns(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeOf(ServiceScaleResult{}).FieldByName("Code")
	if !ok {
		t.Fatal("ServiceScaleResult has no Code")
	}
	enum := strings.Split(field.Tag.Get("enum"), ",")
	for name, err := range map[string]error{
		"the ingress proxy":    swarmops.ErrIngressOwned,
		"a global service":     dockerx.ErrNotScalable,
		"no registry credit":   errRegistryUnresolved,
		"the service changed":  dockerx.ErrServiceChanged,
		"the service is gone":  dockerx.ErrServiceNotFound,
		"a read-only host":     envs.ErrHostReadOnly,
		"no manager connected": envs.ErrNoManager,
		"anything else":        errors.New("the manager refused it"),
	} {
		code, _ := scaleError(err)
		if !slices.Contains(enum, code) {
			t.Errorf("%s: scaleError returns %q, which the enum %v does not list", name, code, enum)
		}
	}
}
