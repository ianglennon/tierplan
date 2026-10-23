// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"context"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// AssertSandboxed lets the tests of package stacks_test reach the check made after a load.
var AssertSandboxed = assertSandboxed

// WithLoader returns a context under which Load calls fn in place of compose-go.
func WithLoader(ctx context.Context, fn func(context.Context, types.ConfigDetails, ...func(*loader.Options)) (*types.Project, error)) context.Context {
	return context.WithValue(ctx, loaderKey{}, loadFunc(fn))
}
