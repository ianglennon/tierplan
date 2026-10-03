// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

var fixedNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type rig struct {
	st  *store.Store
	vlt *vault.Vault
	svc *agents.Service
	ctx context.Context
	// clock is the service's clock; a test moves it to expire a certificate.
	clock time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st := storetest.New(t)
	vlt, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, vlt: vlt, ctx: context.Background(), clock: fixedNow}
	now := func() time.Time { return r.clock }
	r.svc = agents.NewService(st, vlt, audit.NewWriter(st, vlt, slog.New(slog.DiscardHandler), now), now)
	return r
}

func (r *rig) create(t *testing.T, name string) agents.Created {
	t.Helper()
	c, err := r.svc.CreateEnvironment(r.ctx, audit.SystemCLI, name, "203.0.113.9", "test")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	return c
}

func count(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// proof is what an agent sends: a fresh key and a signature over a transcript.
type proof struct {
	pub        ed25519.PublicKey
	transcript []byte
	sig        []byte
}

func newProof(t *testing.T, aud string) proof {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sn, an := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(sn)
	_, _ = rand.Read(an)
	tr := authn.Transcript(wire.ModeEnrol, wire.ProtoVersion, sn, an, aud, pub, nil)
	return proof{pub: pub, transcript: tr, sig: authn.Sign(priv, tr)}
}

func (p proof) request(token string) tunnel.EnrolRequest {
	return tunnel.EnrolRequest{
		Token: token, Pub: p.pub, Sig: p.sig, Transcript: p.transcript, AgentVersion: "0.2.0", IP: "198.51.100.7",
		Facts: wire.Facts{EngineID: "ENG:A", Hostname: "node-1", EngineReachable: true, EngineVersion: "29.1.0"},
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestCreateEnvironmentStoresTheEnvironmentItsCAAndAHashedToken(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")

	envID, secret, ok := agents.ParseToken(c.JoinToken)
	if !ok || envID != c.EnvID {
		t.Fatalf("the returned token %q does not parse as bpjt_<envId>_<secret> for %s", c.JoinToken, c.EnvID)
	}
	if !strings.HasPrefix(c.JoinToken, "bpjt_"+c.EnvID+"_") {
		t.Fatalf("token %q has the wrong shape", c.JoinToken)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(raw) != 32 {
		t.Fatalf("the secret part is not 32 random bytes of base64url: %v", err)
	}

	if n := count(t, r.st, `SELECT COUNT(*) FROM environments WHERE id = ? AND kind = 'agent' AND name = 'Production'`, c.EnvID); n != 1 {
		t.Fatalf("environments rows = %d, want 1", n)
	}
	var sealed string
	if err := r.st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = ?`, c.EnvID).Scan(&sealed); err != nil {
		t.Fatalf("no env_cas row: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1.") || strings.Count(sealed, ".") < 2 {
		t.Fatalf("key_sealed %q is not a vault envelope v1.<keyID>.…", sealed)
	}
	var gen int
	var hash []byte
	if err := r.st.Read.QueryRow(`SELECT generation, token_hash FROM env_join_tokens WHERE env_id = ?`, c.EnvID).Scan(&gen, &hash); err != nil {
		t.Fatalf("no env_join_tokens row: %v", err)
	}
	if gen != 1 || len(hash) != 32 {
		t.Fatalf("generation %d, hash length %d; want 1 and 32", gen, len(hash))
	}

	rows, _, err := audit.Query(r.ctx, r.st, audit.Filter{Actions: []string{audit.ActionEnvironmentAdded}}, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].EnvID != c.EnvID || rows[0].Details["kind"] != "agent" {
		t.Fatalf("environment.added audit = %+v, %v", rows, err)
	}
}

func TestCreateEnvironmentRejectsAnOverlongNameAndWritesNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.svc.CreateEnvironment(r.ctx, audit.SystemCLI, strings.Repeat("x", 65), "", "")
	if !errors.Is(err, agents.ErrInvalidName) {
		t.Fatalf("err = %v, want ErrInvalidName", err)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM environments`); n != 0 {
		t.Fatalf("%d environments written", n)
	}
}

func TestEnrolIssuesACertificateFromTheEnvironmentsCA(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "")
	p := newProof(t, "bayplan.example")

	res, err := r.svc.Enrol(r.ctx, p.request(c.JoinToken))
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if res.EnvID != c.EnvID || res.AgentID == "" {
		t.Fatalf("result %+v", res)
	}

	var caPEM string
	if err := r.st.Read.QueryRow(`SELECT cert_pem FROM env_cas WHERE env_id = ?`, c.EnvID).Scan(&caPEM); err != nil {
		t.Fatal(err)
	}
	leaf, err := agents.VerifyLeaf(caPEM, res.Cert, fixedNow)
	if err != nil {
		t.Fatalf("the certificate does not chain to the environment's CA: %v", err)
	}
	if leaf.Subject.CommonName != res.AgentID {
		t.Errorf("CN = %q, want the agent ID %q", leaf.Subject.CommonName, res.AgentID)
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != c.EnvID {
		t.Errorf("OU = %v, want [%s]", leaf.Subject.OrganizationalUnit, c.EnvID)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want client auth only", leaf.ExtKeyUsage)
	}
	if want := fixedNow.Add(-5 * time.Minute); !leaf.NotBefore.Equal(want) {
		t.Errorf("NotBefore = %v, want %v", leaf.NotBefore, want)
	}
	if want := fixedNow.Add(30 * 24 * time.Hour); !leaf.NotAfter.Equal(want) || !res.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %v (result %v), want %v", leaf.NotAfter, res.NotAfter, want)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != "bayplan://agent/"+res.AgentID {
		t.Errorf("URI SANs = %v", leaf.URIs)
	}
	if pub, ok := leaf.PublicKey.(ed25519.PublicKey); !ok || !bytes.Equal(pub, p.pub) {
		t.Error("the certificate does not carry the key the agent presented")
	}
	if leaf.IsCA {
		t.Error("an agent certificate must not be a CA")
	}
	if _, err := agents.VerifyLeaf(caPEM, res.Cert, fixedNow.Add(31*24*time.Hour)); err == nil {
		t.Error("the certificate still verifies 31 days later")
	}
	if _, err := agents.VerifyLeaf(caPEM, res.Cert, fixedNow.Add(-10*time.Minute)); err == nil {
		t.Error("the certificate verifies 10 minutes before it was issued")
	}
	if _, err := agents.VerifyLeaf(caPEM, res.Cert, fixedNow.Add(-3*time.Minute)); err != nil {
		t.Errorf("the certificate should verify 3 minutes early, inside the skew allowance: %v", err)
	}

	// The agent row and its audit entry.
	var status, name, role, fp string
	if err := r.st.Read.QueryRow(`SELECT status, name, role, cert_fingerprint FROM agents WHERE id = ?`, res.AgentID).Scan(&status, &name, &role, &fp); err != nil {
		t.Fatalf("no agent row: %v", err)
	}
	if status != "active" || name != "node-1" || role != "standalone" || len(fp) != 64 {
		t.Errorf("agent row = %s %s %s %s", status, name, role, fp)
	}
	if want := hex.EncodeToString(sha256Sum(res.Cert)); fp != want {
		t.Errorf("fingerprint %s, want %s", fp, want)
	}
	rows, _, err := audit.Query(r.ctx, r.st, audit.Filter{Actions: []string{audit.ActionAgentEnrolled}}, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("agent.enrolled audit = %+v, %v", rows, err)
	}
	a := rows[0]
	if a.ActorName != "system:agent-enrol" || a.ActorType != "system" || a.IP != "198.51.100.7" || a.TargetID != res.AgentID || a.EnvID != c.EnvID {
		t.Errorf("audit entry = %+v", a)
	}
	if a.Details["fingerprint"] != fp || a.Details["node"] != "node-1" || a.Details["serial"] == "" {
		t.Errorf("audit details = %v", a.Details)
	}

	// D-23: a blank environment name becomes the first agent's hostname.
	var envName string
	if err := r.st.Read.QueryRow(`SELECT name FROM environments WHERE id = ?`, c.EnvID).Scan(&envName); err != nil || envName != "node-1" {
		t.Errorf("environment name = %q, %v; want node-1", envName, err)
	}
}

func TestEnrolNeverRenamesAnEnvironmentTheAdminNamed(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	if _, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(c.JoinToken)); err != nil {
		t.Fatal(err)
	}
	var envName string
	if err := r.st.Read.QueryRow(`SELECT name FROM environments WHERE id = ?`, c.EnvID).Scan(&envName); err != nil || envName != "Production" {
		t.Fatalf("environment name = %q, %v; want Production", envName, err)
	}
}

func TestEnrolRefusesAWrongTokenAndWritesNoAgent(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	p := newProof(t, "h")
	other := r.create(t, "Staging")
	_, secret, _ := agents.ParseToken(c.JoinToken)
	_, otherSecret, _ := agents.ParseToken(other.JoinToken)

	for name, token := range map[string]string{
		"a wrong secret":                     "bpjt_" + c.EnvID + "_" + otherSecret,
		"a secret of zeros":                  "bpjt_" + c.EnvID + "_" + strings.Repeat("A", 43),
		"an environment that does not exist": "bpjt_0197b8a0-0000-7000-8000-000000000000_" + secret,
		"no prefix":                          c.EnvID + "_" + secret,
		"empty":                              "",
		"truncated":                          c.JoinToken[:len(c.JoinToken)-5],
		"extra bytes":                        c.JoinToken + "AAAA",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.svc.Enrol(r.ctx, p.request(token))
			var re *tunnel.RefuseError
			if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected {
				t.Fatalf("err = %v, want an auth_rejected refusal", err)
			}
			if strings.Contains(re.Detail, secret) || strings.Contains(re.Detail, c.EnvID) {
				t.Errorf("the refusal detail leaks the token or the environment: %q", re.Detail)
			}
		})
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents`); n != 0 {
		t.Fatalf("%d agent rows written by refused enrolments", n)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM audit_log WHERE action = 'agent.enrolled'`); n != 0 {
		t.Fatalf("%d agent.enrolled entries written by refused enrolments", n)
	}
}

