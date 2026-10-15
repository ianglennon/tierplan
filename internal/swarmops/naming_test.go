// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// Version naming and reference replacement for a rotate (plan 06-28, RES-08, D-26, UI-SPEC default 14).

func takenSet(names ...string) func(string) bool {
	return func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
}

func TestNextVersionName(t *testing.T) {
	long := strings.Repeat("a", 64)
	cases := []struct {
		name, old string
		taken     []string
		want      string
	}{
		{"the first rotation of a plain name", "db_password", nil, "db_password_v2"},
		{"the lowest unused number above the old version", "db_password_v2", []string{"db_password_v3"}, "db_password_v4"},
		{"two digits", "x_v9", nil, "x_v10"},
		{"a taken second version", "db_password", []string{"db_password_v2"}, "db_password_v3"},
		{"a gap below a taken number is used", "db_v2", []string{"db_v4", "db_v5"}, "db_v3"},
		{"only a trailing suffix is the version", "app_v1_v3", nil, "app_v1_v4"},
		{"version zero", "x_v0", nil, "x_v1"},
		{"leading zeros are the number", "x_v007", nil, "x_v8"},
		{"a name that merely ends in v and digits without the underscore", "relv2", nil, "relv2_v2"},
		{"a dot in the name", "app.conf", nil, "app.conf_v2"},
		{"a long name is trimmed at the base", long, nil, strings.Repeat("a", 61) + "_v2"},
		{"a long versioned name is trimmed at the base", strings.Repeat("a", 60) + "_v9", nil, strings.Repeat("a", 60) + "_v10"},
		{"a long name with a two-digit version", strings.Repeat("a", 61) + "_v9", nil, strings.Repeat("a", 60) + "_v10"},
		{"a number too large to be a version is part of the base", "x_v99999999999999999999", nil, "x_v99999999999999999999_v2"},
	}
	for _, tc := range cases {
		got := nextVersionName(tc.old, takenSet(tc.taken...))
		if got != tc.want {
			t.Errorf("%s: nextVersionName(%q) = %q, want %q", tc.name, tc.old, got, tc.want)
		}
		if len(got) > 64 {
			t.Errorf("%s: %q is %d characters, want at most 64", tc.name, got, len(got))
		}
	}
}

func TestNextVersionNameTrimsTheBaseSoTheResultFitsAndEndsInADigit(t *testing.T) {
	old := strings.Repeat("a", 62) + "-b" // 64 characters
	got := nextVersionName(old, takenSet())
	if len(got) > 64 || !strings.HasSuffix(got, "_v2") {
		t.Errorf("nextVersionName(%q) = %q, want a name of at most 64 characters ending in _v2", old, got)
	}
}

func secretRefSpec() swarm.ServiceSpec {
	return swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
		Secrets: []*swarm.SecretReference{
			{SecretID: "old", SecretName: "pw", File: &swarm.SecretReferenceFileTarget{Name: "pw", UID: "70", GID: "71", Mode: 0o400}},
			{SecretID: "other", SecretName: "tls", File: &swarm.SecretReferenceFileTarget{Name: "tls", UID: "0", GID: "0", Mode: 0o444}},
			{SecretID: "old", SecretName: "pw", File: &swarm.SecretReferenceFileTarget{Name: "pw-copy", UID: "0", GID: "0", Mode: 0o440}},
		},
		Configs: []*swarm.ConfigReference{
			{ConfigID: "old", ConfigName: "conf", File: &swarm.ConfigReferenceFileTarget{Name: "/etc/c", UID: "0", GID: "0", Mode: 0o444}},
			{ConfigID: "old", ConfigName: "conf", Runtime: &swarm.ConfigReferenceRuntimeTarget{}},
		},
	}}}
}

func TestReplaceReferenceMovesOnlyTheNamedSecretAndKeepsTheFileTarget(t *testing.T) {
	spec := secretRefSpec()
	if n := replaceReference(&spec, KindSecret, "old", "new-id", "pw_v2"); n != 2 {
		t.Fatalf("replaced %d references, want 2", n)
	}
	secrets := spec.TaskTemplate.ContainerSpec.Secrets
	for i, want := range []struct{ id, name, file, uid, gid string }{
		{"new-id", "pw_v2", "pw", "70", "71"},
		{"other", "tls", "tls", "0", "0"},
		{"new-id", "pw_v2", "pw-copy", "0", "0"},
	} {
		s := secrets[i]
		if s.SecretID != want.id || s.SecretName != want.name || s.File.Name != want.file || s.File.UID != want.uid || s.File.GID != want.gid {
			t.Errorf("secret %d = %+v file %+v, want %+v", i, s, s.File, want)
		}
	}
	if secrets[0].File.Mode != 0o400 || secrets[2].File.Mode != 0o440 {
		t.Errorf("modes = %o and %o, want 400 and 440", secrets[0].File.Mode, secrets[2].File.Mode)
	}
	if got := spec.TaskTemplate.ContainerSpec.Configs[0].ConfigID; got != "old" {
		t.Errorf("a secret rotate changed a config: %q", got)
	}
}

func TestReplaceReferenceMovesAConfigIncludingARuntimeTarget(t *testing.T) {
	spec := secretRefSpec()
	if n := replaceReference(&spec, KindConfig, "old", "new-id", "conf_v2"); n != 2 {
		t.Fatalf("replaced %d references, want 2", n)
	}
	configs := spec.TaskTemplate.ContainerSpec.Configs
	if configs[0].ConfigID != "new-id" || configs[0].ConfigName != "conf_v2" || configs[0].File.Name != "/etc/c" {
		t.Errorf("config 0 = %+v file %+v", configs[0], configs[0].File)
	}
	if configs[1].ConfigID != "new-id" || configs[1].Runtime == nil || configs[1].File != nil {
		t.Errorf("config 1 = %+v, want the runtime target kept", configs[1])
	}
	if got := spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID; got != "old" {
		t.Errorf("a config rotate changed a secret: %q", got)
	}
}

func TestReplaceReferenceOfASpecThatDoesNotNameTheObjectReplacesNothing(t *testing.T) {
	spec := secretRefSpec()
	if n := replaceReference(&spec, KindSecret, "absent", "n", "n"); n != 0 {
		t.Errorf("replaced %d, want 0", n)
	}
	empty := swarm.ServiceSpec{}
	if n := replaceReference(&empty, KindSecret, "old", "n", "n"); n != 0 {
		t.Errorf("replaced %d in a spec with no container spec, want 0", n)
	}
}
