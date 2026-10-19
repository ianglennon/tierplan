// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package lint_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/lint"
)

// A fixture for each path of the copied SDK list: a compose file that sets the attribute, the line
// it is on and the key the warning names.
type fixture struct {
	compose string
	line    int
	key     string
}

const head = "services:\n  web:\n    image: registry.example/web:1\n"

var fixtures = map[string]fixture{
	"services.*.deploy.mode":             {head + "    deploy:\n      mode: global\n", 5, "deploy.mode"},
	"services.*.deploy.labels":           {head + "    deploy:\n      labels:\n        a: b\n", 5, "deploy.labels"},
	"services.*.deploy.update_config":    {head + "    deploy:\n      update_config:\n        parallelism: 2\n", 5, "deploy.update_config"},
	"services.*.deploy.rollback_config":  {head + "    deploy:\n      rollback_config:\n        parallelism: 2\n", 5, "deploy.rollback_config"},
	"services.*.deploy.endpoint_mode":    {head + "    deploy:\n      endpoint_mode: dnsrr\n", 5, "deploy.endpoint_mode"},
	"services.*.deploy.placement":        {head + "    deploy:\n      placement:\n        constraints:\n          - node.role == manager\n", 5, "deploy.placement"},
	"services.*.credential_spec":         {head + "    credential_spec:\n      registry: HKLM\n", 4, "credential_spec"},
	"configs.*.labels":                   {head + "configs:\n  c:\n    content: hello\n    labels:\n      a: b\n", 7, "configs[].labels"},
	"secrets.*.driver_opts":              {head + "secrets:\n  s:\n    environment: SECRET_VALUE\n    driver_opts:\n      a: b\n", 7, "secrets[].driver_opts"},
	"secrets.*.labels":                   {head + "secrets:\n  s:\n    environment: SECRET_VALUE\n    labels:\n      a: b\n", 7, "secrets[].labels"},
	"services.*.ports.[].mode=host":      {head + "    ports:\n      - target: 80\n        published: 8080\n        mode: host\n", 7, "ports[].mode: host"},
	"services.*.volumes.[].type=cluster": {head + "    volumes:\n      - type: cluster\n        source: v\n        target: /x\nvolumes:\n  v: {}\n", 5, "volumes[].type: cluster"},
	"services.*.configs.[].uid":          {head + "    configs:\n      - source: c\n        uid: \"1000\"\nconfigs:\n  c:\n    content: hello\n", 6, "configs[].uid"},
	"services.*.configs.[].gid":          {head + "    configs:\n      - source: c\n        gid: \"1000\"\nconfigs:\n  c:\n    content: hello\n", 6, "configs[].gid"},
	"services.*.configs.[].mode":         {head + "    configs:\n      - source: c\n        mode: 0440\nconfigs:\n  c:\n    content: hello\n", 6, "configs[].mode"},
	"services.*.secrets.[].uid":          {head + "    secrets:\n      - source: s\n        uid: \"1000\"\nsecrets:\n  s:\n    environment: SECRET_VALUE\n", 6, "secrets[].uid"},
	"services.*.secrets.[].gid":          {head + "    secrets:\n      - source: s\n        gid: \"1000\"\nsecrets:\n  s:\n    environment: SECRET_VALUE\n", 6, "secrets[].gid"},
	"services.*.secrets.[].mode":         {head + "    secrets:\n      - source: s\n        mode: 0440\nsecrets:\n  s:\n    environment: SECRET_VALUE\n", 6, "secrets[].mode"},
}

// ownerPaths are the paths of the table that are an error and not a warning: the agent's archive route
// refuses the owner of a secret or config (plan 07-20 carry-over from 07-04).
var ownerPaths = map[string]bool{
	"services.*.configs.[].uid": true, "services.*.configs.[].gid": true,
	"services.*.secrets.[].uid": true, "services.*.secrets.[].gid": true,
}

