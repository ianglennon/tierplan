// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"testing"
)

// A fold deletes the Local environment's row and every ingress table cascades from it, so a Local
// environment holding ingress state is refused, inside the fold's write transaction (SWM-02,
// D-12, D-17).

// ingressRows is what a refused fold must leave alone: the count of rows in each ingress table.
func (f *foldRig) ingressRows(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"ingress_env", "ingress_domains", "ingress_reserved", "ingress_hostnames", "ingress_routes"} {
		var n int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tbl] = n
	}
	return out
}

func (f *foldRig) mustExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestTracerFoldIsRefusedWhileTheLocalEnvironmentHoldsAnIngressDomain(t *testing.T) {
	f := newFoldRig(t, nil)
	f.mustExec(t, `INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES ('dom-1', ?, 'example.org', 'u1', 1, 1)`, f.localEnv.ID)

	err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "10.0.0.9", "test")
	if !errors.Is(err, ErrFoldIngressActive) {
		t.Fatalf("FoldLocal = %v, want ErrFoldIngressActive", err)
	}
	var ia *IngressActiveError
	if !errors.As(err, &ia) || ia.Domains != 1 {
		t.Fatalf("FoldLocal error %v is not an IngressActiveError with 1 domain", err)
	}
	f.assertNothingFolded(t)
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_domains WHERE id = 'dom-1' AND env_id = ?`, f.localEnv.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the domain row count = %d, %v after a refused fold, want 1", n, err)
	}
}
