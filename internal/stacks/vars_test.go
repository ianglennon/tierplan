// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

func refOf(t *testing.T, refs []stacks.VarRef, name string) stacks.VarRef {
	t.Helper()
	for _, r := range refs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no reference to %s in %+v", name, refs)
	return stacks.VarRef{}
}

func TestRefsOfTheStarterFindsTheWebPortWithItsDefaultAndLine(t *testing.T) {
	t.Parallel()
	refs := stacks.Refs(copyOf(stacks.StarterCompose))
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want exactly WEB_PORT: the example in the starter's comment is not a reference", refs)
	}
	r := refs[0]
	line := 1 + strings.Count(stacks.StarterCompose[:strings.Index(stacks.StarterCompose, `- "${WEB_PORT`)], "\n")
	if r.Name != "WEB_PORT" || !r.HasDefault || r.Default != "8080" || r.Required {
		t.Errorf("ref = %+v, want WEB_PORT with default 8080", r)
	}
	if len(r.Places) != 1 || r.Places[0].File != stacks.ComposeFile || r.Places[0].Line != line || r.Places[0].Plain {
		t.Errorf("places = %+v, want one at compose.yaml line %d with a default", r.Places, line)
	}
}

func TestRefsReadsEveryFormOfAReference(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: ${IMAGE}
    command: echo $PLAIN_DOLLAR
    environment:
      A: ${WITH_COLON_DEFAULT:-one}
      B: ${WITH_DEFAULT-two}
      C: ${NEEDED:?set NEEDED}
      D: ${EMPTY_DEFAULT:-}
      E: $${ESCAPED}
      F: "${NESTED:-${INNER}}"
    labels:
      note: |
        first line
        uses ${BLOCK_VAR}
`
	refs := stacks.Refs(copyOf(compose))
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	want := []string{"BLOCK_VAR", "EMPTY_DEFAULT", "IMAGE", "INNER", "NEEDED", "NESTED", "PLAIN_DOLLAR", "WITH_COLON_DEFAULT", "WITH_DEFAULT"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v (an escaped $${ESCAPED} is not a reference)", names, want)
	}
	if r := refOf(t, refs, "IMAGE"); r.HasDefault || r.Required || len(r.Places) != 1 || !r.Places[0].Plain || r.Places[0].Line != 3 {
		t.Errorf("IMAGE = %+v, want a plain reference on line 3", r)
	}
	if r := refOf(t, refs, "PLAIN_DOLLAR"); r.HasDefault || len(r.Places) != 1 || r.Places[0].Line != 4 {
		t.Errorf("PLAIN_DOLLAR = %+v, want a plain $NAME on line 4", r)
	}
	if r := refOf(t, refs, "WITH_COLON_DEFAULT"); !r.HasDefault || r.Default != "one" {
		t.Errorf("WITH_COLON_DEFAULT = %+v, want default one", r)
	}
	if r := refOf(t, refs, "WITH_DEFAULT"); !r.HasDefault || r.Default != "two" {
		t.Errorf("WITH_DEFAULT = %+v, want default two", r)
	}
	if r := refOf(t, refs, "EMPTY_DEFAULT"); !r.HasDefault || r.Default != "" {
		t.Errorf("EMPTY_DEFAULT = %+v, want an empty default (${NAME:-} is still a default)", r)
	}
	if r := refOf(t, refs, "NEEDED"); !r.Required || r.HasDefault || r.Places[0].Plain {
		t.Errorf("NEEDED = %+v, want a required reference", r)
	}
	if r := refOf(t, refs, "INNER"); r.HasDefault {
		t.Errorf("INNER = %+v, want a plain reference found inside the default of NESTED", r)
	}
	if r := refOf(t, refs, "BLOCK_VAR"); len(r.Places) != 1 || r.Places[0].Line != 15 {
		t.Errorf("BLOCK_VAR = %+v, want the line inside the block scalar (15)", r)
	}
}

func TestRefsCountsEachPlaceAVariableIsUsed(t *testing.T) {
	t.Parallel()
	const compose = `services:
  a:
    image: nginx:${TAG}
  b:
    image: busybox:${TAG}
  c:
    image: redis:${TAG:-7}
`
	r := refOf(t, stacks.Refs(copyOf(compose)), "TAG")
	if len(r.Places) != 3 || r.Places[0].Line != 3 || r.Places[1].Line != 5 || r.Places[2].Line != 7 {
		t.Fatalf("places = %+v, want lines 3, 5 and 7", r.Places)
	}
	if !r.Places[0].Plain || !r.Places[1].Plain || r.Places[2].Plain {
		t.Errorf("places = %+v, only the two without a default are plain", r.Places)
	}
	if !r.HasDefault || r.Default != "7" {
		t.Errorf("ref = %+v, want the default 7 of the third place", r)
	}
}

func TestRefsReadsOnlyTheComposeFileNotCommentsOrOtherFiles(t *testing.T) {
	t.Parallel()
	wc := stacks.WorkingCopy{Files: []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("# ${IN_A_COMMENT}\nservices:\n  web:\n    image: x:1 # ${AFTER_VALUE}\n")},
		{Path: "prometheus.yml", Content: []byte("global:\n  external_labels:\n    host: ${HOSTNAME}\n")},
	}}
	if refs := stacks.Refs(wc); len(refs) != 0 {
		t.Errorf("refs = %+v, want none: a comment is not a reference and only compose.yaml is interpolated", refs)
	}
	if refs := stacks.Refs(copyOf("services: [")); len(refs) != 0 {
		t.Errorf("refs of a file that is not YAML = %+v, want none", refs)
	}
	if refs := stacks.Refs(stacks.WorkingCopy{}); len(refs) != 0 {
		t.Errorf("refs of a working copy with no compose file = %+v, want none", refs)
	}
}

func TestResolveVarsFollowsTheTableThenTheDefault(t *testing.T) {
	t.Parallel()
	refs := stacks.Refs(copyOf(stacks.StarterCompose))
	got := stacks.ResolveVars(refs, nil)
	if len(got) != 1 {
		t.Fatalf("resolved = %+v", got)
	}
	if v := got[0]; v.Name != "WEB_PORT" || v.Value != "8080" || v.Source != stacks.SourceDefault || v.Required || v.Unused || !v.HasDefault || v.Default != "8080" {
		t.Errorf("with an empty table: %+v, want 8080 from the default", v)
	}
	got = stacks.ResolveVars(refs, []stacks.Var{{Name: "WEB_PORT", Value: "9090"}})
	if v := got[0]; v.Value != "9090" || v.Source != stacks.SourceTable || v.Default != "8080" {
		t.Errorf("with WEB_PORT=9090: %+v, want 9090 from the table", v)
	}
	got = stacks.ResolveVars(refs, []stacks.Var{{Name: "WEB_PORT", Value: ""}})
	if v := got[0]; v.Value != "8080" || v.Source != stacks.SourceDefault {
		t.Errorf("with a blank WEB_PORT: %+v, want the default: a row with no value has no value", v)
	}
}

func TestResolveVarsMarksWhatHasNoValueAndNoDefaultAsRequired(t *testing.T) {
	t.Parallel()
	refs := stacks.Refs(copyOf("services:\n  web:\n    image: ${IMAGE}\n"))
	v := stacks.ResolveVars(refs, nil)[0]
	if v.Source != stacks.SourceUnset || !v.Required || v.Value != "" {
		t.Errorf("IMAGE = %+v, want unset and required", v)
	}
	v = stacks.ResolveVars(refs, []stacks.Var{{Name: "IMAGE", Value: "nginx:1.27"}})[0]
	if v.Source != stacks.SourceTable || v.Required {
		t.Errorf("IMAGE with a value = %+v, want from the table and not required", v)
	}
}

func TestResolveVarsFillsTheVariablesADefaultRefersTo(t *testing.T) {
	t.Parallel()
	refs := stacks.Refs(copyOf("services:\n  web:\n    image: x:1\n    hostname: ${HOST:-${DOMAIN}}\n"))
	got := stacks.ResolveVars(refs, []stacks.Var{{Name: "DOMAIN", Value: "example.org"}})
	for _, v := range got {
		if v.Name == "HOST" && (v.Value != "example.org" || v.Source != stacks.SourceDefault) {
			t.Errorf("HOST = %+v, want example.org from the default", v)
		}
	}
}

func TestResolveVarsListsTheTableVariablesNoFileUses(t *testing.T) {
	t.Parallel()
	refs := stacks.Refs(copyOf(stacks.StarterCompose))
	got := stacks.ResolveVars(refs, []stacks.Var{{Name: "ZED", Value: "z"}, {Name: "ALPHA", Value: "a", Secret: true}})
	var names []string
	for _, v := range got {
		names = append(names, v.Name)
	}
	if !reflect.DeepEqual(names, []string{"ALPHA", "WEB_PORT", "ZED"}) {
		t.Fatalf("names = %v, want them sorted", names)
	}
	if a := got[0]; !a.Unused || a.Value != stacks.Hidden || !a.Secret {
		t.Errorf("ALPHA = %+v, want unused, secret and hidden", a)
	}
	if z := got[2]; !z.Unused || z.Value != "z" || z.Source != stacks.SourceTable {
		t.Errorf("ZED = %+v, want unused with its value", z)
	}
}

func TestResolveVarsHidesASecretValueThatIsUsed(t *testing.T) {
	t.Parallel()
	refs := stacks.Refs(copyOf("services:\n  db:\n    image: x:1\n    environment:\n      PASS: ${DB_PASS}\n"))
	v := stacks.ResolveVars(refs, []stacks.Var{{Name: "DB_PASS", Value: "s3cret-value", Secret: true}})[0]
	if v.Value != stacks.Hidden || !v.Secret || v.Source != stacks.SourceTable {
		t.Errorf("DB_PASS = %+v, want <hidden> from the table", v)
	}
}

func TestCheckVarsHoldsATableToItsRules(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		vars []stacks.Var
		want error
		on   string
	}{
		"a good table":            {[]stacks.Var{{Name: "A", Value: "x"}, {Name: "_B2", Value: "abcd", Secret: true}}, nil, ""},
		"a name with a dot":       {[]stacks.Var{{Name: "A.B", Value: "x"}}, stacks.ErrVarNameInvalid, "A.B"},
		"a name starting with 1":  {[]stacks.Var{{Name: "1A", Value: "x"}}, stacks.ErrVarNameInvalid, "1A"},
		"an empty name":           {[]stacks.Var{{Name: "", Value: "x"}}, stacks.ErrVarNameInvalid, ""},
		"a name with a dash":      {[]stacks.Var{{Name: "A-B", Value: "x"}}, stacks.ErrVarNameInvalid, "A-B"},
		"a name used twice":       {[]stacks.Var{{Name: "A", Value: "1"}, {Name: "A", Value: "2"}}, stacks.ErrVarNameTaken, "A"},
		"a short secret":          {[]stacks.Var{{Name: "PIN", Value: "123", Secret: true}}, stacks.ErrSecretTooShort, "PIN"},
		"an empty secret":         {[]stacks.Var{{Name: "PIN", Value: "", Secret: true}}, nil, ""},
		"a short plain value":     {[]stacks.Var{{Name: "PIN", Value: "1"}}, nil, ""},
		"a secret of four":        {[]stacks.Var{{Name: "PIN", Value: "1234", Secret: true}}, nil, ""},
		"one four-byte character": {[]stacks.Var{{Name: "E", Value: "😀", Secret: true}}, stacks.ErrSecretTooShort, "E"},
		"the first fault is told": {[]stacks.Var{{Name: "1A", Value: "x"}, {Name: "B", Value: "1", Secret: true}}, stacks.ErrVarNameInvalid, "1A"},
	} {
		err := stacks.CheckVars(tc.vars)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: CheckVars = %v, want nil", name, err)
			}
			continue
		}
		var ve *stacks.VarError
		if !errors.Is(err, tc.want) || !errors.As(err, &ve) || ve.Name != tc.on {
			t.Errorf("%s: CheckVars = %v, want %v on %q", name, err, tc.want, tc.on)
		}
	}
}

func TestRefsReadsABareEnvironmentEntryInBothForms(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: x:1
    environment:
      - FOO
      - BAR=literal
      - BAZ=${BAZ_VALUE:-z}
  api:
    image: x:1
    environment:
      QUX:
      QUUX: set
      CORGE: null
`
	refs := stacks.Refs(copyOf(compose))
	for name, line := range map[string]int{"FOO": 5, "QUX": 11, "CORGE": 13} {
		r := refOf(t, refs, name)
		if r.HasDefault || r.Required || len(r.Places) != 1 || r.Places[0].Line != line || !r.Places[0].Plain {
			t.Errorf("%s = %+v, want a plain reference on line %d", name, r, line)
		}
	}
	for _, name := range []string{"BAR", "QUUX"} {
		for _, r := range refs {
			if r.Name == name {
				t.Errorf("%s is given a value in the file, so it is not a reference: %+v", name, r)
			}
		}
	}
	if r := refOf(t, refs, "BAZ_VALUE"); !r.HasDefault {
		t.Errorf("BAZ_VALUE = %+v, want its default", r)
	}
}

func TestRefsDoesNotCountANestedDefaultAsPlain(t *testing.T) {
	t.Parallel()
	r := refOf(t, stacks.Refs(copyOf("services:\n  web:\n    image: x:1\n    hostname: ${HOST:-${DOMAIN}}\n")), "DOMAIN")
	if len(r.Places) != 1 || r.Places[0].Plain {
		t.Errorf("DOMAIN = %+v: it is read only when HOST is empty, so it is not an error where it stands", r)
	}
}
