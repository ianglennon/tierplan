// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
	if len(compose) > MaxFileBytes {
		return nil
	}
	roots, ok := decodeRoots(compose)
	if !ok {
		return nil
	}
	w := &walker{budget: maxBudget, refs: map[string]struct{}{}, secrets: map[string]struct{}{}}
	for _, root := range roots {
		if w.measure(root, 0); w.err != nil {
			return nil
		}
	}
	for _, root := range roots {
		if w.document(root); w.err != nil {
			return nil
		}
	}
	out := make([]string, 0, len(w.secrets))
	for p := range w.secrets {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// MarkSecretFiles returns files with the Secret flag set on every file that is secret: one the
// compose file names under secrets, or one that is already secret in base, the files the new copy
// replaces. A secret file stays secret until it is deleted, so removing the reference later does
// not make the content plain. compose.yaml is never secret. The input is not changed.
func MarkSecretFiles(files []File, base []File) []File {
	secret := map[string]struct{}{}
	for _, p := range SecretFiles(WorkingCopy{Files: files}.Compose()) {
		secret[p] = struct{}{}
	}
	for _, f := range base {
		if f.Secret {
			secret[f.Path] = struct{}{}
		}
	}
	out := slices.Clone(files)
	for i, f := range out {
		if _, ok := secret[f.Path]; ok && f.Path != ComposeFile {
			out[i].Secret = true
		}
	}
	return out
}
