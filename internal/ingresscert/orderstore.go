// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// ErrCertGone is returned by an order's StoreIssued when the certificate row the order was placed for no
// longer exists, has been retired, or has been replaced by an upload while the order ran. Nothing is written:
// an ACME certificate is never put over what an administrator did meanwhile (CRT-08).
var ErrCertGone = errors.New("ingresscert: the certificate was deleted, retired or replaced by an upload while its order ran")

// ClosedOrderRetention is how long a closed order, and the sealed key it holds, is kept before PruneClosedOrders
// deletes it (D-35). Open orders are never pruned.
const ClosedOrderRetention = 24 * time.Hour

// auditTargetCertificate is the audit target type of a per-hostname certificate.
const auditTargetCertificate = "ingress_certificate"

// issuanceKind is the kind a certificate's issuance row carries: a renew order counts as a renewal and every
// other order as a new certificate, the conservative reading of the CA's rules (RESEARCH A7).
func issuanceKind(orderKind string) string {
	if orderKind == acme.KindRenew {
		return "renewal"
	}
	return "new"
}

// orderStore is the acme.OrderStore of one certificate: it sees only that certificate's orders, and it stores
// the issued chain into that certificate's ingress_certs row.
type orderStore struct {
	s      *Store
	certID string
	// known is what the issuance ledger needs of the certificate, as the run read it before ordering. It counts a
	// certificate the CA issued for a row that is gone by the time it is stored. Empty for OrderStore.
	known issuance
}

var _ acme.OrderStore = orderStore{}

// OrderStore returns the order store of certificate certID, the seam acme.Orderer runs against.
func (s *Store) OrderStore(certID string) acme.OrderStore {
	return orderStore{s: s, certID: certID}
}

// runOrderStore is OrderStore for a run of certificate c: it also knows c's hostname, issuer and kind of order, so
// a certificate issued for c is counted even when c's row was deleted before it could be stored.
func (s *Store) runOrderStore(c Cert) acme.OrderStore {
	kind := issuanceKind(acme.KindIssue)
	if len(c.ChainPEM) > 0 {
		kind = issuanceKind(acme.KindRenew)
	}
	return orderStore{s: s, certID: c.ID, known: issuance{host: c.Hostname, issuer: c.Issuer, kind: kind}}
}

// issuance is what an issuance ledger row says besides its time.
type issuance struct{ host, issuer, kind string }

// issuanceOfRow is the issuance of a certificate row's order: a row that holds a chain is renewing it, which is
// how a run chooses the kind of its order.
func issuanceOfRow(row sqlc.IngressCert) issuance {
	kind := issuanceKind(acme.KindIssue)
	if row.ChainPem != "" {
		kind = issuanceKind(acme.KindRenew)
	}
	return issuance{host: row.Hostname, issuer: row.Issuer, kind: kind}
}

// countGone records in the issuance ledger a certificate the CA issued, and so counted against its limits, whose
// row was deleted, retired or replaced by an upload before the certificate could be stored. Nothing else is
// written: no certificate, no version and no audit row. An issuance with no hostname, or one under no registered
// domain, is not counted, as the governor would not read it.
func (o orderStore) countGone(ctx context.Context, inf issuance, now time.Time) error {
	if inf.host == "" || inf.issuer == "" {
		return nil
	}
	registered, rerr := RegisteredDomain(inf.host)
	if rerr != nil {
		return nil // a hostname with no registered domain has no budget to count against
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("ingresscert: make an issuance id: %w", err)
	}
	bg := context.WithoutCancel(ctx) // the CA has counted it; a run being stopped does not undo that
	return o.s.write(bg, nil, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.InsertIngressCertIssuance(bg, sqlc.InsertIngressCertIssuanceParams{
			ID: id.String(), Issuer: inf.issuer, RegisteredDomain: registered, Hostname: inf.host, Kind: inf.kind, IssuedAt: toNanos(now),
		}); err != nil {
			return false, fmt.Errorf("ingresscert: count the issuance of %q: %w", inf.host, err)
		}
		return false, nil
	})
}

// gone is ErrCertGone after the issued certificate was counted with inf; a failure to count is joined to it, so
// the run still ends as gone.
func (o orderStore) gone(ctx context.Context, inf issuance, now time.Time) error {
	if err := o.countGone(ctx, inf, now); err != nil {
		return errors.Join(ErrCertGone, err)
	}
	return ErrCertGone
}

// OpenOrderID returns the id of the open order of certificate certID. found is false, with a nil error, when
// none is open. It opens no key, so a caller that only needs the id (the DNS solver's owner) never touches one.
func (s *Store) OpenOrderID(ctx context.Context, certID string) (string, bool, error) {
	id, err := s.st.RQ.GetOpenIngressCertOrderID(ctx, certID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("ingresscert: read the open order of certificate %q: %w", certID, err)
	}
	return id, true, nil
}

