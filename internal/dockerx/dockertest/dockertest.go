// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package dockertest holds the test helpers for code that talks to Docker
// through internal/dockerx: a fake daemon that speaks just enough of the Engine
// API over a real unix socket (so the real client code runs unchanged), and
// helpers that find or start a real engine for the integration tests.
//
// It lives under internal/dockerx so the depguard rule that keeps the moby
// client inside that tree still holds. Nothing in the product imports it.
package dockertest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ianglennon/bayplan/internal/engine"
)

const (
	// EnvDockerHost names the variable that points the integration tests at an
	// engine that is already running, such as the CI dind service.
	EnvDockerHost = "BAYPLAN_TEST_DOCKER_HOST"
	// EnvEngine names the variable that picks the dind major version.
	EnvEngine = "BAYPLAN_TEST_ENGINE"

	defaultEngine = "29"
	dindPort      = "2375/tcp"
)

// Component is one entry of the /version Components list.
type Component struct {
	Name    string
	Version string
}

// FakeConfig describes the engine a FakeDaemon pretends to be. Zero values
// become the defaults noted on each field.
type FakeConfig struct {
	// APIVersion is the daemon's maximum API version. Default "1.52".
	APIVersion string
	// MinAPIVersion is the oldest API version it still serves. Default "1.12",
	// which is old enough that no client request is refused as too old.
	MinAPIVersion string
	// OSType is the OSType ping header. Default "linux".
	OSType string
	// Version is the engine version. Default "29.1.0".
	Version string
	// PlatformName is the /version Platform.Name. Default "Docker Engine - Community".
	PlatformName string
	// Components replaces the default Engine component, for example to
	// present a Podman engine.
	Components []Component
	// Hostname is Info.Name. Default "fake-node".
	Hostname string
	// OperatingSystem is Info.OperatingSystem. Default "Ubuntu 26.04 LTS".
	OperatingSystem string
	// SecurityOptions is Info.SecurityOptions.
	SecurityOptions []string
	// SwarmState is Info.Swarm.LocalNodeState. Default "inactive".
	SwarmState string
	// SwarmControl is Info.Swarm.ControlAvailable.
	SwarmControl bool
	// EngineID is Info.ID. Default "FAKE:ID".
	EngineID string
	// SwarmNodeID is Info.Swarm.NodeID.
	SwarmNodeID string
	// SwarmClusterID is Info.Swarm.Cluster.ID; empty leaves Cluster unset, as an
	// engine outside a swarm reports it. A real worker leaves it unset too, which a
	// fake worker does not do unless this is empty.
	SwarmClusterID string
	// FailInfo makes /info answer 500, for the partial-failure path.
	FailInfo bool

	// Containers, Images, Volumes and Networks are what the list endpoints
	// answer. Nil means an empty engine: the fake answers an empty list, never
	// an error.
	Containers []FakeContainer
	Images     []FakeImage
	Volumes    []FakeVolume
	Networks   []FakeNetwork
	// Secrets are the swarm secrets /secrets lists, none with a value (fake_secrets.go).
	Secrets []FakeSecret
	// Configs are the swarm configs /configs lists, each with its content, as a real manager
	// lists them (fake_configs.go).
	Configs []FakeConfigObject
	// SecretListError makes GET /secrets answer 500 with this message, as a manager does when it cannot
	// list them, while every other read keeps working.
	SecretListError string
	// DiskUsageFail makes GET /system/df answer 500, as an engine does when a volume's files
	// cannot be walked.
	DiskUsageFail bool
	// DiskUsageDelay is how long GET /system/df takes to answer, so a test can make the
	// per-node time limit trip. It ends early when the caller gives up.
	DiskUsageDelay time.Duration
	// BusyVolumes are volumes the daemon refuses to remove as in use although no container
	// in its list mounts them, as when a container starts between a check and the call.
	BusyVolumes []string
	// BusyImages are image IDs the daemon refuses to remove as in use although no container
	// in its list was created from them, as when a container starts between a check and the
	// call.
	BusyImages []string
	// Nodes and Services are how many swarm nodes and services /nodes and
	// /services list. A fake that is not a swarm manager answers both with the
	// 503 a real worker gives, so a client that asks anyway fails loudly.
	Nodes, Services int
	// ListDelay is how long /containers/json takes to answer, so a test can
	// make concurrent callers overlap.
	ListDelay time.Duration
	// WriteDelay is how long each container write (start, stop, restart, kill, pause,
	// unpause, remove) takes to answer, so a test can make concurrent writes overlap
	// and read the high-water mark with MaxConcurrentWrites.
	WriteDelay time.Duration
	// DropWrites makes every container write close its connection without answering,
	// as the engine does when the container it is stopping is the one that serves the
	// caller (Bayplan's own server or an agent). The write is still recorded.
	DropWrites bool
	// WriteError makes every container write answer 500 with this message, as an engine
	// does when it refuses one.
	WriteError string
	// RemoveErrors makes the removal of the secret, config or network of that name answer 500 with the
	// message, as a manager does when it cannot remove one. A name not listed removes as usual.
	RemoveErrors map[string]string
	// NodeUpdateError makes every node update answer with this message, as a manager does when it
	// refuses one: 400 for the last-manager refusal, 500 for any other text.
	NodeUpdateError string
	// PolicyDeny makes the four list endpoints and every write answer 403 with the
	// message an agent gives for a request its policy refuses: agent_policy_denied:
	// followed by the method and path.
	PolicyDeny bool
	// VolumeHostPolicyDeny makes only the volume create answer 403 with the message an
	// agent gives for a host-policy refusal: agent_policy_denied: host_policy: followed
	// by the method and path.
	VolumeHostPolicyDeny bool
	// HistoryDeny makes only the image history route answer 403 agent_policy_denied, as
	// an agent older than the route does, while the lists and the image inspect answer.
	HistoryDeny bool
	// Events is the history the daemon has buffered when it starts, oldest
	// first, as /events replays it to a client that passes since.
	Events []FakeEvent

	// NodeRows are the nodes /nodes lists, in place of the Nodes count, so a test
	// can say who leads, who is reachable and what each node runs.
	NodeRows []FakeNode
	// ServiceRows are the services /services lists, in place of the Services count.
	// With ?status=true each carries its ServiceStatus.
	ServiceRows []FakeService
	// ServiceCreateError, when set, makes the creation of a service answer 400 with this text, as a
	// manager does for a spec it cannot accept; ServiceCreateWarnings are the warnings an accepted
	// creation answers with (fake_services.go).
	ServiceCreateError    string
	ServiceCreateWarnings []string
	// AgentTasks are the tasks /tasks lists. A task is listed when the request's
	// service filter names its service, by ID or by name, as a real daemon matches.
	AgentTasks []FakeAgentTask
	// Tasks are whole swarm tasks /tasks lists, with the filters of a real daemon (service,
	// node, desired-state and id), beside AgentTasks (fake_tasks.go).
	Tasks []swarm.Task
	// TasksFollowWrites makes /tasks list, for every service whose spec the daemon holds (one a
	// client created or updated through it), the tasks a settled swarm shows: a replicated service
	// has its replica count, a global one a task on each node, all running on the service's current
	// task template. A service that is not running is set with SetTaskState (fake_tasks.go).
	TasksFollowWrites bool
	// taskStates are the states SetTaskState gave those tasks, by service name.
	taskStates map[string]fakeTaskState
	// OnWrite, when set, is called with each write as Writes lists it ("METHOD path?query") the
	// moment the daemon receives it, before it answers, from the goroutine that serves the request. A
	// test uses it to do something at an exact point of a deploy, like cancelling it while its first
	// service is being written. It must not call the daemon's own client.
	OnWrite func(write string)
	// ServiceLogs are the lines the tasks of each service wrote, by service ID, which
	// /services/{id}/logs and /tasks/{id}/logs answer (fake_servicelogs.go).
	ServiceLogs map[string][]FakeServiceLogLine
	// Stats is the resource use each container reports, by container ID, which
	// /containers/{id}/stats answers one more sample of per call (fake_stats.go).
	Stats map[string]FakeStats
	// HistoryLimit is Swarm.Cluster.Spec.Orchestration.TaskHistoryRetentionLimit /info
	// reports; zero leaves the field unset, as a cluster that never set it does.
	HistoryLimit int
	// QuorumLost makes /nodes, /services, /tasks and /swarm answer the 500 a
	// manager gives when the swarm has no leader. Node-local calls keep working,
	// and so does /_ping, as on a real manager (RESEARCH Pitfall 11).
	QuorumLost bool
	// RestartPolicies maps a container ID to the restart policy /containers/{id}/json
	// reports: a name such as "always", or "on-failure:3" with its maximum retry
	// count. An ID that is not here is a container with no policy.
	RestartPolicies map[string]string
	// MissingExecutables are commands (the first word of an exec's Cmd) that no
	// container has: starting an exec of one prints the runtime's "no such file or
	// directory" message and ends with exit code 127 (fake_exec.go).
	MissingExecutables []string
	// PullStatus makes POST /images/create answer that status and message before any stream,
	// as the engine on the containerd image store does for a registry's refusal (images.go).
	// A zero Code answers normally.
	PullStatus FakePullStatus
	// PullStreamError makes a pull stream its progress and then an errorDetail with this
	// message, as a classic image store does; empty means the pull succeeds.
	PullStreamError string
	// PullHold holds every pull, after it is recorded and before it answers, until the
	// channel is closed or the caller gives up, so a test can look at a pull in progress.
	PullHold <-chan struct{}
	// ProbeHold holds every /_ping, after it is recorded and before it answers, until the
	// channel is closed or the request's context ends, so a test can show a target whose first
	// probe has not finished (an environment that reads online from another target while this
	// one is still unknown).
	ProbeHold <-chan struct{}
	// ProbeDelay answers every /_ping that much later (deliberate fake latency, not
	// synchronisation). NewFake reads BAYPLAN_TEST_PROBE_DELAY, a time.ParseDuration value,
	// and uses it as the delay of every fake whose ProbeDelay is zero, so a whole package can
	// be swept with slow first probes to find a test that treats "online" as "ready".
	ProbeDelay time.Duration
	// ListHold holds every container list (/containers/json), after it is recorded and before
	// it answers, until the channel is closed or the request's context ends, so a test can
	// show what must keep working while a list is outstanding (a probe, a stop, a caller that
	// gave up) by ordering instead of by comparing elapsed time with a bound.
	ListHold <-chan struct{}
}

