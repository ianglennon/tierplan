// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"errors"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The kinds of object an apply creates for a service, in the order they are created: a network
// first, then the secrets and configs that are mounted into its tasks.
const (
	KindNetwork = "network"
	KindSecret  = "secret"
	KindConfig  = "config"
)

// Sources an audit row of an object made or removed by an apply carries.
const (
	SourceApply        = "service_apply"
	SourceApplyCleanup = "service_apply_cleanup"
)

// ErrObjectPermission means the caller may not create one of the kinds of object the apply needs,
// so nothing was created. The error is an *ObjectPermissionError.
var ErrObjectPermission = errors.New("swarmops: the caller may not create the objects the spec needs")

// ObjectPermissionError names the kinds the caller may not create.
type ObjectPermissionError struct{ Kinds []string }

func (e *ObjectPermissionError) Error() string        { return "swarmops: the caller may not create " + joinKinds(e.Kinds) }
func (e *ObjectPermissionError) Is(target error) bool { return target == ErrObjectPermission }

// ObjectCreateError is the creation of one object failing: which, and why. A name the swarm has
// already is dockerx.ErrNameTaken through Unwrap.
type ObjectCreateError struct {
	Kind, Name string
	Err        error
}

func (e *ObjectCreateError) Error() string {
	return "swarmops: could not create " + e.Kind + " " + e.Name + ": " + e.Err.Error()
}
func (e *ObjectCreateError) Unwrap() error { return e.Err }

// StagedObject is an object the caller made in the editor, which does not exist yet. Data is the
// value of a secret or the content of a config; it crosses Bayplan once, on its way to the manager.
type StagedObject struct {
	Kind, Name string
	Data       []byte
	Templating string
	// Subnet is the optional addressing of a network, in CIDR form.
	Subnet string
	Labels map[string]string
}

// CreatedObject is an object an apply made.
type CreatedObject struct{ Kind, Name, ID string }

// CleanupReport is what removing the objects of a failed apply did.
type CleanupReport struct{ Removed, NotRemoved []CreatedObject }

// ApplyStep is one step of an apply: the image, an object, the service. State is done, failed or
// skipped (not reached); Code says why a step failed.
type ApplyStep struct{ Step, Kind, Name, State, Code string }

// Recorder takes the audit row of an object made or removed. The caller fills in who and where.
type Recorder func(audit.Entry)

func joinKinds(kinds []string) string { return "" }

// createObjects creates the staged objects in dependency order.
func createObjects(_ context.Context, _ *dockerx.Client, _ []StagedObject, _ Recorder) ([]CreatedObject, error) {
	return nil, nil
}

// cleanupObjects removes created objects in reverse order of creation.
func cleanupObjects(_ context.Context, _ *dockerx.Client, _ []CreatedObject, _ Recorder) CleanupReport {
	return CleanupReport{}
}
