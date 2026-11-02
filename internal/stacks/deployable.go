// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotDeployable means a working copy has an error the check refuses it for, so no project of it
// is handed on.
var ErrNotDeployable = errors.New("stacks: the working copy cannot be deployed as it stands")

// NotDeployableError is the refusal of Deployable: the problems the check found, masked with the
// stack's masker. Its text states a count and never quotes a problem, so a value of the stack cannot
// reach a log through it.
type NotDeployableError struct {
	Problems Problems
}

func (e *NotDeployableError) Error() string {
	return fmt.Sprintf("stacks: the working copy has %d errors, so it cannot be deployed", e.Problems.Errors())
}

// Is reports ErrNotDeployable, so a caller can test for the refusal with errors.Is.
func (e *NotDeployableError) Is(target error) bool { return target == ErrNotDeployable }

// Deployable is the one way a deploy, a roll back, a stop or a start turns a working copy into a
// project (CR-03, CR-04, CR-05). It runs the very pipeline Check runs, so the project is the one the
// editor, the review and the standalone deploy already judge: the prescan has passed, the sandbox
// holds, env_file is resolved once from the stack's own files and build is stripped. It returns that
// project only when the check found no error; a warning never refuses. A refusal is a
// *NotDeployableError whose problems are masked as Check masks them.
//
// Deployable is Check's twin and must stay so: a second way to load a compose file is how a stack
// that deployed could not be rolled back, started or stopped.
func Deployable(ctx context.Context, in CheckInput) (*Loaded, error) {
	res := check(ctx, in)
	if res.Loaded != nil && res.Problems.Errors() == 0 {
		return res.Loaded, nil
	}
	// A masker is costly for a large secret file, so it is built only for a refusal.
	MaskProblems(res.Problems, NewMasker(in.Copy.Vars, in.Copy.MaskFiles()))
	return nil, &NotDeployableError{Problems: res.Problems.Sorted()}
}
