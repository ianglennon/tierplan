// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"slices"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// CodeExternalFileObject is the code of the blocking problem for an external secret or config on a
// standalone host. The check reports it at the reference; the review repeats it only when the check did not.
const CodeExternalFileObject = stacks.CodeExternalFileObject

// reviewStandalone fills rv with what a deploy of the saved draft would do to a standalone host (STK-06):
// the host's containers under the stack's Compose project are read, the images are pinned the way a swarm
// deploy pins them (the same table, STK-09), the project is prepared as the deploy prepares it, and each
// service's config hash is compared with the hash its containers carry. A service with no container is
// added; one with a container whose hash differs is changed; a service of the host's project that the file no
// longer declares is removed. The field-level difference is plan 07-22's, so a changed service carries no
// spec. It never writes.
//
// res is the check of the draft, which loaded.
func (d Deps) reviewStandalone(ctx context.Context, in ReviewInput, res stacks.CheckResult, rv *Review) error {
	files, vars := stacks.NewFileSet(in.Copy.Files), varTable(in.Copy.Vars)
	project := res.Loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, files, vars)
	if err != nil {
		rv.Problems = append(rv.Problems, objectErrorProblem(err)).Sorted()
		return nil
	}
	node, err := d.StandaloneNode(ctx, in.EnvID)
	if err != nil {
		return err
	}
	live, err := d.readStandalone(ctx, in.EnvID, node, in.Stack.Name)
	if err != nil {
		return err
	}
	if foreignLive(live, in.Stack.ID) {
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeNameClash, Message: NameClashMessage(in.Stack.Name), File: stacks.ComposeFile,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	// The compose rules judge the file; there is no converted swarm spec to judge on a standalone host.
	pol, err := d.Policy(ctx, in.Caller, in.EnvID, in.Copy, convert.Plan{})
	if err != nil {
		return err
	}
	rv.Problems = append(rv.Problems, pol.Problems...)
	if pol.Kinds != nil {
		rv.NeedsAdmin = pol.Kinds
	}

	items := standaloneItems(project)
	var stored map[string]stacks.LivePin
	if in.Pins != nil {
		var perr error
		if stored, perr = in.Pins.GoodPins(ctx, in.Stack.ID); perr != nil {
			d.log().Warn("could not read the pins of a stack's last good revision", "environment", in.EnvID, "stack", in.Stack.Name, "error", perr)
		}
	}
	var update map[string]bool
	if in.Mode == ReviewModeUpdate {
		update = make(map[string]bool, len(in.Services))
		for _, s := range in.Services {
			update[s] = true
		}
	}
	pinned := d.resolvePins(ctx, in.EnvID, items, standaloneLivePins(live.Containers), stored, update, d.nodeReader(in.EnvID, node))
	rv.Problems = append(rv.Problems, placeProblems(files, pinned.Problems)...)
	if pinned.Code != "" && len(pinned.Problems) == 0 {
		// A failure that is not about one image (the host is not reachable, the agent refused).
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: pinned.Code, Message: CodeMessage(pinned.Code), File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	rv.Pins = pinned.Refs()
	rv.Images = imageRows(items, pinned, standaloneCurrent(live.Containers), update != nil)
	for _, r := range rv.Images {
		switch r.State {
		case ImageNew:
			rv.Summary.ImagesNew++
		case ImagePinned, ImageAsWritten, ImageLocal:
			rv.Summary.ImagesPinned++
		}
	}

	if err := stacks.PrepareStandalone(project, in.Stack.ID, standalonePins(items, pinned), objs); err != nil {
		if p := prepareProblem(err); !slices.ContainsFunc(rv.Problems, func(have stacks.Problem) bool { return have.Code == p.Code }) {
			rv.Problems = append(rv.Problems, p)
		}
		rv.Problems = rv.Problems.Sorted()
		return nil
	}
	diff, err := diffStandalone(project, live.Containers)
	if err != nil {
		return err
	}
	for _, name := range diff.Added {
		rv.Services = append(rv.Services, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeAdded})
		rv.Summary.Services.Added++
	}
	for _, name := range diff.Changed {
		rv.Services = append(rv.Services, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeChanged})
		rv.Summary.Services.Changed++
	}
	for _, name := range diff.Removed {
		rv.Services = append(rv.Services, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeRemoved})
		rv.Summary.Services.Removed++
	}
	for _, name := range missingNetworks(project, live.Networks) {
		rv.Objects = append(rv.Objects, ObjectChange{Change: stacks.ChangeAdded, Kind: "network", Name: name})
		rv.Summary.Networks.Added++
	}
	for _, name := range missingVolumes(project, live.Volumes) {
		rv.Objects = append(rv.Objects, ObjectChange{Change: stacks.ChangeAdded, Kind: "volume", Name: name})
		rv.Summary.Volumes.Added++
	}
	rv.Problems = rv.Problems.Sorted()
	return nil
}

// prepareProblem is the blocking problem for a project the Compose SDK cannot be given.
func prepareProblem(err error) stacks.Problem {
	msg := "The stack can't be prepared for a standalone host."
	code := CodeConvertProblem
	if errors.Is(err, stacks.ErrUseAPISocket) {
		code = stacks.CodeUseAPISocket
		msg = "use_api_socket can't be used on a standalone host: it would mount the host's Docker socket into the container. Mount the socket as a bind volume if the service needs it; that needs Admin."
	}
	if errors.Is(err, stacks.ErrExternalFileObject) {
		code = CodeExternalFileObject
		msg = "A standalone host can't use an external secret or config: it writes the content of each into the container, and an external one has none. Declare it with a file, an environment variable or content instead."
	}
	return stacks.Problem{
		Severity: stacks.SeverityError, Code: code, Message: msg, File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
	}
}
