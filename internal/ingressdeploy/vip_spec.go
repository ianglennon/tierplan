// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The values of the keepalived service, the second service of the virtual IP (D-14). Each one is what
// the S3 spike measured on engines 24 and 29, and TestKeepalivedServiceSpec pins them.
const (
	// KeepalivedRepository is the keepalived image without a tag; goreleaser publishes it.
	KeepalivedRepository = "ghcr.io/ianglennon/tierplan-keepalived"

	// KeepalivedStopGrace is the stop_grace_period. keepalived leaves MASTER and drops the address on
	// SIGTERM within a second, so 10 s is only a ceiling.
	KeepalivedStopGrace = 10 * time.Second

	// KeepalivedUpdateDelay is the pause between two nodes of a rolling update (A-08, S3: 10s). A node
	// that was just updated needs about 2 s to be a candidate, so the next node's holder must not stop
	// before then; 10 s adds an advert timeout and a margin.
	KeepalivedUpdateDelay = 10 * time.Second

	// KeepalivedUpdateMonitor is how long swarm watches a node that was just updated before it takes the next one. It is swarm's
	// own default, named because swarm writes the default into the spec it stores: a spec that left it out would differ from the
	// stored one for ever, and the ensure loop would write the service on every pass.
	KeepalivedUpdateMonitor = 5 * time.Second

	// KeepalivedRestartDelay is how long swarm waits before it replaces a keepalived task that exited.
	// A keepalived that is killed leaves its address on the node's interface, and the node keeps
	// answering ARP for it until the replacement starts as BACKUP and clears it. S3 leg 2 measured
	// that window as 5.4 s with swarm's default delay of 5 s, longer than the 3.3 s election, so a
	// client could still reach the dead holder after another node had taken over. At 2 s the replacement
	// clears the address about 2.4 s after the crash (the 0.4 s is the start measured there), inside the
	// election, so the stale address no longer lengthens the outage. It is not lower because a task that
	// exits at once (a bad configuration, an address in use that a start cannot clear) is restarted
	// without a limit, and a 2 s delay keeps that to at most one start every two seconds.
	KeepalivedRestartDelay = 2 * time.Second

	// KeepalivedSecretTarget is the file name the VRRP key secret has inside the container. S3 found
	// the default target under /run/secrets mounts beneath the tmpfs on /run and keepalived reads it on
	// engines 24 and 29, so the spec keeps the read-only root and the default location.
	KeepalivedSecretTarget = "tierplan-keepalived-key" //nolint:gosec // G101: a file name, not a credential
	// KeepalivedKeyFile is the path keepalived's auth_hmac reads the key from. The environment carries
	// this path and never the key (A-03).
	KeepalivedKeyFile = "/run/secrets/" + KeepalivedSecretTarget //nolint:gosec // G101: a path, not a credential

	// keepalivedLogMaxSize and keepalivedLogMaxFile bound the keepalived json-file log on each node to
	// 3 MB. The helper writes one JSON line per event, so this is a long history.
	keepalivedLogMaxSize = "1m"
	keepalivedLogMaxFile = "3"
)

// KeepalivedImage is the keepalived image a server of serverVersion installs: the exact X.Y.Z for a
// release, edge otherwise, by the same rule as the agent and the proxy image.
func KeepalivedImage(serverVersion string) string {
	return agents.ImageFor(KeepalivedRepository, serverVersion)
}

// KeepalivedCaps is the capability set keepalived runs with after every capability is dropped: the
// `S3 caps:` line of the S3 result (A-01). NET_ADMIN adds and removes the address, NET_RAW opens the
// VRRP and ARP sockets, and SETUID and SETGID are what lets keepalived run the vrrp_script check as
// another user: without them the check never runs ("Couldn't setgroups") and a holder whose proxy is
// dead never faults. Each call returns a new slice.
func KeepalivedCaps() []string {
	return []string{"NET_ADMIN", "NET_RAW", "SETUID", "SETGID"}
}

// ErrKeepalivedKeyLength is returned for a VRRP key that is not keepalivedKeyBytes long.
var ErrKeepalivedKeyLength = errors.New("ingressdeploy: the VRRP key is not 32 bytes")

// keepalivedKeyBytes is how many random bytes the VRRP key has before it is written as text.
const keepalivedKeyBytes = 32

// KeepalivedSecretName is the swarm secret that holds generation gen of the VRRP key of environment
// envID: tierplan-keepalived-key-g<gen>-<envtag>. Every environment has its own name and every
// generation its own secret, so a key change creates the new secret beside the old one and the
// service update swaps them, and a restored install never collides with a secret an earlier install
// left on the swarm. It is 38 characters plus the digits of the generation, well under Docker's 64, and
// in the form of the agent's secret names (D-03, A-03).
func KeepalivedSecretName(envID string, generation int) string {
	return ingress.SecretPrefixKeepalived + "g" + strconv.Itoa(generation) + "-" + agents.SecretTag(envID)
}