// InsertOrder stores a new open order with its fresh certificate key sealed under ingress-order-key/v1/<order
// id>. The key is durable when InsertOrder returns, which is before any finalize can happen: a certificate
// issued for a key that was then lost could never be installed.
func (o orderStore) InsertOrder(ctx context.Context, ord acme.Order, key *ecdsa.PrivateKey) error {
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return err
	}
	sealed, err := o.s.v.Seal(vault.ContextIngressOrderKey+ord.ID, keyPEM)
	if err != nil {
		return fmt.Errorf("ingresscert: seal the key of order %q: %w", ord.ID, err)
	}
	names, err := json.Marshal(ord.Names)
	if err != nil {
		return fmt.Errorf("ingresscert: encode the names of order %q: %w", ord.ID, err)
	}
	// An order is not a change to what is served, so it raises no certs_version.
	return o.s.writeDurable(ctx, nil, "the order key", func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.InsertIngressCertOrder(ctx, sqlc.InsertIngressCertOrderParams{
			ID: ord.ID, CertID: o.certID, DirectoryUrl: ord.DirectoryURL, OrderUrl: ord.URL, Kind: ord.Kind,
			Names: string(names), Profile: ord.Profile, Replaces: ord.Replaces, AccountUrl: ord.AccountURL,
			CertKeySealed: sealed, Stage: ord.Stage, ResumeCount: int64(ord.ResumeCount),
			ExpiresAt: toNanos(ord.ExpiresAt), CreatedAt: toNanos(ord.CreatedAt), UpdatedAt: toNanos(ord.UpdatedAt),
		}); err != nil {
			return false, fmt.Errorf("ingresscert: insert order %q: %w", ord.ID, err)
		}
		return false, nil
	})
}

// OpenOrder returns the newest open order of the certificate and its key. found is false, with a nil error,
// only when no order is open: a key that does not open is an error.
func (o orderStore) OpenOrder(ctx context.Context) (acme.Order, *ecdsa.PrivateKey, bool, error) {
	row, err := o.s.st.RQ.GetOpenIngressCertOrder(ctx, o.certID)
	if errors.Is(err, sql.ErrNoRows) {
		return acme.Order{}, nil, false, nil
	}
	if err != nil {
		return acme.Order{}, nil, false, fmt.Errorf("ingresscert: read the open order of certificate %q: %w", o.certID, err)
	}
	keyPEM, err := o.s.v.Open(vault.ContextIngressOrderKey+row.ID, row.CertKeySealed)
	if err != nil {
		return acme.Order{}, nil, false, fmt.Errorf("ingresscert: open the key of order %q: %w", row.ID, err)
	}
	key, err := decodeECKey(keyPEM)
	if err != nil {
		return acme.Order{}, nil, false, fmt.Errorf("ingresscert: decode the key of order %q: %w", row.ID, err)
	}
	var names []string
	if err := json.Unmarshal([]byte(row.Names), &names); err != nil {
		return acme.Order{}, nil, false, fmt.Errorf("ingresscert: read the names of order %q: %w", row.ID, err)
	}
	return acme.Order{
		ID: row.ID, DirectoryURL: row.DirectoryUrl, URL: row.OrderUrl, Kind: row.Kind, Names: names,
		Profile: row.Profile, Replaces: row.Replaces, AccountURL: row.AccountUrl, Stage: row.Stage, Status: row.Status,
		ResumeCount: int(row.ResumeCount), ExpiresAt: fromNanos(row.ExpiresAt),
		CreatedAt: fromNanos(row.CreatedAt), UpdatedAt: fromNanos(row.UpdatedAt),
	}, key, true, nil
}

// UpdateOrder moves an order to a stage and records how many times it has been resumed.
func (o orderStore) UpdateOrder(ctx context.Context, id, stage string, resumeCount int, now time.Time) error {
	if err := o.s.st.Q.UpdateIngressCertOrder(ctx, sqlc.UpdateIngressCertOrderParams{
		Stage: stage, ResumeCount: int64(resumeCount), UpdatedAt: toNanos(now), ID: id,
	}); err != nil {
		return fmt.Errorf("ingresscert: update order %q: %w", id, err)
	}
	return nil
}

// CloseOrder closes an order in a final stage. The order and its sealed key stay for the record until
// PruneClosedOrders removes them, and OpenOrder no longer returns it.
func (o orderStore) CloseOrder(ctx context.Context, id, stage string, now time.Time) error {
	if err := o.s.st.Q.CloseIngressCertOrder(ctx, sqlc.CloseIngressCertOrderParams{
		Stage: stage, UpdatedAt: toNanos(now), ID: id,
	}); err != nil {
		return fmt.Errorf("ingresscert: close order %q: %w", id, err)
	}
	return nil
}

// errEnvMoved is the internal signal that the certificate's environment changed between reading it and taking
// the write lock, so the version bump would have gone to the wrong environment; StoreIssued reads again.
var errEnvMoved = errors.New("ingresscert: the certificate moved environments while it was stored")

// storeIssuedAttempts bounds how often StoreIssued re-reads a certificate that keeps moving environments.
const storeIssuedAttempts = 3

