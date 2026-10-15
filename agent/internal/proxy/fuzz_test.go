// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"context"
	"encoding/json"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
)

// The oracle is the route table written out a second time, on purpose, with its
// own expressions and its own query-key sets. It does not read the production
// table and does not call its lookup. If the production table ever grows a route
// this one does not name, the fuzz target finds it and fails, so widening what the
// agent forwards is a decision made in two places and not an accident of a table.

const (
	oVer = `^/(v[0-9]+\.[0-9]+/)?`
	oID  = `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`
	oVol = `[A-Za-z0-9][A-Za-z0-9_.-]{0,254}`
	// oImg is an image reference or ID: repository, tag, digest, written again here
	// and not read from routes.go.
	oImg = `[A-Za-z0-9][A-Za-z0-9_.:@/-]{0,254}`
)

// oracleEntry is one thing the agent may forward.
type oracleEntry struct {
	method string
	path   *regexp.Regexp
	// keys are the query keys the route takes, each with the value it accepts. A
	// read entry takes any query.
	keys map[string]*regexp.Regexp
	read bool
	// body is true when the route takes a JSON body, upgrade when it may upgrade.
	body, upgrade bool
}

func rx(s string) *regexp.Regexp { return regexp.MustCompile(s) }

var (
	oBool   = rx(`^(0|1|true|false)$`)
	oSecs   = rx(`^(-1|[0-9]{1,4}|[0-7][0-9]{4}|8[0-5][0-9]{3}|86[0-3][0-9]{2}|86400)$`)
	oSignal = rx(`^[A-Za-z0-9+]{1,16}$`)
	// oExecStart is the one path an upgrade may be on.
	oExecStart = rx(oVer + `exec/` + oID + `/start$`)
)

func oRead(tail string) oracleEntry {
	return oracleEntry{method: "GET", path: rx(oVer + tail + `$`), read: true}
}

func oEntry(method, tail string, keys map[string]*regexp.Regexp) oracleEntry {
	return oracleEntry{method: method, path: rx(oVer + tail + `$`), keys: keys}
}