func TestEnrolRefusesADisabledTokenAsAuthRejectedAndKeepsTheReasonInTheAudit(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	if _, err := r.st.Write.Exec(`UPDATE env_join_tokens SET disabled_at = ? WHERE env_id = ?`, fixedNow.UnixNano(), c.EnvID); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(c.JoinToken))
	var re *tunnel.RefuseError
	if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected || re.AuditReason != wire.RefuseTokenDisabled {
		t.Fatalf("err = %v, want auth_rejected with the audit reason %s", err, wire.RefuseTokenDisabled)
	}
}

func TestEnrolRefusesAnInvalidProofOfPossession(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")

	good := newProof(t, "h")
	// A signature made over a different transcript, and one made by another key.
	otherTranscript := newProof(t, "other-host")
	stolen := good
	stolen.sig = otherTranscript.sig
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey := good
	wrongKey.sig = authn.Sign(otherPriv, good.transcript)
	short := good
	short.pub = good.pub[:10]

	for name, p := range map[string]proof{"a signature over another transcript": stolen, "a signature by another key": wrongKey, "a short key": short} {
		t.Run(name, func(t *testing.T) {
			_, err := r.svc.Enrol(r.ctx, p.request(c.JoinToken))
			var re *tunnel.RefuseError
			if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected {
				t.Fatalf("err = %v, want an auth_rejected refusal", err)
			}
		})
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents`); n != 0 {
		t.Fatalf("%d agent rows written", n)
	}
}

func TestACertificateOfOneEnvironmentDoesNotVerifyAgainstAnothers(t *testing.T) {
	r := newRig(t)
	a, b := r.create(t, "A"), r.create(t, "B")
	res, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(a.JoinToken))
	if err != nil {
		t.Fatal(err)
	}
	pem := func(env string) string {
		var s string
		if err := r.st.Read.QueryRow(`SELECT cert_pem FROM env_cas WHERE env_id = ?`, env).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := agents.VerifyLeaf(pem(a.EnvID), res.Cert, fixedNow); err != nil {
		t.Fatalf("the certificate does not verify against its own CA: %v", err)
	}
	if _, err := agents.VerifyLeaf(pem(b.EnvID), res.Cert, fixedNow); err == nil {
		t.Fatal("a certificate of environment A verified against environment B's CA (D-07)")
	}
	if pem(a.EnvID) == pem(b.EnvID) {
		t.Fatal("two environments share a CA certificate")
	}
}

func TestACASealedKeyOpensOnlyForItsOwnEnvironment(t *testing.T) {
	r := newRig(t)
	a, b := r.create(t, "A"), r.create(t, "B")
	var sealedA string
	if err := r.st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = ?`, a.EnvID).Scan(&sealedA); err != nil {
		t.Fatal(err)
	}
	if _, err := r.vlt.Open("agent-ca/v1/"+a.EnvID, sealedA); err != nil {
		t.Fatalf("the key does not open in its own environment: %v", err)
	}
	if _, err := r.vlt.Open("agent-ca/v1/"+b.EnvID, sealedA); !errors.Is(err, vault.ErrDecrypt) {
		t.Fatalf("a sealed key opened under another environment's context: %v", err)
	}
	// Moved into B's row, it can no longer issue for B.
	if _, err := r.st.Write.Exec(`UPDATE env_cas SET key_sealed = ? WHERE env_id = ?`, sealedA, b.EnvID); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(b.JoinToken))
	if err == nil {
		t.Fatal("enrolled into B with A's CA key")
	}
	var re *tunnel.RefuseError
	if errors.As(err, &re) {
		t.Fatalf("a tampered row is a server fault, not a refusal of the agent: %v", err)
	}
}