func TestEveryStandaloneUnsupportedPathIsReported(t *testing.T) {
	t.Parallel()
	if len(lint.StandaloneUnsupported) < 18 {
		t.Fatalf("the copied table has %d paths, want the SDK's 18", len(lint.StandaloneUnsupported))
	}
	// One loop, not a subtest per path: every failure names its path, and the test reads as one
	// check of the whole copied table.
	for _, path := range lint.StandaloneUnsupported {
		fx, ok := fixtures[path]
		if !ok {
			t.Errorf("%s has no fixture: add one to the test", path)
			continue
		}
		res := checkOf(t, stacks.TargetStandalone, fx.compose)
		if res.Loaded == nil {
			t.Errorf("%s: the fixture does not load: %+v", path, res.Problems)
			continue
		}
		if ownerPaths[path] {
			owner := byCode(res, stacks.CodeFileObjectOwner)
			if len(owner) != 1 || len(res.Problems) != 1 || owner[0].Severity != stacks.SeverityError || owner[0].Line != fx.line {
				t.Errorf("%s: problems = %+v, want exactly one blocking file_object_owner at line %d", path, res.Problems, fx.line)
			} else if want := "`" + fx.key + "` can't be set on a standalone host. "; !strings.HasPrefix(owner[0].Message, want) {
				t.Errorf("%s: message = %q, want it to start %q", path, owner[0].Message, want)
			}
			if swarm := byCode(checkOf(t, stacks.TargetSwarm, fx.compose), stacks.CodeFileObjectOwner); len(swarm) != 0 {
				t.Errorf("%s: swarm problems = %+v, want none: a swarm applies it", path, swarm)
			}
			continue
		}
		got := byCode(res, stacks.CodeIgnoredOnTarget)
		if len(got) != 1 || len(res.Problems) != 1 {
			t.Errorf("%s: problems = %+v, want exactly one ignored_on_target", path, res.Problems)
			continue
		}
		p := got[0]
		if p.Severity != stacks.SeverityWarning || p.Line != fx.line {
			t.Errorf("%s: problem = %+v, want a warning at line %d", path, p, fx.line)
		}
		if want := "`" + fx.key + "` is ignored on a standalone host. "; !strings.HasPrefix(p.Message, want) || len(p.Message) <= len(want) {
			t.Errorf("%s: message = %q, want %q followed by what to do", path, p.Message, want)
		}
		// The same file on a swarm is not told that.
		if swarm := byCode(checkOf(t, stacks.TargetSwarm, fx.compose), stacks.CodeIgnoredOnTarget); len(swarm) != 0 {
			t.Errorf("%s: swarm problems = %+v, want none: a swarm applies it", path, swarm)
		}
	}
	for path := range fixtures {
		if !slices.Contains(lint.StandaloneUnsupported, path) {
			t.Errorf("fixture %s is not in the copied table", path)
		}
	}
}

func TestEveryStandalonePathHasWording(t *testing.T) {
	t.Parallel()
	// A path without a sentence would print an empty key; the fixtures above check the key of each.
	for _, path := range lint.StandaloneUnsupported {
		if fixtures[path].key == "" {
			t.Errorf("%s has no expected key in its fixture", path)
		}
	}
}

func TestAReadOnlyStandaloneServiceThatUsesAStackFileObjectIsAnError(t *testing.T) {
	t.Parallel()
	const compose = head + "    read_only: true\n    secrets:\n      - s\n    configs:\n      - c\nsecrets:\n  s:\n    environment: SECRET_VALUE\nconfigs:\n  c:\n    content: hello\n"
	res := checkOf(t, stacks.TargetStandalone, compose)
	got := byCode(res, stacks.CodeReadOnlyFileObject)
	if len(got) != 2 {
		t.Fatalf("problems = %+v, want an error for the secret and one for the config", res.Problems)
	}
	for _, p := range got {
		if p.Severity != stacks.SeverityError || p.Line != 4 || !strings.HasPrefix(p.Message, "web is read_only") {
			t.Errorf("problem = %+v, want an error naming web at the read_only line 4", p)
		}
	}
	if !strings.Contains(got[0].Message+got[1].Message, "secret “s”") || !strings.Contains(got[0].Message+got[1].Message, "config “c”") {
		t.Errorf("messages %q %q must name the secret and the config", got[0].Message, got[1].Message)
	}
	if res.Problems.Errors() != 2 {
		t.Errorf("errors = %d, want 2: an error blocks", res.Problems.Errors())
	}
}

