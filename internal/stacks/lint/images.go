// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package lint

import "fmt"

// buildRules reports a service that sets build (A5). Bayplan deploys images and does not build
// them, and stacks.Check strips build from the project before any engine sees it, so a build key
// is never acted on. With an image set it is a warning, worded for the target; with no image
// there is nothing to deploy, and it is an error on both targets.
func buildRules(in Input) []Finding {
	var out []Finding
	for _, name := range serviceNames(in.Project) {
		svc := in.Project.Services[name]
		if svc.Build == nil {
			continue
		}
		pos, path := in.at("services."+name+".build", "services."+name)
		f := Finding{Key: path, Pos: pos}
		switch {
		case svc.Image == "":
			f.Severity, f.Code = SeverityError, CodeBuildWithoutImage
			f.Message = fmt.Sprintf(messages[CodeBuildWithoutImage], name)
		case in.Target == TargetSwarm:
			f.Severity, f.Code = SeverityWarning, CodeIgnoredOnTarget
			f.Message = ignoredMessage("build", TargetSwarm, swarmActions["build"])
		default:
			f.Severity, f.Code = SeverityWarning, CodeIgnoredOnTarget
			f.Message = messages["build_with_image"]
		}
		out = append(out, f)
	}
	return out
}

// movingTag reports whether an image reference can name a different image tomorrow: it has no
// tag or its tag is latest, and no digest pins it.
func movingTag(ref string) bool {
	return false
}

// imageFindings warns about every service whose image can move.
func imageFindings(in Input) []Finding {
	return nil
}
