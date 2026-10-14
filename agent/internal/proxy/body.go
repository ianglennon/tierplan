// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
)

// bodyEnv is what a validator needs besides the bytes: the host's policy and a way
// to ask the agent's own engine about a network. The zero value is the strictest
// reading: nothing allowed, no lookup.
type bodyEnv struct {
	policy Policy
	lookup networkLookup
	ctx    context.Context
}

// maxBody caps a request body of every kind but a service spec. Every such body is
// a small JSON object, so a larger one is a refusal, not a truncation.
const maxBody = 64 << 10

// maxServiceSpecBody caps a service spec, which carries a whole container spec
// (environment, mounts, labels, healthcheck, secret and config references).
const maxServiceSpecBody = 256 << 10

// bodyCap is the most bytes a body of one kind may have. A body over the cap is
// refused, never cut short.
func bodyCap(kind BodyKind) int64 {
	if kind == BodyServiceSpec {
		return maxServiceSpecBody
	}
	return maxBody
}

const (
	maxMapEntries = 64
	maxMapKey     = 256
	maxMapValue   = 4096
	maxCmdEntries = 64
	maxArgBytes   = 4096
	maxConsole    = 1000
	maxAliases    = 16
	maxAliasChars = 253
)

var (
	idRe            = regexp.MustCompile(`^` + idShape + `$`)
	networkNameRe   = idRe
	volumeNameRe    = regexp.MustCompile(`^` + volumeShape + `$`)
	volumeDriverRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
	errBodyRefused  = errors.New("request body refused")
	networkDrivers  = []string{"", "bridge", "overlay", "macvlan", "ipvlan"}
	networkScopes   = []string{"", "local", "swarm"}
	ipamDriverNames = []string{"", "default"}
)

// refuse is the one error a validator returns. Its text names the rule for the
// log and is never sent back to the server, which only sees the typed refusal.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBodyRefused, fmt.Sprintf(format, args...))
}

// validateBody reads one request body of the given kind and returns the bytes to
// forward in its place. It never returns the bytes it was given: the body is
// decoded into the engine's own request type with unknown fields refused, a fresh
// value is built that carries only the allowlisted fields, and the decoded value
// must equal that fresh value, so any other field that was set is a refusal. What
// is forwarded is the fresh value encoded again, so a field the decoder accepted
// but the allowlist does not name cannot cross.
func validateBody(kind BodyKind, r io.Reader, env bodyEnv) ([]byte, error) {
	limit := bodyCap(kind)
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, refuse("reading the body: %v", err)
	}
	if int64(len(raw)) > limit {
		return nil, refuse("the body is over %d bytes", limit)
	}
	var clean any
	switch kind {
	case BodyExecCreate:
		clean, err = validateExecCreate(raw)
	case BodyExecStart:
		clean, err = validateExecStart(raw)
	case BodyNetworkCreate:
		clean, err = validateNetworkCreate(raw)
	case BodyNetworkConnect:
		clean, err = validateNetworkConnect(raw)
	case BodyNetworkDisconnect:
		clean, err = validateNetworkDisconnect(raw)
	case BodyVolumeCreate:
		clean, err = validateVolumeCreate(raw)
	case BodyServiceSpec:
		clean, err = validateServiceSpec(raw, env)
	case BodyNodeSpec:
		clean, err = validateNodeSpec(raw)
	default:
		return nil, refuse("a route with no body kind has no validator")
	}
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return nil, refuse("encoding the rebuilt body: %v", err)
	}
	return out, nil
}

// decodeStrict decodes exactly one JSON value into v, refusing unknown fields and
// anything after the value.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse("not the expected JSON object: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse("data after the JSON value")
	}
	return nil
}

// sameAsAllowlisted refuses the body when the decoded value carries anything the
// freshly built value does not.
func sameAsAllowlisted(decoded, clean any) error {
	if !reflect.DeepEqual(decoded, clean) {
		return refuse("a field that is not on the allowlist was set")
	}
	return nil
}

func checkMap(name string, m map[string]string) error {
	if len(m) > maxMapEntries {
		return refuse("%s has more than %d entries", name, maxMapEntries)
	}
	for k, v := range m {
		if len(k) > maxMapKey || len(v) > maxMapValue {
			return refuse("%s has an entry that is too long", name)
		}
	}
	return nil
}

func checkConsole(size *[2]uint) error {
	if size != nil && (size[0] > maxConsole || size[1] > maxConsole) {
		return refuse("the console size is over %d", maxConsole)
	}
	return nil
}

