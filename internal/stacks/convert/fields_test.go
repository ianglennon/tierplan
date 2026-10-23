// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert_test

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// serviceKeys returns the compose YAML key of every field of types.ServiceConfig, including the
// embedded ContainerSpec and WorkloadSpec. A field without a YAML key (yaml:"-") and the
// extensions catch-all are not compose keys and are left out.
func serviceKeys(t reflect.Type) []string {
	var keys []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			keys = append(keys, serviceKeys(f.Type)...)
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" || strings.HasPrefix(tag, "#") {
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

// TestEveryServiceFieldIsClassified is the guard of RESEARCH Pitfall 3: when compose-go grows a
// field, this fails until someone decides what a swarm does with it, so a new host-access key
// can never be dropped unlinted.
func TestEveryServiceFieldIsClassified(t *testing.T) {
	t.Parallel()
	var missing []string
	for _, key := range serviceKeys(reflect.TypeOf(composetypes.ServiceConfig{})) {
		if _, ok := convert.FieldClasses[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("ServiceConfig fields with no entry in convert.FieldClasses: %s", strings.Join(missing, ", "))
	}
}

func TestNoFieldClassIsLeftForAFieldThatIsGone(t *testing.T) {
	t.Parallel()
	keys := serviceKeys(reflect.TypeOf(composetypes.ServiceConfig{}))
	for key := range convert.FieldClasses {
		if !slices.Contains(keys, key) {
			t.Errorf("convert.FieldClasses has %q, which is not a field of types.ServiceConfig", key)
		}
	}
	if len(convert.FieldClasses) == 0 {
		t.Error("convert.FieldClasses is empty")
	}
}

func TestEveryFieldClassIsOneOfTheThree(t *testing.T) {
	t.Parallel()
	for key, class := range convert.FieldClasses {
		switch class {
		case convert.Converted, convert.IgnoredOnSwarm, convert.Blocking:
		default:
			t.Errorf("%s has the unknown class %d", key, class)
		}
	}
}

// A key that reaches the host, or that a swarm cannot honour, must never be classed Converted
// by accident: it is either carried deliberately (and so named here) or it is warned or refused.
func TestHostAccessKeysAreNeverSilentlyConverted(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"privileged", "devices", "pid", "ipc", "uts", "cgroup", "cgroup_parent", "userns_mode",
		"security_opt", "network_mode", "use_api_socket", "device_cgroup_rules", "gpus", "runtime",
	} {
		class, ok := convert.FieldClasses[key]
		if !ok {
			t.Errorf("%s is not classified", key)
			continue
		}
		if class == convert.Converted {
			t.Errorf("%s is classed Converted, but a swarm service spec carries nothing for it", key)
		}
	}
}

func TestTheClassesMatchWhatDockerStackDeployDoes(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]convert.FieldClass{
		"build":          convert.IgnoredOnSwarm,
		"image":          convert.Converted,
		"deploy":         convert.Converted,
		"extends":        convert.Blocking,
		"volumes_from":   convert.Blocking,
		"volume_driver":  convert.Blocking,
		"mem_limit":      convert.Blocking,
		"cpu_shares":     convert.Blocking,
		"env_file":       convert.Blocking,
		"container_name": convert.IgnoredOnSwarm,
		"expose":         convert.IgnoredOnSwarm,
		"depends_on":     convert.IgnoredOnSwarm,
	} {
		if got := convert.FieldClasses[key]; got != want {
			t.Errorf("FieldClasses[%q] = %d, want %d", key, got, want)
		}
	}
}
