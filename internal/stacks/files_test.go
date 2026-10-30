// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/stacks"
)

func TestValidPath(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a/", 100) + "x" // 201 characters, every segment short
	for name, p := range map[string]string{
		"a plain file":       "nginx.conf",
		"a dotfile":          ".env",
		"a nested file":      "config/nginx.conf",
		"a deeper file":      "a/b/c/d.txt",
		"the compose file":   "compose.yaml",
		"a name with spaces": "my file.txt",
		"a unicode name":     "konfiguracja/zażółć.conf",
		"a name with a dot":  "app.prod.env",
		"255 characters":     strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 53),
		"a long nest":        long,
	} {
		if err := stacks.ValidPath(p); err != nil {
			t.Errorf("%s (%q): ValidPath = %v, want nil", name, p, err)
		}
	}
	for name, p := range map[string]string{
		"empty":                    "",
		"absolute":                 "/etc/passwd",
		"parent":                   "../x",
		"parent inside":            "a/../b",
		"parent at the end":        "a/..",
		"just dots":                "..",
		"a dot":                    ".",
		"a dot segment":            "a/./b",
		"leading dot slash":        "./x",
		"a doubled slash":          "a//b",
		"a trailing slash":         "a/",
		"a backslash":              `a\b`,
		"a windows backslash path": `C:\x`,
		"a drive path":             "C:/x",
		"a lower case drive":       "c:/x",
		"a drive alone":            "C:",
		"a colon in a name":        "a:b",
		"a nul":                    "x\x00",
		"a newline":                "a\nb",
		"a tab":                    "a\tb",
		"a delete":                 "a\x7fb",
		"a right-to-left mark":     "a\u202eb",
		"a line separator":         "a\u2028b",
		"invalid utf-8":            "a\xffb",
		"256 characters":           strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 54),
		"a segment of 101":         strings.Repeat("a", 101),
		"a long segment inside":    "ok/" + strings.Repeat("a", 101) + "/x",
	} {
		err := stacks.ValidPath(p)
		if !errors.Is(err, stacks.ErrPathInvalid) {
			t.Errorf("%s (%q): ValidPath = %v, want ErrPathInvalid", name, p, err)
		}
	}
}

func TestValidPathCountsCharactersNotBytes(t *testing.T) {
	t.Parallel()
	// 100 two-byte characters: 200 bytes, 100 characters, so a legal segment.
	if err := stacks.ValidPath(strings.Repeat("ż", 100)); err != nil {
		t.Errorf("100 characters of 2 bytes: %v, want nil", err)
	}
	if err := stacks.ValidPath(strings.Repeat("ż", 101)); !errors.Is(err, stacks.ErrPathInvalid) {
		t.Errorf("101 characters: %v, want ErrPathInvalid", err)
	}
}

func compose() stacks.File {
	return stacks.File{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}
}

func TestCheckFilesAcceptsAStackWithAComposeFile(t *testing.T) {
	t.Parallel()
	files := []stacks.File{compose(), {Path: ".env", Content: []byte("A=1")}, {Path: "config/a.conf"}}
	if err := stacks.CheckFiles(files); err != nil {
		t.Errorf("CheckFiles = %v, want nil", err)
	}
}

