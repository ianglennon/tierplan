// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"strconv"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// StandaloneNetworkPrefix is the prefix length of the standalone proxy bridge's subnet: a /24. A bridge on one
// host needs far fewer addresses than the swarm overlay (NetworkPrefix is a /22), and a smaller block leaves
// more of the host's address space free (discretionary, assumption A3).
const StandaloneNetworkPrefix = 24

// TokenLabel is the custom label that names the ID of the join token a standalone proxy container was created
// with. It is a custom label (types.ServiceConfig.CustomLabels), so it is outside the Compose SDK's config
// hash: a token that rolls over does not by itself make the SDK recreate the container (D-26).
const TokenLabel = "tierplan.ingress.token" //nolint:gosec // G101: a label name, not a credential

// The keys of the project's own network, volume and secret, and the name of the environment variable that
// carries the secret's clear text. The Docker names of the network and the volume are explicit (ingress.NetworkName,
// ingress.VolumeName), so these keys never reach the engine.
const (
	standaloneNetworkKey = "ingress"
	// standaloneSelfNetworkKey is the project's key of TierPlan's self network, which the project names as external: the
	// ensure loop makes it and removes it, never the Compose SDK.
	standaloneSelfNetworkKey = "self"
	standaloneVolumeKey  = "cache"
	standaloneSecretKey  = "token"
	standaloneSecretEnv  = "TIERPLAN_SECRET_token" //nolint:gosec // G101: the name of a variable, not a credential
)

// StandaloneToken is the join token a standalone proxy is created with.
type StandaloneToken struct {
	// ID is the token's ID in the server's token table; it goes in the TokenLabel.
	ID string
	// Value is the clear text of the token. It goes only into project.Environment, from which the Compose
	// SDK reads it into the container's secret file, and is never logged.
	Value string
}

// StandaloneInput is what the standalone proxy project is built from.
type StandaloneInput struct {
	// Image is the proxy image with its tag (IngressImage).
	Image string
	// URL is the full address of the server's ingress channel, wss://host:port/ingress/connect.
	URL string
	// Pin is the pin of the server's certificate; empty when the server serves a certificate the system trusts.
	Pin string
	// AccessLog switches Traefik's access log on (D-31).
	AccessLog bool
	// Subnet is the CIDR of the proxy bridge network; empty lets the engine choose one.
	Subnet string
	// Token is the join token; nil builds the project without a secret and without the token label.
	Token *StandaloneToken
	// SelfNetwork puts the proxy on TierPlan's self network as well (D-04), an external network of the project: the bridge
	// the ensure loop made, which TierPlan's own container is connected to.
	SelfNetwork bool
}

