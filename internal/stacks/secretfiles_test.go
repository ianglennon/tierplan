// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ianglennon/coaming/internal/stacks"
)

// Secret stack files (plan 07-13, STK-04, UI-SPEC Defaults 6): a file the compose file uses under
// secrets: is a secret file, and stays one until it is deleted.

func TestSecretFilesReturnsTheFileOfASecretAndNothingForConfigs(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27\n" +
		"secrets:\n  tok:\n    file: tok.txt\n" +
		"configs:\n  site:\n    file: nginx.conf\n"
	if got, want := stacks.SecretFiles([]byte(compose)), []string{"tok.txt"}; !slices.Equal(got, want) {
		t.Errorf("SecretFiles = %v, want %v: a config file is not a secret", got, want)
	}
}

func TestSecretFilesCleansSortsAndRepeatsNothing(t *testing.T) {
	t.Parallel()
	compose := "secrets:\n  b:\n    file: ./z/key.pem\n  a:\n    file: a.txt\n  c:\n    file: a.txt\n  d:\n    environment: FROM_ENV\n  e:\n    external: true\n"
	if got, want := stacks.SecretFiles([]byte(compose)), []string{"a.txt", "z/key.pem"}; !slices.Equal(got, want) {
		t.Errorf("SecretFiles = %v, want %v: ./ cleaned, sorted, once each, and only secrets that name a file", got, want)
	}
}

func TestSecretFilesFollowsAnAnchorAndNamesNoPathAStackCouldNotHave(t *testing.T) {
	t.Parallel()
	compose := "x-file: &f tok.txt\nsecrets:\n  a:\n    file: *f\n  b:\n    file: ../outside\n  c:\n    file: /etc/shadow\n"
	if got, want := stacks.SecretFiles([]byte(compose)), []string{"tok.txt"}; !slices.Equal(got, want) {
		t.Errorf("SecretFiles = %v, want %v", got, want)
	}
}

func TestSecretFilesOfTextThatIsNotAComposeFileIsNone(t *testing.T) {
	t.Parallel()
	for name, compose := range map[string]string{
		"empty":           "",
		"not yaml":        "secrets: [\n",
		"no such section": "services:\n  web:\n    image: nginx\n",
		"a scalar root":   "hello\n",
	} {
		if got := stacks.SecretFiles([]byte(compose)); len(got) != 0 {
			t.Errorf("%s: SecretFiles = %v, want none", name, got)
		}
	}
}

func TestMarkSecretFilesMarksTheReferencedFileAndOnlyIt(t *testing.T) {
	t.Parallel()
	files := []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("secrets:\n  tok:\n    file: tok.txt\n")},
		{Path: "tok.txt", Content: []byte("abc123")},
		{Path: "notes.txt", Content: []byte("plain")},
	}
	got := stacks.MarkSecretFiles(files, nil)
	secret := map[string]bool{}
	for _, f := range got {
		secret[f.Path] = f.Secret
	}
	if !secret["tok.txt"] || secret["notes.txt"] || secret[stacks.ComposeFile] {
		t.Errorf("secret flags = %v, want only tok.txt", secret)
	}
	if files[1].Secret {
		t.Error("MarkSecretFiles changed its input")
	}
}

func TestMarkSecretFilesKeepsAFileSecretWhenTheReferenceIsGone(t *testing.T) {
	t.Parallel()
	base := []stacks.File{{Path: "tok.txt", Content: []byte("abc123"), Secret: true}}
	files := []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("services: {}\n")},
		{Path: "tok.txt", Content: []byte("abc123")},
	}
	got := stacks.MarkSecretFiles(files, base)
	if !got[1].Secret {
		t.Error("tok.txt is plain once the reference is gone, want it secret until it is deleted")
	}
}

func TestMarkSecretFilesLetsADeletedAndReaddedFileStartAgainAsPlain(t *testing.T) {
	t.Parallel()
	// The file was secret in the draft, was deleted and saved, and is added again: the base the
	// next save is judged against no longer has it, so it is a new, plain file.
	base := []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}}
	files := []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("services: {}\n")},
		{Path: "tok.txt", Content: []byte("abc123")},
	}
	if got := stacks.MarkSecretFiles(files, base); got[1].Secret {
		t.Error("a file that is new and not referenced was made secret")
	}
}

func TestMarkSecretFilesNeverMakesTheComposeFileSecret(t *testing.T) {
	t.Parallel()
	base := []stacks.File{{Path: stacks.ComposeFile, Content: []byte("x"), Secret: true}}
	files := []stacks.File{{Path: stacks.ComposeFile, Content: []byte("secrets:\n  a:\n    file: compose.yaml\n")}}
	if got := stacks.MarkSecretFiles(files, base); got[0].Secret {
		t.Error("compose.yaml was made secret")
	}
}

func TestCheckNamesTheFilesThatAreSecret(t *testing.T) {
	t.Parallel()
	wc := stacks.WorkingCopy{Files: []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("services:\n  web:\n    image: nginx:1.27\nsecrets:\n  tok:\n    file: tok.txt\n")},
		{Path: "tok.txt", Content: []byte("abc123")},
		{Path: "old.key", Content: []byte("kept"), Secret: true},
		{Path: "notes.txt", Content: []byte("plain")},
	}}
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "shop", Target: stacks.TargetSwarm, Copy: wc})
	if want := []string{"old.key", "tok.txt"}; !slices.Equal(res.SecretFiles, want) {
		t.Errorf("SecretFiles = %v, want %v: the referenced file and the one already flagged, not the plain one", res.SecretFiles, want)
	}
}
