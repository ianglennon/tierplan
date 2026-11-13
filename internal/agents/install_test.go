// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/agents"
)

// update rewrites the golden files: go test ./internal/agents/ -run InstallCommand -update-golden.
// Do it only when the change to the command text is intended; the golden files
// are what an admin pastes into a shell as root.
//
// The flag is not called -update: the Compose SDK links gotest.tools into the server (through
// docker/compose/v5/pkg/utils), and that package registers a -update flag of its own in every binary that
// holds it, so a second one here makes the test binary panic when it starts (plan 07-20).
var update = flag.Bool("update-golden", false, "rewrite the install golden files")

const (
	goldenToken    = "tpjt_E_S" //nolint:gosec // a made-up token for the golden files
	goldenEndpoint = "wss://example.com/tierplan/agent/connect"
	goldenPin      = "sha256/P"
	// goldenEnvID is a UUIDv7; its secret tag is the random tail, 4b1e6f0a8c37.
	goldenEnvID = "018f3c2e-7a41-7c3b-9d52-4b1e6f0a8c37"
	goldenTag   = "4b1e6f0a8c37"
)

func goldenParams() agents.InstallParams {
	return agents.InstallParams{
		Token: goldenToken, Endpoint: goldenEndpoint, Pin: goldenPin, ServerVersion: "v0.2.0", Generation: 1, EnvID: goldenEnvID,
	}
}

func mustBuild(t *testing.T, p agents.InstallParams) agents.Install {
	t.Helper()
	in, err := agents.BuildInstall(p)
	if err != nil {
		t.Fatalf("BuildInstall(%+v): %v", p, err)
	}
	return in
}

