// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// decodeExact decodes a JSON document keeping numbers exact, to compare two documents.
func decodeExact(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, s)
	}
	return m
}

// objAt is the JSON object at m[key].
func objAt(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", key, m[key])
	}
	return o
}

// The saved inspect keeps every field the engine sent, the ones the typed detail lacks included, and
// withholds only the values that may be secrets (plan 10-12, D-12, T-10-31).
func TestInspectRecordFullAndMasked(t *testing.T) {
	t.Parallel()
	raw := `{
	  "Name": "shop_default", "Id": "abc123", "Created": "2026-01-02T03:04:05.123456789Z", "Scope": "local", "Driver": "bridge",
	  "EnableIPv6": false, "Internal": false, "Attachable": true, "Ingress": false,
	  "ConfigFrom": {"Network": "cfg-net"}, "ConfigOnly": false,
	  "IPAM": {"Driver": "default", "Options": {"api-token": "ipam-secret", "plain": "v"}, "Config": [{"Subnet": "172.30.0.0/24", "Gateway": "172.30.0.1"}]},
	  "Containers": {}, "Peers": [{"Name": "n1", "IP": "10.0.0.1"}],
	  "Options": {"token": "opt-secret", "com.docker.network.driver.mtu": "1400"},
	  "Labels": {"password": "hunter2", "dsn": "DSN", "tier": "backend", "del": "a\u007fb", "<html>": "&"},
	  "FutureField": {"n": 12345678901234567890, "f": 1.50, "list": [1, "two", null, true]}
	}`
	// The made-up credential in a URL is put together here, as a URL with user-info is what the masking is for.
	dsn := "https://user:" + "pass@db.example/x"
	raw = strings.ReplaceAll(raw, "DSN", dsn)
	doc, masked, err := inspectRecord([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !masked {
		t.Error("masked = false, want true: secret-like values were withheld")
	}
	for _, secret := range []string{"hunter2", "opt-secret", "ipam-secret", "user:pass"} {
		if strings.Contains(doc, secret) {
			t.Errorf("the record holds %q: %s", secret, doc)
		}
	}
	if strings.ContainsFunc(doc, unicode.IsControl) {
		t.Errorf("the record holds a control character the audit writer would change: %q", doc)
	}
	if strings.HasSuffix(doc, "\n") || strings.Contains(doc, "\n") || strings.Contains(doc, "\\u003c") {
		t.Errorf("the record is not compact with HTML escaping off: %s", doc)
	}

	// The record parses as one JSON object equal to the raw document with only the secrets replaced.
	want := decodeExact(t, raw)
	labels := objAt(t, want, "Labels")
	labels["password"] = "<hidden>"
	labels["dsn"] = "https://<hidden>@db.example/x"
	objAt(t, want, "Options")["token"] = "<hidden>"
	objAt(t, objAt(t, want, "IPAM"), "Options")["api-token"] = "<hidden>"
	got := decodeExact(t, doc)
	if gl := objAt(t, got, "Labels"); gl["dsn"] == dsn || gl["dsn"] == "" {
		t.Errorf("dsn = %v", gl["dsn"])
	} else {
		labels["dsn"] = gl["dsn"] // the masking of a URL's user-info is redact's to word
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record = %s\nwant the raw document with only the secret values replaced: %v", doc, want)
	}
	// An unknown field, a field the typed detail lacks and numbers are exactly as sent.
	for _, frag := range []string{`"ConfigFrom":{"Network":"cfg-net"}`, `12345678901234567890`, `"Peers":[{"IP":"10.0.0.1","Name":"n1"}]`} {
		if !strings.Contains(doc, frag) {
			t.Errorf("the record lacks %s: %s", frag, doc)
		}
	}
	if gl := objAt(t, got, "Labels"); gl["del"] != "a\x7fb" || gl["<html>"] != "&" {
		t.Errorf("labels = %v, want DEL, < and & kept exactly", gl)
	}
}

func TestInspectRecordNothingToMask(t *testing.T) {
	t.Parallel()
	doc, masked, err := inspectRecord([]byte(`{"Name":"n","Labels":null,"Options":{},"IPAM":{"Options":null}}`))
	if err != nil || masked {
		t.Fatalf("doc %s masked %v err %v, want an unmasked record", doc, masked, err)
	}
	if doc != `{"IPAM":{"Options":null},"Labels":null,"Name":"n","Options":{}}` {
		t.Errorf("doc = %s", doc)
	}
}

// A shape the engine does not send for these fields is withheld whole, never passed through.
func TestInspectRecordWithholdsUnexpectedShapes(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"labels array":       `{"Labels":["a=b"]}`,
		"labels number":      `{"Labels":{"a":1}}`,
		"labels nested":      `{"Labels":{"a":{"b":"c"}}}`,
		"options string":     `{"Options":"token=x"}`,
		"options null value": `{"Options":{"a":null}}`,
		"ipam options array": `{"IPAM":{"Options":["x"]}}`,
		"ipam string":        `{"IPAM":"secret"}`,
	} {
		doc, masked, err := inspectRecord([]byte(raw))
		if err != nil || !masked || !strings.Contains(doc, `"<hidden>"`) {
			t.Errorf("%s: doc %s masked %v err %v, want the field withheld whole", name, doc, masked, err)
		}
		for _, leak := range []string{"a=b", "token=x", `"secret"`, `"c"`, `"x"`} {
			if strings.Contains(doc, leak) {
				t.Errorf("%s: the record holds %s: %s", name, leak, doc)
			}
		}
	}
}

func TestInspectRecordRefusesWhatIsNotOneObject(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{``, `null`, `[]`, `"x"`, `{"a":1} {"b":2}`, `{"a":`, `{"a":1}x`} {
		if doc, _, err := inspectRecord([]byte(raw)); err == nil {
			t.Errorf("%q: got %q, want an error", raw, doc)
		}
	}
}
