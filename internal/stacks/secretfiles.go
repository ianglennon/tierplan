// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

// SecretFiles returns the paths of the stack files the compose file uses as secrets.
func SecretFiles(compose []byte) []string { return nil }

// MarkSecretFiles returns files with the Secret flag set on every secret file.
func MarkSecretFiles(files []File, base []File) []File { return files }