// FakePullStatus is an HTTP status and message a pull is answered with.
type FakePullStatus struct {
	Code    int
	Message string
}

// FakeNode is one entry of /nodes. Leader marks the one manager whose
// ManagerStatus says so; Reachability applies to managers only and defaults to
// "reachable". Zero Role is "worker", zero Availability "active", zero State
// "ready".
type FakeNode struct {
	ID            string
	Hostname      string
	Role          string
	Availability  string
	State         string
	EngineVersion string
	OS            string
	Leader        bool
	Reachability  string
	// Address is the node's Status.Addr, Arch its platform architecture (zero is
	// "x86_64"), NanoCPUs and MemoryBytes its resources, Labels its spec labels,
	// EngineLabels the labels of its engine and ManagerAddr the address a manager
	// reports for the raft port.
	Address               string
	Arch                  string
	NanoCPUs, MemoryBytes int64
	Labels, EngineLabels  map[string]string
	ManagerAddr           string
	// Version is the node's object version (zero is 1), CreatedAt and UpdatedAt its
	// times (zero is the Unix epoch).
	Version              uint64
	CreatedAt, UpdatedAt time.Time
}

// FakeService is one entry of /services. Mode is "replicated" (the default),
// "global", "replicated-job" or "global-job". UpdateState is the UpdateStatus
// state, empty for none. A zero UpdatedAt is the Unix epoch.
type FakeService struct {
	ID, Name    string
	Mode        string
	Running     uint64
	Desired     uint64
	UpdateState string
	UpdatedAt   time.Time
	// Labels are the service's own labels (the --label of docker service create,
	// the deploy.labels of a stack file).
	Labels map[string]string
	// Networks are the targets of the service spec's network attachments, an ID or a
	// name as the spec wrote it, and Mounts the mounts of its container spec.
	Networks []string
	Mounts   []FakeServiceMount
	// Image is the image the spec names; empty leaves the container spec without one.
	Image string
	// Secrets and Configs are the secrets and configs the spec's container spec references.
	Secrets, Configs []FakeServiceObject
	// Constraints are the placement constraints of the service's spec.
	Constraints []string
	// Preferences are the spread descriptors of the spec's placement preferences.
	Preferences []string
	// MaxReplicas is the spec's placement limit of tasks per node.
	MaxReplicas uint64
	// Version is the version the service reports and a write must send back; zero is 1.
	// Every update the fake accepts adds one.
	Version uint64
	// Replicas is the replica count of a replicated service's spec; zero takes Desired.
	Replicas uint64
	// Spec, when set, is the service's spec as /services answers it, in place of the one
	// the fields above make up (fake_services.go).
	Spec *swarm.ServiceSpec
	// Ports are the service's endpoint ports, the ones the swarm publishes.
	Ports []swarm.PortConfig
	// UpdateMessage is the UpdateStatus message, empty for none.
	UpdateMessage string
	// PreviousSpec, when set, is the spec the service had before its last update.
	PreviousSpec *swarm.ServiceSpec
	// CreatedAt is when the service was created; zero is the Unix epoch.
	CreatedAt time.Time
	// RollbackError, when set, makes a rollback of the service answer 400 with this text, as a
	// manager does when the previous spec names a secret, a config or a network that is gone.
	RollbackError string
	// UpdateError, when set, makes an update of the service answer 400 with this text, as a
	// manager does for a spec it cannot accept (a secret or config that does not exist, a
	// field it refuses). UpdateWarnings are the warnings an accepted update answers with.
	UpdateError    string
	UpdateWarnings []string
}

