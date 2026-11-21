// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"slices"
	"sort"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// IngressPolicy is the world a stack's ingress labels are judged against: the environment's domains,
// the install-wide reserved names and the claims every stack holds. Check is pure, so the world
// arrives as data; the caller builds it from the store and the caller's permissions, and Check never
// reads a database or an ingress switch.
//
// It is the Phase 18 field of CheckInput; Phase 24's network attachment is CheckInput.Ingress.
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

// IngressAttach asks check to attach the routed services of a swarm stack to the shared ingress
// network (D-19, D-38). It is set only when ingress is enabled for a swarm environment; the routed
// set is derived inside check from the same parse the policy uses, so no caller can attach a
// different set.
type IngressAttach struct {
	// Network is the name of the shared overlay network (ingress.NetworkName).
	Network string
}

// InjectedIngress records what attachIngress added to a loaded project, so Render can show the
// project as the user wrote it (D-20) and drift can tell the attachment from the user's own.
type InjectedIngress struct {
	// Network is the network the services were attached to.
	Network string
	// NetworkAdded is true when the project did not declare the network and it was added as an
	// external one; false when the file already declared it.
	NetworkAdded bool
	// Services are the routed services that were given the attachment, sorted. A service that
	// already listed the network is not here.
	Services []string
	// DefaultAdded are the services of Services that had no networks of their own, so the
	// default network was written for them to keep them on it (Pitfall 7). Sorted.
	DefaultAdded []string
}

// IngressDeclarations are the routes the stack's labels declare, with the problems dropped (check
// reports those). target is the stack's target: a swarm stack's routes are under deploy.labels.
func IngressDeclarations(project *types.Project, target Target) []ingress.Declaration {
	decls, _ := ingress.ParseStack(serviceLabels(project), ingress.Target(target))
	return decls
}

// RoutedServices are the names of the services that declare at least one route, sorted.
func RoutedServices(project *types.Project, target Target) []string {
	var out []string
	for _, d := range IngressDeclarations(project, target) {
		if !slices.Contains(out, d.Service) {
			out = append(out, d.Service)
		}
	}
	sort.Strings(out)
	return out
}

// defaultNetworkKey is the key of the network a service joins when it names none.
const defaultNetworkKey = "default"

// attachIngress joins each routed service to the network, in the loaded project, once (D-19,
// Pattern 2). It is idempotent: a project that already declares the network, by key or by its name
// (external: true), keeps that declaration, and a service that already lists it is left alone. A
// service with no networks of its own gets the default network written first, because naming one
// network would otherwise take it off the default one. The attachment has no alias: the service
// name must never be a DNS name on the network every stack shares (D-37). It returns what it
// added, or nil when there was nothing to add.
func attachIngress(project *types.Project, routed []string, network string) *InjectedIngress {
	if len(routed) == 0 || network == "" {
		return nil
	}
	key, found := sharedNetworkKey(project, network)
	if !found && networkNameTaken(project, network) {
		// The file declares something else under the name or the key (a network of its own, or an
		// external one with another name). Check refuses that (ingress_network_reserved); nothing is
		// attached, and the declaration is never overwritten.
		return nil
	}
	inj := &InjectedIngress{Network: network}
	if !found {
		key = network
		if project.Networks == nil {
			project.Networks = types.Networks{}
		}
		project.Networks[key] = types.NetworkConfig{Name: network, External: true}
		inj.NetworkAdded = true
	}
	for _, name := range routed {
		svc, ok := project.Services[name]
		if !ok {
			continue
		}
		if _, has := svc.Networks[key]; has {
			continue
		}
		nets := make(map[string]*types.ServiceNetworkConfig, len(svc.Networks)+2)
		for k, v := range svc.Networks {
			nets[k] = v
		}
		if len(nets) == 0 {
			nets[defaultNetworkKey] = nil
			inj.DefaultAdded = append(inj.DefaultAdded, name)
		}
		nets[key] = nil
		svc.Networks = nets
		project.Services[name] = svc
		inj.Services = append(inj.Services, name)
	}
	sort.Strings(inj.Services)
	sort.Strings(inj.DefaultAdded)
	if !inj.NetworkAdded && len(inj.Services) == 0 {
		return nil
	}
	return inj
}

// undoIngress returns a copy of project with the injection taken out: what the user wrote. The
// project itself is never changed (Render is a view).
func undoIngress(project *types.Project, inj *InjectedIngress) *types.Project {
	cp := *project
	if inj.NetworkAdded {
		nets := make(types.Networks, len(project.Networks))
		for k, v := range project.Networks {
			if k != inj.Network {
				nets[k] = v
			}
		}
		cp.Networks = nets
	}
	services := make(types.Services, len(project.Services))
	for name, svc := range project.Services {
		services[name] = svc
	}
	for _, name := range inj.Services {
		svc, ok := services[name]
		if !ok {
			continue
		}
		nets := make(map[string]*types.ServiceNetworkConfig, len(svc.Networks))
		for k, v := range svc.Networks {
			nets[k] = v
		}
		delete(nets, inj.injectedKey(project))
		if slices.Contains(inj.DefaultAdded, name) {
			delete(nets, defaultNetworkKey)
		}
		if len(nets) == 0 {
			nets = nil
		}
		svc.Networks = nets
		services[name] = svc
	}
	cp.Services = services
	return &cp
}