// KeepalivedSecretContent is the content of the VRRP key secret for raw, which must be 32 bytes: the
// text `hex:` followed by 64 lower-case hex digits. keepalived reads the key file as text, and the
// prefix is how it reads the digits as bytes (A-03). The clear content exists only between the moment
// it is generated and the SecretCreate call that seals it into the swarm; this is the one place that
// builds it, and it is called nowhere else. The error names the length it got and never the bytes
// (PROH-26-03).
func KeepalivedSecretContent(raw []byte) ([]byte, error) {
	if len(raw) != keepalivedKeyBytes {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrKeepalivedKeyLength, len(raw), keepalivedKeyBytes)
	}
	out := make([]byte, 0, len("hex:")+hex.EncodedLen(len(raw)))
	out = append(out, "hex:"...)
	return hex.AppendEncode(out, raw), nil
}

// KeepalivedSpecInput is what the keepalived service spec is built from. None of it is the VRRP key:
// the key is a swarm secret that the spec only refers to, by name and by file path.
type KeepalivedSpecInput struct {
	// Image is the keepalived image with its tag (KeepalivedImage).
	Image string
	// Address is the virtual IP with the prefix length of the segment, for example 192.168.1.250/24.
	Address netip.Prefix
	// RouterID is the VRRP router ID (VRID) of the environment, 1 to 255.
	RouterID int
	// SecretName is the name of the swarm secret that holds the VRRP key (KeepalivedSecretName). The
	// caller fills in SecretID in the returned spec from the secret it created.
	SecretName string
}

// KeepalivedServiceSpec is the global service that runs keepalived on every node (D-14): on the host
// network with no published port, every capability dropped but KeepalivedCaps, a read-only root with a
// tmpfs on /run and /tmp (keepalived writes its stats file to /tmp), the VRRP key as a secret file
// and nothing else mounted, and a rolling update that moves one node at a time, stopping the old task
// first, with a delay so a holder never stops before another node is BACKUP.
//
// The service has no Healthcheck (A-08): the image is distroless and has nothing to run one with but
// the helper, and swarm reports a crashed task by itself, which is what the rollback needs. It
// publishes no port, because a host-network service cannot (Pitfall 8). The key appears only as a file
// path in the environment (A-03), never as a value. The configuration is rendered by the helper in the
// container, so there is no swarm config. The result is the same for every node, and the node's own ID
// and name come from the templates swarm fills in per task.
//
// It carries two escalations the agent refuses without its owner's opt-in: the host network and the
// added capabilities (D-19).
func KeepalivedServiceSpec(in KeepalivedSpecInput) swarm.ServiceSpec {
	grace := KeepalivedStopGrace
	restartDelay := KeepalivedRestartDelay
	env := []string{
		ingressbundle.EnvVIPAddress + "=" + in.Address.String(),
		ingressbundle.EnvVIPRouterID + "=" + strconv.Itoa(in.RouterID),
		ingressbundle.EnvVIPProbePort + "=" + strconv.Itoa(ingressbundle.ReadyPublishedPort),
		ingressbundle.EnvVIPKeyFile + "=" + KeepalivedKeyFile,
		// Swarm fills the templates in with the ID and the host name of the node the task runs on.
		ingressbundle.EnvVIPNodeID + "={{.Node.ID}}",
		ingressbundle.EnvVIPNodeName + "={{.Node.Hostname}}",
	}
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: ingress.ServiceNameKeepalived, Labels: ingress.KeepalivedLabels()},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: in.Image,
				// Service labels do not reach the task's container, so it carries its own copy.
				Labels:          ingress.KeepalivedLabels(),
				Env:             env,
				CapabilityDrop:  []string{"ALL"},
				CapabilityAdd:   KeepalivedCaps(),
				ReadOnly:        true,
				StopGracePeriod: &grace,
				Mounts: []mount.Mount{
					{Type: mount.TypeTmpfs, Target: "/run"},
					{Type: mount.TypeTmpfs, Target: "/tmp"},
				},
				Secrets: []*swarm.SecretReference{{
					File:       &swarm.SecretReferenceFileTarget{Name: KeepalivedSecretTarget, UID: "0", GID: "0", Mode: os.FileMode(0o400)},
					SecretName: in.SecretName,
				}},
			},
			Networks: []swarm.NetworkAttachmentConfig{{Target: "host"}},
			LogDriver: &swarm.Driver{
				Name:    "json-file",
				Options: map[string]string{"max-size": keepalivedLogMaxSize, "max-file": keepalivedLogMaxFile},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, Delay: &restartDelay},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			Delay:         KeepalivedUpdateDelay,
			Monitor:       KeepalivedUpdateMonitor,
			FailureAction: swarm.UpdateFailureActionRollback,
			Order:         swarm.UpdateOrderStopFirst,
		},
		RollbackConfig: &swarm.UpdateConfig{
			Parallelism: 1,
			Monitor:     KeepalivedUpdateMonitor,
			Order:       swarm.UpdateOrderStopFirst,
		},
	}
}
