// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"sort"

	"github.com/ianglennon/bayplan/internal/stacks/lint"
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
	// CodeObjectTooLarge is a secret or config with more content than the target takes.
	CodeObjectTooLarge = "object_too_large"

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
}

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
