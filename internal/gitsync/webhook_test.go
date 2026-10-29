// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
)

// A webhook proves a request came from the git host, and only asks for a check (plan 09-22, GIT-05, D-15).

var hookSecret = []byte("0123456789abcdef0123456789abcdef-secret")

const hookBody = `{"ref":"refs/heads/other","after":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}`

// hmacHex is the HMAC-SHA256 of body under secret, in hex.
func hmacHex(secret []byte, body string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func header(kv ...string) http.Header {
	h := http.Header{}
	for pair := range slices.Chunk(kv, 2) {
		h.Add(pair[0], pair[1])
	}
	return h
}

func TestVerifyGitHub(t *testing.T) {
	t.Parallel()
	good := "sha256=" + hmacHex(hookSecret, hookBody)
	for name, tc := range map[string]struct {
		secret []byte
		h      http.Header
		body   string
		want   bool
	}{
		"a valid signature":            {hookSecret, header("X-Hub-Signature-256", good), hookBody, true},
		"the header name in any case":  {hookSecret, header("x-hub-signature-256", good), hookBody, true},
		"another secret":               {[]byte("another-secret-entirely"), header("X-Hub-Signature-256", good), hookBody, false},
		"a body changed in flight":     {hookSecret, header("X-Hub-Signature-256", good), hookBody + " ", false},
		"no header":                    {hookSecret, header(), hookBody, false},
		"an empty header":              {hookSecret, header("X-Hub-Signature-256", ""), hookBody, false},
		"only the prefix":              {hookSecret, header("X-Hub-Signature-256", "sha256="), hookBody, false},
		"no prefix":                    {hookSecret, header("X-Hub-Signature-256", hmacHex(hookSecret, hookBody)), hookBody, false},
		"the old sha1 prefix":          {hookSecret, header("X-Hub-Signature-256", "sha1="+hmacHex(hookSecret, hookBody)), hookBody, false},
		"malformed hex":                {hookSecret, header("X-Hub-Signature-256", "sha256=zz"+good[9:]), hookBody, false},
		"odd length hex":               {hookSecret, header("X-Hub-Signature-256", good[:len(good)-1]), hookBody, false},
		"a truncated digest":           {hookSecret, header("X-Hub-Signature-256", good[:20]), hookBody, false},
		"a gitea header is not github": {hookSecret, header("X-Gitea-Signature", hmacHex(hookSecret, hookBody)), hookBody, false},
		"an empty secret":              {nil, header("X-Hub-Signature-256", "sha256="+hmacHex(nil, hookBody)), hookBody, false},
		"an empty body, signed":        {hookSecret, header("X-Hub-Signature-256", "sha256="+hmacHex(hookSecret, "")), "", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := gitsync.VerifyDelivery(gitsync.FlavourGitHub, gitsync.PlacementHeader, tc.secret, tc.h, []byte(tc.body))
			if got != tc.want {
				t.Errorf("VerifyDelivery = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifyGitLab(t *testing.T) {
	t.Parallel()
	secret := string(hookSecret)
	for name, tc := range map[string]struct {
		secret []byte
		h      http.Header
		want   bool
	}{
		"the matching token":      {hookSecret, header("X-Gitlab-Token", secret), true},
		"another token":           {hookSecret, header("X-Gitlab-Token", secret+"x"), false},
		"a prefix of the token":   {hookSecret, header("X-Gitlab-Token", secret[:10]), false},
		"no header":               {hookSecret, header(), false},
		"an empty token":          {hookSecret, header("X-Gitlab-Token", ""), false},
		"an empty secret":         {nil, header("X-Gitlab-Token", ""), false},
		"the same token twice":    {hookSecret, header("X-Gitlab-Token", secret, "X-Gitlab-Token", secret), true},
		"one right, one wrong":    {hookSecret, header("X-Gitlab-Token", secret, "X-Gitlab-Token", "nope"), false},
		"a github signature only": {hookSecret, header("X-Hub-Signature-256", "sha256="+hmacHex(hookSecret, hookBody)), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := gitsync.VerifyDelivery(gitsync.FlavourGitLab, gitsync.PlacementHeader, tc.secret, tc.h, []byte(hookBody)); got != tc.want {
				t.Errorf("VerifyDelivery = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifyGitea(t *testing.T) {
	t.Parallel()
	good := hmacHex(hookSecret, hookBody)
	wrong := hmacHex([]byte("another-secret-entirely"), hookBody)
	// Each header alone verifies. X-Hub-Signature-256 carries the sha256= prefix; the others the bare digest.
	for name, h := range map[string]http.Header{
		"X-Hub-Signature-256": header("X-Hub-Signature-256", "sha256="+good),
		"X-Forgejo-Signature": header("X-Forgejo-Signature", good),
		"X-Gitea-Signature":   header("X-Gitea-Signature", good),
		"X-Gogs-Signature":    header("X-Gogs-Signature", good),
	} {
		t.Run("alone "+name, func(t *testing.T) {
			t.Parallel()
			if !gitsync.VerifyDelivery(gitsync.FlavourGitea, gitsync.PlacementHeader, hookSecret, h, []byte(hookBody)) {
				t.Errorf("a valid %s alone was refused", name)
			}
		})
	}
	for name, tc := range map[string]struct {
		secret []byte
		h      http.Header
		want   bool
	}{
		"two valid headers":                     {hookSecret, header("X-Gitea-Signature", good, "X-Gogs-Signature", good), true},
		"a valid header and a wrong second":     {hookSecret, header("X-Gitea-Signature", good, "X-Gogs-Signature", wrong), false},
		"a wrong header and a valid second":     {hookSecret, header("X-Forgejo-Signature", wrong, "X-Gitea-Signature", good), false},
		"a valid header and an empty second":    {hookSecret, header("X-Gitea-Signature", good, "X-Forgejo-Signature", ""), false},
		"a valid header and a malformed second": {hookSecret, header("X-Gitea-Signature", good, "X-Gogs-Signature", "not hex"), false},
		"the same header twice, one wrong":      {hookSecret, header("X-Gitea-Signature", good, "X-Gitea-Signature", wrong), false},
		"no signature header":                   {hookSecret, header(), false},
		"an empty digest alone":                 {hookSecret, header("X-Gitea-Signature", ""), false},
		"an empty prefixed digest alone":        {hookSecret, header("X-Hub-Signature-256", "sha256="), false},
		"a hub header without its prefix":       {hookSecret, header("X-Hub-Signature-256", good), false},
		"a bare header with the prefix":         {hookSecret, header("X-Gitea-Signature", "sha256="+good), true},
		"an empty secret":                       {nil, header("X-Gitea-Signature", hmacHex(nil, hookBody)), false},
		"a gitlab token only":                   {hookSecret, header("X-Gitlab-Token", string(hookSecret)), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := gitsync.VerifyDelivery(gitsync.FlavourGitea, gitsync.PlacementHeader, tc.secret, tc.h, []byte(hookBody)); got != tc.want {
				t.Errorf("VerifyDelivery = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifyGeneric(t *testing.T) {
	t.Parallel()
	secret := string(hookSecret)
	// With the secret in the address the token that routed the request is the proof: there is nothing else to check.
	if !gitsync.VerifyDelivery(gitsync.FlavourGeneric, gitsync.PlacementPath, nil, header(), []byte(hookBody)) {
		t.Error("a generic webhook with the secret in the address refused a request that reached it")
	}
	for name, tc := range map[string]struct {
		secret []byte
		h      http.Header
		want   bool
	}{
		"a bearer token":             {hookSecret, header("Authorization", "Bearer "+secret), true},
		"the scheme in any case":     {hookSecret, header("Authorization", "bearer "+secret), true},
		"another token":              {hookSecret, header("Authorization", "Bearer "+secret+"x"), false},
		"no header":                  {hookSecret, header(), false},
		"an empty bearer":            {hookSecret, header("Authorization", "Bearer "), false},
		"a bearer with no space":     {hookSecret, header("Authorization", "Bearer"), false},
		"another scheme":             {hookSecret, header("Authorization", "Basic "+secret), false},
		"the bare secret":            {hookSecret, header("Authorization", secret), false},
		"an empty secret":            {nil, header("Authorization", "Bearer "), false},
		"two headers, one wrong":     {hookSecret, header("Authorization", "Bearer "+secret, "Authorization", "Bearer nope"), false},
		"a signature is not a token": {hookSecret, header("X-Hub-Signature-256", "sha256="+hmacHex(hookSecret, hookBody)), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := gitsync.VerifyDelivery(gitsync.FlavourGeneric, gitsync.PlacementHeader, tc.secret, tc.h, []byte(hookBody)); got != tc.want {
				t.Errorf("VerifyDelivery = %v, want %v", got, tc.want)
			}
		})
	}
	// A flavour or a placement that does not exist never verifies.
	if gitsync.VerifyDelivery("bitbucket", gitsync.PlacementHeader, hookSecret, header("Authorization", "Bearer "+secret), nil) {
		t.Error("an unknown flavour verified")
	}
	if gitsync.VerifyDelivery(gitsync.FlavourGeneric, "cookie", hookSecret, header("Authorization", "Bearer "+secret), nil) {
		t.Error("an unknown placement verified")
	}
}

func TestWebhookSecrets(t *testing.T) {
	t.Parallel()
	token, secret := gitsync.NewWebhookSecrets()
	// 32 bytes in unpadded URL-safe base64 are 43 characters: 256 bits each.
	for name, v := range map[string]string{"token": token, "secret": secret} {
		if len(v) != 43 || strings.ContainsAny(v, "+/= ") {
			t.Errorf("%s = %q, want 43 URL-safe characters", name, v)
		}
	}
	if token == secret {
		t.Error("the token and the secret are the same value")
	}
	if t2, s2 := gitsync.NewWebhookSecrets(); t2 == token || s2 == secret {
		t.Error("two calls gave the same value")
	}
	hash := gitsync.TokenHash(token)
	if len(hash) != 64 || hash != gitsync.TokenHash(token) || hash == gitsync.TokenHash(secret) || strings.Contains(hash, token) {
		t.Errorf("TokenHash(%q) = %q, want a stable 64 character hex digest that is not the token", token, hash)
	}
	// SHA-256 of "abc": a fixed vector, so the stored value is the plain hash and not a keyed one.
	if got := gitsync.TokenHash("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("TokenHash(abc) = %s", got)
	}
}

// triggerFixture is a stack whose fetches are held, with a channel of the triggered checks that ended (true: another
// follows) and one of the callers that joined a check.
type triggerFixture struct {
	*checkFixture
	gate   *gatedClient
	ended  chan bool
	joined chan int
}

func newTriggerFixture(t *testing.T) *triggerFixture {
	t.Helper()
	tf := &triggerFixture{gate: &gatedClient{release: make(chan struct{})}, ended: make(chan bool, 16), joined: make(chan int, 16)}
	tf.checkFixture = newCheckFixtureDeps(t, func(c gitx.Client) gitx.Client { tf.gate.Client = c; return tf.gate }, func(d *gitsync.Deps) {
		d.Joined = func(_ string, waiting int) { tf.joined <- waiting }
		d.Triggered = func(_ string, _ gitsync.Outcome, _ error, more bool) { tf.ended <- more }
	}, nil)
	tf.deploy(t)
	return tf
}

func (tf *triggerFixture) fetches() int {
	tf.gate.mu.Lock()
	defer tf.gate.mu.Unlock()
	return tf.gate.fetches
}

// TestWebhookCoalesces: five deliveries while a check runs cause exactly one trailing check (D-15).
func TestWebhookCoalesces(t *testing.T) {
	t.Parallel()
	tf := newTriggerFixture(t)
	tf.svc.Trigger("env-1", tf.stackID, gitsync.TriggerWebhook)
	<-tf.joined // the first check has started and holds at its fetch
	for range 5 {
		tf.svc.Trigger("env-1", tf.stackID, gitsync.TriggerWebhook)
	}
	close(tf.gate.release)
	if more := <-tf.ended; !more {
		t.Fatal("the first check ended with nothing after it, though five deliveries came while it ran")
	}
	if more := <-tf.ended; more {
		t.Fatal("a third check follows: the five deliveries were not collapsed into one")
	}
	if n := tf.fetches(); n != 2 {
		t.Errorf("%d fetches for six deliveries, want 2 (the check and one trailing check)", n)
	}
	if s := tf.state(t); s.CheckedAt == nil {
		t.Errorf("state = %+v, want the checks recorded", s)
	}
	// Once idle, a delivery starts a check again.
	tf.svc.Trigger("env-1", tf.stackID, gitsync.TriggerWebhook)
	if more := <-tf.ended; more {
		t.Error("a delivery after the burst was followed by another check")
	}
	if n := tf.fetches(); n != 3 {
		t.Errorf("%d fetches after a later delivery, want 3", n)
	}
}

// TestWebhookTriggerWaitsForACheckThatRuns: a check that began before the push may not show it, so a delivery that
// finds one running waits for it and checks again.
func TestWebhookTriggerWaitsForACheckThatRuns(t *testing.T) {
	t.Parallel()
	tf := newTriggerFixture(t)
	manual := make(chan struct{})
	go func() {
		defer close(manual)
		if _, err := tf.svc.Check(context.Background(), "env-1", tf.stackID, gitsync.TriggerManual); err != nil {
			t.Errorf("manual check: %v", err)
		}
	}()
	<-tf.joined // the manual check runs and holds at its fetch
	tf.svc.Trigger("env-1", tf.stackID, gitsync.TriggerWebhook)
	<-tf.joined // the triggered run has joined it
	close(tf.gate.release)
	if more := <-tf.ended; more {
		t.Error("one delivery gave a trailing check")
	}
	<-manual
	if n := tf.fetches(); n != 2 {
		t.Errorf("%d fetches, want 2: the running check and a fresh one for the delivery", n)
	}
}
