// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultRouteName is the name of the route the short label form declares.
//
// The labels are the user's, like tierplan.keep: they are an on-host contract written into compose
// files, so a rename needs a dual read for as long as labelled stacks live. The short form
// declares one route, named "default":
//
//	tierplan.ingress.host: app.apps.example.com
//	tierplan.ingress.port: "8080"
//
// The named form declares several routes on one service:
//
//	tierplan.ingress.api.host: api.apps.example.com
//	tierplan.ingress.api.port: "9000"
//
// Each route takes host and port, and optionally path (a prefix, "/" when absent), scheme (http or
// https to the backend, http when absent) and strip (true to remove the prefix before the request
// reaches the backend, false when absent).
const DefaultRouteName = "default"

// The fields of a route.
const (
	fieldHost   = "host"
	fieldPort   = "port"
	fieldPath   = "path"
	fieldScheme = "scheme"
	fieldStrip  = "strip"
)

// maxPathLen bounds a path prefix, in bytes.
const maxPathLen = 255

// routeNameRE is a route name: lower-case letters, digits and inner hyphens, 32 at most.
var routeNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// IsLabel reports whether key is an ingress label.
func IsLabel(key string) bool { return strings.HasPrefix(key, LabelPrefix) }

// HasLabels reports whether any key of labels is an ingress label.
func HasLabels(labels map[string]string) bool {
	for k := range labels {
		if IsLabel(k) {
			return true
		}
	}
	return false
}

// isField reports whether s is one of the route fields.
func isField(s string) bool {
	switch s {
	case fieldHost, fieldPort, fieldPath, fieldScheme, fieldStrip:
		return true
	}
	return false
}

// rawRoute is the labels of one route, as written.
type rawRoute struct {
	fields map[string]string
	keys   map[string]string // field to the label key that set it
}

// ParseServiceLabels reads the routes one service declares. It never fails: everything wrong is a
// Problem, and a route with an error is left out of the result, so the caller can show every
// problem of a file at once. A label under the ingress prefix is never ignored without an error: a
// key that means nothing is ingress_label_unknown, and a route with a missing field is reported
// for that field. The declarations come back sorted by route name.
func ParseServiceLabels(service string, labels map[string]string) ([]Declaration, []Problem) {
	raw := map[string]*rawRoute{}
	var problems []Problem
	bad := func(code, key, format string, args ...any) {
		problems = append(problems, Problem{
			Severity: SeverityError, Code: code, Service: service, Label: key, Message: fmt.Sprintf(format, args...),
		})
	}
	for _, key := range sortedKeys(labels) {
		if !IsLabel(key) {
			continue
		}
		name, field, ok := splitKey(key)
		switch {
		case !ok:
			bad(CodeLabelUnknown, key, "“%s” is not an ingress label. Use %shost, %sport, %spath, %sscheme or %sstrip, or put a route name before the field, as in %sapi.host.",
				key, LabelPrefix, LabelPrefix, LabelPrefix, LabelPrefix, LabelPrefix, LabelPrefix)
			continue
		case name == DefaultRouteName && strings.Count(key, ".") == 3:
			// tierplan.ingress.default.host: the name the short form already uses.
			bad(CodeRouteNameInvalid, key, "“%s” names the route “%s”, which is the name of the short form. Write %s%s instead.",
				key, name, LabelPrefix, field)
			continue
		case name != DefaultRouteName && !routeNameRE.MatchString(name):
			bad(CodeRouteNameInvalid, key, "The route name “%s” in “%s” must be lower-case letters, digits and hyphens, 32 characters at most, and start and end with a letter or a digit.",
				name, key)
			continue
		}
		r := raw[name]
		if r == nil {
			r = &rawRoute{fields: map[string]string{}, keys: map[string]string{}}
			raw[name] = r
		}
		r.fields[field] = strings.TrimSpace(labels[key])
		r.keys[field] = key
	}

	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Declaration
	for _, name := range names {
		d, ps := buildDeclaration(service, name, raw[name])
		problems = append(problems, ps...)
		if !HasErrors(ps) {
			out = append(out, d)
		}
	}
	return out, problems
}