func TestReadOnlyIsFineWhereNothingIsWrittenIntoTheContainer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		target  stacks.Target
		compose string
	}{
		"on a swarm the secret is mounted by the engine": {stacks.TargetSwarm, head + "    read_only: true\n    secrets:\n      - s\nsecrets:\n  s:\n    environment: SECRET_VALUE\n"},
		"with no secret or config":                       {stacks.TargetStandalone, head + "    read_only: true\n"},
		"with an external secret":                        {stacks.TargetStandalone, head + "    read_only: true\n    secrets:\n      - s\nsecrets:\n  s:\n    external: true\n"},
		"without read_only":                              {stacks.TargetStandalone, head + "    secrets:\n      - s\nsecrets:\n  s:\n    environment: SECRET_VALUE\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := byCode(checkOf(t, tc.target, tc.compose), stacks.CodeReadOnlyFileObject); len(got) != 0 {
				t.Errorf("problems = %+v, want none", got)
			}
		})
	}
}

func TestAStandaloneSecretOrConfigWhoseTargetThePathCannotHoldIsAnError(t *testing.T) {
	t.Parallel()
	long := "/" + strings.Repeat("d", 150)
	const mid = "    secrets:\n      - source: s\n        target: "
	res := checkOf(t, stacks.TargetStandalone, head+mid+long+"\nsecrets:\n  s:\n    environment: SECRET_VALUE\n")
	got := byCode(res, stacks.CodeFileObjectName)
	if len(got) != 1 || got[0].Severity != stacks.SeverityError || got[0].Line != 5 || !strings.HasPrefix(got[0].Message, "The path /ddd") {
		t.Fatalf("problems = %+v, want one blocking file_object_name at the secret reference (line 5)", res.Problems)
	}
	// A path of the length a header holds is fine, on both targets; a long one is only a standalone matter.
	ok := checkOf(t, stacks.TargetStandalone, head+mid+"/run/secrets/"+strings.Repeat("e", 60)+"\nsecrets:\n  s:\n    environment: SECRET_VALUE\n")
	if len(byCode(ok, stacks.CodeFileObjectName)) != 0 {
		t.Errorf("problems = %+v, want a 73 character path accepted", ok.Problems)
	}
	if swarm := byCode(checkOf(t, stacks.TargetSwarm, head+mid+long+"\nsecrets:\n  s:\n    environment: SECRET_VALUE\n"), stacks.CodeFileObjectName); len(swarm) != 0 {
		t.Errorf("swarm problems = %+v, want none: only the archive route of a standalone host holds the limit", swarm)
	}
}

func TestAStandaloneExternalSecretOrConfigIsAnErrorAtItsReference(t *testing.T) {
	t.Parallel()
	for name, compose := range map[string]string{
		"secret": head + "    secrets:\n      - s\nsecrets:\n  s:\n    external: true\n",
		"config": head + "    configs:\n      - c\nconfigs:\n  c:\n    external: true\n",
	} {
		res := checkOf(t, stacks.TargetStandalone, compose)
		got := byCode(res, stacks.CodeExternalFileObject)
		if len(got) != 1 || got[0].Severity != stacks.SeverityError || !strings.Contains(got[0].Message, "external") {
			t.Errorf("%s: problems = %+v, want one blocking external_file_object", name, res.Problems)
		}
		if swarm := byCode(checkOf(t, stacks.TargetSwarm, compose), stacks.CodeExternalFileObject); len(swarm) != 0 {
			t.Errorf("%s: swarm problems = %+v, want none: a swarm uses an external object", name, swarm)
		}
	}
}

func TestAStandaloneServiceThatUsesTheAPISocketIsAnError(t *testing.T) {
	t.Parallel()
	const compose = head + "    use_api_socket: true\n"
	res := checkOf(t, stacks.TargetStandalone, compose)
	got := byCode(res, stacks.CodeUseAPISocket)
	if len(got) != 1 || got[0].Severity != stacks.SeverityError || got[0].Line != 4 || !strings.Contains(got[0].Message, "Docker socket") {
		t.Fatalf("problems = %+v, want one blocking use_api_socket_unsupported at line 4", res.Problems)
	}
	if swarm := byCode(checkOf(t, stacks.TargetSwarm, compose), stacks.CodeUseAPISocket); len(swarm) != 0 {
		t.Errorf("swarm problems = %+v, want none: a swarm ignores the key", swarm)
	}
}
