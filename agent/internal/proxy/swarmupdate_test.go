// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
)

// engineSwarmSpec is the swarm spec the fake engine reports in /info: every setting that is not the
// task history limit has a value that is not the zero value, so a forwarded spec that dropped one
// of them differs from it.
func engineSwarmSpec(limit int64) swarm.Spec {
	keep := uint64(2)
	return swarm.Spec{
		Annotations: swarm.Annotations{Name: "default", Labels: map[string]string{"env": "prod"}},
		Orchestration: swarm.OrchestrationConfig{
			TaskHistoryRetentionLimit: &limit,
		},
		Raft: swarm.RaftConfig{
			SnapshotInterval: 10000, KeepOldSnapshots: &keep, LogEntriesForSlowFollowers: 500,
			ElectionTick: 10, HeartbeatTick: 1,
		},
		Dispatcher: swarm.DispatcherConfig{HeartbeatPeriod: 5 * time.Second},
		CAConfig:   swarm.CAConfig{NodeCertExpiry: 90 * 24 * time.Hour},
		TaskDefaults: swarm.TaskDefaults{
			LogDriver: &swarm.Driver{Name: "json-file", Options: map[string]string{"max-size": "10m"}},
		},
		EncryptionConfig: swarm.EncryptionConfig{AutoLockManagers: false},
	}
}

// infoAnswer is what the fake engine answers GET /info with: a swarm manager with the given spec.
func infoAnswer(t *testing.T, spec swarm.Spec, manager bool) string {
	t.Helper()
	info := struct{ Swarm swarm.Info }{Swarm: swarm.Info{
		NodeID: "n1", LocalNodeState: swarm.LocalNodeStateActive, ControlAvailable: manager,
		Cluster: &swarm.ClusterInfo{ID: "c1", Meta: swarm.Meta{Version: swarm.Version{Index: 42}}, Spec: spec},
	}}
	return enc(t, info)
}

// swarmUpdateRequest is a swarm update as the server's client sends it.
func swarmUpdateRequest(t *testing.T, query string, spec swarm.Spec) string {
	t.Helper()
	return postJSON("POST", "/swarm/update"+query, enc(t, spec))
}

// updatesSeen is the swarm updates the engine received, whatever else it was asked.
func updatesSeen(f *fakeSocket) []string {
	var out []string
	for _, r := range f.requests() {
		if strings.Contains(r, " /swarm") || strings.Contains(r, "/swarm/") {
			out = append(out, r)
		}
	}
	return out
}

// engineSpecWithLimit is the spec a server would send to set the limit: the engine's spec read from
// /info with the one field changed.
func engineSpecWithLimit(limit int64) swarm.Spec {
	spec := engineSwarmSpec(5)
	spec.Orchestration.TaskHistoryRetentionLimit = &limit
	return spec
}

func TestSwarmUpdateForwardsTheOneField(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
	h := Handler(sock.path, Policy{}, nil)

	resp := do(t, h, swarmUpdateRequest(t, "?version=42", engineSpecWithLimit(10)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	updates := updatesSeen(sock)
	if len(updates) != 1 || updates[0] != "POST /swarm/update?version=42" {
		t.Fatalf("the engine saw the swarm requests %v, want exactly POST /swarm/update?version=42", updates)
	}
	// The engine was asked its own spec once, by the agent, and nothing else.
	if got := sock.requests(); len(got) != 2 || got[0] != "GET /info" {
		t.Fatalf("the engine saw %v, want GET /info then the update", got)
	}
	var got, want swarm.Spec
	bodies := sock.bodiesSeen()
	if err := json.Unmarshal(bodies[len(bodies)-1], &got); err != nil {
		t.Fatalf("the forwarded body does not decode: %v: %s", err, bodies[len(bodies)-1])
	}
	want = engineSwarmSpec(10)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the engine received %s, want its own spec with the limit 10", bodies[len(bodies)-1])
	}
	if ct := sock.headersSeen()[1].Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q, want application/json", ct)
	}
}

