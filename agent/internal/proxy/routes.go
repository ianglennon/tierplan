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
	// BodyServiceSpec is POST /services/create and POST /services/{id}/update.
	BodyServiceSpec
	// BodyNodeSpec is POST /nodes/{id}/update.
	BodyNodeSpec
	// BodySecretCreate is POST /secrets/create.
	BodySecretCreate
	// BodyConfigCreate is POST /configs/create.
	BodyConfigCreate
	// BodyContainerCreate is POST /containers/create.
	BodyContainerCreate
	// BodyArchive is PUT /containers/{id}/archive. Unlike every other kind it is not JSON: the
	// body is a tar of one regular file, judged by validateArchive and forwarded as received.
	BodyArchive
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
	// required are the keys a request must carry, where the engine would answer a request
	// without one with an error: a rename without a name, an archive without a path.
	required []string
	// anyQuery is true for the read surface of earlier phases, whose query is
	// only bounded in size.
	anyQuery bool
	body     BodyKind
	upgrade  bool
	// registryAuth is true for the routes that may carry an X-Registry-Auth
	// header, which holds a registry credential. Only image pull and the
	// distribution check use one; the header is deleted from every other request
	// before it is forwarded (Handler).
	registryAuth bool
}

const (
	// idShape is a container, network or exec ID or name: it starts with a letter
	// or digit, so ".." and a leading dash are not IDs, and it is at most 128
	// characters.
	idShape = `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`
	// volumeShape is a volume name, which Docker allows to be longer.
	volumeShape = `[A-Za-z0-9][A-Za-z0-9_.-]{0,254}`
	// imageNameShape is an image given by repository (with a registry host and
	// port), repository:tag, repository@digest or image ID, so it may hold slashes,
	// colons and @. It starts with a letter or digit and is at most 255
	// characters. A path that holds "..", "//" or a trailing slash never gets this
	// far: Allowed refuses a path that path.Clean would change.
	imageNameShape = `[A-Za-z0-9][A-Za-z0-9_.:@/-]{0,254}`
)

