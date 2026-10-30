// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package stacks holds the pure core of Bayplan's compose stacks: the model of a stack's working
// copy (its files and variables), the ownership labels, and the sandboxed compose-go load. It
// imports no HTTP type, no store and no Docker client: a stack's content comes in as values and
// leaves as values, so every rule about it is testable without an engine.
//
// Nothing in this package opens a file on the server: a stack's files are rows in the store, so
// there is no directory to join a path to and no symlink to follow. The path rules in files.go
// (ValidPath, FileSet.Lookup) still hold every name to "a plain path inside the stack", so a
// later layer that does hand a stack file to something that reads one can trust the name
// (RESEARCH Q5, layers 2 and 4; T-07-11).
package stacks

import (
	"regexp"
	"slices"

	"github.com/compose-spec/compose-go/v2/loader"
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

// Adoption is what Bayplan recorded when it took over a stack that was made elsewhere (D-14, D-18). It is
// stored with the stack and read by the ownership rule: until the stack's first deploy it says which
// objects of the engine are the stack's (Pending), and for as long as the stack lives it names the networks
// and volumes the stack owns, so a later removal cleans up the right ones. The zero value is a stack that was
// not adopted.
type Adoption struct {
	// PriorID is the bayplan.stack.id the stack's objects carried when it was adopted (Bayplan made it once,
	// for a record this instance does not hold, D-11), empty for none. It widens ownership of the workloads of
	// this namespace or project while Pending, and is never the stack's own ID.
	PriorID string `json:"priorId,omitempty"`
	// Networks and Volumes are the engine names of the networks and volumes the stack owns.
	Networks []string `json:"networks,omitempty"`
	Volumes  []string `json:"volumes,omitempty"`
	// Secrets and Configs are the names of the existing secrets and configs the stack references as external.
	Secrets []string `json:"secrets,omitempty"`
	Configs []string `json:"configs,omitempty"`
	// Pending is true from the adoption until the first deploy of the stack has put Bayplan's labels on what
	// it runs.
	Pending bool `json:"pending,omitempty"`
	// Gaps are the places the rebuilt definition differs from what runs, by kind, found when the stack was
	// adopted (D-55). The adoption review fills them in.
	Gaps map[string][]string `json:"gaps,omitempty"`
}

// ComposeFile is the path of the compose file in a stack's working copy.
const ComposeFile = "compose.yaml"

// MaxFileBytes is the largest stack file Bayplan loads or stores.
const MaxFileBytes = 1 << 20

// FileOrigin says where a stack file came from (D-09). An inline stack's files are all OriginInline; a
// stack made from a git repository has files of the repository and overlay files that fill the paths the
// repository lacks.
type FileOrigin string

// The origins of a file.
const (
	// OriginInline is a file typed or uploaded into Bayplan: the origin of every file of an inline or
	// adopted stack, and the zero value.
	OriginInline FileOrigin = ""
	// OriginRepo is a file of the repository the stack was made from.
	OriginRepo FileOrigin = "repo"
	// OriginOverlay is a file Bayplan holds for a path the repository does not have. It may never
	// shadow a repository file (D-09).
	OriginOverlay FileOrigin = "overlay"
)

// DotEnvFile is the file at the stack root whose values a stack from a repository takes as defaults
// (D-07).
const DotEnvFile = ".env"

// File is one file of a stack's working copy. A secret file is stored sealed and never shown
// without secrets.reveal.
type File struct {
	Path    string
	Content []byte
	Secret  bool
	// Origin is where the file came from. The zero value is an inline file.
	Origin FileOrigin
}

// Var is one variable of the stack's table. A secret variable is interpolated like any other,
// stored sealed and shown as <hidden>.
type Var struct {
	Name   string
	Value  string
	Secret bool
}

// WorkingCopy is what the editor holds and a deploy reads: the stack's files and its variables.
//
// A stack is made of one compose file or, for a stack that comes from a git repository, several that
// merge in order like `docker compose -f a -f b` (D-06). ComposeFiles names them; empty means the one
// file compose.yaml, which is what an inline stack always has.
type WorkingCopy struct {
	Files []File
	Vars  []Var
	// ComposeFiles are the paths of the compose files, relative to the stack, in merge order: a key of
	// a later file wins over the same key of an earlier one. Empty means [ComposeFile]. Every path is
	// one of Files.
	ComposeFiles []string
}

// ComposeContent is one compose file of a working copy: its path and its content, which is never nil.
type ComposeContent struct {
	Path    string
	Content []byte
}

// ComposePaths returns the paths of the compose files in merge order. It is never empty: a working
// copy with no ComposeFiles has the one file compose.yaml.
func (w WorkingCopy) ComposePaths() []string {
	if len(w.ComposeFiles) == 0 {
		return []string{ComposeFile}
	}
	return slices.Clone(w.ComposeFiles)
}

// ComposeContents returns the compose files in merge order, each with its content from Files. The
// content is never nil, because compose-go reads a file from disk when a config file has nil content;
// a listed file the working copy does not hold has an empty one.
func (w WorkingCopy) ComposeContents() []ComposeContent {
	paths := w.ComposePaths()
	out := make([]ComposeContent, len(paths))
	for i, p := range paths {
		out[i] = ComposeContent{Path: p, Content: []byte{}}
		for _, f := range w.Files {
			if f.Path == p {
				if f.Content != nil {
					out[i].Content = f.Content
				}
				break
			}
		}
	}
	return out
}

// multiFile reports whether the working copy names its compose files, as a stack from a git
// repository does. It selects the wording of the include and extends refusals (D-26).
func (w WorkingCopy) multiFile() bool { return len(w.ComposeFiles) > 0 }

// Compose returns the content of the first compose file (compose.yaml for an inline stack), or an
// empty (never nil) slice when the working copy has none. The slice is never nil because compose-go
// reads a file from disk when a config file has nil content. It is for callers that only ever see an
// inline stack; a reader of a stack that may come from git goes through ComposeContents.
func (w WorkingCopy) Compose() []byte {
	return w.ComposeContents()[0].Content
}

// maxSwarmName is the longest stack name on a swarm. Swarm names a stack's services and networks
// {stack}_{key} and refuses a service or network name over maxSwarmObjectName bytes, so 61 is the
// longest stack name that can still hold a one-character service key.
const maxSwarmName = 61

// maxStandaloneName is the longest stack name on a standalone host.
const maxStandaloneName = 255

// maxSwarmObjectName is the longest service or network name swarm takes (swarmkit
// validateAnnotations).
const maxSwarmObjectName = 63

var (
	swarmNameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,60}$`)
	swarmObjectNameRe = regexp.MustCompile(`^[a-zA-Z0-9](?:[-_]*[A-Za-z0-9]+)*$`)
)

// NameValid reports whether name can be the name of a stack on target. It is the one rule that
// decides both creating a stack in Bayplan and adopting one made elsewhere (D-48, D-53): there is no
// second, create-only rule.
//
// On a swarm the name is letters in either case, digits, "_" and "-", starting with a letter or a
// digit, 1 to 61 bytes. It has no ".", because swarm refuses a dot in a service or network name and
// those are named {stack}_{key}. The cap is 61 because swarm refuses a service or network name over
// 63 bytes: 61 is the longest stack name that can hold a one-character service, so a longer name,
// which Phase 7 accepted, could never deploy a service.
//
// On a standalone host the name is compose-go's own project-name rule (lowercase letters, digits,
// "_" and "-", starting with a letter or digit; a dot is refused there too), at most 255 bytes.
func NameValid(target Target, name string) bool {
	if target == TargetSwarm {
		return len(name) <= maxSwarmName && swarmNameRe.MatchString(name)
	}
	return name != "" && len(name) <= maxStandaloneName && loader.NormalizeProjectName(name) == name
}

// NameRuleText describes the rule NameValid applies to target, as a phrase that completes "Use only
// ..." and "a stack name on a swarm takes only ...". Every sentence about the rule is built from it,
// so create and adopt cannot word it differently (D-53).
func NameRuleText(target Target) string {
	if target == TargetSwarm {
		return "letters, numbers, dash and underscore, starting with a letter or number, up to 61 characters"
	}
	return "lowercase letters, numbers, dash and underscore, starting with a letter or number, up to 255 characters"
}

// SwarmObjectNameValid reports whether swarm takes name as the name of a service or a network: letters,
// digits, "-" and "_" (no "." and no run of the two at either end), at most 63 bytes (swarmkit
// validateAnnotations).
func SwarmObjectNameValid(name string) bool {
	return len(name) <= maxSwarmObjectName && swarmObjectNameRe.MatchString(name)
}

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
