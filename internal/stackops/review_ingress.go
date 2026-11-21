// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// What a deploy, a roll back or a restore does to ingress, as the review lists it before anything is changed (D-22,
// D-23, DEP-03). The section is read, never written: the routes the stack holds now against the ones the revision
// declares, the certificates those hostnames will have, and the services that join or leave the shared network.

// The changes of a route row.
const (
	IngressRouteAdded    = "added"
	IngressRouteKept     = "kept"
	IngressRouteReleased = "released"
)

// The changes of an attachment row.
const (
	IngressAttachJoins  = "joins"
	IngressAttachLeaves = "leaves"
)

// NoteNotServedYet is the note of a route of a standalone host: it is claimed but nothing serves it there yet.
const NoteNotServedYet = "Not served on this host yet."

// The codes of the warnings a review gives for a routed service (D-13, D-23). They are warnings: they never stop a
// deploy.
const (
	CodeIngressDisabled   = "ingress_disabled"
	CodeIngressNoHealth   = "ingress_no_healthcheck"
	CodeIngressDNSRR      = "ingress_dnsrr"
	CodeIngressNoReplicas = "ingress_no_replicas"
)

// The sentences of the warnings, worded as the UI contract has them. {service} is the name in the compose file.
const (
	messageIngressDisabled   = "Ingress is off for this environment. The routes will serve once it is switched on and the stack is redeployed."
	messageIngressNoHealth   = " has no health check, so a starting task can get traffic before it is ready."
	messageIngressDNSRR      = " uses endpoint_mode: dnsrr, so its name resolves to task addresses and not one stable address. The proxy can keep sending traffic to a replaced task until it reconnects."
	messageIngressNoReplicas = " has no replicas, so its route will show Backend not running."
)

// IngressRouteRow is one route of the review: Change is added, kept or released. Path is the prefix as the proxy
// matches it ("/" for none).
type IngressRouteRow struct {
	Change, Hostname, Path, Service string
	Port                            int
	Scheme                          string
	Note                            string
}

// IngressCertRow is the certificate of one hostname the stack will hold: State is reused, will_issue or waiting, and
// Reason says why for waiting.
type IngressCertRow struct {
	Hostname, State, Reason string
}

// IngressAttachRow is a service that joins or leaves the shared ingress network. A spec change of the service's
// networks replaces its running tasks, which ReplacesTasks says; a service that does not exist yet has none to replace.
type IngressAttachRow struct {
	Service, Change string
	ReplacesTasks   bool
}

// IngressReview is the ingress section of a review. It is nil when there is nothing to say: no route, no certificate
// and no attachment change.
type IngressReview struct {
	Routes       []IngressRouteRow
	Certificates []IngressCertRow
	Network      []IngressAttachRow
}

// empty reports whether the section has no row at all.
func (r *IngressReview) empty() bool {
	return r == nil || (len(r.Routes) == 0 && len(r.Certificates) == 0 && len(r.Network) == 0)
}

// routeKey is what makes two routes the same route: where it matches, and what it goes to.
type routeKey struct {
	host, path, service, scheme string
	port                        int
}

func pathOf(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func schemeOf(s ingress.Scheme) string {
	if s == "" {
		return string(ingress.SchemeHTTP)
	}
	return string(s)
}

// routeRows is the routes the stack holds now (held) against the ones its revision declares (decls): a route in both,
// unchanged in hostname, path, service, port and scheme, is kept; one only declared is added; one only held is
// released. The rows are ordered by hostname, then path, then change, so two calls agree. note is put on the rows of
// routes that will be held (added and kept).
func routeRows(held []ingress.Route, decls []ingress.Declaration, note string) []IngressRouteRow {
	type entry struct {
		key        routeKey
		held, decl bool
	}
	byKey := map[routeKey]*entry{}
	var order []*entry
	add := func(k routeKey, isHeld bool) {
		e, ok := byKey[k]
		if !ok {
			e = &entry{key: k}
			byKey[k] = e
			order = append(order, e)
		}
		if isHeld {
			e.held = true
		} else {
			e.decl = true
		}
	}
	for _, r := range held {
		add(routeKey{host: ingress.NormalizeHost(r.Hostname), path: pathOf(r.PathPrefix), service: r.Service, scheme: schemeOf(r.Scheme), port: r.Port}, true)
	}
	for _, d := range decls {
		add(routeKey{host: ingress.NormalizeHost(d.Host), path: pathOf(d.PathPrefix), service: d.Service, scheme: schemeOf(d.Scheme), port: d.Port}, false)
	}
	rows := make([]IngressRouteRow, 0, len(order))
	for _, e := range order {
		row := IngressRouteRow{Hostname: e.key.host, Path: e.key.path, Service: e.key.service, Port: e.key.port, Scheme: e.key.scheme}
		switch {
		case e.held && e.decl:
			row.Change, row.Note = IngressRouteKept, note
		case e.decl:
			row.Change, row.Note = IngressRouteAdded, note
		default:
			row.Change = IngressRouteReleased
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b IngressRouteRow) int {
		if c := strings.Compare(a.Hostname, b.Hostname); c != 0 {
			return c
		}
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Change, b.Change); c != 0 {
			return c
		}
		if c := strings.Compare(a.Service, b.Service); c != 0 {
			return c
		}
		return a.Port - b.Port
	})
	return rows
}

// ingressSection is the routes and certificates of the section for the revision that declares decls, read against what
// the stack holds now. It is nil when the server has no ingress model, and when the stack's holdings cannot be read: a
// list built on a guess would call every route added. note is put on the routes that will be held.
func (d Deps) ingressSection(ctx context.Context, in ReviewInput, decls []ingress.Declaration, note string) *IngressReview {
	if d.Ingress == nil {
		return nil
	}
	held, err := d.Ingress.HeldRoutes(ctx, in.EnvID, in.Stack.ID)
	if err != nil {
		d.log().Warn("could not read the routes a stack holds for its review", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return nil
	}
	return &IngressReview{Routes: routeRows(held, decls, note)}
}

// fillIngressSwarm sets Review.Ingress of a review of a swarm stack from the revision's project. An update of images
// changes no route, so it has no section.
func (d Deps) fillIngressSwarm(ctx context.Context, in ReviewInput, rv *Review, project *types.Project) {
	if in.Mode == ReviewModeUpdate || project == nil {
		return
	}
	decls := stacks.IngressDeclarations(project, in.Stack.Target)
	if sec := d.ingressSection(ctx, in, decls, ""); !sec.empty() {
		rv.Ingress = sec
	}
}

// fillIngressStandalone sets Review.Ingress of a review of a stack of a standalone host: the routes and the
// certificates, with the note that nothing serves them on the host yet, and no network rows (the host's attachment
// is a later phase). There are no warnings: they are about a swarm service.
func (d Deps) fillIngressStandalone(ctx context.Context, in ReviewInput, rv *Review, project *types.Project) {
	if in.Mode == ReviewModeUpdate || project == nil {
		return
	}
	decls := stacks.IngressDeclarations(project, in.Stack.Target)
	if sec := d.ingressSection(ctx, in, decls, NoteNotServedYet); !sec.empty() {
		rv.Ingress = sec
	}
}