// checkGolden compares got with testdata/name byte for byte, or rewrites it with -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixed testdata path
	if err != nil {
		t.Fatalf("read the golden file (regenerate with -update if the change is intended): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the builder's output\n--- golden ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

func TestInstallCommandMatchesTheGoldenFiles(t *testing.T) {
	in := mustBuild(t, goldenParams())
	checkGolden(t, "swarm-command.golden", in.SwarmCommand)
	checkGolden(t, "standalone-command.golden", in.StandaloneCommand)
	checkGolden(t, "swarm-stack.golden.yml", in.SwarmStackFile)
	checkGolden(t, "standalone-compose.golden.yml", in.StandaloneComposeFile)
	checkGolden(t, "manual-cleanup.golden", "Swarm\n"+in.SwarmCleanup+"\n\nStandalone\n"+in.StandaloneCleanup+"\n")

	// A rotation hands the swarm a second generation of the secret: the golden
	// file is for generation 2, the one that replaces generation 1.
	p := goldenParams()
	p.Generation = 2
	checkGolden(t, "swarm-rotate-command.golden", mustBuild(t, p).SwarmRotateCommand)
}

func TestInstallCommandCarriesTheNamesTheAgentIdentifiesItselfBy(t *testing.T) {
	in := mustBuild(t, goldenParams())
	if in.JoinToken != goldenToken || in.AgentURL != goldenEndpoint || in.Pin != goldenPin || in.Generation != 1 {
		t.Fatalf("install = %+v, want the parameters echoed", in)
	}
	if in.SecretName != "tierplan-agent-token-g1-"+goldenTag {
		t.Errorf("SecretName = %q, want tierplan-agent-token-g1-%s", in.SecretName, goldenTag)
	}
	if in.Image != "ghcr.io/ianglennon/tierplan-agent:0.2.0" {
		t.Errorf("Image = %q, want the exact server version, never latest", in.Image)
	}

	for _, want := range []string{
		"docker service create", "--name tierplan-agent", "--mode global", "--constraint node.platform.os==linux",
		"--secret source=tierplan-agent-token-g1-" + goldenTag + ",target=tierplan-agent-token,mode=0400",
		"--mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
		"--mount type=volume,src=tierplan-agent,dst=/var/lib/tierplan-agent",
		"--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID",
		"--label tierplan.agent=true", "--container-label tierplan.agent=true",
		"TIERPLAN_AGENT_URL=" + goldenEndpoint, "TIERPLAN_AGENT_PIN=" + goldenPin,
		"TIERPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/tierplan-agent-token",
		"docker secret create tierplan-agent-token-g1-" + goldenTag + " -",
	} {
		if !strings.Contains(in.SwarmCommand, want) {
			t.Errorf("swarm command lacks %q:\n%s", want, in.SwarmCommand)
		}
	}
	if strings.Contains(in.SwarmCommand, "TIERPLAN_AGENT_JOIN_TOKEN=") {
		t.Errorf("the swarm command passes the token as an env var instead of the secret file:\n%s", in.SwarmCommand)
	}

	for _, want := range []string{
		"docker run -d", "--name tierplan-agent", "--restart unless-stopped", "--read-only", "--cap-drop ALL",
		"--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v /var/run/docker.sock:/var/run/docker.sock", "-v tierplan-agent:/var/lib/tierplan-agent",
		"--label tierplan.agent=true", "TIERPLAN_AGENT_URL=" + goldenEndpoint, "TIERPLAN_AGENT_PIN=" + goldenPin,
		"TIERPLAN_AGENT_JOIN_TOKEN=" + goldenToken,
	} {
		if !strings.Contains(in.StandaloneCommand, want) {
			t.Errorf("standalone command lacks %q:\n%s", want, in.StandaloneCommand)
		}
	}
	if strings.Contains(in.StandaloneCommand, "--network host") || strings.Contains(in.SwarmCommand, "--network host") {
		t.Error("an install command uses the host network")
	}
}

// TestInstallCommandMountsOnlyTheRouteFile pins the prohibition on the host-route mount (NET-05, T-10-21): every form
// of an install and the manual standalone update mount the one file /proc/1/net/route read-only at /host/route, and
// none mounts /proc, /proc/1/net or any other procfs path.
func TestInstallCommandMountsOnlyTheRouteFile(t *testing.T) {
	in := mustBuild(t, goldenParams())
	p := goldenParams()
	update := agents.ManualUpdateCommand("standalone", in.Image, p)

	want := map[string]string{
		"swarm command":       "--mount type=bind,src=/proc/1/net/route,dst=/host/route,ro",
		"standalone command":  "-v /proc/1/net/route:/host/route:ro",
		"swarm stack file":    "      - type: bind\n        source: /proc/1/net/route\n        target: /host/route\n        read_only: true\n",
		"standalone compose":  "      - /proc/1/net/route:/host/route:ro\n",
		"update (standalone)": "-v /proc/1/net/route:/host/route:ro",
	}
	texts := allInstallText(in)
	texts["update (standalone)"] = update
	for name, mount := range want {
		if got := strings.Count(texts[name], mount); got != 1 {
			t.Errorf("%s holds the route mount %d times, want once:\n%s", name, got, texts[name])
		}
	}
	// Any other mention of a procfs path in a source or target is a wider mount than the one file.
	procfs := regexp.MustCompile(`/proc[^ \n'"]*`)
	for name, text := range texts {
		for _, m := range procfs.FindAllString(text, -1) {
			if m != "/proc/1/net/route" && m != "/proc/1/net/route:/host/route:ro" && m != "/proc/1/net/route,dst=/host/route,ro" {
				t.Errorf("%s names the procfs path %q; only /proc/1/net/route may be mounted:\n%s", name, m, text)
			}
		}
		for _, bad := range []string{"src=/proc,", "source: /proc\n", " /proc:/", " -v /proc:", "/proc/1/net:", "/proc/net"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s mounts more of /proc than the route file (%q):\n%s", name, bad, text)
			}
		}
	}
	// The hardening is unchanged by the extra mount.
	for name, text := range map[string]string{"swarm command": in.SwarmCommand, "standalone command": in.StandaloneCommand} {
		for _, w := range []string{"--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID"} {
			if !strings.Contains(text, w) {
				t.Errorf("%s lost %q", name, w)
			}
		}
		if strings.Contains(text, "--network host") || strings.Contains(text, "--net host") || strings.Contains(text, "--privileged") {
			t.Errorf("%s widens the container's access:\n%s", name, text)
		}
	}
}

func TestHostRouteMountCommand(t *testing.T) {
	p := goldenParams()
	p.Token = "" // an agent that is already installed has its identity; the fix carries no join token

	const swarm = " docker service update --mount-add type=bind,src=/proc/1/net/route,dst=/host/route,ro tierplan-agent"
	if got := agents.HostRouteMountCommand("swarm", p); got != swarm {
		t.Errorf("swarm = %q, want %q", got, swarm)
	}

	got := agents.HostRouteMountCommand("standalone", p)
	for _, want := range []string{
		"-v /proc/1/net/route:/host/route:ro", "docker rm -f tierplan-agent", "--read-only", "--cap-drop ALL",
		"--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v tierplan-agent:/var/lib/tierplan-agent", "TIERPLAN_AGENT_URL=" + goldenEndpoint,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("standalone lacks %q:\n%s", want, got)
		}
	}
	if want := agents.ManualUpdateCommand("standalone", agents.AgentImage(p.ServerVersion), p); got != want {
		t.Errorf("standalone = %q, want the recreate the manual update gives (%q)", got, want)
	}
	if strings.Contains(got, "JOIN_TOKEN") || strings.Contains(got, "--network host") {
		t.Errorf("standalone carries a join token or the host network:\n%s", got)
	}
	for name, cmd := range map[string]string{"swarm": agents.HostRouteMountCommand("swarm", p), "standalone": got} {
		if !strings.HasPrefix(cmd, " ") || strings.HasPrefix(cmd, "  ") || strings.Contains(cmd, "\n") {
			t.Errorf("%s is not a one-liner with one leading space: %q", name, cmd)
		}
	}

	if got := agents.HostRouteMountCommand("unknown", p); got != "" {
		t.Errorf("an unknown type answered %q, want nothing", got)
	}
	if got := agents.HostRouteMountCommand("", p); got != "" {
		t.Errorf("no type answered %q, want nothing", got)
	}
	// A standalone host whose recreate cannot be built has no fix to show.
	bad := p
	bad.Endpoint = ""
	if got := agents.HostRouteMountCommand("standalone", bad); got != "" {
		t.Errorf("a standalone host with no endpoint answered %q, want nothing", got)
	}
}

