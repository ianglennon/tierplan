// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/tunnel"
)

// The names an installed agent identifies itself by. The self-update and uninstall
// paths (D-26) find the agent through them, so they are part of what an install
// command promises and do not change once agents are in the field (D-21).
const (
	// ImageRepository is the agent image without a tag (D-16).
	ImageRepository = "ghcr.io/ianglennon/tierplan-agent"
	// ServiceName is the swarm service and the standalone container.
	ServiceName = "tierplan-agent"
	// VolumeName is the named volume that keeps the agent's key, certificate and pins.
	VolumeName = "tierplan-agent"
	// SecretPrefix starts the name of every generation of the join token secret.
	SecretPrefix = "tierplan-agent-token-g"

	stateDir      = "/var/lib/tierplan-agent"
	dockerSock    = "/var/run/docker.sock"
	secretTarget  = "tierplan-agent-token"
	tokenFilePath = "/run/secrets/" + secretTarget
	labelPair     = "tierplan.agent=true"

	// hostRouteSrc and hostRouteDst are the one host file every install mounts, read-only: the
	// host's IPv4 route table, which the agent reads for the subnet picker's host-route check
	// (D-17). It is one file because mounting /proc/1/net fails (a bind mount of a directory
	// under /proc is refused) and mounting /proc would show the agent every process of the host
	// (RESEARCH F7). hostRouteDst is agent/internal/hostnet.DefaultRouteFile.
	hostRouteSrc = "/proc/1/net/route"
	hostRouteDst = "/host/route"
)

// InstallParams are the values an install is built from.
type InstallParams struct {
	// Token is the join token, shown once (D-02).
	Token string
	// Endpoint is the address the agent dials: wss:// (ws:// for plain http), the
	// agent or public URL with its path, and /agent/connect (D-18).
	Endpoint string
	// Pin is the SPKI pin of TierPlan's own self-signed certificate. Empty when
	// TierPlan serves a supplied certificate or plain HTTP, and then no command
	// carries one.
	Pin string
	// ServerVersion is the running server's version; it picks the image tag (D-17).
	ServerVersion string
	// Generation numbers the token (env_join_tokens.generation, from 1). It is in
	// the name of the swarm secret so every rotation creates a fresh name.
	Generation int
	// AllowInsecure is the server's own TIERPLAN_AGENT_ALLOW_INSECURE (development
	// only). With it on and a ws:// Endpoint, every install text also sets
	// TIERPLAN_AGENT_ALLOW_INSECURE=true on the agent, which otherwise refuses to dial
	// plain ws:// to another host (T-02-31). Nothing else may set it, and it never
	// appears for a wss:// Endpoint. With it off, a ws:// Endpoint for a host that is
	// not this machine is one agents refuse, so no install, manual update command or
	// hint is built for it (ErrPlainEndpointRefused); a ws:// Endpoint on this machine
	// needs no switch.
	AllowInsecure bool
}

// agentSwitch reports whether the commands built from p must carry the agent's
// TIERPLAN_AGENT_ALLOW_INSECURE: the server's switch is on and the address is plain.
func (p InstallParams) agentSwitch() bool {
	return p.AllowInsecure && strings.HasPrefix(p.Endpoint, "ws://")
}

// Install is everything an admin copies to connect hosts to an agent environment.
// It is built in one place, BuildInstall, so no other code assembles a command a
// person runs as root.
type Install struct {
	JoinToken  string
	AgentURL   string
	Image      string
	Pin        string
	SecretName string
	Generation int

	// SwarmCommand creates the secret and the global service; run it on any manager.
	SwarmCommand string
	// SwarmStackFile is the same service as a stack file. A stack file cannot create
	// a secret from text, so it names the secret as external and its first comment
	// line holds the one-line command that creates it.
	SwarmStackFile string
	// StandaloneCommand runs the agent on one host.
	StandaloneCommand string
	// StandaloneComposeFile is the same container as a compose file.
	StandaloneComposeFile string
	// SwarmRotateCommand swaps the running service over to this generation's secret.
	// Empty for generation 1, which has nothing to replace.
	SwarmRotateCommand string
	// SwarmCleanup and StandaloneCleanup remove what the install created, for a host
	// that is offline when its environment is removed or when the admin does not let
	// TierPlan uninstall the agents (D-30).
	SwarmCleanup      string
	StandaloneCleanup string
}