// injectedKey is the key the network has in project.
func (inj *InjectedIngress) injectedKey(project *types.Project) string {
	if k, ok := sharedNetworkKey(project, inj.Network); ok {
		return k
	}
	return inj.Network
}

// sharedNetworkKey is the key under which project declares the shared network, found by its resolved
// name: only an external declaration of that name is the shared network (compose-go gives an external
// network without a name its key as the name). A network the stack would create itself is not.
func sharedNetworkKey(project *types.Project, network string) (string, bool) {
	keys := make([]string, 0, len(project.Networks))
	for k := range project.Networks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if n := project.Networks[k]; bool(n.External) && n.Name == network {
			return k, true
		}
	}
	return "", false
}

// networkNameTaken is true when project declares a network whose key or name is the shared network's.
func networkNameTaken(project *types.Project, network string) bool {
	for k, n := range project.Networks {
		if k == network || n.Name == network {
			return true
		}
	}
	return false
}

// CodeIngressNetworkReserved is a compose file that declares the shared ingress network's name as a
// network of its own: the name belongs to TierPlan (WR-09), so a stack may only point at that network
// as an external one.
const CodeIngressNetworkReserved = "ingress_network_reserved"

// networkReservedProblems reports each top-level network whose key or resolved name is the shared
// ingress network's, unless it is the external declaration of that network. The error is at the
// network's key, for every target.
func networkReservedProblems(project *types.Project, idx *PositionIndex) Problems {
	var out Problems
	for _, key := range sortedKeys(project.Networks) {
		n := project.Networks[key]
		if key != ingress.NetworkName && n.Name != ingress.NetworkName {
			continue
		}
		if bool(n.External) && n.Name == ingress.NetworkName {
			continue
		}
		node, file := idx.locateAny("networks."+key+".name", "networks."+key)
		line, col, endLine, endCol := at(node)
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeIngressNetworkReserved, File: file, Key: "networks." + key,
			Line: line, Col: col, EndLine: endLine, EndCol: endCol,
			Message: "The network name " + ingress.NetworkName + " is TierPlan's own for the network ingress routes use. To reach it, declare it with external: true; for a network of your own, use another name.",
		})
	}
	return out
}

// CodeIngressComponentLabel is a compose file that sets TierPlan's own component label on a service
// or on a top-level object (D-41): the label marks the objects of the ingress deployment, and a
// stack must not be able to make its objects look like them.
const CodeIngressComponentLabel = "ingress_component_label"

// componentLabelProblems reports every place the compose file sets ingress.LabelComponent (the key,
// byte for byte): a service's labels and deploy.labels, and the labels of a top-level network,
// volume, secret and config. Each is an error at the label's key, or at the labels block when the
// labels are a list, for every target.
func componentLabelProblems(project *types.Project, idx *PositionIndex) Problems {
	var out Problems
	report := func(base, block string, labels map[string]string) {
		if _, ok := labels[ingress.LabelComponent]; !ok {
			return
		}
		paths := []string{base + "." + block + "." + ingress.LabelComponent, base + "." + block, base}
		n, file := idx.locateAny(paths...)
		line, col, endLine, endCol := at(n)
		key := base + "." + block
		if n != nil && n.Value == ingress.LabelComponent {
			key = paths[0]
		}
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeIngressComponentLabel, File: file, Key: key,
			Line: line, Col: col, EndLine: endLine, EndCol: endCol,
			Message: ingress.LabelComponent + " is TierPlan's own label for its ingress objects; remove it from the compose file.",
		})
	}
	for _, name := range sortedKeys(project.Services) {
		svc := project.Services[name]
		report("services."+name, "labels", svc.Labels)
		if svc.Deploy != nil {
			report("services."+name, "deploy.labels", svc.Deploy.Labels)
		}
	}
	for _, name := range sortedKeys(project.Networks) {
		report("networks."+name, "labels", project.Networks[name].Labels)
	}
	for _, name := range sortedKeys(project.Volumes) {
		report("volumes."+name, "labels", project.Volumes[name].Labels)
	}
	for _, name := range sortedKeys(project.Secrets) {
		report("secrets."+name, "labels", project.Secrets[name].Labels)
	}
	for _, name := range sortedKeys(project.Configs) {
		report("configs."+name, "labels", project.Configs[name].Labels)
	}
	return out
}
