// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The limits of a stack's files (D-05, T-07-11, T-07-12). The per-file limit is MaxFileBytes.
const (
	// MaxFiles is the most files one stack holds, compose.yaml included.
	MaxFiles = 50
	// MaxTotalBytes is the most all the files of one stack hold together.
	MaxTotalBytes = 8 << 20
	// MaxPathChars is the longest path of a stack file, in characters.
	MaxPathChars = 255
	// MaxSegmentChars is the longest single name in a path, in characters.
	MaxSegmentChars = 100
)

var (
	// ErrPathInvalid means a file path is not a path inside the stack.
	ErrPathInvalid = errors.New("stacks: a file path must be relative and inside the stack")
	// ErrPathTaken means two files have the same path.
	ErrPathTaken = errors.New("stacks: two files have the same path")
	// ErrFileTooLarge means a file is over MaxFileBytes.
	ErrFileTooLarge = errors.New("stacks: a file is over the size limit")
	// ErrStackTooLarge means a stack has too many files or too many bytes.
	ErrStackTooLarge = errors.New("stacks: the stack's files are over the limit")
	// ErrComposeMissing means a stack does not hold one of its compose files: compose.yaml, or a file
	// named in its list of compose files.
	ErrComposeMissing = errors.New("stacks: a stack needs a compose.yaml")
)

// FileError is a rule broken by one file: which file (empty when the rule is about the stack as a
// whole) and which rule. errors.Is reaches the rule and errors.As the path.
type FileError struct {
	Path string
	Err  error
}

func (e *FileError) Error() string { return e.Err.Error() }

// Unwrap returns the rule.
func (e *FileError) Unwrap() error { return e.Err }

// ValidPath reports why p cannot be the path of a stack file, or nil. A path is relative, written
// with "/" and made of plain names: no leading or doubled "/", no "." or ".." name, no backslash
// (a Windows separator), no colon (a drive such as "C:" or a bind-mount separator), no control,
// format or line-separator character (a NUL, a newline, a right-to-left override that makes one
// name look like another), valid UTF-8, at most MaxPathChars characters in all and MaxSegmentChars
// in a name.
//
// The rule is a single source for every layer that takes a path from a person: the save, the
// working copy and the lookup of an env_file or a secret file. It matters most where a path
// reaches something that reads a file, and in this package nothing does: see the package comment.
func ValidPath(p string) error {
	bad := &FileError{Path: p, Err: ErrPathInvalid}
	if p == "" || strings.HasPrefix(p, "/") || !utf8.ValidString(p) || utf8.RuneCountInString(p) > MaxPathChars {
		return bad
	}
	for _, r := range p {
		if r == '\\' || r == ':' || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return bad
		}
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." || utf8.RuneCountInString(seg) > MaxSegmentChars {
			return bad
		}
	}
	return nil
}

// CheckFiles reports the first rule the files of a stack break, or nil. The error is a *FileError
// naming the file at fault (none for a rule about the whole stack). The count is judged before
// anything else, so a request with a great many files is refused without looking at each. The
// stack's one compose file is compose.yaml.
func CheckFiles(files []File) error {
	return CheckFilesFor(files, nil)
}

// CheckFilesFor is CheckFiles for a stack whose compose files are named (WorkingCopy.ComposeFiles):
// each must be a valid path and one of files, and none may be listed twice. A listed file the stack
// does not hold is ErrComposeMissing naming it. Empty composeFiles is the one compose.yaml.
func CheckFilesFor(files []File, composeFiles []string) error {
	if len(files) > MaxFiles {
		return &FileError{Err: ErrStackTooLarge}
	}
	seen := make(map[string]struct{}, len(files))
	total := 0
	for _, f := range files {
		if err := ValidPath(f.Path); err != nil {
			return err
		}
		if _, dup := seen[f.Path]; dup {
			return &FileError{Path: f.Path, Err: ErrPathTaken}
		}
		seen[f.Path] = struct{}{}
		if len(f.Content) > MaxFileBytes {
			return &FileError{Path: f.Path, Err: ErrFileTooLarge}
		}
		total += len(f.Content)
	}
	if total > MaxTotalBytes {
		return &FileError{Err: ErrStackTooLarge}
	}
	listed := make(map[string]struct{}, len(composeFiles))
	paths := WorkingCopy{ComposeFiles: composeFiles}.ComposePaths()
	for _, p := range paths {
		if err := ValidPath(p); err != nil {
			return err
		}
		if _, dup := listed[p]; dup {
			return &FileError{Path: p, Err: ErrPathTaken}
		}
		listed[p] = struct{}{}
		if _, ok := seen[p]; !ok {
			return &FileError{Path: p, Err: ErrComposeMissing}
		}
	}
	return nil
}

// FileSet is a stack's files by path: the only place an env_file, a secret file or a config file
// of a compose file is looked for (D-05).
type FileSet map[string]File

// NewFileSet indexes files by path.
func NewFileSet(files []File) FileSet {
	set := make(FileSet, len(files))
	for _, f := range files {
		set[f.Path] = f
	}
	return set
}

// Lookup finds the file a reference in the compose file names. A leading "./" is the usual way to
// write a path inside the stack and is dropped; anything else must be a valid stack path, so a
// reference with "..", an absolute path or a drive finds nothing. It never reads a filesystem.
func (s FileSet) Lookup(ref string) (File, bool) {
	p := strings.TrimPrefix(ref, "./")
	if ValidPath(p) != nil {
		return File{}, false
	}
	f, ok := s[p]
	return f, ok
}