func TestInstallCommandWithoutAPinNamesNone(t *testing.T) {
	p := goldenParams()
	p.Pin = ""
	in := mustBuild(t, p)
	for name, text := range allInstallText(in) {
		if strings.Contains(text, "TIERPLAN_AGENT_PIN") || strings.Contains(text, "sha256/") {
			t.Errorf("%s mentions a pin although there is none:\n%s", name, text)
		}
	}
	if in.Pin != "" {
		t.Errorf("Pin = %q, want empty", in.Pin)
	}
}

// allInstallText is every generated text an admin can copy, by name.
func allInstallText(in agents.Install) map[string]string {
	return map[string]string{
		"swarm command":      in.SwarmCommand,
		"standalone command": in.StandaloneCommand,
		"swarm stack file":   in.SwarmStackFile,
		"standalone compose": in.StandaloneComposeFile,
		"rotate command":     in.SwarmRotateCommand,
		"swarm cleanup":      in.SwarmCleanup,
		"standalone cleanup": in.StandaloneCleanup,
	}
}

func TestInstallCommandAgentImageTagFollowsTheServer(t *testing.T) {
	for server, want := range map[string]string{
		"v0.2.0":             "ghcr.io/ianglennon/tierplan-agent:0.2.0",
		"0.2.0":              "ghcr.io/ianglennon/tierplan-agent:0.2.0",
		"v0.2.0-rc.1":        "ghcr.io/ianglennon/tierplan-agent:0.2.0-rc.1",
		"v1.10.3":            "ghcr.io/ianglennon/tierplan-agent:1.10.3",
		"v0.2.0+build.5":     "ghcr.io/ianglennon/tierplan-agent:0.2.0",
		"dev (abc123)":       "ghcr.io/ianglennon/tierplan-agent:edge",
		"edge":               "ghcr.io/ianglennon/tierplan-agent:edge",
		"":                   "ghcr.io/ianglennon/tierplan-agent:edge",
		"v0.2":               "ghcr.io/ianglennon/tierplan-agent:edge",
		"v0.2.0; rm -rf /":   "ghcr.io/ianglennon/tierplan-agent:edge",
		"v0.2.0-rc.1 --x":    "ghcr.io/ianglennon/tierplan-agent:edge",
		"vv0.2.0":            "ghcr.io/ianglennon/tierplan-agent:edge",
		"v0.2.0-$(id)":       "ghcr.io/ianglennon/tierplan-agent:edge",
		"v0.2.0-rc.1\nextra": "ghcr.io/ianglennon/tierplan-agent:edge",
	} {
		if got := agents.AgentImage(server); got != want {
			t.Errorf("AgentImage(%q) = %q, want %q", server, got, want)
		}
	}
	if got := mustBuild(t, agents.InstallParams{Token: goldenToken, Endpoint: goldenEndpoint, ServerVersion: "dev (abc123)", Generation: 1, EnvID: goldenEnvID}).Image; !strings.HasSuffix(got, ":edge") {
		t.Errorf("a dev server's install image = %q, want the edge tag", got)
	}
}

func TestInstallCommandsAreOneLinersWithALeadingSpaceAndNoInboundPort(t *testing.T) {
	for label, p := range map[string]agents.InstallParams{"production": goldenParams()} {
		t.Run(label, func(t *testing.T) { checkOneLiners(t, p) })
	}
}

func checkOneLiners(t *testing.T, p agents.InstallParams) {
	t.Helper()
	p.Generation = 2
	in := mustBuild(t, p)
	oneLiners := map[string]string{
		"swarm command":       in.SwarmCommand,
		"standalone command":  in.StandaloneCommand,
		"rotate command":      in.SwarmRotateCommand,
		"update (swarm)":      agents.ManualUpdateCommand("swarm", in.Image, p),
		"update (standalone)": agents.ManualUpdateCommand("standalone", in.Image, p),
	}
	for name, cmd := range oneLiners {
		if !strings.HasPrefix(cmd, " ") || strings.HasPrefix(cmd, "  ") {
			t.Errorf("%s must start with exactly one space (HISTCONTROL=ignorespace): %q", name, cmd[:min(len(cmd), 12)])
		}
		if strings.Contains(cmd, "\n") {
			t.Errorf("%s is not a one-liner:\n%s", name, cmd)
		}
	}
	for name, text := range allInstallText(in) {
		for _, bad := range []string{" -p ", "--publish", "ports:", "-P ", "--publish-all", "published"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s publishes or requires a port (%q): ENV-03 forbids an inbound port\n%s", name, bad, text)
			}
		}
	}
	for name, text := range map[string]string{"update (swarm)": oneLiners["update (swarm)"], "update (standalone)": oneLiners["update (standalone)"]} {
		for _, bad := range []string{" -p ", "--publish", "ports:"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s publishes a port (%q)", name, bad)
			}
		}
	}
}

