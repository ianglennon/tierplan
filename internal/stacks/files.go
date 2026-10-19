// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import "errors"

// The limits of a stack's files (D-05, T-07-11, T-07-12).
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
	// ErrComposeMissing means a stack has no compose.yaml.
	ErrComposeMissing = errors.New("stacks: a stack needs a compose.yaml")
)

// ValidPath reports why p cannot be the path of a stack file, or nil.
func ValidPath(p string) error { return nil }

// CheckFiles reports the first rule the files of a stack break, or nil.
func CheckFiles(files []File) error { return nil }

// FileSet is a stack's files by path.
type FileSet map[string]File

// Lookup finds the file a reference names.
func (s FileSet) Lookup(path string) (File, bool) { return File{}, false }

// FileError is a rule broken by one file: which, and which rule. errors.Is reaches the rule.
type FileError struct {
	Path string
	Err  error
}

func (e *FileError) Error() string { return e.Err.Error() }

// Unwrap returns the rule.
func (e *FileError) Unwrap() error { return e.Err }

// NewFileSet indexes files by path.
func NewFileSet(files []File) FileSet { return nil }