// TestSwarmUpdateForwardsTheEnginesBytesNotTheServers: a body that spells the same spec another way
// (a different field order, an empty label map) is forwarded as the engine's own spec, never as the
// bytes the server sent.
func TestSwarmUpdateForwardsTheEnginesBytesNotTheServers(t *testing.T) {
	sock := newFakeSocket(t)
	engine := engineSwarmSpec(5)
	engine.Labels = nil
	sock.answers = map[string]string{"/info": infoAnswer(t, engine, true)}
	h := Handler(sock.path, Policy{}, nil)

	// The same spec as the engine's, written with the labels as an empty map and the fields in
	// another order.
	body := `{"Orchestration":{"TaskHistoryRetentionLimit":7},"Labels":{},"Name":"default",` +
		`"EncryptionConfig":{"AutoLockManagers":false},` +
		`"TaskDefaults":{"LogDriver":{"Name":"json-file","Options":{"max-size":"10m"}}},` +
		`"CAConfig":{"NodeCertExpiry":7776000000000000},"Dispatcher":{"HeartbeatPeriod":5000000000},` +
		`"Raft":{"SnapshotInterval":10000,"KeepOldSnapshots":2,"LogEntriesForSlowFollowers":500,"ElectionTick":10,"HeartbeatTick":1}}`
	resp := do(t, h, postJSON("POST", "/swarm/update?version=42", body))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	bodies := sock.bodiesSeen()
	sent := bodies[len(bodies)-1]
	if string(sent) == body {
		t.Fatalf("the engine received the bytes the server sent: %s", sent)
	}
	want := engine
	seven := int64(7)
	want.Orchestration.TaskHistoryRetentionLimit = &seven
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(sent) != string(wantBytes) {
		t.Errorf("the engine received %s, want %s", sent, wantBytes)
	}
}

func TestSwarmUpdateRefusesRotation(t *testing.T) {
	spec := engineSpecWithLimit(10)
	for _, tc := range []struct{ name, query string }{
		{"rotate the worker token", "?version=42&rotateWorkerToken=true"},
		{"rotate the manager token", "?version=42&rotateManagerToken=true"},
		{"rotate the unlock key", "?version=42&rotateManagerUnlockKey=true"},
		{"rotate the worker token, false", "?version=42&rotateWorkerToken=false"},
		{"every rotation", "?version=42&rotateWorkerToken=1&rotateManagerToken=1&rotateManagerUnlockKey=1"},
		{"any other key", "?version=42&force=1"},
		{"a key spelled with escapes", "?version=42&%72otateWorkerToken=true"},
		{"no version", ""},
		{"a version with no value", "?version="},
		{"a version that is not digits", "?version=abc"},
		{"a negative version", "?version=-1"},
		{"a version with a trailing key", "?version=42x"},
		{"a 21 digit version", "?version=" + strings.Repeat("1", 21)},
		{"the only key but not version", "?rotateWorkerToken=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, swarmUpdateRequest(t, tc.query, spec))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			if got := sock.requests(); len(got) != 0 {
				t.Fatalf("a refused request reached the engine: %v", got)
			}
			if len(reasons) != 1 || reasons[0] != "allowlist" {
				t.Errorf("the refusal reasons were %v, want one allowlist", reasons)
			}
		})
	}
}

