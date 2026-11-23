// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
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

// IngressAttachRow is a service that joins or leaves the shared ingress network. A change of the service's networks
// replaces what runs: its tasks on a swarm, its containers on a standalone host (compose recreates a container whose
// networks change). ReplacesTasks says so; a service that does not exist yet has nothing to replace.
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

// routeKey is what makes two routes the same route: where it matches, what it goes to and how it is forwarded, which
// is every field the store compares (hostname, name, path, service, port, scheme and strip). A route that changes in any
// of them reads as one released and one added.
type routeKey struct {
	host, name, path, service, scheme string
	port                              int
	strip                             bool
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
// unchanged in hostname, name, path, service, port, scheme and strip, is kept; one only declared is added; one only held is
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
		add(routeKey{host: ingress.NormalizeHost(r.Hostname), name: r.Name, path: pathOf(r.PathPrefix), service: r.Service, scheme: schemeOf(r.Scheme), port: r.Port, strip: r.Strip}, true)
	}
	for _, d := range decls {
		add(routeKey{host: ingress.NormalizeHost(d.Host), name: d.Name, path: pathOf(d.PathPrefix), service: d.Service, scheme: schemeOf(d.Scheme), port: d.Port, strip: d.Strip}, false)
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

// declaredHosts are the distinct hostnames the declarations name, sorted.
func declaredHosts(decls []ingress.Declaration) []string {
	hosts := make([]string, 0, len(decls))
	for _, d := range decls {
		hosts = append(hosts, ingress.NormalizeHost(d.Host))
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// certificateRows is one row for each hostname the revision declares that the ingress model could say something
// about, ordered by hostname. It reads nothing but the states it is given.
func certificateRows(hosts []string, states map[string]CertState) []IngressCertRow {
	rows := make([]IngressCertRow, 0, len(hosts))
	for _, h := range hosts {
		st, ok := states[h]
		if !ok {
			continue
		}
		row := IngressCertRow{Hostname: h, State: st.State}
		if st.State == CertWaiting {
			row.Reason = st.Reason
		}
		rows = append(rows, row)
	}
	return rows
}

// hasIngressNetwork reports whether a service spec lists the shared ingress network among its networks. A converted
// spec names an object it found on the swarm by its ID, and a live spec's IDs are turned to names (liveSpecs), so the
// network is known by its name and by ids, the IDs of the network the swarm holds under that name.
func hasIngressNetwork(spec swarm.ServiceSpec, ids []string) bool {
	return len(stacks.StripIngressAttachment(spec, ids...).TaskTemplate.Networks) != len(spec.TaskTemplate.Networks)
}

// ingressNetworkIDs are the IDs of the networks the swarm holds under the shared ingress network's name.
func ingressNetworkIDs(all []dockerx.NetworkRow) []string {
	var ids []string
	for _, n := range all {
		if n.Name == ingress.NetworkName {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// networkRows is the services that join or leave the shared ingress network. A service that runs and whose desired
// spec adds the network joins it, and a service that runs on it and whose desired spec drops it leaves it: both
// replace its tasks, because a spec change of TaskTemplate.Networks does. A service that is made new joins with
// nothing to replace. A service that is removed is its own row of the service list and says nothing here. short maps
// a swarm service name to the name in the compose file; the rows are ordered by that name.
func networkRows(desired, running map[string]swarm.ServiceSpec, short map[string]string, ids []string) []IngressAttachRow {
	var rows []IngressAttachRow
	for name, spec := range desired {
		now, exists := running[name]
		want, had := hasIngressNetwork(spec, ids), exists && hasIngressNetwork(now, ids)
		switch {
		case want && !had:
			rows = append(rows, IngressAttachRow{Service: shortOf(short, name), Change: IngressAttachJoins, ReplacesTasks: exists})
		case !want && had:
			rows = append(rows, IngressAttachRow{Service: shortOf(short, name), Change: IngressAttachLeaves, ReplacesTasks: true})
		}
	}
	slices.SortFunc(rows, func(a, b IngressAttachRow) int {
		if c := strings.Compare(a.Service, b.Service); c != 0 {
			return c
		}
		return strings.Compare(a.Change, b.Change)
	})
	return rows
}

func shortOf(short map[string]string, name string) string {
	if s, ok := short[name]; ok && s != "" {
		return s
	}
	return name
}

// shortNames maps the swarm service name of each planned service to its name in the compose file.
func shortNames(services []convert.ServicePlan) map[string]string {
	out := make(map[string]string, len(services))
	for _, sp := range services {
		out[sp.Name] = sp.Short
	}
	return out
}

// ingressWarnings are the warnings a review gives for the routed services of a project (D-13, D-23). The services
// are the ones decls name; none of them is an error. off says the environment's ingress is switched off, which one
// warning (ingress_disabled) tells once, at the first routed service. A service the file does not hold is not judged.
// The problems are written against compose.yaml at the service's key, for the caller to place.
//
// target and stopped carry what differs on a standalone host: endpoint_mode has no meaning there, so ingress_dnsrr is
// never given, and a stopped stack runs no container for any routed service, so each of them has no replicas.
func ingressWarnings(project *types.Project, decls []ingress.Declaration, off bool, target stacks.Target, stopped bool) stacks.Problems {
	if project == nil || len(decls) == 0 {
		return nil
	}
	routed := make([]string, 0, len(decls))
	for _, d := range decls {
		if !slices.Contains(routed, d.Service) {
			routed = append(routed, d.Service)
		}
	}
	slices.Sort(routed)
	warn := func(code, service, msg string) stacks.Problem {
		return stacks.Problem{
			Severity: stacks.SeverityWarning, Code: code, Message: msg, File: stacks.ComposeFile, Key: "services." + service,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		}
	}
	standalone := target == stacks.TargetStandalone
	var out stacks.Problems
	if off {
		out = append(out, warn(CodeIngressDisabled, routed[0], messageIngressDisabled))
	}
	for _, name := range routed {
		svc, ok := project.Services[name]
		if !ok {
			continue
		}
		if svc.HealthCheck == nil || svc.HealthCheck.Disable {
			out = append(out, warn(CodeIngressNoHealth, name, name+messageIngressNoHealth))
		}
		if !standalone && svc.Deploy != nil && svc.Deploy.EndpointMode == "dnsrr" {
			out = append(out, warn(CodeIngressDNSRR, name, name+messageIngressDNSRR))
		}
		if (standalone && stopped) || (svc.Deploy != nil && svc.Deploy.Replicas != nil && *svc.Deploy.Replicas == 0) || (svc.Scale != nil && *svc.Scale == 0) {
			out = append(out, warn(CodeIngressNoReplicas, name, name+messageIngressNoReplicas))
		}
	}
	return out
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
	sec := &IngressReview{Routes: routeRows(held, decls, note)}
	if hosts := declaredHosts(decls); len(hosts) > 0 {
		states, err := d.Ingress.CertStates(ctx, in.EnvID, hosts)
		if err != nil {
			// The certificates are told where they can be read; the routes still are.
			d.log().Warn("could not read the certificate states of a stack for its review", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		} else {
			sec.Certificates = certificateRows(hosts, states)
		}
	}
	return sec
}

// fillIngressSwarm sets Review.Ingress and adds the ingress warnings of a review of a swarm stack: the revision's
// project (the one the review converted), the specs a deploy would write (desired) against the ones that run. An update
// of images changes no route, so it has no section.
func (d Deps) fillIngressSwarm(ctx context.Context, in ReviewInput, rv *Review, project *types.Project, plan []convert.ServicePlan, desired, running map[string]swarm.ServiceSpec, live liveStack) {
	if in.Mode == ReviewModeUpdate || project == nil {
		return
	}
	decls := stacks.IngressDeclarations(project, in.Stack.Target)
	sec := d.ingressSection(ctx, in, decls, "")
	if sec != nil {
		sec.Network = networkRows(desired, running, shortNames(plan), ingressNetworkIDs(live.AllNetworks))
	}
	if !sec.empty() {
		rv.Ingress = sec
	}
	rv.Problems = append(rv.Problems, placeProblemsOf(in.Copy, ingressWarnings(project, decls, in.IngressOff, stacks.TargetSwarm, false))...)
}

// fillIngressStandalone sets Review.Ingress and adds the ingress warnings of a review of a stack of a standalone host,
// as fillIngressSwarm does for a swarm: the routes and the certificates, the services that join or leave the shared
// bridge (each with the container recreate that causes) and the warnings that apply to one host. An update of images
// changes no route, so it has no section.
func (d Deps) fillIngressStandalone(ctx context.Context, in ReviewInput, rv *Review, project *types.Project) {
	if in.Mode == ReviewModeUpdate || project == nil {
		return
	}
	decls := stacks.IngressDeclarations(project, in.Stack.Target)
	sec := d.ingressSection(ctx, in, decls, "")
	if sec != nil {
		sec.Network = d.standaloneAttachRows(ctx, in, project)
	}
	if !sec.empty() {
		rv.Ingress = sec
	}
	rv.Problems = append(rv.Problems, placeProblemsOf(in.Copy, ingressWarnings(project, decls, in.IngressOff, stacks.TargetStandalone, in.Stack.Stopped))...)
}

// joinsIngress reports whether a service of the prepared project lists the shared ingress network, by the name the
// network has on the host.
func joinsIngress(project *types.Project, svc types.ServiceConfig) bool {
	for key := range svc.Networks {
		name := key
		if cfg, ok := project.Networks[key]; ok && cfg.Name != "" {
			name = cfg.Name
		}
		if name == ingress.NetworkName {
			return true
		}
	}
	return false
}

// standaloneAttachRows is the services of a standalone stack that join or leave the shared ingress network: what the
// prepared project asks for against what the stack's containers are attached to now. Compose recreates a container
// whose networks change, so a service that has containers and changes either way replaces them; a service with none
// joins with nothing to replace. A host that cannot be read leaves the rows out (the routes still show), because a list
// built on a guess would call every service joining.
func (d Deps) standaloneAttachRows(ctx context.Context, in ReviewInput, project *types.Project) []IngressAttachRow {
	node, err := d.StandaloneNode(ctx, in.EnvID)
	if err != nil {
		d.log().Warn("could not find the host of a stack to review its ingress attachment", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return nil
	}
	var rows []dockerx.ContainerRow
	if err := d.Aggregator.OnNode(ctx, in.EnvID, node, func(c *dockerx.Client) error {
		var e error
		rows, e = c.ContainersWithLabel(ctx, stacks.LabelComposeProject, in.Stack.Name)
		return e
	}); err != nil {
		d.log().Warn("could not read the containers of a stack to review its ingress attachment", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return nil
	}
	has, attached := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		svc := r.Labels[stacks.LabelComposeService]
		has[svc] = true
		if slices.ContainsFunc(r.Networks, func(n dockerx.ContainerNet) bool { return n.Name == ingress.NetworkName }) {
			attached[svc] = true
		}
	}
	var out []IngressAttachRow
	for name, svc := range project.Services {
		want, had := joinsIngress(project, svc), attached[name]
		switch {
		case want && !had:
			out = append(out, IngressAttachRow{Service: name, Change: IngressAttachJoins, ReplacesTasks: has[name]})
		case !want && had:
			out = append(out, IngressAttachRow{Service: name, Change: IngressAttachLeaves, ReplacesTasks: true})
		}
	}
	slices.SortFunc(out, func(a, b IngressAttachRow) int {
		if c := strings.Compare(a.Service, b.Service); c != 0 {
			return c
		}
		return strings.Compare(a.Change, b.Change)
	})
	return out
}