var singleQuoted = regexp.MustCompile(`'[^']*'`)

func TestInstallCommandKeepsTheTokenInsideSingleQuotesAndOutOfEveryURL(t *testing.T) {
	for label, p := range map[string]agents.InstallParams{"production": goldenParams()} {
		t.Run(label, func(t *testing.T) { checkTokenQuoting(t, p) })
	}
}

func checkTokenQuoting(t *testing.T, p agents.InstallParams) {
	t.Helper()
	p.Generation = 2
	in := mustBuild(t, p)

	for name, cmd := range map[string]string{"swarm command": in.SwarmCommand, "standalone command": in.StandaloneCommand, "rotate command": in.SwarmRotateCommand} {
		if !strings.Contains(cmd, "'") {
			t.Errorf("%s has no quoted value", name)
		}
		if outside := singleQuoted.ReplaceAllString(cmd, "''"); strings.Contains(outside, goldenToken) {
			t.Errorf("%s has the token outside single quotes:\n%s", name, cmd)
		}
	}
	for name, text := range allInstallText(in) {
		if strings.Contains(text, "?token=") || strings.Contains(text, "&token=") {
			t.Errorf("%s puts the token in a query string", name)
		}
		for _, after := range []string{"://" + goldenToken, "@" + goldenToken, "/" + goldenToken} {
			if strings.Contains(text, after) {
				t.Errorf("%s puts the token in a URL (%q)", name, after)
			}
		}
	}
	// Only the install texts that hold the secret may carry it; the cleanup text never does.
	for name, text := range map[string]string{
		"swarm cleanup": in.SwarmCleanup, "standalone cleanup": in.StandaloneCleanup,
		"update (swarm)":      agents.ManualUpdateCommand("swarm", in.Image, p),
		"update (standalone)": agents.ManualUpdateCommand("standalone", in.Image, p),
	} {
		if strings.Contains(text, goldenToken) {
			t.Errorf("%s carries the join token", name)
		}
	}
}

func TestInstallCommandStackFileKeepsTheTokenOutOfTheYAMLBody(t *testing.T) {
	in := mustBuild(t, goldenParams())

	first, rest, _ := strings.Cut(in.SwarmStackFile, "\n")
	want := "# Create the secret first:  printf %s '" + goldenToken + "' | docker secret create tierplan-agent-token-g1-" + goldenTag + " -"
	if first != want {
		t.Errorf("first line = %q, want the one-line docker secret create command %q", first, want)
	}
	if strings.Contains(rest, goldenToken) {
		t.Errorf("the stack file has the token below its first comment line:\n%s", rest)
	}
	if strings.Count(in.SwarmStackFile, "external: true") != 1 || !strings.Contains(in.SwarmStackFile, "tierplan-agent-token-g1-"+goldenTag+":") {
		t.Errorf("the stack file must declare the g1 secret as external exactly once:\n%s", in.SwarmStackFile)
	}
	for _, want := range []string{
		"mode: global", "node.platform.os == linux", "read_only: true", "- ALL", "- SETUID", "- SETGID",
		"tierplan.agent: \"true\"", "target: tierplan-agent-token", "mode: 0400",
		"TIERPLAN_AGENT_JOIN_TOKEN_FILE: /run/secrets/tierplan-agent-token", "/var/run/docker.sock:/var/run/docker.sock",
		"tierplan-agent:/var/lib/tierplan-agent",
	} {
		if !strings.Contains(in.SwarmStackFile, want) {
			t.Errorf("the stack file lacks %q:\n%s", want, in.SwarmStackFile)
		}
	}
	if strings.Count(in.SwarmStackFile, "tierplan.agent:") != 2 {
		t.Errorf("the stack file must label both the service (deploy.labels) and its containers (labels):\n%s", in.SwarmStackFile)
	}
}

func TestInstallCommandComposeFileCarriesTheSameEnvironmentAsTheCommand(t *testing.T) {
	in := mustBuild(t, goldenParams())
	for _, want := range []string{
		"restart: unless-stopped", "no-new-privileges:true", "read_only: true", "- SETUID", "- SETGID",
		"TIERPLAN_AGENT_URL: \"" + goldenEndpoint + "\"", "TIERPLAN_AGENT_PIN: \"" + goldenPin + "\"",
		"TIERPLAN_AGENT_JOIN_TOKEN: \"" + goldenToken + "\"",
		"/var/run/docker.sock:/var/run/docker.sock", "tierplan-agent:/var/lib/tierplan-agent", "tierplan.agent: \"true\"",
		"container_name: tierplan-agent",
	} {
		if !strings.Contains(in.StandaloneComposeFile, want) {
			t.Errorf("the compose file lacks %q:\n%s", want, in.StandaloneComposeFile)
		}
	}
	if strings.Contains(in.StandaloneComposeFile, "external: true") {
		t.Errorf("a standalone compose file has no secret to be external:\n%s", in.StandaloneComposeFile)
	}
}