func TestSwarmUpdateRefusesAnyOtherChange(t *testing.T) {
	keep := uint64(9)
	for _, tc := range []struct {
		name   string
		change func(*swarm.Spec)
	}{
		{"Raft.SnapshotInterval", func(s *swarm.Spec) { s.Raft.SnapshotInterval = 1 }},
		{"Raft.KeepOldSnapshots", func(s *swarm.Spec) { s.Raft.KeepOldSnapshots = &keep }},
		{"Raft.LogEntriesForSlowFollowers", func(s *swarm.Spec) { s.Raft.LogEntriesForSlowFollowers = 1 }},
		{"Raft.ElectionTick", func(s *swarm.Spec) { s.Raft.ElectionTick = 20 }},
		{"Raft.HeartbeatTick", func(s *swarm.Spec) { s.Raft.HeartbeatTick = 2 }},
		{"Dispatcher.HeartbeatPeriod", func(s *swarm.Spec) { s.Dispatcher.HeartbeatPeriod = time.Hour }},
		{"CAConfig.NodeCertExpiry", func(s *swarm.Spec) { s.CAConfig.NodeCertExpiry = time.Hour }},
		{"CAConfig.ForceRotate", func(s *swarm.Spec) { s.CAConfig.ForceRotate = 1 }},
		{"CAConfig.SigningCACert", func(s *swarm.Spec) { s.CAConfig.SigningCACert = "-----BEGIN CERTIFICATE-----" }},
		{"CAConfig.SigningCAKey", func(s *swarm.Spec) { s.CAConfig.SigningCAKey = "-----BEGIN EC PRIVATE KEY-----" }},
		{"CAConfig.ExternalCAs", func(s *swarm.Spec) {
			s.CAConfig.ExternalCAs = []*swarm.ExternalCA{{Protocol: swarm.ExternalCAProtocolCFSSL, URL: "https://ca.example", CACert: "x"}}
		}},
		{"Annotations.Name", func(s *swarm.Spec) { s.Name = "other" }},
		{"Annotations.Labels, added", func(s *swarm.Spec) { s.Labels = map[string]string{"env": "prod", "x": "y"} }},
		{"Annotations.Labels, changed", func(s *swarm.Spec) { s.Labels = map[string]string{"env": "dev"} }},
		{"Annotations.Labels, removed", func(s *swarm.Spec) { s.Labels = nil }},
		{"EncryptionConfig.AutoLockManagers", func(s *swarm.Spec) { s.EncryptionConfig.AutoLockManagers = true }},
		{"TaskDefaults.LogDriver, removed", func(s *swarm.Spec) { s.TaskDefaults.LogDriver = nil }},
		{"TaskDefaults.LogDriver, changed", func(s *swarm.Spec) { s.TaskDefaults.LogDriver = &swarm.Driver{Name: "syslog"} }},
		{"TaskDefaults.LogDriver.Options", func(s *swarm.Spec) { s.TaskDefaults.LogDriver.Options = map[string]string{"max-size": "1g"} }},
		{"the whole of the other settings, zeroed", func(s *swarm.Spec) {
			lim := s.Orchestration
			*s = swarm.Spec{Orchestration: lim}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			spec := engineSpecWithLimit(10)
			// A copy of the driver, so a change to its options does not change the engine's spec.
			if d := spec.TaskDefaults.LogDriver; d != nil {
				spec.TaskDefaults.LogDriver = &swarm.Driver{Name: d.Name, Options: map[string]string{"max-size": "10m"}}
			}
			tc.change(&spec)
			resp := do(t, h, swarmUpdateRequest(t, "?version=42", spec))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			if len(reasons) != 1 || reasons[0] != "allowlist" {
				t.Errorf("the refusal reasons were %v, want one allowlist", reasons)
			}
			if updates := updatesSeen(sock); len(updates) != 0 {
				t.Fatalf("a refused update reached the engine: %v", updates)
			}
		})
	}
}

