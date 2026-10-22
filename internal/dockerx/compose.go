// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/sirupsen/logrus"
)

// composeConfigDirName is the directory under the data directory that holds the Docker CLI configuration
// the Compose SDK reads: always empty, so no credential, credential helper, context or proxy setting of
// the server's own user ever reaches a deploy (RESEARCH Q1, T-07-97).
const composeConfigDirName = "compose-config"

// composeMaxConcurrency bounds how many engine calls one SDK instance makes at once: a tunnel carries 64
// streams per agent session and refuses the 65th, and the SDK is unbounded by default (Pitfall 9).
const composeMaxConcurrency = 8

// ErrComposeNotReady means ComposeUp or another Compose method was called before InitCompose.
var ErrComposeNotReady = errors.New("dockerx: InitCompose has not been called")

var (
	composeMu  sync.Mutex
	composeDir string
)

// InitCompose prepares the process for the Compose SDK, once, at startup. It creates the SDK's private
// Docker CLI configuration directory under dataDir (mode 0700, empty: Bayplan passes registry credentials
// itself and the SDK never reads a login), points the CLI library's process-wide configuration directory at
// it, and sends the SDK's logging (logrus, which the SDK and compose-go write to) to log. All three are
// process-global state, which is why they are set here and never per deploy (RESEARCH Pitfall 11).
//
// Calling it again with the same directory changes nothing but the logger's destination.
func InitCompose(dataDir string, log *slog.Logger) error {
	if dataDir == "" {
		return errors.New("dockerx: the Compose SDK needs a data directory")
	}
	if log == nil {
		log = slog.Default()
	}
	dir := filepath.Join(dataDir, composeConfigDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the Compose SDK configuration directory: %w", err)
	}
	// MkdirAll leaves the mode of a directory that exists: an operator or an older version may have
	// made it wider.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs the execute bit; 0700 is the narrowest mode that works
		return fmt.Errorf("restrict the Compose SDK configuration directory: %w", err)
	}

	composeMu.Lock()
	defer composeMu.Unlock()
	config.SetDir(dir)
	composeDir = dir
	std := logrus.StandardLogger()
	std.SetOutput(io.Discard)
	std.ReplaceHooks(make(logrus.LevelHooks))
	std.AddHook(slogHook{log: log})
	return nil
}

func composeReady() bool {
	composeMu.Lock()
	defer composeMu.Unlock()
	return composeDir != ""
}

// slogHook sends a logrus entry to slog at the matching level.
type slogHook struct{ log *slog.Logger }

func (slogHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h slogHook) Fire(e *logrus.Entry) error {
	level := slog.LevelInfo
	switch e.Level {
	case logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel:
		level = slog.LevelError
	case logrus.WarnLevel:
		level = slog.LevelWarn
	case logrus.DebugLevel, logrus.TraceLevel:
		level = slog.LevelDebug
	}
	attrs := make([]any, 0, 2*len(e.Data)+2)
	attrs = append(attrs, "source", "compose-sdk")
	for k, v := range e.Data {
		attrs = append(attrs, k, v)
	}
	h.log.Log(context.Background(), level, e.Message, attrs...)
	return nil
}

// ComposeUpOptions are the few things a deploy sets about a Compose up.
type ComposeUpOptions struct {
	// WaitTimeout is how long the SDK waits for the containers to be running (or healthy, when a service
	// has a health check) before it gives up. Zero waits as long as the context lives.
	WaitTimeout time.Duration
	// ForceRecreate are the services whose containers the up recreates though their configuration hash
	// is the one on the container: a container changed by hand on the host keeps its labels, and the SDK
	// compares labels only (mustRecreate), so it would leave such a container as it is.
	ForceRecreate []string
}

// The kinds of resource a ComposeEvent is about.
const (
	ComposeKindContainer = "container"
	ComposeKindNetwork   = "network"
	ComposeKindVolume    = "volume"
	ComposeKindImage     = "image"
)

