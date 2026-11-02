// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"sort"

	"github.com/ianglennon/tierplan/internal/stacks/lint"
)

// Severity says how a Problem stops a stack: an error blocks the deploy, a warning only informs.
type Severity string

// The severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// The codes of the problems Check reports. A code is stable: the editor and the API tests key on
// it, the message is for a person.
const (
	CodeYAMLInvalid          = "yaml_invalid"
	CodeSchemaInvalid        = "schema_invalid"
	CodeComposeInvalid       = "compose_invalid"
	CodeComposeTooLarge      = "compose_too_large"
	CodeComposeTooComplex    = "compose_too_complex"
	CodeIncludeUnsupported   = "include_unsupported"
	CodeExtendsUnsupported   = "extends_unsupported"
	CodeLabelFileUnsupported = "label_file_unsupported"
	CodeFileMissing          = "file_missing"
	CodePathIllegal          = "path_illegal"
	CodeRelativeBind         = "relative_bind"
	CodeInternalCheckFailed  = "internal_check_failed"
	CodeCheckTimeout         = "check_timeout"
	CodeUnsetVariable        = "unset_variable"
	CodeUnusedVariable       = "unused_variable"
	CodeEnvFileInvalid       = "env_file_invalid"
	CodeDotenvNotRead        = "dotenv_not_read"
	// CodePortsTooMany is a file whose ports and expose ranges add up to more than MaxPorts.
	CodePortsTooMany = "ports_too_many"
	// CodePortRangeInvalid is a port range outside 0 to 65535, or one that runs backwards.
	CodePortRangeInvalid = "port_range_invalid"
	// CodeSwarmNameInvalid is a service or network the stack would create on a swarm whose engine name swarm
	// refuses (D-48, D-53).
	CodeSwarmNameInvalid = "swarm_name_invalid"
	// CodeObjectTooLarge is a secret or config with more content than the target takes.
	CodeObjectTooLarge = "object_too_large"

	// The rules of a stack made from a git repository (D-07, D-09, D-10); see gitlint.go.

	// CodeEnvSecretLike is a key of the repository's .env whose name looks like a secret (a warning).
	CodeEnvSecretLike = "env_secret_like"
	// A path that is both a repository file and an overlay file is an overlay clash.
	CodeOverlayClash = "overlay_clash"
	// CodeRepoFileMissing is a file the compose files refer to that is neither in the repository nor an
	// overlay file.
	CodeRepoFileMissing = "repo_file_missing"
	// CodeLFSPointer is a repository file whose content is a Git LFS pointer.
	CodeLFSPointer = "git_lfs_pointer"
	// CodeSubmodule is a path that is a git submodule.
	CodeSubmodule = "git_submodule"
	// CodeSymlinkOutside is a repository link that points outside the stack folder.
	CodeSymlinkOutside = "git_symlink_outside"

	// The target-aware lint (STK-03); see package lint.
	CodeIgnoredOnTarget          = lint.CodeIgnoredOnTarget
	CodeBlockingKey              = lint.CodeBlockingKey
	CodeBuildWithoutImage        = lint.CodeBuildWithoutImage
	CodeMovingTag                = lint.CodeMovingTag
	CodeReadOnlyFileObject       = lint.CodeReadOnlyFileObject
	CodeFileObjectOwner          = lint.CodeFileObjectOwner
	CodeFileObjectName           = lint.CodeFileObjectName
	CodeExternalFileObject       = lint.CodeExternalFileObject
	CodeUseAPISocket             = lint.CodeUseAPISocket
	CodeStandaloneUnsupported    = lint.CodeStandaloneUnsupported
	CodeNetworkOptionUnsupported = lint.CodeNetworkOptionUnsupported
	CodeNetworkSubnetOverlap     = lint.CodeNetworkSubnetOverlap
	CodeNetworkSubnetOutsidePool = lint.CodeNetworkSubnetOutsidePool
)

// Problem is one thing the check found wrong with a stack: where (a file and a 1-based line and
// column, with the end of the range) and what (a stable code and a sentence that says what is
// wrong, then what to do).
type Problem struct {
	Severity Severity
	Code     string
	Message  string
	File     string
	Line     int
	Col      int
	EndLine  int
	EndCol   int
	// Key is the dotted path of the compose key at fault, such as services.web.image, when the
	// problem is about a key.
	Key string
	// Action is the one thing the editor offers to fix the problem, by a stable name (ActionAddOverlay),
	// or empty when it offers none.
	Action string
}

// The actions a problem can offer.
const (
	// ActionAddOverlay offers to add an overlay file for a path the repository lacks.
	ActionAddOverlay = "add_overlay"
	// ActionSetSecret offers to set a secret variable that overrides a value committed in the repository.
	ActionSetSecret = "set_secret"
)

// Problems is the list of what a check found.
type Problems []Problem

// Errors counts the problems that block a deploy.
func (p Problems) Errors() int { return p.count(SeverityError) }

// Warnings counts the problems that do not.
func (p Problems) Warnings() int { return p.count(SeverityWarning) }

func (p Problems) count(s Severity) int {
	n := 0
	for _, x := range p {
		if x.Severity == s {
			n++
		}
	}
	return n
}

// Sorted returns the problems ordered by file, then line, then column, then code, so a check of
// the same bytes always lists the same way. It does not change p.
func (p Problems) Sorted() Problems {
	out := make(Problems, len(p))
	copy(out, p)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.Code < b.Code
	})
	return out
}