func TestInstallCommandRotationSwapsTheSecretInPlace(t *testing.T) {
	p := goldenParams()
	p.Generation = 2
	in := mustBuild(t, p)
	if in.SecretName != "tierplan-agent-token-g2-"+goldenTag {
		t.Fatalf("SecretName = %q, want tierplan-agent-token-g2-%s", in.SecretName, goldenTag)
	}
	for _, want := range []string{
		`OLD=$(docker service inspect -f '{{(index .Spec.TaskTemplate.ContainerSpec.Secrets 0).SecretName}}' tierplan-agent)`,
		"docker secret create tierplan-agent-token-g2-" + goldenTag + " -",
		`docker service update --secret-rm "$OLD" --secret-add source=tierplan-agent-token-g2-` + goldenTag + `,target=tierplan-agent-token,mode=0400 tierplan-agent`,
		`docker secret rm "$OLD"`,
	} {
		if !strings.Contains(in.SwarmRotateCommand, want) {
			t.Errorf("the rotate command lacks %q:\n%s", want, in.SwarmRotateCommand)
		}
	}
	// The old secret is removed only after the service no longer uses it.
	if strings.Index(in.SwarmRotateCommand, "--secret-rm") > strings.Index(in.SwarmRotateCommand, `docker secret rm "$OLD"`) {
		t.Errorf("the old secret is removed before the service stops using it:\n%s", in.SwarmRotateCommand)
	}
	if g1 := mustBuild(t, goldenParams()); g1.SwarmRotateCommand != "" {
		t.Errorf("a first-generation install has nothing to rotate, got %q", g1.SwarmRotateCommand)
	}
}

func TestInstallCommandCleanupNamesWhatTheInstallCreated(t *testing.T) {
	in := mustBuild(t, goldenParams())
	for _, want := range []string{
		"docker service ls -q --filter label=tierplan.agent=true",
		`docker service inspect -f '{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{.SecretName}} {{end}}' "$v"`,
		`docker service rm "$v"`, `docker secret rm "$s"`, "docker volume rm tierplan-agent",
	} {
		if !strings.Contains(in.SwarmCleanup, want) {
			t.Errorf("the swarm cleanup lacks %q:\n%s", want, in.SwarmCleanup)
		}
	}
	for _, want := range []string{"docker rm -f tierplan-agent", "docker volume rm tierplan-agent"} {
		if !strings.Contains(in.StandaloneCleanup, want) {
			t.Errorf("the standalone cleanup lacks %q:\n%s", want, in.StandaloneCleanup)
		}
	}
}

func TestInstallCommandRefusesValuesThatCouldBreakOutOfTheQuotes(t *testing.T) {
	for name, mutate := range map[string]func(*agents.InstallParams){
		"token with a quote":                       func(p *agents.InstallParams) { p.Token = "tp'; touch /tmp/x; '" },
		"endpoint with a quote":                    func(p *agents.InstallParams) { p.Endpoint = "wss://x'y/agent/connect" },
		"pin with a quote":                         func(p *agents.InstallParams) { p.Pin = "sha256/a'b" },
		"token with a newline":                     func(p *agents.InstallParams) { p.Token = "tp\nrm" },
		"endpoint with newline":                    func(p *agents.InstallParams) { p.Endpoint = "wss://x/agent/connect\n" },
		"empty token":                              func(p *agents.InstallParams) { p.Token = "" },
		"empty endpoint":                           func(p *agents.InstallParams) { p.Endpoint = "" },
		"generation zero":                          func(p *agents.InstallParams) { p.Generation = 0 },
		"empty environment ID":                     func(p *agents.InstallParams) { p.EnvID = "" },
		"environment ID with no letters or digits": func(p *agents.InstallParams) { p.EnvID = "-_- ." },
	} {
		t.Run(name, func(t *testing.T) {
			p := goldenParams()
			mutate(&p)
			if in, err := agents.BuildInstall(p); err == nil {
				t.Fatalf("BuildInstall accepted %+v and built:\n%s", p, in.SwarmCommand)
			}
		})
	}
}

func TestInstallCommandEscapesDollarSignsInComposeFiles(t *testing.T) {
	p := goldenParams()
	p.Endpoint = "wss://example.com/a$b/agent/connect"
	in := mustBuild(t, p)
	if !strings.Contains(in.StandaloneComposeFile, "a$$b") || strings.Contains(in.StandaloneComposeFile, "a$b") {
		t.Errorf("compose would interpolate a $ in the URL:\n%s", in.StandaloneComposeFile)
	}
}

