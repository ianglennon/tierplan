// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package lint is the target-aware part of a stack check (STK-03, D-10): what a compose file
// asks for that the stack's target will ignore or refuse. It is pure. It reads a loaded project
// and an index of the stored YAML's positions, and returns findings; it opens no file, calls no
// engine and imports neither the store nor an HTTP type.
//
// It does not import package stacks. stacks.Check calls Lint, so the dependency has to run the
// other way: the package defines its own Finding and Index, and stacks turns one into a Problem
// and implements the other with its position index.
//
// Wording follows the UI-SPEC "Lint copy": what is wrong, then what to do. A rule's name never
// reaches a message.
package lint

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/compose-spec/compose-go/v2/types"
)

// Target is where a stack deploys. Its values are those of stacks.Target.
type Target string

// The targets.
const (
	TargetSwarm      Target = "swarm"
	TargetStandalone Target = "standalone"
)

// Severity says how a finding stops a stack: an error blocks the deploy, a warning only informs.
// Its values are those of stacks.Severity.
type Severity string

// The severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// The codes of the findings. A code is stable; the message is for a person. stacks re-exports
// them as problem codes.
const (
	CodeIgnoredOnTarget          = "ignored_on_target"
	CodeBlockingKey              = "blocking_key"
	CodeBuildWithoutImage        = "build_without_image"
	CodeMovingTag                = "moving_tag"
	CodeReadOnlyFileObject       = "read_only_file_object"
	CodeNetworkOptionUnsupported = "network_option_unsupported"
	// What the agent's archive route refuses to write into a container (plan 07-04): an owner, a name a
	// ustar header cannot hold, and an object with nothing to write.
	CodeFileObjectOwner    = "file_object_owner"
	CodeFileObjectName     = "file_object_name"
	CodeExternalFileObject = "external_file_object"
	// CodeUseAPISocket is use_api_socket on a standalone host, which the Compose SDK would turn into a bind of
	// the host's Docker socket.
	CodeUseAPISocket = "use_api_socket_unsupported"
)

// Pos is a range in a stack file: 1-based line and column of its start, and the end.
type Pos struct {
	Line, Col, EndLine, EndCol int
}

// Index is what lint needs to know about the stored YAML. Paths are dotted: services.web.deploy.mode.
// A sequence item is addressed by its index, services.web.ports.0.mode. A key with a dot in it is
// matched whole.
type Index interface {
	// Lookup is the position of the key (or sequence item) a path names, and false when the file
	// has no such path.
	Lookup(path string) (Pos, bool)
	// Keys are the keys of the mapping a path names, in file order, or "0", "1", ... for a
	// sequence; nil for anything else.
	Keys(path string) []string
	// Scalar is the text of the scalar a path names.
	Scalar(path string) (string, bool)
}

// Input is what Lint judges: the target, the project as compose-go loaded it (before build is
// stripped, which is how a build key is seen) and the positions of the file it came from.
type Input struct {
	Target  Target
	Project *types.Project
	Index   Index
}

// Finding is one thing lint found, at a position of compose.yaml.
type Finding struct {
	Severity Severity
	Code     string
	Message  string
	// Key is the dotted path of the compose key at fault.
	Key string
	Pos Pos
}

// messages are the sentences of the rules, keyed by what they are for. Each says what is wrong,
// then what to do. The formats take the arguments named beside them.
var messages = map[string]string{
	// key, where ("Swarm" or "a standalone host"), what to do
	CodeIgnoredOnTarget: "`%s` is ignored on %s. %s",
	// key, what to do
	CodeBlockingKey: "`%s` can't be deployed to Swarm. %s",
	// service
	CodeBuildWithoutImage: "%s has build but no image. Build and push the image, then set image.",
	// image
	CodeMovingTag: "“%s” can move: it has no tag or uses :latest. Bayplan keeps the running digest on redeploys, but picks one when the service is new or the reference changes.",
	// service, kind ("secret" or "config"), name
	CodeReadOnlyFileObject: "%s is read_only, but a standalone host has to write the %s “%s” into the container. Remove read_only from the service.",
	// key, kind ("secret" or "config")
	CodeFileObjectOwner: "`%s` can't be set on a standalone host. Bayplan writes the %s into the container as the user the container starts as, and its owner can't be changed. Remove the key, or deploy this stack to a Swarm.",
	// service, kind, name, path
	CodeFileObjectName: "The path %[4]s of the %[2]s “%[3]s” in %[1]s is too long to write into a container. Use a shorter target.",
	// kind, name
	CodeExternalFileObject: "The %s “%s” is external, and a standalone host writes the content of each secret and config into the container. Declare it with a file, an environment variable or content instead.",
	CodeUseAPISocket:       "`use_api_socket` can't be used. It would mount the host's Docker socket into the container, past the checks a bind mount of the socket gets. Mount the socket as a bind volume if the service needs it; that needs Admin.",
	// key
	CodeNetworkOptionUnsupported: "`%s` isn't supported on a stack network. Bayplan creates networks without it. Remove it, or create the network yourself and use it as an external network.",
	// A build key that is ignored because the image is set (A5); the swarm table words it for Swarm.
	"build_with_image": "`build` is ignored. Bayplan doesn't build images; it deploys the image named in `image`.",
}

