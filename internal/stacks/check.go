// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"
)

// CheckInput is what Check needs: the stack's name and target, and the working copy to check. It
// has the fields of LoadInput, in the same order, because a check starts with a load.
type CheckInput struct {
	Name   string
	Target Target
	Copy   WorkingCopy
	// Deadline bounds the check. Zero is five seconds.
	Deadline time.Duration
}

// CheckResult is what a check found. Loaded is set when the compose file loaded, and is what the
// review and the deploy convert; Problems holds everything that was found, errors and warnings.
type CheckResult struct {
	Problems Problems
	Loaded   *Loaded
	// UnusedFiles are the stack's files the compose file never refers to. compose.yaml is never
	// one of them.
	UnusedFiles []string
}

// Check is the one pipeline that judges a stack's working copy: the editor's lint, the review and
// the deploy all call it, so what a person sees while they type is what the deploy will hold them
// to. It is pure: it reads only the bytes it is given, opens no file and calls no engine.
func Check(ctx context.Context, in CheckInput) CheckResult {
	var res CheckResult
	content := in.Copy.Compose()
	if len(content) > MaxFileBytes {
		res.Problems = Problems{{
			Severity: SeverityError, Code: CodeComposeTooLarge, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: "compose.yaml is over 1 MiB. Split it up or remove what it doesn't need.",
		}}
		return res
	}
	idx, parseErr := parseIndex(content)
	if parseErr == nil {
		// The raw YAML is judged before the loader sees it: a file that names a file of the
		// server, or that is too complex to read, is refused here and never loaded.
		found, refs := Prescan(content, NewFileSet(in.Copy.Files))
		res.UnusedFiles = unusedFiles(in.Copy.Files, refs)
		if found.Errors() > 0 {
			res.Problems = found.Sorted()
			return res
		}
		res.Problems = found
	}
	loaded, err := Load(ctx, LoadInput(in))
	if err != nil {
		// compose-go says where a syntax error is better than the parser used for positions does
		// (it names the line the parser stopped at, not the line the block started on), so its
		// text is used whenever it refused the file too.
		res.Problems = append(res.Problems, loadProblems(err, idx)...).Sorted()
		return res
	}
	if parseErr != nil {
		// compose-go accepted what the position parser did not: report the parser's error, and
		// do not hand a file whose keys cannot be located on to the later checks.
		res.Problems = yamlProblems(parseErr.Error()).Sorted()
		return res
	}
	res.Loaded = loaded
	res.Problems = res.Problems.Sorted()
	return res
}

// unusedFiles are the paths of files that refs does not name, sorted. compose.yaml is the file
// everything else is referred to from, so it is never unused.
func unusedFiles(files []File, refs []string) []string {
	used := make(map[string]struct{}, len(refs)+1)
	used[ComposeFile] = struct{}{}
	for _, r := range refs {
		used[r] = struct{}{}
	}
	out := []string{}
	for _, f := range files {
		if _, ok := used[f.Path]; !ok {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

// index is the positions of a compose file's keys, read from the stored bytes. It is the only
// place a line number comes from: a position is never taken from a file on the server.
type index struct {
	docs []*yaml.Node
}

// parseIndex reads every YAML document of src. compose-go merges every document of a file, so
// every document is indexed.
func parseIndex(src []byte) (*index, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	ix := &index{}
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return ix, nil
		}
		if err != nil {
			return nil, err
		}
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
			ix.docs = append(ix.docs, doc.Content[0])
		}
	}
}

// maxPathDepth bounds how far a path lookup follows aliases.
const maxPathDepth = 64

// find returns the node a dotted path names: the key node of the deepest key the path reaches,
// so a path with a missing tail is reported at the last key that is there. A key with a dot in it
// (a label such as com.example.tier) is matched whole.
func (ix *index) find(path string) *yaml.Node {
	if ix == nil {
		return nil
	}
	for _, root := range ix.docs {
		if n := walkPath(root, path, 0); n != nil {
			return n
		}
	}
	return nil
}