var oracle = []oracleEntry{
	oRead(`(_ping|version|info|containers/json|images/json|volumes|networks|events|nodes|services|tasks)`),
	oRead(`containers/` + oID + `/json`),

	oEntry("POST", `containers/`+oID+`/(start|pause|unpause)`, nil),
	oEntry("POST", `containers/`+oID+`/(stop|restart)`, map[string]*regexp.Regexp{"t": oSecs, "signal": oSignal}),
	oEntry("POST", `containers/`+oID+`/kill`, map[string]*regexp.Regexp{"signal": oSignal}),
	oEntry("DELETE", `containers/`+oID, map[string]*regexp.Regexp{"force": oBool, "v": oBool}),

	oEntry("GET", `containers/`+oID+`/logs`, map[string]*regexp.Regexp{
		"stdout": oBool, "stderr": oBool, "timestamps": oBool, "follow": oBool, "details": oBool,
		"since": rx(`^[0-9TZ:.+-]{1,40}$`), "until": rx(`^[0-9TZ:.+-]{1,40}$`), "tail": rx(`^(all|[0-9]{1,6})$`),
	}),

	{method: "POST", path: rx(oVer + `containers/` + oID + `/exec$`), body: true},
	{method: "POST", path: oExecStart, body: true, upgrade: true},
	oEntry("POST", `exec/`+oID+`/resize`, map[string]*regexp.Regexp{"h": rx(`^[0-9]{1,4}$`), "w": rx(`^[0-9]{1,4}$`)}),
	oEntry("GET", `exec/`+oID+`/json`, nil),

	oEntry("GET", `networks/`+oID, map[string]*regexp.Regexp{"verbose": oBool, "scope": rx(`^(local|swarm|global)$`)}),
	{method: "POST", path: rx(oVer + `networks/create$`), body: true},
	oEntry("DELETE", `networks/`+oID, nil),
	{method: "POST", path: rx(oVer + `networks/` + oID + `/(connect|disconnect)$`), body: true},

	oEntry("GET", `volumes/`+oVol, nil),
	{method: "POST", path: rx(oVer + `volumes/create$`), body: true},
	oEntry("DELETE", `volumes/`+oVol, nil),

	// Images: a pull with its three keys and no body, the inspect, history and remove
	// of an image by any name, and the distribution check.
	oEntry("POST", `images/create`, map[string]*regexp.Regexp{
		"fromImage": rx(`^` + oImg + `$`),
		"tag":       rx(`^([A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`),
		"platform":  rx(`^[a-z0-9]{1,16}(/[A-Za-z0-9_.-]{1,32}){1,2}$`),
	}),
	oEntry("GET", `images/`+oImg+`/json`, nil),
	oEntry("GET", `images/`+oImg+`/history`, nil),
	oEntry("DELETE", `images/`+oImg, map[string]*regexp.Regexp{"noprune": oBool}),
	oEntry("GET", `distribution/`+oImg+`/json`, nil),

	oEntry("GET", `system/df`, map[string]*regexp.Regexp{"type": rx(`^volume$`), "verbose": oBool}),

	// One service: the inspect, the create and update that take a whole spec as
	// their body, and the remove, which takes no key at all.
	oEntry("GET", `services/`+oID, map[string]*regexp.Regexp{"insertDefaults": oBool}),
	// One node: the inspect, which takes no key at all.
	oEntry("GET", `nodes/`+oID, nil),
	// A node remove takes no key at all: force is never sent.
	oEntry("DELETE", `nodes/`+oID, nil),
	// A node update: a version and a node spec body.
	{method: "POST", path: rx(oVer + `nodes/` + oID + `/update$`), body: true, keys: map[string]*regexp.Regexp{"version": rx(`^[0-9]{1,20}$`)}},
	{method: "POST", path: rx(oVer + `services/create$`), body: true},
	oEntry("DELETE", `services/`+oID, nil),
	// Secrets: the list and the inspect take no key at all, the create takes a SecretSpec body, and the
	// remove takes no key either.
	oEntry("GET", `secrets`, nil),
	oEntry("GET", `secrets/`+oID, nil),
	{method: "POST", path: rx(oVer + `secrets/create$`), body: true},
	oEntry("DELETE", `secrets/`+oID, nil),
	// Configs: the same four, none with a key.
	oEntry("GET", `configs`, nil),
	oEntry("GET", `configs/`+oID, nil),
	{method: "POST", path: rx(oVer + `configs/create$`), body: true},
	oEntry("DELETE", `configs/`+oID, nil),
	{
		method: "POST", path: rx(oVer + `services/` + oID + `/update$`), body: true,
		keys: map[string]*regexp.Regexp{
			"version":          rx(`^[0-9]{1,20}$`),
			"registryAuthFrom": rx(`^(spec|previous-spec)$`),
			"rollback":         rx(`^(previous|none)$`),
		},
	},
}

// oracleFor returns every oracle entry a method and path fall under. HEAD stands
// for GET.
func oracleFor(method, rawPath string) []oracleEntry {
	if method == "HEAD" {
		method = "GET"
	}
	var out []oracleEntry
	for _, e := range oracle {
		if e.method == method && e.path.MatchString(rawPath) {
			out = append(out, e)
		}
	}
	return out
}