func TestNeitherTheTokenNorTheCAKeyIsInTheDatabaseInPlaintext(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	if _, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(c.JoinToken)); err != nil {
		t.Fatal(err)
	}
	_, secret, _ := agents.ParseToken(c.JoinToken)
	var sealed string
	if err := r.st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = ?`, c.EnvID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	keyDER, err := r.vlt.Open("agent-ca/v1/"+c.EnvID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	needles := map[string][]byte{
		"the join token":         []byte(c.JoinToken),
		"its secret part":        []byte(secret),
		"the CA key (DER)":       keyDER,
		"the CA key (base64)":    []byte(base64.StdEncoding.EncodeToString(keyDER)),
		"the CA key (base64url)": []byte(base64.RawURLEncoding.EncodeToString(keyDER)),
		"the CA key (hex)":       []byte(hex.EncodeToString(keyDER)),
		"a PEM private key":      []byte("PRIVATE KEY"),
	}
	// Read the raw bytes with no checkpoint, so a page still in the WAL counts.
	for _, f := range []string{r.st.Path, r.st.Path + "-wal"} {
		b, err := os.ReadFile(f) //nolint:gosec // the test's own database file
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for what, needle := range needles {
			if bytes.Contains(b, needle) {
				t.Errorf("%s is in %s in plaintext", what, f)
			}
		}
	}
}

func TestParseToken(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "x")
	for token, want := range map[string]bool{
		c.JoinToken:      true,
		"":               false,
		"bpjt_":          false,
		"bpjt_nope_AAAA": false,
		"xpjt_" + strings.TrimPrefix(c.JoinToken, "bpjt_"): false,
		"bpjt_" + c.EnvID:                                 false,
		"bpjt_" + c.EnvID + "_":                           false,
		"bpjt_" + c.EnvID + "_" + strings.Repeat("A", 42): false,
		"bpjt_" + c.EnvID + "_" + strings.Repeat("A", 44): false,
		"bpjt_" + c.EnvID + "_" + strings.Repeat("!", 43): false,
		"bpjt_" + c.EnvID + "_" + strings.Repeat("A", 43): true,
		"bpjt_" + c.EnvID + "_" + strings.Repeat("_", 43): true, // the secret alphabet includes _ and -
	} {
		if _, _, ok := agents.ParseToken(token); ok != want {
			t.Errorf("ParseToken(%q) ok = %v, want %v", token, ok, want)
		}
	}
}

func TestAgentURLFollowsThePublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://bayplan.example":           "wss://bayplan.example/agent/connect",
		"https://bayplan.example/":          "wss://bayplan.example/agent/connect",
		"https://example.com/bayplan":       "wss://example.com/bayplan/agent/connect",
		"https://example.com/bayplan/":      "wss://example.com/bayplan/agent/connect",
		"http://127.0.0.1:7480":             "ws://127.0.0.1:7480/agent/connect",
		"https://bayplan.example:8443/a/b":  "wss://bayplan.example:8443/a/b/agent/connect",
		"https://u:p@bayplan.example/x?q=1": "wss://bayplan.example/x/agent/connect",
	} {
		got, err := agents.AgentURL(in)
		if err != nil || got != want {
			t.Errorf("AgentURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "not a url", "https://"} {
		if got, err := agents.AgentURL(bad); err == nil {
			t.Errorf("AgentURL(%q) = %q, want an error", bad, got)
		}
	}
}

// member is an enrolled agent: its key and the certificate it was issued.
type member struct {
	id, env string
	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	cert    []byte
}

func (r *rig) enrolMember(t *testing.T, c agents.Created) member {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr := authn.Transcript(wire.ModeEnrol, wire.ProtoVersion, make([]byte, 32), make([]byte, 32), "h", pub, nil)
	res, err := r.svc.Enrol(r.ctx, tunnel.EnrolRequest{
		Token: c.JoinToken, Pub: pub, Sig: authn.Sign(priv, tr), Transcript: tr, AgentVersion: "0.2.0", IP: "198.51.100.7",
		Facts: wire.Facts{EngineID: "ENG:" + c.EnvID, Hostname: "node-1", EngineReachable: true},
	})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	return member{id: res.AgentID, env: res.EnvID, priv: priv, pub: pub, cert: res.Cert}
}

// hello builds the request the handler would: a transcript for hello over the
// certificate, signed by key.
func (m member) hello(cert []byte, key ed25519.PrivateKey) tunnel.HelloRequest {
	tr := authn.Transcript(wire.ModeHello, wire.ProtoVersion, make([]byte, 32), make([]byte, 32), "h", cert, nil)
	return tunnel.HelloRequest{AgentID: m.id, Cert: cert, Transcript: tr, Sig: authn.Sign(key, tr), AgentVersion: "0.2.0", IP: "198.51.100.7"}
}

func refusal(t *testing.T, err error) *tunnel.Refusal {
	t.Helper()
	var re *tunnel.Refusal
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a *tunnel.Refusal", err)
	}
	return re
}

func TestHelloAcceptsTheCertificateAndSignatureOfAnEnrolledAgent(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	m := r.enrolMember(t, c)

	res, err := r.svc.Hello(r.ctx, m.hello(m.cert, m.priv))
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if res.AgentID != m.id || res.EnvID != c.EnvID || !res.NotAfter.Equal(fixedNow.Add(30*24*time.Hour)) {
		t.Fatalf("result = %+v", res)
	}
}

func TestHelloGivesAnUnknownEnvironmentAndAnotherCATheSameRefusal(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	m := r.enrolMember(t, c)

	unknown := tunneltest.ForeignCert(t, "0197b8a0-0000-7000-8000-000000000000", m.id, m.pub, fixedNow)
	otherCA := tunneltest.ForeignCert(t, c.EnvID, m.id, m.pub, fixedNow)
	_, e1 := r.svc.Hello(r.ctx, m.hello(unknown, m.priv))
	_, e2 := r.svc.Hello(r.ctx, m.hello(otherCA, m.priv))
	a, b := refusal(t, e1), refusal(t, e2)
	if a.Code != wire.ReasonAuthRejected || a.Code != b.Code || a.Detail != b.Detail {
		t.Fatalf("an unknown environment gets %+v, another CA gets %+v; they must be identical auth_rejected", a, b)
	}
	if strings.Contains(a.Detail, c.EnvID) {
		t.Errorf("the refusal names the environment: %q", a.Detail)
	}
}

func TestHelloRefusesAnExpiredCertificateARevokedAgentAndAChangedKey(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	m := r.enrolMember(t, c)

	// A key that is not the stored one: the certificate is still valid, so this
	// is only caught by comparing it with the row.
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := r.st.Write.Exec(`UPDATE agents SET public_key = ? WHERE id = ?`, []byte(pub2), m.id); err != nil {
		t.Fatal(err)
	}
	if re := refusal(t, func() error { _, err := r.svc.Hello(r.ctx, m.hello(m.cert, m.priv)); return err }()); re.Code != wire.ReasonAuthRejected {
		t.Errorf("a changed key: %+v, want auth_rejected", re)
	}
	if _, err := r.st.Write.Exec(`UPDATE agents SET public_key = ? WHERE id = ?`, []byte(m.pub), m.id); err != nil {
		t.Fatal(err)
	}

	if _, err := r.st.Write.Exec(`UPDATE agents SET status = 'revoked' WHERE id = ?`, m.id); err != nil {
		t.Fatal(err)
	}
	if re := refusal(t, func() error { _, err := r.svc.Hello(r.ctx, m.hello(m.cert, m.priv)); return err }()); re.Code != wire.ReasonRevoked {
		t.Errorf("a revoked agent: %+v, want revoked", re)
	}

	// Expiry is reported before the row is looked at, even for a revoked agent.
	r.clock = fixedNow.Add(31 * 24 * time.Hour)
	if re := refusal(t, func() error { _, err := r.svc.Hello(r.ctx, m.hello(m.cert, m.priv)); return err }()); re.Code != wire.ReasonCertExpired {
		t.Errorf("an expired certificate: %+v, want cert_expired", re)
	}
}

func TestHelloRefusesABadSignatureAClaimedIDThatIsNotTheCertificatesAndGarbage(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	m := r.enrolMember(t, c)
	_, other, _ := ed25519.GenerateKey(rand.Reader)

	wrongID := m.hello(m.cert, m.priv)
	wrongID.AgentID = "someone-else"
	for name, req := range map[string]tunnel.HelloRequest{
		"a signature by another key":                 m.hello(m.cert, other),
		"a claimed ID that is not the certificate's": wrongID,
		"garbage instead of a certificate":           m.hello([]byte("nope"), m.priv),
		"no certificate":                             m.hello(nil, m.priv),
	} {
		_, err := r.svc.Hello(r.ctx, req)
		if re := refusal(t, err); re.Code != wire.ReasonAuthRejected {
			t.Errorf("%s: %+v, want auth_rejected", name, re)
		}
	}
	// The transcript is the one the handler built: a signature over another
	// transcript does not verify even by the right key.
	req := m.hello(m.cert, m.priv)
	req.Transcript = authn.Transcript(wire.ModeHello, wire.ProtoVersion, make([]byte, 32), make([]byte, 32), "evil", m.cert, nil)
	if _, err := r.svc.Hello(r.ctx, req); refusal(t, err).Code != wire.ReasonAuthRejected {
		t.Error("a signature that does not cover the transcript was accepted")
	}
}

func lastSeen(t *testing.T, r *rig, id string) (int64, bool) {
	t.Helper()
	var ns sql.NullInt64
	if err := r.st.Read.QueryRow(`SELECT last_seen_at FROM agents WHERE id = ?`, id).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	return ns.Int64, ns.Valid
}

func TestTouchLastSeenWritesAtMostOncePerMinutePerAgent(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	a := r.enrolMember(t, c)
	b := r.enrolMember(t, c)
	if _, ok := lastSeen(t, r, a.id); ok {
		t.Fatal("a freshly enrolled agent already has a last_seen_at")
	}

	t0 := r.clock
	r.svc.TouchLastSeen(r.ctx, a.id)
	if got, ok := lastSeen(t, r, a.id); !ok || got != t0.UnixNano() {
		t.Fatalf("after the first touch last_seen_at = %d (%v), want %d", got, ok, t0.UnixNano())
	}

	// Two more heartbeats inside the minute change nothing: no second UPDATE.
	for _, d := range []time.Duration{10 * time.Second, 59 * time.Second} {
		r.clock = t0.Add(d)
		r.svc.TouchLastSeen(r.ctx, a.id)
		if got, _ := lastSeen(t, r, a.id); got != t0.UnixNano() {
			t.Fatalf("a touch %v after the first wrote last_seen_at = %d; at most one write a minute", d, got)
		}
	}

	// Another agent has its own minute.
	r.svc.TouchLastSeen(r.ctx, b.id)
	if got, ok := lastSeen(t, r, b.id); !ok || got != r.clock.UnixNano() {
		t.Fatalf("the second agent's last_seen_at = %d (%v); each agent is limited on its own", got, ok)
	}

	// After a minute the next one is written.
	r.clock = t0.Add(61 * time.Second)
	r.svc.TouchLastSeen(r.ctx, a.id)
	if got, _ := lastSeen(t, r, a.id); got != r.clock.UnixNano() {
		t.Fatalf("after a minute last_seen_at = %d, want %d", got, r.clock.UnixNano())
	}
}

func TestTouchLastSeenForAnUnknownAgentWritesNothingAndDoesNotFail(t *testing.T) {
	r := newRig(t)
	r.svc.TouchLastSeen(r.ctx, "no-such-agent")
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents`); n != 0 {
		t.Fatalf("%d agent rows", n)
	}
}