// ComposeEvent is one progress event of the Compose SDK, for display: what the SDK is doing to which
// container, network, volume or image. A deploy never decides anything from the text of an event
// (RESEARCH anti-pattern); its result comes from the error ComposeUp returns and from ComposeContainers.
type ComposeEvent struct {
	// Kind is one of the ComposeKind words, empty for an event about the project as a whole.
	Kind string
	// Name is the name of the container, network, volume or image the event is about.
	Name string
	// Container is Name for a container event, and Service the service in the compose file the container
	// belongs to (empty for an event that is not about a container, or a container no service names).
	Container, Service string
	// Status is the SDK's word for the step (Creating, Starting, Running, Healthy, Recreated, Error ...).
	Status string
	// Text is the SDK's own words when it has more to say than Status, empty otherwise. It can hold what
	// the engine answered, so a caller cleans and masks it before it shows it.
	Text string
	// Done is true for an event that ends a step successfully, Failed for one that ends it with an error.
	Done, Failed bool
}

// eventBridge turns the SDK's events into ComposeEvents and calls on for each, one at a time: the SDK
// reports from several goroutines.
type eventBridge struct {
	mu      sync.Mutex
	project *types.Project
	on      func(ComposeEvent)
}

var _ api.EventProcessor = (*eventBridge)(nil)

func (b *eventBridge) Start(context.Context, string) {}

func (b *eventBridge) Done(string, bool) {}

func (b *eventBridge) On(events ...api.Resource) {
	if b.on == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range events {
		b.on(translateEvent(b.project, r))
	}
}

// translateEvent reads an SDK resource event. The SDK names a resource "Container <name>", "Network
// <name>", "Volume <name>" or "Image <ref>" and uses the bare word Compose for the project.
func translateEvent(p *types.Project, r api.Resource) ComposeEvent {
	ev := ComposeEvent{Status: r.Text, Done: r.Status == api.Done, Failed: r.Status == api.Error}
	if r.Details != "" {
		ev.Text = r.Details
	}
	if ev.Failed && ev.Text == "" {
		ev.Text = r.Text
	}
	for prefix, kind := range map[string]string{
		"Container ": ComposeKindContainer, "Network ": ComposeKindNetwork,
		"Volume ": ComposeKindVolume, "Image ": ComposeKindImage,
	} {
		if name, ok := strings.CutPrefix(r.ID, prefix); ok {
			ev.Kind, ev.Name = kind, name
			break
		}
	}
	if ev.Kind == ComposeKindContainer {
		ev.Container = ev.Name
		ev.Service = serviceOfContainer(p, ev.Name)
	}
	return ev
}

// serviceOfContainer is the service of the project a container name belongs to: its container_name, or
// <project>-<service>-<n> (the separator was "_" in older Compose), after the temporary prefix a recreate
// gives (<12 hex>_<name>). Empty when no service matches.
func serviceOfContainer(p *types.Project, name string) string {
	if p == nil {
		return ""
	}
	if head, rest, ok := strings.Cut(name, "_"); ok && len(head) == 12 && isHex(head) {
		name = rest
	}
	best := ""
	for svcName, svc := range p.Services {
		matched := svc.ContainerName != "" && svc.ContainerName == name
		for _, sep := range []string{"-", "_"} {
			prefix := p.Name + sep + svcName + sep
			if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" && isDigits(rest) {
				matched = true
			}
		}
		// The longest service name wins, so web-2 is not mistaken for web.
		if matched && len(svcName) > len(best) {
			best = svcName
		}
	}
	return best
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// composeService builds the SDK for one deploy over this client, wrapped in the guard. Each deploy gets an
// instance of its own, so the events of one reach only its callback.
func (c *Client) composeService(p *types.Project, on func(ComposeEvent)) (api.Compose, error) {
	if !composeReady() {
		return nil, ErrComposeNotReady
	}
	cli, err := command.NewDockerCli(
		command.WithAPIClient(guardClient{APIClient: c.cur()}),
		command.WithInputStream(io.NopCloser(strings.NewReader(""))),
		command.WithOutputStream(io.Discard),
		command.WithErrorStream(io.Discard),
	)
	if err != nil {
		return nil, fmt.Errorf("build the Docker CLI for the Compose SDK: %w", err)
	}
	// The API client is already set, so the host and the context are never looked up; naming a host
	// keeps DOCKER_HOST and DOCKER_CONTEXT of the server's environment out of it (Pitfall 11). The
	// configuration directory was set once by InitCompose: setting it here would write a process
	// global while another deploy reads it.
	opts := cliflags.NewClientOptions()
	opts.Hosts = []string{hostURL}
	if err := cli.Initialize(opts); err != nil {
		return nil, fmt.Errorf("initialise the Docker CLI for the Compose SDK: %w", err)
	}
	svc, err := compose.NewComposeService(cli,
		compose.WithMaxConcurrency(composeMaxConcurrency),
		compose.WithPrompt(compose.AlwaysOkPrompt()),
		compose.WithStreams(io.Discard, io.Discard, strings.NewReader("")),
		compose.WithEventProcessor(&eventBridge{project: p, on: on}),
	)
	if err != nil {
		return nil, fmt.Errorf("build the Compose SDK: %w", err)
	}
	return svc, nil
}

