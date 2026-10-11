// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package redact keeps secret-bearing values from reaching a browser that may not
// see them (D-15a, AUTH-07). Masking happens on the server: an environment
// variable's value is never put in a response unless the caller holds
// secrets.reveal on that environment, so no amount of client-side hiding is
// relied on. Every endpoint that returns a container's or service's environment
// (the inspect views of Phase 4 and Phase 6) must send it through Env, with
// CanReveal deciding the second argument.
package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/internal/rbac"
)

// EnvVar is one environment variable as a browser receives it. Value is empty
// and omitted while Masked is true, so the masked form carries the name and
// nothing of the value, not even its length.
type EnvVar struct {
	Name   string `json:"name" doc:"The variable's name"`
	Value  string `json:"value,omitempty" doc:"The variable's value; absent while masked"`
	Masked bool   `json:"masked" doc:"True when the value was withheld because the caller may not reveal secrets on this environment"`
}

// Placeholder stands for a value withheld from a document that is sent as it is
// structured, such as the raw inspect of a container.
const Placeholder = "<hidden>"

// OptionVar is one option of a log driver as a browser receives it. The values of
// these options can hold credentials (a syslog address with a password, a Splunk
// token), so they are masked exactly as environment values are.
type OptionVar struct {
	Name   string `json:"name" doc:"The option's name"`
	Value  string `json:"value,omitempty" doc:"The option's value; absent while masked"`
	Masked bool   `json:"masked" doc:"True when the value was withheld because the caller may not reveal secrets on this environment"`
}

// Options turns a map of driver options into OptionVars sorted by name. With reveal
// false every value is withheld.
func Options(opts map[string]string, reveal bool) []OptionVar {
	out := make([]OptionVar, 0, len(opts))
	for _, name := range slices.Sorted(maps.Keys(opts)) {
		if reveal {
			out = append(out, OptionVar{Name: name, Value: opts[name]})
			continue
		}
		out = append(out, OptionVar{Name: name, Masked: true})
	}
	return out
}

// Env turns the "NAME=value" strings Docker reports into EnvVars, in the same
// order. The name is everything before the first "=", the value everything after
// it, so a value that holds "=" stays whole; an entry with no "=" is a name with
// an empty value. With reveal false every value is withheld.
func Env(env []string, reveal bool) []EnvVar {
	out := make([]EnvVar, 0, len(env))
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if reveal {
			out = append(out, EnvVar{Name: name, Value: value})
			continue
		}
		out = append(out, EnvVar{Name: name, Masked: true})
	}
	return out
}

// Argv withholds a command line from a caller who may not reveal secrets. A command,
// an entrypoint or a health check is routinely where a password or token is passed
// (--requirepass, -p<password>, --api-key=, a DSN), so it is treated like an
// environment value: with reveal false a non-empty argv becomes one Placeholder and
// masked is true; an empty one has nothing to withhold and stays as it is.
func Argv(argv []string, reveal bool) (out []string, masked bool) {
	if reveal || len(argv) == 0 {
		return argv, false
	}
	return []string{Placeholder}, true
}

// CanReveal says whether p may see secret values on envID: exactly when the
// Checker allows secrets.reveal there, which an Admin, an Operator granted the
// extra and an instance admin hold. A missing Checker or principal, and any
// failure to decide, is no.
func CanReveal(ctx context.Context, c *rbac.Checker, p *rbac.Principal, envID string) bool {
	if c == nil || p == nil {
		return false
	}
	return c.Check(ctx, p, rbac.Requirement{Perm: rbac.PermSecretsReveal, Scope: rbac.ScopeEnvironment}, envID).Allowed
}

// MaskContainerInspect prepares the raw inspect document of a container for a
// browser. With reveal true the document is returned as it is and masked is
// false. Otherwise the value of every Config.Env entry ("NAME=value" becomes
// "NAME=<hidden>") is replaced by Placeholder and the rest of the document is kept
// as it was, re-encoded with two-space indentation; every HostConfig.LogConfig.Config
// option value is replaced the same way. The command line is withheld too, wherever
// the document carries it: Config.Cmd, Config.Entrypoint, Path and Args become one
// Placeholder when they are not empty, and the command of Config.Healthcheck.Test is
// replaced while its mode (CMD, CMD-SHELL, NONE) stays. masked is true when anything was
// replaced. Numbers keep their exact digits. A document that is not a JSON object
// is an error, never passed through, so a malformed answer cannot carry a value
// past the mask.
func MaskContainerInspect(raw []byte, reveal bool) (out []byte, masked bool, err error) {
	if reveal {
		return raw, false, nil
	}
	doc, err := decodeObject(raw, "container inspect")
	if err != nil {
		return nil, false, err
	}
	if maskArgv(doc, "Args") {
		masked = true
	}
	if path, ok := doc["Path"].(string); ok && path != "" {
		doc["Path"] = Placeholder
		masked = true
	}
	if cfg, ok := doc["Config"].(map[string]any); ok {
		for _, key := range []string{"Cmd", "Entrypoint"} {
			if maskArgv(cfg, key) {
				masked = true
			}
		}
		if hc, ok := cfg["Healthcheck"].(map[string]any); ok {
			if test, ok := hc["Test"].([]any); ok && len(test) > 1 {
				// The first element is the mode, not a secret; the rest is the command.
				hc["Test"] = []any{test[0], Placeholder}
				masked = true
			}
		}
		if env, ok := cfg["Env"].([]any); ok {
			for i, e := range env {
				kv, isString := e.(string)
				if !isString {
					env[i] = Placeholder
					masked = true
					continue
				}
				if name, _, hasValue := strings.Cut(kv, "="); hasValue {
					env[i] = name + "=" + Placeholder
					masked = true
				}
			}
		}
	}
	// The log driver's options can hold credentials too (a Splunk token, a syslog
	// address with a password): every value goes, the option names stay.
	if host, ok := doc["HostConfig"].(map[string]any); ok {
		if lc, ok := host["LogConfig"].(map[string]any); ok {
			if opts, ok := lc["Config"].(map[string]any); ok {
				for name := range opts {
					opts[name] = Placeholder
					masked = true
				}
			}
		}
	}
	out, err = encodeObject(doc, "container inspect")
	return out, masked, err
}