// Join token rotation and disabling (plan 02-10, D-02).

func tokenRowCounts(t *testing.T, st *store.Store, envID string) (total, current int) {
	t.Helper()
	total = count(t, st, `SELECT COUNT(*) FROM env_join_tokens WHERE env_id = ?`, envID)
	current = count(t, st, `SELECT COUNT(*) FROM env_join_tokens WHERE env_id = ? AND superseded_at IS NULL`, envID)
	return total, current
}

func auditRows(t *testing.T, st *store.Store, action string) (details []string) {
	t.Helper()
	rows, err := st.Read.Query(`SELECT details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		details = append(details, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

func TestRotateTokenSupersedesTheCurrentTokenAndMakesGenerationTwo(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	r.clock = fixedNow.Add(time.Hour)
	rot, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}
	if rot.Generation != 2 || rot.JoinToken == "" || rot.JoinToken == c.JoinToken {
		t.Fatalf("rotation = generation %d token %q", rot.Generation, rot.JoinToken)
	}
	envID, _, ok := agents.ParseToken(rot.JoinToken)
	if !ok || envID != c.EnvID {
		t.Fatalf("the new token %q does not parse into environment %s", rot.JoinToken, c.EnvID)
	}
	if total, current := tokenRowCounts(t, r.st, c.EnvID); total != 2 || current != 1 {
		t.Fatalf("%d token rows, %d current; want 2 and exactly 1", total, current)
	}
	var superseded sql.NullInt64
	if err := r.st.Read.QueryRow(`SELECT superseded_at FROM env_join_tokens WHERE env_id = ? AND generation = 1`, c.EnvID).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if !superseded.Valid || superseded.Int64 != fixedNow.Add(time.Hour).UnixNano() {
		t.Errorf("generation 1 superseded_at = %+v, want the rotation time", superseded)
	}
	// Only the hash of the new token's secret is kept.
	_, secret, _ := agents.ParseToken(rot.JoinToken)
	var hash []byte
	if err := r.st.Read.QueryRow(`SELECT token_hash FROM env_join_tokens WHERE env_id = ? AND generation = 2 AND superseded_at IS NULL`, c.EnvID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, sha256Sum([]byte(secret))) {
		t.Error("the current row does not hold the SHA-256 of the new token's secret")
	}
	entries := auditRows(t, r.st, audit.ActionEnvironmentTokenRotated)
	if len(entries) != 1 || !strings.Contains(entries[0], `"generation":"2"`) {
		t.Fatalf("environment.token_rotated rows = %v, want one recording generation 2", entries)
	}
}

func TestRotateTokenAuditsTheGenerationAndNeverTheToken(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	rot, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.DisableToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test"); err != nil {
		t.Fatal(err)
	}
	_, oldSecret, _ := agents.ParseToken(c.JoinToken)
	_, newSecret, _ := agents.ParseToken(rot.JoinToken)
	rows, err := r.st.Read.Query(`SELECT action, target_id, env_id, reason, ip, user_agent, details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cols [7]sql.NullString
		if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6]); err != nil {
			t.Fatal(err)
		}
		var line string
		for _, c := range cols {
			line += c.String + "|"
		}
		if strings.Contains(line, oldSecret) || strings.Contains(line, newSecret) {
			t.Errorf("an audit row holds a join token secret: %s", line)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateTokenRefusesTheOldTokenAsSupersededAndAcceptsTheNewOne(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	rot, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.Enrol(r.ctx, newProof(t, "h").request(c.JoinToken))
	var re *tunnel.RefuseError
	if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected || re.AuditReason != wire.RefuseTokenSuperseded {
		t.Fatalf("enrol with the old token: err = %v, want auth_rejected with the audit reason %s", err, wire.RefuseTokenSuperseded)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID); n != 0 {
		t.Fatalf("%d agents enrolled with a superseded token", n)
	}
	if _, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(rot.JoinToken)); err != nil {
		t.Fatalf("enrol with the new token: %v", err)
	}
}