// StandaloneProject is the standalone host's system project (D-06, D-07): the compose project
// ingress.ProjectName with the one service ingress.ProxyComposeService, the bridge network ingress.NetworkName
// (and, when in.SelfNetwork, the external self network ingress.SelfNetworkName) and the cache volume
// ingress.VolumeName. It is built from typed compose-go values and never from text, so nothing a user wrote can
// reach it, and from the swarm builder's constants (ProxyUser, StateDir, TokenFile, the ports and the log bounds),
// so the two cannot drift.
//
// The container it makes is a plain one: no Docker socket, no privileged mode, no capability, no device, no
// host network, PID or IPC mode and no host path; the host ports 80 and 443 are the only host-facing setting
// (PROH-25-01). A test ties its create body to the vector every container policy forwards (D-03).
//
// Deviations from ProxyServiceSpec: the proxy is a single container on a bridge (not a global service on an
// overlay), so there is no node ID variable (D-07), no update or rollback policy, and the published ports are
// ordinary bindings; the join token is an environment-sourced compose secret and not a swarm secret (D-08,
// D-24), so no file of the server is ever bind-mounted.
func StandaloneProject(in StandaloneInput) *types.Project {
	grace := types.Duration(ingressbundle.StopGracePeriod)
	interval := types.Duration(ingressbundle.HealthcheckInterval)
	timeout := types.Duration(ingressbundle.HealthcheckTimeout)
	start := types.Duration(ingressbundle.HealthcheckStartPeriod)
	retries := uint64(ingressbundle.HealthcheckRetries)

	env := types.MappingWithEquals{
		ingressbundle.EnvURL:           strPtr(in.URL),
		ingressbundle.EnvPin:           strPtr(in.Pin),
		ingressbundle.EnvJoinTokenFile: strPtr(TokenFile),
	}
	if in.AccessLog {
		env[ingressbundle.EnvAccessLog] = strPtr(accessLogOn)
	}

	svc := types.ServiceConfig{
		Name:    ingress.ProxyComposeService,
		Image:   in.Image,
		User:    ProxyUser,
		Restart: "unless-stopped",
		// D-25: false. The SDK refuses a secret in a read-only service (A-2), and the image's state directory is a
		// volume already, so the proxy's own writes are confined without it.
		ReadOnly: false,
		Labels:   types.Labels(ingress.ObjectLabels()),
		Ports: []types.ServicePortConfig{
			{Target: HTTPTarget, Published: strconv.Itoa(HTTPPublished), Protocol: "tcp"},
			{Target: HTTPSTarget, Published: strconv.Itoa(HTTPSPublished), Protocol: "tcp"},
		},
		Networks:    map[string]*types.ServiceNetworkConfig{standaloneNetworkKey: {}},
		Volumes:     []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: standaloneVolumeKey, Target: StateDir}},
		Environment: env,
		HealthCheck: &types.HealthCheckConfig{
			Test:        types.HealthCheckTest{"CMD", "/usr/local/bin/tierplan-ingress", "healthcheck"},
			Interval:    &interval,
			Timeout:     &timeout,
			Retries:     &retries,
			StartPeriod: &start,
		},
		StopGracePeriod: &grace,
		// A project built from typed values has none of the labels compose-go's loader adds, and the SDK finds
		// the project's containers by them (remove, orphan cleanup, the one-off marker).
		CustomLabels: types.Labels{
			stacks.LabelComposeProject: ingress.ProjectName,
			stacks.LabelComposeService: ingress.ProxyComposeService,
			stacks.LabelComposeOneoff:  "False",
		},
		Logging: &types.LoggingConfig{
			Driver:  "json-file",
			Options: types.Options{"max-size": LogMaxSize, "max-file": LogMaxFile},
		},
	}

	network := types.NetworkConfig{
		Name:   ingress.NetworkName,
		Driver: "bridge",
		Labels: types.Labels(ingress.ObjectLabels()),
	}
	if in.Subnet != "" {
		network.Ipam = types.IPAMConfig{Config: []*types.IPAMPool{{Subnet: in.Subnet}}}
	}

	networks := types.Networks{standaloneNetworkKey: network}
	if in.SelfNetwork {
		svc.Networks[standaloneSelfNetworkKey] = &types.ServiceNetworkConfig{}
		networks[standaloneSelfNetworkKey] = types.NetworkConfig{Name: ingress.SelfNetworkName, External: true}
	}

	p := &types.Project{
		Name:     ingress.ProjectName,
		Services: types.Services{ingress.ProxyComposeService: svc},
		Networks: networks,
		Volumes: types.Volumes{standaloneVolumeKey: types.VolumeConfig{
			Name:   ingress.VolumeName,
			Labels: types.Labels(ingress.ObjectLabels()),
		}},
		Environment: types.Mapping{},
	}

	if in.Token != nil {
		// D-24: the secret's only source is an environment variable of the project, so the SDK reads the value
		// from project.Environment and puts it in the container as a file; there is no File (no server path is
		// bind-mounted) and no Content. D-08: no UID, GID or Mode; the container's user reads the file as the
		// SDK writes it.
		p.Secrets = types.Secrets{standaloneSecretKey: types.SecretConfig{Name: standaloneSecretKey, Environment: standaloneSecretEnv}}
		p.Environment[standaloneSecretEnv] = in.Token.Value
		svc.Secrets = []types.ServiceSecretConfig{{Source: standaloneSecretKey, Target: TokenFile}}
		svc.CustomLabels[TokenLabel] = in.Token.ID
		p.Services[ingress.ProxyComposeService] = svc
	}
	return p
}

func strPtr(s string) *string { return &s }
