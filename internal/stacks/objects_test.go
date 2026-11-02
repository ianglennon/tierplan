// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/stacks"
)

func loadProject(t *testing.T, compose string) *types.Project {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: "web", Target: stacks.TargetSwarm,
		Copy: copyOf(compose),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return loaded.Project
}

const objectsCompose = `services:
  app:
    image: nginx:1.27-alpine
    secrets: [tok, envsec]
    configs: [conf, inline, ext_conf]
secrets:
  tok:
    file: tok.txt
  envsec:
    environment: API_KEY
  unused:
    file: nothing.txt
configs:
  conf:
    file: ./conf.txt
    labels:
      team: web
  inline:
    content: "listen 80;"
  ext_conf:
    external: true
    name: shared_conf
`

func TestResolveObjectsReadsFilesContentAndVariables(t *testing.T) {
	t.Parallel()
	files := stacks.NewFileSet([]stacks.File{
		{Path: "tok.txt", Content: []byte("file-secret")},
		{Path: "conf.txt", Content: []byte("file-config")},
	})
	got, err := stacks.ResolveObjects(loadProject(t, objectsCompose), files, map[string]string{"API_KEY": "from-table"})
	if err != nil {
		t.Fatalf("ResolveObjects: %v", err)
	}
	type row struct {
		kind    stacks.ObjectKind
		name    string
		content string
		ext     string
	}
	var rows []row
	for _, o := range got {
		rows = append(rows, row{o.Kind, o.Name, string(o.Content), o.ExternalName})
	}
	want := []row{
		{stacks.KindSecret, "envsec", "from-table", ""},
		{stacks.KindSecret, "tok", "file-secret", ""},
		{stacks.KindConfig, "conf", "file-config", ""},
		{stacks.KindConfig, "ext_conf", "", "shared_conf"},
		{stacks.KindConfig, "inline", "listen 80;", ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("objects = %+v, want %+v (an unused secret is left out)", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("object %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	for _, o := range got {
		if o.Name == "ext_conf" && !o.External {
			t.Error("ext_conf is not marked External")
		}
		if o.Name == "conf" && o.Labels["team"] != "web" {
			t.Errorf("conf labels = %v, want the file's team=web", o.Labels)
		}
	}
}

func TestResolveObjectsNamesAnExternalObjectByItsKeyWhenTheFileGivesNoName(t *testing.T) {
	t.Parallel()
	const compose = `services:
  app:
    image: nginx:1.27-alpine
    secrets: [shared]
secrets:
  shared:
    external: true
`
	got, err := stacks.ResolveObjects(loadProject(t, compose), nil, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("ResolveObjects = %+v, %v", got, err)
	}
	if !got[0].External || got[0].ExternalName != "shared" || got[0].Content != nil {
		t.Errorf("external secret = %+v, want External named shared with no content", got[0])
	}
}

func TestResolveObjectsErrorsNameTheObject(t *testing.T) {
	t.Parallel()
	const fileSource = `services:
  app:
    image: nginx:1.27-alpine
    secrets: [tok]
secrets:
  tok:
    file: tok.txt
`
	const envSource = `services:
  app:
    image: nginx:1.27-alpine
    secrets: [tok]
secrets:
  tok:
    environment: MISSING
`
	const configDriver = `services:
  app:
    image: nginx:1.27-alpine
    configs: [c]
configs:
  c:
    content: "x"
    template_driver: golang
`
	cases := []struct {
		name    string
		compose string
		files   []stacks.File
		vars    map[string]string
		kind    stacks.ObjectKind
		object  string
		reason  error
	}{
		{"a file the stack lacks", fileSource, nil, nil, stacks.KindSecret, "tok", stacks.ErrObjectFileMissing},
		{"an empty file", fileSource, []stacks.File{{Path: "tok.txt"}}, nil, stacks.KindSecret, "tok", stacks.ErrObjectEmpty},
		{"a variable the table lacks", envSource, nil, nil, stacks.KindSecret, "tok", stacks.ErrObjectVarMissing},
		{"a secret over the limit", fileSource, []stacks.File{{Path: "tok.txt", Content: bytes.Repeat([]byte("x"), stacks.MaxSecretBytes+1)}}, nil, stacks.KindSecret, "tok", stacks.ErrObjectTooLarge},
		{"a template driver", configDriver, nil, nil, stacks.KindConfig, "c", stacks.ErrObjectUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := stacks.ResolveObjects(loadProject(t, tc.compose), stacks.NewFileSet(tc.files), tc.vars)
			var oe *stacks.ObjectError
			if !errors.As(err, &oe) {
				t.Fatalf("err = %v, want an *ObjectError", err)
			}
			if oe.Kind != tc.kind || oe.Name != tc.object || !errors.Is(err, tc.reason) {
				t.Errorf("err = %v (%s %q), want %s %q: %v", err, oe.Kind, oe.Name, tc.kind, tc.object, tc.reason)
			}
			if !strings.Contains(err.Error(), tc.object) {
				t.Errorf("err text %q does not name the object", err)
			}
		})
	}
}

func TestResolveObjectsAcceptsTheLargestSizeTheSwarmTakes(t *testing.T) {
	t.Parallel()
	const tokYAML = `services:
  app:
    image: nginx:1.27-alpine
    secrets: [tok]
secrets:
  tok:
    file: tok.txt
`
	const config = `services:
  app:
    image: nginx:1.27-alpine
    configs: [c]
configs:
  c:
    file: c.txt
`
	files := stacks.NewFileSet([]stacks.File{
		{Path: "tok.txt", Content: bytes.Repeat([]byte("x"), stacks.MaxSecretBytes)},
		{Path: "c.txt", Content: bytes.Repeat([]byte("x"), stacks.MaxConfigBytes)},
	})
	for _, compose := range []string{tokYAML, config} {
		if _, err := stacks.ResolveObjects(loadProject(t, compose), files, nil); err != nil {
			t.Errorf("an object at the limit: %v", err)
		}
	}
	// A config may be larger than a secret may.
	bigFor := stacks.NewFileSet([]stacks.File{{Path: "tok.txt", Content: bytes.Repeat([]byte("x"), stacks.MaxSecretBytes+1)}})
	if _, err := stacks.ResolveObjects(loadProject(t, tokYAML), bigFor, nil); !errors.Is(err, stacks.ErrObjectTooLarge) {
		t.Errorf("a secret one byte over: err = %v, want ErrObjectTooLarge", err)
	}
}
