// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package audit_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
)

// ingressRow is one line of the ingress audit table (Phase 27, D-23, A-01): an ingress.* action, the verb of success
// criterion 5 it answers to, who writes it, how its failure is told, the detail keys it records and the tests that
// prove it.
//
// Class says what a failed attempt leaves behind:
//   - F: the failure is a row of the same action with Result failure; Failure names the test that proves it.
//   - P: the action has a failure half of its own (a different action, or a failure row); Failure names the test that
//     proves it, or FailureOf names the action this one is the failure of.
//   - A: atomic. A refusal or a failed attempt changes nothing and writes no row (a denied call is permission.denied);
//     Reason says why.
//
// A drain_handoff move is preceded by an ingress.vip_handoff row; that overlap is intended, so both rows exist.
type ingressRow struct {
	Action      string
	Verb        string
	Actor       string
	Class       string
	DetailKeys  []string // recorded on success
	FailureKeys []string // recorded on a failure or refusal, where it is recorded at all
	Proofs      []string // top-level test funcs that provoke the success row
	Failure     string   // the test func that provokes the failure row (F and P)
	FailureOf   []string // the actions this one is the failure half of (P)
	Reason      string   // why a failed attempt writes no row (A)
}

// checkTable compares the actions found in audit.go with the rows of the table. missing lists the actions without a
// row and unknown the rows without an action; both are sorted and without duplicates.
func checkTable(actions []string, rows []ingressRow) (missing, unknown []string) {
	have := make(map[string]bool, len(actions))
	for _, a := range actions {
		have[a] = true
	}
	listed := make(map[string]bool, len(rows))
	for _, r := range rows {
		listed[r.Action] = true
	}
	for a := range have {
		if !listed[a] {
			missing = append(missing, a)
		}
	}
	for a := range listed {
		if !have[a] {
			unknown = append(unknown, a)
		}
	}
	slices.Sort(missing)
	slices.Sort(unknown)
	return missing, unknown
}