func TestManualUpdateCommandTargetsTheAgentOfTheEnvironmentType(t *testing.T) {
	p := goldenParams()
	image := agents.AgentImage(p.ServerVersion)

	if got, want := agents.ManualUpdateCommand("swarm", image, p), " docker service update --image ghcr.io/ianglennon/tierplan-agent:0.2.0 tierplan-agent"; got != want {
		t.Errorf("swarm update = %q, want %q", got, want)
	}

	got := agents.ManualUpdateCommand("standalone", image, p)
	for _, want := range []string{
		"docker pull ghcr.io/ianglennon/tierplan-agent:0.2.0", "docker rm -f tierplan-agent", "docker run -d --name tierplan-agent",
		"--restart unless-stopped", "--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v tierplan-agent:/var/lib/tierplan-agent", "TIERPLAN_AGENT_URL=" + goldenEndpoint, "TIERPLAN_AGENT_PIN=" + goldenPin,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("standalone update lacks %q:\n%s", want, got)
		}
	}
	// The state volume already holds the agent's identity, so no join token goes in.
	if strings.Contains(got, "JOIN_TOKEN") || strings.Contains(got, goldenToken) {
		t.Errorf("the standalone update carries a join token:\n%s", got)
	}
	if !strings.HasSuffix(got, " ghcr.io/ianglennon/tierplan-agent:0.2.0") {
		t.Errorf("the image must come last: %s", got)
	}

	if got := agents.ManualUpdateCommand("unknown", image, p); got != "" {
		t.Errorf("an unknown environment type answered %q, want nothing", got)
	}
	if got := agents.ManualUpdateCommand("swarm", "ghcr.io/x/y:1'; id; '", p); got != "" {
		t.Errorf("an image that could escape its quotes answered %q", got)
	}
}

func TestManualUpdateCommandUpdateHintCarriesTheCommandForTheEnvironmentType(t *testing.T) {
	p := goldenParams()
	p.Token = "" // a refusal has no join token to give
	swarm := agents.UpdateHint(true, "swarm", p)
	if want := "docker service update --image ghcr.io/ianglennon/tierplan-agent:0.2.0 tierplan-agent"; !strings.Contains(swarm, want) {
		t.Errorf("swarm hint = %q, want it to hold %q", swarm, want)
	}
	standalone := agents.UpdateHint(true, "standalone", p)
	if !strings.Contains(standalone, "docker run -d --name tierplan-agent") || strings.Contains(standalone, "JOIN_TOKEN") {
		t.Errorf("standalone hint = %q, want a recreate with no join token", standalone)
	}
	for _, h := range []string{swarm, standalone} {
		if strings.HasPrefix(h, " ") || strings.Contains(h, "\n") {
			t.Errorf("a hint is a sentence in a refusal, not a command line: %q", h)
		}
	}
	// A server that is behind cannot be fixed with an agent command, and an
	// unknown type has no command.
	if h := agents.UpdateHint(false, "swarm", p); h != "" {
		t.Errorf("hint for an agent that is too new = %q, want none", h)
	}
	if h := agents.UpdateHint(true, "", p); h != "" {
		t.Errorf("hint for an unknown type = %q, want none", h)
	}
}

func TestCleanupCommandsAreTheOnesTheInstallGivesAndReadTheSecretNamesFromTheService(t *testing.T) {
	in, err := agents.BuildInstall(agents.InstallParams{Token: "tpjt_x", Endpoint: "wss://tierplan.example/agent/connect", ServerVersion: "v0.2.1", Generation: 3, EnvID: goldenEnvID})
	if err != nil {
		t.Fatal(err)
	}
	swarm, standalone := agents.CleanupCommands()
	if swarm != in.SwarmCleanup || standalone != in.StandaloneCleanup {
		t.Fatalf("CleanupCommands differ from the install's: %q %q", swarm, standalone)
	}
	if strings.Contains(swarm+standalone, "tpjt_") || strings.Contains(swarm+standalone, agents.SecretPrefix) {
		t.Fatalf("the cleanup names no secret and carries no token: %q %q", swarm, standalone)
	}
	// The names are read from the service before the service is removed, so a swarm with
	// the old generation-only names, or with the names of several generations, is cleaned too.
	line, _, _ := strings.Cut(swarm, "\n")
	find, read, rm, rmSecrets := strings.Index(line, "docker service ls"), strings.Index(line, "S=$(docker service inspect"), strings.Index(line, "docker service rm"), strings.Index(line, "docker secret rm")
	if find < 0 || read < find || rm < read || rmSecrets < rm {
		t.Errorf("the secret names must be read before the service goes, and removed after it: %q", line)
	}
}