func validateExecCreate(raw []byte) (container.ExecCreateRequest, error) {
	var in container.ExecCreateRequest
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	// The moby client encodes a nil Env as null; an empty list says the same.
	if len(in.Env) == 0 {
		in.Env = nil
	}
	clean := container.ExecCreateRequest{
		User:         in.User,
		Tty:          in.Tty,
		ConsoleSize:  in.ConsoleSize,
		AttachStdin:  in.AttachStdin,
		AttachStderr: in.AttachStderr,
		AttachStdout: in.AttachStdout,
		WorkingDir:   in.WorkingDir,
		Cmd:          in.Cmd,
	}
	// Privileged, DetachKeys and Env are not copied, so any of them set is a
	// refusal here (T-04-06): the daemon accepts a privileged exec.
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	switch {
	case len(clean.User) > 64:
		return in, refuse("the user is over 64 characters")
	case !clean.Tty:
		return in, refuse("an exec without a terminal is not forwarded")
	case len(clean.WorkingDir) > maxArgBytes:
		return in, refuse("the working directory is too long")
	case len(clean.Cmd) < 1 || len(clean.Cmd) > maxCmdEntries:
		return in, refuse("the command must have 1 to %d entries", maxCmdEntries)
	}
	for _, arg := range clean.Cmd {
		if len(arg) > maxArgBytes {
			return in, refuse("a command entry is over %d bytes", maxArgBytes)
		}
	}
	if err := checkConsole(clean.ConsoleSize); err != nil {
		return in, err
	}
	return clean, nil
}

func validateExecStart(raw []byte) (container.ExecStartRequest, error) {
	var in container.ExecStartRequest
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	clean := container.ExecStartRequest{Tty: in.Tty, ConsoleSize: in.ConsoleSize}
	// Detach is not copied: a detached exec is not a shell and is refused.
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	if !clean.Tty {
		return in, refuse("an exec start without a terminal is not forwarded")
	}
	if err := checkConsole(clean.ConsoleSize); err != nil {
		return in, err
	}
	return clean, nil
}

func validateNetworkCreate(raw []byte) (network.CreateRequest, error) {
	var in network.CreateRequest
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	// An empty options map or list says the same as null.
	if in.IPAM != nil {
		if len(in.IPAM.Options) == 0 {
			in.IPAM.Options = nil
		}
		for i := range in.IPAM.Config {
			if len(in.IPAM.Config[i].AuxAddress) == 0 {
				in.IPAM.Config[i].AuxAddress = nil
			}
		}
	}
	clean := network.CreateRequest{
		Name:       in.Name,
		Driver:     in.Driver,
		Scope:      in.Scope,
		EnableIPv4: in.EnableIPv4,
		EnableIPv6: in.EnableIPv6,
		Internal:   in.Internal,
		Attachable: in.Attachable,
		Options:    in.Options,
		Labels:     in.Labels,
	}
	if in.IPAM != nil {
		ipam := &network.IPAM{Driver: in.IPAM.Driver}
		if in.IPAM.Config != nil {
			ipam.Config = make([]network.IPAMConfig, len(in.IPAM.Config))
			for i, c := range in.IPAM.Config {
				ipam.Config[i] = network.IPAMConfig{Subnet: c.Subnet, IPRange: c.IPRange, Gateway: c.Gateway}
			}
		}
		clean.IPAM = ipam
	}
	// Ingress, ConfigOnly, ConfigFrom, IPAM options and auxiliary addresses are
	// not copied, so any of them set is a refusal (T-04-09).
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	switch {
	case !networkNameRe.MatchString(clean.Name):
		return in, refuse("the network name is not a valid name")
	case !slices.Contains(networkDrivers, clean.Driver):
		return in, refuse("the network driver is not one of bridge, overlay, macvlan, ipvlan")
	case !slices.Contains(networkScopes, clean.Scope):
		return in, refuse("the network scope is not local or swarm")
	case clean.IPAM != nil && !slices.Contains(ipamDriverNames, clean.IPAM.Driver):
		return in, refuse("the IPAM driver is not the default")
	}
	if err := checkMap("Options", clean.Options); err != nil {
		return in, err
	}
	if err := checkMap("Labels", clean.Labels); err != nil {
		return in, err
	}
	if clean.IPAM != nil && len(clean.IPAM.Config) > maxMapEntries {
		return in, refuse("too many IPAM configs")
	}
	return clean, nil
}

func validateNetworkConnect(raw []byte) (network.ConnectRequest, error) {
	var in network.ConnectRequest
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	// The moby client encodes every field of the endpoint settings, zero values
	// included; an empty list or map says the same as null.
	if ep := in.EndpointConfig; ep != nil {
		if len(ep.Links) == 0 {
			ep.Links = nil
		}
		if len(ep.DriverOpts) == 0 {
			ep.DriverOpts = nil
		}
		if len(ep.DNSNames) == 0 {
			ep.DNSNames = nil
		}
		if ep.IPAMConfig != nil && len(ep.IPAMConfig.LinkLocalIPs) == 0 {
			ep.IPAMConfig.LinkLocalIPs = nil
		}
	}
	clean := network.ConnectRequest{Container: in.Container}
	if ep := in.EndpointConfig; ep != nil {
		clean.EndpointConfig = &network.EndpointSettings{Aliases: ep.Aliases}
		if ep.IPAMConfig != nil {
			clean.EndpointConfig.IPAMConfig = &network.EndpointIPAMConfig{
				IPv4Address: ep.IPAMConfig.IPv4Address,
				IPv6Address: ep.IPAMConfig.IPv6Address,
			}
		}
	}
	// Links, DriverOpts, GwPriority, the MAC address and every other field of the
	// endpoint settings are not copied, so any of them set is a refusal.
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	if !idRe.MatchString(clean.Container) {
		return in, refuse("the container is not a valid ID or name")
	}
	if ep := clean.EndpointConfig; ep != nil {
		if len(ep.Aliases) > maxAliases {
			return in, refuse("more than %d aliases", maxAliases)
		}
		for _, a := range ep.Aliases {
			if len(a) > maxAliasChars {
				return in, refuse("an alias is over %d characters", maxAliasChars)
			}
		}
	}
	return clean, nil
}

