// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package redact_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
)

// Environment variable values reach a browser only for someone who may reveal
// them (plan 03-22, D-15a). Negative cases are mandatory (D-81).

func TestEnvMasksEveryValueWhenTheCallerMayNotRevealAndNeverCarriesIt(t *testing.T) {
	got := redact.Env([]string{"A=1", "B=two=parts", "C"}, false)
	want := []redact.EnvVar{
		{Name: "A", Masked: true},
		{Name: "B", Masked: true},
		{Name: "C", Masked: true},
	}
	if len(got) != len(want) {
		t.Fatalf("Env = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Env[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// What would be sent to the browser holds the names and nothing of the values.
	raw, err := json.Marshal(redact.Env([]string{"DB_PASSWORD=hunter2", "TOKEN=a=b=c"}, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "a=b=c", "b=c"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the masked body %s carries %q", raw, leak)
		}
	}
}

func TestEnvKeepsTheValuesWhenTheCallerMayRevealAndSplitsOnTheFirstEqualsSignOnly(t *testing.T) {
	got := redact.Env([]string{"A=1", "B=two=parts", "C", "D="}, true)
	want := []redact.EnvVar{
		{Name: "A", Value: "1"},
		{Name: "B", Value: "two=parts"},
		{Name: "C"},
		{Name: "D"},
	}
	if len(got) != len(want) {
		t.Fatalf("Env = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Env[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestEnvOfNothingIsAnEmptyListNotNull(t *testing.T) {
	for _, reveal := range []bool{false, true} {
		raw, err := json.Marshal(redact.Env(nil, reveal))
		if err != nil || string(raw) != "[]" {
			t.Errorf("Env(nil, %v) marshals to %s (%v), want []", reveal, raw, err)
		}
	}
}

func TestOptionsMaskEveryValueUnlessTheCallerMayRevealAndSortByName(t *testing.T) {
	opts := map[string]string{"splunk-token": "hunter2", "tag": "web", "env": ""}
	got := redact.Options(opts, false)
	want := []redact.OptionVar{{Name: "env", Masked: true}, {Name: "splunk-token", Masked: true}, {Name: "tag", Masked: true}}
	if len(got) != len(want) {
		t.Fatalf("Options = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Options[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "web"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the masked body %s carries %q", raw, leak)
		}
	}

	shown := redact.Options(opts, true)
	if len(shown) != 3 || shown[1] != (redact.OptionVar{Name: "splunk-token", Value: "hunter2"}) || shown[2].Value != "web" {
		t.Errorf("revealed Options = %+v", shown)
	}
}

func TestOptionsOfNothingIsAnEmptyListNotNull(t *testing.T) {
	for _, reveal := range []bool{false, true} {
		raw, err := json.Marshal(redact.Options(nil, reveal))
		if err != nil || string(raw) != "[]" {
			t.Errorf("Options(nil, %v) marshals to %s (%v), want []", reveal, raw, err)
		}
	}
}

type revealFx struct {
	st *store.Store
	c  *rbac.Checker
}

func newRevealFx(t *testing.T) *revealFx {
	t.Helper()
	st := storetest.New(t)
	f := &revealFx{st: st, c: rbac.NewChecker(st)}
	for _, u := range []string{"admin", "operator", "extra", "viewer", "owner", "nobody"} {
		f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, u, u)
	}
	f.exec(t, `UPDATE users SET instance_admin = 1 WHERE id = 'owner'`)
	f.assign(t, "admin", "admin", "env-a")
	f.assign(t, "operator", "operator", "env-a")
	f.assign(t, "extra", "operator", "env-a")
	f.exec(t, `INSERT INTO assignment_extras (assignment_id, permission) VALUES ('ra-extra', 'secrets.reveal')`)
	f.assign(t, "viewer", "viewer", "env-a")
	return f
}

func (f *revealFx) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *revealFx) assign(t *testing.T, user, role, env string) {
	t.Helper()
	f.exec(t, `INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, 'user', ?, ?, ?, 1)`,
		"ra-"+user, user, role, env)
}

func user(id string) *rbac.Principal {
	return &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id}
}