// FakeServiceObject is one secret or config reference of a service's container spec. File
// is the file name in the container, empty for a config with a runtime target.
type FakeServiceObject struct {
	ID, Name, File string
	UID, GID       string
	Mode           uint32
}

// FakeServiceMount is one mount of a service's container spec. Type is "volume",
// "bind" or "tmpfs".
type FakeServiceMount struct {
	Type, Source string
	// Target is where the mount appears in the container; ReadOnly mounts it read-only.
	Target   string
	ReadOnly bool
}

// FakeAgentTask is one task of a service in ServiceRows, an agent's when that
// service is one.
type FakeAgentTask struct {
	NodeID, State, Error string
	// Service is the ID or name of the service the task belongs to. Empty means the
	// service named bayplan-agent.
	Service string
	// ID and Created order the tasks of one node: the newest is the current one.
	ID      string
	Created time.Time
}

// FakeEvent is one event of the fake daemon's /events stream. A zero TimeNano
// means the moment the event is published.
type FakeEvent struct {
	Type    string
	Action  string
	ActorID string
	// Scope is local or swarm; empty means local.
	Scope string
	// Attributes are sent as they are, so a test can prove which ones Bayplan
	// drops.
	Attributes map[string]string
	TimeNano   int64
}

// eventBuffer is how many events a real daemon keeps for a client that asks
// for history with since (RESEARCH Pitfall 7).
const eventBuffer = 256

// eventSub is one open /events stream.
type eventSub struct {
	ch       chan FakeEvent
	drop     chan struct{}
	dropOnce sync.Once
}

// FakeContainer is one entry of /containers/json. Labels and Mounts exist so a
// test can prove they never reach a Bayplan response.
type FakeContainer struct {
	ID    string
	Names []string
	Image string
	// ImageID is the ID of the image the container was created from.
	ImageID string
	State   string
	Status  string
	Created int64
	Ports   []FakePort
	Labels  map[string]string
	// Mounts are listed as volume mounts when the entry is a plain name and as
	// bind mounts when it starts with a slash.
	Mounts []string
	// Networks are the container's attachments by network name.
	Networks map[string]FakeEndpoint
	// Inspect holds what only an inspect reports (fake_inspect.go).
	Inspect FakeInspect
	// Health is the engine's health summary (starting, healthy, unhealthy). Empty
	// sends none, as an engine before API 1.52 does.
	Health string
	// StopTimeout and StopSignal are the container's own, as inspect reports them.
	StopTimeout *int
	StopSignal  string
}

// FakeEndpoint is one network attachment of a FakeContainer.
type FakeEndpoint struct {
	NetworkID string
	IPv4      string
	IPv6      string
	MAC       string
	// Aliases are the extra DNS names the container answers to on the network.
	Aliases []string
}

// FakePort is one published or exposed container port. IP is empty for a port
// that is not published.
type FakePort struct {
	IP      string
	Private uint16
	Public  uint16
	Type    string
}

// FakeImage is one entry of /images/json. An untagged image has no RepoTags.
type FakeImage struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  int64
	Labels   map[string]string
	// RepoDigests are the repo@sha256 names of the image, shown by its inspect.
	RepoDigests []string
	// OS and Architecture default to linux and amd64 in the inspect; Variant is omitted
	// when empty.
	OS, Architecture, Variant string
	// Env is the image config's environment, "NAME=value" each. The inspect carries it in
	// Config.Env and in ContainerConfig.Env, as an older engine does.
	Env []string
	// Cmd and Entrypoint are the image config's command, carried in Config when set.
	Cmd, Entrypoint []string
	// User, WorkingDir and StopSignal are the image config's, carried in Config when set, and
	// ExposedPorts are its EXPOSE lines ("80/tcp" each).
	User, WorkingDir, StopSignal string
	ExposedPorts                 []string
	// History is the layers /images/{name}/history answers, newest first. Empty gives one
	// layer.
	History []FakeLayer
}

// FakeLayer is one entry of an image's history.
type FakeLayer struct {
	ID        string
	CreatedBy string
	Comment   string
	Size      int64
	Created   int64
}

// FakeVolume is one entry of /volumes. CreatedAt is RFC 3339, or empty.
type FakeVolume struct {
	Name      string
	Driver    string
	CreatedAt string
	Labels    map[string]string
	// Options are the driver options the volume was created with.
	Options map[string]string
	// Size is the bytes /system/df reports for the volume. A volume of a driver other than
	// local reports -1, "not available", whatever is set here.
	Size int64
}

// FakeNetwork is one entry of /networks.
type FakeNetwork struct {
	ID      string
	Name    string
	Driver  string
	Scope   string
	Subnets []string
	Created time.Time
	// Labels, Internal, Attachable and Ingress are what /networks reports for the
	// network.
	Labels     map[string]string
	Internal   bool
	Attachable bool
	Ingress    bool
	// IPv6 is EnableIPv6, Options the driver options, and Gateways the gateway of
	// each subnet of Subnets by position (a shorter list leaves the rest without
	// one). They are what an inspect of the network reports.
	IPv6     bool
	Options  map[string]string
	Gateways []string
}

func (c FakeConfig) withDefaults() FakeConfig {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&c.APIVersion, "1.52")
	def(&c.MinAPIVersion, "1.12")
	def(&c.OSType, "linux")
	def(&c.Version, "29.1.0")
	def(&c.PlatformName, "Docker Engine - Community")
	def(&c.Hostname, "fake-node")
	def(&c.OperatingSystem, "Ubuntu 26.04 LTS")
	def(&c.SwarmState, "inactive")
	def(&c.EngineID, "FAKE:ID")
	if len(c.Components) == 0 {
		c.Components = []Component{{Name: "Engine", Version: c.Version}}
	}
	return c
}

