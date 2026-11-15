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
//   - the hostname is under a domain registered for the stack's environment;
//   - the hostname is not held by another stack or by the system, in any environment.
//
// A stack with no declarations always passes, with an empty plan: that is how a stack lets go of
// its hostnames. A declaration with a problem is left out of the plan.
func Check(stackID string, decls []Declaration, w World) (Plan, []Problem) {
	if len(decls) == 0 {
		return Plan{}, nil
	}
	var problems []Problem
	byHost := map[string]*PlannedClaim{}
	refused := map[string]bool{}
	for _, d := range decls {
		if refused[d.Host] {
			continue
		}
		if c, ok := byHost[d.Host]; ok {
			c.Routes = append(c.Routes, d)
			continue
		}
		if p, bad := judgeHost(stackID, d, w); bad {
			problems = append(problems, p)
			refused[d.Host] = true
			continue
		}
		dom, _ := MatchDomain(d.Host, w.Domains)
		byHost[d.Host] = &PlannedClaim{Hostname: d.Host, DomainID: dom.ID, Routes: []Declaration{d}}
	}
	plan := Plan{Claims: make([]PlannedClaim, 0, len(byHost))}
	for _, c := range byHost {
		plan.Claims = append(plan.Claims, *c)
	}
	sort.Slice(plan.Claims, func(i, j int) bool { return plan.Claims[i].Hostname < plan.Claims[j].Hostname })
	return plan, problems
}

// judgeHost applies the hostname rules to one declaration. bad is false when the stack may have the
// hostname.
func judgeHost(stackID string, d Declaration, w World) (p Problem, bad bool) {
	p = Problem{Severity: SeverityError, Service: d.Service, Label: d.Label}
	if _, ok := MatchDomain(d.Host, w.Domains); !ok {
		p.Code = CodeDomainNotAllowed
		p.Message = "“" + d.Host + "” is not under a domain registered for this environment. Ask an Admin to register the domain, or choose a name under one that is."
		return p, true
	}
	if c, held := w.Claims[d.Host]; held && !(c.OwnerKind == OwnerStack && c.StackID == stackID) {
		p.Code = CodeHostTaken
		switch {
		case c.OwnerKind == OwnerSystem:
			p.Message = "“" + d.Host + "” is the name TierPlan itself is served on. Choose another name."
		case w.CanView != nil && w.CanView(c.EnvID):
			p.Message = "“" + d.Host + "” is already held by the stack “" + c.StackName + "” in the environment “" + c.EnvName + "”. A name has one owner across TierPlan. Choose another name."
		default:
			p.Message = "“" + d.Host + "” is already claimed in an environment you can't see. A name has one owner across TierPlan. Choose another name."
		}
		return p, true
	}
	return Problem{}, false
}