func TestCanRevealIsTrueExactlyWhenSecretsRevealIsAllowedOnThatEnvironment(t *testing.T) {
	f := newRevealFx(t)
	ctx := context.Background()
	for _, tc := range []struct {
		who  string
		env  string
		want bool
	}{
		{"admin", "env-a", true},     // Admin has every permission of the environment
		{"extra", "env-a", true},     // an Operator granted the extra
		{"owner", "env-a", true},     // an instance admin, with no assignment at all
		{"owner", "env-other", true}, // ... on any environment
		{"operator", "env-a", false}, // a plain Operator
		{"viewer", "env-a", false},
		{"nobody", "env-a", false},
		{"admin", "env-other", false}, // the grant is for another environment
		{"extra", "env-other", false},
	} {
		if got := redact.CanReveal(ctx, f.c, user(tc.who), tc.env); got != tc.want {
			t.Errorf("CanReveal(%s, %s) = %v, want %v", tc.who, tc.env, got, tc.want)
		}
	}
}

func TestCanRevealIsFalseWithNoCheckerNoPrincipalAndForAnAutomationWithoutRights(t *testing.T) {
	f := newRevealFx(t)
	ctx := context.Background()
	if redact.CanReveal(ctx, nil, user("owner"), "env-a") {
		t.Error("CanReveal with no checker = true")
	}
	if redact.CanReveal(ctx, f.c, nil, "env-a") {
		t.Error("CanReveal with no principal = true")
	}
	// A listed system principal holds no permission, so it never reveals.
	if redact.CanReveal(ctx, f.c, &rbac.Principal{Type: rbac.PrincipalSystem, ID: "system:cli"}, "env-a") {
		t.Error("CanReveal for system:cli = true")
	}
	// Disabled users reveal nothing, whatever they were granted.
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'admin'`)
	if redact.CanReveal(ctx, f.c, user("admin"), "env-a") {
		t.Error("CanReveal for a disabled Admin = true")
	}
}

const inspectFixture = `{"Id":"abc","Config":{"Image":"nginx","Env":["DB_PASSWORD=hunter2","MODE=a=b","BARE"],"Labels":{"a":"b"}},"HostConfig":{"Memory":9007199254740993,"LogConfig":{"Type":"json-file","Config":{}}}}`

func TestMaskContainerInspectHidesEveryVariableValueAndKeepsTheRestOfTheDocument(t *testing.T) {
	out, masked, err := redact.MaskContainerInspect([]byte(inspectFixture), false)
	if err != nil {
		t.Fatalf("MaskContainerInspect: %v", err)
	}
	if !masked {
		t.Error("masked = false, want true")
	}
	for _, leak := range []string{"hunter2", "a=b\""} {
		if strings.Contains(string(out), leak) {
			t.Errorf("output carries %q: %s", leak, out)
		}
	}
	var got struct {
		ID     string `json:"Id"`
		Config struct {
			Image  string
			Env    []string
			Labels map[string]string
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	wantEnv := []string{"DB_PASSWORD=<hidden>", "MODE=<hidden>", "BARE"}
	if strings.Join(got.Config.Env, "|") != strings.Join(wantEnv, "|") {
		t.Errorf("Env = %v, want %v", got.Config.Env, wantEnv)
	}
	if got.ID != "abc" || got.Config.Image != "nginx" || got.Config.Labels["a"] != "b" {
		t.Errorf("the rest of the document changed: %+v", got)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Errorf("a large number lost its digits: %s", out)
	}
	if !strings.Contains(string(out), "\n  \"Config\"") {
		t.Errorf("not indented with two spaces: %s", out)
	}
}

func TestMaskContainerInspectWithRevealReturnsTheInputAndReportsNothingMasked(t *testing.T) {
	out, masked, err := redact.MaskContainerInspect([]byte(inspectFixture), true)
	if err != nil || masked || string(out) != inspectFixture {
		t.Fatalf("reveal = %q masked=%v err=%v", out, masked, err)
	}
}

func TestMaskContainerInspectOfADocumentWithNoVariablesIsNotMasked(t *testing.T) {
	out, masked, err := redact.MaskContainerInspect([]byte(`{"Id":"abc","Config":{"Env":[]}}`), false)
	if err != nil || masked || !strings.Contains(string(out), `"Id": "abc"`) {
		t.Fatalf("out=%q masked=%v err=%v", out, masked, err)
	}
}

func TestMaskContainerInspectHidesEveryLogDriverOptionValueAndKeepsTheNames(t *testing.T) {
	raw := `{"Id":"abc","Config":{"Env":[]},"HostConfig":{"LogConfig":{"Type":"splunk","Config":{"splunk-token":"s3cret-token","splunk-url":"https://splunk.example:8088"}}}}`
	out, masked, err := redact.MaskContainerInspect([]byte(raw), false)
	if err != nil {
		t.Fatalf("MaskContainerInspect: %v", err)
	}
	if !masked {
		t.Error("masked = false, want true")
	}
	for _, leak := range []string{"s3cret-token", "splunk.example"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("output carries %q: %s", leak, out)
		}
	}
	var got struct {
		HostConfig struct {
			LogConfig struct {
				Type   string
				Config map[string]string
			}
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	lc := got.HostConfig.LogConfig
	if lc.Type != "splunk" || len(lc.Config) != 2 || lc.Config["splunk-token"] != redact.Placeholder || lc.Config["splunk-url"] != redact.Placeholder {
		t.Errorf("log config = %+v", lc)
	}
	// With reveal the options are as the engine sent them.
	out, masked, err = redact.MaskContainerInspect([]byte(raw), true)
	if err != nil || masked || string(out) != raw {
		t.Errorf("reveal = %q masked=%v err=%v", out, masked, err)
	}
}

func TestMaskContainerInspectOfAnythingThatIsNotAJSONObjectIsAnErrorNeverAPassthrough(t *testing.T) {
	for _, raw := range []string{``, `{"Config":{"Env":["A=secret"]}`, `not json`, `null`, `[]`, `"A=secret"`, `{"a":1} {"Config":{"Env":["A=secret"]}}`} {
		out, masked, err := redact.MaskContainerInspect([]byte(raw), false)
		if err == nil || out != nil || masked {
			t.Errorf("MaskContainerInspect(%q) = %q, %v, %v; want an error and no output", raw, out, masked, err)
		}
	}
}

func TestMaskContainerInspectMasksAnEnvEntryThatIsNotAStringRatherThanKeepingIt(t *testing.T) {
	out, masked, err := redact.MaskContainerInspect([]byte(`{"Config":{"Env":[{"v":"secret"},"A=b"]}}`), false)
	if err != nil || !masked || strings.Contains(string(out), "secret") || strings.Contains(string(out), "=b") {
		t.Fatalf("out=%q masked=%v err=%v", out, masked, err)
	}
}

// A volume create is audited with its driver options, which can hold an SMB password
// (plan 04-16, T-04-55, RES-07). The row keeps the option names and values and masks
// any password-like part. Negative cases are mandatory (D-81).

func TestAuditOptionsMasksThePasswordSegmentOfOAndKeepsTheRest(t *testing.T) {
	got := redact.AuditOptions(map[string]string{
		"type": "cifs", "o": "addr=x,username=u,password=p", "device": "//x/s",
	})
	want := "device=//x/s; o=addr=x,username=u,password=<hidden>; type=cifs"
	if got != want {
		t.Fatalf("AuditOptions = %q, want %q", got, want)
	}
	if strings.Contains(got, "password=p") {
		t.Fatalf("the password is in the audit text: %q", got)
	}
}

func TestAuditOptionsReplacesTheWholeValueOfAKeyThatNamesASecret(t *testing.T) {
	for _, key := range []string{"password", "Password", "DB_PASS", "passwd", "secret", "api_secret", "credentials", "token", "AUTH_TOKEN"} {
		got := redact.AuditOptions(map[string]string{key: "hunter2", "type": "x"})
		if strings.Contains(got, "hunter2") {
			t.Errorf("key %q: the value is in the audit text: %q", key, got)
		}
		if !strings.Contains(got, key+"="+redact.Placeholder) || !strings.Contains(got, "type=x") {
			t.Errorf("key %q: AuditOptions = %q, want the key kept with the placeholder and other options kept", key, got)
		}
	}
}

// WR-06: a plugin volume driver takes arbitrary option names, so more than password-like
// names carry a secret.
func TestAuditOptionsMasksKeyAuthPrivateCertAndIdentityOptionsAndTheSegmentsOfO(t *testing.T) {
	for _, key := range []string{"key", "apikey", "api_key", "sshkey", "ssh-key", "AuthFile", "auth", "private", "private_key", "cert", "tls_cert", "identityfile", "IdentityFile"} {
		got := redact.AuditOptions(map[string]string{key: "s3cr3t-value", "type": "x"})
		if strings.Contains(got, "s3cr3t-value") {
			t.Errorf("key %q: the value is in the audit text: %q", key, got)
		}
		if !strings.Contains(got, key+"="+redact.Placeholder) || !strings.Contains(got, "type=x") {
			t.Errorf("key %q: AuditOptions = %q, want the key kept with the placeholder and other options kept", key, got)
		}
	}
	got := redact.AuditOptions(map[string]string{"o": "addr=h,sshkey=/root/k,authfile=/a,identity=/i,rw"})
	for _, leaked := range []string{"/root/k", "/a,", "/i"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q is in the audit text: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "addr=h") {
		t.Errorf("addr=h is missing: %q", got)
	}
}

func TestAuditOptionsMasksTheUserInfoOfAURLOrSharePathInAnyOption(t *testing.T) {
	for _, c := range []struct{ name, value, want, leak string }{
		{"device", "//bob:pw0rd@fileserver/share", "//<hidden>@fileserver/share", "pw0rd"},
		{"addr", "https://bob:pw0rd@host:8443/path", "https://<hidden>@host:8443/path", "pw0rd"},
		{"endpoint", "s3://AKIAEXAMPLE:wJalrSecret@bucket/prefix", "s3://<hidden>@bucket/prefix", "wJalrSecret"},
		{"o", "addr=h,device=//u:pw0rd@h/s,rw", "device=//<hidden>@h/s", "pw0rd"},
	} {
		got := redact.AuditOptions(map[string]string{c.name: c.value})
		if strings.Contains(got, c.leak) {
			t.Errorf("%s=%q: the credential is in the audit text: %q", c.name, c.value, got)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s=%q: AuditOptions = %q, want it to contain %q", c.name, c.value, got, c.want)
		}
	}
}

func TestAuditOptionsKeepsAShareWithNoUserInfoAndAHostWithAnAtSignOutsideIt(t *testing.T) {
	if got := redact.AuditOptions(map[string]string{"device": "//fileserver/share"}); got != "device=//fileserver/share" {
		t.Errorf("AuditOptions = %q", got)
	}
}

func TestAuditOptionsMasksEverySecretLikeSegmentNameInAnOptionList(t *testing.T) {
	got := redact.AuditOptions(map[string]string{"o": "addr=h,pass=1,credentials=/etc/smb,token=t,secret=s,vers=3.0"})
	for _, leaked := range []string{"pass=1", "/etc/smb", "token=t", "secret=s"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q is in the audit text: %q", leaked, got)
		}
	}
	for _, kept := range []string{"addr=h", "vers=3.0", "pass=<hidden>", "credentials=<hidden>"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q is missing from the audit text: %q", kept, got)
		}
	}
}

func TestAuditOptionsDoesNotLeakTheRestOfAPasswordThatHoldsACommaOrADoubledComma(t *testing.T) {
	for _, o := range []string{"addr=h,password=ab,cd,username=u", "addr=h,password=ab,,cd,username=u"} {
		got := redact.AuditOptions(map[string]string{"o": o})
		if strings.Contains(got, "cd") || strings.Contains(got, "ab") {
			t.Errorf("o=%q: part of the password is in the audit text: %q", o, got)
		}
		if !strings.Contains(got, "username=u") || !strings.Contains(got, "password=<hidden>") {
			t.Errorf("o=%q: AuditOptions = %q, want the next option and the placeholder", o, got)
		}
	}
}

func TestAuditOptionsKeepsAPlainBindVolumeAsItIsAndNothingIsEmpty(t *testing.T) {
	if got := redact.AuditOptions(map[string]string{"type": "none", "o": "bind", "device": "/srv/data"}); got != "device=/srv/data; o=bind; type=none" {
		t.Errorf("AuditOptions = %q", got)
	}
	if got := redact.AuditOptions(nil); got != "" {
		t.Errorf("AuditOptions(nil) = %q, want empty", got)
	}
}

func TestAuditOptionsIsCappedAtFiveHundredBytesWithoutSplittingACharacter(t *testing.T) {
	got := redact.AuditOptions(map[string]string{"device": strings.Repeat("é", 400)})
	if len(got) > 500 {
		t.Fatalf("len = %d, want at most 500", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("the cap split a character: %q", got)
	}
	if got := redact.AuditOptions(map[string]string{"o": strings.Repeat("a", 600) + ",password=hunter2"}); strings.Contains(got, "hunter2") {
		t.Fatalf("a cap must never reveal a password: %q", got)
	}
}

func TestAuditOptionsMasksALoneSecretSegmentWithNoCommaToo(t *testing.T) {
	got := redact.AuditOptions(map[string]string{"o": "password=hunter2", "type": "cifs"})
	if got != "o=password=<hidden>; type=cifs" {
		t.Fatalf("AuditOptions = %q", got)
	}
}

// A volume's Options can hold a remote share's password, so its inspect document goes to a
// browser with them withheld unless the caller holds secrets.reveal (plan 04-18, T-04-59).

const volumeInspectFixture = `{"CreatedAt":"2026-09-01T10:00:00Z","Driver":"local","Labels":{"app":"x"},"Mountpoint":"/var/lib/docker/volumes/share/_data","Name":"share","Options":{"device":"//10.0.0.9/data","o":"username=bob,password=hunter2","type":"cifs"},"Scope":"local","Big":12345678901234567890}`

func TestMaskVolumeInspectReplacesEveryOptionValueKeepsTheNamesAndTheRestOfTheDocument(t *testing.T) {
	out, masked, err := redact.MaskVolumeInspect([]byte(volumeInspectFixture), false)
	if err != nil {
		t.Fatalf("MaskVolumeInspect: %v", err)
	}
	if !masked {
		t.Error("masked = false, want true")
	}
	for _, leak := range []string{"hunter2", "10.0.0.9", "cifs", "bob"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("output carries %q: %s", leak, out)
		}
	}
	var got struct {
		Name    string
		Driver  string
		Labels  map[string]string
		Options map[string]string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Name != "share" || got.Driver != "local" || got.Labels["app"] != "x" {
		t.Errorf("the rest of the document changed: %+v", got)
	}
	if len(got.Options) != 3 || got.Options["device"] != redact.Placeholder || got.Options["o"] != redact.Placeholder || got.Options["type"] != redact.Placeholder {
		t.Errorf("Options = %v, want the three names with the placeholder", got.Options)
	}
	// A number keeps its exact digits.
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Errorf("a number lost digits: %s", out)
	}
	// Two-space indentation, like the container's.
	if !strings.Contains(string(out), "\n  \"Driver\": \"local\"") {
		t.Errorf("not indented by two spaces: %s", out)
	}
}

func TestMaskVolumeInspectWithRevealReturnsTheInputAndReportsNothingMasked(t *testing.T) {
	out, masked, err := redact.MaskVolumeInspect([]byte(volumeInspectFixture), true)
	if err != nil || masked || string(out) != volumeInspectFixture {
		t.Fatalf("reveal = %q masked=%v err=%v", out, masked, err)
	}
}

func TestMaskVolumeInspectOfADocumentWithNoOptionsIsNotMasked(t *testing.T) {
	for _, raw := range []string{`{"Name":"v"}`, `{"Name":"v","Options":null}`, `{"Name":"v","Options":{}}`} {
		out, masked, err := redact.MaskVolumeInspect([]byte(raw), false)
		if err != nil || masked || !strings.Contains(string(out), `"Name": "v"`) {
			t.Errorf("%s: out=%q masked=%v err=%v", raw, out, masked, err)
		}
	}
}

func TestMaskVolumeInspectWithholdsOptionsOfAShapeTheEngineDoesNotSendRatherThanPassingThemThrough(t *testing.T) {
	out, masked, err := redact.MaskVolumeInspect([]byte(`{"Name":"v","Options":["o=password=secret"]}`), false)
	if err != nil || !masked || strings.Contains(string(out), "secret") {
		t.Fatalf("out=%q masked=%v err=%v", out, masked, err)
	}
}

func TestMaskVolumeInspectOfAnythingThatIsNotAJSONObjectIsAnErrorNeverAPassthrough(t *testing.T) {
	for _, raw := range []string{``, `{"Options":{"o":"password=secret"}`, `not json`, `null`, `[]`, `"o=secret"`, `{"a":1} {"Options":{"o":"password=secret"}}`} {
		out, masked, err := redact.MaskVolumeInspect([]byte(raw), false)
		if err == nil || out != nil || masked {
			t.Errorf("MaskVolumeInspect(%q) = %q, %v, %v; want an error and no output", raw, out, masked, err)
		}
	}
}

// WR-04: a command line is where credentials are passed (--requirepass, -p<password>, a
// DSN), so it is withheld from a caller who may not reveal secrets, like an environment
// value.
const argvFixture = `{"Id":"abc","Path":"redis-server","Args":["--requirepass","hunter2"],` +
	`"State":{"Status":"running"},` +
	`"Config":{"Image":"redis","Cmd":["redis-server","--requirepass","hunter2"],"Entrypoint":["docker-entrypoint.sh","-p","s3cret"],` +
	`"Healthcheck":{"Test":["CMD-SHELL","mysqladmin ping -uroot -pHunter3"],"Interval":30000000000},"Env":[]}}`

func TestMaskContainerInspectWithholdsTheCommandLineEverywhereTheDocumentCarriesIt(t *testing.T) {
	out, masked, err := redact.MaskContainerInspect([]byte(argvFixture), false)
	if err != nil {
		t.Fatalf("MaskContainerInspect: %v", err)
	}
	if !masked {
		t.Error("masked = false, want true")
	}
	for _, leak := range []string{"hunter2", "s3cret", "Hunter3", "redis-server", "docker-entrypoint", "mysqladmin"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("output carries %q: %s", leak, out)
		}
	}
	var got struct {
		ID     string `json:"Id"`
		Path   string
		Args   []string
		Config struct {
			Image       string
			Cmd         []string
			Entrypoint  []string
			Healthcheck struct {
				Test     []string
				Interval json.Number
			}
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	hidden := []string{"<hidden>"}
	if got.Path != "<hidden>" || !slices.Equal(got.Args, hidden) || !slices.Equal(got.Config.Cmd, hidden) || !slices.Equal(got.Config.Entrypoint, hidden) {
		t.Errorf("Path/Args/Cmd/Entrypoint = %q %v %v %v, want the placeholder", got.Path, got.Args, got.Config.Cmd, got.Config.Entrypoint)
	}
	if want := []string{"CMD-SHELL", "<hidden>"}; !slices.Equal(got.Config.Healthcheck.Test, want) {
		t.Errorf("Healthcheck.Test = %v, want %v: the mode stays, the command goes", got.Config.Healthcheck.Test, want)
	}
	if got.ID != "abc" || got.Config.Image != "redis" || got.Config.Healthcheck.Interval != "30000000000" {
		t.Errorf("the rest of the document changed: %+v", got)
	}
}

func TestMaskContainerInspectWithRevealKeepsTheCommandLine(t *testing.T) {
	out, masked, err := redact.MaskContainerInspect([]byte(argvFixture), true)
	if err != nil || masked || string(out) != argvFixture {
		t.Fatalf("reveal = %q masked=%v err=%v", out, masked, err)
	}
}

func TestMaskContainerInspectLeavesAnEmptyOrAbsentCommandAndANoneHealthCheckUnmasked(t *testing.T) {
	doc := `{"Id":"abc","Path":"","Args":[],"Config":{"Cmd":null,"Entrypoint":[],"Healthcheck":{"Test":["NONE"]},"Env":[]}}`
	out, masked, err := redact.MaskContainerInspect([]byte(doc), false)
	if err != nil || masked {
		t.Fatalf("out=%q masked=%v err=%v; nothing was withheld", out, masked, err)
	}
	if !strings.Contains(string(out), `"NONE"`) || strings.Contains(string(out), "hidden") {
		t.Errorf("a NONE health check or an empty command was changed: %s", out)
	}
}

func TestArgvWithholdsANonEmptyCommandUnlessTheCallerMayReveal(t *testing.T) {
	cmd := []string{"redis-server", "--requirepass", "hunter2"}
	if got, masked := redact.Argv(cmd, false); !masked || !slices.Equal(got, []string{"<hidden>"}) {
		t.Errorf("Argv(cmd, false) = %v, %v; want the placeholder, masked", got, masked)
	}
	if got, masked := redact.Argv(cmd, true); masked || !slices.Equal(got, cmd) {
		t.Errorf("Argv(cmd, true) = %v, %v; want the command, not masked", got, masked)
	}
	if got, masked := redact.Argv([]string{}, false); masked || got == nil || len(got) != 0 {
		t.Errorf("Argv(empty, false) = %v, %v; want an empty list, not masked", got, masked)
	}
}