// Fake is a running fake daemon. It is an engine.Transport that dials its own
// unix socket, and it records every request path it served.
type Fake struct {
	path string

	mu       sync.Mutex
	cfg      FakeConfig
	requests []string
	srv      *httptest.Server // nil while the daemon is stopped
	// states is the state a write left each container in, by container ID, and
	// writes is every write served, in order (fake_writes.go).
	states map[string]fakeState
	writes []string
	// inflight is how many container writes are being served now, and maxInflight
	// the most there have been at once (fake_writes.go).
	inflight, maxInflight int
	// execs is the exec state (fake_exec.go).
	execs fakeExecs
	// logs is the container log state (fake_logs.go).
	logs fakeLogs
	// svcLogs is the service log state (fake_servicelogs.go).
	svcLogs fakeServiceLogs
	// stats is the container stats state (fake_stats.go).
	stats fakeStatsState
	// nets are the networks created through the fake, and netCreates the bodies of
	// those requests (fake_networks.go).
	nets       []FakeNetwork
	netCreates []network.CreateRequest
	// netGone are the IDs of networks removed through the fake; withoutNetworks
	// leaves them out of every list.
	netGone map[string]bool
	// netConnects and netDisconnects are the bodies of the connects and disconnects the
	// fake accepted, and epChanges the attachments they left, by container ID and
	// network name: a nil endpoint is a disconnect (fake_networks.go).
	netConnects    []network.ConnectRequest
	netDisconnects []network.DisconnectRequest
	epChanges      map[[2]string]*FakeEndpoint
	// vols are the volumes created through the fake, and volCreates the bodies of those
	// requests (fake_volumes.go).
	vols       []FakeVolume
	volCreates []volume.CreateRequest
	// volGone are the names of the volumes removed through the fake.
	volGone map[string]bool
	// dfCalls counts the GET /system/df requests the fake answered or refused.
	dfCalls int
	// pulled are the images pulled through the fake, listed after the configured ones, and
	// pullAuth the X-Registry-Auth header of every pull, in order (fake_images.go).
	pulled   []FakeImage
	pullAuth []string
	// imgGone are the IDs of the images removed through the fake, and imgUntagged the names
	// (normalised) taken off an image, by its ID.
	imgGone     map[string]bool
	imgUntagged map[string]map[string]bool
	// svcState is what the updates the fake accepted left each service with, by ID, and
	// svcUpdates the updates themselves (fake_services.go).
	svcState   map[string]fakeServiceState
	svcUpdates []FakeServiceUpdate
	// svcCreated are the services created through the fake, listed after the configured ones, and
	// svcCreates the creates themselves (fake_services.go).
	svcCreated []FakeService
	svcCreates []FakeServiceCreate
	// nodeState is what the updates the fake accepted left each node with, by ID, and
	// nodeUpdates the updates themselves (fake_nodes.go).
	nodeState   map[string]fakeNodeState
	nodeUpdates []FakeNodeUpdate
	// nodeGone are the IDs of the nodes removed through the fake.
	nodeGone map[string]bool
	// svcGone are the IDs of the services removed through the fake.
	svcGone map[string]bool
	// taskStates are the states SetTaskState gave the tasks that follow writes, by service name.
	taskStates map[string]fakeTaskState
	// quorumLost is set by LoseQuorum.
	quorumLost bool
	// updateStates are the update statuses SetUpdateState gave services, by service name.
	updateStates map[string]fakeTaskState
	// secrets are the secrets created through the fake, secretCreates the requests that made
	// them (as byte counts) and secretGone the IDs of those removed (fake_secrets.go).
	secrets       []FakeSecret
	secretCreates []FakeSecretCreate
	secretGone    map[string]bool
	// configs are the configs created through the fake, configCreates the requests that made
	// them and configGone the IDs of those removed (fake_configs.go).
	configs       []FakeConfigObject
	configCreates []FakeConfigCreate
	configGone    map[string]bool
	// distDigest is the digest the distribution route answers a full reference with, where a
	// test has moved a tag (SetDistributionDigest); a reference not in it gets fakeDigest.
	distDigest map[string]string
	// distFail is the status and message the distribution route answers one full reference
	// with (SetDistributionFailure), where a test makes a single registry fail.
	distFail map[string]FakePullStatus
	// distAuth and distRefs are the header and reference of each distribution check.
	distAuth, distRefs []string

	// evMu guards the events history, the open streams and the queries seen.
	evMu      sync.Mutex
	evHistory []FakeEvent
	evSubs    map[*eventSub]struct{}
	evQueries []url.Values

	// await wakes Await whenever the fake served or finished a request, was reconfigured or
	// stopped, or a long-lived handler changed a counter a test reads.
	await awaitHub
}

// awaitHub is a change broadcast: notify wakes everyone who took the channel before it.
// Its mutex is a leaf, so notify may be called with any other lock of the fake held.
type awaitHub struct {
	mu sync.Mutex
	ch chan struct{}
}

func (a *awaitHub) channel() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ch == nil {
		a.ch = make(chan struct{})
	}
	return a.ch
}

func (a *awaitHub) notify() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ch != nil {
		close(a.ch)
		a.ch = nil
	}
}

