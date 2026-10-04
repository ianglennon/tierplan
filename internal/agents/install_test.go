// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents_test

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/agents"
)

// update rewrites the golden files: go test ./internal/agents/ -run InstallCommand -update.
// Do it only when the change to the command text is intended; the golden files
// are what an admin pastes into a shell as root.
var update = flag.Bool("update", false, "rewrite the install golden files")

const (
	goldenToken    = "bpjt_E_S" //nolint:gosec // a made-up token for the golden files
	goldenEndpoint = "wss://example.com/bayplan/agent/connect"
	goldenPin      = "sha256/P"
)

func goldenParams() agents.InstallParams {
	return agents.InstallParams{
		Token: goldenToken, Endpoint: goldenEndpoint, Pin: goldenPin, ServerVersion: "v0.2.0", Generation: 1,
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
	if in.SecretName != "bayplan-agent-token-g1" {
		t.Errorf("SecretName = %q, want bayplan-agent-token-g1", in.SecretName)
	}
	if in.Image != "ghcr.io/ianglennon/bayplan-agent:0.2.0" {
		t.Errorf("Image = %q, want the exact server version, never latest", in.Image)
	}

	for _, want := range []string{
		"docker service create", "--name bayplan-agent", "--mode global", "--constraint node.platform.os==linux",
		"--secret source=bayplan-agent-token-g1,target=bayplan-agent-token,mode=0400",
		"--mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
		"--mount type=volume,src=bayplan-agent,dst=/var/lib/bayplan-agent",
		"--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID",
		"--label bayplan.agent=true", "--container-label bayplan.agent=true",
		"BAYPLAN_AGENT_URL=" + goldenEndpoint, "BAYPLAN_AGENT_PIN=" + goldenPin,
		"BAYPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/bayplan-agent-token",
		"docker secret create bayplan-agent-token-g1 -",
	} {
		if !strings.Contains(in.SwarmCommand, want) {
			t.Errorf("swarm command lacks %q:\n%s", want, in.SwarmCommand)
		}
	}
	if strings.Contains(in.SwarmCommand, "BAYPLAN_AGENT_JOIN_TOKEN=") {
		t.Errorf("the swarm command passes the token as an env var instead of the secret file:\n%s", in.SwarmCommand)
	}

	for _, want := range []string{
		"docker run -d", "--name bayplan-agent", "--restart unless-stopped", "--read-only", "--cap-drop ALL",
		"--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v /var/run/docker.sock:/var/run/docker.sock", "-v bayplan-agent:/var/lib/bayplan-agent",
		"--label bayplan.agent=true", "BAYPLAN_AGENT_URL=" + goldenEndpoint, "BAYPLAN_AGENT_PIN=" + goldenPin,
		"BAYPLAN_AGENT_JOIN_TOKEN=" + goldenToken,
	} {
		if !strings.Contains(in.StandaloneCommand, want) {
			t.Errorf("standalone command lacks %q:\n%s", want, in.StandaloneCommand)
		}
	}
	if strings.Contains(in.StandaloneCommand, "--network host") || strings.Contains(in.SwarmCommand, "--network host") {
		t.Error("an install command uses the host network")
	}
}

func TestInstallCommandWithoutAPinNamesNone(t *testing.T) {
	p := goldenParams()
	p.Pin = ""
	in := mustBuild(t, p)
	for name, text := range allInstallText(in) {
		if strings.Contains(text, "BAYPLAN_AGENT_PIN") || strings.Contains(text, "sha256/") {
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
		"v0.2.0":             "ghcr.io/ianglennon/bayplan-agent:0.2.0",
		"0.2.0":              "ghcr.io/ianglennon/bayplan-agent:0.2.0",
		"v0.2.0-rc.1":        "ghcr.io/ianglennon/bayplan-agent:0.2.0-rc.1",
		"v1.10.3":            "ghcr.io/ianglennon/bayplan-agent:1.10.3",
		"v0.2.0+build.5":     "ghcr.io/ianglennon/bayplan-agent:0.2.0",
		"dev (abc123)":       "ghcr.io/ianglennon/bayplan-agent:edge",
		"edge":               "ghcr.io/ianglennon/bayplan-agent:edge",
		"":                   "ghcr.io/ianglennon/bayplan-agent:edge",
		"v0.2":               "ghcr.io/ianglennon/bayplan-agent:edge",
		"v0.2.0; rm -rf /":   "ghcr.io/ianglennon/bayplan-agent:edge",
		"v0.2.0-rc.1 --x":    "ghcr.io/ianglennon/bayplan-agent:edge",
		"vv0.2.0":            "ghcr.io/ianglennon/bayplan-agent:edge",
		"v0.2.0-$(id)":       "ghcr.io/ianglennon/bayplan-agent:edge",
		"v0.2.0-rc.1\nextra": "ghcr.io/ianglennon/bayplan-agent:edge",
	} {
		if got := agents.AgentImage(server); got != want {
			t.Errorf("AgentImage(%q) = %q, want %q", server, got, want)
		}
	}
	if got := mustBuild(t, agents.InstallParams{Token: goldenToken, Endpoint: goldenEndpoint, ServerVersion: "dev (abc123)", Generation: 1}).Image; !strings.HasSuffix(got, ":edge") {
		t.Errorf("a dev server's install image = %q, want the edge tag", got)
	}
}

func TestInstallCommandsAreOneLinersWithALeadingSpaceAndNoInboundPort(t *testing.T) {
	p := goldenParams()
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
	p := goldenParams()
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
	want := "# Create the secret first:  printf %s '" + goldenToken + "' | docker secret create bayplan-agent-token-g1 -"
	if first != want {
		t.Errorf("first line = %q, want the one-line docker secret create command %q", first, want)
	}
	if strings.Contains(rest, goldenToken) {
		t.Errorf("the stack file has the token below its first comment line:\n%s", rest)
	}
	if strings.Count(in.SwarmStackFile, "external: true") != 1 || !strings.Contains(in.SwarmStackFile, "bayplan-agent-token-g1:") {
		t.Errorf("the stack file must declare bayplan-agent-token-g1 as external exactly once:\n%s", in.SwarmStackFile)
	}
	for _, want := range []string{
		"mode: global", "node.platform.os == linux", "read_only: true", "- ALL", "- SETUID", "- SETGID",
		"bayplan.agent: \"true\"", "target: bayplan-agent-token", "mode: 0400",
		"BAYPLAN_AGENT_JOIN_TOKEN_FILE: /run/secrets/bayplan-agent-token", "/var/run/docker.sock:/var/run/docker.sock",
		"bayplan-agent:/var/lib/bayplan-agent",
	} {
		if !strings.Contains(in.SwarmStackFile, want) {
			t.Errorf("the stack file lacks %q:\n%s", want, in.SwarmStackFile)
		}
	}
	if strings.Count(in.SwarmStackFile, "bayplan.agent:") != 2 {
		t.Errorf("the stack file must label both the service (deploy.labels) and its containers (labels):\n%s", in.SwarmStackFile)
	}
}

func TestInstallCommandComposeFileCarriesTheSameEnvironmentAsTheCommand(t *testing.T) {
	in := mustBuild(t, goldenParams())
	for _, want := range []string{
		"restart: unless-stopped", "no-new-privileges:true", "read_only: true", "- SETUID", "- SETGID",
		"BAYPLAN_AGENT_URL: \"" + goldenEndpoint + "\"", "BAYPLAN_AGENT_PIN: \"" + goldenPin + "\"",
		"BAYPLAN_AGENT_JOIN_TOKEN: \"" + goldenToken + "\"",
		"/var/run/docker.sock:/var/run/docker.sock", "bayplan-agent:/var/lib/bayplan-agent", "bayplan.agent: \"true\"",
		"container_name: bayplan-agent",
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
	if in.SecretName != "bayplan-agent-token-g2" {
		t.Fatalf("SecretName = %q, want bayplan-agent-token-g2", in.SecretName)
	}
	for _, want := range []string{
		`OLD=$(docker service inspect -f '{{(index .Spec.TaskTemplate.ContainerSpec.Secrets 0).SecretName}}' bayplan-agent)`,
		"docker secret create bayplan-agent-token-g2 -",
		`docker service update --secret-rm "$OLD" --secret-add source=bayplan-agent-token-g2,target=bayplan-agent-token,mode=0400 bayplan-agent`,
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
	for _, want := range []string{"docker service rm bayplan-agent", "docker secret rm bayplan-agent-token-g1", "docker volume rm bayplan-agent"} {
		if !strings.Contains(in.SwarmCleanup, want) {
			t.Errorf("the swarm cleanup lacks %q:\n%s", want, in.SwarmCleanup)
		}
	}
	for _, want := range []string{"docker rm -f bayplan-agent", "docker volume rm bayplan-agent"} {
		if !strings.Contains(in.StandaloneCleanup, want) {
			t.Errorf("the standalone cleanup lacks %q:\n%s", want, in.StandaloneCleanup)
		}
	}
}

func TestInstallCommandRefusesValuesThatCouldBreakOutOfTheQuotes(t *testing.T) {
	for name, mutate := range map[string]func(*agents.InstallParams){
		"token with a quote":    func(p *agents.InstallParams) { p.Token = "bp'; touch /tmp/x; '" },
		"endpoint with a quote": func(p *agents.InstallParams) { p.Endpoint = "wss://x'y/agent/connect" },
		"pin with a quote":      func(p *agents.InstallParams) { p.Pin = "sha256/a'b" },
		"token with a newline":  func(p *agents.InstallParams) { p.Token = "bp\nrm" },
		"endpoint with newline": func(p *agents.InstallParams) { p.Endpoint = "wss://x/agent/connect\n" },
		"empty token":           func(p *agents.InstallParams) { p.Token = "" },
		"empty endpoint":        func(p *agents.InstallParams) { p.Endpoint = "" },
		"generation zero":       func(p *agents.InstallParams) { p.Generation = 0 },
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

	if got, want := agents.ManualUpdateCommand("swarm", image, p), " docker service update --image ghcr.io/ianglennon/bayplan-agent:0.2.0 bayplan-agent"; got != want {
		t.Errorf("swarm update = %q, want %q", got, want)
	}

	got := agents.ManualUpdateCommand("standalone", image, p)
	for _, want := range []string{
		"docker pull ghcr.io/ianglennon/bayplan-agent:0.2.0", "docker rm -f bayplan-agent", "docker run -d --name bayplan-agent",
		"--restart unless-stopped", "--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v bayplan-agent:/var/lib/bayplan-agent", "BAYPLAN_AGENT_URL=" + goldenEndpoint, "BAYPLAN_AGENT_PIN=" + goldenPin,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("standalone update lacks %q:\n%s", want, got)
		}
	}
	// The state volume already holds the agent's identity, so no join token goes in.
	if strings.Contains(got, "JOIN_TOKEN") || strings.Contains(got, goldenToken) {
		t.Errorf("the standalone update carries a join token:\n%s", got)
	}
	if !strings.HasSuffix(got, " ghcr.io/ianglennon/bayplan-agent:0.2.0") {
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
	if want := "docker service update --image ghcr.io/ianglennon/bayplan-agent:0.2.0 bayplan-agent"; !strings.Contains(swarm, want) {
		t.Errorf("swarm hint = %q, want it to hold %q", swarm, want)
	}
	standalone := agents.UpdateHint(true, "standalone", p)
	if !strings.Contains(standalone, "docker run -d --name bayplan-agent") || strings.Contains(standalone, "JOIN_TOKEN") {
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

func TestCleanupCommandsAreTheOnesTheInstallGivesAndNameTheGivenSecretGeneration(t *testing.T) {
	in, err := agents.BuildInstall(agents.InstallParams{Token: "bpjt_x", Endpoint: "wss://bayplan.example/agent/connect", ServerVersion: "v0.2.1", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	swarm, standalone := agents.CleanupCommands(3)
	if swarm != in.SwarmCleanup || standalone != in.StandaloneCleanup {
		t.Fatalf("CleanupCommands differ from the install's: %q %q", swarm, standalone)
	}
	if !strings.Contains(swarm, "docker secret rm "+agents.SecretName(3)) || strings.Contains(swarm+standalone, "bpjt_") {
		t.Fatalf("the swarm cleanup must name generation 3's secret and carry no token: %q", swarm)
	}
	if other, _ := agents.CleanupCommands(4); !strings.Contains(other, agents.SecretName(4)) {
		t.Fatalf("generation 4: %q", other)
	}
}
