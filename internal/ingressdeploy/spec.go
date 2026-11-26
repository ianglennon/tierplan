// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The values of the proxy service. The container listens on unprivileged ports (D-06) and the service
// publishes them on 80 and 443 in host mode, so the proxy sees the client's own address (D-08), and
// publishes its readiness endpoint on a third host port (D-13).
const (
	// ImageRepository is the proxy image without a tag; goreleaser publishes it.
	ImageRepository = "ghcr.io/ianglennon/tierplan-ingress"
	// ProxyUser is the user and group the proxy runs as. The image's state directory and the join token
	// secret are owned by it.
	ProxyUser = "1000:1000"
	// StateDir is where the proxy keeps its certificate cache; the cache volume mounts here.
	StateDir = "/var/lib/tierplan-ingress"
	// SecretTarget is the file name the join token secret has inside the container.
	SecretTarget = "tierplan-ingress-token" //nolint:gosec // G101: a file name, not a credential
	// TokenFile is the path the proxy reads its join token from.
	TokenFile = "/run/secrets/" + SecretTarget //nolint:gosec // G101: a path, not a credential

	// HTTPPublished and HTTPTarget are the published and the container port of plain HTTP.
	HTTPPublished = 80
	HTTPTarget    = 8080
	// HTTPSPublished and HTTPSTarget are the published and the container port of HTTPS. The target
	// ports differ on purpose: published ports that share one target collapse into one mapping (S2).
	HTTPSPublished = 443
	HTTPSTarget    = 8443
	// ReadyPublished and ReadyTarget are the published and the container port of the proxy's readiness
	// endpoint (D-13, A-06). The port is published always, whether or not a virtual IP is on, so
	// switching the virtual IP on never rolls the proxies. It is visible on the LAN; the endpoint
	// answers a bare status code and nothing else. The three target ports 8080, 8443 and 8083 are
	// distinct (S2).
	ReadyPublished = ingressbundle.ReadyPublishedPort
	ReadyTarget    = ingressbundle.ReadyPort

	// UpdateMonitor is how long Swarm watches a replaced task before it counts the update as done. It
	// is longer than the health check interval, so an update whose new task fails its first probes
	// rolls back (D-47).
	UpdateMonitor = 60 * time.Second

	// LogMaxSize and LogMaxFile bound the proxy's json-file log on each node to 30 MB (D-33).
	LogMaxSize = "10m"
	LogMaxFile = "3"

	// NetworkPrefix is the prefix length of the ingress network's subnet: a /22.
	NetworkPrefix = 22

	// Attachable is whether the ingress overlay lets a plain container attach. S5 found that a service
	// spec attaches to a non-attachable overlay and that a plain `docker run --network` is refused, so
	// it stays false (24-S5-RESULT.md, D-05). The refusal is not a hard barrier: see that file.
	Attachable = false
)

// accessLogOn is the value of the access-log variable that switches the access log on.
const accessLogOn = "1"

// IngressImage is the proxy image a server of serverVersion installs: the exact X.Y.Z for a release,
// edge otherwise, by the same rule as the agent image.
func IngressImage(serverVersion string) string {
	return agents.ImageFor(ImageRepository, serverVersion)
}

// tokenTagLen is how many trailing letters and digits of the token ID go in a secret name.
const tokenTagLen = 8

// SecretName is the swarm secret that holds the join token the server issued as tokenID for generation
// gen of environment envID: tierplan-ingress-token-g<gen>-<envtag>-<tokentag>. The token part makes the
// name unique per issued token, so a restored install never collides with a secret of the same
// generation that an earlier install left on the swarm. At most 64 characters, and in the form the
// agent's secret names have.
func SecretName(envID string, generation int, tokenID string) string {
	return ingress.SecretPrefix + "g" + strconv.Itoa(generation) + "-" + agents.SecretTag(envID) + "-" + tail(tokenID, tokenTagLen)
}

// tail is the last n of the lower-case letters and digits of s.
func tail(s string, n int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	// Every kept rune is one ASCII byte, so cutting by bytes cuts by characters.
	t := b.String()
	return t[max(0, len(t)-n):]
}

// SpecInput is what the proxy service spec is built from. None of it is the join token: the token is
// a swarm secret that the spec only refers to.
type SpecInput struct {
	// Image is the proxy image with its tag (IngressImage).
	Image string
	// URL is the full address of the server's ingress channel, wss://host:port/ingress/connect.
	URL string
	// Pin is the pin of the server's certificate; empty when the server serves a certificate the
	// system trusts.
	Pin string
	// SecretName is the name of the swarm secret that holds the join token (SecretName). The caller
	// fills in SecretID in the returned spec from the secret it created.
	SecretName string
	// AccessLog switches Traefik's access log on (D-31).
	AccessLog bool
}

