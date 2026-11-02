// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"slices"
	"sort"
)

// SecretFiles returns the paths of the stack files the compose file uses as secrets: each
// top-level secrets entry that names a file (secrets: {tok: {file: tok.txt}}), cleaned of a
// leading "./", sorted and without repeats. A file named only by configs or env_file is not one,
// and neither is a path no stack could have. The paths are as the compose file names them: a
// caller keeps the ones it has a file for.
//
// A file that is secret by this rule is stored sealed, shown as <hidden> and revealed only under
// audit (UI-SPEC Defaults 6, D-02). The compose file is not read for anything else here, so a
// compose file that is not YAML, or that is too large or too complex to walk, has none.
func SecretFiles(compose []byte) []string {
	uses := SecretFileUses([]ComposeContent{{Path: ComposeFile, Content: compose}})
	out := make([]string, len(uses))
	for i, u := range uses {
		out[i] = u.Path
	}
	return out
}

// SecretFileUse is a stack file that is used as a secret and the compose file that says so.
type SecretFileUse struct {
	// Path is the stack file, as the compose file names it without a leading "./".
	Path string
	// Compose is the first compose file, in merge order, that names Path under secrets.
	Compose string
}

// SecretFileUses is SecretFiles over every compose file of a working copy: each stack file one of them
// uses as a secret, with the compose file that declares the use, sorted by path. A compose file that is
// not YAML, or is too large or too complex to walk, has none; the others are still read.
func SecretFileUses(contents []ComposeContent) []SecretFileUse {
	first := map[string]string{}
	for _, c := range contents {
		if len(c.Content) > MaxFileBytes {
			continue
		}
		roots, ok := decodeRoots(c.Content)
		if !ok {
			continue
		}
		w := &walker{budget: maxBudget, refs: map[string]struct{}{}, secrets: map[string]struct{}{}, file: c.Path}
		failed := false
		for _, root := range roots {
			if w.measure(root, 0); w.err != nil {
				failed = true
				break
			}
		}
		for _, root := range roots {
			if failed {
				break
			}
			if w.document(root); w.err != nil {
				failed = true
			}
		}
		if failed {
			continue
		}
		for p := range w.secrets {
			if _, seen := first[p]; !seen {
				first[p] = c.Path
			}
		}
	}
	out := make([]SecretFileUse, 0, len(first))
	for p, c := range first {
		out = append(out, SecretFileUse{Path: p, Compose: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// MarkSecretFiles returns files with the Secret flag set on every file that is secret: one the
// compose file names under secrets, or one that is already secret in base, the files the new copy
// replaces. A secret file stays secret until it is deleted, so removing the reference later does
// not make the content plain. compose.yaml is never secret. The input is not changed.
func MarkSecretFiles(files []File, base []File) []File {
	return MarkSecretFilesFor(files, base, nil)
}

// MarkSecretFilesFor is MarkSecretFiles for a stack whose compose files are named: every file in
// composeFiles is read for secrets, and none of them is ever secret. Empty composeFiles is the one
// compose.yaml.
func MarkSecretFilesFor(files []File, base []File, composeFiles []string) []File {
	secret := map[string]struct{}{}
	copyOf := WorkingCopy{Files: files, ComposeFiles: composeFiles}
	for _, u := range SecretFileUses(copyOf.ComposeContents()) {
		secret[u.Path] = struct{}{}
	}
	for _, f := range base {
		if f.Secret {
			secret[f.Path] = struct{}{}
		}
	}
	isCompose := map[string]struct{}{}
	for _, p := range copyOf.ComposePaths() {
		isCompose[p] = struct{}{}
	}
	out := slices.Clone(files)
	for i, f := range out {
		if _, ok := secret[f.Path]; !ok {
			continue
		}
		if _, c := isCompose[f.Path]; !c {
			out[i].Secret = true
		}
	}
	return out
}

// MaskFiles are the files a masker is built from. For a stack with named compose files the secrets are
// found in all of them, because NewMasker reads compose.yaml only; a stack with the one compose.yaml is
// handed on as it is.
func (w WorkingCopy) MaskFiles() []File {
	if !w.multiFile() {
		return w.Files
	}
	return MarkSecretFilesFor(w.Files, nil, w.ComposeFiles)
}