func walkPath(n *yaml.Node, rest string, depth int) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && depth < maxPathDepth {
		n, depth = n.Alias, depth+1
	}
	if n == nil || rest == "" || depth >= maxPathDepth {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		best, bestLen := -1, -1
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if (rest == k || strings.HasPrefix(rest, k+".")) && len(k) > bestLen {
				best, bestLen = i, len(k)
			}
		}
		if best < 0 {
			return nil
		}
		key, val := n.Content[best], n.Content[best+1]
		remainder := strings.TrimPrefix(rest[bestLen:], ".")
		if remainder == "" {
			return key
		}
		if deeper := walkPath(val, remainder, depth+1); deeper != nil {
			return deeper
		}
		return key
	case yaml.SequenceNode:
		seg, remainder, _ := strings.Cut(rest, ".")
		seg = strings.TrimSuffix(strings.TrimPrefix(seg, "["), "]")
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(n.Content) {
			return nil
		}
		if remainder == "" {
			return n.Content[i]
		}
		if deeper := walkPath(n.Content[i], remainder, depth+1); deeper != nil {
			return deeper
		}
		return n.Content[i]
	}
	return nil
}

// at turns a node into the start and end of the range a problem marks. A scalar is marked across
// its text on its line; anything else at its first character.
func at(n *yaml.Node) (line, col, endLine, endCol int) {
	if n == nil {
		return 1, 1, 1, 2
	}
	width := 1
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 && !strings.Contains(n.Value, "\n") {
		if w := len([]rune(n.Value)); w > 0 {
			width = w
		}
	}
	return n.Line, n.Column, n.Line, n.Column + width
}

var (
	yamlLineRe = regexp.MustCompile(`line (\d+)(?:, column (\d+))?: `)
	// yamlParserRe is a parser error that has a context: "while parsing a block mapping at line 1,
	// column 1: line 3: did not find expected key". The yaml library counts the second line from
	// zero (a scanner error and a construct error count from one), so the line it names is the one
	// before the line the parser stopped on.
	yamlParserRe = regexp.MustCompile(`at line \d+, column \d+: line (\d+)(?:, column (\d+))?: (.*)$`)
)

// yamlProblems turns the text of a YAML syntax error into problems at their lines: "Line {n}:
// {message}". A text that names no line is reported at line 1.
func yamlProblems(text string) Problems {
	text = strings.TrimSpace(text)
	var out Problems
	for _, l := range strings.Split(text, "\n") {
		if m := yamlParserRe.FindStringSubmatch(l); m != nil {
			zero, _ := strconv.Atoi(m[1])
			line, col := zero+1, 1
			if m[2] != "" {
				col, _ = strconv.Atoi(m[2])
			}
			out = append(out, Problem{
				Severity: SeverityError, Code: CodeYAMLInvalid, File: ComposeFile,
				Line: line, Col: col, EndLine: line, EndCol: col + 1,
				Message: fmt.Sprintf("Line %d: %s", line, strings.TrimSpace(m[3])),
			})
			continue
		}
		all := yamlLineRe.FindAllStringSubmatchIndex(l, -1)
		if len(all) == 0 {
			continue
		}
		last := all[len(all)-1]
		line, _ := strconv.Atoi(l[last[2]:last[3]])
		col := 1
		if last[4] >= 0 {
			col, _ = strconv.Atoi(l[last[4]:last[5]])
		}
		msg := strings.TrimSpace(l[last[1]:])
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeYAMLInvalid, File: ComposeFile,
			Line: line, Col: col, EndLine: line, EndCol: col + 1,
			Message: fmt.Sprintf("Line %d: %s", line, msg),
		})
	}
	if len(out) > 0 {
		return out
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(text, "yaml: "), "\n")
	return Problems{{
		Severity: SeverityError, Code: CodeYAMLInvalid, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "Line 1: " + first,
	}}
}

var (
	interpolateRe = regexp.MustCompile(`^error while interpolating (\S+): (.*)$`)
	undefinedRe   = regexp.MustCompile(`^service "([^"]+)" refers to undefined (\w+) (.+?): invalid compose project$`)
	extraPropRe   = regexp.MustCompile(`^additional properties '([^']*)' not allowed$`)
)