func validateNetworkDisconnect(raw []byte) (network.DisconnectRequest, error) {
	var in network.DisconnectRequest
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	clean := network.DisconnectRequest{Container: in.Container}
	// Force is not copied: a forced disconnect is not offered.
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	if !idRe.MatchString(clean.Container) {
		return in, refuse("the container is not a valid ID or name")
	}
	return clean, nil
}

func validateVolumeCreate(raw []byte) (volume.CreateRequest, error) {
	var in volume.CreateRequest
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	clean := volume.CreateRequest{
		Name:       in.Name,
		Driver:     in.Driver,
		DriverOpts: in.DriverOpts,
		Labels:     in.Labels,
	}
	// ClusterVolumeSpec is not copied: a cluster volume is not offered.
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	switch {
	case clean.Name != "" && !volumeNameRe.MatchString(clean.Name):
		return in, refuse("the volume name is not a valid name")
	case clean.Driver != "" && !volumeDriverRe.MatchString(clean.Driver):
		return in, refuse("the volume driver is not a valid name")
	}
	if err := checkMap("DriverOpts", clean.DriverOpts); err != nil {
		return in, err
	}
	if err := checkMap("Labels", clean.Labels); err != nil {
		return in, err
	}
	return clean, nil
}

// validateServiceSpec decodes the body of a service update into the engine's own
// swarm.ServiceSpec with unknown fields refused. The whole spec is the allowlist: it
// is a user's service, and every field of it is the user's to set. What is refused is
// what is not a container service: a plugin or a network attachment task, and a
// runtime other than container. The value returned is encoded again, so what is
// forwarded is what the decoder understood. A spec that gives tasks host access
// (host mounts, added capabilities, a lowered sandbox, host networking) is refused
// with errHostPolicy unless the host allows it (checkHostPolicy).
func validateServiceSpec(raw []byte, env bodyEnv) (swarm.ServiceSpec, error) {
	var in swarm.ServiceSpec
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	tt := in.TaskTemplate
	switch {
	case tt.Runtime != "" && tt.Runtime != swarm.RuntimeContainer:
		return in, refuse("the task runtime is not container")
	case tt.PluginSpec != nil:
		return in, refuse("a plugin spec is not forwarded")
	case tt.NetworkAttachmentSpec != nil:
		return in, refuse("a network attachment spec is not forwarded")
	}
	// Host access (D-36): a spec that mounts the host, adds capabilities, lowers
	// the sandbox or joins the host network is refused unless the host owner allowed
	// it with BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES.
	if err := checkHostPolicy(in, env); err != nil {
		return in, err
	}
	return in, nil
}

// validateNodeSpec decodes the body of a node update into the engine's own swarm.NodeSpec
// with unknown fields refused, and builds the value to forward from the four fields a node
// update may set: its name, its labels, its role (manager or worker) and its availability
// (active, pause or drain). The labels are bounded like every other free map. The role and
// availability are required, as the server always sends the spec it read with one field changed.
func validateNodeSpec(raw []byte) (swarm.NodeSpec, error) {
	var in swarm.NodeSpec
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	clean := swarm.NodeSpec{
		Annotations:  swarm.Annotations{Name: in.Name, Labels: in.Labels},
		Role:         in.Role,
		Availability: in.Availability,
	}
	if err := sameAsAllowlisted(in, clean); err != nil {
		return in, err
	}
	switch {
	case clean.Role != swarm.NodeRoleManager && clean.Role != swarm.NodeRoleWorker:
		return in, refuse("the node role is not manager or worker")
	case clean.Availability != swarm.NodeAvailabilityActive && clean.Availability != swarm.NodeAvailabilityPause &&
		clean.Availability != swarm.NodeAvailabilityDrain:
		return in, refuse("the node availability is not active, pause or drain")
	case len(clean.Name) > maxAliasChars:
		return in, refuse("the node name is over %d characters", maxAliasChars)
	}
	if err := checkMap("Labels", clean.Labels); err != nil {
		return in, err
	}
	return clean, nil
}
