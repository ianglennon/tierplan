// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/vault"
)

// The own certificate's ACME account and the ingress account can both be registered with the same directory:
// they live in separate tables, the ingress store never reads or writes the own one, and forgetting the
// ingress account leaves the own (T-20-12, RESEARCH Pitfall 2).
func TestIngressAccountLivesBesideTheOwnAccount(t *testing.T) {
	ctx := context.Background()
	r := newIssueRig(t)
	dirURL := r.fake.DirectoryURL()

	own := acme.NewClient(r.directory(), r.ledger, r.clock, "tierplan-test/0")
	if _, err := own.Register(ctx, "", true); err != nil {
		t.Fatalf("register the own account: %v", err)
	}
	r.client() // registers the ingress account through Store.AccountStore()

	if n := r.count(`SELECT COUNT(*) FROM acme_account WHERE directory_url = ?`, dirURL); n != 1 {
		t.Errorf("acme_account holds %d rows for the directory, want 1", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_acme_account WHERE directory_url = ?`, dirURL); n != 1 {
		t.Errorf("ingress_acme_account holds %d rows for the directory, want 1", n)
	}

	ownAcct, _, found, err := r.ledger.Account(ctx, dirURL)
	if err != nil || !found {
		t.Fatalf("the own account = found %v, %v", found, err)
	}
	ingAcct, key, found, err := r.s.AccountStore().Account(ctx, dirURL)
	if err != nil || !found || key == nil {
		t.Fatalf("the ingress account = found %v, %v", found, err)
	}
	if ingAcct.URL == ownAcct.URL {
		t.Errorf("both accounts are %s: the ingress account must be its own", ingAcct.URL)
	}
	if url, found, err := r.s.AccountURL(ctx, dirURL); err != nil || !found || url != ingAcct.URL {
		t.Errorf("AccountURL = %q, %v, %v, want %q", url, found, err, ingAcct.URL)
	}

	// The key opens only under ingress-acme-account/v1/<directory url>.
	var sealed string
	if err := r.st.Read.QueryRow(`SELECT key_sealed FROM ingress_acme_account WHERE directory_url = ?`, dirURL).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.v.Open(vault.ContextIngressACMEAccountKey+dirURL, sealed); err != nil {
		t.Errorf("the account key does not open under its own context: %v", err)
	}
	for _, wrong := range []string{vault.ContextACMEAccountKey + dirURL, vault.ContextIngressACMEAccountKey + "https://other.test/directory", vault.ContextIngressOrderKey + dirURL} {
		if _, err := r.v.Open(wrong, sealed); err == nil {
			t.Errorf("the account key opened under %q", wrong)
		}
	}

	// Forgetting the ingress account leaves the own one.
	if err := r.s.AccountStore().ForgetAccount(ctx, dirURL); err != nil {
		t.Fatalf("ForgetAccount: %v", err)
	}
	if _, _, found, err := r.s.AccountStore().Account(ctx, dirURL); err != nil || found {
		t.Errorf("the ingress account after ForgetAccount = found %v, %v, want none", found, err)
	}
	if _, found, err := r.s.AccountURL(ctx, dirURL); err != nil || found {
		t.Errorf("AccountURL after ForgetAccount = found %v, %v, want none", found, err)
	}
	if after, _, found, err := r.ledger.Account(ctx, dirURL); err != nil || !found || after.URL != ownAcct.URL {
		t.Errorf("the own account after the ingress ForgetAccount = %+v, found %v, %v, want it untouched", after, found, err)
	}
}

// The terms acknowledgement is install-wide: a fresh install has none, AcceptTerms records who and when and
// runs its hook in the same transaction, and a failing hook leaves the terms unaccepted (D-36).
func TestAcceptTermsIsRecordedWithItsHook(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh install and a successful accept", func(t *testing.T) {
		s, _ := newTestStore(t)
		got, err := s.State(ctx)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if got.PausedReason != "" || !got.TermsAcceptedAt.IsZero() || got.TermsAcceptedBy != "" {
			t.Fatalf("a fresh State = %+v, want no pause and no acceptance", got)
		}

		var sawInTx string
		err = s.AcceptTerms(ctx, "user:1", func(_ context.Context, tx *sql.Tx) error {
			// The hook sees the write: it runs in the transaction that made it.
			return tx.QueryRow(`SELECT terms_accepted_by FROM ingress_cert_state WHERE id = 1`).Scan(&sawInTx)
		})
		if err != nil {
			t.Fatalf("AcceptTerms: %v", err)
		}
		if sawInTx != "user:1" {
			t.Errorf("the hook saw terms_accepted_by %q, want user:1: it did not run in the same transaction", sawInTx)
		}
		got, err = s.State(ctx)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if got.TermsAcceptedBy != "user:1" || !got.TermsAcceptedAt.Equal(fixedNow) || !got.UpdatedAt.Equal(fixedNow) {
			t.Errorf("State = %+v, want accepted by user:1 at %v", got, fixedNow)
		}
	})

	t.Run("a failing hook leaves the terms unaccepted", func(t *testing.T) {
		s, _ := newTestStore(t)
		boom := errors.New("the audit row could not be written")
		err := s.AcceptTerms(ctx, "user:1", func(context.Context, *sql.Tx) error { return boom })
		if !errors.Is(err, boom) {
			t.Fatalf("AcceptTerms = %v, want the hook's error", err)
		}
		got, err := s.State(ctx)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if !got.TermsAcceptedAt.IsZero() || got.TermsAcceptedBy != "" {
			t.Errorf("State = %+v after a failed accept, want the terms unaccepted", got)
		}
	})

	t.Run("a nil hook is allowed", func(t *testing.T) {
		s, _ := newTestStore(t)
		if err := s.AcceptTerms(ctx, "user:2", nil); err != nil {
			t.Fatalf("AcceptTerms: %v", err)
		}
		if got, err := s.State(ctx); err != nil || got.TermsAcceptedBy != "user:2" {
			t.Errorf("State = %+v, %v, want accepted by user:2", got, err)
		}
	})
}