// FuzzAllowed drives the decision with arbitrary input. Whatever it is given, an
// allowed request falls under exactly one oracle entry, has that entry's method,
// carries only that entry's query keys with values the entry accepts, has a path
// that is already clean, no control character, a query within the cap, a body only
// where the entry takes one and an upgrade only where it may upgrade; and a
// read-only agent allows nothing that changes anything.
func FuzzAllowed(f *testing.F) {
	for _, s := range []struct {
		method, path, query string
		body, upgrade, ro   bool
	}{
		{"GET", "/info", "", false, false, false},
		{"HEAD", "/_ping", "", false, false, true},
		{"GET", "/v1.47/containers/json", "all=1", false, false, false},
		{"GET", "/containers/abc/json", "", false, false, false},
		{"POST", "/containers/abc/start", "", false, false, false},
		{"POST", "/containers/abc/stop", "t=10&signal=SIGTERM", false, false, false},
		{"POST", "/containers/abc/restart", "t=-1", false, false, false},
		{"POST", "/containers/abc/kill", "signal=9", false, false, false},
		{"POST", "/containers/abc/pause", "", false, false, false},
		{"POST", "/containers/abc/unpause", "", false, false, false},
		{"DELETE", "/containers/abc", "force=1&v=1", false, false, false},
		{"DELETE", "/containers/abc", "link=1", false, false, false},
		{"GET", "/containers/abc/logs", "follow=1&stdout=1&stderr=1&tail=500&since=1700000000.5", false, false, false},
		{"POST", "/containers/abc/exec", "", true, false, false},
		{"POST", "/exec/x/start", "", true, true, false},
		{"POST", "/exec/x/resize", "h=24&w=80", false, false, false},
		{"GET", "/exec/x/json", "", false, false, false},
		{"GET", "/networks/n1", "verbose=1&scope=swarm", false, false, false},
		{"POST", "/networks/create", "", true, false, false},
		{"DELETE", "/networks/n1", "", false, false, false},
		{"POST", "/networks/n1/connect", "", true, false, false},
		{"POST", "/networks/n1/disconnect", "", true, false, false},
		{"GET", "/volumes/v1", "", false, false, false},
		{"POST", "/volumes/create", "", true, false, false},
		{"DELETE", "/volumes/v1", "", false, false, false},
		{"DELETE", "/volumes/v1", "force=1", false, false, false},
		{"GET", "/system/df", "type=volume&verbose=1", false, false, false},
		{"GET", "/nodes/abc123", "", false, false, false},
		{"GET", "/nodes/abc123", "x=1", false, false, false},
		{"GET", "/nodes/abc123", "", true, false, false},
		{"HEAD", "/v1.56/nodes/abc123", "", false, false, true},
		{"GET", "/nodes/abc123/tasks", "", false, false, false},
		{"POST", "/nodes/abc123/update", "version=1", true, false, false},
		{"POST", "/nodes/abc123/update", "version=1&x=2", true, false, false},
		{"POST", "/nodes/abc123/update", "version=x", true, false, false},
		{"POST", "/nodes/abc123/update", "version=1", false, false, false},
		{"POST", "/nodes/abc123/update", "version=1", true, false, true},
		{"POST", "/v1.56/nodes/abc123/update", "version=12", true, false, false},
		{"DELETE", "/nodes/abc123", "", false, false, false},
		{"GET", "/services/web", "insertDefaults=1", false, false, false},
		{"GET", "/services/web", "insertDefaults=maybe", false, false, false},
		{"GET", "/services/web", "", true, false, false},
		{"HEAD", "/v1.56/services/abc", "", false, false, true},
		{"POST", "/services/web/update", "version=12&registryAuthFrom=spec&rollback=none", true, false, false},
		{"POST", "/services/web/update", "version=12", false, false, false},
		{"POST", "/services/web/update", "version=12", true, false, true},
		{"POST", "/services/web/update", "version=12&force=1", true, false, false},
		{"POST", "/services/web/update", "version=12", true, true, false},
		{"POST", "/services/create", "", true, false, false},
		{"DELETE", "/services/web", "", false, false, false},
		{"GET", "/secrets", "", false, false, false},
		{"GET", "/secrets", "filters=%7B%7D", false, false, false},
		{"HEAD", "/v1.56/secrets", "", false, false, true},
		{"GET", "/secrets/abc123", "", false, false, false},
		{"GET", "/secrets/abc123", "x=1", false, false, false},
		{"GET", "/secrets/abc123/update", "", false, false, false},
		{"POST", "/secrets/create", "", true, false, false},
		{"POST", "/secrets/create", "x=1", true, false, false},
		{"POST", "/secrets/create", "", false, false, false},
		{"POST", "/secrets/create", "", true, false, true},
		{"POST", "/secrets/abc123/update", "version=1", true, false, false},
		{"DELETE", "/secrets/abc123", "", false, false, false},
		{"DELETE", "/secrets/abc123", "force=1", false, false, false},
		{"DELETE", "/secrets/abc123", "", true, false, false},
		{"GET", "/configs", "", false, false, false},
		{"GET", "/configs", "filters=%7B%7D", false, false, false},
		{"HEAD", "/v1.56/configs", "", false, false, true},
		{"GET", "/configs/abc123", "", false, false, false},
		{"GET", "/configs/abc123", "x=1", false, false, false},
		{"GET", "/configs/abc123/update", "", false, false, false},
		{"POST", "/configs/create", "", true, false, false},
		{"POST", "/configs/create", "x=1", true, false, false},
		{"POST", "/configs/create", "", false, false, false},
		{"POST", "/configs/create", "", true, false, true},
		{"POST", "/configs/abc123/update", "version=1", true, false, false},
		{"DELETE", "/configs/abc123", "", false, false, false},
		{"DELETE", "/configs/abc123", "force=1", false, false, false},
		{"DELETE", "/configs/abc123", "", true, false, false},
		{"GET", "/services/web/logs", "follow=1", false, false, false},
		{"POST", "/images/create", "fromSrc=http://x", false, false, false},
		{"POST", "/images/create", "fromImage=a&tag=b&platform=linux/arm64", false, false, false},
		{"POST", "/images/create", "fromImage=ghcr.io/acme/app&tag=sha256:0000000000000000000000000000000000000000000000000000000000000000", false, false, false},
		{"POST", "/images/create", "fromImage=a&tag=b&repo=x", true, false, false},
		{"POST", "/images/create", "fromImage=a&tag=b", false, false, true},
		{"DELETE", "/images/x", "force=1", false, false, false},
		{"DELETE", "/images/x", "noprune=1", false, false, false},
		{"DELETE", "/images/x", "noprune=1", false, false, true},
		{"DELETE", "/images/ghcr.io/acme/app:1", "", false, false, false},
		{"GET", "/images/a/json/history", "", false, false, false},
		{"GET", "/images/a/history/json", "", false, false, false},
		{"GET", "/images/json", "", false, false, false},
		{"HEAD", "/images/json", "all=1", false, false, true},
		{"GET", "/images/a/json", "manifests=1", false, false, false},
		{"GET", "/distribution/a:1/json", "", false, false, false},
		{"GET", "/images/sha256:0000000000000000000000000000000000000000000000000000000000000000/json", "", false, false, false},
		{"GET", "/images/../x/json", "", false, false, false},
		{"GET", "/images//x/json", "", false, false, false},
		{"POST", "/images/x/tag", "repo=y", false, false, false},
		{"POST", "/images/load", "", true, false, false},
		{"GET", "/images/get", "names=x", false, false, false},
		{"POST", "/containers/create", "", true, false, false},
		{"POST", "/containers/abc/attach", "", false, true, false},
		{"GET", "/containers/abc/archive", "", false, false, false},
		{"GET", "//info", "", false, false, false},
		{"GET", "/./info", "", false, false, false},
		{"GET", "/%2e%2e/info", "", false, false, false},
		{"GET", "/containers%2Fjson", "", false, false, false},
		{"GET", "/containers/../info", "", false, false, false},
		{"GET", "/v1.47//info", "", false, false, false},
		{"GET", "/info%00", "", false, false, false},
		{"GET", "/info\x00", "", false, false, false},
		{"GET", "/info", "a=\n", false, false, false},
		{"POST", "/containers/abc/stop", "t=10", false, true, false},
		{"POST", "/containers/abc/start", "", true, false, false},
		{"GET", "/containers/json", strings.Repeat("a", 5<<10), false, false, false},
		{"GET", "/info", "", false, true, false},
		{"POST", "/info", "", false, false, true},
		{"POST", "/containers/abc/stop", "t=1", false, false, true},
	} {
		f.Add(s.method, s.path, s.query, s.body, s.upgrade, s.ro)
	}
	f.Fuzz(func(t *testing.T, method, rawPath, rawQuery string, hasBody, hasUpgrade, readOnly bool) {
		ok, reason, kind := Allowed(method, rawPath, rawQuery, hasBody, hasUpgrade, Policy{ReadOnly: readOnly})
		if !ok {
			if reason != "allowlist" && reason != "read_only_flag" {
				t.Fatalf("a refusal with the reason %q", reason)
			}
			if kind != BodyNone {
				t.Fatalf("a refusal came with the body kind %d", kind)
			}
			return
		}
		if reason != "" {
			t.Fatalf("an allowed request came with the reason %q", reason)
		}
		if readOnly && method != "GET" && method != "HEAD" {
			t.Fatalf("method %q was allowed on a read-only agent", method)
		}
		entries := oracleFor(method, rawPath)
		if len(entries) != 1 {
			t.Fatalf("%s %q was allowed but falls under %d oracle entries", method, rawPath, len(entries))
		}
		e := entries[0]
		if method != e.method && (method != "HEAD" || e.method != "GET") {
			t.Fatalf("method %q was allowed on a %s entry", method, e.method)
		}
		if rawPath != path.Clean(rawPath) {
			t.Fatalf("path %q was allowed but is not clean", rawPath)
		}
		if hasControlBytes(rawPath) || hasControlBytes(rawQuery) {
			t.Fatalf("%q?%q was allowed with a control character", rawPath, rawQuery)
		}
		if len(rawQuery) > maxQuery {
			t.Fatalf("a query of %d bytes was allowed", len(rawQuery))
		}
		if !e.read {
			q, err := url.ParseQuery(rawQuery)
			if err != nil {
				t.Fatalf("%q?%q was allowed with a query that does not parse", rawPath, rawQuery)
			}
			for k, vs := range q {
				pattern, known := e.keys[k]
				if !known {
					t.Fatalf("%s %q was allowed with the query key %q", method, rawPath, k)
				}
				for _, v := range vs {
					if !pattern.MatchString(v) {
						t.Fatalf("%s %q was allowed with %s=%q", method, rawPath, k, v)
					}
				}
			}
		}
		if hasBody != e.body {
			t.Fatalf("%s %q was allowed with a body (%v) where the entry takes one (%v)", method, rawPath, hasBody, e.body)
		}
		if (kind != BodyNone) != e.body {
			t.Fatalf("%s %q was allowed with the body kind %d where the entry takes a body (%v)", method, rawPath, kind, e.body)
		}
		if hasUpgrade && (!e.upgrade || !oExecStart.MatchString(rawPath)) {
			t.Fatalf("an upgrade was allowed on %s %q, which is not an exec start", method, rawPath)
		}
	})
}