// ignoredMessage is the warning for a key a target ignores: "`{key}` is ignored on {target}. {what to do}".
func ignoredMessage(key string, target Target, action string) string {
	return fmt.Sprintf(messages[CodeIgnoredOnTarget], key, targetLabel(target), action)
}

func targetLabel(t Target) string {
	if t == TargetSwarm {
		return "Swarm"
	}
	return "a standalone host"
}

// Lint returns what the target will ignore or refuse in the project. The order is not
// significant; stacks sorts what it reports.
func Lint(in Input) []Finding {
	if in.Project == nil || in.Index == nil {
		return nil
	}
	out := buildRules(in)
	switch in.Target {
	case TargetSwarm:
		out = append(out, swarmFindings(in)...)
	case TargetStandalone:
		out = append(out, standaloneFindings(in)...)
	}
	out = append(out, imageFindings(in)...)
	out = append(out, networkFindings(in)...)
	return out
}

// whole is the position of a file with nothing to point at.
var whole = Pos{Line: 1, Col: 1, EndLine: 1, EndCol: 2}

// at is the position of the first of the paths that the file has, with that path; the start of
// the file and the first path when it has none.
func (in Input) at(paths ...string) (Pos, string) {
	for _, p := range paths {
		if pos, ok := in.Index.Lookup(p); ok {
			return pos, p
		}
	}
	return whole, paths[0]
}

// serviceNames are the project's services in name order, so findings come out the same way every time.
func serviceNames(p *types.Project) []string {
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// blockingMessage is the error for a key a swarm cannot deploy: "`{key}` can't be deployed to
// Swarm. {what to do}".
func blockingMessage(key string) string {
	return fmt.Sprintf(messages[CodeBlockingKey], key, swarmActions[key])
}

// networkFindings reports a stack network the agent would refuse to create: the agent copies the
// driver, scope, options, labels and IPAM subnet, gateway and ip_range of a network create and
// refuses one that sets IPAM options, auxiliary addresses, config_from or ingress (T-04-09), so
// the deploy would fail at the networks step. The rules read the stored YAML, because an empty
// options map says the same as none. An external network is never created, so it is left alone.
func networkFindings(in Input) []Finding {
	names := make([]string, 0, len(in.Project.Networks))
	for name := range in.Project.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Finding
	flag := func(path string) {
		pos, key := in.at(path)
		out = append(out, Finding{
			Severity: SeverityError, Code: CodeNetworkOptionUnsupported, Key: key, Pos: pos,
			Message: fmt.Sprintf(messages[CodeNetworkOptionUnsupported], key),
		})
	}
	for _, name := range names {
		if in.Project.Networks[name].External {
			continue
		}
		base := "networks." + name
		if len(in.Index.Keys(base+".ipam.options")) > 0 {
			flag(base + ".ipam.options")
		}
		for _, i := range in.Index.Keys(base + ".ipam.config") {
			if _, err := strconv.Atoi(i); err != nil {
				continue
			}
			if p := base + ".ipam.config." + i + ".aux_addresses"; len(in.Index.Keys(p)) > 0 {
				flag(p)
			}
		}
		if p := base + ".config_from"; set(in.Index, p) {
			flag(p)
		}
		if p := base + ".ingress"; set(in.Index, p) {
			flag(p)
		}
	}
	return out
}

// set reports whether the file gives the path a value that is not empty, null or false.
func set(ix Index, path string) bool {
	if len(ix.Keys(path)) > 0 {
		return true
	}
	if v, ok := ix.Scalar(path); ok {
		switch v {
		case "", "null", "~", "false":
			return false
		}
		return true
	}
	return false
}