func TestRotateTokenAnAgentEnrolledEarlierStillSaysHelloAndOldAndNewTokensAreJudgedByTheHub(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "Production")
	first := srv.Enrol(t, env)

	rot, err := srv.Svc.RotateToken(context.Background(), audit.SystemCLI, env.ID, "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}

	// The old command is refused with the same answer any wrong token gets...
	refused := srv.Dial(t, tunneltest.Agent{Token: env.Token})
	if refused.Refuse == nil || refused.Refuse.Code != wire.ReasonAuthRejected {
		t.Fatalf("enrol with the superseded token: refuse = %+v, err %v, want auth_rejected", refused.Refuse, refused.Err)
	}
	// ...and the audit keeps the specific reason.
	var reason string
	if err := srv.Store.Read.QueryRow(`SELECT reason FROM audit_log WHERE action = ? ORDER BY seq DESC LIMIT 1`, audit.ActionAgentEnrolRefused).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "token_superseded" {
		t.Errorf("agent.enrol_refused reason = %q, want token_superseded", reason)
	}

	// The new one enrols a second agent.
	if c := srv.Dial(t, tunneltest.Agent{Token: rot.JoinToken}); c.Welcome == nil {
		t.Fatalf("enrol with the new token was refused: %+v (err %v)", c.Refuse, c.Err)
	}
	// The agent enrolled before the rotation reconnects with its certificate.
	if c := srv.Hello(t, first.Identity); c.Welcome == nil {
		t.Fatalf("hello after the rotation was refused: %+v (err %v)", c.Refuse, c.Err)
	}
}