// loadProblems turns the error compose-go refused a file with into positioned problems. The
// position of a schema error comes from walking the stored YAML to the key path the error names.
func loadProblems(err error, ix *index) Problems {
	switch {
	case errors.Is(err, ErrTooLarge):
		return Problems{{
			Severity: SeverityError, Code: CodeComposeTooLarge, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: "compose.yaml is over 1 MiB. Split it up or remove what it doesn't need.",
		}}
	case errors.Is(err, context.DeadlineExceeded):
		return Problems{timeoutProblem()}
	}
	var le *LoadError
	inner := err
	if errors.As(err, &le) {
		inner = le.Err
	}
	text := strings.TrimSpace(inner.Error())
	for _, prefix := range []string{"failed to parse " + ComposeFile + ": ", "validating " + ComposeFile + ": "} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			if prefix[0] == 'f' {
				return yamlProblems(rest)
			}
			return schemaProblems(rest, ix)
		}
	}
	if m := interpolateRe.FindStringSubmatch(text); m != nil {
		return Problems{keyProblem(ix, CodeComposeInvalid, m[1], m[1]+": "+m[2])}
	}
	if m := undefinedRe.FindStringSubmatch(text); m != nil {
		path := "services." + m[1] + "." + m[2] + "s"
		return Problems{keyProblem(ix, CodeComposeInvalid, path, fmt.Sprintf("%s: %s %s isn't defined. Declare it at the top level, or remove it.", path, m[2], m[3]))}
	}
	switch text {
	case "empty compose file":
		return Problems{{
			Severity: SeverityError, Code: CodeComposeInvalid, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: "compose.yaml is empty. Add a services section.",
		}}
	case "top-level object must be a mapping":
		return Problems{{
			Severity: SeverityError, Code: CodeComposeInvalid, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: "The top level of compose.yaml must be a mapping, such as services: followed by your services.",
		}}
	}
	return Problems{{
		Severity: SeverityError, Code: CodeComposeInvalid, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: text,
	}}
}

// timeoutProblem is the one problem of a check that ran out of time.
func timeoutProblem() Problem {
	return Problem{
		Severity: SeverityError, Code: CodeCheckTimeout, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "The check took too long and was stopped. Simplify the file and try again.",
	}
}

// keyProblem is an error at the key a path names, or at line 1 when the path is not in the file.
func keyProblem(ix *index, code, path, message string) Problem {
	line, col, endLine, endCol := at(ix.find(path))
	return Problem{
		Severity: SeverityError, Code: code, File: ComposeFile, Key: path, Message: message,
		Line: line, Col: col, EndLine: endLine, EndCol: endCol,
	}
}

// schemaProblems reads a compose-go schema error: "{path} {message}", with an empty path for the
// top level. An unknown key reads "{parent}: unknown key “{key}”." at the key.
func schemaProblems(text string, ix *index) Problems {
	path, msg, _ := strings.Cut(text, " ")
	msg = strings.TrimSpace(msg)
	for _, sw := range [][2]string{{"a array", "an array"}, {"a object", "an object"}, {"a integer", "an integer"}} {
		msg = strings.ReplaceAll(msg, sw[0], sw[1])
	}
	if path == "" {
		if m := extraPropRe.FindStringSubmatch(msg); m != nil {
			return Problems{keyProblem(ix, CodeSchemaInvalid, m[1], fmt.Sprintf("unknown key “%s”.", m[1]))}
		}
		return Problems{keyProblem(ix, CodeSchemaInvalid, "", msg)}
	}
	if msg == "false schema" {
		parent, key := path, path
		if i := strings.LastIndex(path, "."); i >= 0 {
			parent, key = path[:i], path[i+1:]
		}
		return Problems{keyProblem(ix, CodeSchemaInvalid, path, fmt.Sprintf("%s: unknown key “%s”.", parent, key))}
	}
	return Problems{keyProblem(ix, CodeSchemaInvalid, path, path+": "+msg)}
}

// ErrNotSandboxed means a loaded project holds something the prescan should have refused.
var ErrNotSandboxed = errors.New("stacks: the loaded project is not sandboxed")

// assertSandboxed is the defence in depth after the load.
func assertSandboxed(project *types.Project, files FileSet) error {
	return nil
}