// ErrInstallValue is returned by BuildInstall for a value that could not be put
// safely inside a shell command or a compose file.
var ErrInstallValue = errors.New("a value for the install command is not safe to quote")

// releaseVersion matches vMAJOR.MINOR.PATCH with an optional prerelease and build
// part, with or without the v. The tag it yields is made of characters a Docker
// tag allows and a shell leaves alone, so it needs no quoting.
var releaseVersion = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?)(?:\+[0-9A-Za-z.-]+)?$`)

// AgentImage is the agent image a server of serverVersion installs: the exact
// X.Y.Z, so the agent is always one the server was built against (D-17). A
// development or edge server has no release tag to match and installs edge
// (RESEARCH Pitfall 23). Build metadata after a + is not part of a Docker tag.
func AgentImage(serverVersion string) string {
	if m := releaseVersion.FindStringSubmatch(strings.TrimSpace(serverVersion)); m != nil {
		return ImageRepository + ":" + m[1]
	}
	return ImageRepository + ":edge"
}

// SecretName is the Docker secret that holds generation g of the join token.
func SecretName(g int) string { return SecretPrefix + strconv.Itoa(g) }

// ErrPlainEndpointRefused is returned by BuildInstall for an endpoint that is plain
// ws:// for a host that is not this machine while the server's switch is off: agents
// refuse such an address at start (plan 02-41), so no text may point them at it
// (G-02-R4-3). The API checks first and answers 409 insecure_agent_url, so reaching
// this error from there is a server bug.
var ErrPlainEndpointRefused = errors.New("the agent endpoint is plain ws:// for a host that is not this machine, which agents refuse")

// BuildInstall builds every install text for one join token. It refuses a token,
// endpoint or pin that holds a quote, a control character or a space: they are
// generated or normalised values and never contain one, so this is only a guard
// against a shell or YAML injection if that ever changes (T-02-36). It also refuses
// an endpoint agents would not dial (ErrPlainEndpointRefused).
func BuildInstall(p InstallParams) (Install, error) {
	if p.Generation < 1 {
		return Install{}, fmt.Errorf("%w: the token generation starts at 1", ErrInstallValue)
	}
	if p.Token == "" || p.Endpoint == "" {
		return Install{}, fmt.Errorf("%w: a join token and an endpoint are required", ErrInstallValue)
	}
	for name, v := range map[string]string{"token": p.Token, "endpoint": p.Endpoint, "pin": p.Pin} {
		if !safeValue(v) {
			return Install{}, fmt.Errorf("%w: the %s", ErrInstallValue, name)
		}
	}
	if tunnel.PlainAddressRefused(p.Endpoint, p.AllowInsecure) {
		return Install{}, ErrPlainEndpointRefused
	}

	image := AgentImage(p.ServerVersion)
	secret := SecretName(p.Generation)
	in := Install{
		JoinToken: p.Token, AgentURL: p.Endpoint, Image: image, Pin: p.Pin, SecretName: secret, Generation: p.Generation,
	}

	create := " printf %s " + sq(p.Token) + " | docker secret create " + secret + " -"
	secretFlag := "source=" + secret + ",target=" + secretTarget + ",mode=0400"

	in.SwarmCommand = create + " && docker service create --name " + ServiceName +
		" --mode global --constraint node.platform.os==linux" +
		" --secret " + secretFlag +
		" --mount type=bind,src=" + dockerSock + ",dst=" + dockerSock +
		" --mount type=volume,src=" + VolumeName + ",dst=" + stateDir +
		" --mount type=bind,src=" + hostRouteSrc + ",dst=" + hostRouteDst + ",ro" +
		" --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID" +
		" --update-parallelism 1 --update-delay 10s" +
		" --label " + labelPair + " --container-label " + labelPair +
		envFlag("--env", "TIERPLAN_AGENT_URL", p.Endpoint) +
		insecureFlag("--env", p) +
		optionalEnvFlag("--env", "TIERPLAN_AGENT_PIN", p.Pin) +
		envFlag("--env", "TIERPLAN_AGENT_JOIN_TOKEN_FILE", tokenFilePath) +
		" " + image

	in.StandaloneCommand = " docker run -d --name " + ServiceName + " --restart unless-stopped" +
		hardening() +
		" -v " + dockerSock + ":" + dockerSock + " -v " + VolumeName + ":" + stateDir +
		" -v " + hostRouteSrc + ":" + hostRouteDst + ":ro" +
		" --label " + labelPair +
		envFlag("-e", "TIERPLAN_AGENT_URL", p.Endpoint) +
		insecureFlag("-e", p) +
		optionalEnvFlag("-e", "TIERPLAN_AGENT_PIN", p.Pin) +
		envFlag("-e", "TIERPLAN_AGENT_JOIN_TOKEN", p.Token) +
		" " + image

	if p.Generation > 1 {
		in.SwarmRotateCommand = ` OLD=$(docker service inspect -f '{{(index .Spec.TaskTemplate.ContainerSpec.Secrets 0).SecretName}}' ` + ServiceName + `)` +
			" && printf %s " + sq(p.Token) + " | docker secret create " + secret + " -" +
			` && docker service update --secret-rm "$OLD" --secret-add ` + secretFlag + " " + ServiceName +
			` && docker secret rm "$OLD"`
	}

	in.SwarmStackFile = swarmStack(p, image, secret)
	in.StandaloneComposeFile = standaloneCompose(p, image)

	in.SwarmCleanup, in.StandaloneCleanup = CleanupCommands(p.Generation)
	return in, nil
}

// CleanupCommands are the commands that take the agent off a host by hand, for hosts
// whose agent TierPlan could not uninstall: offline, refusing self-update, or no answer
// (D-30). generation is the join token's current generation, which names the swarm
// secret. They carry no join token.
func CleanupCommands(generation int) (swarm, standalone string) {
	swarm = " docker service rm " + ServiceName + " && docker secret rm " + SecretName(generation) +
		"\n docker volume rm " + VolumeName + "   # on every node that ran the agent"
	standalone = " docker rm -f " + ServiceName + " && docker volume rm " + VolumeName
	return swarm, standalone
}

// ManualUpdateCommand is the command an operator runs to bring an agent of the
// given environment type ("swarm" or "standalone") to image. It is what a
// version_incompatible refusal shows (D-25). A swarm updates its service in
// place and keeps its environment, so it needs no switch. A standalone host
// recreates the container with the same flags, including the agent's
// TIERPLAN_AGENT_ALLOW_INSECURE when the install had it (a development server and a
// plain address, T-02-31), so a recreated dev agent keeps dialing; its state
// volume already holds the agent's identity, so no join token goes in. An
// unknown type, a value that could escape its quotes, or a standalone endpoint
// agents refuse (plain ws:// for another host without the server's switch, see
// ErrPlainEndpointRefused) answers "".
func ManualUpdateCommand(envType, image string, p InstallParams) string {
	if !safeValue(image) || !safeValue(p.Endpoint) || !safeValue(p.Pin) || strings.ContainsAny(image, "'\"$`\\ ") {
		return ""
	}
	switch envType {
	case "swarm":
		return " docker service update --image " + image + " " + ServiceName
	case "standalone":
		if p.Endpoint == "" {
			return "" // a recreated container with no address to dial would never connect
		}
		if tunnel.PlainAddressRefused(p.Endpoint, p.AllowInsecure) {
			return "" // the recreated agent would refuse the address and exit at start
		}
		return " docker pull " + image + " && docker rm -f " + ServiceName +
			" && docker run -d --name " + ServiceName + " --restart unless-stopped" +
			hardening() +
			" -v " + dockerSock + ":" + dockerSock + " -v " + VolumeName + ":" + stateDir +
			" -v " + hostRouteSrc + ":" + hostRouteDst + ":ro" +
			" --label " + labelPair +
			envFlag("-e", "TIERPLAN_AGENT_URL", p.Endpoint) +
			insecureFlag("-e", p) +
			optionalEnvFlag("-e", "TIERPLAN_AGENT_PIN", p.Pin) +
			" " + image
	}
	return ""
}