// Await returns nil once cond is true, and ctx's error if ctx ends first. cond is evaluated
// now and again after every change of the fake: a request served or finished (so a counter a
// handler changes, such as writes in flight, is seen after the handler returns), a
// Reconfigure or Stop, and each change a long-lived handler makes while it runs (an event
// stream, a log or service log follow, a stats stream or an exec session opening or ending).
// It never polls, so the deadline in ctx only guards against a hang. cond may read the
// fake's counters (for example ServiceLogFollows() == 1) but must not call Await.
func (f *Fake) Await(ctx context.Context, cond func() bool) error {
	for {
		// The channel is taken before cond is evaluated, so a change in between is not missed.
		ch := f.await.channel()
		if cond() {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var versionPrefix = regexp.MustCompile(`^/v(\d+)\.(\d+)(/.*)$`)

// FakeDaemon starts a fake daemon on a unix socket and returns the transport
// that reaches it. The socket lives in a short temporary directory because
// unix socket paths are limited to about 100 bytes.
func FakeDaemon(t testing.TB, cfg FakeConfig) engine.Transport {
	t.Helper()
	return NewFake(t, cfg)
}

// NewFake is FakeDaemon with the concrete type, so a test can read the
// requests it served and change what it reports.
func NewFake(t testing.TB, cfg FakeConfig) *Fake {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpfd")
	if err != nil {
		t.Fatalf("make the fake daemon directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	cfg, err = withEnvProbeDelay(cfg)
	if err != nil {
		t.Fatalf("%v", err)
	}

	f := &Fake{path: filepath.Join(dir, "d.sock"), cfg: cfg.withDefaults(), evSubs: map[*eventSub]struct{}{}, states: map[string]fakeState{}}
	f.evHistory = append(f.evHistory, cfg.Events...)
	if err := f.listen(); err != nil {
		t.Fatalf("listen on the fake daemon socket: %v", err)
	}
	t.Cleanup(f.Stop)
	return f
}

// listen starts serving on the fake's socket path.
func (f *Fake) listen() error {
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		return err
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener = ln
	srv.Start()
	f.mu.Lock()
	f.srv = srv
	f.mu.Unlock()
	return nil
}

// Stop takes the daemon down: the socket is closed and removed, so a client
// sees "no such file" or "connection refused" as it would with a stopped
// engine. It is safe to call twice. Restart brings the daemon back on the same
// path, with whatever configuration it has then.
func (f *Fake) Stop() {
	f.mu.Lock()
	srv := f.srv
	f.srv = nil
	f.mu.Unlock()
	defer f.await.notify()
	if srv != nil {
		// An open /events stream would keep Close waiting.
		f.DropEventStreams()
		srv.Close()
	}
}

// Restart starts the daemon again on the same socket path after Stop. It does
// nothing if the daemon is already running.
func (f *Fake) Restart(t testing.TB) {
	t.Helper()
	f.mu.Lock()
	running := f.srv != nil
	f.mu.Unlock()
	if running {
		return
	}
	if err := f.listen(); err != nil {
		t.Fatalf("restart the fake daemon: %v", err)
	}
}

// SetAPIVersion changes the maximum API version the daemon reports from now
// on, as an engine upgraded while it was stopped.
func (f *Fake) SetAPIVersion(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.APIVersion = v
}

// SocketPath is the path of the fake's unix socket.
func (f *Fake) SocketPath() string { return f.path }

// DialContext implements engine.Transport: every dial reaches the fake.
func (f *Fake) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", f.path)
}

// Describe implements engine.Transport.
func (f *Fake) Describe() string { return "unix://" + f.path }

// Requests returns the request paths served so far, in order, as the daemon
// saw them (including any /vX.Y prefix).
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// Reconfigure changes what the fake reports from now on, as a daemon that was
// upgraded or downgraded between two probes.
func (f *Fake) Reconfigure(cfg FakeConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The environment was validated by NewFake, so the error cannot occur here.
	cfg, _ = withEnvProbeDelay(cfg)
	f.cfg = cfg.withDefaults()
	f.await.notify()
}

// withEnvProbeDelay gives cfg the BAYPLAN_TEST_PROBE_DELAY delay (a time.ParseDuration value)
// when its own ProbeDelay is zero. NewFake and Reconfigure both apply it, so a sweep with slow
// first probes keeps applying after a test reconfigures the fake.
func withEnvProbeDelay(cfg FakeConfig) (FakeConfig, error) {
	if cfg.ProbeDelay != 0 {
		return cfg, nil
	}
	v := os.Getenv("BAYPLAN_TEST_PROBE_DELAY")
	if v == "" {
		return cfg, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return cfg, fmt.Errorf("BAYPLAN_TEST_PROBE_DELAY=%q is not a duration: %w", v, err)
	}
	cfg.ProbeDelay = d
	return cfg, nil
}

func (f *Fake) config() FakeConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.withStates(f.states).withNetworks(f.nets).withoutNetworks(f.netGone).withEndpoints(f.epChanges).withVolumes(f.vols).withoutVolumes(f.volGone).withImages(f.pulled, f.imgGone, f.imgUntagged).withCreatedServices(f.svcCreated).withServices(f.svcState).withoutServices(f.svcGone).withNodes(f.nodeState).withoutNodes(f.nodeGone).withSecrets(f.secrets).withoutSecrets(f.secretGone).withConfigs(f.configs).withoutConfigs(f.configGone).withTaskStates(f.taskStates).withUpdateStates(f.updateStates).withQuorum(f.quorumLost)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	cfg := f.config()
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()
	f.await.notify()
	defer f.await.notify()

	p := r.URL.Path
	if p == "/_ping" {
		if cfg.ProbeHold != nil {
			select {
			case <-cfg.ProbeHold:
			case <-r.Context().Done():
				return
			}
		}
		if cfg.ProbeDelay > 0 {
			timer := time.NewTimer(cfg.ProbeDelay)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				return
			}
		}
		h := w.Header()
		h.Set("API-Version", cfg.APIVersion)
		h.Set("OSType", cfg.OSType)
		h.Set("Docker-Experimental", "false")
		switch cfg.SwarmState {
		case "active":
			role := "worker"
			if cfg.SwarmControl {
				role = "manager"
			}
			h.Set("Swarm", "active/"+role)
		default:
			h.Set("Swarm", cfg.SwarmState)
		}
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write([]byte("OK"))
		return
	}

	// A real daemon refuses a request whose version prefix is newer than its
	// own maximum (or older than its minimum). The fake does too, so a client
	// that forgot to negotiate fails here exactly as it would against Docker 23.
	if m := versionPrefix.FindStringSubmatch(p); m != nil {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		got := fmt.Sprintf("%d.%d", major, minor)
		switch {
		case cmpAPI(got, cfg.APIVersion) > 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("client version %s is too new. Maximum supported API version is %s", got, cfg.APIVersion)})
			return
		case cmpAPI(got, cfg.MinAPIVersion) < 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("client version %s is too old. Minimum supported API version is %s", got, cfg.MinAPIVersion)})
			return
		}
		p = m[3]
	}

	// A write is answered by the first handler that claims it (fake_writes.go and
	// the files later plans add), before any read route is tried.
	for _, h := range fakeHandlers {
		if h(f, w, r, p, cfg) {
			return
		}
	}

	if cfg.PolicyDeny {
		switch p {
		case "/containers/json", "/images/json", "/volumes", "/networks":
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
			return
		}
	}

	switch p {
	case "/version":
		comps := make([]system.ComponentVersion, 0, len(cfg.Components))
		for _, c := range cfg.Components {
			comps = append(comps, system.ComponentVersion{Name: c.Name, Version: c.Version})
		}
		writeJSON(w, http.StatusOK, system.VersionResponse{
			Platform:      system.PlatformInfo{Name: cfg.PlatformName},
			Version:       cfg.Version,
			APIVersion:    cfg.APIVersion,
			MinAPIVersion: cfg.MinAPIVersion,
			Os:            "linux",
			Arch:          "amd64",
			Components:    comps,
		})
	case "/info":
		if cfg.FailInfo {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "info is down"})
			return
		}
		writeJSON(w, http.StatusOK, system.Info{
			ID:              cfg.EngineID,
			Name:            cfg.Hostname,
			ServerVersion:   cfg.Version,
			OperatingSystem: cfg.OperatingSystem,
			OSType:          cfg.OSType,
			Architecture:    "x86_64",
			KernelVersion:   "7.0.0-fake",
			Driver:          "overlayfs",
			NCPU:            4,
			MemTotal:        8 << 30,
			SecurityOptions: cfg.SecurityOptions,
			Swarm:           cfg.swarmInfo(),
		})
	case "/events":
		f.serveEvents(w, r)
	case "/containers/json":
		if cfg.ListHold != nil {
			select {
			case <-cfg.ListHold:
			case <-r.Context().Done():
				return
			}
		}
		if cfg.ListDelay > 0 {
			select {
			case <-time.After(cfg.ListDelay):
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, http.StatusOK, cfg.containerList(r.URL.Query().Get("all") != ""))
	case "/images/json":
		writeJSON(w, http.StatusOK, cfg.imageList())
	case "/volumes":
		writeJSON(w, http.StatusOK, cfg.volumeList())
	case "/networks":
		writeJSON(w, http.StatusOK, filterNetworksByLabel(cfg.networkList(), r.URL.Query().Get("filters")))
	case "/nodes", "/services", "/tasks", "/swarm":
		f.serveCluster(w, r, p, cfg)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
}

