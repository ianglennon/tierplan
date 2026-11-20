// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package supervisor

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/channel"
	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// pemForms returns every spelling of secret PEM text a log line could carry it in, each under its own name so a
// failure says which form leaked: the PEM, the PEM as a JSON string holds it, each base64 line, the whole
// base64 body, the file in base64 as a bundle frame carries it, and its DER in base64 and hex.
func pemForms(name string, p []byte) map[string]string {
	out := map[string]string{name + " PEM": string(p)}
	esc, _ := json.Marshal(string(p))
	out[name+" PEM as a JSON string"] = strings.Trim(string(esc), `"`)
	var body []string
	for _, l := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if l != "" && !strings.HasPrefix(l, "-----") {
			body = append(body, l)
			out[name+" base64 line "+l[:min(len(l), 12)]] = l
		}
	}
	out[name+" base64 body"] = strings.Join(body, "")
	// A bundle's files travel as base64 inside the frame, so a frame in a log holds the PEM in this form.
	out[name+" file as base64 (a bundle frame)"] = base64.StdEncoding.EncodeToString(p)
	out[name+" file as base64 (URL)"] = base64.RawURLEncoding.EncodeToString(p)
	out[name+" file as hex"] = hex.EncodeToString(p)
	if blk, _ := pem.Decode(p); blk != nil {
		out[name+" DER as base64"] = base64.StdEncoding.EncodeToString(blk.Bytes)
		out[name+" DER as hex"] = hex.EncodeToString(blk.Bytes)
	}
	return out
}

// leaks lists the names of the forbidden strings that appear in logged.
func leaks(logged string, forbidden map[string]string) []string {
	var found []string
	for name, s := range forbidden {
		if s != "" && strings.Contains(logged, s) {
			found = append(found, name)
		}
	}
	return found
}

func TestNoSecretsInLogs(t *testing.T) {
	const token = "tpit_e-1_SECRETSECRETSECRET" //nolint:gosec // G101: a fake token the test searches the logs for
	h := newHarness(t, "")
	cfg, err := config.Load(func(k string) string {
		switch k {
		case config.EnvURL:
			return "wss://tierplan.example.test/api/ingress"
		case config.EnvJoinToken:
			return token
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	h.deps.Config = cfg
	h.deps.ReadFile = func(name string) ([]byte, error) { return nil, os.ErrNotExist }
	// What the binary logs at start, so the configuration's own redaction is part of the check.
	h.deps.Log.Info("starting tierplan-ingress", "config", cfg)
	h.deps.Log.Info("config as a string", "config", cfg.String(), "gostring", cfg.GoString())

	// The fake Dial enrols with the token, reports the welcome, then fails: the loop would back off and retry.
	h.deps.Dial = func(_ context.Context, o channel.Options, cr channel.Credentials) (*channel.Conn, error) {
		got, err := cr.Token()
		if err != nil || got != token {
			t.Errorf("Token = (%q, %v), want the configured token", got, err)
		}
		if err := o.OnEnrolled(ingresswire.Welcome{NodeID: "n-1", EnvID: "e-1", InstanceID: testInstance}); err != nil {
			t.Errorf("OnEnrolled: %v", err)
		}
		return nil, errNoDial
	}

	good := validBundle(t, 1, "app.example.test")
	bad := refusedBundle(t, 2, "other.example.test")
	h.deps.RunChannel = func(ctx context.Context, lo channel.LoopOptions) error {
		h.events.add("channel")
		_, _ = lo.Dial(ctx)
		if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, good), testInstance); reason != "" {
			t.Errorf("the valid bundle was refused: %s", reason)
		}
		if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, bad), testInstance); reason == "" {
			t.Errorf("the refused bundle was applied")
		}
		_, _ = lo.Session.Health(ctx)
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.run(ctx)
	waitUntil(t, h.events, "channel")
	// RunChannel returns only after both Handle calls above, so every handler line is in the log by then.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	logged := h.logs.String()
	for _, want := range []string{`"msg":"bundle applied"`, `"msg":"bundle rejected"`, `"msg":"starting tierplan-ingress"`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the log is missing %s, so the check below would prove nothing:\n%s", want, logged)
		}
	}

	forbidden := map[string]string{"the join token": token, "the string PRIVATE KEY": "PRIVATE KEY"}

	// The node key in every form: its PEM, the DER in base64 and hex, the whole private key and its seed.
	keyPEM, err := os.ReadFile(filepath.Join(h.root, "state", "node.key"))
	if err != nil {
		t.Fatalf("read the node key: %v", err)
	}
	for k, v := range pemForms("node key", keyPEM) {
		forbidden[k] = v
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatalf("the node key is not PEM")
	}
	forbidden["node key DER base64"] = base64.StdEncoding.EncodeToString(block.Bytes)
	forbidden["node key DER base64 (URL)"] = base64.RawURLEncoding.EncodeToString(block.Bytes)
	forbidden["node key DER hex"] = hex.EncodeToString(block.Bytes)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse the node key: %v", err)
	}
	priv := parsed.(ed25519.PrivateKey) //nolint:errcheck // the key was just parsed from a PEM this test wrote with an Ed25519 key
	forbidden["node key seed base64"] = base64.StdEncoding.EncodeToString(priv.Seed())
	forbidden["node key seed hex"] = hex.EncodeToString(priv.Seed())
	forbidden["node key full base64"] = base64.StdEncoding.EncodeToString(priv)
	forbidden["node key full hex"] = hex.EncodeToString(priv)

	// Every certificate key of both bundles, the refused one's included.
	for _, b := range []ingressbundle.Bundle{good, bad} {
		for _, f := range b.Files {
			if strings.HasSuffix(f.Path, ".key") {
				for k, v := range pemForms("certificate key "+f.Path[len(f.Path)-12:], f.Data) {
					forbidden[k] = v
				}
			}
		}
	}

	for _, name := range leaks(logged, forbidden) {
		t.Errorf("the log holds %s", name)
	}

	// The check can see a leak: the same text with the token appended is caught.
	if got := leaks(logged+" "+token, forbidden); len(got) != 1 || got[0] != "the join token" {
		t.Fatalf("the detector found %v in a log with the token appended, want only the token", got)
	}
}
