// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package lint

import (
	"fmt"

	"github.com/distribution/reference"
)

// buildRules reports a service that sets build (A5). Coaming deploys images and does not build
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
// tag or its tag is latest, and no digest pins it. A reference that does not parse is not for this
// rule to judge.
func movingTag(ref string) bool {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}
	if _, pinned := named.(reference.Digested); pinned {
		return false
	}
	tagged, ok := reference.TagNameOnly(named).(reference.Tagged)
	return ok && tagged.Tag() == "latest"
}

// imageFindings warns, at the image's line, about every service whose image can move.
func imageFindings(in Input) []Finding {
	var out []Finding
	for _, name := range serviceNames(in.Project) {
		image := in.Project.Services[name].Image
		if image == "" || !movingTag(image) {
			continue
		}
		pos, path := in.at("services."+name+".image", "services."+name)
		out = append(out, Finding{
			Severity: SeverityWarning, Code: CodeMovingTag, Key: path, Pos: pos,
			Message: fmt.Sprintf(messages[CodeMovingTag], image),
		})
	}
	return out
}