// ingressActions are the rows of the table, one per ingress.* constant of audit.go.
var ingressActions = []ingressRow{
	{
		Action: "ingress.claimed", Verb: "claim", Actor: "user or system", Class: "F",
		DetailKeys: []string{"stack", "hostnames", "count"}, FailureKeys: []string{"stack", "hostnames", "count"},
		Proofs: []string{"TestAuditProofClaimed"}, Failure: "TestAuditProofClaimedFailure",
	},
	{
		Action: "ingress.claim_replaced", Verb: "claim", Actor: "user or system", Class: "F",
		DetailKeys: []string{"stack", "hostnames", "count"}, FailureKeys: []string{"stack", "hostnames", "count"},
		Proofs: []string{"TestAuditProofClaimReplaced"}, Failure: "TestAuditProofClaimReplacedFailure",
	},
	{
		Action: "ingress.claim_released", Verb: "release", Actor: "user or system", Class: "A",
		DetailKeys: []string{"stack", "count"},
		Proofs:     []string{"TestAuditProofClaimReleased"},
		Reason:     "atomic: a release is never refused by policy; a store error rolls it back and the remove or deploy row carries the failure",
	},
	{
		Action: "ingress.cert_issued", Verb: "issuance", Actor: "system", Class: "P",
		DetailKeys: []string{"hostname", "issuer"},
		Proofs:     []string{"TestAuditProofCertIssued"}, Failure: "TestAuditProofCertFailed",
	},
	{
		Action: "ingress.cert_failed", Verb: "issuance", Actor: "system", Class: "P",
		DetailKeys: []string{"hostname", "issuer", "reason"},
		Proofs:     []string{"TestAuditProofCertFailed"},
		FailureOf:  []string{"ingress.cert_issued", "ingress.cert_renewed"},
	},
	{
		Action: "ingress.cert_requested", Verb: "issuance", Actor: "user", Class: "A",
		DetailKeys: []string{"hostname", "issuer"},
		Proofs:     []string{"TestAuditProofCertRequested"},
		Reason:     "atomic: a refused request is a 4xx that changes nothing",
	},
	{
		Action: "ingress.cert_renewed", Verb: "renewal", Actor: "system", Class: "P",
		DetailKeys: []string{"hostname", "issuer"},
		Proofs:     []string{"TestAuditProofCertRenewed"}, Failure: "TestAuditProofCertFailed",
	},
	{
		Action: "ingress.cert_retried", Verb: "renewal", Actor: "user", Class: "A",
		DetailKeys: []string{"hostname"},
		Proofs:     []string{"TestAuditProofCertRetried"},
		Reason:     "atomic: a refused retry changes nothing",
	},
	{
		Action: "ingress.cert_uploaded", Verb: "upload", Actor: "user", Class: "F",
		DetailKeys: []string{"hostname", "replaced", "warning"}, FailureKeys: []string{"hostname", "code"},
		Proofs: []string{"TestAuditProofCertUploaded"}, Failure: "TestAuditProofCertUploadedFailure",
	},
	{
		Action: "ingress.cert_switched_to_acme", Verb: "upload", Actor: "user", Class: "A",
		DetailKeys: []string{"hostname", "issuer"},
		Proofs:     []string{"TestAuditProofCertSwitchedToACME"},
		Reason:     "atomic: a refused switch-back changes nothing",
	},
	{
		Action: "ingress.cert_deleted", Verb: "other", Actor: "system", Class: "A",
		DetailKeys: []string{"hostname", "reason", "source"},
		Proofs:     []string{"TestAuditProofCertDeleted"},
		Reason:     "atomic: a failed delete changes nothing and is retried next pass",
	},
	{
		Action: "ingress.cert_moved", Verb: "other", Actor: "system", Class: "A",
		DetailKeys: []string{"hostname", "from_env", "to_env", "from_domain", "to_domain", "outcome", "cause"},
		Proofs:     []string{"TestAuditProofCertMoved"},
		Reason:     "atomic: a failed move changes nothing and is retried",
	},
	{
		Action: "ingress.node_enrolled", Verb: "enrolment", Actor: "system", Class: "P",
		DetailKeys: []string{"node", "swarm_node", "repeated"},
		Proofs:     []string{"TestAuditProofNodeEnrolled"}, Failure: "TestAuditProofNodeEnrolRefused",
	},
	{
		Action: "ingress.node_enrol_refused", Verb: "enrolment", Actor: "anonymous", Class: "P",
		DetailKeys: []string{"code", "mode"},
		Proofs:     []string{"TestAuditProofNodeEnrolRefused"},
		FailureOf:  []string{"ingress.node_enrolled"},
	},
	{
		Action: "ingress.node_revoked", Verb: "revocation", Actor: "user", Class: "A",
		DetailKeys: []string{"node", "swarm_node", "held", "reenrol", "reason", "sample"},
		Proofs:     []string{"TestAuditProofNodeRevoked"},
		Reason:     "atomic: a refused revoke changes nothing",
	},
	{
		Action: "ingress.node_removed", Verb: "revocation", Actor: "system or user", Class: "A",
		DetailKeys: []string{"node"},
		Proofs:     []string{"TestAuditProofNodeRemoved"},
		Reason:     "atomic",
	},
	{
		Action: "ingress.enabled", Verb: "switch", Actor: "user", Class: "F",
		DetailKeys: []string{"subnet", "image"}, FailureKeys: []string{"step", "code"},
		Proofs: []string{"TestAuditProofEnabled"}, Failure: "TestAuditProofEnabledFailure",
	},
	{
		Action: "ingress.disabled", Verb: "switch", Actor: "user", Class: "F",
		DetailKeys: []string{}, FailureKeys: []string{"step", "code"},
		Proofs: []string{"TestAuditProofDisabled"}, Failure: "TestAuditProofDisabledFailure",
	},
	{
		Action: "ingress.vip_enabled", Verb: "switch", Actor: "user", Class: "F",
		DetailKeys: []string{"address", "router_id", "generation"}, FailureKeys: []string{"step", "code"},
		Proofs: []string{"TestAuditProofVIPEnabled"}, Failure: "TestAuditProofVIPEnabledFailure",
	},
	{
		Action: "ingress.vip_changed", Verb: "switch", Actor: "user", Class: "F",
		DetailKeys: []string{"address", "router_id"}, FailureKeys: []string{"step", "code"},
		Proofs: []string{"TestAuditProofVIPChanged"}, Failure: "TestAuditProofVIPChangedFailure",
	},
	{
		Action: "ingress.vip_disabled", Verb: "switch", Actor: "user", Class: "F",
		DetailKeys: []string{"address", "router_id", "generation"}, FailureKeys: []string{"step", "code"},
		Proofs: []string{"TestAuditProofVIPDisabled"}, Failure: "TestAuditProofVIPDisabledFailure",
	},
	{
		Action: "ingress.access_logs_changed", Verb: "switch", Actor: "user", Class: "A",
		DetailKeys: []string{"state"},
		Proofs:     []string{"TestAuditProofAccessLogsChanged"},
		Reason:     "atomic: the store write is one transaction",
	},
	{
		Action: "ingress.vip_moved", Verb: "vip_move", Actor: "system", Class: "A",
		DetailKeys: []string{"from", "to", "reason"},
		Proofs:     []string{"TestAuditProofVIPMoved"},
		Reason:     "atomic: a move that cannot be stored is not recorded and is read again next pass",
	},
	{
		Action: "ingress.vip_handoff", Verb: "vip_move", Actor: "user", Class: "P",
		DetailKeys: []string{"node", "holder", "outcome"}, FailureKeys: []string{"node", "holder", "outcome"},
		Proofs: []string{"TestAuditProofVIPHandoff"}, Failure: "TestAuditProofVIPHandoffFailure",
	},
	{
		Action: "ingress.domain_created", Verb: "other", Actor: "user", Class: "P",
		DetailKeys: []string{"suffix", "issuer", "provider", "credential", "reason"}, FailureKeys: []string{"suffix", "issuer", "provider"},
		Proofs: []string{"TestAuditProofDomainCreated"}, Failure: "TestAuditProofDomainCreatedFailure",
	},
	{
		Action: "ingress.domain_credential_replaced", Verb: "other", Actor: "user", Class: "P",
		DetailKeys: []string{"suffix", "credential", "reason"}, FailureKeys: []string{"suffix"},
		Proofs: []string{"TestAuditProofDomainCredentialReplaced"}, Failure: "TestAuditProofDomainCredentialReplacedFailure",
	},
	{
		Action: "ingress.domain_deleted", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"suffix", "issuer", "certificates"},
		Proofs:     []string{"TestAuditProofDomainDeleted"}, Reason: "atomic",
	},
	{
		Action: "ingress.host_reserved", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"suffix", "hostname"},
		Proofs:     []string{"TestAuditProofHostReserved"}, Reason: "atomic",
	},
	{
		Action: "ingress.host_unreserved", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"suffix", "hostname"},
		Proofs:     []string{"TestAuditProofHostUnreserved"}, Reason: "atomic",
	},
	{
		Action: "ingress.domain_issuer_changed", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"suffix", "issuer", "previous", "certificates", "uploads"},
		Proofs:     []string{"TestAuditProofDomainIssuerChanged"}, Reason: "atomic",
	},
	{
		Action: "ingress.join_token_created", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"generation"},
		Proofs:     []string{"TestAuditProofJoinTokenCreated"}, Reason: "atomic",
	},
	{
		Action: "ingress.join_token_rotated", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"generation"},
		Proofs:     []string{"TestAuditProofJoinTokenRotated"}, Reason: "atomic",
	},
	{
		Action: "ingress.join_token_disabled", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{"generation"},
		Proofs:     []string{"TestAuditProofJoinTokenDisabled"}, Reason: "atomic",
	},
	{
		Action: "ingress.wipe_queued", Verb: "other", Actor: "system", Class: "A",
		DetailKeys: []string{"node"},
		Proofs:     []string{"TestAuditProofWipeQueued"}, Reason: "atomic",
	},
	{
		Action: "ingress.wipe_done", Verb: "other", Actor: "system", Class: "A",
		DetailKeys: []string{"node"},
		Proofs:     []string{"TestAuditProofWipeDone"}, Reason: "atomic",
	},
	{
		Action: "ingress.deploy_paused", Verb: "other", Actor: "system", Class: "A",
		DetailKeys: []string{},
		Proofs:     []string{"TestAuditProofDeployPaused"}, Reason: "atomic",
	},
	{
		Action: "ingress.deploy_resumed", Verb: "other", Actor: "user", Class: "A",
		DetailKeys: []string{},
		Proofs:     []string{"TestAuditProofDeployResumed"}, Reason: "atomic",
	},
}