func hasControlBytes(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// bodyShape is what a rebuilt body of one kind may contain. A key maps to the
// shape of the object (or of each element of the list) under it, or to nil for a
// scalar or a free map. zero lists the dotted keys the engine's type always
// encodes but the allowlist does not name: they must be empty.
type bodyShape struct {
	keys map[string]*bodyShape
	zero []string
}

var bodyShapes = map[BodyKind]bodyShape{
	BodyExecCreate: {
		keys: map[string]*bodyShape{
			"User": nil, "Tty": nil, "ConsoleSize": nil, "AttachStdin": nil, "AttachStderr": nil,
			"AttachStdout": nil, "WorkingDir": nil, "Cmd": nil,
			"Privileged": nil, "DetachKeys": nil, "Env": nil,
		},
		zero: []string{"Privileged", "DetachKeys", "Env"},
	},
	BodyExecStart: {
		keys: map[string]*bodyShape{"Tty": nil, "ConsoleSize": nil, "Detach": nil},
		zero: []string{"Detach"},
	},
	BodyNetworkCreate: {
		keys: map[string]*bodyShape{
			"Name": nil, "Driver": nil, "Scope": nil, "EnableIPv4": nil, "EnableIPv6": nil,
			"Internal": nil, "Attachable": nil, "Options": nil, "Labels": nil,
			"IPAM": {keys: map[string]*bodyShape{
				"Driver": nil, "Options": nil,
				"Config": {keys: map[string]*bodyShape{"Subnet": nil, "IPRange": nil, "Gateway": nil}},
			}},
			"Ingress": nil, "ConfigOnly": nil, "ConfigFrom": nil,
		},
		zero: []string{"Ingress", "ConfigOnly", "ConfigFrom", "IPAM.Options"},
	},
	BodyNetworkConnect: {
		keys: map[string]*bodyShape{
			"Container": nil,
			"EndpointConfig": {keys: map[string]*bodyShape{
				"IPAMConfig": {keys: map[string]*bodyShape{"IPv4Address": nil, "IPv6Address": nil, "LinkLocalIPs": nil}},
				"Aliases":    nil,
				"Links":      nil, "DriverOpts": nil, "GwPriority": nil, "NetworkID": nil, "EndpointID": nil,
				"Gateway": nil, "IPAddress": nil, "MacAddress": nil, "IPPrefixLen": nil, "IPv6Gateway": nil,
				"GlobalIPv6Address": nil, "GlobalIPv6PrefixLen": nil, "DNSNames": nil,
			}},
		},
		zero: []string{
			"EndpointConfig.Links", "EndpointConfig.DriverOpts", "EndpointConfig.GwPriority", "EndpointConfig.NetworkID",
			"EndpointConfig.EndpointID", "EndpointConfig.Gateway", "EndpointConfig.IPAddress", "EndpointConfig.MacAddress",
			"EndpointConfig.IPPrefixLen", "EndpointConfig.IPv6Gateway", "EndpointConfig.GlobalIPv6Address",
			"EndpointConfig.GlobalIPv6PrefixLen", "EndpointConfig.DNSNames", "EndpointConfig.IPAMConfig.LinkLocalIPs",
		},
	},
	BodyNetworkDisconnect: {
		keys: map[string]*bodyShape{"Container": nil, "Force": nil},
		zero: []string{"Force"},
	},
	BodyVolumeCreate: {
		keys: map[string]*bodyShape{"Name": nil, "Driver": nil, "DriverOpts": nil, "Labels": nil},
	},
	// A node spec is a name, labels, a role and an availability, and nothing else.
	BodyNodeSpec: {
		keys: map[string]*bodyShape{"Name": nil, "Labels": nil, "Role": nil, "Availability": nil},
	},
	// A secret to create is a name, labels and the value; a driver and templating are never forwarded.
	BodySecretCreate: {
		keys: map[string]*bodyShape{"Name": nil, "Labels": nil, "Data": nil},
	},
	// A config to create is a name, labels, the content and the golang templating driver; no other driver
	// and no driver option is ever forwarded.
	BodyConfigCreate: {
		keys: map[string]*bodyShape{"Name": nil, "Labels": nil, "Data": nil, "Templating": {keys: map[string]*bodyShape{"Name": nil}}},
	},
	// A service spec is the user's whole service, so the shape is its top-level keys.
	// What it must not hold is a plugin or a network attachment task, and a runtime
	// other than container (checked in FuzzBody).
	BodyServiceSpec: {
		keys: map[string]*bodyShape{
			"Name": nil, "Labels": nil, "Mode": nil, "UpdateConfig": nil, "RollbackConfig": nil, "EndpointSpec": nil,
			"TaskTemplate": {keys: map[string]*bodyShape{
				"ContainerSpec": nil, "PluginSpec": nil, "NetworkAttachmentSpec": nil, "Resources": nil,
				"RestartPolicy": nil, "Placement": nil, "Networks": nil, "LogDriver": nil, "ForceUpdate": nil, "Runtime": nil,
			}},
		},
		zero: []string{"TaskTemplate.PluginSpec", "TaskTemplate.NetworkAttachmentSpec"},
	},
}

// checkShape returns the first key under v that the shape does not name, or "".
func checkShape(v map[string]any, s *bodyShape, prefix string) string {
	for k, child := range v {
		sub, known := s.keys[k]
		if !known {
			return prefix + k
		}
		if sub == nil {
			continue
		}
		switch c := child.(type) {
		case map[string]any:
			if bad := checkShape(c, sub, prefix+k+"."); bad != "" {
				return bad
			}
		case []any:
			for _, el := range c {
				if m, ok := el.(map[string]any); ok {
					if bad := checkShape(m, sub, prefix+k+"."); bad != "" {
						return bad
					}
				}
			}
		}
	}
	return ""
}

// emptyAt reports whether the dotted key is absent or holds a zero value.
func emptyAt(v map[string]any, dotted string) bool {
	var cur any = v
	for part := range strings.SplitSeq(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return true
		}
		cur, ok = m[part]
		if !ok {
			return true
		}
	}
	switch c := cur.(type) {
	case nil:
		return true
	case bool:
		return !c
	case float64:
		return c == 0
	case string:
		return c == ""
	case []any:
		return len(c) == 0
	case map[string]any:
		return len(c) == 0
	}
	return false
}