func TestCheckFilesNamesTheRuleAndThePath(t *testing.T) {
	t.Parallel()
	big := make([]byte, stacks.MaxFileBytes+1)
	exact := make([]byte, stacks.MaxFileBytes)
	many := []stacks.File{compose()}
	for i := 0; i < stacks.MaxFiles; i++ {
		many = append(many, stacks.File{Path: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"})
	}
	var heavy []stacks.File
	heavy = append(heavy, compose())
	for i := 0; i < 8; i++ { // 8 files of exactly 1 MiB plus compose.yaml: over 8 MiB
		heavy = append(heavy, stacks.File{Path: "d" + string(rune('a'+i)), Content: exact})
	}
	atLimit := []stacks.File{{Path: stacks.ComposeFile}}
	for i := 0; i < 8; i++ {
		atLimit = append(atLimit, stacks.File{Path: "d" + string(rune('a'+i)), Content: exact})
	}
	// atLimit holds 8 MiB exactly (compose.yaml is empty), the most that is allowed.
	if err := stacks.CheckFiles(atLimit); err != nil {
		t.Errorf("exactly %d bytes: %v, want nil", stacks.MaxTotalBytes, err)
	}

	for name, tc := range map[string]struct {
		files []stacks.File
		want  error
		path  string
	}{
		"no compose file":       {[]stacks.File{{Path: "app.env"}}, stacks.ErrComposeMissing, stacks.ComposeFile},
		"no files":              {nil, stacks.ErrComposeMissing, stacks.ComposeFile},
		"a repeated path":       {[]stacks.File{compose(), {Path: "a"}, {Path: "a"}}, stacks.ErrPathTaken, "a"},
		"a repeated compose":    {[]stacks.File{compose(), compose()}, stacks.ErrPathTaken, stacks.ComposeFile},
		"a bad path":            {[]stacks.File{compose(), {Path: "../x"}}, stacks.ErrPathInvalid, "../x"},
		"a file over 1 MiB":     {[]stacks.File{compose(), {Path: "big.bin", Content: big}}, stacks.ErrFileTooLarge, "big.bin"},
		"a big compose file":    {[]stacks.File{{Path: stacks.ComposeFile, Content: big}}, stacks.ErrFileTooLarge, stacks.ComposeFile},
		"more than 50 files":    {many, stacks.ErrStackTooLarge, ""},
		"more than 8 MiB total": {append(heavy, stacks.File{Path: "one", Content: []byte("x")}), stacks.ErrStackTooLarge, ""},
	} {
		err := stacks.CheckFiles(tc.files)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: CheckFiles = %v, want %v", name, err, tc.want)
			continue
		}
		var fe *stacks.FileError
		if !errors.As(err, &fe) || fe.Path != tc.path {
			t.Errorf("%s: path = %+v, want %q", name, fe, tc.path)
		}
	}
}

func TestCheckFilesAllowsFiftyFilesAndRefusesFiftyOne(t *testing.T) {
	t.Parallel()
	files := []stacks.File{compose()}
	for i := 1; i < stacks.MaxFiles; i++ {
		files = append(files, stacks.File{Path: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	if len(files) != stacks.MaxFiles {
		t.Fatalf("built %d files", len(files))
	}
	if err := stacks.CheckFiles(files); err != nil {
		t.Errorf("%d files: %v, want nil", len(files), err)
	}
	files = append(files, stacks.File{Path: "extra"})
	if err := stacks.CheckFiles(files); !errors.Is(err, stacks.ErrStackTooLarge) {
		t.Errorf("%d files: %v, want ErrStackTooLarge", len(files), err)
	}
}

func TestFileSetLookupFindsOnlyWhatTheStackHolds(t *testing.T) {
	t.Parallel()
	set := stacks.NewFileSet([]stacks.File{compose(), {Path: "app.env", Content: []byte("A=1")}, {Path: "config/a.conf"}})
	for _, ref := range []string{"app.env", "./app.env", "config/a.conf", "./config/a.conf", "compose.yaml"} {
		if _, ok := set.Lookup(ref); !ok {
			t.Errorf("Lookup(%q) found nothing", ref)
		}
	}
	if f, ok := set.Lookup("./app.env"); !ok || string(f.Content) != "A=1" {
		t.Errorf("Lookup(./app.env) = %+v, %v", f, ok)
	}
	for _, ref := range []string{"", "missing.env", "/etc/passwd", "../app.env", "config/../app.env", "config//a.conf", `config\a.conf`, "C:/app.env", "app.env\x00", "."} {
		if f, ok := set.Lookup(ref); ok {
			t.Errorf("Lookup(%q) = %+v, want nothing: a reference outside the stack's own files is never found", ref, f)
		}
	}
}