var (
	// boolValue is every spelling of a boolean the engine accepts and the
	// server's client sends.
	boolValue = regexp.MustCompile(`^(0|1|true|false)$`)
	// streamOff is the one value the stream key of a stats read may have: the agent
	// answers one sample and never holds a stats stream open.
	streamOff = regexp.MustCompile(`^(0|false)$`)
	// secondsValue is a timeout in seconds, -1 to 86400.
	secondsValue = regexp.MustCompile(`^(-1|[0-9]{1,4}|[0-7][0-9]{4}|8[0-5][0-9]{3}|86[0-3][0-9]{2}|86400)$`)
	// logTime is a log bound: a Unix time or an RFC 3339 time.
	logTime = regexp.MustCompile(`^[0-9TZ:.+-]{1,40}$`)
	// tailValue is how many log lines from the end, or all of them.
	tailValue = regexp.MustCompile(`^(all|[0-9]{1,6})$`)
	// sizeValue is one side of a terminal size.
	sizeValue = regexp.MustCompile(`^[0-9]{1,4}$`)
	// networkScope is the scope a network inspect may ask for.
	networkScope = regexp.MustCompile(`^(local|swarm|global)$`)
	// volumeType is the one object type a disk usage request may ask for.
	volumeType = regexp.MustCompile(`^volume$`)
	// signalValue is a signal name or number.
	signalValue = regexp.MustCompile(`^[A-Za-z0-9+]{1,16}$`)
	// fromImageValue is the repository an image pull names, which the client
	// sends without its tag.
	fromImageValue = regexp.MustCompile(`^` + imageNameShape + `$`)
	// pullTag is a tag or a digest, which the engine takes in the same key.
	pullTag = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
	// versionValue is the object version a service update carries, a swarm index.
	versionValue = regexp.MustCompile(`^[0-9]{1,20}$`)
	// authFromValue is where a service update takes the registry credential's
	// registry from.
	authFromValue = regexp.MustCompile(`^(spec|previous-spec)$`)
	// rollbackValue is the rollback a service update may ask for.
	rollbackValue = regexp.MustCompile(`^(previous|none)$`)
	// platformValue is os/arch or os/arch/variant.
	platformValue = regexp.MustCompile(`^[a-z0-9]{1,16}(/[A-Za-z0-9_.-]{1,32}){1,2}$`)
	// containerNameValue is the name a container create may ask for: the engine's own
	// rule, with the optional leading slash, and at most 255 characters.
	containerNameValue = regexp.MustCompile(`^/?[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
	// rootPathValue is the one directory an archive write may name: the root of the container.
	// The tar entry's own name says where the file goes (validateArchive).
	rootPathValue = regexp.MustCompile(`^/$`)
	// falseValue is the one value copyUIDGID may have: the owner in the tar is not applied, so a
	// file lands owned by root.
	falseValue = regexp.MustCompile(`^(0|false)$`)
	// inspectPlatformValue is the platform of an image inspect. The moby client sends the JSON of
	// an OCI platform (architecture, os, then os.version and variant when set), which is the form
	// the engine takes; os/arch and os/arch/variant are the same platform in the shorter spelling
	// the other image routes use.
	inspectPlatformValue = regexp.MustCompile(`^(` + platformValue.String()[1:len(platformValue.String())-1] +
		`|\{"architecture":"[a-z0-9_]{1,16}","os":"[a-z0-9]{1,16}"(,"os\.version":"[A-Za-z0-9_.+-]{1,64}")?(,"variant":"[A-Za-z0-9_.-]{1,32}")?\})$`)
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

// serviceLogKeys are the query keys of a service's and a task's logs: the seven the server
// sends, with the same value shapes as a container's logs, and no until.
var serviceLogKeys = map[string]*regexp.Regexp{
	"stdout": boolValue, "stderr": boolValue, "timestamps": boolValue, "follow": boolValue,
	"details": boolValue, "since": logTime, "tail": tailValue,
}

// routes is the whole surface the agent forwards, and the first place to look
// when asking whether the agent may do something. It is consulted after the
// read-only flag, so no entry can reach a write on a read-only host (D-35).
//
// GET /swarm is deliberately absent: its answer carries the swarm join tokens,
// which are credentials to add nodes. The cluster ID is read from /info on a
// manager instead. A container create is forwarded through a closed-shape body validator and
// the container host policy (containerpolicy.go), because the Compose SDK that deploys a
// standalone stack creates containers. A write into a container's filesystem (archive) is forwarded
// only as a tar of one regular file into a stopped compose container (archive.go), because the SDK
// injects a stack file secret or config that way; reading from a container (archive GET and
// export) and attach stay absent. The stats
// of a container are forwarded for one sample at a time only (RES-11): the two keys a
// one-shot read sends, and never a stream the agent would have to hold open.
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

	// One node: the inspect, which takes no key. It sits after the list so GET /nodes is
	// never read as an inspect. The update changes one node's availability, role or labels:
	// it takes the version just read and a node spec body that carries a name, labels, a
	// role and an availability and nothing else. The remove takes no key at all: with no force
	// key a compromised server cannot remove a node that is still up (RES-05, D-22), and the
	// daemon itself refuses a node that is not down.
	{name: "node inspect", method: http.MethodGet, path: pathOf(`nodes/` + idShape)},
	{name: "node remove", method: http.MethodDelete, path: pathOf(`nodes/` + idShape)},
	{
		name: "node update", method: http.MethodPost, path: pathOf(`nodes/` + idShape + `/update`),
		query: map[string]*regexp.Regexp{"version": versionValue}, body: BodyNodeSpec,
	},

	// One service: the inspect (matched after the list, so GET /services is never
	// read as an inspect), the create and the update, which take the service's whole
	// spec as their body and the registry credential its image needs, and the
	// remove, which takes no key. A spec that gives tasks host access is refused by
	// the body validator unless the host allows it (D-36). The logs of a service and of a
	// task are further down, beside the container logs.
	{
		name: "service inspect", method: http.MethodGet, path: pathOf(`services/` + idShape),
		query: map[string]*regexp.Regexp{"insertDefaults": boolValue},
	},
	{
		name: "service create", method: http.MethodPost, path: pathOf(`services/create`),
		body: BodyServiceSpec, registryAuth: true,
	},
	{name: "service remove", method: http.MethodDelete, path: pathOf(`services/` + idShape)},
	{
		name: "service update", method: http.MethodPost, path: pathOf(`services/` + idShape + `/update`),
		query:        map[string]*regexp.Regexp{"version": versionValue, "registryAuthFrom": authFromValue, "rollback": rollbackValue},
		body:         BodyServiceSpec,
		registryAuth: true,
	},

	// Secrets. The list sits ahead of the inspect so GET /secrets is never read as an
	// inspect (Pitfall 19), and none of the four takes a query key: the list takes no filter,
	// and a secret's value is never an answer, because the engine does not return one (E17).
	// The create takes a name, labels and the value, and refuses a driver and templating in
	// its body validator. An update is absent on purpose: a secret is replaced, not edited.
	{name: "secrets list", method: http.MethodGet, path: pathOf(`secrets`)},
	{name: "secret inspect", method: http.MethodGet, path: pathOf(`secrets/` + idShape)},
	{name: "secret create", method: http.MethodPost, path: pathOf(`secrets/create`), body: BodySecretCreate},
	{name: "secret remove", method: http.MethodDelete, path: pathOf(`secrets/` + idShape)},

	// Configs. The same four routes, in the same order and for the same reason: the list sits
	// ahead of the inspect, none takes a query key, and an update is absent on purpose (a config
	// is replaced, not edited). The list and the inspect answer with the content, which a config
	// has and a secret does not; the server drops it from the list. The create takes a name,
	// labels, the content and the golang templating driver, and nothing else.
	{name: "configs list", method: http.MethodGet, path: pathOf(`configs`)},
	{name: "config inspect", method: http.MethodGet, path: pathOf(`configs/` + idShape)},
	{name: "config create", method: http.MethodPost, path: pathOf(`configs/create`), body: BodyConfigCreate},
	{name: "config remove", method: http.MethodDelete, path: pathOf(`configs/` + idShape)},

	// Container create: the name and the platform are the only keys, and the body is the
	// container spec the Compose SDK sends, rebuilt from an allowlist and judged by the host
	// policy (STK-02). It sits before every containers/{id} pattern, so "create" is never read as
	// an ID.
	{
		name: "container create", method: http.MethodPost, path: pathOf(`containers/create`),
		query: map[string]*regexp.Regexp{"name": containerNameValue, "platform": platformValue},
		body:  BodyContainerCreate,
	},

	// Container archive: one regular file written into a compose container that is not running,
	// which is how the Compose SDK injects a stack file secret or config between create and
	// start. The only keys are the three the SDK's CopyToContainer sends, and the tar is judged by
	// validateArchive and the container by archiveTargetOK (STK-02). Only PUT: the read of a
	// container's files stays absent.
	{
		name: "container archive", method: http.MethodPut, path: pathOf(`containers/` + idShape + `/archive`),
		query:    map[string]*regexp.Regexp{"path": rootPathValue, "noOverwriteDirNonDir": boolValue, "copyUIDGID": falseValue},
		required: []string{"path"},
		body:     BodyArchive,
	},

	// Container rename: the Compose SDK names a recreated container after it has created it. The
	// only key is the new name, in the shape a container create takes, and the request has no body.
	{
		name: "container rename", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/rename`),
		query: map[string]*regexp.Regexp{"name": containerNameValue}, required: []string{"name"},
	},

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

	// Container logs, followed or not.
	{
		name: "container logs", method: http.MethodGet, path: pathOf(`containers/` + idShape + `/logs`),
		query: map[string]*regexp.Regexp{
			"stdout": boolValue, "stderr": boolValue, "timestamps": boolValue, "follow": boolValue,
			"details": boolValue, "since": logTime, "until": logTime, "tail": tailValue,
		},
	},

	// The stats of one container, read one sample at a time: the engine's stream and
	// one-shot switches are the only keys, so a request that follows the stats of a
	// container without end is as refused as any other key. Block I/O and the network are
	// in the sample; nothing in it names a secret.
	{
		name: "container stats", method: http.MethodGet, path: pathOf(`containers/` + idShape + `/stats`),
		query: map[string]*regexp.Regexp{"stream": streamOff, "one-shot": boolValue},
	},

	// The logs of a service (all its tasks, on one stream a manager answers) and of one
	// task, followed or not. The keys are the container log keys without until, which the
	// engine ignores on both routes (RESEARCH E2); the server cuts by time itself. They sit
	// after "service inspect" and "tasks", which cannot match a path ending in /logs.
	{
		name: "service logs", method: http.MethodGet, path: pathOf(`services/` + idShape + `/logs`),
		query: serviceLogKeys,
	},
	{
		name: "task logs", method: http.MethodGet, path: pathOf(`tasks/` + idShape + `/logs`),
		query: serviceLogKeys,
	},

	// A shell: create the exec, start it (the one HTTP upgrade the agent
	// forwards), resize its terminal and read its state.
	{name: "exec create", method: http.MethodPost, path: pathOf(`containers/` + idShape + `/exec`), body: BodyExecCreate},
	{name: "exec start", method: http.MethodPost, path: pathOf(`exec/` + idShape + `/start`), body: BodyExecStart, upgrade: true},
	{
		name: "exec resize", method: http.MethodPost, path: pathOf(`exec/` + idShape + `/resize`),
		query: map[string]*regexp.Regexp{"h": sizeValue, "w": sizeValue},
	},
	{name: "exec inspect", method: http.MethodGet, path: pathOf(`exec/` + idShape + `/json`)},

	// Networks.
	{
		name: "network inspect", method: http.MethodGet, path: pathOf(`networks/` + idShape),
		query: map[string]*regexp.Regexp{"verbose": boolValue, "scope": networkScope},
	},
	{name: "network create", method: http.MethodPost, path: pathOf(`networks/create`), body: BodyNetworkCreate},
	{name: "network remove", method: http.MethodDelete, path: pathOf(`networks/` + idShape)},
	{name: "network connect", method: http.MethodPost, path: pathOf(`networks/` + idShape + `/connect`), body: BodyNetworkConnect},
	{name: "network disconnect", method: http.MethodPost, path: pathOf(`networks/` + idShape + `/disconnect`), body: BodyNetworkDisconnect},

	// Volumes. force is absent on remove on purpose: it removes a volume that is
	// in use.
	{name: "volume inspect", method: http.MethodGet, path: pathOf(`volumes/` + volumeShape)},
	{name: "volume create", method: http.MethodPost, path: pathOf(`volumes/create`), body: BodyVolumeCreate},
	{name: "volume remove", method: http.MethodDelete, path: pathOf(`volumes/` + volumeShape)},

	// Images. Pull is the one write that takes a registry credential. Tag, push,
	// save, load, import, build and prune are absent on purpose: nothing Bayplan
	// does needs them, and each one changes or exports what the host holds.
	{
		// fromSrc, repo, message and changes are absent on purpose: they import an
		// image from a URL or from the request body instead of pulling it from a
		// registry.
		name: "image pull", method: http.MethodPost, path: pathOf(`images/create`),
		query:        map[string]*regexp.Regexp{"fromImage": fromImageValue, "tag": pullTag, "platform": platformValue},
		registryAuth: true,
	},
	// The list ("images list", above) is matched first, so GET /images/json is
	// never read as the inspect of an image whose name has no slash.
	// The inspect takes the two keys the Compose SDK sends: manifests, to read the manifests of a
	// multi-platform image, and platform, to pick one of them.
	{
		name: "image inspect", method: http.MethodGet, path: pathOf(`images/` + imageNameShape + `/json`),
		query: map[string]*regexp.Regexp{"manifests": boolValue, "platform": inspectPlatformValue},
	},
	{name: "image history", method: http.MethodGet, path: pathOf(`images/` + imageNameShape + `/history`)},
	{
		// force is absent on purpose: without it the engine itself refuses to
		// remove an image a container uses. platforms is absent because Bayplan
		// removes whole images.
		name: "image remove", method: http.MethodDelete, path: pathOf(`images/` + imageNameShape),
		query: map[string]*regexp.Regexp{"noprune": boolValue},
	},
	// The distribution check asks the registry whether this host may pull an
	// image, with the credential the pull would use.
	{
		name: "distribution", method: http.MethodGet, path: pathOf(`distribution/` + imageNameShape + `/json`),
		registryAuth: true,
	},

	// Disk usage, for the size of volumes only.
	{
		name: "system df", method: http.MethodGet, path: pathOf(`system/df`),
		query: map[string]*regexp.Regexp{"type": volumeType, "verbose": boolValue},
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