func TestDisableTokenRefusesEnrolmentAsDisabledUntilTheNextRotation(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	rot2, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.DisableToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test"); err != nil {
		t.Fatal(err)
	}
	entries := auditRows(t, r.st, audit.ActionEnvironmentTokenDisabled)
	if len(entries) != 1 || !strings.Contains(entries[0], `"generation":"2"`) {
		t.Fatalf("environment.token_disabled rows = %v, want one recording generation 2", entries)
	}
	_, err = r.svc.Enrol(r.ctx, newProof(t, "h").request(rot2.JoinToken))
	var re *tunnel.RefuseError
	if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected || re.AuditReason != wire.RefuseTokenDisabled {
		t.Fatalf("enrol with a disabled token: err = %v, want auth_rejected with the audit reason %s", err, wire.RefuseTokenDisabled)
	}
	st, err := r.svc.TokenStatus(r.ctx, c.EnvID)
	if err != nil || !st.Disabled || st.Generation != 2 {
		t.Fatalf("status = %+v, %v, want generation 2 disabled", st, err)
	}

	// Disabling again changes nothing and is not audited a second time.
	if err := r.svc.DisableToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test"); err != nil {
		t.Fatal(err)
	}
	if n := len(auditRows(t, r.st, audit.ActionEnvironmentTokenDisabled)); n != 1 {
		t.Errorf("%d environment.token_disabled rows after disabling twice, want 1", n)
	}

	// A rotation makes generation 3 and lets agents enrol again.
	rot3, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test")
	if err != nil || rot3.Generation != 3 {
		t.Fatalf("rotation after a disable = %+v, %v, want generation 3", rot3, err)
	}
	if _, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(rot3.JoinToken)); err != nil {
		t.Fatalf("enrol with the generation 3 token: %v", err)
	}
	st, err = r.svc.TokenStatus(r.ctx, c.EnvID)
	if err != nil || st.Disabled || st.Generation != 3 {
		t.Fatalf("status = %+v, %v, want generation 3 enabled", st, err)
	}
}

func TestEnrolWithAWrongSecretIsStillATokenMismatchAfterARotation(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	if _, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test"); err != nil {
		t.Fatal(err)
	}
	other, _, _ := agents.ParseToken(c.JoinToken)
	forged := "bpjt_" + other + "_" + strings.Repeat("A", 43)
	_, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(forged))
	var re *tunnel.RefuseError
	if !errors.As(err, &re) || re.Code != wire.ReasonAuthRejected || re.AuditReason != "token_mismatch" {
		t.Fatalf("err = %v, want auth_rejected with the audit reason token_mismatch", err)
	}
}

func TestRotateTokenReportsASwarmWithAgentsAndAStandaloneHostAndNone(t *testing.T) {
	r := newRig(t)
	none := r.create(t, "Empty")
	rot, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, none.EnvID, "203.0.113.9", "test")
	if err != nil || rot.HasAgents || rot.EnvType != "" {
		t.Fatalf("an environment with no agents: %+v, %v, want hasAgents false and no type", rot, err)
	}

	standalone := r.create(t, "Standalone")
	if _, err := r.svc.Enrol(r.ctx, newProof(t, "h").request(standalone.JoinToken)); err != nil {
		t.Fatal(err)
	}
	rot, err = r.svc.RotateToken(r.ctx, audit.SystemCLI, standalone.EnvID, "203.0.113.9", "test")
	if err != nil || !rot.HasAgents || rot.EnvType != "standalone" {
		t.Fatalf("a standalone host: %+v, %v", rot, err)
	}

	swarm := r.create(t, "Swarm")
	req := newProof(t, "h").request(swarm.JoinToken)
	req.Facts.EngineID = "ENG:swarm-1"
	req.Facts.Swarm = &wire.SwarmFacts{State: "active", NodeID: "node-1", ClusterID: "cl-1", Control: true}
	if _, err := r.svc.Enrol(r.ctx, req); err != nil {
		t.Fatal(err)
	}
	rot, err = r.svc.RotateToken(r.ctx, audit.SystemCLI, swarm.EnvID, "203.0.113.9", "test")
	if err != nil || !rot.HasAgents || rot.EnvType != "swarm" {
		t.Fatalf("a swarm with a manager: %+v, %v, want swarm with agents", rot, err)
	}

	// A revoked agent does not count.
	if _, err := r.st.Write.Exec(`UPDATE agents SET status = 'revoked' WHERE env_id = ?`, swarm.EnvID); err != nil {
		t.Fatal(err)
	}
	rot, err = r.svc.RotateToken(r.ctx, audit.SystemCLI, swarm.EnvID, "203.0.113.9", "test")
	if err != nil || rot.HasAgents {
		t.Fatalf("a swarm whose only agent is revoked: %+v, %v, want hasAgents false", rot, err)
	}
}

