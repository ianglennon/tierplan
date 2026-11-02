// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// Only the files a stack refers to are part of the stack (D-27): the compose files, the root .env and
// the env_file, secrets and configs files they name. The rest of a repository folder is not stored.

const (
	refBase = `services:
  web:
    image: nginx:1.27-alpine
    env_file: ./web.env
secrets:
  db:
    file: ./db_pw.txt
configs:
  nginx:
    file: ./conf/nginx.conf
`
	refProd = `services:
  web:
    deploy:
      replicas: 2
`
)

// refCopy is a repository folder of two compose files and some files nothing refers to.
func refCopy(base, prod string) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files: []stacks.File{
			repoFile("compose.yaml", base), repoFile("compose.prod.yaml", prod),
			repoFile("web.env", "A=1\n"), repoFile("db_pw.txt", "pw-pw-pw\n"), repoFile("conf/nginx.conf", "events {}\n"),
			repoFile("README.md", "docs\n"), repoFile("scripts/backup.sh", "#!/bin/sh\n"),
		},
		ComposeFiles: []string{"compose.yaml", "compose.prod.yaml"},
	}
}

func TestReferencedPathsAreTheStackFilesOnly(t *testing.T) {
	t.Parallel()
	got := stacks.ReferencedPaths(nil, refCopy(refBase, refProd))
	want := []string{".env", "compose.prod.yaml", "compose.yaml", "conf/nginx.conf", "db_pw.txt", "web.env"}
	if !reflect.DeepEqual(got.Paths, want) {
		t.Errorf("Paths = %q, want %q (README.md and scripts/backup.sh are not stack files; .env is listed although absent)", got.Paths, want)
	}
	if len(got.Escapes) != 0 {
		t.Errorf("Escapes = %q, want none", got.Escapes)
	}
}

func TestReferencedPathsAfterALoadAreTheSame(t *testing.T) {
	t.Parallel()
	c := refCopy(refBase, refProd)
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "r", Target: stacks.TargetSwarm, Copy: c})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if with, without := stacks.ReferencedPaths(loaded, c), stacks.ReferencedPaths(nil, c); !reflect.DeepEqual(with, without) {
		t.Errorf("with a loaded project %+v, without %+v", with, without)
	}
}

func TestReferencedPathsEscapesNeverBecomePaths(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    env_file:
      - ../outside.env
      - /etc/passwd
      - ./ok.env
secrets:
  key:
    file: ../../id_rsa
`
	c := stacks.WorkingCopy{Files: []stacks.File{repoFile("compose.yaml", compose)}, ComposeFiles: []string{"compose.yaml"}}
	got := stacks.ReferencedPaths(nil, c)
	if want := []string{"../../id_rsa", "../outside.env", "/etc/passwd"}; !reflect.DeepEqual(got.Escapes, want) {
		t.Errorf("Escapes = %q, want %q", got.Escapes, want)
	}
	if want := []string{".env", "compose.yaml", "ok.env"}; !reflect.DeepEqual(got.Paths, want) {
		t.Errorf("Paths = %q, want %q", got.Paths, want)
	}
}

func TestReferencedPathsListsAFileOnce(t *testing.T) {
	t.Parallel()
	const a = `services:
  web:
    image: nginx:1.27-alpine
    env_file: shared.env
  api:
    image: nginx:1.27-alpine
    env_file: [./shared.env]
`
	const b = `services:
  web:
    env_file: ./shared.env
`
	c := stacks.WorkingCopy{
		Files:        []stacks.File{repoFile("a.yaml", a), repoFile("b.yaml", b)},
		ComposeFiles: []string{"a.yaml", "b.yaml"},
	}
	got := stacks.ReferencedPaths(nil, c)
	if want := []string{".env", "a.yaml", "b.yaml", "shared.env"}; !reflect.DeepEqual(got.Paths, want) {
		t.Errorf("Paths = %q, want %q", got.Paths, want)
	}
}

func TestReferencedPathsOptional(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    env_file:
      - ./must.env
      - path: ./maybe.env
        required: false
`
	c := stacks.WorkingCopy{Files: []stacks.File{repoFile("compose.yaml", compose)}, ComposeFiles: []string{"compose.yaml"}}
	got := stacks.ReferencedPaths(nil, c)
	if want := []string{".env", "compose.yaml", "maybe.env", "must.env"}; !reflect.DeepEqual(got.Paths, want) {
		t.Errorf("Paths = %q, want %q", got.Paths, want)
	}
	if want := []string{".env", "maybe.env"}; !reflect.DeepEqual(got.Optional, want) {
		t.Errorf("Optional = %q, want %q", got.Optional, want)
	}
}
