// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/ingressrender"
)

// The codes of a failure that is not a renderer refusal. A refusal carries the renderer's own code
// (ingressrender.CodeUnsafeValue and the others, or CodeSchemaUnsupported).
const (
	// CodeReadFailed: the snapshot or the certificates of the environment could not be read.
	CodeReadFailed = "ingress_render_read_failed"
	// CodeRenderFailed: the render failed for a reason the renderer does not name.
	CodeRenderFailed = "ingress_render_failed"
)

// Failure is an environment whose latest render failed, so its nodes still hold the last good bundle (D-20). It holds
// names and a code only: never a certificate, a key or the error text. Hostname and PathPrefix are empty when the
// failure is not about one route. Since is when this same failure was first seen, on the reconciler's clock.
type Failure struct {
	EnvID      string
	Hostname   string
	PathPrefix string
	Code       string
	Since      time.Time
}

// readError marks an error from reading the snapshot or the certificates, as opposed to rendering them.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// classify names the failure an error is: a refusal names its route and carries its code, a read error is
// CodeReadFailed and anything else CodeRenderFailed.
func classify(envID string, err error) Failure {
	f := Failure{EnvID: envID, Code: CodeRenderFailed}
	var refused *ingressrender.RefusedError
	var schema *ingressrender.UnsupportedSchemaError
	var read *readError
	switch {
	case errors.As(err, &refused):
		f.Code, f.Hostname, f.PathPrefix = refused.Code, refused.Hostname, refused.PathPrefix
	case errors.As(err, &schema):
		f.Code = ingressrender.CodeSchemaUnsupported
	case errors.As(err, &read):
		f.Code = CodeReadFailed
	}
	return f
}

// key is what makes two failures of one environment the same failure.
func (f Failure) key() string {
	return strings.Join([]string{f.Code, f.Hostname, f.PathPrefix}, "\x00")
}

// recordFailure notes that the environment's render failed with err. The failure keeps its Since while it repeats, and
// the cause is logged at Warn once per distinct failure until a render succeeds (D-20, T-21-40). The stored bundle is
// not touched here or by the caller.
func (r *Reconciler) recordFailure(envID string, err error, now time.Time) {
	f := classify(envID, err)
	f.Since = now

	r.fmu.Lock()
	if old, ok := r.failures[envID]; ok && old.key() == f.key() {
		f.Since = old.Since
	}
	r.failures[envID] = f
	seen := r.logged[envID]
	if seen == nil {
		seen = map[string]struct{}{}
		r.logged[envID] = seen
	}
	_, said := seen[f.key()]
	seen[f.key()] = struct{}{}
	r.fmu.Unlock()

	if !said {
		r.log.Warn("the ingress reconciler could not render an environment and keeps its last good bundle",
			"env_id", envID, "hostname", f.Hostname, "path_prefix", f.PathPrefix, "code", f.Code, "error", err)
	}
}

// clearFailure forgets the environment's failure and what was logged for it, so a recurrence is said again.
func (r *Reconciler) clearFailure(envID string) {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	delete(r.failures, envID)
	delete(r.logged, envID)
}

// Failures lists the environments whose latest render failed, sorted by environment id. The slice is a copy.
func (r *Reconciler) Failures() []Failure {
	r.fmu.Lock()
	out := make([]Failure, 0, len(r.failures))
	for _, f := range r.failures {
		out = append(out, f)
	}
	r.fmu.Unlock()
	slices.SortFunc(out, func(a, b Failure) int { return strings.Compare(a.EnvID, b.EnvID) })
	return out
}