func TestRotateAndDisableTokenAnUnknownOrLocalEnvironmentIsNotFoundAndWritesNothing(t *testing.T) {
	r := newRig(t)
	if _, err := r.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES ('local-1', 'local', 'This host', '/var/run/docker.sock', 1, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"0197b8a0-0000-7000-8000-000000000000", "local-1"} {
		if _, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, id, "203.0.113.9", "test"); !errors.Is(err, agents.ErrEnvironmentNotFound) {
			t.Errorf("RotateToken(%s) = %v, want ErrEnvironmentNotFound", id, err)
		}
		if err := r.svc.DisableToken(r.ctx, audit.SystemCLI, id, "203.0.113.9", "test"); !errors.Is(err, agents.ErrEnvironmentNotFound) {
			t.Errorf("DisableToken(%s) = %v, want ErrEnvironmentNotFound", id, err)
		}
		if _, err := r.svc.TokenStatus(r.ctx, id); !errors.Is(err, agents.ErrEnvironmentNotFound) {
			t.Errorf("TokenStatus(%s) = %v, want ErrEnvironmentNotFound", id, err)
		}
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM audit_log WHERE action LIKE 'environment.token_%'`); n != 0 {
		t.Errorf("%d token audit rows written for environments that have no token", n)
	}
}

func TestTokenStatusReportsGenerationCreatorAndTheDisabledFlagNeverTheToken(t *testing.T) {
	r := newRig(t)
	if _, err := r.st.Write.Exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at)
		VALUES ('user-1', 'ada', 'x', 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	c, err := r.svc.CreateEnvironment(r.ctx, audit.Actor{Type: audit.ActorUser, ID: "user-1", Name: "ada"}, "Production", "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.svc.TokenStatus(r.ctx, c.EnvID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != 1 || st.Disabled || st.CreatedBy != "ada" || !st.CreatedAt.Equal(fixedNow) {
		t.Fatalf("status = %+v, want generation 1, enabled, by ada at %v", st, fixedNow)
	}
	// A creator that is not a user (the CLI, a deleted account) is shown as it was recorded.
	rot, err := r.svc.RotateToken(r.ctx, audit.SystemCLI, c.EnvID, "203.0.113.9", "test")
	if err != nil {
		t.Fatal(err)
	}
	st, err = r.svc.TokenStatus(r.ctx, c.EnvID)
	if err != nil || st.Generation != 2 || st.CreatedBy != "system:cli" {
		t.Fatalf("status = %+v, %v, want generation 2 by system:cli", st, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", st), rot.JoinToken) {
		t.Error("the status holds the token")
	}
}

// kicks is a SessionKicker that records what it was asked, and what the database
// said at that moment, so a test can tell that the kick came after the commit.
type kicks struct {
	st     *store.Store
	agents []string
	envs   []string
	codes  []string
	// statusAtKick is the status the agent's row had when Kick ran.
	statusAtKick string
}

func (k *kicks) Kick(agentID, code string) {
	k.agents = append(k.agents, agentID)
	k.codes = append(k.codes, code)
	_ = k.st.Read.QueryRow(`SELECT status FROM agents WHERE id = ?`, agentID).Scan(&k.statusAtKick)
}

func (k *kicks) KickEnv(envID, code string) {
	k.envs = append(k.envs, envID)
	k.codes = append(k.codes, code)
}

// enrolWith enrols an agent that reports facts.
func (r *rig) enrolWith(t *testing.T, token string, facts wire.Facts) (tunnel.EnrolResult, error) {
	t.Helper()
	p := newProof(t, "h")
	req := p.request(token)
	req.Facts = facts
	return r.svc.Enrol(r.ctx, req)
}

var swarmWorker = wire.Facts{
	EngineID: "ENG:worker-1", Hostname: "worker-1", EngineReachable: true,
	Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-worker-1"},
}

var ops = audit.Actor{Type: audit.ActorUser, ID: "u-ops", Name: "ops"}

func blockRows(t *testing.T, st *store.Store, envID string) map[string]string {
	t.Helper()
	rows, err := st.Read.Query(`SELECT kind, identity FROM agent_blocks WHERE env_id = ?`, envID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			t.Fatal(err)
		}
		got[k] = id
	}
	return got
}

func TestRevokeSetsTheAgentRevokedBlocksBothIdentitiesAuditsAndKicksAfterTheCommit(t *testing.T) {
	r := newRig(t)
	k := &kicks{st: r.st}
	r.svc.WithKicker(k)
	c := r.create(t, "Swarm")
	res, err := r.enrolWith(t, c.JoinToken, swarmWorker)
	if err != nil {
		t.Fatal(err)
	}
	r.clock = fixedNow.Add(time.Hour)

	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, "decommissioned", "203.0.113.5", "test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	var status, by, reason string
	var at int64
	if err := r.st.Read.QueryRow(`SELECT status, revoked_by, revoke_reason, revoked_at FROM agents WHERE id = ?`, res.AgentID).Scan(&status, &by, &reason, &at); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" || by != "u-ops" || reason != "decommissioned" || at != fixedNow.Add(time.Hour).UnixNano() {
		t.Errorf("row = %s by %s because %q at %d", status, by, reason, at)
	}
	if got := blockRows(t, r.st, c.EnvID); got["engine_id"] != "ENG:worker-1" || got["swarm_node_id"] != "node-worker-1" || len(got) != 2 {
		t.Errorf("blocks = %v, want the engine ID and the swarm node ID", got)
	}
	if len(k.agents) != 1 || k.agents[0] != res.AgentID || k.codes[0] != "revoked" {
		t.Errorf("kicks = %v %v, want one kick of the agent with the code revoked", k.agents, k.codes)
	}
	if k.statusAtKick != "revoked" {
		t.Errorf("the agent was kicked while its row said %q: the kick must follow the commit", k.statusAtKick)
	}

	revoked := auditRows(t, r.st, "agent.revoked")
	if len(revoked) != 1 || !strings.Contains(revoked[0], `"reason":"decommissioned"`) || !strings.Contains(revoked[0], `"node":"worker-1"`) || !strings.Contains(revoked[0], `"fingerprint":"`) {
		t.Errorf("agent.revoked = %v", revoked)
	}
	blocked := auditRows(t, r.st, "agent.blocked")
	if len(blocked) != 2 {
		t.Fatalf("agent.blocked = %v, want one per identity", blocked)
	}
	joined := strings.Join(blocked, "\n")
	for _, want := range []string{`"kind":"engine_id"`, `"identity":"ENG:worker-1"`, `"kind":"swarm_node_id"`, `"identity":"node-worker-1"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("agent.blocked lacks %s: %s", want, joined)
		}
	}
}

func TestRevokeOfAStandaloneAgentBlocksOnlyItsEngineID(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Edge")
	res, err := r.enrolWith(t, c.JoinToken, wire.Facts{EngineID: "ENG:solo", Hostname: "solo", EngineReachable: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := blockRows(t, r.st, c.EnvID); len(got) != 1 || got["engine_id"] != "ENG:solo" {
		t.Errorf("blocks = %v", got)
	}
}

func TestRevokeRefusesAnAlreadyRevokedAgentAnUnknownOneAndOneOfAnotherEnvironmentAndChangesNothing(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "One")
	other := r.create(t, "Two")
	res, err := r.enrolWith(t, c.JoinToken, swarmWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, "first", "", ""); err != nil {
		t.Fatal(err)
	}
	audits := len(auditRows(t, r.st, "agent.revoked"))

	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, "second", "", ""); !errors.Is(err, agents.ErrAlreadyRevoked) {
		t.Errorf("revoking twice = %v, want ErrAlreadyRevoked", err)
	}
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, "no-such-agent", "", "", ""); !errors.Is(err, agents.ErrAgentNotFound) {
		t.Errorf("unknown agent = %v, want ErrAgentNotFound", err)
	}
	// An agent is only found through its own environment.
	fresh, err := r.enrolWith(t, c.JoinToken, worker("ENG:fresh", "node-fresh", "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(r.ctx, ops, other.EnvID, fresh.AgentID, "", "", ""); !errors.Is(err, agents.ErrAgentNotFound) {
		t.Errorf("an agent through another environment = %v, want ErrAgentNotFound", err)
	}
	if err := r.svc.Revoke(r.ctx, ops, "0197b8a0-0000-7000-8000-000000000000", fresh.AgentID, "", "", ""); !errors.Is(err, agents.ErrEnvironmentNotFound) {
		t.Errorf("an unknown environment = %v, want ErrEnvironmentNotFound", err)
	}
	if got := len(auditRows(t, r.st, "agent.revoked")); got != audits {
		t.Errorf("agent.revoked rows went from %d to %d", audits, got)
	}
	var status string
	_ = r.st.Read.QueryRow(`SELECT status FROM agents WHERE id = ?`, fresh.AgentID).Scan(&status)
	if status != "active" {
		t.Errorf("the other environment's agent is %s", status)
	}
}

func TestRevokeKeepsAnOverlongReasonToTheLimitAndStripsControlCharacters(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "One")
	res, err := r.enrolWith(t, c.JoinToken, swarmWorker)
	if err != nil {
		t.Fatal(err)
	}
	long := "ok\x00\x1b[31m" + strings.Repeat("é", 400)
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, long, "", ""); err != nil {
		t.Fatal(err)
	}
	var reason string
	_ = r.st.Read.QueryRow(`SELECT revoke_reason FROM agents WHERE id = ?`, res.AgentID).Scan(&reason)
	if n := len([]rune(reason)); n > 256 || strings.ContainsAny(reason, "\x00\x1b") {
		t.Errorf("stored reason has %d runes and control characters: %q", n, reason)
	}
}

func TestEnrolRefusesABlockedEngineIDAndABlockedSwarmNodeIDEvenWithAValidTokenUntilUnblocked(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Swarm")
	res, err := r.enrolWith(t, c.JoinToken, swarmWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, "gone", "", ""); err != nil {
		t.Fatal(err)
	}
	agentsBefore := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID)

	// The same engine under a new node ID, and the same node ID under a new engine:
	// each identity blocks on its own.
	sameEngine := swarmWorker
	sameEngine.Swarm = &wire.SwarmFacts{State: "active", NodeID: "node-other"}
	sameNode := swarmWorker
	sameNode.EngineID = "ENG:other"
	for name, f := range map[string]wire.Facts{"the same host": swarmWorker, "the same engine ID": sameEngine, "the same swarm node ID": sameNode} {
		_, err := r.enrolWith(t, c.JoinToken, f)
		re := refusal(t, err)
		if re.Code != wire.ReasonBlocked {
			t.Errorf("%s: %+v, want blocked", name, re)
		}
	}
	if got := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID); got != agentsBefore {
		t.Errorf("a refused enrolment wrote an agent row: %d -> %d", agentsBefore, got)
	}
	// A different host with the same token is not affected.
	if _, err := r.enrolWith(t, c.JoinToken, worker("ENG:new", "node-new", "new")); err != nil {
		t.Errorf("an unrelated host was refused: %v", err)
	}

	if err := r.svc.Unblock(r.ctx, ops, c.EnvID, res.AgentID, "203.0.113.5", "test"); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	if got := blockRows(t, r.st, c.EnvID); len(got) != 0 {
		t.Errorf("blocks after Unblock = %v", got)
	}
	again, err := r.enrolWith(t, c.JoinToken, swarmWorker)
	if err != nil {
		t.Fatalf("enrolling after Unblock: %v", err)
	}
	if again.AgentID == res.AgentID {
		t.Error("the unblocked host got its old agent row back; it must get a new one")
	}
	var old string
	_ = r.st.Read.QueryRow(`SELECT status FROM agents WHERE id = ?`, res.AgentID).Scan(&old)
	if old != "revoked" {
		t.Errorf("the old agent is %s after the host enrolled again", old)
	}
	un := auditRows(t, r.st, "agent.unblocked")
	if len(un) != 2 || !strings.Contains(strings.Join(un, ""), `"identity":"node-worker-1"`) {
		t.Errorf("agent.unblocked = %v", un)
	}
}

func TestUnblockOfAnAgentWithNoBlocksIsNotBlockedAndOfAnUnknownOneIsNotFound(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "One")
	res, err := r.enrolWith(t, c.JoinToken, swarmWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Unblock(r.ctx, ops, c.EnvID, res.AgentID, "", ""); !errors.Is(err, agents.ErrNotBlocked) {
		t.Errorf("an active agent = %v, want ErrNotBlocked", err)
	}
	if err := r.svc.Unblock(r.ctx, ops, c.EnvID, "nope", "", ""); !errors.Is(err, agents.ErrAgentNotFound) {
		t.Errorf("an unknown agent = %v, want ErrAgentNotFound", err)
	}
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, res.AgentID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Unblock(r.ctx, ops, c.EnvID, res.AgentID, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Unblock(r.ctx, ops, c.EnvID, res.AgentID, "", ""); !errors.Is(err, agents.ErrNotBlocked) {
		t.Errorf("unblocking twice = %v, want ErrNotBlocked", err)
	}
}

func TestStillActiveIsTrueOnlyForAnActiveAgent(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "One")
	m := r.enrolMember(t, c)
	if !r.svc.StillActive(m.id) {
		t.Error("an active agent is not active")
	}
	if r.svc.StillActive("no-such-agent") {
		t.Error("an unknown agent is active")
	}
	if err := r.svc.Revoke(r.ctx, ops, c.EnvID, m.id, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if r.svc.StillActive(m.id) {
		t.Error("a revoked agent is active")
	}
}
