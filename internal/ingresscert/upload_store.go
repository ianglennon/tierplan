// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// What an upload replaced, as the upload hook is told.
const (
	// ReplacedNone means the hostname had no live certificate.
	ReplacedNone = "none"
	// ReplacedACME means the hostname's live row was an ACME certificate (or an order for one).
	ReplacedACME = SourceACME
	// ReplacedUpload means the hostname's live row was an earlier upload.
	ReplacedUpload = SourceUpload
)

// RetiredSwitchedToACME is the retired_reason of an upload an administrator put back on ACME.
const RetiredSwitchedToACME = "switched_to_acme"

// RequestRefusedError is a refusal of a request for a certificate for a hostname: an upload or a request
// ahead (D-08). Problem carries a stable code and a sentence that names nothing the caller may not see.
type RequestRefusedError struct {
	Problem ingress.Problem
}

// Error returns the refusal's sentence.
func (e *RequestRefusedError) Error() string { return e.Problem.Message }

// refuseHeldElsewhere is the refusal of a hostname whose live certificate another environment's request
// ahead still holds (D-08). It names no environment.
func refuseHeldElsewhere() *RequestRefusedError {
	return &RequestRefusedError{Problem: ingress.Problem{
		Severity: ingress.SeverityError, Code: ingress.CodeHostTaken,
		Message: "A certificate for this hostname was requested ahead by another environment. Choose another name.",
	}}
}

// refuseUploadedElsewhere is the refusal of an upload for a hostname whose live certificate another environment
// uploaded. Like refuseHeldElsewhere it names nothing of that environment.
func refuseUploadedElsewhere() *RequestRefusedError {
	return &RequestRefusedError{Problem: ingress.Problem{
		Severity: ingress.SeverityError, Code: ingress.CodeHostTaken,
		Message: "Another environment uploaded the certificate for this hostname. Choose another name.",
	}}
}

// refuseHostChanged is the refusal of a write that found, inside its transaction, a live certificate of an
// environment it does not bump: the hostname's certificate changed while the request was written.
func refuseHostChanged() *RequestRefusedError {
	return &RequestRefusedError{Problem: ingress.Problem{
		Severity: ingress.SeverityError, Code: ingress.CodeHostTaken,
		Message: "The certificate for this hostname changed while your request was written. Try again.",
	}}
}

// UploadInput is an upload: an Admin's chain and key for a hostname of environment EnvID. World is what the
// environment's hostname rules judge the hostname against, read by the caller.
type UploadInput struct {
	EnvID, Hostname, Actor string
	ChainPEM, KeyPEM       []byte
	World                  ingress.World
}

// UploadHook runs inside the transaction of an upload, after the write; an error rolls the upload back.
// replaced is what the upload replaced (ReplacedACME, ReplacedUpload or ReplacedNone).
type UploadHook func(ctx context.Context, tx *sql.Tx, c Cert, replaced string, warnings []string) error