// ComposeUp brings the project up on this engine the way `docker compose up --detach --wait
// --remove-orphans` does: it creates the networks and volumes the project declares, creates the
// containers that are missing, recreates the ones whose configuration changed, leaves the others alone,
// removes the containers of services the project no longer declares, starts everything and waits until the
// containers run (and are healthy, where a service has a health check). on, when not nil, is called for
// each progress event, one at a time.
//
// The SDK works over this client wrapped in a guard that refuses every call a deploy must not make (build,
// save, load, push, import, export, commit, copy out, exec, attach, anything of the swarm, a plugin
// install): see ErrComposeForbidden. It is not retried here; a deploy that failed is run again by the
// person who started it. The project must already be sandboxed: no build, no file-backed secret or config,
// no relative bind mount (stacks.PrepareStandalone).
func (c *Client) ComposeUp(ctx context.Context, p *types.Project, o ComposeUpOptions, on func(ComposeEvent)) error {
	svc, err := c.composeService(p, on)
	if err != nil {
		return err
	}
	return svc.Up(ctx, p, composeUpOptions(p, o))
}

// composeUpOptions are the SDK's options for an up: orphans removed, every service recreated when its
// configuration diverged, and the wait for the containers to run.
//
// With services to force, the SDK is told they are the ones the user targeted and that the targeted ones are
// recreated whatever their labels say (Recreate force), while the others keep the diverged policy
// (RecreateDependencies): CreateOptions.Services does not narrow what converges, it only says which policy
// each service follows. A container changed by hand keeps every label, including the config hash, and the
// SDK's check (mustRecreate) reads labels, networks and volumes only, so without this it stays as it is.
func composeUpOptions(p *types.Project, o ComposeUpOptions) api.UpOptions {
	create := api.CreateOptions{
		RemoveOrphans: true, Recreate: api.RecreateDiverged, RecreateDependencies: api.RecreateDiverged, QuietPull: true,
	}
	if len(o.ForceRecreate) > 0 {
		create.Services = slices.Clone(o.ForceRecreate)
		create.Recreate = api.RecreateForce
	}
	return api.UpOptions{
		Create: create,
		Start:  api.StartOptions{Project: p, Wait: true, WaitTimeout: o.WaitTimeout},
	}
}

// ComposeStop stops the containers of the project the way `docker compose stop` does, with their own stop
// timeouts: nothing is removed, so the networks, volumes and containers stay for ComposeStart. p names the
// project and its services (the stop order follows depends_on); it need not be sandboxed. It returns once no
// container of the project runs: an engine older than 28 answers a stop that ran out its timeout as soon as it
// has sent SIGKILL, before the container has exited, so a start right after it would find a dependency still dying
// and refuse it ("exited (137)").
func (c *Client) ComposeStop(ctx context.Context, p *types.Project) error {
	svc, err := c.composeService(p, nil)
	if err != nil {
		return err
	}
	if err := svc.Stop(ctx, p.Name, api.StopOptions{Project: p}); err != nil {
		return err
	}
	return c.waitComposeStopped(ctx, p.Name)
}

// composeStopSettle bounds how long ComposeStop waits for a container the engine has said it stopped to exit.
// A SIGKILL takes effect at once, so this is only reached by a container the kernel cannot end.
const composeStopSettle = 30 * time.Second