// workerRefusal and quorumRefusal are the messages a real Engine 29 daemon gives
// a cluster question it cannot answer (RESEARCH Pattern 5, read from real daemons).
const (
	workerRefusal = "This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state. Please run this command on a manager node or promote the current node to a manager."
	quorumRefusal = "rpc error: code = Unknown desc = The swarm does not have a leader. It's possible that too few managers are online. Make sure more than half of the managers are online."
)

// serveCluster answers the swarm-wide lists: the worker 503, the quorum-loss 500
// or the configured rows.
func (f *Fake) serveCluster(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) {
	if !cfg.swarmManager() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": workerRefusal})
		return
	}
	if cfg.QuorumLost {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": quorumRefusal})
		return
	}
	switch p {
	case "/nodes":
		writeJSON(w, http.StatusOK, cfg.nodeList())
	case "/services":
		writeJSON(w, http.StatusOK, filterServicesByLabel(cfg.serviceList(r.URL.Query().Get("status") == "true"), r.URL.Query().Get("filters")))
	case "/tasks":
		writeJSON(w, http.StatusOK, cfg.taskList(r.URL.Query().Get("filters")))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
}

// resolveContainer finds a container the way a real daemon does: by its full ID,
// by its name (with or without the leading slash), or by an ID prefix that only one
// container has.
func (c FakeConfig) resolveContainer(ref string) (FakeContainer, bool) {
	for _, fc := range c.Containers {
		if fc.ID == ref {
			return fc, true
		}
	}
	for _, fc := range c.Containers {
		for _, n := range fc.Names {
			if strings.TrimPrefix(n, "/") == strings.TrimPrefix(ref, "/") {
				return fc, true
			}
		}
	}
	var hit FakeContainer
	n := 0
	for _, fc := range c.Containers {
		if ref != "" && strings.HasPrefix(fc.ID, ref) {
			hit = fc
			n++
		}
	}
	return hit, n == 1
}

// swarmInfo is Info.Swarm: the state, the node ID and the cluster ID when one is set.
func (c FakeConfig) swarmInfo() swarm.Info {
	info := swarm.Info{
		NodeID:           c.SwarmNodeID,
		LocalNodeState:   swarm.LocalNodeState(c.SwarmState),
		ControlAvailable: c.SwarmControl,
	}
	if c.SwarmClusterID != "" || c.HistoryLimit != 0 {
		info.Cluster = &swarm.ClusterInfo{ID: c.SwarmClusterID}
	}
	if c.HistoryLimit != 0 {
		limit := int64(c.HistoryLimit)
		info.Cluster.Spec.Orchestration.TaskHistoryRetentionLimit = &limit
	}
	return info
}

func (c FakeConfig) swarmManager() bool { return c.SwarmState == "active" && c.SwarmControl }

// containerList is the /containers/json answer. Without all, the way a real
// daemon behaves, only running containers are listed.
func (c FakeConfig) containerList(all bool) []container.Summary {
	out := make([]container.Summary, 0, len(c.Containers))
	for _, fc := range c.Containers {
		if !all && fc.State != "running" {
			continue
		}
		s := container.Summary{
			ID: fc.ID, Names: fc.Names, Image: fc.Image, ImageID: fc.ImageID, Created: fc.Created,
			State: container.ContainerState(fc.State), Status: fc.Status, Labels: fc.Labels,
		}
		for _, p := range fc.Ports {
			ps := container.PortSummary{PrivatePort: p.Private, PublicPort: p.Public, Type: p.Type}
			if p.IP != "" {
				ps.IP = netip.MustParseAddr(p.IP)
			}
			s.Ports = append(s.Ports, ps)
		}
		for _, m := range fc.Mounts {
			mp := container.MountPoint{Type: "volume", Name: m, Source: "/var/lib/docker/volumes/" + m + "/_data", Destination: "/mnt", RW: true}
			if strings.HasPrefix(m, "/") {
				mp = container.MountPoint{Type: "bind", Source: m, Destination: "/mnt", RW: true}
			}
			if to := fc.Inspect.MountTargets[m]; to != "" {
				mp.Destination = to
			}
			mp.RW = !strings.Contains(fc.Inspect.MountModes[m], "ro")
			s.Mounts = append(s.Mounts, mp)
		}
		if len(fc.Networks) > 0 {
			s.NetworkSettings = &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{}}
			for name, ep := range fc.Networks {
				es := &network.EndpointSettings{NetworkID: ep.NetworkID}
				if ep.IPv4 != "" {
					es.IPAddress = netip.MustParseAddr(ep.IPv4)
				}
				if ep.IPv6 != "" {
					es.GlobalIPv6Address = netip.MustParseAddr(ep.IPv6)
				}
				if ep.MAC != "" {
					_ = es.MacAddress.UnmarshalText([]byte(ep.MAC))
				}
				s.NetworkSettings.Networks[name] = es
			}
		}
		if fc.Health != "" {
			s.Health = &container.HealthSummary{Status: container.HealthStatus(fc.Health)}
		}
		out = append(out, s)
	}
	return out
}

func (c FakeConfig) imageList() []image.Summary {
	out := make([]image.Summary, 0, len(c.Images))
	for _, fi := range c.Images {
		out = append(out, image.Summary{ID: fi.ID, RepoTags: fi.RepoTags, RepoDigests: fi.RepoDigests, Size: fi.Size, Created: fi.Created, Labels: fi.Labels})
	}
	return out
}

func (c FakeConfig) volumeList() volume.ListResponse {
	out := volume.ListResponse{Volumes: make([]volume.Volume, 0, len(c.Volumes))}
	for _, fv := range c.Volumes {
		out.Volumes = append(out.Volumes, volume.Volume{
			Name: fv.Name, Driver: fv.Driver, CreatedAt: fv.CreatedAt, Labels: fv.Labels, Options: fv.Options,
			Mountpoint: "/var/lib/docker/volumes/" + fv.Name + "/_data", Scope: "local",
		})
	}
	return out
}

