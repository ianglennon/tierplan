// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// wantVIPAllDone fails the test unless every item of the virtual IP enable job ended done.
func wantVIPAllDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemVIPKey, ingressdeploy.ItemVIPSecret, ingressdeploy.ItemVIPService, ingressdeploy.ItemVIPSwitch} {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
}

// codesOf are the codes of the findings of a severity.
func codesOf(fs []ingressdeploy.Finding, severity string) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == severity {
			out = append(out, f.Code)
		}
	}
	return out
}

// TestEnableVIPOnSwarm is the tracer: on a swarm whose ingress is on and whose three managers allow, the preview refuses
// nothing, shows the router ID, the manager table, the fixed warning and the unverified line, and writes nothing; the
// enable then makes the key, the swarm secret, the keepalived service and the stored row with its audit entry (D-16).
func TestEnableVIPOnSwarm(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	ctx := testsupport.Context(t)
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}

	pv, err := v.svc.PreviewVIP(ctx, v.envID, req)
	if err != nil {
		t.Fatalf("PreviewVIP: %v", err)
	}
	if errs := codesOf(pv.Findings, ingressdeploy.SeverityError); len(errs) > 0 {
		t.Fatalf("the preview refuses with %v, want no refusal; findings = %+v", errs, pv.Findings)
	}
	if !contains(codesOf(pv.Findings, ingressdeploy.SeverityWarning), ingressdeploy.CodeVIPUnverified) {
		t.Errorf("the preview has no %s line: %+v", ingressdeploy.CodeVIPUnverified, pv.Findings)
	}
	want := ingressdeploy.DerivedRouterID(testClusterID)
	if pv.RouterID != want || pv.DerivedRouterID != want || !pv.RouterDerived {
		t.Errorf("router ID = %d (derived %d, %v), want the derived %d", pv.RouterID, pv.DerivedRouterID, pv.RouterDerived, want)
	}
	if pv.Address != netip.MustParsePrefix(vipTestAddress) || len(pv.Managers) != 3 || pv.Warning != ingressdeploy.VIPPrivilegeWarning || len(pv.Items) != 4 {
		t.Errorf("preview = address %v, %d managers, %d items, warning %q", pv.Address, len(pv.Managers), len(pv.Items), pv.Warning)
	}
	if w := v.fake.Writes(); len(w) != 0 {
		t.Errorf("the preview wrote %v", w)
	}
	if _, err := v.istore.GetVIP(ctx, v.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("GetVIP after the preview = %v, want ErrVIPNotFound", err)
	}
	if v.auditCount("ingress.vip_enabled") != 0 {
		t.Error("the preview wrote an audit row")
	}

	job := v.enableVIP(req)
	wantVIPAllDone(t, job)
	if job.Kind != ingressdeploy.JobKindVIPEnable || job.EnvID != v.envID || job.Owner != testOwner {
		t.Errorf("job = kind %q env %q owner %q", job.Kind, job.EnvID, job.Owner)
	}

	// The secret holds the key as the text hex: and 64 digits, under the keepalived labels.
	name := ingressdeploy.KeepalivedSecretName(v.envID, 1)
	var secretID string
	for _, s := range v.secrets() {
		if s.Name == name {
			secretID = s.ID
			if !ingress.OwnsKeepalivedSecret(s.Name, s.Labels) {
				t.Errorf("secret labels = %v, want the keepalived labels", s.Labels)
			}
		}
	}
	if secretID == "" {
		t.Fatalf("the swarm holds no secret %s: %+v", name, v.secrets())
	}
	if creates := v.fake.SecretCreates(); len(creates) != 1 || creates[0].DataBytes != len("hex:")+64 {
		t.Errorf("secret creates = %+v, want one of 68 bytes", creates)
	}

	// The service is what KeepalivedServiceSpec builds, with the secret's ID filled in.
	creates := v.fake.ServiceCreates()
	if len(creates) != 1 {
		t.Fatalf("service creates = %d, want 1", len(creates))
	}
	spec := ingressdeploy.KeepalivedServiceSpec(ingressdeploy.KeepalivedSpecInput{
		Image: ingressdeploy.KeepalivedImage(testServerVersion), Address: netip.MustParsePrefix(vipTestAddress), RouterID: want, SecretName: name,
	})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = secretID
	if d := firstDiff("spec", asTree(t, spec), asTree(t, creates[0].Spec)); d != "" {
		t.Errorf("the created service differs from KeepalivedServiceSpec at %s", d)
	}

	// The row holds the address, the router ID and the key sealed under its context; the audit row names no key.
	row, err := v.istore.GetVIP(ctx, v.envID)
	if err != nil {
		t.Fatalf("GetVIP: %v", err)
	}
	if row.Address != netip.MustParseAddr("10.9.0.50") || row.PrefixLen != 24 || row.RouterID != want || !row.RouterDerived || row.KeyGeneration != 1 || !row.Enabled {
		t.Errorf("row = %+v", row)
	}
	vl, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := vl.Open(vault.ContextIngressVIPKey+v.envID, row.KeySealed); err != nil || len(raw) != 32 {
		t.Errorf("the sealed key opens to %d bytes, %v; want 32 under ContextIngressVIPKey", len(raw), err)
	}
	if got := v.auditCount("ingress.vip_enabled"); got != 1 {
		t.Fatalf("ingress.vip_enabled audit rows = %d, want 1", got)
	}
	var details string
	if err := v.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'ingress.vip_enabled'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(details), &d); err != nil || d["address"] != vipTestAddress || d["generation"] != "1" || d["router_id"] == "" {
		t.Errorf("audit details = %q (%v), want address, router_id and generation", details, err)
	}
	if _, held := v.locks.Held(v.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}