// waitComposeStopped polls the containers of the project until none of those the SDK stops (every one but a
// one-off run) is running. It reads through the container list, which the agent already allows.
func (c *Client) waitComposeStopped(ctx context.Context, project string) error {
	ctx, cancel := context.WithTimeout(ctx, composeStopSettle)
	defer cancel()
	t := time.NewTicker(waitPoll)
	defer t.Stop()
	for {
		rows, err := c.ContainersWithLabel(ctx, api.ProjectLabel, project)
		if err != nil {
			return err
		}
		var running []string
		for _, r := range rows {
			if r.Labels[api.OneoffLabel] != "True" && (r.State == "running" || r.State == "restarting") {
				running = append(running, r.Name)
			}
		}
		if len(running) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stop project %s on %s: %s still running: %w", project, c.t.Describe(), strings.Join(running, ", "), ctx.Err())
		case <-t.C:
		}
	}
}

// ComposeStart starts the stopped containers of the project the way `docker compose start` does, in
// depends_on order, and waits until they run (and are healthy, where a service has a health check). It
// creates nothing: a service without a container is not started.
func (c *Client) ComposeStart(ctx context.Context, p *types.Project) error {
	svc, err := c.composeService(p, nil)
	if err != nil {
		return err
	}
	return svc.Start(ctx, p.Name, api.StartOptions{Project: p, Wait: true})
}

// ComposeDown removes the containers and the networks of the project the way `docker compose down
// --remove-orphans` does. It never removes a volume, an image or an external network (Volumes false and no
// Images): what a stack keeps is for the person who removes the stack to choose. The project is found by
// its name, from the labels of what the host holds.
func (c *Client) ComposeDown(ctx context.Context, projectName string) error {
	svc, err := c.composeService(&types.Project{Name: projectName}, nil)
	if err != nil {
		return err
	}
	return svc.Down(ctx, projectName, api.DownOptions{RemoveOrphans: true})
}

// ComposeContainer is a container of a Compose project as the engine lists it, with the labels the
// review and the pin decision read.
type ComposeContainer struct {
	ID, Name string
	// Service is the com.docker.compose.service label.
	Service string
	// ConfigHash is the com.docker.compose.config-hash label: ServiceHash of the service the container
	// was created from. Empty for a container Compose did not create from a service.
	ConfigHash string
	// Image is the image the container was created from, as the engine lists it: the reference it was
	// created with (repo:tag@sha256:...), or the image ID once the reference has moved.
	Image string
	// State is the engine's state word (running, exited, created ...), Health none, starting, healthy
	// or unhealthy.
	State, Health string
	// Labels is every label of the container.
	Labels map[string]string
}

// ComposeContainers lists the containers of a Compose project, stopped ones too, by the
// com.docker.compose.project label, the filter built with the engine client's Filters type. One-off
// containers (`compose run`) are left out: they are not part of the project's services. It is a read.
func (c *Client) ComposeContainers(ctx context.Context, project string) ([]ComposeContainer, error) {
	rows, err := c.ContainersWithLabel(ctx, api.ProjectLabel, project)
	if err != nil {
		return nil, err
	}
	out := make([]ComposeContainer, 0, len(rows))
	for _, r := range rows {
		if strings.EqualFold(r.Labels[api.OneoffLabel], "true") {
			continue
		}
		out = append(out, ComposeContainer{
			ID: r.ID, Name: r.Name, Service: r.Labels[api.ServiceLabel], ConfigHash: r.Labels[api.ConfigHashLabel],
			Image: r.Image, State: r.State, Health: r.Health, Labels: r.Labels,
		})
	}
	return out, nil
}

// ServiceHash is the configuration hash the Compose SDK stamps on a container it creates from a service
// (com.docker.compose.config-hash), computed the same way: a container whose label differs from the hash
// of the service as it is now is recreated by the next Up. It leaves out what the SDK leaves out (build,
// pull policy, scale, replicas, depends_on, profiles), and custom labels are not in it.
func ServiceHash(svc types.ServiceConfig) (string, error) { return compose.ServiceHash(svc) }
