// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

func copyOf(compose string, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}},
		Vars:  vars,
	}
}

func TestLoadReadsOnlyTheTable(t *testing.T) {
	// Not parallel: t.Setenv changes the test process's environment.
	t.Setenv("BAYPLAN_LEAK_PROBE", "leaked")
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    environment:
      PROBE: ${BAYPLAN_LEAK_PROBE:-unset}
      FROM_TABLE: ${FROM_TABLE:-none}
`
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: "probe", Target: stacks.TargetSwarm,
		Copy: copyOf(compose, stacks.Var{Name: "FROM_TABLE", Value: "table"}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := loaded.Project.Services["web"].Environment
	if got := env["PROBE"]; got == nil || *got != "unset" {
		t.Errorf("PROBE = %v, want unset: the server's own environment must not reach interpolation", got)
	}
	if got := env["FROM_TABLE"]; got == nil || *got != "table" {
		t.Errorf("FROM_TABLE = %v, want table", got)
	}
}

func TestLoadNeverOpensAServerFile(t *testing.T) {
	t.Parallel()
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no host name to look for: %v", err)
	}
	const withInclude = `include:
  - /etc/hostname
services:
  web:
    image: nginx:1.27-alpine
`
	const withExtends = `services:
  web:
    image: nginx:1.27-alpine
  other:
    image: busybox:1.37
    extends:
      file: /etc/hostname
      service: x
`
	for name, compose := range map[string]string{"include": withInclude, "extends": withExtends} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(compose)})
			// The directive is not followed, so the load succeeds as if it were not there; a load
			// that followed it would fail on the file it opened.
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if y, mErr := loaded.Project.MarshalYAML(); mErr == nil && strings.Contains(string(y), host) {
				t.Errorf("the project holds the host name read from /etc/hostname")
			}
		})
	}
}

func TestLoadDoesNotResolveEnvFileOrReadItFromDisk(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    env_file:
      - /proc/self/environ
`
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(compose)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(loaded.Project.Services["web"].Environment); n != 0 {
		t.Errorf("the env_file was read: the service has %d environment entries", n)
	}
}

func TestLoadRefusesAFileOverTheLimit(t *testing.T) {
	t.Parallel()
	big := "services: {}\n# " + strings.Repeat("x", stacks.MaxFileBytes)
	_, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "big", Target: stacks.TargetSwarm, Copy: copyOf(big)})
	if !errors.Is(err, stacks.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestLoadOfAMissingComposeFileNeverReadsTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	// A working copy with no compose.yaml must fail as an empty file, not read ./compose.yaml.
	_, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "none", Target: stacks.TargetSwarm})
	if err == nil {
		t.Fatal("Load of a working copy without a compose file succeeded")
	}
}

func TestStarterComposeLoadsForBothTargets(t *testing.T) {
	t.Parallel()
	for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
		t.Run(string(target), func(t *testing.T) {
			t.Parallel()
			loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "starter", Target: target, Copy: copyOf(stacks.StarterCompose)})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			web, ok := loaded.Project.Services["web"]
			if !ok {
				t.Fatalf("no web service in %v", loaded.Project.ServiceNames())
			}
			if web.Image != "nginx:1.27-alpine" {
				t.Errorf("image = %q", web.Image)
			}
			if len(web.Ports) != 1 || web.Ports[0].Published != "8080" || web.Ports[0].Target != 80 {
				t.Errorf("ports = %+v, want 8080:80", web.Ports)
			}
		})
	}
}

func TestNameValid(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"web": true, "a": true, "0abc": true, "my-stack_2": true, strings.Repeat("a", 63): true,
		"": false, "Web": false, "-web": false, "_web": false, "my stack": false, "my.stack": false,
		strings.Repeat("a", 64): false, "ünï": false,
	} {
		if got := stacks.NameValid(name); got != want {
			t.Errorf("NameValid(%q) = %v, want %v", name, got, want)
		}
	}
}