func (c FakeConfig) networkList() []network.Summary {
	out := make([]network.Summary, 0, len(c.Networks))
	for _, fn := range c.Networks {
		n := network.Summary{Network: network.Network{
			ID: fn.ID, Name: fn.Name, Driver: fn.Driver, Scope: fn.Scope, Created: fn.Created,
			Labels: fn.Labels, Internal: fn.Internal, Attachable: fn.Attachable, Ingress: fn.Ingress,
			EnableIPv6: fn.IPv6,
		}}
		for _, sub := range fn.Subnets {
			n.IPAM.Config = append(n.IPAM.Config, network.IPAMConfig{Subnet: netip.MustParsePrefix(sub)})
		}
		out = append(out, n)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cmpAPI compares two "major.minor" versions.
func cmpAPI(a, b string) int {
	pa, pb := splitAPI(a), splitAPI(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitAPI(v string) [2]int {
	maj, min, _ := strings.Cut(v, ".")
	a, _ := strconv.Atoi(maj)
	b, _ := strconv.Atoi(min)
	return [2]int{a, b}
}

// DockerHost returns the address of a real engine for the integration tests:
// BAYPLAN_TEST_DOCKER_HOST when it is set (CI points it at the dind service),
// otherwise a docker:<BAYPLAN_TEST_ENGINE or 29>-dind container started with
// testcontainers and removed when the test ends. It waits for the daemon by
// polling /_ping, never by sleeping (RESEARCH Pitfall 23).
func DockerHost(t testing.TB) string {
	t.Helper()
	if h := os.Getenv(EnvDockerHost); h != "" {
		return h
	}
	engineMajor := os.Getenv(EnvEngine)
	if engineMajor == "" {
		engineMajor = defaultEngine
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "docker:" + engineMajor + "-dind",
			ExposedPorts: []string{dindPort},
			Env:          map[string]string{"DOCKER_TLS_CERTDIR": ""},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Privileged = true
			},
			WaitingFor: wait.ForHTTP("/_ping").WithPort(dindPort).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start docker:%s-dind: %v", engineMajor, err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, dindPort, "tcp")
	if err != nil {
		t.Fatalf("find the dind port: %v", err)
	}
	return endpoint
}

// TCPTransport dials one TCP address whatever the client asks for. It exists
// for the integration tests only: the product constructs nothing but a local
// socket transport, so a tcp:// engine can never be reached by configuration
// (D-21).
type TCPTransport struct {
	// Addr is host:port, with no scheme.
	Addr string
}

// TCPTransportFor builds a TCPTransport from a DockerHost value such as
// tcp://localhost:2375.
func TCPTransportFor(host string) TCPTransport {
	return TCPTransport{Addr: strings.TrimPrefix(host, "tcp://")}
}

// DialContext implements engine.Transport.
func (t TCPTransport) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", t.Addr)
}

// Describe implements engine.Transport.
func (t TCPTransport) Describe() string { return "tcp://" + t.Addr }

// RawClient returns a moby client to host, for test setup only (swarm init,
// creating containers). It is not the product API: production code reaches
// Docker only through dockerx.
func RawClient(t testing.TB, host string) *client.Client {
	t.Helper()
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatalf("create the test setup client for %s: %v", host, err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// PublishEvent adds an event to the daemon's buffered history and sends it to
// every open /events stream, as the daemon does when something happens on the
// host. A zero TimeNano is replaced with the current time.
func (f *Fake) PublishEvent(e FakeEvent) {
	if e.TimeNano == 0 {
		e.TimeNano = time.Now().UnixNano()
	}
	f.evMu.Lock()
	defer f.evMu.Unlock()
	f.evHistory = append(f.evHistory, e)
	if len(f.evHistory) > eventBuffer {
		f.evHistory = f.evHistory[len(f.evHistory)-eventBuffer:]
	}
	for sub := range f.evSubs {
		select {
		case sub.ch <- e:
		default: // a stream nobody reads is as good as dropped
			sub.dropOnce.Do(func() { close(sub.drop) })
		}
	}
}

// ClearEvents empties the buffered history, as a daemon restart does.
func (f *Fake) ClearEvents() {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	f.evHistory = nil
}

// DropEventStreams ends every open /events stream without ending the daemon,
// as a network blip or a proxy timeout would.
func (f *Fake) DropEventStreams() {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	for sub := range f.evSubs {
		sub.dropOnce.Do(func() { close(sub.drop) })
	}
}

// OpenEventStreams is how many /events streams are open now.
func (f *Fake) OpenEventStreams() int {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	return len(f.evSubs)
}

// EventQueries returns the query string of every /events request served, in
// order, so a test can check the since and filters a client sent.
func (f *Fake) EventQueries() []url.Values {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	return append([]url.Values(nil), f.evQueries...)
}

// serveEvents answers GET /events like a daemon: the response starts at once
// (headers flushed), history at or after since is replayed when since is given,
// then new events follow until the client leaves or the stream is dropped.
func (f *Fake) serveEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	types := eventTypeFilter(q.Get("filters"))
	since, hasSince := parseEventSince(q.Get("since"))

	sub := &eventSub{ch: make(chan FakeEvent, 2*eventBuffer), drop: make(chan struct{})}
	f.evMu.Lock()
	f.evQueries = append(f.evQueries, q)
	var replay []FakeEvent
	if hasSince {
		for _, e := range f.evHistory {
			if e.TimeNano >= since {
				replay = append(replay, e)
			}
		}
	}
	f.evSubs[sub] = struct{}{}
	f.evMu.Unlock()
	f.await.notify()
	defer func() {
		f.evMu.Lock()
		delete(f.evSubs, sub)
		f.evMu.Unlock()
		f.await.notify()
	}()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	enc := json.NewEncoder(w)
	send := func(e FakeEvent) bool {
		if types != nil && !types[e.Type] {
			return true
		}
		scope := cmp.Or(e.Scope, "local")
		msg := events.Message{
			Type: events.Type(e.Type), Action: events.Action(e.Action), Scope: scope,
			Actor:    events.Actor{ID: e.ActorID, Attributes: e.Attributes},
			Time:     e.TimeNano / int64(time.Second),
			TimeNano: e.TimeNano,
		}
		return enc.Encode(msg) == nil && rc.Flush() == nil
	}
	for _, e := range replay {
		if !send(e) {
			return
		}
	}
	for {
		select {
		case e := <-sub.ch:
			if !send(e) {
				return
			}
		case <-sub.drop:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// eventTypeFilter reads the type terms of the filters query parameter, which is
// JSON like {"type":{"container":true}}. Nil means no type filter.
func eventTypeFilter(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	var f map[string]map[string]bool
	if json.Unmarshal([]byte(raw), &f) != nil || len(f["type"]) == 0 {
		return nil
	}
	return f["type"]
}

// parseEventSince reads since as unix seconds or seconds.nanoseconds and
// returns nanoseconds.
func parseEventSince(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	secs, frac, _ := strings.Cut(raw, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return 0, false
	}
	var ns int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		ns, _ = strconv.ParseInt(frac, 10, 64)
	}
	return s*int64(time.Second) + ns, true
}

// nodeList is the /nodes answer: the configured rows, or Nodes bare nodes.
func (c FakeConfig) nodeList() []swarm.Node {
	if len(c.NodeRows) == 0 {
		nodes := make([]swarm.Node, c.Nodes)
		for i := range nodes {
			nodes[i].ID = fmt.Sprintf("node%02d", i)
		}
		return nodes
	}
	out := make([]swarm.Node, 0, len(c.NodeRows))
	for _, fn := range c.NodeRows {
		role := fn.Role
		if role == "" {
			role = "worker"
		}
		avail := fn.Availability
		if avail == "" {
			avail = "active"
		}
		state := fn.State
		if state == "" {
			state = "ready"
		}
		arch := fn.Arch
		if arch == "" {
			arch = "x86_64"
		}
		n := swarm.Node{
			ID:   fn.ID,
			Meta: swarm.Meta{Version: swarm.Version{Index: max(fn.Version, 1)}, CreatedAt: fn.CreatedAt, UpdatedAt: fn.UpdatedAt},
			Spec: swarm.NodeSpec{
				Annotations: swarm.Annotations{Labels: fn.Labels},
				Role:        swarm.NodeRole(role), Availability: swarm.NodeAvailability(avail),
			},
			Description: swarm.NodeDescription{
				Hostname:  fn.Hostname,
				Platform:  swarm.Platform{OS: fn.OS, Architecture: arch},
				Resources: swarm.Resources{NanoCPUs: fn.NanoCPUs, MemoryBytes: fn.MemoryBytes},
				Engine:    swarm.EngineDescription{EngineVersion: fn.EngineVersion, Labels: fn.EngineLabels},
			},
			Status: swarm.NodeStatus{State: swarm.NodeState(state), Addr: fn.Address},
		}
		if role == "manager" {
			reach := fn.Reachability
			if reach == "" {
				reach = "reachable"
			}
			n.ManagerStatus = &swarm.ManagerStatus{Leader: fn.Leader, Reachability: swarm.Reachability(reach), Addr: fn.ManagerAddr}
		}
		out = append(out, n)
	}
	return out
}

// serviceList is the /services answer: the configured rows, or Services bare
// services. The status counts are sent only when the client asked for them, as a
// real daemon does.
func (c FakeConfig) serviceList(withStatus bool) []swarm.Service {
	if len(c.ServiceRows) == 0 {
		services := make([]swarm.Service, c.Services)
		for i := range services {
			services[i].ID = fmt.Sprintf("svc%02d", i)
		}
		return services
	}
	out := make([]swarm.Service, 0, len(c.ServiceRows))
	for _, fs := range c.ServiceRows {
		s := swarm.Service{ID: fs.ID}
		s.Version.Index = max(fs.Version, 1)
		if fs.Spec != nil {
			s.Spec = *fs.Spec
			s.UpdatedAt = fs.UpdatedAt
			if withStatus {
				s.ServiceStatus = &swarm.ServiceStatus{RunningTasks: fs.Running, DesiredTasks: fs.Desired}
			}
			fs.decorate(&s)
			out = append(out, s)
			continue
		}
		s.Spec.Name = fs.Name
		s.Spec.Labels = fs.Labels
		if len(fs.Constraints) > 0 || len(fs.Preferences) > 0 || fs.MaxReplicas > 0 {
			pl := &swarm.Placement{Constraints: slices.Clone(fs.Constraints), MaxReplicas: fs.MaxReplicas}
			for _, label := range fs.Preferences {
				pl.Preferences = append(pl.Preferences, swarm.PlacementPreference{Spread: &swarm.SpreadOver{SpreadDescriptor: label}})
			}
			s.Spec.TaskTemplate.Placement = pl
		}
		for _, target := range fs.Networks {
			s.Spec.TaskTemplate.Networks = append(s.Spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: target})
		}
		if len(fs.Mounts) > 0 || fs.Image != "" || len(fs.Secrets) > 0 || len(fs.Configs) > 0 {
			s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: fs.Image}
			for _, o := range fs.Secrets {
				ref := &swarm.SecretReference{SecretID: o.ID, SecretName: o.Name}
				if o.File != "" {
					ref.File = &swarm.SecretReferenceFileTarget{Name: o.File, UID: o.UID, GID: o.GID, Mode: os.FileMode(o.Mode)}
				}
				s.Spec.TaskTemplate.ContainerSpec.Secrets = append(s.Spec.TaskTemplate.ContainerSpec.Secrets, ref)
			}
			for _, o := range fs.Configs {
				ref := &swarm.ConfigReference{ConfigID: o.ID, ConfigName: o.Name}
				if o.File != "" {
					ref.File = &swarm.ConfigReferenceFileTarget{Name: o.File, UID: o.UID, GID: o.GID, Mode: os.FileMode(o.Mode)}
				} else {
					ref.Runtime = &swarm.ConfigReferenceRuntimeTarget{}
				}
				s.Spec.TaskTemplate.ContainerSpec.Configs = append(s.Spec.TaskTemplate.ContainerSpec.Configs, ref)
			}
			for _, m := range fs.Mounts {
				s.Spec.TaskTemplate.ContainerSpec.Mounts = append(s.Spec.TaskTemplate.ContainerSpec.Mounts, mount.Mount{Type: mount.Type(m.Type), Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
			}
		}
		s.UpdatedAt = fs.UpdatedAt
		switch fs.Mode {
		case "global":
			s.Spec.Mode.Global = &swarm.GlobalService{}
		case "replicated-job":
			s.Spec.Mode.ReplicatedJob = &swarm.ReplicatedJob{}
		case "global-job":
			s.Spec.Mode.GlobalJob = &swarm.GlobalJob{}
		default:
			replicas := fs.Replicas
			if replicas == 0 {
				replicas = fs.Desired
			}
			s.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &replicas}
		}
		if withStatus {
			s.ServiceStatus = &swarm.ServiceStatus{RunningTasks: fs.Running, DesiredTasks: fs.Desired}
		}
		fs.decorate(&s)
		out = append(out, s)
	}
	return out
}

// decorate sets what every row has whatever made its spec: the update status and its
// message, the endpoint's published ports, the previous spec and the creation time.
func (fs FakeService) decorate(s *swarm.Service) {
	if fs.UpdateState != "" || fs.UpdateMessage != "" {
		s.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateState(fs.UpdateState), Message: fs.UpdateMessage}
	}
	if len(fs.Ports) > 0 {
		s.Endpoint.Ports = slices.Clone(fs.Ports)
	}
	s.PreviousSpec = fs.PreviousSpec
	s.CreatedAt = fs.CreatedAt
}
