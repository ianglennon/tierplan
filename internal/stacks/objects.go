// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/compose-spec/compose-go/v2/types"
)

// ObjectKind says whether a stack object is a swarm secret or a swarm config.
type ObjectKind string

// The kinds of stack object.
const (
	KindSecret ObjectKind = "secret"
	KindConfig ObjectKind = "config"
)

// The most bytes a stack secret or config may hold (D-40): the largest the swarm takes, 511,999
// for a secret (it refuses 512,000) and 1,023,999 for a config (it refuses 1,024,000). They are
// the limits the secret and config APIs and the agent hold their own creates to.
const (
	MaxSecretBytes = 511999
	MaxConfigBytes = 1023999
)

// The reasons ResolveObjects refuses an object. They are wrapped in an *ObjectError that names the
// object, so errors.Is finds the reason and errors.As the object.
var (
	// ErrObjectSource means a secret or config gives no source, or more than one.
	ErrObjectSource = errors.New("stacks: a secret or config needs exactly one of file, environment or content")
	// ErrObjectFileMissing means the file a secret or config names is not a file of the stack.
	ErrObjectFileMissing = errors.New("stacks: the file of a secret or config is not a file of the stack")
	// ErrObjectVarMissing means the variable a secret or config reads is not in the stack's table.
	ErrObjectVarMissing = errors.New("stacks: the variable of a secret or config is not in the stack's table")
	// ErrObjectEmpty means a secret or config has no content; a swarm refuses an empty one.
	ErrObjectEmpty = errors.New("stacks: a secret or config has no content")
	// ErrObjectTooLarge means a secret or config is over the size the swarm takes (D-40).
	ErrObjectTooLarge = errors.New("stacks: a secret or config is over the size limit")
	// ErrObjectUnsupported means a secret or config sets a driver or a template driver, which a
	// stack deploy does not apply: creating it without them would change what a service reads.
	ErrObjectUnsupported = errors.New("stacks: a secret or config sets a driver, which a stack deploy does not apply")
)

// MaxStandaloneFileBytes is the most a secret or config may hold on a standalone host: the agent's
// archive route takes at most 1,000,000 bytes of content for the one file it writes into a container
// (plan 07-04), for a secret and a config alike.
const MaxStandaloneFileBytes = 1000000

// ObjectError is a secret or config that cannot be resolved: its kind, the name the compose file
// uses and the reason. Target is the target it was judged for; the zero value is a swarm.
type ObjectError struct {
	Kind   ObjectKind
	Name   string
	Target Target
	Err    error
}

func (e *ObjectError) Error() string { return fmt.Sprintf("%s %q: %v", e.Kind, e.Name, e.Err) }

// Unwrap returns the reason.
func (e *ObjectError) Unwrap() error { return e.Err }

// ObjectContent is a secret or config a stack's services use, with the content a deploy creates
// it from. Name is the key the compose file uses under secrets or configs. An external object
// carries no content: it is only referenced, by ExternalName on the swarm. Content is shared with
// the stack's files and must not be changed.
type ObjectContent struct {
	Kind         ObjectKind
	Name         string
	External     bool
	ExternalName string
	Content      []byte
	// Labels are the labels the compose file gives the object. A deploy adds the stack's own
	// labels over them.
	Labels map[string]string
}

// ResolveObjects reads the content of every secret and config that at least one service uses
// (a secret or config reference, or a credential_spec config), secrets first and then configs,
// each in name order. The content comes from the stack's file named by file:, from the text of
// content:, or from the value of the variable named by environment:, and nothing else: it never
// reads the server's disk or environment. An external object is returned without content.
//
// An object no service uses is left out, as an unused network is: a deploy creates nothing a
// service does not need. A source that is missing, an empty content, a content over MaxSecretBytes
// or MaxConfigBytes and a driver the deploy does not apply are each an *ObjectError that names the
// object, and nothing has been sent anywhere by then.
func ResolveObjects(p *types.Project, files FileSet, vars map[string]string) ([]ObjectContent, error) {
	return ResolveObjectsFor(TargetSwarm, p, files, vars)
}

// ResolveObjectsFor is ResolveObjects judged for a target: the content is resolved the same way, and
// the size limit is the target's (the swarm's 500 KB and 1000 KB, or MaxStandaloneFileBytes for a
// standalone host, where a secret or config is written into the container as one file).
func ResolveObjectsFor(target Target, p *types.Project, files FileSet, vars map[string]string) ([]ObjectContent, error) {
	usedSecrets, usedConfigs := map[string]bool{}, map[string]bool{}
	for _, svc := range p.Services {
		for _, s := range svc.Secrets {
			usedSecrets[s.Source] = true
		}
		for _, c := range svc.Configs {
			usedConfigs[c.Source] = true
		}
		if cs := svc.CredentialSpec; cs != nil && cs.Config != "" {
			usedConfigs[cs.Config] = true
		}
	}

	var out []ObjectContent
	for _, name := range sortedUsed(usedSecrets) {
		decl, ok := p.Secrets[name]
		if !ok {
			continue // compose-go refuses an undeclared reference; the converter says so by name
		}
		oc, err := resolveOne(target, KindSecret, name, types.FileObjectConfig(decl), files, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, oc)
	}
	for _, name := range sortedUsed(usedConfigs) {
		decl, ok := p.Configs[name]
		if !ok {
			continue
		}
		oc, err := resolveOne(target, KindConfig, name, types.FileObjectConfig(decl), files, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, oc)
	}
	return out, nil
}

func sortedUsed(used map[string]bool) []string {
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func resolveOne(target Target, kind ObjectKind, name string, decl types.FileObjectConfig, files FileSet, vars map[string]string) (ObjectContent, error) {
	fail := func(err error) (ObjectContent, error) {
		return ObjectContent{}, &ObjectError{Kind: kind, Name: name, Target: target, Err: err}
	}
	if decl.External {
		return ObjectContent{Kind: kind, Name: name, External: true, ExternalName: cmp.Or(decl.Name, name)}, nil
	}
	if decl.Driver != "" || len(decl.DriverOpts) > 0 || decl.TemplateDriver != "" {
		return fail(ErrObjectUnsupported)
	}
	sources := 0
	for _, set := range []bool{decl.File != "", decl.Environment != "", decl.Content != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return fail(ErrObjectSource)
	}

	var content []byte
	switch {
	case decl.File != "":
		f, ok := files.Lookup(decl.File)
		if !ok {
			return fail(ErrObjectFileMissing)
		}
		content = f.Content
	case decl.Environment != "":
		v, ok := vars[decl.Environment]
		if !ok {
			return fail(ErrObjectVarMissing)
		}
		content = []byte(v)
	default:
		content = []byte(decl.Content)
	}

	limit := MaxSecretBytes
	switch {
	case target == TargetStandalone:
		limit = MaxStandaloneFileBytes
	case kind == KindConfig:
		limit = MaxConfigBytes
	}
	switch {
	case len(content) == 0:
		return fail(ErrObjectEmpty)
	case len(content) > limit:
		return fail(ErrObjectTooLarge)
	}
	return ObjectContent{Kind: kind, Name: name, Content: content, Labels: maps.Clone(decl.Labels)}, nil
}