func TestSwarmUpdateLimitRange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit *int64
		want  bool // forwarded
	}{
		{"absent", nil, false},
		{"minus one", ptr(int64(-1)), false},
		{"minus a lot", ptr(int64(-1 << 40)), false},
		{"1001", ptr(int64(1001)), false},
		{"far above", ptr(int64(1 << 40)), false},
		{"zero", ptr(int64(0)), true},
		{"one", ptr(int64(1)), true},
		{"1000", ptr(int64(1000)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
			h := Handler(sock.path, Policy{}, nil)
			spec := engineSwarmSpec(5)
			spec.Orchestration.TaskHistoryRetentionLimit = tc.limit
			resp := do(t, h, swarmUpdateRequest(t, "?version=42", spec))
			_ = resp.Body.Close()
			forwarded := len(updatesSeen(sock)) == 1
			if forwarded != tc.want {
				t.Fatalf("forwarded = %v, want %v (status %d)", forwarded, tc.want, resp.StatusCode)
			}
			if !tc.want && resp.StatusCode != http.StatusForbidden {
				t.Errorf("status %d, want 403", resp.StatusCode)
			}
			if !tc.want && len(sock.requests()) != 0 {
				t.Errorf("a body outside the range made the agent call the engine: %v", sock.requests())
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestSwarmUpdateStrictBody(t *testing.T) {
	good := enc(t, engineSpecWithLimit(10))
	// good is an object; an unknown field is added by hand, once at the top and once nested.
	topLevel := strings.TrimSuffix(good, "}") + `,"JoinTokens":{"Worker":"x","Manager":"y"}}`
	nested := strings.Replace(good, `"Raft":{`, `"Raft":{"Unknown":1,`, 1)
	for name, body := range map[string]string{
		"an unknown top-level field": topLevel,
		"an unknown nested field":    nested,
		"not an object":              `[1,2]`,
		"a string":                   `"x"`,
		"data after the value":       good + ` {}`,
		"an empty body":              ``,
		"a limit that is a string":   `{"Orchestration":{"TaskHistoryRetentionLimit":"10"}}`,
		"a limit that is a float":    `{"Orchestration":{"TaskHistoryRetentionLimit":1.5}}`,
	} {
		t.Run(name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, postJSON("POST", "/swarm/update?version=42", body))
			_ = resp.Body.Close()
			// An empty body is refused as no body at all; every other one by the validator.
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			if updates := updatesSeen(sock); len(updates) != 0 {
				t.Fatalf("a refused update reached the engine: %v", updates)
			}
			if len(reasons) != 1 {
				t.Errorf("the refusal reasons were %v, want one", reasons)
			}
		})
	}
	// The body kind refuses what the validator is handed with no lookup at all.
	if _, err := validateBody(BodySwarmUpdate, strings.NewReader(good), bodyEnv{}); err == nil {
		t.Error("a swarm update was accepted with no swarm lookup")
	}
	if got := bodyCap(BodySwarmUpdate); got != maxBody {
		t.Errorf("the body cap is %d, want %d", got, maxBody)
	}
}

func TestSwarmUpdateLookupFailureIsNotPermission(t *testing.T) {
	spec := engineSpecWithLimit(10)
	for _, tc := range []struct {
		name    string
		answers map[string]string
		status  map[string]int
	}{
		{"the engine answers 500", map[string]string{"/info": `{"message":"boom"}`}, map[string]int{"/info": http.StatusInternalServerError}},
		{"the engine answers 503", map[string]string{"/info": `{"message":"down"}`}, map[string]int{"/info": http.StatusServiceUnavailable}},
		{"the engine answers something that is not JSON", map[string]string{"/info": `<html>`}, nil},
		{"the engine answers an empty body", map[string]string{"/info": ``}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers, sock.status = tc.answers, tc.status
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, swarmUpdateRequest(t, "?version=42", spec))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status %d, want 502", resp.StatusCode)
			}
			var body struct{ Message string }
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("the answer is not JSON: %v", err)
			}
			if !strings.HasPrefix(body.Message, "agent_engine_unreachable") {
				t.Errorf("message %q, want agent_engine_unreachable", body.Message)
			}
			if updates := updatesSeen(sock); len(updates) != 0 {
				t.Fatalf("the update was forwarded though the engine's spec could not be read: %v", updates)
			}
			if len(reasons) != 0 {
				t.Errorf("an engine fault was reported as a refusal: %v", reasons)
			}
		})
	}
}