// Upload stores a validated certificate and key as the live certificate of the hostname (D-22, D-23). The
// hostname must pass the environment's request rules and the second gate before anything is read, and the
// chain and key must pass ValidateUpload; a refusal writes nothing. The live row of the hostname, whatever
// its source, is replaced in the same transaction: deleted by the id read inside the write, with its orders.
// A live row of another environment is replaced only when it is not an upload and that environment's request
// ahead no longer holds it, with an ingress.cert_moved audit row, and certs_version of both environments rises (D-31). The key is
// sealed under the new row's id and is never logged or returned. roots nil means the system pool. It returns
// the new row and the warnings.
func (s *Store) Upload(ctx context.Context, in UploadInput, roots *x509.CertPool, hook UploadHook) (Cert, []string, error) {
	host := ingress.NormalizeHost(in.Hostname)
	dom, problem, bad := ingress.CheckRequest(host, in.World)
	if bad {
		return Cert{}, nil, &RequestRefusedError{Problem: problem}
	}
	// The second gate: the hostname is strictly under the domain, and is a name an order could be made for.
	// checkPublic is false: no CA is involved, and an internal CA is a valid reason to upload (D-23).
	if !ingress.Under(host, dom.Suffix) || acme.CheckName(host, false) != nil {
		return Cert{}, nil, &RequestRefusedError{Problem: ingress.Problem{
			Severity: ingress.SeverityError, Code: ingress.CodeHostInvalid,
			Message: "The hostname “" + host + "” cannot carry a certificate.",
		}}
	}
	now := s.now()
	res, err := ValidateUpload(host, in.ChainPEM, in.KeyPEM, now, roots)
	if err != nil {
		return Cert{}, nil, err
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Cert{}, nil, fmt.Errorf("ingresscert: make a certificate id: %w", err)
	}
	id := uid.String()

	// The environment of an existing live row is read first, to know whose version the write must raise,
	// and compared again inside the write.
	envs := []string{in.EnvID}
	preEnv := ""
	if pre, found, err := s.LiveCert(ctx, host); err != nil {
		return Cert{}, nil, err
	} else if found && pre.EnvID != in.EnvID {
		preEnv = pre.EnvID
		envs = append(envs, preEnv)
	}

	var out Cert
	err = s.writeCommitted(ctx, envs, "the uploaded certificate", func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		replaced := ReplacedNone
		moved := false
		var fromEnv, fromDomain string
		row, err := q.GetLiveIngressCertByHostname(ctx, host)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return false, fmt.Errorf("ingresscert: read the certificate of %q: %w", host, err)
		default:
			if row.EnvID != in.EnvID && row.EnvID != preEnv {
				return false, refuseHostChanged()
			}
			if row.EnvID != in.EnvID {
				live, err := certOf(row)
				if err != nil {
					return false, err
				}
				if RequestHeld(live, now) {
					return false, refuseHeldElsewhere()
				}
				// Another environment's upload is its Admin's own material: an upload never deletes it.
				if live.Source == SourceUpload {
					return false, refuseUploadedElsewhere()
				}
				moved, fromEnv, fromDomain = true, row.EnvID, row.DomainID
			}
			replaced, err = q.DeleteReplacedIngressCert(ctx, row.ID)
			if err != nil {
				return false, fmt.Errorf("ingresscert: replace the certificate of %q: %w", host, err)
			}
		}
		ts := toNanos(now)
		ready := int64(0)
		if !now.Before(res.Leaf.NotBefore) && !now.After(res.Leaf.NotAfter) {
			ready = 1
		}
		if err := q.InsertIngressCertUploaded(ctx, sqlc.InsertIngressCertUploadedParams{
			ID: id, Hostname: host, EnvID: in.EnvID, DomainID: dom.ID, Ready: ready,
			CreatedBy: in.Actor, CreatedAt: ts, UpdatedAt: ts,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: insert the uploaded certificate of %q: %w", host, err)
		}
		if err := s.writeCertificate(ctx, q, id, res.ChainPEM, res.KeyPKCS8PEM, "", now); err != nil {
			return false, err
		}
		stored, err := q.GetIngressCert(ctx, id)
		if err != nil {
			return false, fmt.Errorf("ingresscert: read back the uploaded certificate of %q: %w", host, err)
		}
		if out, err = certOf(stored); err != nil {
			return false, err
		}
		if moved && s.audit != nil {
			if err := s.audit.RecordTx(ctx, tx, audit.Entry{
				Actor: audit.SystemIngress, Action: audit.ActionIngressCertMoved, TargetType: auditTargetCertificate,
				TargetID: id, EnvID: in.EnvID, Result: audit.ResultSuccess,
				Details: map[string]string{
					"hostname": host, "from_env": fromEnv, "to_env": in.EnvID,
					"from_domain": fromDomain, "to_domain": dom.ID, "outcome": "replaced", "cause": "upload",
				},
			}); err != nil {
				return false, fmt.Errorf("ingresscert: record the move of %q: %w", host, err)
			}
		}
		if hook != nil {
			if err := hook(ctx, tx, out, replaced, res.Warnings); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return Cert{}, nil, err
	}
	return out, res.Warnings, nil
}

// SwitchToACME hands an uploaded hostname back to ACME (D-22): the live upload row certID of environment
// envID is retired with RetiredSwitchedToACME, and keeps serving until it expires or its replacement is
// ready, and a queued ACME row for the hostname is inserted with issuer, which is the hostname's domain's
// current issuer, passed by the caller. Nothing takes a hostname over to ACME by itself. A row that is not a
// live upload of the environment is ErrCertNotFound and nothing changes. hook, which may be nil, runs in the
// transaction with the new row. It returns the new row.
func (s *Store) SwitchToACME(ctx context.Context, envID, certID, actor, issuer string, hook CertHook) (Cert, error) {
	if issuer != IssuerStaging && issuer != IssuerProduction {
		return Cert{}, fmt.Errorf("ingresscert: the issuer %q is neither staging nor production", issuer)
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Cert{}, fmt.Errorf("ingresscert: make a certificate id: %w", err)
	}
	now := s.now()
	ts := toNanos(now)
	var out Cert
	err = s.write(ctx, []string{envID}, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := q.GetIngressCert(ctx, certID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrCertNotFound
		}
		if err != nil {
			return false, fmt.Errorf("ingresscert: read certificate %q: %w", certID, err)
		}
		if row.EnvID != envID || row.Source != SourceUpload || row.RetiredAt != 0 {
			return false, ErrCertNotFound
		}
		if err := q.RetireIngressCert(ctx, sqlc.RetireIngressCertParams{
			RetiredAt: ts, RetiredReason: RetiredSwitchedToACME, UpdatedAt: ts, ID: certID,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: retire certificate %q: %w", certID, err)
		}
		id := uid.String()
		if err := q.InsertIngressCert(ctx, sqlc.InsertIngressCertParams{
			ID: id, Hostname: row.Hostname, EnvID: envID, DomainID: row.DomainID, Source: SourceACME, Issuer: issuer,
			Phase: PhaseQueued, CreatedBy: actor, CreatedAt: ts, UpdatedAt: ts,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: queue the certificate of %q: %w", row.Hostname, err)
		}
		fresh, err := q.GetIngressCert(ctx, id)
		if err != nil {
			return false, fmt.Errorf("ingresscert: read back the certificate of %q: %w", row.Hostname, err)
		}
		if out, err = certOf(fresh); err != nil {
			return false, err
		}
		if hook != nil {
			if err := hook(ctx, tx, out); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return Cert{}, err
	}
	return out, nil
}
