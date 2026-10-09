// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"net/http"
	"regexp"
)

// BodyKind names the validator a route's request body goes through. A route
// that has no body is BodyNone; every other kind is a closed shape in body.go
// that is decoded, rebuilt from its allowlisted fields and forwarded as the
// rebuilt bytes, never as the bytes the server sent (D-27, D-38).
type BodyKind int

const (
	// BodyNone is a route that takes no body at all.
	BodyNone BodyKind = iota
	// BodyExecCreate is POST /containers/{id}/exec.
	BodyExecCreate
	// BodyExecStart is POST /exec/{id}/start.
	BodyExecStart
	// BodyNetworkCreate is POST /networks/create.
	BodyNetworkCreate
	// BodyNetworkConnect is POST /networks/{id}/connect.
	BodyNetworkConnect
	// BodyNetworkDisconnect is POST /networks/{id}/disconnect.
	BodyNetworkDisconnect
	// BodyVolumeCreate is POST /volumes/create.
	BodyVolumeCreate
)

// route is one thing the agent forwards: a method, an anchored path, the query
// keys it may carry with the pattern each value must match, the kind of body it
// takes and whether it may be upgraded to a raw stream.
type route struct {
	name   string
	method string
	path   *regexp.Regexp
	// query is the closed set of query keys; a key not in it, or a value that
	// does not match its pattern, refuses the request. Ignored when anyQuery.
	query map[string]*regexp.Regexp
	// anyQuery is true for the read surface of earlier phases, whose query is
	// only bounded in size.
	anyQuery bool
	body     BodyKind
	upgrade  bool
}

const (
	// idShape is a container, network or exec ID or name: it starts with a letter
	// or digit, so ".." and a leading dash are not IDs, and it is at most 128
	// characters.
	idShape = `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`
	// volumeShape is a volume name, which Docker allows to be longer.
	volumeShape = `[A-Za-z0-9][A-Za-z0-9_.-]{0,254}`
)

var (
	// boolValue is every spelling of a boolean the engine accepts and the
	// server's client sends.
	boolValue = regexp.MustCompile(`^(0|1|true|false)$`)
	// secondsValue is a timeout in seconds, -1 to 86400.
	secondsValue = regexp.MustCompile(`^(-1|[0-9]{1,4}|[0-7][0-9]{4}|8[0-5][0-9]{3}|86[0-3][0-9]{2}|86400)$`)
	// signalValue is a signal name or number.
	signalValue = regexp.MustCompile(`^[A-Za-z0-9+]{1,16}$`)
)

// pathOf anchors tail and allows the optional /vX.Y prefix, which is the API
// version the server's client negotiated.
func pathOf(tail string) *regexp.Regexp {
	return regexp.MustCompile(`^/(v[0-9]+\.[0-9]+/)?` + tail + `$`)
}

// read is a GET route of the earlier phases: any query within maxQuery.
func read(name, tail string) route {
	return route{name: name, method: http.MethodGet, path: pathOf(tail), anyQuery: true}
}

// routes is the whole surface the agent forwards, and the first place to look
// when asking whether the agent may do something. It is consulted after the
// read-only flag, so no entry can reach a write on a read-only host (D-35).
//
// GET /swarm is deliberately absent: its answer carries the swarm join tokens,
// which are credentials to add nodes. The cluster ID is read from /info on a
// manager instead. Container create, attach, archive, export and stats are
// absent too.
var routes = []route{
	// The read surface of Phase 2: ping, version, info, the lists the dashboard
	// reads, events, the swarm lists and the inspect of one container.
	read("ping", `_ping`),
	read("version", `version`),
	read("info", `info`),
	read("containers list", `containers/json`),
	read("images list", `images/json`),
	read("volumes list", `volumes`),
	read("networks list", `networks`),
	read("events", `events`),
	read("nodes", `nodes`),
	read("services", `services`),
	read("tasks", `tasks`),
	read("container inspect", `containers/`+idShape+`/json`),

	// Container lifecycle.
	{name: "container start", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/start`)},
	{name: "container pause", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/pause`)},
	{name: "container unpause", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/unpause`)},
	{
		name: "container stop", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/stop`),
		query: map[string]*regexp.Regexp{"t": secondsValue, "signal": signalValue},
	},
	{
		name: "container restart", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/restart`),
		query: map[string]*regexp.Regexp{"t": secondsValue, "signal": signalValue},
	},
	{
		name: "container kill", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/kill`),
		query: map[string]*regexp.Regexp{"signal": signalValue},
	},
	// link is absent on purpose: it removes a legacy link, which is not offered.
	{
		name: "container remove", method: http.MethodDelete, path: pathOf(`containers/` + idShape),
		query: map[string]*regexp.Regexp{"force": boolValue, "v": boolValue},
	},
}

// Match returns the route for a method and a raw path. HEAD matches a GET
// route. The first route in the table that matches wins.
func Match(method, rawPath string) (route, bool) {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	for _, r := range routes {
		if r.method == method && r.path.MatchString(rawPath) {
			return r, true
		}
	}
	return route{}, false
}