// FuzzBody drives every validator with arbitrary bytes. Whatever it is given,
// when it accepts, the body it forwards decodes into an object whose keys, at
// every depth, are the ones that kind allowlists plus the zero-valued keys the
// engine's type always encodes, and nothing that escalates (a privileged exec, a
// detached start, a forced disconnect, an ingress or config-only network, an
// environment, detach keys, endpoint links) is set.
func FuzzBody(f *testing.F) {
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			f.Fatal(err)
		}
		return string(b)
	}
	seeds := []struct {
		kind uint8
		body string
	}{
		{uint8(BodyExecCreate), enc(container.ExecCreateRequest{AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: true, Cmd: []string{"/bin/sh"}, User: "root", WorkingDir: "/app", ConsoleSize: &[2]uint{24, 80}})},
		{uint8(BodyExecCreate), enc(container.ExecCreateRequest{Tty: true, Privileged: true, Cmd: []string{"sh"}})},
		{uint8(BodyExecCreate), enc(container.ExecCreateRequest{Tty: true, Env: []string{"A=1"}, DetachKeys: "ctrl-p", Cmd: []string{"sh"}})},
		{uint8(BodyExecStart), enc(container.ExecStartRequest{Tty: true, ConsoleSize: &[2]uint{24, 80}})},
		{uint8(BodyExecStart), enc(container.ExecStartRequest{Detach: true, Tty: true})},
		{uint8(BodyNetworkCreate), enc(network.CreateRequest{
			Name: "app", Driver: "overlay", Attachable: true, Labels: map[string]string{"a": "b"},
			IPAM: &network.IPAM{Driver: "default", Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix("10.1.0.0/24"), Gateway: netip.MustParseAddr("10.1.0.1")}}},
		})},
		{uint8(BodyNetworkCreate), enc(network.CreateRequest{Name: "app", Driver: "overlay", Ingress: true, ConfigOnly: true})},
		{uint8(BodyNetworkConnect), enc(network.ConnectRequest{Container: "c", EndpointConfig: &network.EndpointSettings{
			IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.1.0.5")}, Aliases: []string{"web"},
		}})},
		{uint8(BodyNetworkConnect), enc(network.ConnectRequest{Container: "c", EndpointConfig: &network.EndpointSettings{Links: []string{"x:y"}, GwPriority: 3}})},
		{uint8(BodyNetworkDisconnect), enc(network.DisconnectRequest{Container: "c"})},
		{uint8(BodyNetworkDisconnect), enc(network.DisconnectRequest{Container: "c", Force: true})},
		{uint8(BodyVolumeCreate), enc(volume.CreateRequest{Name: "v", Driver: "local", DriverOpts: map[string]string{"type": "tmpfs"}, Labels: map[string]string{"a": "b"}})},
		{uint8(BodyVolumeCreate), enc(volume.CreateRequest{Name: "v", ClusterVolumeSpec: &volume.ClusterVolumeSpec{Group: "g"}})},
		{uint8(BodyServiceSpec), enc(swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: "web", Labels: map[string]string{"a": "b"}},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "ghcr.io/acme/web:1", Env: []string{"A=1"}}},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: new(uint64)}},
		})},
		{uint8(BodyServiceSpec), enc(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{Runtime: swarm.RuntimePlugin, PluginSpec: &swarm.RuntimeSpec{Name: "p"}}})},
		{uint8(BodyServiceSpec), enc(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{NetworkAttachmentSpec: &swarm.NetworkAttachmentSpec{ContainerID: "c"}}})},
		{uint8(BodyServiceSpec), enc(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: "x", Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/s"}},
		}}})},
		{uint8(BodyServiceSpec), enc(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: "x", CapabilityAdd: []string{"SYS_ADMIN"}, Privileges: &swarm.Privileges{Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined}},
		}}})},
		{uint8(BodyNodeSpec), enc(swarm.NodeSpec{Annotations: swarm.Annotations{Name: "alpha", Labels: map[string]string{"a": "b"}}, Role: swarm.NodeRoleWorker, Availability: swarm.NodeAvailabilityDrain})},
		{uint8(BodyNodeSpec), enc(swarm.NodeSpec{Role: swarm.NodeRoleManager, Availability: swarm.NodeAvailabilityActive})},
		{uint8(BodyNodeSpec), `{"Role":"leader","Availability":"down"}`},
		{uint8(BodyNodeSpec), `{"Role":"worker","Availability":"active","ID":"x"}`},
		{uint8(BodySecretCreate), enc(swarm.SecretSpec{Annotations: swarm.Annotations{Name: "dbpw", Labels: map[string]string{"a": "b"}}, Data: []byte("v")})},
		{uint8(BodySecretCreate), enc(swarm.SecretSpec{Annotations: swarm.Annotations{Name: "dbpw"}, Data: []byte("v"), Driver: &swarm.Driver{Name: "vault"}})},
		{uint8(BodySecretCreate), enc(swarm.SecretSpec{Annotations: swarm.Annotations{Name: "dbpw"}, Data: []byte("v"), Templating: &swarm.Driver{Name: "golang"}})},
		{uint8(BodySecretCreate), `{"Name":"dbpw","Data":""}`},
		{uint8(BodyConfigCreate), enc(swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "nginx.conf", Labels: map[string]string{"a": "b"}}, Data: []byte("v")})},
		{uint8(BodyConfigCreate), enc(swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "nginx.conf"}, Data: []byte("v"), Templating: &swarm.Driver{Name: "golang"}})},
		{uint8(BodyConfigCreate), enc(swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "nginx.conf"}, Data: []byte("v"), Templating: &swarm.Driver{Name: "vault"}})},
		{uint8(BodyConfigCreate), enc(swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "nginx.conf"}, Data: []byte("v"), Templating: &swarm.Driver{Name: "golang", Options: map[string]string{"a": "b"}}})},
		{uint8(BodyConfigCreate), `{"Name":"nginx.conf","Data":"","Driver":{"Name":"vault"}}`},
		{uint8(BodyNone), `{}`},
	}
	for _, s := range seeds {
		f.Add(s.kind, s.body)
	}
	f.Fuzz(func(t *testing.T, kind uint8, body string) {
		k := BodyKind(kind % 11) // BodyNone and the ten kinds
		out, err := validateBody(k, strings.NewReader(body), bodyEnv{})
		if err != nil {
			return
		}
		shape, known := bodyShapes[k]
		if !known {
			t.Fatalf("kind %d accepted a body but has no shape", k)
		}
		if int64(len(out)) > bodyCap(k)*2 {
			t.Fatalf("the forwarded body is %d bytes", len(out))
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("the forwarded body is not an object: %v: %s", err, out)
		}
		if bad := checkShape(m, &shape, ""); bad != "" {
			t.Fatalf("kind %d forwarded the key %q: %s", k, bad, out)
		}
		for _, z := range shape.zero {
			if !emptyAt(m, z) {
				t.Fatalf("kind %d forwarded %s set: %s", k, z, out)
			}
		}
		if k == BodyNodeSpec {
			var spec swarm.NodeSpec
			if err := json.Unmarshal(out, &spec); err != nil {
				t.Fatalf("the forwarded node spec does not decode: %v: %s", err, out)
			}
			if spec.Role != swarm.NodeRoleManager && spec.Role != swarm.NodeRoleWorker {
				t.Fatalf("a node spec with the role %q was forwarded: %s", spec.Role, out)
			}
			if a := spec.Availability; a != swarm.NodeAvailabilityActive && a != swarm.NodeAvailabilityPause && a != swarm.NodeAvailabilityDrain {
				t.Fatalf("a node spec with the availability %q was forwarded: %s", a, out)
			}
			if len(spec.Labels) > maxMapEntries || len(spec.Name) > maxAliasChars {
				t.Fatalf("a node spec over the bounds was forwarded: %s", out)
			}
		}
		if k == BodySecretCreate {
			var spec swarm.SecretSpec
			if err := json.Unmarshal(out, &spec); err != nil {
				t.Fatalf("the forwarded secret spec does not decode: %v: %s", err, out)
			}
			if spec.Driver != nil || spec.Templating != nil {
				t.Fatalf("a secret spec with a driver or templating was forwarded: %.200s", out)
			}
			if n := len(spec.Data); n < 1 || n > maxSecretData {
				t.Fatalf("a secret spec with %d bytes of value was forwarded", n)
			}
			if !secretNameRe.MatchString(spec.Name) || len(spec.Labels) > maxMapEntries {
				t.Fatalf("a secret spec over the bounds was forwarded: %.200s", out)
			}
		}
		if k == BodyConfigCreate {
			var spec swarm.ConfigSpec
			if err := json.Unmarshal(out, &spec); err != nil {
				t.Fatalf("the forwarded config spec does not decode: %v: %s", err, out)
			}
			if tpl := spec.Templating; tpl != nil && (tpl.Name != "golang" || len(tpl.Options) > 0) {
				t.Fatalf("a config spec with the templating driver %+v was forwarded: %.200s", tpl, out)
			}
			if n := len(spec.Data); n < 1 || n > maxConfigData {
				t.Fatalf("a config spec with %d bytes of content was forwarded", n)
			}
			if !secretNameRe.MatchString(spec.Name) || len(spec.Labels) > maxMapEntries {
				t.Fatalf("a config spec over the bounds was forwarded: %.200s", out)
			}
		}
		if k == BodyServiceSpec {
			var spec swarm.ServiceSpec
			if err := json.Unmarshal(out, &spec); err != nil {
				t.Fatalf("the forwarded service spec does not decode: %v: %s", err, out)
			}
			if rt := spec.TaskTemplate.Runtime; rt != "" && rt != swarm.RuntimeContainer {
				t.Fatalf("a service spec with the runtime %q was forwarded: %s", rt, out)
			}
			// With the zero environment (no opt-in, no lookup) nothing that gives
			// tasks host access may be forwarded.
			if found := escalations(context.Background(), spec, nil); len(found) > 0 {
				t.Fatalf("a service spec with host access %v was forwarded: %s", found, out)
			}
		}
	})
}
