// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package stackstest holds test support for the packages that check, review and deploy stacks. It is
// imported by tests only.
package stackstest

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ianglennon/coaming/internal/stacks"
)

// samplePath stands in for a field's key when a withheld sentence's fixed part is worked out.
const samplePath = "x"

// fixedSentences are the characters of Coaming's own withheld sentences that hold no value: the general
// sentence, and what follows the key path in the sentence for a field and in the sentence for a file. They
// are worked out from stacks.SecretValueMessage and stacks.SecretFileMessage, never typed in, so they
// follow those functions.
func fixedSentences() []string {
	return []string{
		stacks.SecretValueMessage(""),
		strings.TrimPrefix(stacks.SecretValueMessage(samplePath), samplePath),
		strings.TrimPrefix(stacks.SecretFileMessage(samplePath), samplePath),
	}
}

// NoSecretRun fails the test for every run of stacks.MinSecretLen characters of secret that it finds in
// any of texts.
//
// Before it scans, it removes only the characters of Coaming's own fixed sentences: the general sentence
// of stacks.SecretValueMessage(""), and the part after the key path of stacks.SecretValueMessage(path)
// and stacks.SecretFileMessage(path). Those are constants that hold no value, and without the removal a
// secret such as "2024secretword" would match the word "secret" in them. The key path before a sentence
// is not removed, so a secret placed there is reported. The text is split at each removed sentence and
// each piece is scanned on its own, so the removal never joins two pieces into a run.
//
// Use it on texts built from the words of the loader, the converter or a job: problem messages, the
// detail and errors of a 422, and log text. Decode JSON first, so that field names and links of the API
// are not scanned. Never use it on text that echoes the caller's own input on purpose, such as the
// guesses of an oracle test, which are windows of the secret by design.
//
// A secret shorter than stacks.MinSecretLen is a Fatalf, because it would check nothing.
func NoSecretRun(t testing.TB, secret string, texts ...string) {
	t.Helper()
	if utf8.RuneCountInString(secret) < stacks.MinSecretLen {
		t.Fatalf("NoSecretRun: the secret has fewer than %d characters, so there is no run to look for", stacks.MinSecretLen)
		return
	}
	windows := windowsOf(secret)
	fixed := fixedSentences()
	for i, text := range texts {
		pieces := []string{text}
		for _, sentence := range fixed {
			var next []string
			for _, p := range pieces {
				next = append(next, strings.Split(p, sentence)...)
			}
			pieces = next
		}
		for _, w := range windows {
			for _, p := range pieces {
				if strings.Contains(p, w) {
					t.Errorf("text %d holds %q, a run of %d characters of the secret: %.300s", i, w, stacks.MinSecretLen, text)
					break
				}
			}
		}
	}
}

// windowsOf is every distinct run of stacks.MinSecretLen runes of s, in order.
func windowsOf(s string) []string {
	rs := []rune(s)
	seen := map[string]bool{}
	var out []string
	for i := 0; i+stacks.MinSecretLen <= len(rs); i++ {
		w := string(rs[i : i+stacks.MinSecretLen])
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}
