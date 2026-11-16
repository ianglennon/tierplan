// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"sort"
)

// World is what the install already holds, read in one go, that a stack's declarations are judged
// against. Domains and Reserved are the stack's own environment's; Claims are install-wide, because
// a hostname has one owner across every environment.
type World struct {
	// Domains are the environment's registered domains.
	Domains []Domain
	// Reserved holds the hostnames an admin blocked, normalized.
	Reserved map[string]bool
	// Claims holds every claim in the install by hostname, with the names a message about it uses.
	Claims map[string]ClaimInfo
	// CanView says whether the caller may see an environment. A nil CanView means no environment
	// may be named: a message about another environment's claim then names neither stack nor
	// environment.
	CanView func(envID string) bool
}

// PlannedClaim is a hostname a stack will hold once its plan is stored, with its routes.
type PlannedClaim struct {
	Hostname string
	DomainID string
	Routes   []Declaration
}

// Plan is what a stack's declarations come to: the hostnames it will hold. Storing a plan replaces
// everything the stack held before, so a hostname that is not in it is released.
type Plan struct {
	Claims []PlannedClaim
}

// Hostnames returns the plan's hostnames, sorted.
func (p Plan) Hostnames() []string {
	out := make([]string, 0, len(p.Claims))
	for _, c := range p.Claims {
		out = append(out, c.Hostname)
	}
	sort.Strings(out)
	return out
}

// Check judges the declarations of the stack stackID against the world and returns the plan to
// store and every problem. It is the one place that decides whether a stack may have a hostname:
// the stack check calls it to show the answer, and the store calls it again inside its write
// transaction before it stores anything. It reads no ingress switch.
//
// The rules, each an error:
//   - no two declarations of the stack serve the same host and path;
//   - the hostname is under a domain registered for the stack's environment, and is not the
//     domain's own suffix; the longest matching suffix is the domain the claim is made under;
//   - the hostname is not reserved, under any environment's domain;
//   - the hostname is not held by another stack or by the system, in any environment.
//
// A stack with no declarations always passes, with an empty plan: that is how a stack lets go of
// its hostnames. A declaration with a problem is left out of the plan, and a hostname refused once
// is not judged again. The plan's claims are sorted by hostname and each claim's routes by path,
// then service, then name; problems are in declaration order.
func Check(stackID string, decls []Declaration, w World) (Plan, []Problem) {
	if len(decls) == 0 {
		return Plan{}, nil
	}
	var problems []Problem
	byHost := map[string]*PlannedClaim{}
	refused := map[string]bool{}
	served := map[string]bool{}
	for _, d := range decls {
		key := d.Host + "\x00" + d.PathPrefix
		if served[key] {
			problems = append(problems, Problem{
				Severity: SeverityError, Code: CodeRouteDuplicate, Service: d.Service, Label: d.Label,
				Message: "More than one route serves “" + d.Host + d.PathPrefix + "”. Give each route its own host or path.",
			})
			continue
		}
		served[key] = true
		if refused[d.Host] {
			continue
		}
		if c, ok := byHost[d.Host]; ok {
			c.Routes = append(c.Routes, d)
			continue
		}
		dom, p, bad := judgeHost(stackID, d, w)
		if bad {
			problems = append(problems, p)
			refused[d.Host] = true
			continue
		}
		byHost[d.Host] = &PlannedClaim{Hostname: d.Host, DomainID: dom.ID, Routes: []Declaration{d}}
	}
	plan := Plan{Claims: make([]PlannedClaim, 0, len(byHost))}
	for _, c := range byHost {
		sort.Slice(c.Routes, func(i, j int) bool {
			a, b := c.Routes[i], c.Routes[j]
			if a.PathPrefix != b.PathPrefix {
				return a.PathPrefix < b.PathPrefix
			}
			if a.Service != b.Service {
				return a.Service < b.Service
			}
			return a.Name < b.Name
		})
		plan.Claims = append(plan.Claims, *c)
	}
	sort.Slice(plan.Claims, func(i, j int) bool { return plan.Claims[i].Hostname < plan.Claims[j].Hostname })
	return plan, problems
}

// judgeHost applies the hostname rules to one declaration and returns the domain the hostname is
// under. bad is false when the stack may have the hostname.
func judgeHost(stackID string, d Declaration, w World) (dom Domain, p Problem, bad bool) {
	p = Problem{Severity: SeverityError, Service: d.Service, Label: d.Label}
	dom, ok := MatchDomain(d.Host, w.Domains)
	if !ok {
		p.Code = CodeDomainNotAllowed
		p.Message = "“" + d.Host + "” is not under a domain registered for this environment. Ask an Admin to register the domain, or choose a name under one that is."
		return dom, p, true
	}
	if w.Reserved[d.Host] {
		p.Code = CodeHostReserved
		p.Message = "“" + d.Host + "” is reserved by an Admin and cannot be used by a stack. Choose another name."
		return dom, p, true
	}
	if c, held := w.Claims[d.Host]; held && (c.OwnerKind != OwnerStack || c.StackID != stackID) {
		p.Code = CodeHostTaken
		c.Hostname = d.Host
		p.Message = DescribeHolder(c, w.CanView)
		if c.OwnerKind == OwnerStack {
			p.Message += " A name has one owner across TierPlan."
		}
		p.Message += " Choose another name."
		return dom, p, true
	}
	return dom, Problem{}, false
}

// DescribeHolder says in one sentence who holds a claimed hostname. It is the one place that
// decides what a person may be told about another environment's claim: a system claim names
// TierPlan's own address and no stack; a stack's claim names the stack and its environment only
// when canView says the caller may see that environment, and a nil canView means none may be seen.
// Otherwise the sentence says the name is claimed in an environment the caller can't see and
// carries no name and no id. The Check problem, the store's refusal to reserve a held name and the
// API all use it, so they redact alike.
func DescribeHolder(c ClaimInfo, canView func(envID string) bool) string {
	switch {
	case c.OwnerKind == OwnerSystem:
		return "“" + c.Hostname + "” is the name TierPlan itself is served on."
	case canView != nil && canView(c.EnvID):
		return "“" + c.Hostname + "” is already held by the stack “" + c.StackName + "” in the environment “" + c.EnvName + "”."
	default:
		return "“" + c.Hostname + "” is already claimed in an environment you can't see."
	}
}