// ProxyServiceSpec is the global service that runs the proxy on every node: a user without
// privileges, no socket, no capability and no bind mount, host-mode ports, one cache volume, the join
// token as a secret file, and an update that stops the old task first and rolls back when the new one
// does not come up healthy. The result is the same for every node (S6) and equals the spec every
// validator was shown (S4).
func ProxyServiceSpec(in SpecInput) swarm.ServiceSpec {
	grace := ingressbundle.StopGracePeriod
	env := []string{
		ingressbundle.EnvURL + "=" + in.URL,
		ingressbundle.EnvPin + "=" + in.Pin,
		ingressbundle.EnvJoinTokenFile + "=" + TokenFile,
		// Swarm fills the template in with the ID of the node the task runs on.
		ingressbundle.EnvNodeID + "={{.Node.ID}}",
	}
	if in.AccessLog {
		env = append(env, ingressbundle.EnvAccessLog+"="+accessLogOn)
	}
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: ingress.ServiceName, Labels: ingress.ObjectLabels()},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: in.Image,
				User:  ProxyUser,
				// Service labels do not reach the task's container, so it carries its own copy.
				Labels:          ingress.ObjectLabels(),
				Env:             env,
				StopGracePeriod: &grace,
				Healthcheck: &container.HealthConfig{
					Test:        []string{"CMD", "/usr/local/bin/tierplan-ingress", "healthcheck"},
					Interval:    ingressbundle.HealthcheckInterval,
					Timeout:     ingressbundle.HealthcheckTimeout,
					Retries:     ingressbundle.HealthcheckRetries,
					StartPeriod: ingressbundle.HealthcheckStartPeriod,
				},
				Secrets: []*swarm.SecretReference{{
					File:       &swarm.SecretReferenceFileTarget{Name: SecretTarget, UID: "1000", GID: "1000", Mode: os.FileMode(0o400)},
					SecretName: in.SecretName,
				}},
				Mounts: []mount.Mount{{
					Type:          mount.TypeVolume,
					Source:        ingress.VolumeName,
					Target:        StateDir,
					VolumeOptions: &mount.VolumeOptions{Labels: ingress.ObjectLabels()},
				}},
			},
			Networks: []swarm.NetworkAttachmentConfig{{Target: ingress.NetworkName}},
			LogDriver: &swarm.Driver{
				Name:    "json-file",
				Options: map[string]string{"max-size": LogMaxSize, "max-file": LogMaxFile},
			},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{Protocol: "tcp", TargetPort: HTTPTarget, PublishedPort: HTTPPublished, PublishMode: swarm.PortConfigPublishModeHost},
			{Protocol: "tcp", TargetPort: HTTPSTarget, PublishedPort: HTTPSPublished, PublishMode: swarm.PortConfigPublishModeHost},
			{Protocol: "tcp", TargetPort: ReadyTarget, PublishedPort: ReadyPublished, PublishMode: swarm.PortConfigPublishModeHost},
		}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: swarm.UpdateFailureActionRollback,
			Order:         swarm.UpdateOrderStopFirst,
			Monitor:       UpdateMonitor,
		},
		RollbackConfig: &swarm.UpdateConfig{
			Parallelism: 1,
			Order:       swarm.UpdateOrderStopFirst,
			Monitor:     UpdateMonitor,
		},
	}
}

// NetworkSpec is the ingress overlay network on the given subnet: a swarm-scoped overlay that is not
// attachable (Attachable), carrying the object labels.
func NetworkSpec(subnet string) dockerx.NetworkCreateSpec {
	return dockerx.NetworkCreateSpec{
		Name:       ingress.NetworkName,
		Driver:     "overlay",
		Scope:      "swarm",
		Attachable: Attachable,
		Labels:     ingress.ObjectLabels(),
		Subnet:     subnet,
	}
}

// SecretSpec is the swarm secret that carries the join token, with the object labels. The token
// crosses TierPlan here, on its way to the manager, and is not kept.
func SecretSpec(name string, token []byte) dockerx.SecretSpec {
	return dockerx.SecretSpec{Name: name, Labels: ingress.ObjectLabels(), Data: token}
}