// splitKey reads the route name and the field out of an ingress label key. ok is false for a key
// that has neither form.
func splitKey(key string) (name, field string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(key, LabelPrefix), ".")
	switch len(parts) {
	case 1:
		return DefaultRouteName, parts[0], isField(parts[0])
	case 2:
		// A field word cannot be a route name: tierplan.ingress.host.port would read both ways.
		return parts[0], parts[1], isField(parts[1]) && !isField(parts[0])
	default:
		return "", "", false
	}
}

// buildDeclaration turns one route's labels into a Declaration, or says what is wrong with them.
func buildDeclaration(service, name string, r *rawRoute) (Declaration, []Problem) {
	var problems []Problem
	bad := func(code, field, format string, args ...any) {
		problems = append(problems, Problem{
			Severity: SeverityError, Code: code, Service: service, Label: r.keys[field], Message: fmt.Sprintf(format, args...),
		})
	}
	d := Declaration{Service: service, Name: name, Label: r.keys[fieldHost], PathPrefix: "/", Scheme: SchemeHTTP}

	if v := r.fields[fieldHost]; v == "" {
		bad(CodeHostMissing, fieldHost, "The route “%s” of service “%s” has no host. Add %s.", name, service, labelKey(name, fieldHost))
	} else {
		d.Host = NormalizeHost(v)
		if err := ValidateHost(d.Host); err != nil {
			bad(CodeHostInvalid, fieldHost, "The host “%s” of route “%s” is not valid: %v.", v, name, err)
		}
	}

	if v := r.fields[fieldPort]; v == "" {
		bad(CodePortMissing, fieldPort, "The route “%s” of service “%s” has no port. Add %s, the port the service listens on.", name, service, labelKey(name, fieldPort))
	} else if p, ok := parsePort(v); !ok {
		bad(CodePortInvalid, fieldPort, "The port “%s” of route “%s” must be digits only, from 1 to 65535.", v, name)
	} else {
		d.Port = p
	}

	pathOK := true
	if v := r.fields[fieldPath]; v != "" {
		p, err := normalizePath(v)
		if err != nil {
			pathOK = false
			bad(CodePathInvalid, fieldPath, "The path “%s” of route “%s” is not valid: %v.", v, name, err)
		} else {
			d.PathPrefix = p
		}
	}

	if v := r.fields[fieldScheme]; v != "" {
		switch Scheme(strings.ToLower(v)) {
		case SchemeHTTP:
			d.Scheme = SchemeHTTP
		case SchemeHTTPS:
			d.Scheme = SchemeHTTPS
		default:
			bad(CodeSchemeInvalid, fieldScheme, "The scheme “%s” of route “%s” must be http or https.", v, name)
		}
	}

	if v := r.fields[fieldStrip]; v != "" {
		switch strings.ToLower(v) {
		case "true":
			d.Strip = true
		case "false":
		default:
			bad(CodeStripInvalid, fieldStrip, "The strip value “%s” of route “%s” must be true or false.", v, name)
		}
	}
	// Stripping the prefix of a route that serves the whole host removes nothing. It is judged on the
	// normalised path, so /, // and an absent path all land here; a path that failed already has an
	// error of its own and is not reported twice.
	if d.Strip && pathOK && d.PathPrefix == "/" {
		bad(CodeStripRoot, fieldStrip, "The route “%s” strips its path prefix but serves the whole host. Add %s with a prefix such as /api, or remove %s.",
			name, labelKey(name, fieldPath), labelKey(name, fieldStrip))
	}
	return d, problems
}

// labelKey is the key of a route field in the form the route was written: the short form for the
// route named default, the named form otherwise.
func labelKey(route, field string) string {
	if route == DefaultRouteName {
		return LabelPrefix + field
	}
	return LabelPrefix + route + "." + field
}

