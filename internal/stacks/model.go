// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package stacks holds the pure core of Bayplan's compose stacks: the model of a stack's working
// copy (its files and variables), the ownership labels, and the sandboxed compose-go load. It
// imports no HTTP type, no store and no Docker client: a stack's content comes in as values and
// leaves as values, so every rule about it is testable without an engine.
package stacks

import (
	"regexp"
)

// Target is where a stack deploys: a swarm (Bayplan's own stack deploy) or a standalone host (the
// Compose engine). It is fixed when the stack is created, from the environment's kind.
type Target string

// The targets.
const (
	TargetSwarm      Target = "swarm"
	TargetStandalone Target = "standalone"
)

// SourceKind says where a stack's definition lives. Only inline exists in Phase 7; adopted
// (Phase 8) and git (Phase 9) arrive on the same field.
type SourceKind string

// The source kinds.
const (
	SourceInline  SourceKind = "inline"
	SourceAdopted SourceKind = "adopted"
	SourceGit     SourceKind = "git"
)

// Source is a stack's source: its kind and, for an adopted or git stack, a reference to it.
type Source struct {
	Kind SourceKind
	Ref  string
}

// ComposeFile is the path of the compose file in a stack's working copy.
const ComposeFile = "compose.yaml"

// MaxFileBytes is the largest stack file Bayplan loads or stores.
const MaxFileBytes = 1 << 20

// File is one file of a stack's working copy. A secret file is stored sealed and never shown
// without secrets.reveal.
type File struct {
	Path    string
	Content []byte
	Secret  bool
}

// Var is one variable of the stack's table. A secret variable is interpolated like any other,
// stored sealed and shown as <hidden>.
type Var struct {
	Name   string
	Value  string
	Secret bool
}

// WorkingCopy is what the editor holds and a deploy reads: the stack's files and its variables.
type WorkingCopy struct {
	Files []File
	Vars  []Var
}

// Compose returns the content of compose.yaml, or an empty (never nil) slice when the working
// copy has none. The slice is never nil because compose-go reads a file from disk when a config
// file has nil content.
func (w WorkingCopy) Compose() []byte {
	for _, f := range w.Files {
		if f.Path == ComposeFile {
			if f.Content == nil {
				return []byte{}
			}
			return f.Content
		}
	}
	return []byte{}
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// NameValid reports whether name can be a stack name: lowercase letters, digits, dash and
// underscore, starting with a letter or digit, at most 63 characters. The same rule keeps the
// name safe as a docker label value, a swarm namespace and a compose project name.
func NameValid(name string) bool { return nameRe.MatchString(name) }

// StarterCompose is the compose file a new stack starts from when the user chooses the starter
// example. It loads clean for both targets.
const StarterCompose = `# A starter stack: one web server.
#
# Edit this file, then save the draft and review it before you deploy. Nothing changes on the
# host until you deploy.
#
# ${WEB_PORT:-8080} is a variable. Set WEB_PORT in the Variables table to change the port; with
# no value the default after ":-" is used.
services:
  web:
    image: nginx:1.27-alpine
    ports:
      - "${WEB_PORT:-8080}:80"
`