// ingressConstants are the ingress.* actions among the constants of audit.go.
func ingressConstants(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, a := range definedActions(t) {
		if strings.HasPrefix(a, "ingress.") {
			out = append(out, a)
		}
	}
	return out
}

// TestIngressActionTable closes the list of ingress audit actions (D-23): one row per constant, each row stating how
// its failure is told, which keys it records and which tests prove it. A constant added without a row fails here.
func TestIngressActionTable(t *testing.T) {
	t.Run("constants", func(t *testing.T) {
		missing, unknown := checkTable(ingressConstants(t), ingressActions)
		if len(missing) > 0 {
			t.Errorf("ingress actions without a row in ingressActions: %v", missing)
		}
		if len(unknown) > 0 {
			t.Errorf("rows in ingressActions naming no ingress.* constant of audit.go: %v", unknown)
		}
		seen := map[string]bool{}
		for _, r := range ingressActions {
			if seen[r.Action] {
				t.Errorf("%s has two rows", r.Action)
			}
			seen[r.Action] = true
		}
	})

	// Success criterion 5 names these verbs; each one has at least one action.
	t.Run("verbs", func(t *testing.T) {
		for _, verb := range []string{"claim", "release", "issuance", "renewal", "upload", "enrolment", "revocation", "switch", "vip_move"} {
			if !slices.ContainsFunc(ingressActions, func(r ingressRow) bool { return r.Verb == verb }) {
				t.Errorf("no row for the verb %q", verb)
			}
		}
	})

	// A failed attempt is told by a failure proof, by the action it is the failure of, or by the reason it writes no row.
	t.Run("classes", func(t *testing.T) {
		actions := map[string]bool{}
		for _, r := range ingressActions {
			actions[r.Action] = true
		}
		for _, r := range ingressActions {
			if r.Actor == "" || r.Verb == "" || len(r.Proofs) == 0 {
				t.Errorf("%s: a row names its verb, its actor and at least one proof", r.Action)
			}
			switch r.Class {
			case "F", "P":
				if r.Failure == "" && len(r.FailureOf) == 0 {
					t.Errorf("%s (class %s): names neither a failure proof nor the action it is the failure of", r.Action, r.Class)
				}
				for _, of := range r.FailureOf {
					if !actions[of] {
						t.Errorf("%s: is the failure of %q, which has no row", r.Action, of)
					}
				}
				if r.Reason != "" {
					t.Errorf("%s (class %s): a reason for writing no row belongs to class A", r.Action, r.Class)
				}
			case "A":
				if strings.TrimSpace(r.Reason) == "" {
					t.Errorf("%s (class A): names no reason a failed attempt writes no row", r.Action)
				}
				if r.Failure != "" || len(r.FailureOf) > 0 {
					t.Errorf("%s (class A): an atomic action has no failure row", r.Action)
				}
			default:
				t.Errorf("%s: class %q is not F, P or A", r.Action, r.Class)
			}
		}
	})

	// Every key of every row, success and failure, is recorded through a real Writer: a key holding a secret word is
	// refused by it, so a row that names one fails here.
	t.Run("detail_keys", func(t *testing.T) {
		f := newFixture(t)
		recorded := 0
		for _, r := range ingressActions {
			sets := [][]string{r.DetailKeys}
			if r.FailureKeys != nil {
				sets = append(sets, r.FailureKeys)
			}
			for _, keys := range sets {
				details := make(map[string]string, len(keys))
				for _, k := range keys {
					lower := strings.ToLower(k)
					for _, w := range audittest.SecretWords() {
						if strings.Contains(lower, w) {
							t.Errorf("%s: detail key %q contains the secret word %q", r.Action, k, w)
						}
					}
					details[k] = "x"
				}
				e := ent(r.Action)
				e.Details = details
				if err := f.w.Record(context.Background(), e); err != nil {
					if errors.Is(err, audit.ErrSecretField) {
						t.Errorf("%s: a documented detail key trips the secret-word filter: %v", r.Action, err)
					} else {
						t.Errorf("%s: Record: %v", r.Action, err)
					}
					continue
				}
				recorded++
			}
		}
		if got := f.verify(t); got.Broken != nil || got.Entries != recorded {
			t.Fatalf("verify = %+v, want %d entries and an intact chain", got, recorded)
		}
	})

	// A proof is accepted by what its body does, never by its name: it must exist as a top-level func of a _test.go
	// file and call audittest.AssertNoSecrets, so that no row it provokes can hold a token or a key.
	t.Run("proofs", func(t *testing.T) {
		wanted := map[string][]string{} // proof name -> actions that name it
		for _, r := range ingressActions {
			for _, name := range append(slices.Clone(r.Proofs), r.Failure) {
				if name != "" {
					wanted[name] = append(wanted[name], r.Action)
				}
			}
		}
		for _, problem := range checkProofs(t, "../../internal", wanted) {
			t.Error(problem)
		}
	})
}