// parsePort reads a port: ASCII digits only (so +80, 0x50 and -1 are refused) and from 1 to 65535.
func parsePort(s string) (int, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

// normalizePath checks a path prefix and drops its trailing slash, so /api and /api/ are one
// prefix. A prefix is matched as written and ends up in a proxy rule, so it may not hold a dot
// segment, an empty segment, a query, a fragment, a space, a control character, a backslash, a
// backtick or a brace: none of them can be carried into a rule safely.
func normalizePath(p string) (string, error) {
	if len(p) > maxPathLen {
		return "", fmt.Errorf("it is longer than %d characters", maxPathLen)
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("it must start with /")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c <= ' ' || c == 0x7f:
			return "", fmt.Errorf("it holds a space or a control character")
		case c == '?' || c == '#' || c == '`' || c == '\\' || c == '{' || c == '}':
			return "", fmt.Errorf("it holds the character %q, which a path prefix cannot carry", string(c))
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("it holds a dot segment")
		}
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("it holds an empty segment")
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	return p, nil
}

// Target is where a stack is deployed. The values are those of internal/stacks.
type Target string

// The targets.
const (
	TargetSwarm      Target = "swarm"
	TargetStandalone Target = "standalone"
)

// ServiceLabels is the two label sets of one compose service: its own labels, which a standalone
// container carries, and its deploy labels, which a swarm service carries.
type ServiceLabels struct {
	Name         string
	Labels       map[string]string
	DeployLabels map[string]string
}

// ParseStack reads the routes a whole stack declares for target. A swarm stack is read from
// deploy.labels and a standalone one from labels, because that is where each engine puts a label on
// the thing the proxy routes to. Ingress labels in the other place alone would otherwise mean a
// silent "no route", so they are an error; in both places, the other place is ignored with a
// warning, which lets one file serve both targets.
//
// Two routes of one stack with the same host and path are an error: the proxy could not tell them
// apart. The declarations come back sorted by service, then route name.
func ParseStack(services []ServiceLabels, target Target) ([]Declaration, []Problem) {
	sorted := append([]ServiceLabels(nil), services...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var out []Declaration
	var problems []Problem
	for _, s := range sorted {
		right, wrong, rightName, wrongName := s.DeployLabels, s.Labels, "deploy.labels", "labels"
		if target == TargetStandalone {
			right, wrong, rightName, wrongName = s.Labels, s.DeployLabels, "labels", "deploy.labels"
		}
		if HasLabels(wrong) {
			p := Problem{Service: s.Name}
			if HasLabels(right) {
				p.Severity, p.Code = SeverityWarning, CodeLabelsIgnored
				p.Message = fmt.Sprintf("The service “%s” has ingress labels under both %s and %s. A %s stack reads %s, so the ones under %s are ignored.",
					s.Name, rightName, wrongName, target, rightName, wrongName)
			} else {
				p.Severity, p.Code = SeverityError, CodeLabelsWrongPlace
				p.Message = fmt.Sprintf("The service “%s” has its ingress labels under %s. A %s stack reads them from %s, so move them there.",
					s.Name, wrongName, target, rightName)
			}
			problems = append(problems, p)
		}
		ds, ps := ParseServiceLabels(s.Name, right)
		out = append(out, ds...)
		problems = append(problems, ps...)
	}

	seen := map[string]Declaration{}
	var kept []Declaration
	for _, d := range out {
		key := d.Host + "\x00" + d.PathPrefix
		if first, dup := seen[key]; dup {
			problems = append(problems, Problem{
				Severity: SeverityError, Code: CodeRouteDuplicate, Service: d.Service, Label: d.Label,
				Message: fmt.Sprintf("The route “%s” of service “%s” and the route “%s” of service “%s” both serve %s%s. Give one of them a different host or path.",
					first.Name, first.Service, d.Name, d.Service, d.Host, d.PathPrefix),
			})
			continue
		}
		seen[key] = d
		kept = append(kept, d)
	}
	return kept, problems
}

// sortedKeys returns the keys of m in order, so problems come out in a stable order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
