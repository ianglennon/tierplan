// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package agents_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/agents"
	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/internal/tunnel/tunneltest"
	"github.com/ianglennon/coaming/internal/vault"
	"github.com/ianglennon/coaming/proto/authn"
	"github.com/ianglennon/coaming/proto/wire"
)

const restorePass = "a long enough passphrase"

// restoreFrom builds a server on a data directory restored from a backup file: another
// vault key, the same database content as of the backup.
func restoreFrom(t *testing.T, file []byte) *agents.Service {
	t.Helper()
	ctx := context.Background()
	target, err := vault.New(bytes.Repeat([]byte{9}, 32), "restored")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := store.TargetSchema()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	staged, err := backup.Stage(ctx, bytes.NewReader(file), []byte(restorePass), dataDir, schema)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer staged.Discard()
	if err := staged.Prepare(ctx, target, backup.PrepareOptions{Source: "test", AppVersion: "v9.9.9", Now: func() time.Time { return fixedNow }}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := staged.Swap(ctx, dataDir, fixedNow); err != nil {
		t.Fatalf("swap: %v", err)
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := func() time.Time { return fixedNow }
	return agents.NewService(st, target, audit.NewWriter(st, target, slog.New(slog.DiscardHandler), now), now)
}

// After a restore, an agent enrolled before the backup says hello with the
// certificate it has and is accepted; one enrolled after the backup holds a
// certificate the restored CA signed but has no row, and is told unknown_agent (D-31).
func TestRestoredHelloAcceptsAnAgentFromBeforeTheBackupAndRefusesOneEnrolledAfterAsUnknownAgent(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	old := r.enrolMember(t, c)
	// The backup is taken here.
	var file bytes.Buffer
	if _, err := backup.Export(context.Background(), r.st, r.vlt, t.TempDir(), &file, []byte(restorePass), backup.ExportOptions{AppVersion: "v9.9.9", Now: func() time.Time { return fixedNow }}); err != nil {
		t.Fatalf("export: %v", err)
	}
	late := r.enrolMember(t, c)
	restored := restoreFrom(t, file.Bytes())

	res, err := restored.Hello(r.ctx, old.hello(old.cert, old.priv))
	if err != nil || res.AgentID != old.id || res.EnvID != c.EnvID {
		t.Fatalf("the agent enrolled before the backup: %+v, %v; want it accepted", res, err)
	}

	_, err = restored.Hello(r.ctx, late.hello(late.cert, late.priv))
	re := refusal(t, err)
	if re.Code != wire.ReasonUnknownAgent || re.AuditReason != "unknown_agent" {
		t.Fatalf("the agent enrolled after the backup: code %q, audit reason %q; want unknown_agent for both", re.Code, re.AuditReason)
	}
	if !strings.Contains(re.Detail, "enrolled after the backup this Coaming was restored from") {
		t.Errorf("detail = %q", re.Detail)
	}
	listed := restored.Unknown().List(fixedNow)
	if len(listed) != 1 || listed[0].EnvID != c.EnvID || listed[0].Node != late.id || listed[0].IP != "198.51.100.7" || !listed[0].LastAttempt.Equal(fixedNow) {
		t.Fatalf("unknown agents = %+v, want only the late agent", listed)
	}
}

// Only a holder of a certificate the environment issued is told unknown_agent
// (T-03-115, P2 02-06): everything else stays the uniform auth_rejected.
func TestUnknownAgentIsSaidOnlyForACertificateTheEnvironmentIssuedAndItsKey(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	m := r.enrolMember(t, c)
	if _, err := r.st.Write.Exec(`DELETE FROM agents WHERE id = ?`, m.id); err != nil {
		t.Fatal(err)
	}

	foreign := tunneltest.ForeignCert(t, c.EnvID, m.id, m.pub, fixedNow)
	_, err := r.svc.Hello(r.ctx, m.hello(foreign, m.priv))
	if re := refusal(t, err); re.Code != wire.ReasonAuthRejected || re.Detail != "the agent's certificate was not accepted" || re.AuditReason != "bad_chain" {
		t.Errorf("a certificate from another CA: %+v, want the uniform auth_rejected", re)
	}

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	_, err = r.svc.Hello(r.ctx, m.hello(m.cert, other))
	if re := refusal(t, err); re.Code != wire.ReasonAuthRejected || re.AuditReason != "bad_signature" {
		t.Errorf("the right certificate signed by another key: %+v, want auth_rejected", re)
	}
	if got := r.svc.Unknown().List(fixedNow); len(got) != 0 {
		t.Fatalf("a refusal that was not unknown_agent was listed: %+v", got)
	}

	_, err = r.svc.Hello(r.ctx, m.hello(m.cert, m.priv))
	if re := refusal(t, err); re.Code != wire.ReasonUnknownAgent {
		t.Errorf("the right certificate and key: %+v, want unknown_agent", re)
	}
}

func TestEnrollingWithAListedKeyTakesItOffTheList(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "Production")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r.svc.Unknown().Record(agents.UnknownAgent{EnvID: c.EnvID, Node: "n", LastAttempt: fixedNow, Key: agents.KeyFingerprint(pub)})
	tr := authn.Transcript(wire.ModeEnrol, wire.ProtoVersion, make([]byte, 32), make([]byte, 32), "h", pub, nil)
	if _, err := r.svc.Enrol(r.ctx, tunnel.EnrolRequest{
		Token: c.JoinToken, Pub: pub, Sig: authn.Sign(priv, tr), Transcript: tr, AgentVersion: "0.2.0", IP: "198.51.100.7",
		Facts: wire.Facts{EngineID: "ENG:x", Hostname: "node-1", EngineReachable: true},
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.svc.Unknown().List(fixedNow); len(got) != 0 {
		t.Fatalf("still listed after enrolling: %+v", got)
	}
}

func TestUnknownAgentsListsNewestFirstAndEmptiesBy24HoursAfterTheLastAttempt(t *testing.T) {
	u := agents.NewUnknownAgents()
	u.Record(agents.UnknownAgent{EnvID: "e1", Node: "a", Key: "k1", LastAttempt: fixedNow.Add(-3 * time.Hour)})
	u.Record(agents.UnknownAgent{EnvID: "e1", Node: "b", Key: "k2", LastAttempt: fixedNow.Add(-1 * time.Hour)})
	u.Record(agents.UnknownAgent{EnvID: "e2", Node: "c", Key: "k1", LastAttempt: fixedNow.Add(-2 * time.Hour)})
	got := u.List(fixedNow)
	if len(got) != 3 || got[0].Node != "b" || got[1].Node != "c" || got[2].Node != "a" {
		t.Fatalf("order = %+v, want b, c, a", got)
	}
	// The same key in the same environment is one entry, with the newest attempt.
	u.Record(agents.UnknownAgent{EnvID: "e1", Node: "a", IP: "203.0.113.1", Key: "k1", LastAttempt: fixedNow})
	got = u.List(fixedNow)
	if len(got) != 3 || got[0].Node != "a" || got[0].IP != "203.0.113.1" {
		t.Fatalf("after a second attempt = %+v", got)
	}
	// 24 hours after its last attempt an entry is gone, and the others stay.
	got = u.List(fixedNow.Add(24*time.Hour - 150*time.Minute))
	if len(got) != 3 {
		t.Fatalf("before the expiry = %+v", got)
	}
	got = u.List(fixedNow.Add(24*time.Hour - 90*time.Minute))
	if len(got) != 2 {
		t.Fatalf("after the oldest expired = %+v, want 2 (c was 2h before now)", got)
	}
	if got := u.List(fixedNow.Add(25 * time.Hour)); len(got) != 0 {
		t.Fatalf("a day after the last attempt: %+v, want none", got)
	}
}

func TestUnknownAgentsKeepsAtMost1024AndDropsTheOldestFirst(t *testing.T) {
	u := agents.NewUnknownAgents()
	for i := range 1100 {
		u.Record(agents.UnknownAgent{EnvID: "e", Node: fmt.Sprint(i), Key: fmt.Sprint(i), LastAttempt: fixedNow.Add(time.Duration(i) * time.Second)})
	}
	got := u.List(fixedNow.Add(time.Hour))
	if len(got) != 1024 {
		t.Fatalf("len = %d, want 1024", len(got))
	}
	if got[0].Node != "1099" || got[len(got)-1].Node != "76" {
		t.Errorf("kept %s down to %s, want 1099 down to 76 (the oldest are dropped)", got[0].Node, got[len(got)-1].Node)
	}
}

func TestUnknownAgentsForgetRemovesOnlyThatKeyInThatEnvironment(t *testing.T) {
	u := agents.NewUnknownAgents()
	u.Record(agents.UnknownAgent{EnvID: "e1", Key: "k", LastAttempt: fixedNow})
	u.Record(agents.UnknownAgent{EnvID: "e2", Key: "k", LastAttempt: fixedNow})
	u.Forget("e1", "k")
	if got := u.List(fixedNow); len(got) != 1 || got[0].EnvID != "e2" {
		t.Fatalf("after Forget = %+v", got)
	}
	var none *agents.UnknownAgents
	none.Record(agents.UnknownAgent{})
	none.Forget("a", "b")
	if none.List(fixedNow) != nil {
		t.Error("a nil registry lists something")
	}
}