// TestSwarmUpdateEngineThatIsGoneIsNotPermission: the engine's socket answers nothing at all.
func TestSwarmUpdateEngineThatIsGoneIsNotPermission(t *testing.T) {
	h := Handler(t.TempDir()+"/none.sock", Policy{}, nil)
	resp := do(t, h, swarmUpdateRequest(t, "?version=42", engineSpecWithLimit(10)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

func TestSwarmUpdateWorkerRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(t *testing.T) string
	}{
		{"a worker", func(t *testing.T) string { return infoAnswer(t, engineSwarmSpec(5), false) }},
		{"not in a swarm", func(*testing.T) string {
			return `{"Swarm":{"NodeID":"","LocalNodeState":"inactive","ControlAvailable":false}}`
		}},
		{"a manager with no cluster in the answer", func(*testing.T) string {
			return `{"Swarm":{"LocalNodeState":"active","ControlAvailable":true}}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/info": tc.answer(t)}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, swarmUpdateRequest(t, "?version=42", engineSpecWithLimit(10)))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			if updates := updatesSeen(sock); len(updates) != 0 {
				t.Fatalf("the update was forwarded: %v", updates)
			}
			if len(reasons) != 1 || reasons[0] != "allowlist" {
				t.Errorf("the refusal reasons were %v, want one allowlist", reasons)
			}
		})
	}
}

// TestValidateSwarmUpdateJudgesTheLookupItself holds the validator to its own rules with a lookup the
// test controls: a node that is not a manager is refused even when its spec is the one submitted, an
// engine fault is its own error and never a refusal, and an accepted body is the engine's spec.
func TestValidateSwarmUpdateJudgesTheLookupItself(t *testing.T) {
	body := []byte(enc(t, engineSpecWithLimit(10)))
	lookupOf := func(spec swarm.Spec, manager bool, err error) bodyEnv {
		return bodyEnv{swarm: func(context.Context) (swarm.Spec, bool, error) { return spec, manager, err }}
	}

	if _, err := validateSwarmUpdate(body, lookupOf(engineSwarmSpec(5), false, nil)); !errors.Is(err, errBodyRefused) {
		t.Errorf("a node that is not a manager: err = %v, want a refusal", err)
	}

	boom := errors.New("boom")
	_, err := validateSwarmUpdate(body, lookupOf(engineSwarmSpec(5), true, boom))
	if !errors.Is(err, errSwarmLookup) || !errors.Is(err, boom) || errors.Is(err, errBodyRefused) {
		t.Errorf("an engine fault: err = %v, want errSwarmLookup wrapping the fault and not a refusal", err)
	}

	got, err := validateSwarmUpdate(body, lookupOf(engineSwarmSpec(5), true, nil))
	if err != nil {
		t.Fatalf("the engine's spec with the limit changed was refused: %v", err)
	}
	if want := engineSwarmSpec(10); !reflect.DeepEqual(got, want) {
		t.Errorf("returned %+v, want %+v", got, want)
	}
	// The value returned shares no pointer with the engine's own spec, so a later change of one
	// cannot move the other.
	current := engineSwarmSpec(5)
	got, err = validateSwarmUpdate(body, lookupOf(current, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Orchestration.TaskHistoryRetentionLimit == current.Orchestration.TaskHistoryRetentionLimit {
		t.Error("the returned limit is the engine's own pointer")
	}
	if *current.Orchestration.TaskHistoryRetentionLimit != 5 {
		t.Errorf("the engine's spec was changed to %d", *current.Orchestration.TaskHistoryRetentionLimit)
	}
}

func TestSwarmUpdateReadOnlyAgentRefusesIt(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true}, func(_, _, reason string) { reasons = append(reasons, reason) })
	resp := do(t, h, swarmUpdateRequest(t, "?version=42", engineSpecWithLimit(10)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a write reached the engine of a read-only host: %v", got)
	}
	if !slices.Equal(reasons, []string{"read_only_flag"}) {
		t.Errorf("the refusal reasons were %v, want read_only_flag", reasons)
	}
}

// FuzzSwarmUpdate: whatever body is submitted, what the validator returns is the engine's own spec
// with a limit in range, and nothing else of the submitted bytes survives.
func FuzzSwarmUpdate(f *testing.F) {
	engine := engineSwarmSpec(5)
	for _, seed := range []string{
		enc2(engineSpecWithLimit(10)),
		enc2(engineSpecWithLimit(0)),
		enc2(engineSpecWithLimit(1000)),
		enc2(engineSpecWithLimit(1001)),
		enc2(engineSpecWithLimit(-1)),
		`{}`,
		`{"Orchestration":{"TaskHistoryRetentionLimit":3}}`,
		`{"Orchestration":{"TaskHistoryRetentionLimit":3},"CAConfig":{"ForceRotate":1}}`,
		`{"JoinTokens":{"Worker":"x"}}`,
		`null`,
		``,
	} {
		f.Add(seed)
	}
	env := bodyEnv{swarm: func(context.Context) (swarm.Spec, bool, error) { return engine, true, nil }}
	f.Fuzz(func(t *testing.T, body string) {
		got, err := validateSwarmUpdate([]byte(body), env)
		if err != nil {
			return
		}
		limit := got.Orchestration.TaskHistoryRetentionLimit
		if limit == nil || *limit < minTaskHistoryLimit || *limit > maxTaskHistoryLimit {
			t.Fatalf("a spec with the limit %v was returned for %.200s", limit, body)
		}
		// Everything but the limit is the engine's own.
		got.Orchestration.TaskHistoryRetentionLimit = engine.Orchestration.TaskHistoryRetentionLimit
		if !reflect.DeepEqual(got, engine) {
			t.Fatalf("a spec that differs from the engine's was returned for %.200s", body)
		}
	})
}

func enc2(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