// HostRouteMountCommand is the command that gives an agent installed before the host-route check
// the one mount it needs (hostRouteSrc read-only), for the picker to show (D-17). An agent
// cannot get a mount by self-update (RESEARCH F7), so an admin runs this: a swarm adds the mount
// to the service, which rolls through the nodes one at a time with the install's update delay;
// a standalone host recreates the container, which is the manual update command and carries the
// mount. An unknown type, or a standalone host whose command cannot be built, answers "".
func HostRouteMountCommand(envType string, p InstallParams) string {
	switch envType {
	case "swarm":
		return " docker service update --mount-add type=bind,src=" + hostRouteSrc + ",dst=" + hostRouteDst + ",ro " + ServiceName
	case "standalone":
		return ManualUpdateCommand("standalone", AgentImage(p.ServerVersion), p)
	}
	return ""
}

// UpdateHint is the sentence a version_incompatible refusal appends for an agent
// that is too old: the manual command for the agent's environment type (D-25). An
// agent that is too new needs the server updated, which no agent command fixes,
// and an unknown type has no command, so both answer "", as does a standalone
// agent whose address agents refuse (it follows ManualUpdateCommand). It carries no join
// token: the refused agent either has its identity already or enrols again with
// the token it was installed with.
func UpdateHint(tooOld bool, envType string, p InstallParams) string {
	if !tooOld {
		return ""
	}
	cmd := ManualUpdateCommand(envType, AgentImage(p.ServerVersion), p)
	if cmd == "" {
		return ""
	}
	return "update the agent with: " + strings.TrimSpace(cmd)
}