// recordingHandler keeps every log record's message and attributes, as text.
type recordingHandler struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&h.buf, " %s=%v", a.Key, a.Value.Resolve())
		return true
	})
	h.buf.WriteByte('\n')
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// keyShape is the form the key takes wherever it could leak: 64 hex digits.
var keyShape = regexp.MustCompile(`[0-9a-fA-F]{64}`)

// seenOf gathers everything an enable shows: the job with its items and details, its output, the preview, the audit
// details, the created service's spec and the log.
func seenOf(t *testing.T, v *vipEnableRig, log *recordingHandler, job jobs.Job, pv ingressdeploy.VIPPreview) string {
	t.Helper()
	var all strings.Builder
	for _, x := range []any{job, pv} {
		raw, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	lines, _ := v.hub.Output(job.ID)
	for _, l := range lines {
		all.WriteString(l.Text)
	}
	rows, err := v.st.Read.Query(`SELECT details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		all.WriteString(d)
	}
	for _, c := range v.fake.ServiceCreates() {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	all.WriteString(log.text())
	return all.String()
}

// TestEnableVIPNeverShowsTheKey: the key's bytes and their hex form are in no job item, detail or output line, no preview field,
// no log record, no audit detail and not the service's spec or environment; only the secret's content carries them (PROH-26-03).
func TestEnableVIPNeverShowsTheKey(t *testing.T) {
	log := &recordingHandler{}
	v := newVIPEnableRig(t, nil, func(d *ingressdeploy.Deps) { d.Log = slog.New(log) })
	ctx := testsupport.Context(t)
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}
	pv, err := v.svc.PreviewVIP(ctx, v.envID, req)
	if err != nil {
		t.Fatal(err)
	}
	job := v.enableVIP(req)
	wantVIPAllDone(t, job)

	row, err := v.istore.GetVIP(ctx, v.envID)
	if err != nil {
		t.Fatal(err)
	}
	vl, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := vl.Open(vault.ContextIngressVIPKey+v.envID, row.KeySealed)
	if err != nil || len(raw) != 32 {
		t.Fatalf("the sealed key = %d bytes, %v", len(raw), err)
	}
	seen := seenOf(t, v, log, job, pv)
	if strings.Contains(seen, string(raw)) {
		t.Error("the key's raw bytes are shown somewhere")
	}
	if strings.Contains(seen, hex.EncodeToString(raw)) {
		t.Error("the key's hex form is shown somewhere")
	}
	if m := keyShape.FindString(seen); m != "" {
		t.Errorf("a 64-digit hex string is shown: %q", m)
	}
	// Only the secret's content carries the key: it is the text hex: and the 64 digits, 68 bytes.
	if creates := v.fake.SecretCreates(); len(creates) != 1 || creates[0].DataBytes != 68 {
		t.Errorf("secret creates = %+v, want one of 68 bytes", creates)
	}
	// The container environment carries the key's file path and never the key.
	for _, c := range v.fake.ServiceCreates() {
		env := strings.Join(c.Spec.TaskTemplate.ContainerSpec.Env, "\n")
		if !strings.Contains(env, ingressdeploy.KeepalivedKeyFile) {
			t.Errorf("the service environment lacks the key file's path: %s", env)
		}
	}

	// A failing secret step shows no key shape either, in its job, output or log.
	f := newVIPEnableRig(t, func(c *dockertest.FakeConfig) { c.WriteError = "the manager refused the write" },
		func(d *ingressdeploy.Deps) { d.Log = slog.New(log) })
	failed := f.enableVIP(req)
	if failed.State == jobs.JobDone {
		t.Fatal("the enable with a refusing manager ended done")
	}
	if m := keyShape.FindString(seenOf(t, f, log, failed, pv)); m != "" {
		t.Errorf("a failed enable shows a 64-digit hex string: %q", m)
	}
}

// TestEnableVIPLocked: a second enable while the first's job holds the lock is refused with the first as the holder, and
// writes nothing of its own.
func TestEnableVIPLocked(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	ctx := testsupport.Context(t)
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}
	g := newGate()
	v.eng.gate = g
	id, err := v.svc.EnableVIP(ctx, testActor, testOwner, v.envID, req)
	if err != nil {
		t.Fatalf("EnableVIP: %v", err)
	}
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("the job never reached its first write")
	}
	_, err = v.svc.EnableVIP(ctx, testActor, testOwner, v.envID, req)
	var locked *ingressdeploy.LockedError
	if !errors.As(err, &locked) || locked.Holder.Kind != stackops.HolderKindIngress || locked.Holder.JobID != id || locked.Holder.By != testOwner {
		t.Fatalf("a second EnableVIP = %v, want a *LockedError naming the first job %s", err, id)
	}
	if got := len(v.fake.SecretCreates()); got != 0 {
		t.Errorf("the refused enable or the gated one made %d secrets before the gate opened", got)
	}
	close(g.proceed)
	wantVIPAllDone(t, v.wait(id))
	if _, held := v.locks.Held(v.envID); held {
		t.Error("the lock is held after the job ended")
	}
}

// TestEnableVIPFailedStep: a SecretCreate that fails ends the secret item with vip_secret_failed, the service and switch items
// as not run, no row stored and nothing rolled back or retried; the key item stays done.
func TestEnableVIPFailedStep(t *testing.T) {
	v := newVIPEnableRig(t, func(c *dockertest.FakeConfig) { c.WriteError = "the manager refused the write: boom" }, nil)
	job := v.enableVIP(ingressdeploy.VIPRequest{Address: vipTestAddress})
	if job.State == jobs.JobDone {
		t.Fatalf("the job ended done: %+v", job.Items)
	}
	want := map[string]struct{ state, code string }{
		ingressdeploy.ItemVIPKey:     {jobs.ItemDone, ""},
		ingressdeploy.ItemVIPSecret:  {jobs.ItemFailed, ingressdeploy.CodeVIPSecretFailed},
		ingressdeploy.ItemVIPService: {jobs.ItemSkipped, ingressdeploy.CodeNotRun},
		ingressdeploy.ItemVIPSwitch:  {jobs.ItemSkipped, ingressdeploy.CodeNotRun},
	}
	for id, w := range want {
		if it := itemOf(job, id); it.State != w.state || it.Code != w.code {
			t.Errorf("item %s = %q (%q), want %q (%q)", id, it.State, it.Code, w.state, w.code)
		}
	}
	if it := itemOf(job, ingressdeploy.ItemVIPSecret); strings.Contains(it.Message, "boom") || strings.Contains(it.Detail, "boom") {
		t.Errorf("the secret item shows the engine's words: %+v", it)
	}
	if _, err := v.istore.GetVIP(testsupport.Context(t), v.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("a failed enable stored a row: %v", err)
	}
	if n := len(v.fake.ServiceCreates()); n != 0 {
		t.Errorf("a failed secret step went on to create %d services", n)
	}
	if v.auditCount("ingress.vip_enabled") != 0 {
		t.Error("a failed enable wrote an audit row")
	}
	if _, held := v.locks.Held(v.envID); held {
		t.Error("the lock is held after the failed job")
	}
}

// TestEnableVIPStartsOverAfterAFailedService: a service step that fails leaves the secret behind; switching on again removes it,
// makes a new key and secret, and finishes, because no row means nothing else relies on that secret.
func TestEnableVIPStartsOverAfterAFailedService(t *testing.T) {
	v := newVIPEnableRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the manager cannot create the service" }, nil)
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}
	job := v.enableVIP(req)
	if it := itemOf(job, ingressdeploy.ItemVIPService); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPServiceFailed {
		t.Fatalf("service item = %q (%q), want failed with vip_service_failed", it.State, it.Code)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPSwitch); it.State != jobs.ItemSkipped {
		t.Errorf("switch item = %q, want skipped", it.State)
	}

	v.fake.Reconfigure(v.base)
	wantVIPAllDone(t, v.enableVIP(req))
	name := ingressdeploy.KeepalivedSecretName(v.envID, 1)
	n := 0
	for _, s := range v.secrets() {
		if s.Name == name {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the swarm holds %d secrets named %s, want 1", n, name)
	}
	if _, err := v.istore.GetVIP(testsupport.Context(t), v.envID); err != nil {
		t.Errorf("GetVIP after the second enable: %v", err)
	}
}