// checkProofs scans every _test.go file under root and returns one problem per wanted proof that is not a top-level
// func, or whose body does not call audittest.AssertNoSecrets. wanted maps each proof name to the actions that name it.
func checkProofs(t *testing.T, root string, wanted map[string][]string) []string {
	t.Helper()
	found := map[string]bool{}   // proof name -> declared as a top-level func
	checked := map[string]bool{} // proof name -> its body calls AssertNoSecrets
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), "func Test") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if _, want := wanted[fn.Name.Name]; !want {
				continue
			}
			found[fn.Name.Name] = true
			if callsAssertNoSecrets(fn.Body) {
				checked[fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		switch {
		case !found[name]:
			problems = append(problems, "proof "+name+" ("+strings.Join(wanted[name], ", ")+") is not a top-level func of any _test.go file")
		case !checked[name]:
			problems = append(problems, "proof "+name+" ("+strings.Join(wanted[name], ", ")+") never calls audittest.AssertNoSecrets")
		}
	}
	return problems
}

// callsAssertNoSecrets reports whether body holds a call expression audittest.AssertNoSecrets(...).
func callsAssertNoSecrets(body *ast.BlockStmt) bool {
	hit := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !hit
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AssertNoSecrets" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "audittest" {
			hit = true
		}
		return !hit
	})
	return hit
}