// maskArgv replaces the array at m[key] by one Placeholder when it has any element.
// Anything else at that key (absent, null, empty) has nothing to withhold. It reports
// whether it replaced something.
func maskArgv(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case []any:
		if len(v) > 0 {
			m[key] = []any{Placeholder}
			return true
		}
	case string:
		// Not a shape the engine sends for an argv: withhold rather than pass it on.
		if v != "" {
			m[key] = []any{Placeholder}
			return true
		}
	}
	return false
}

// MaskVolumeInspect prepares the raw inspect document of a volume for a browser. With
// reveal true the document is returned as it is and masked is false. Otherwise the value of
// every entry of Options (the driver options, which for a remote share can hold its
// password) is replaced by Placeholder, the names are kept and the rest of the document is
// as it was, re-encoded with two-space indentation. masked is true when anything was
// replaced. A document that is not a JSON object is an error, never passed through.
func MaskVolumeInspect(raw []byte, reveal bool) (out []byte, masked bool, err error) {
	if reveal {
		return raw, false, nil
	}
	doc, err := decodeObject(raw, "volume inspect")
	if err != nil {
		return nil, false, err
	}
	switch opts := doc["Options"].(type) {
	case map[string]any:
		for name := range opts {
			opts[name] = Placeholder
			masked = true
		}
	case nil:
	default:
		// Options that are not a map of text are not a shape the engine sends: withhold
		// them whole rather than pass something unknown through.
		doc["Options"] = Placeholder
		masked = true
	}
	out, err = encodeObject(doc, "volume inspect")
	return out, masked, err
}

// decodeObject reads raw as one JSON object, keeping every number with its exact digits. Text
// after the object, or anything but an object, is an error.
func decodeObject(raw []byte, what string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", what, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%s is not a JSON object", what)
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("%s has data after the JSON object", what)
	}
	return doc, nil
}

// encodeObject writes doc with two-space indentation, without escaping HTML and without a
// trailing newline.
func encodeObject(doc map[string]any, what string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode %s: %w", what, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// auditOptionsMax caps the text AuditOptions returns, under the audit log's own limit
// on a detail value.
const auditOptionsMax = 500

// secretWords are the parts of an option name that mark its value as a secret. A volume
// driver may take any option name, so the list errs on the side of masking: a key of any
// kind (apikey, sshkey, key), an auth setting or file, a private key, a certificate and
// an identity file hide their value too, as the audit writer's own key filter already
// treats key as a secret word.
var secretWords = []string{"pass", "secret", "credential", "token", "key", "auth", "private", "cert", "identity"}

// userInfo matches the user[:password]@ part that follows // in a URL or a share path
// (//user:pw@host/share, https://user:pw@host), which holds a credential whatever the
// option is called.
var userInfo = regexp.MustCompile(`//[^/\s@]+@`)

// maskUserInfo replaces the user-info of every URL or share path in value.
func maskUserInfo(value string) string {
	return userInfo.ReplaceAllString(value, "//"+Placeholder+"@")
}

// secretName reports whether an option name says its value is a secret: a password
// (password, passwd, pass), a secret, a credentials file, a token, a key, an auth
// setting, a private key, a certificate or an identity file, in any case.
func secretName(name string) bool {
	lower := strings.ToLower(name)
	for _, w := range secretWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// AuditOptions renders the driver options of a volume for an audit row: the "name=value"
// pairs sorted by name and joined with "; ", so the row says what was asked for without
// holding a share's password (T-04-55). The user-info of any URL or share path in a value
// (//user:pw@host/share) is replaced by Placeholder. The value of an option whose name
// looks like a secret (see secretWords) is replaced by Placeholder, and so is that part of a comma-separated value such
// as the local driver's o: "addr=h,username=u,password=p" becomes
// "addr=h,username=u,password=<hidden>". The rest of a password that holds a comma (a
// doubled comma is how cifs spells one) would otherwise stay behind, so the pieces with
// no "=" that follow a secret are dropped with it; a plain flag after a password, such as
// rw, is lost from the row for the same reason. The text is cut at 500 bytes without
// splitting a character, after the masking, so a cut can never reveal a value.
func AuditOptions(opts map[string]string) string {
	if len(opts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(opts))
	for _, name := range slices.Sorted(maps.Keys(opts)) {
		value := opts[name]
		if secretName(name) {
			value = Placeholder
		} else {
			value = maskUserInfo(maskSegments(value))
		}
		parts = append(parts, name+"="+value)
	}
	out := strings.Join(parts, "; ")
	if len(out) <= auditOptionsMax {
		return out
	}
	cut := auditOptionsMax
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut]
}

// maskSegments masks the secret segments of a comma-separated option value.
func maskSegments(value string) string {
	segments := strings.Split(value, ",")
	out := make([]string, 0, len(segments))
	afterSecret := false
	for _, seg := range segments {
		name, _, hasValue := strings.Cut(seg, "=")
		switch {
		case hasValue && secretName(name):
			out = append(out, name+"="+Placeholder)
			afterSecret = true
		case !hasValue && afterSecret:
			// What is left of a password that held a comma.
		default:
			out = append(out, seg)
			afterSecret = false
		}
	}
	return strings.Join(out, ",")
}