// StoreIssued writes the issued chain and its key into the certificate's ingress_certs row, closes the order
// as stored, counts the issuance, raises the environment's certs_version and records the audit entry, in one
// transaction: a failure keeps none of it. The certificate row is checked first, inside the transaction: when
// it is missing, retired or no longer an ACME row (an upload replaced it), the certificate is not stored and
// ErrCertGone is returned (CRT-08, T-20-11); the issuance alone is still counted, since the CA counted it. The certificate is durable when StoreIssued returns.
func (o orderStore) StoreIssued(ctx context.Context, orderID string, chainPEM []byte, key *ecdsa.PrivateKey, now time.Time) error {
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return err
	}
	for range storeIssuedAttempts {
		// The environment is needed to open the transaction, so it is read first and checked again inside it.
		pre, err := o.s.st.RQ.GetIngressCert(ctx, o.certID)
		if errors.Is(err, sql.ErrNoRows) {
			// The CA issued and counted the certificate all the same (WR-06).
			return o.gone(ctx, o.known, now)
		}
		if err != nil {
			return fmt.Errorf("ingresscert: read certificate %q: %w", o.certID, err)
		}
		err = o.storeIssued(ctx, pre.EnvID, orderID, chainPEM, keyPEM, now)
		if errors.Is(err, errEnvMoved) {
			continue
		}
		if errors.Is(err, ErrCertGone) {
			return o.gone(ctx, issuanceOfRow(pre), now)
		}
		if err != nil {
			return err
		}
		return o.s.checkpoint(ctx, "the issued certificate")
	}
	return errEnvMoved
}

// storeIssued is the transaction of StoreIssued for a certificate believed to sit in environment envID.
func (o orderStore) storeIssued(ctx context.Context, envID, orderID string, chainPEM, keyPEM []byte, now time.Time) error {
	s := o.s
	var gone bool
	err := s.write(ctx, []string{envID}, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		// The certificate first: deleting or replacing its row cascades its orders away, so the order is not a
		// reliable witness that the certificate is still the one the order was placed for.
		row, err := q.GetIngressCert(ctx, o.certID)
		if errors.Is(err, sql.ErrNoRows) {
			gone = true
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ingresscert: read certificate %q: %w", o.certID, err)
		}
		if row.RetiredAt != 0 || row.Source != SourceACME {
			gone = true
			return false, nil
		}
		if row.EnvID != envID {
			return false, errEnvMoved
		}
		ord, err := q.GetIngressCertOrder(ctx, orderID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("ingresscert: order %q of certificate %q does not exist", orderID, o.certID)
		}
		if err != nil {
			return false, fmt.Errorf("ingresscert: read order %q: %w", orderID, err)
		}
		if ord.CertID != o.certID || ord.Status != "open" {
			return false, fmt.Errorf("ingresscert: order %q is not an open order of certificate %q", orderID, o.certID)
		}
		if err := s.writeCertificate(ctx, q, o.certID, chainPEM, keyPEM, ord.AccountUrl, now); err != nil {
			return false, err
		}
		if err := q.CloseIngressCertOrder(ctx, sqlc.CloseIngressCertOrderParams{
			Stage: acme.StageStored, UpdatedAt: toNanos(now), ID: orderID,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: close order %q: %w", orderID, err)
		}
		registered, err := RegisteredDomain(row.Hostname)
		if err != nil {
			return false, err
		}
		issuanceID, err := uuid.NewV7()
		if err != nil {
			return false, fmt.Errorf("ingresscert: make an issuance id: %w", err)
		}
		if err := q.InsertIngressCertIssuance(ctx, sqlc.InsertIngressCertIssuanceParams{
			ID: issuanceID.String(), Issuer: row.Issuer, RegisteredDomain: registered, Hostname: row.Hostname,
			Kind: issuanceKind(ord.Kind), IssuedAt: toNanos(now),
		}); err != nil {
			return false, fmt.Errorf("ingresscert: count the issuance of %q: %w", row.Hostname, err)
		}
		if s.audit != nil {
			action := audit.ActionIngressCertIssued
			details := map[string]string{"hostname": row.Hostname, "issuer": row.Issuer}
			if ord.Kind == acme.KindRenew {
				action = audit.ActionIngressCertRenewed
				details["basis"] = row.RenewBasis
			}
			if err := s.audit.RecordTx(ctx, tx, audit.Entry{
				Actor: audit.SystemIngress, Action: action, TargetType: auditTargetCertificate, TargetID: o.certID,
				EnvID: row.EnvID, Result: audit.ResultSuccess, Details: details,
			}); err != nil {
				return false, fmt.Errorf("ingresscert: record the issue of %q: %w", row.Hostname, err)
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if gone {
		return ErrCertGone
	}
	return nil
}

// PruneClosedOrders deletes the closed orders last touched before olderThan, with the sealed keys they hold,
// and returns how many it deleted. An open order is never deleted. The scheduler passes now minus
// ClosedOrderRetention.
func (s *Store) PruneClosedOrders(ctx context.Context, olderThan time.Time) (int, error) {
	n, err := s.st.Q.PruneClosedIngressCertOrders(ctx, toNanos(olderThan))
	if err != nil {
		return 0, fmt.Errorf("ingresscert: prune the closed orders: %w", err)
	}
	return int(n), nil
}