func TestSecretNameCarriesAGenerationAndTheTailOfTheEnvironmentID(t *testing.T) {
	name := agents.SecretName(goldenEnvID, 3)
	if name != "tierplan-agent-token-g3-"+goldenTag {
		t.Errorf("SecretName = %q, want tierplan-agent-token-g3-%s", name, goldenTag)
	}
	if !strings.HasPrefix(name, agents.SecretPrefix) {
		t.Errorf("%q does not start with %q, which agents in the field match on", name, agents.SecretPrefix)
	}
	// Docker's rule for a secret name.
	if len(name) > 64 || !regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,62}[A-Za-z0-9])?$`).MatchString(name) {
		t.Errorf("%q is not a valid Docker secret name", name)
	}
	if long := agents.SecretName(goldenEnvID, 1_000_000); len(long) > 64 {
		t.Errorf("%q is %d characters, over Docker's 64", long, len(long))
	}
	// Two environments created in the same UUIDv7 millisecond share their first 18 characters.
	a, b := "018f3c2e-7a41-7c3b-9d52-4b1e6f0a8c37", "018f3c2e-7a41-7c3b-8e10-aa77c0de1234"
	if agents.SecretName(a, 1) == agents.SecretName(b, 1) {
		t.Errorf("environments %s and %s share a secret name", a, b)
	}
	if got := agents.SecretName("ABC-12", 2); got != "tierplan-agent-token-g2-abc12" {
		t.Errorf("a short mixed-case ID gave %q, want the lowercase letters and digits it has", got)
	}
}

func TestInstallCommandCreatesNothingWhileAnAgentServiceExists(t *testing.T) {
	in := mustBuild(t, goldenParams())
	cmd := in.SwarmCommand
	// The agent service is found by its label, whatever it is called (a stack names it
	// <stack>_tierplan-agent), and by the name for one made by hand without the label.
	guard := `if [ -n "$(docker service ls -q --filter label=tierplan.agent=true)" ] || docker service inspect tierplan-agent >/dev/null 2>&1; then`
	if !strings.HasPrefix(cmd, " "+guard) {
		t.Fatalf("the swarm command must start with the service guard:\n%s", cmd)
	}
	thenPart, elsePart, ok := strings.Cut(cmd, "; else ")
	if !ok || !strings.HasSuffix(cmd, "; fi") {
		t.Fatalf("the swarm command is not an if/else:\n%s", cmd)
	}
	for _, want := range []string{"Reinstall an agent on a host that ran one", ">&2", "; false"} {
		if !strings.Contains(thenPart, want) {
			t.Errorf("the then-branch lacks %q:\n%s", want, thenPart)
		}
	}
	if strings.Contains(thenPart, "docker secret create") || strings.Contains(thenPart, "docker service create") {
		t.Errorf("the guarded branch creates something:\n%s", thenPart)
	}
	if strings.Count(thenPart, "'") != 2 {
		t.Errorf("the message must sit in one pair of single quotes (it holds no apostrophe):\n%s", thenPart)
	}
	if !strings.HasPrefix(elsePart, "printf %s '"+goldenToken+"' | docker secret create ") || !strings.Contains(elsePart, "&& docker service create --name tierplan-agent") {
		t.Errorf("the else-branch is not the secret create and the service create:\n%s", elsePart)
	}
}

// stubDocker is a POSIX sh docker for the generated commands. It logs each call, one line of
// its arguments, to STUB_LOG, and answers from the environment: service ls prints STUB_LS
// (fails with STUB_LS_FAIL set), service inspect -f prints STUB_SECRETS, service inspect by
// name succeeds only with STUB_NAMED set, service rm of STUB_RM_FAIL fails, and secret rm of
// STUB_SECRET_RM_FAIL fails. Anything else reads its input and succeeds.
const stubDocker = `#!/bin/sh
echo "$*" >> "$STUB_LOG"
case "$1 $2" in
"service ls") [ -n "$STUB_LS_FAIL" ] && exit 1; for id in $STUB_LS; do echo "$id"; done; exit 0 ;;
"service inspect") if [ "$3" = "-f" ]; then for n in $STUB_SECRETS; do printf '%s ' "$n"; done; echo; exit 0; fi; [ -n "$STUB_NAMED" ] && exit 0; exit 1 ;;
"service rm") [ "$3" = "$STUB_RM_FAIL" ] && exit 1; echo "$3"; exit 0 ;;
"secret rm") [ "$3" = "$STUB_SECRET_RM_FAIL" ] && exit 1; echo "$3"; exit 0 ;;
esac
cat >/dev/null; exit 0
`

// shells are the shells a generated command is run under: sh, dash and bash, each when it is
// installed and is not one of the others already. An admin pastes the command into whichever
// they have, and the command is POSIX sh.
func shells(t *testing.T) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, name := range []string{"sh", "dash", "bash"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Skip("no POSIX shell")
	}
	return out
}

// runWithStub runs script under shell with the stub docker first on the PATH and env added,
// and returns the output, whether it failed and the docker calls made.
func runWithStub(t *testing.T, shell, script string, env ...string) (out string, failed bool, calls []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stubDocker), 0o700); err != nil { //nolint:gosec // a test stub in a temp dir
		t.Fatal(err)
	}
	cmd := exec.Command(shell, "-c", script) //nolint:gosec // the builder's own output
	cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "STUB_LOG=" + log}, env...)
	b, err := cmd.CombinedOutput()
	got, _ := os.ReadFile(log) //nolint:gosec // a temp file
	if s := strings.TrimSpace(string(got)); s != "" {
		calls = strings.Split(s, "\n")
	}
	return string(b), err != nil, calls
}

func checkCalls(t *testing.T, calls, want []string) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("docker was called %d times, want %d:\n%s", len(calls), len(want), strings.Join(calls, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(calls[i], w) {
			t.Errorf("call %d = %q, want it to start with %q", i, calls[i], w)
		}
	}
}

// The guard is real shell: with a docker that finds an agent service, by its label (as a stack
// deploy makes it) or by its name, nothing is created and the status is not zero; with one that
// finds neither, the secret and the service are created.
func TestInstallCommandGuardRunsInAShell(t *testing.T) {
	const ls = "service ls -q --filter label=tierplan.agent=true"
	const inspect = "service inspect tierplan-agent"
	for _, tc := range []struct {
		name      string
		env       []string
		wantFail  bool
		wantCalls []string
	}{
		{"a stack-deployed service with the label", []string{"STUB_LS=st4ckagent01"}, true, []string{ls}},
		{"a service named tierplan-agent without the label", []string{"STUB_NAMED=1"}, true, []string{ls, inspect}},
		{"no service", nil, false, []string{ls, inspect, "secret create tierplan-agent-token-g1-" + goldenTag + " -", "service create --name tierplan-agent"}},
	} {
		for _, shell := range shells(t) {
			t.Run(tc.name+"/"+filepath.Base(shell), func(t *testing.T) {
				out, failed, calls := runWithStub(t, shell, mustBuild(t, goldenParams()).SwarmCommand, tc.env...)
				if failed != tc.wantFail {
					t.Fatalf("failed = %v, want %v; output:\n%s", failed, tc.wantFail, out)
				}
				checkCalls(t, calls, tc.wantCalls)
				if tc.wantFail && !strings.Contains(out, "Reinstall an agent on a host that ran one") {
					t.Errorf("the refusal prints %q, want it to name the docs section", out)
				}
			})
		}
	}
}

// The cleanup is real shell: it finds the agent service by its label, so a stack-deployed
// <stack>_tierplan-agent is removed too, reads its secrets before it removes it, and its status
// says whether the list and the removal worked.
func TestSwarmCleanupRunsInAShell(t *testing.T) {
	swarm, _ := agents.CleanupCommands()
	line, _, _ := strings.Cut(swarm, "\n")
	const ls = "service ls -q --filter label=tierplan.agent=true"
	const inspect = "service inspect -f {{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{.SecretName}} {{end}} "
	for _, tc := range []struct {
		name      string
		env       []string
		wantFail  bool
		wantOut   string
		wantCalls []string
	}{
		{
			"a stack-deployed service", []string{"STUB_LS=st4ckagent01", "STUB_SECRETS=tierplan-agent-token-g2-4b1e6f0a8c37"}, false, "",
			[]string{ls, inspect + "st4ckagent01", "service rm st4ckagent01", "secret rm tierplan-agent-token-g2-4b1e6f0a8c37"},
		},
		{
			"an old-name service", []string{"STUB_LS=0ldagent0001", "STUB_SECRETS=tierplan-agent-token-g1"}, false, "",
			[]string{ls, inspect + "0ldagent0001", "service rm 0ldagent0001", "secret rm tierplan-agent-token-g1"},
		},
		{"no agent service", nil, false, "no agent service", []string{ls}},
		{"the list fails", []string{"STUB_LS_FAIL=1"}, true, "", []string{ls}},
		{
			"a service removal fails", []string{"STUB_LS=st4ckagent01", "STUB_SECRETS=tierplan-agent-token-g2-x", "STUB_RM_FAIL=st4ckagent01"}, true, "",
			[]string{ls, inspect + "st4ckagent01", "service rm st4ckagent01"},
		},
	} {
		for _, shell := range shells(t) {
			t.Run(tc.name+"/"+filepath.Base(shell), func(t *testing.T) {
				out, failed, calls := runWithStub(t, shell, line, tc.env...)
				if failed != tc.wantFail {
					t.Fatalf("failed = %v, want %v; output:\n%s", failed, tc.wantFail, out)
				}
				if !strings.Contains(out, tc.wantOut) {
					t.Errorf("output %q, want it to hold %q", out, tc.wantOut)
				}
				checkCalls(t, calls, tc.wantCalls)
			})
		}
	}
}

// An install carries no allow-insecure line: agents connect only over wss://, and the
// endpoint TierPlan hands out is always wss. D-09, T-02-144.
func TestInstallCarriesNoAllowInsecureLine(t *testing.T) {
	// The removed switch is spelled in two halves so a repository-wide gate for its name stays exact.
	const switchName = "ALLOW_" + "INSECURE"

	// No text carries an allow-insecure line.
	p := goldenParams()
	p.Generation = 2
	in := mustBuild(t, p)
	for label, text := range allInstallText(in) {
		if strings.Contains(text, switchName) {
			t.Errorf("%s carries an allow-insecure line:\n%s", label, text)
		}
	}
	image := agents.AgentImage(p.ServerVersion)
	standalone := agents.ManualUpdateCommand("standalone", image, p)
	if !strings.Contains(standalone, "docker run") || strings.Contains(standalone, switchName) {
		t.Errorf("the standalone manual update = %q, want a docker run with no allow-insecure line", standalone)
	}
	if strings.Contains(standalone, p.Token) {
		t.Error("a manual update command carries the join token")
	}
}
