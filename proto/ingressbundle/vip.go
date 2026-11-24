// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// What the keepalived helper (the ingress module's vip subcommands) and the TierPlan server must agree on
// lives here, once (D-15). The helper is a separate module that cannot import internal/, and the server
// reads the helper's output from a node it does not control, so no second copy of any string exists: the
// env names the server writes into the VIP service, the event vocabulary the helper writes and the server
// parses, and the numbers both sides depend on.

// The ports and times of the virtual-IP health check and its authentication.
const (
	// ReadyPublishedPort is the host port the swarm proxy publishes its readiness endpoint (ReadyPort) on,
	// so the keepalived container, which runs in the host network namespace, can ask the node's own proxy
	// whether it is ready (D-13). It sits next to TierPlan's own 7480 and below the ephemeral range, so no
	// outgoing connection picks it by chance. It is visible on the LAN; the endpoint stays a bare status
	// code and carries no body.
	ReadyPublishedPort = 7483
	// VIPProbeTimeout is how long `tierplan-ingress probe` waits for the readiness endpoint (D-12). It is
	// below the one-second vrrp_script timeout, so a stuck endpoint makes the probe report its own failure
	// instead of keepalived killing it.
	VIPProbeTimeout = 800 * time.Millisecond
	// VIPAuthWindow is the auth_hmac time window (A-04): an advert whose trailer is older than this, by the
	// sender's clock, is refused as stale, so two nodes' clocks must agree to within it.
	VIPAuthWindow = 30 * time.Second
)

// The environment variables the VIP service sets for the helper. The server writes them and the helper
// reads them.
const (
	// EnvVIPAddress is the virtual address with its prefix, for example 192.168.1.250/24.
	EnvVIPAddress = "TIERPLAN_VIP_ADDRESS"
	// EnvVIPRouterID is the VRRP router ID (VRID) of the environment, 1 to 255.
	EnvVIPRouterID = "TIERPLAN_VIP_ROUTER_ID"
	// EnvVIPProbePort is the port the probe asks on 127.0.0.1, ReadyPublishedPort.
	EnvVIPProbePort = "TIERPLAN_VIP_PROBE_PORT"
	// EnvVIPKeyFile is the path of the swarm secret holding the auth_hmac key.
	EnvVIPKeyFile = "TIERPLAN_VIP_KEY_FILE" //nolint:gosec // the name of a variable, not a credential
	// EnvVIPNodeID is the swarm node ID of the node the helper runs on.
	EnvVIPNodeID = "TIERPLAN_VIP_NODE_ID"
	// EnvVIPNodeName is the node's host name, for display.
	EnvVIPNodeName = "TIERPLAN_VIP_NODE_NAME"
)

// The wire format of one event line. The helper writes one JSON object per line on its standard output and
// the server reads those lines out of a node's container log (D-15, D-21). A node's log is untrusted input to
// the server: a compromised node can write anything, so a line is parsed strictly, every value is from a
// closed vocabulary or bounded, and no raw keepalived text is ever carried (Pitfall 15).
const (
	// VIPLineVersion is the schema version of a line, in the v field. A server refuses any other value, so a
	// later helper that changes the format must bump it, and an older server then ignores its lines instead
	// of misreading them.
	VIPLineVersion = 1
	// VIPLineMax is the longest line, in bytes, that ParseVIPLine reads. The longest legal event, with eight
	// interface names, is a few hundred bytes, so a line over this is not one of ours.
	VIPLineMax = 1024
)

// The events a line reports.
const (
	// VIPEventStart is the helper's start: the interface it chose and whether it found a stale address.
	VIPEventStart = "start"
	// VIPEventState is a VRRP state change of this node.
	VIPEventState = "state"
	// VIPEventFault is a fault of the helper or the check, with a reason.
	VIPEventFault = "fault"
	// VIPEventForeignRouter is another router on the segment using this router ID (A-04).
	VIPEventForeignRouter = "foreign_router"
	// VIPEventClockSkew is a rising Stale counter: the clocks of two routers disagree by more than
	// VIPAuthWindow (A-04).
	VIPEventClockSkew = "clock_skew"
)

// The VRRP states of a state event.
const (
	VIPStateMaster = "MASTER"
	VIPStateBackup = "BACKUP"
	VIPStateFault  = "FAULT"
	VIPStateStop   = "STOP"
)

// The reasons of a fault event, or of a FAULT state.
const (
	// VIPReasonNoInterface means no interface of the node has an address inside the virtual address's
	// prefix (D-09).
	VIPReasonNoInterface = "no_interface"
	// VIPReasonAmbiguousInterface means more than one interface does (D-09).
	VIPReasonAmbiguousInterface = "ambiguous_interface"
	// VIPReasonAddressInUse means another host answers for the virtual address before this node holds it
	// (A-05).
	VIPReasonAddressInUse = "address_in_use"
	// VIPReasonProbeFailing means the node's own proxy is not ready, so it must not hold the address (D-12).
	VIPReasonProbeFailing = "probe_failing"
)

// The counters of a foreign_router event: which authentication error of keepalived's statistics rose (A-04).
const (
	VIPCounterInvalidHMAC = "invalid_hmac"
	VIPCounterMissing     = "missing"
	VIPCounterUnknownKey  = "unknown_key"
)

// vipMaxIfaces is the most interface names one line carries, and vipMaxName the longest node name.
const (
	vipMaxIfaces = 8
	vipMaxName   = 64
)

