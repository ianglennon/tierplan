// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// ErrTooLarge means the compose file is over MaxFileBytes.
var ErrTooLarge = errors.New("stacks: the compose file is too large")

// sandboxDir is the working directory a load runs in. It does not exist on any host Bayplan
// runs on: a relative path in a compose file resolves under it and so reads nothing.
const sandboxDir = "/bayplan-stack-sandbox"

// loadTimeout bounds one load; compose-go walks a YAML tree that a user wrote.
const loadTimeout = 5 * time.Second

// LoadInput is what Load needs: the stack's name (it becomes the compose project name), its
// target and the working copy to load.
type LoadInput struct {
	Name   string
	Target Target
	Copy   WorkingCopy
}

// Loaded is a compose file read by compose-go, with its variables interpolated.
type Loaded struct {
	Project *types.Project
}

// Load reads the working copy's compose file with compose-go, in a sandbox:
//
//   - the content is in memory and the environment is the stack's variable table only, so a
//     variable set in the Bayplan server's own environment never reaches interpolation (D-01);
//   - include and extends are not followed, and env_file and label_file are not resolved, so the
//     load opens no file;
//   - paths are not resolved, and the working directory does not exist.
//
// A compose file over MaxFileBytes is refused with ErrTooLarge before it is parsed.
func Load(ctx context.Context, in LoadInput) (*Loaded, error) {
	content := in.Copy.Compose()
	if len(content) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	env := make(types.Mapping, len(in.Copy.Vars))
	for _, v := range in.Copy.Vars {
		env[v.Name] = v.Value
	}
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	details := types.ConfigDetails{
		WorkingDir:  sandboxDir,
		ConfigFiles: []types.ConfigFile{{Filename: ComposeFile, Content: content}},
		Environment: env,
	}
	project, err := loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(in.Name, true)
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipResolveEnvironment = true
		o.SkipResolveLabels = true
		o.ResolvePaths = false
	})
	if err != nil {
		return nil, fmt.Errorf("stacks: load %s: %w", ComposeFile, err)
	}
	return &Loaded{Project: project}, nil
}
