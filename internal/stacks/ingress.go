// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"sort"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// IngressPolicy is the world a stack's ingress labels are judged against: the environment's domains,
// the install-wide reserved names and the claims every stack holds. Check is pure, so the world
// arrives as data; the caller builds it from the store and the caller's permissions, and Check never
// reads a database or an ingress switch.
//
// It is the Phase 18 field of CheckInput. The name Ingress is reserved for Phase 24's network
// attachment, so this one is IngressPolicy.
type IngressPolicy struct {
	// StackID is the stack being checked, so a hostname it already holds is not taken.
	StackID string
	// World is what the stack's hostnames are judged against.
	World ingress.World
}

// serviceLabels reads the two label sets of each service of project, for the ingress parser.
func serviceLabels(project *types.Project) []ingress.ServiceLabels {
	out := make([]ingress.ServiceLabels, 0, len(project.Services))
	for name, svc := range project.Services {
		s := ingress.ServiceLabels{Name: name, Labels: map[string]string(svc.Labels)}
		if svc.Deploy != nil {
			s.DeployLabels = map[string]string(svc.Deploy.Labels)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ingressProblems reports what is wrong with the tierplan.ingress.* labels of a loaded project, as
// problems at the label (map form) or at the labels block (list form), whichever target the stack is
// for (LBL-02). With a policy it also judges the hostnames against the environment's world (LBL-04,
// D-18). Nothing here reads a switch: a label that is declared is always judged, so no route is
// dropped without an error.
func ingressProblems(project *types.Project, idx *PositionIndex, in CheckInput) Problems {
	target := ingress.Target(in.Target)
	decls, found := ingress.ParseStack(serviceLabels(project), target)
	if in.IngressPolicy != nil {
		_, policy := ingress.Check(in.IngressPolicy.StackID, decls, in.IngressPolicy.World)
		found = append(found, policy...)
	}
	out := make(Problems, 0, len(found))
	for _, p := range found {
		out = append(out, ingressProblemAt(p, idx, in.Target))
	}
	return out
}

// ingressProblemAt places one ingress problem in the compose file. A swarm stack carries its labels
// under deploy.labels and a standalone one under labels; the two problems about the other place point
// at it. The position is the label's key when the labels are a map, the labels block when they are a
// list (a list item has no key of its own to name), and the service when there is no block.
func ingressProblemAt(p ingress.Problem, idx *PositionIndex, target Target) Problem {
	block := "labels"
	if target == TargetSwarm {
		block = "deploy.labels"
	}
	if p.Code == ingress.CodeLabelsWrongPlace || p.Code == ingress.CodeLabelsIgnored {
		if block == "labels" {
			block = "deploy.labels"
		} else {
			block = "labels"
		}
	}
	base := "services." + p.Service
	paths := make([]string, 0, 3)
	if p.Label != "" {
		paths = append(paths, base+"."+block+"."+p.Label)
	}
	paths = append(paths, base+"."+block, base)
	n, file := idx.locateAny(paths...)
	line, col, endLine, endCol := at(n)
	key := base + "." + block
	// A map-form label is found by its own key; a list-form one is only found as far as its block.
	if p.Label != "" && n != nil && n.Value == p.Label {
		key = paths[0]
	}
	return Problem{
		Severity: Severity(p.Severity), Code: p.Code, Message: p.Message, File: file, Key: key,
		Line: line, Col: col, EndLine: endLine, EndCol: endCol,
	}
}