// VIPEvent is one line of the helper's report. Only V, Event, Node and Seq are always present.
type VIPEvent struct {
	// V is the schema version, VIPLineVersion.
	V int `json:"v"`
	// Event is one of the VIPEvent constants.
	Event string `json:"event"`
	// State is the VRRP state, for a state event.
	State string `json:"state,omitempty"`
	// Reason is one of the VIPReason constants.
	Reason string `json:"reason,omitempty"`
	// Node is the swarm node ID of the node that wrote the line.
	Node string `json:"node"`
	// Name is that node's host name, for display.
	Name string `json:"name,omitempty"`
	// Router is the VRRP router ID the helper runs, 1 to 255.
	Router int `json:"router,omitempty"`
	// Seq counts the helper's lines from one, so a gap or a repeat is visible.
	Seq uint64 `json:"seq"`
	// Iface is the interface the helper chose, for a start event.
	Iface string `json:"iface,omitempty"`
	// Ifaces are the interface names it saw, for a no_interface or ambiguous_interface fault.
	Ifaces []string `json:"ifaces,omitempty"`
	// Stale is true when a start event found the address already on the interface (D-10).
	Stale bool `json:"stale,omitempty"`
	// Active is true while a foreign_router or clock_skew condition holds and false when it has cleared.
	Active bool `json:"active,omitempty"`
	// Counter is one of the VIPCounter constants, for a foreign_router event.
	Counter string `json:"counter,omitempty"`
	// Addr is the sender's address of a foreign_router event, after netip validation.
	Addr string `json:"addr,omitempty"`
}

// ErrVIPLine is what every refusal of MarshalLine and ParseVIPLine wraps. The text after it names the rule,
// never the input, so a hostile line cannot reach a log or the UI through an error.
var ErrVIPLine = errors.New("ingress vip line refused")

// MarshalLine returns the event as one JSON line without its trailing newline. It sets V when it is zero and
// refuses an event the parser would refuse, so the helper cannot write a line the server drops.
func (e VIPEvent) MarshalLine() ([]byte, error) {
	if e.V == 0 {
		e.V = VIPLineVersion
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return nil, vipRefused("encode")
	}
	if len(line) > VIPLineMax {
		return nil, vipRefused("line too long")
	}
	return line, nil
}

// ParseVIPLine reads one line, with or without its trailing newline. The length cap comes first, so a hostile
// line is never decoded; the decode refuses any field it does not know, so a later helper's extra field is a
// version bump and not a silent pass; and every value is then checked against its rule.
func ParseVIPLine(line []byte) (VIPEvent, error) {
	if len(line) > VIPLineMax {
		return VIPEvent{}, vipRefused("line too long")
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	if bytes.ContainsAny(line, "\r\n") {
		return VIPEvent{}, vipRefused("line break inside the line")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var e VIPEvent
	if err := dec.Decode(&e); err != nil {
		// json's own errors name the offending field or value, so none of their text is passed on.
		return VIPEvent{}, vipRefused("not a valid event object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return VIPEvent{}, vipRefused("data after the event object")
	}
	if err := e.validate(); err != nil {
		return VIPEvent{}, err
	}
	return e, nil
}

// vipRefused wraps ErrVIPLine with a fixed rule name. The text is never built from the input.
func vipRefused(rule string) error { return fmt.Errorf("%w: %s", ErrVIPLine, rule) }

var (
	// vipNodeID is the shape of a swarm node ID: 25 lower-case alphanumerics.
	vipNodeID = regexp.MustCompile(`^[a-z0-9]{25}$`)
	// vipIfaceName is a Linux interface name as the helper reports it: at most 15 characters from a set that
	// holds no space, slash or control character.
	vipIfaceName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,15}$`)
)

// validate holds every rule of the format, shared by MarshalLine and ParseVIPLine so a helper cannot write
// what the server would refuse.
func (e VIPEvent) validate() error {
	if e.V != VIPLineVersion {
		return vipRefused("schema version")
	}
	switch e.Event {
	case VIPEventStart, VIPEventState, VIPEventFault, VIPEventForeignRouter, VIPEventClockSkew:
	default:
		return vipRefused("event")
	}
	switch e.State {
	case "", VIPStateMaster, VIPStateBackup, VIPStateFault, VIPStateStop:
	default:
		return vipRefused("state")
	}
	if e.Event == VIPEventState && e.State == "" {
		return vipRefused("state event without a state")
	}
	switch e.Reason {
	case "", VIPReasonNoInterface, VIPReasonAmbiguousInterface, VIPReasonAddressInUse, VIPReasonProbeFailing:
	default:
		return vipRefused("reason")
	}
	if e.Event == VIPEventFault && e.Reason == "" {
		return vipRefused("fault event without a reason")
	}
	switch e.Counter {
	case "", VIPCounterInvalidHMAC, VIPCounterMissing, VIPCounterUnknownKey:
	default:
		return vipRefused("counter")
	}
	if !vipNodeID.MatchString(e.Node) {
		return vipRefused("node id")
	}
	// The name is shown in the UI. A line is bounded to VIPLineMax anyway, so 64 characters is a display rule,
	// and a control character (an escape sequence) is refused because the name reaches a terminal-like view.
	if utf8.RuneCountInString(e.Name) > vipMaxName || !utf8.ValidString(e.Name) || strings.IndexFunc(e.Name, unicode.IsControl) >= 0 {
		return vipRefused("name")
	}
	if e.Router < 0 || e.Router > 255 {
		return vipRefused("router id")
	}
	if e.Iface != "" && !vipIfaceName.MatchString(e.Iface) {
		return vipRefused("interface name")
	}
	if len(e.Ifaces) > vipMaxIfaces {
		return vipRefused("too many interface names")
	}
	for _, n := range e.Ifaces {
		if !vipIfaceName.MatchString(n) {
			return vipRefused("interface name")
		}
	}
	if e.Addr != "" {
		if _, err := netip.ParseAddr(e.Addr); err != nil {
			return vipRefused("addr")
		}
	}
	return nil
}
