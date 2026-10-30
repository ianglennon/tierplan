// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/proto/wire"
)

// composeReplayRequest is one request of the corpus: a request the Compose SDK sent to a real engine for a
// standalone deploy (plan 07-21). internal/stackops/standalone_integration_test.go records it (with
// COAMING_UPDATE_REPLAY=1) from the real SDK, running a stack with a secret and a config through create,
// recreate, orphan removal, stop, start and down; IDs, digests and hashes are one placeholder.
type composeReplayRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Query   string          `json:"query"`
	HasBody bool            `json:"hasBody"`
	Upgrade bool            `json:"upgrade"`
	JSON    json.RawMessage `json:"json"`
	Tar     []byte          `json:"tar"`
}

// TestComposeReplayPassesTheAllowlist replays every request the SDK sent against the agent's rules: the
// route table (Allowed), the body check of the body kind the route takes (validateBody) and, for an archive
// write, the check of the container it is for (archiveTargetOK) with a lookup that reports a compose-labelled
// container that is not running, which is what the SDK writes into. Every request is allowed. A request the
// SDK sends that the allowlist refuses would fail a standalone deploy through an agent, so it is found here
// first, and fixed with a rule and a vector in the route table (STK-02, T-07-101).
func TestComposeReplayPassesTheAllowlist(t *testing.T) {
	raw, err := os.ReadFile("testdata/compose-replay.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var corpus []composeReplayRequest
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("the corpus is not readable: %v", err)
	}
	if len(corpus) < 20 {
		t.Fatalf("the corpus holds %d requests, want the whole set a deploy sends (regenerate it with COAMING_UPDATE_REPLAY=1 go test -tags integration ./internal/stackops -run TestRecordComposeRequests)", len(corpus))
	}

	ctx := context.Background()
	// The engine has no network or volume of the names the bodies mention that would make a create a
	// host-access one.
	env := bodyEnv{
		ctx: ctx,
		lookup: func(context.Context, string) (string, string, error) {
			return "", "", nil
		},
		volumes: func(context.Context, string) (string, map[string]string, bool, error) {
			return "", nil, false, nil
		},
	}
	composeContainer := func(context.Context, string) (map[string]string, bool, bool, error) {
		return map[string]string{composeProjectLabel: "replay"}, false, true, nil
	}

	have := map[string]bool{}
	for i, q := range corpus {
		what := q.Method + " " + q.Path
		have[what] = true
		hasBody := len(q.JSON) > 0 || len(q.Tar) > 0
		if hasBody != q.HasBody {
			t.Errorf("corpus entry %d (%s) says hasBody %v but holds a body of %d bytes", i, what, q.HasBody, len(q.JSON)+len(q.Tar))
		}
		ok, reason, kind := Allowed(q.Method, q.Path, q.Query, q.HasBody, q.Upgrade, Policy{})
		if !ok {
			t.Errorf("%s ? %s is refused by the route table (%s), a standalone deploy through an agent would fail", what, q.Query, reason)
			continue
		}
		// The same request on a read-only host is refused whenever it changes anything.
		if ro, why, _ := Allowed(q.Method, q.Path, q.Query, q.HasBody, q.Upgrade, Policy{ReadOnly: true}); ro != (q.Method == http.MethodGet || q.Method == http.MethodHead) {
			t.Errorf("%s on a read-only host: allowed %v (%s), want it allowed only for a read", what, ro, why)
		}
		if kind == BodyNone {
			continue
		}
		body := []byte(q.JSON)
		if kind == BodyArchive {
			body = q.Tar
		}
		if _, err := validateBody(kind, bytes.NewReader(body), env); err != nil {
			t.Errorf("%s: the body is refused: %v", what, err)
			continue
		}
		if kind == BodyArchive {
			if err := archiveTargetOK(ctx, archiveContainerID(q.Path), composeContainer); err != nil {
				t.Errorf("%s: the container is refused: %v", what, err)
			}
		}
	}

	// A corpus that lacks the requests the plan names proves nothing about the allowlist.
	for _, want := range []string{
		"POST /containers/create", "POST /networks/create", "POST /volumes/create", "POST /images/create",
		"PUT /containers/" + strings.Repeat("a", 64) + "/archive", "POST /containers/" + strings.Repeat("a", 64) + "/rename",
		"POST /containers/" + strings.Repeat("a", 64) + "/start", "POST /containers/" + strings.Repeat("a", 64) + "/stop",
		"DELETE /containers/" + strings.Repeat("a", 64), "DELETE /networks/" + strings.Repeat("a", 64),
	} {
		if !have[want] {
			t.Errorf("the corpus holds no %s", want)
		}
	}
	// A request the SDK never sends for a deploy, an image build, is refused by the allowlist with the typed
	// reason the server reads.
	if ok, reason, _ := Allowed(http.MethodPost, "/build", "", true, false, Policy{}); ok || reason != wire.DeniedAllowlist {
		t.Errorf("an image build = allowed %v reason %q, want it refused by the allowlist", ok, reason)
	}
}