// hardening is the flag set a standalone container shares with the swarm service
// (D-22): a read-only root filesystem, every capability dropped but the two the
// privilege drop needs, and no new privileges. No flag publishes a port or joins
// the host network (ENV-03, D-21).
func hardening() string {
	return " --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID --security-opt no-new-privileges"
}

// safeValue reports whether v can sit inside single quotes and a double-quoted
// YAML string without help: no quote, backslash, control character or space.
func safeValue(v string) bool {
	for _, r := range v {
		if r == '\'' || r == '"' || r == '\\' || r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// sq single-quotes a value. safeValue has already refused any value holding a
// quote, so there is nothing to escape.
func sq(v string) string { return "'" + v + "'" }

func envFlag(flag, name, value string) string { return " " + flag + " " + sq(name+"="+value) }

// insecureFlag is the agent's development switch as a flag, or "" when p does
// not need it. Name and value are fixed literals, so nothing from input is quoted.
func insecureFlag(flag string, p InstallParams) string {
	if !p.agentSwitch() {
		return ""
	}
	return envFlag(flag, "TIERPLAN_AGENT_ALLOW_INSECURE", "true")
}

// insecureYAML is the same switch as a line of a compose environment block.
func insecureYAML(p InstallParams) string {
	if !p.agentSwitch() {
		return ""
	}
	return "      TIERPLAN_AGENT_ALLOW_INSECURE: " + yamlString("true") + "\n"
}

func optionalEnvFlag(flag, name, value string) string {
	if value == "" {
		return ""
	}
	return envFlag(flag, name, value)
}

// yamlString is v as a double-quoted YAML scalar. Compose and stack files
// interpolate $, so it is doubled.
func yamlString(v string) string {
	b, err := json.Marshal(strings.ReplaceAll(v, "$", "$$"))
	if err != nil { // a string always marshals
		return `""`
	}
	return string(b)
}

func swarmStack(p InstallParams, image, secret string) string {
	var b strings.Builder
	b.WriteString("# Create the secret first:  printf %s " + sq(p.Token) + " | docker secret create " + secret + " -\n")
	b.WriteString("# Then deploy this file:  docker stack deploy -c tierplan-agent.yml tierplan\n")
	b.WriteString("services:\n")
	b.WriteString("  " + ServiceName + ":\n")
	b.WriteString("    image: " + image + "\n")
	b.WriteString("    read_only: true\n")
	b.WriteString("    cap_drop:\n      - ALL\n")
	b.WriteString("    cap_add:\n      - SETUID\n      - SETGID\n")
	b.WriteString("    labels:\n      tierplan.agent: \"true\"\n")
	b.WriteString("    environment:\n")
	b.WriteString("      TIERPLAN_AGENT_URL: " + yamlString(p.Endpoint) + "\n")
	b.WriteString(insecureYAML(p))
	if p.Pin != "" {
		b.WriteString("      TIERPLAN_AGENT_PIN: " + yamlString(p.Pin) + "\n")
	}
	b.WriteString("      TIERPLAN_AGENT_JOIN_TOKEN_FILE: " + tokenFilePath + "\n")
	b.WriteString("    volumes:\n")
	b.WriteString("      - " + dockerSock + ":" + dockerSock + "\n")
	b.WriteString("      - " + VolumeName + ":" + stateDir + "\n")
	b.WriteString("      - type: bind\n        source: " + hostRouteSrc + "\n        target: " + hostRouteDst + "\n        read_only: true\n")
	b.WriteString("    secrets:\n")
	b.WriteString("      - source: " + secret + "\n        target: " + secretTarget + "\n        mode: 0400\n")
	b.WriteString("    deploy:\n")
	b.WriteString("      mode: global\n")
	b.WriteString("      placement:\n        constraints:\n          - node.platform.os == linux\n")
	b.WriteString("      update_config:\n        parallelism: 1\n        delay: 10s\n")
	b.WriteString("      labels:\n        tierplan.agent: \"true\"\n")
	b.WriteString("volumes:\n  " + VolumeName + ":\n    name: " + VolumeName + "\n")
	b.WriteString("secrets:\n  " + secret + ":\n    external: true\n")
	return b.String()
}

func standaloneCompose(p InstallParams, image string) string {
	var b strings.Builder
	b.WriteString("# Start it with:  docker compose -f tierplan-agent.yml up -d\n")
	b.WriteString("services:\n")
	b.WriteString("  " + ServiceName + ":\n")
	b.WriteString("    image: " + image + "\n")
	b.WriteString("    container_name: " + ServiceName + "\n")
	b.WriteString("    restart: unless-stopped\n")
	b.WriteString("    read_only: true\n")
	b.WriteString("    cap_drop:\n      - ALL\n")
	b.WriteString("    cap_add:\n      - SETUID\n      - SETGID\n")
	b.WriteString("    security_opt:\n      - no-new-privileges:true\n")
	b.WriteString("    labels:\n      tierplan.agent: \"true\"\n")
	b.WriteString("    environment:\n")
	b.WriteString("      TIERPLAN_AGENT_URL: " + yamlString(p.Endpoint) + "\n")
	b.WriteString(insecureYAML(p))
	if p.Pin != "" {
		b.WriteString("      TIERPLAN_AGENT_PIN: " + yamlString(p.Pin) + "\n")
	}
	b.WriteString("      TIERPLAN_AGENT_JOIN_TOKEN: " + yamlString(p.Token) + "\n")
	b.WriteString("    volumes:\n")
	b.WriteString("      - " + dockerSock + ":" + dockerSock + "\n")
	b.WriteString("      - " + VolumeName + ":" + stateDir + "\n")
	b.WriteString("      - " + hostRouteSrc + ":" + hostRouteDst + ":ro\n")
	b.WriteString("volumes:\n  " + VolumeName + ":\n    name: " + VolumeName + "\n")
	return b.String()
}
