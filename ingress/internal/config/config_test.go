// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
)

const (
	goodURL = "wss://tierplan.example:7480/ingress/connect"
	goodPin = "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	pinTwo  = "sha256/BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
	// secretToken is a test fixture, not a credential.
	secretToken = "tpit_secret-value-that-must-not-leak" //nolint:gosec // a test fixture
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func files(m map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if v, ok := m[name]; ok {
			return []byte(v), nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
}

func TestLoadURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string // substring; empty means success
	}{
		{"full wss url as given", goodURL, ""},
		{"uppercase scheme", "WSS://tierplan.example/ingress/connect", ""},
		{"empty", "", "TIERPLAN_INGRESS_URL must be set"},
		{"plain ws", "ws://tierplan.example/ingress/connect", "must start with wss://"},
		{"https", "https://tierplan.example/ingress/connect", "must start with wss://"},
		{"no host", "wss:///ingress/connect", "must be a wss:// address with a host"},
		{"userinfo", "wss://operator:hunter2@tierplan.example/ingress/connect", "no user name"},
		{"query", "wss://tierplan.example/ingress/connect?token=x", "no user name, query or fragment"},
		{"fragment", "wss://tierplan.example/ingress/connect#zq9frag", "no user name, query or fragment"},
		{"empty fragment", "wss://tierplan.example/ingress/connect#", "no user name, query or fragment"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{EnvURL: tc.url}))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if cfg.URL != tc.url {
					t.Fatalf("URL = %q, want it kept as given", cfg.URL)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.wantErr)
			}
			for _, secret := range []string{"hunter2", "token=x", "zq9frag"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q repeats part of the URL (%q)", err, secret)
				}
			}
		})
	}
}

func TestLoadPins(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvURL: goodURL, EnvPin: goodPin + " , " + pinTwo + ",,"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pins) != 2 || cfg.Pins[0] != goodPin || cfg.Pins[1] != pinTwo {
		t.Fatalf("Pins = %v, want the two pins", cfg.Pins)
	}

	cfg, err = Load(env(map[string]string{EnvURL: goodURL}))
	if err != nil || len(cfg.Pins) != 0 {
		t.Fatalf("no pin: Pins = %v, err = %v; want none", cfg.Pins, err)
	}

	for _, bad := range []string{"sha256/short", "md5/" + goodPin[7:], goodPin[7:], "sha256/" + strings.Repeat("!", 44)} {
		_, err := Load(env(map[string]string{EnvURL: goodURL, EnvPin: goodPin + "," + bad}))
		if err == nil || !strings.Contains(err.Error(), EnvPin) || !strings.Contains(err.Error(), bad) {
			t.Errorf("pin %q: error = %v, want one naming %s and quoting the entry", bad, err, EnvPin)
		}
	}
}

func TestLoadNodeID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		want    string
		wantErr bool
	}{
		{"unset", "", "", false},
		{"valid", "x2k9p0aqzcr6bbx1s3d4e5f6g", "x2k9p0aqzcr6bbx1s3d4e5f6g", false},
		{"trimmed", " node-1 ", "node-1", false},
		{"too long", strings.Repeat("a", 200), "", true},
		{"template", "{{.Node.ID}}", "", true},
		{"bad character", "node/1", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{EnvURL: goodURL, EnvNodeID: tc.id}))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), EnvNodeID) {
					t.Fatalf("error = %v, want one naming %s", err, EnvNodeID)
				}
				return
			}
			if err != nil || cfg.NodeID != tc.want {
				t.Fatalf("NodeID = %q, err = %v; want %q", cfg.NodeID, err, tc.want)
			}
		})
	}
}

func TestJoinToken(t *testing.T) {
	const path = "/run/secrets/ingress-token"
	tests := []struct {
		name    string
		env     map[string]string
		files   map[string]string
		want    string
		wantErr string
	}{
		{"from file, newline trimmed", map[string]string{EnvJoinTokenFile: path}, map[string]string{path: secretToken + "\n"}, secretToken, ""},
		{"from variable", map[string]string{EnvJoinToken: " " + secretToken + " "}, nil, secretToken, ""},
		{"file wins over variable", map[string]string{EnvJoinTokenFile: path, EnvJoinToken: "from-env"}, map[string]string{path: secretToken}, secretToken, ""},
		{"empty file", map[string]string{EnvJoinTokenFile: path, EnvJoinToken: "from-env"}, map[string]string{path: " \n"}, "", path + " is empty"},
		{"missing file", map[string]string{EnvJoinTokenFile: path}, nil, "", EnvJoinTokenFile},
		{"neither set", nil, nil, "", "set " + EnvJoinTokenFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := map[string]string{EnvURL: goodURL}
			for k, v := range tc.env {
				e[k] = v
			}
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatal(err)
			}
			got, err := cfg.JoinToken(files(tc.files))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("JoinToken error = %v, want it to contain %q", err, tc.wantErr)
				}
				if got != "" || strings.Contains(err.Error(), "from-env") || strings.Contains(err.Error(), secretToken) {
					t.Fatalf("JoinToken returned %q with error %q; want no token anywhere", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("JoinToken = %q, err = %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestLoadNeverReadsTheTokenFile(t *testing.T) {
	_, err := Load(env(map[string]string{EnvURL: goodURL, EnvJoinTokenFile: "/run/secrets/absent"})) //nolint:gosec // a test fixture
	if err != nil {
		t.Fatalf("Load with a token file named: %v; an enrolled node needs no token", err)
	}
	// Load has no readFile parameter at all, so it cannot read one; JoinToken is
	// where the file is opened, and only when it is called.
	cfg, _ := Load(env(map[string]string{EnvURL: goodURL, EnvJoinTokenFile: "/run/secrets/absent"})) //nolint:gosec // a test fixture
	called := false
	_, _ = cfg.JoinToken(func(string) ([]byte, error) {
		called = true
		return nil, errors.New("not here")
	})
	if !called {
		t.Fatal("JoinToken did not read the file it was told to")
	}
}

func TestConfigFormattingNeverPrintsTheToken(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvURL: goodURL, EnvJoinToken: secretToken, EnvJoinTokenFile: "/run/secrets/" + secretToken, EnvNodeID: "node-1", EnvPin: goodPin}))
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{
		"%v":     fmt.Sprintf("%v", cfg),
		"%+v":    fmt.Sprintf("%+v", cfg),
		"%#v":    fmt.Sprintf("%#v", cfg),
		"%s":     fmt.Sprintf("%s|", cfg),
		"ptr %v": fmt.Sprintf("%v", &cfg),
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "config", cfg)
	outputs["slog json"] = buf.String()
	for verb, out := range outputs {
		if strings.Contains(out, secretToken) || strings.Contains(out, "secret-value") {
			t.Errorf("%s printed the token: %s", verb, out)
		}
		if !strings.Contains(out, "tierplan.example:7480") {
			t.Errorf("%s did not show the host: %s", verb, out)
		}
	}
}

func TestValidPin(t *testing.T) {
	if !ValidPin(goodPin) {
		t.Error("a 32-byte SHA-256 pin was refused")
	}
	for _, bad := range []string{"", "sha256/", "sha256/AAAA", goodPin + "A", "SHA256/" + goodPin[7:]} {
		if ValidPin(bad) {
			t.Errorf("ValidPin(%q) = true", bad)
		}
	}
}
