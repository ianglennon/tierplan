// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"slices"
	"time"
)

// What the node, the image and TierPlan's service spec must agree on lives here, once, because the
// supervisor is a separate module that cannot import internal/ (PRX-05).

// The ports and paths a node answers on, all unprivileged (D-06).
const (
	// ReadyPort is the port of the supervisor's readiness endpoint (D-17), which Phases 24 and 26 probe.
	ReadyPort = 8083
	// ReadyPath is the readiness path on ReadyPort (D-17).
	ReadyPath = "/readyz"
	// PingPort is the port of Traefik's own ping entrypoint, which answers liveness only.
	PingPort = 8082
	// PingPath is the path Traefik's ping answers on PingPort.
	PingPath = "/ping"
)

// The image's Docker HEALTHCHECK values (D-18). --start-interval is not used because Engine 24
// (API 1.43) lacks it. The start period is the backoff for a node that has no cached bundle and no
// TierPlan to fetch one from: it fails its health check for the whole period, and Swarm replaces it
// about once every 11.5 minutes instead of in a tight loop.
const (
	// HealthcheckInterval is the time between health probes.
	HealthcheckInterval = 30 * time.Second
	// HealthcheckTimeout is how long one probe may take; it is shorter than HealthcheckInterval.
	HealthcheckTimeout = 5 * time.Second
	// HealthcheckRetries is the number of failed probes after the start period that make a task
	// unhealthy.
	HealthcheckRetries = 3
	// HealthcheckStartPeriod is the time after start during which failed probes do not count.
	HealthcheckStartPeriod = 10 * time.Minute
)

// The stop times (D-11, D-21). Each stage waits for the one inside it, so each is longer than the last.
const (
	// TraefikGraceTimeout is how long Traefik lets in-flight requests finish after it is told to stop
	// (D-11). It equals graceTimeOut on the web and websecure entrypoints in static/traefik.yml, which
	// TestStaticConfig in internal/ingressrender proves, so the file and this constant cannot drift.
	TraefikGraceTimeout = 10 * time.Second
	// SupervisorStopWait is how long the supervisor waits for Traefik after forwarding SIGTERM before it
	// sends SIGKILL (D-21).
	SupervisorStopWait = 20 * time.Second
	// StopGracePeriod is the stop_grace_period Phase 24 sets on the service (D-21): Docker's wait
	// before it kills the whole container.
	StopGracePeriod = 30 * time.Second
)

// SupportedSchemas is the bundle schemas a node applies, as a new slice each call. The supervisor sends
// it in hello (D-23).
func SupportedSchemas() []int { return []int{Schema2} }

// The reasons a node refuses a bundle, added to the verifier's nine above (D-15, D-37). Like those they
// are stable codes: they go on the wire in rejected{id, reason} and Phase 27 shows them, so one is
// never renamed, only added. A reason is a code only, never a path or file content.
const (
	// ReasonInstanceMismatch means the envelope's instance ID differs from the one this node is pinned to.
	ReasonInstanceMismatch = "instance_mismatch"
	// ReasonTooLarge means the bundle is larger than the wire's maximum.
	ReasonTooLarge = "too_large"
	// ReasonSchemaUnsupported means the bundle's schema is one this node does not apply.
	ReasonSchemaUnsupported = "schema_unsupported"
	// ReasonMalformed means the message does not decode into a bundle.
	ReasonMalformed = "malformed"
	// ReasonYAMLInvalid means the dynamic file is not valid YAML.
	ReasonYAMLInvalid = "yaml_invalid"
	// ReasonTemplateSyntax means the dynamic file contains "{{", which Traefik would expand as a template.
	ReasonTemplateSyntax = "template_syntax"
	// ReasonCertMissing means a certificate path in the dynamic file is missing from the bundle.
	ReasonCertMissing = "cert_missing"
	// ReasonCertPath means a certificate or key path in the dynamic file is not one under the node's certs directory.
	ReasonCertPath = "cert_path"
	// ReasonKeyMismatch means a certificate chain and its private key do not belong together.
	ReasonKeyMismatch = "key_mismatch"
	// ReasonTLSOptions means the dynamic file lacks tls.options.default.sniStrict: true or sets a
	// tls.stores.default certificate (D-37).
	ReasonTLSOptions = "tls_options"
	// ReasonApplyFailed means the node could not put the verified bundle in place.
	ReasonApplyFailed = "apply_failed"
)

// RejectReasons is the closed set of reason codes a node may report, sorted, as a new slice each call:
// the nine the verifier uses and the eleven the supervisor adds. Each is lower snake case and far
// shorter than the wire's reason limit.
func RejectReasons() []string {
	all := []string{
		ReasonChecksum, ReasonMissingFile, ReasonMissingEntry, ReasonID, ReasonPath,
		ReasonDuplicate, ReasonSchema, ReasonMode, ReasonStep,
		ReasonInstanceMismatch, ReasonTooLarge, ReasonSchemaUnsupported, ReasonMalformed,
		ReasonYAMLInvalid, ReasonTemplateSyntax, ReasonCertMissing, ReasonCertPath,
		ReasonKeyMismatch, ReasonTLSOptions, ReasonApplyFailed,
	}
	slices.Sort(all)
	return all
}