// TestIngressActionTableCatchesAProofThatDoesNotCheckForSecrets proves the proof scan refuses a proof that is missing
// and one that never calls the shared no-secret helper.
func TestIngressActionTableCatchesAProofThatDoesNotCheckForSecrets(t *testing.T) {
	dir := t.TempDir()
	src := `package x

import "testing"

func TestAuditProofChecked(t *testing.T)   { audittest.AssertNoSecrets(t, nil) }
func TestAuditProofUnchecked(t *testing.T) { _ = t }
`
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	problems := checkProofs(t, dir, map[string][]string{
		"TestAuditProofChecked":   {"ingress.a"},
		"TestAuditProofUnchecked": {"ingress.b"},
		"TestAuditProofGone":      {"ingress.c"},
	})
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want the unchecked and the missing proof", problems)
	}
	if !strings.Contains(problems[0], "TestAuditProofGone") || !strings.Contains(problems[0], "not a top-level func") {
		t.Errorf("problems[0] = %q", problems[0])
	}
	if !strings.Contains(problems[1], "TestAuditProofUnchecked") || !strings.Contains(problems[1], "never calls") {
		t.Errorf("problems[1] = %q", problems[1])
	}
}

// TestIngressActionTableCatchesAMissingRow proves the checker reports both ways the table can drift.
func TestIngressActionTableCatchesAMissingRow(t *testing.T) {
	missing, unknown := checkTable(
		[]string{"ingress.a", "ingress.b"},
		[]ingressRow{{Action: "ingress.a"}, {Action: "ingress.c"}},
	)
	if !slices.Equal(missing, []string{"ingress.b"}) {
		t.Errorf("missing = %v, want [ingress.b]", missing)
	}
	if !slices.Equal(unknown, []string{"ingress.c"}) {
		t.Errorf("unknown = %v, want [ingress.c]", unknown)
	}
	if m, u := checkTable([]string{"ingress.a"}, []ingressRow{{Action: "ingress.a"}}); len(m) != 0 || len(u) != 0 {
		t.Errorf("a matching table reported missing %v, unknown %v", m, u)
	}
}
