// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package agents_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/agents"
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
	goldenToken    = "cmjt_E_S" //nolint:gosec // a made-up token for the golden files
	goldenEndpoint = "wss://example.com/coaming/agent/connect"
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
	if in.SecretName != "coaming-agent-token-g1" {
		t.Errorf("SecretName = %q, want coaming-agent-token-g1", in.SecretName)
	}
	if in.Image != "ghcr.io/ianglennon/coaming-agent:0.2.0" {
		t.Errorf("Image = %q, want the exact server version, never latest", in.Image)
	}

	for _, want := range []string{
		"docker service create", "--name coaming-agent", "--mode global", "--constraint node.platform.os==linux",
		"--secret source=coaming-agent-token-g1,target=coaming-agent-token,mode=0400",
		"--mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
		"--mount type=volume,src=coaming-agent,dst=/var/lib/coaming-agent",
		"--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID",
		"--label coaming.agent=true", "--container-label coaming.agent=true",
		"COAMING_AGENT_URL=" + goldenEndpoint, "COAMING_AGENT_PIN=" + goldenPin,
		"COAMING_AGENT_JOIN_TOKEN_FILE=/run/secrets/coaming-agent-token",
		"docker secret create coaming-agent-token-g1 -",
	} {
		if !strings.Contains(in.SwarmCommand, want) {
			t.Errorf("swarm command lacks %q:\n%s", want, in.SwarmCommand)
		}
	}
	if strings.Contains(in.SwarmCommand, "COAMING_AGENT_JOIN_TOKEN=") {
		t.Errorf("the swarm command passes the token as an env var instead of the secret file:\n%s", in.SwarmCommand)
	}

	for _, want := range []string{
		"docker run -d", "--name coaming-agent", "--restart unless-stopped", "--read-only", "--cap-drop ALL",
		"--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v /var/run/docker.sock:/var/run/docker.sock", "-v coaming-agent:/var/lib/coaming-agent",
		"--label coaming.agent=true", "COAMING_AGENT_URL=" + goldenEndpoint, "COAMING_AGENT_PIN=" + goldenPin,
		"COAMING_AGENT_JOIN_TOKEN=" + goldenToken,
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
		if strings.Contains(text, "COAMING_AGENT_PIN") || strings.Contains(text, "sha256/") {
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
		"v0.2.0":             "ghcr.io/ianglennon/coaming-agent:0.2.0",
		"0.2.0":              "ghcr.io/ianglennon/coaming-agent:0.2.0",
		"v0.2.0-rc.1":        "ghcr.io/ianglennon/coaming-agent:0.2.0-rc.1",
		"v1.10.3":            "ghcr.io/ianglennon/coaming-agent:1.10.3",
		"v0.2.0+build.5":     "ghcr.io/ianglennon/coaming-agent:0.2.0",
		"dev (abc123)":       "ghcr.io/ianglennon/coaming-agent:edge",
		"edge":               "ghcr.io/ianglennon/coaming-agent:edge",
		"":                   "ghcr.io/ianglennon/coaming-agent:edge",
		"v0.2":               "ghcr.io/ianglennon/coaming-agent:edge",
		"v0.2.0; rm -rf /":   "ghcr.io/ianglennon/coaming-agent:edge",
		"v0.2.0-rc.1 --x":    "ghcr.io/ianglennon/coaming-agent:edge",
		"vv0.2.0":            "ghcr.io/ianglennon/coaming-agent:edge",
		"v0.2.0-$(id)":       "ghcr.io/ianglennon/coaming-agent:edge",
		"v0.2.0-rc.1\nextra": "ghcr.io/ianglennon/coaming-agent:edge",
	} {
		if got := agents.AgentImage(server); got != want {
			t.Errorf("AgentImage(%q) = %q, want %q", server, got, want)
		}
	}
	if got := mustBuild(t, agents.InstallParams{Token: goldenToken, Endpoint: goldenEndpoint, ServerVersion: "dev (abc123)", Generation: 1}).Image; !strings.HasSuffix(got, ":edge") {
		t.Errorf("a dev server's install image = %q, want the edge tag", got)
	}
}

// devParams is goldenParams for a development server: a plain endpoint and the
// server's switch, so the install texts carry the agent's switch too (T-02-31).
func devParams() agents.InstallParams {
	p := goldenParams()
	p.Endpoint, p.AllowInsecure = "ws://10.0.0.5:7480/agent/connect", true
	return p
}

func TestInstallCommandsAreOneLinersWithALeadingSpaceAndNoInboundPort(t *testing.T) {
	for label, p := range map[string]agents.InstallParams{"production": goldenParams(), "development": devParams()} {
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
	for label, p := range map[string]agents.InstallParams{"production": goldenParams(), "development": devParams()} {
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
	want := "# Create the secret first:  printf %s '" + goldenToken + "' | docker secret create coaming-agent-token-g1 -"
	if first != want {
		t.Errorf("first line = %q, want the one-line docker secret create command %q", first, want)
	}
	if strings.Contains(rest, goldenToken) {
		t.Errorf("the stack file has the token below its first comment line:\n%s", rest)
	}
	if strings.Count(in.SwarmStackFile, "external: true") != 1 || !strings.Contains(in.SwarmStackFile, "coaming-agent-token-g1:") {
		t.Errorf("the stack file must declare coaming-agent-token-g1 as external exactly once:\n%s", in.SwarmStackFile)
	}
	for _, want := range []string{
		"mode: global", "node.platform.os == linux", "read_only: true", "- ALL", "- SETUID", "- SETGID",
		"coaming.agent: \"true\"", "target: coaming-agent-token", "mode: 0400",
		"COAMING_AGENT_JOIN_TOKEN_FILE: /run/secrets/coaming-agent-token", "/var/run/docker.sock:/var/run/docker.sock",
		"coaming-agent:/var/lib/coaming-agent",
	} {
		if !strings.Contains(in.SwarmStackFile, want) {
			t.Errorf("the stack file lacks %q:\n%s", want, in.SwarmStackFile)
		}
	}
	if strings.Count(in.SwarmStackFile, "coaming.agent:") != 2 {
		t.Errorf("the stack file must label both the service (deploy.labels) and its containers (labels):\n%s", in.SwarmStackFile)
	}
}

func TestInstallCommandComposeFileCarriesTheSameEnvironmentAsTheCommand(t *testing.T) {
	in := mustBuild(t, goldenParams())
	for _, want := range []string{
		"restart: unless-stopped", "no-new-privileges:true", "read_only: true", "- SETUID", "- SETGID",
		"COAMING_AGENT_URL: \"" + goldenEndpoint + "\"", "COAMING_AGENT_PIN: \"" + goldenPin + "\"",
		"COAMING_AGENT_JOIN_TOKEN: \"" + goldenToken + "\"",
		"/var/run/docker.sock:/var/run/docker.sock", "coaming-agent:/var/lib/coaming-agent", "coaming.agent: \"true\"",
		"container_name: coaming-agent",
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
	if in.SecretName != "coaming-agent-token-g2" {
		t.Fatalf("SecretName = %q, want coaming-agent-token-g2", in.SecretName)
	}
	for _, want := range []string{
		`OLD=$(docker service inspect -f '{{(index .Spec.TaskTemplate.ContainerSpec.Secrets 0).SecretName}}' coaming-agent)`,
		"docker secret create coaming-agent-token-g2 -",
		`docker service update --secret-rm "$OLD" --secret-add source=coaming-agent-token-g2,target=coaming-agent-token,mode=0400 coaming-agent`,
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
	for _, want := range []string{"docker service rm coaming-agent", "docker secret rm coaming-agent-token-g1", "docker volume rm coaming-agent"} {
		if !strings.Contains(in.SwarmCleanup, want) {
			t.Errorf("the swarm cleanup lacks %q:\n%s", want, in.SwarmCleanup)
		}
	}
	for _, want := range []string{"docker rm -f coaming-agent", "docker volume rm coaming-agent"} {
		if !strings.Contains(in.StandaloneCleanup, want) {
			t.Errorf("the standalone cleanup lacks %q:\n%s", want, in.StandaloneCleanup)
		}
	}
}

func TestInstallCommandRefusesValuesThatCouldBreakOutOfTheQuotes(t *testing.T) {
	for name, mutate := range map[string]func(*agents.InstallParams){
		"token with a quote":    func(p *agents.InstallParams) { p.Token = "cm'; touch /tmp/x; '" },
		"endpoint with a quote": func(p *agents.InstallParams) { p.Endpoint = "wss://x'y/agent/connect" },
		"pin with a quote":      func(p *agents.InstallParams) { p.Pin = "sha256/a'b" },
		"token with a newline":  func(p *agents.InstallParams) { p.Token = "cm\nrm" },
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

	if got, want := agents.ManualUpdateCommand("swarm", image, p), " docker service update --image ghcr.io/ianglennon/coaming-agent:0.2.0 coaming-agent"; got != want {
		t.Errorf("swarm update = %q, want %q", got, want)
	}

	got := agents.ManualUpdateCommand("standalone", image, p)
	for _, want := range []string{
		"docker pull ghcr.io/ianglennon/coaming-agent:0.2.0", "docker rm -f coaming-agent", "docker run -d --name coaming-agent",
		"--restart unless-stopped", "--read-only", "--cap-drop ALL", "--cap-add SETUID", "--cap-add SETGID", "--security-opt no-new-privileges",
		"-v coaming-agent:/var/lib/coaming-agent", "COAMING_AGENT_URL=" + goldenEndpoint, "COAMING_AGENT_PIN=" + goldenPin,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("standalone update lacks %q:\n%s", want, got)
		}
	}
	// The state volume already holds the agent's identity, so no join token goes in.
	if strings.Contains(got, "JOIN_TOKEN") || strings.Contains(got, goldenToken) {
		t.Errorf("the standalone update carries a join token:\n%s", got)
	}
	if !strings.HasSuffix(got, " ghcr.io/ianglennon/coaming-agent:0.2.0") {
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
	if want := "docker service update --image ghcr.io/ianglennon/coaming-agent:0.2.0 coaming-agent"; !strings.Contains(swarm, want) {
		t.Errorf("swarm hint = %q, want it to hold %q", swarm, want)
	}
	standalone := agents.UpdateHint(true, "standalone", p)
	if !strings.Contains(standalone, "docker run -d --name coaming-agent") || strings.Contains(standalone, "JOIN_TOKEN") {
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
	in, err := agents.BuildInstall(agents.InstallParams{Token: "cmjt_x", Endpoint: "wss://coaming.example/agent/connect", ServerVersion: "v0.2.1", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	swarm, standalone := agents.CleanupCommands(3)
	if swarm != in.SwarmCleanup || standalone != in.StandaloneCleanup {
		t.Fatalf("CleanupCommands differ from the install's: %q %q", swarm, standalone)
	}
	if !strings.Contains(swarm, "docker secret rm "+agents.SecretName(3)) || strings.Contains(swarm+standalone, "cmjt_") {
		t.Fatalf("the swarm cleanup must name generation 3's secret and carry no token: %q", swarm)
	}
	if other, _ := agents.CleanupCommands(4); !strings.Contains(other, agents.SecretName(4)) {
		t.Fatalf("generation 4: %q", other)
	}
}

// TestInstallCommandCarriesTheAgentSwitchOnlyForAPlainEndpointWithTheServerSwitch
// pins T-02-31: the agent's COAMING_AGENT_ALLOW_INSECURE is in an install only when
// the server's own switch is on and the endpoint is ws://. Every production text
// (wss://, or no server switch) is free of it, the manual update included.
func TestInstallCommandCarriesTheAgentSwitchOnlyForAPlainEndpointWithTheServerSwitch(t *testing.T) {
	const plain = "ws://10.0.0.5:7480/agent/connect"
	const loopback = "ws://127.0.0.1:7480/agent/connect"
	const name = "COAMING_AGENT_ALLOW_INSECURE"
	cases := []struct {
		label    string
		allow    bool
		endpoint string
		want     bool
	}{
		{"production: wss, no server switch", false, goldenEndpoint, false},
		{"wss whatever the server switch", true, goldenEndpoint, false},
		{"plain loopback endpoint without the server switch", false, loopback, false},
		{"development: plain endpoint with the server switch", true, plain, true},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			p := goldenParams()
			p.Endpoint, p.AllowInsecure, p.Generation = tc.endpoint, tc.allow, 2
			in := mustBuild(t, p)
			// The four texts carry it exactly once, right after the address; the rotate
			// command and the cleanup commands never do.
			for label, text := range map[string]string{
				"swarm command": in.SwarmCommand, "standalone command": in.StandaloneCommand,
				"stack file": in.SwarmStackFile, "compose file": in.StandaloneComposeFile,
			} {
				got := strings.Count(text, name)
				if tc.want && got != 1 || !tc.want && got != 0 {
					t.Errorf("%s holds %s %d times, want %v:\n%s", label, name, got, tc.want, text)
				}
			}
			for label, text := range map[string]string{
				"rotate command": in.SwarmRotateCommand, "swarm cleanup": in.SwarmCleanup, "standalone cleanup": in.StandaloneCleanup,
			} {
				if strings.Contains(text, name) {
					t.Errorf("%s carries %s:\n%s", label, name, text)
				}
			}
			if tc.want {
				for label, want := range map[string]string{
					"swarm command":      "--env 'COAMING_AGENT_URL=" + plain + "' --env '" + name + "=true'",
					"standalone command": "-e 'COAMING_AGENT_URL=" + plain + "' -e '" + name + "=true'",
					"stack file":         "COAMING_AGENT_URL: \"" + plain + "\"\n      " + name + ": \"true\"\n",
					"compose file":       "COAMING_AGENT_URL: \"" + plain + "\"\n      " + name + ": \"true\"\n",
				} {
					text := map[string]string{"swarm command": in.SwarmCommand, "standalone command": in.StandaloneCommand,
						"stack file": in.SwarmStackFile, "compose file": in.StandaloneComposeFile}[label]
					if !strings.Contains(text, want) {
						t.Errorf("%s lacks the switch right after the address, %q:\n%s", label, want, text)
					}
				}
			}

			// The manual update and its hint follow the same rule; the swarm one never
			// carries it, because a service update keeps the service's environment.
			image := agents.AgentImage(p.ServerVersion)
			standalone := agents.ManualUpdateCommand("standalone", image, p)
			if got := strings.Contains(standalone, "-e '"+name+"=true'"); got != tc.want {
				t.Errorf("standalone manual update carries the switch = %v, want %v:\n%s", got, tc.want, standalone)
			}
			if tc.want && !strings.Contains(standalone, "-e 'COAMING_AGENT_URL="+plain+"' -e '"+name+"=true'") {
				t.Errorf("standalone manual update lacks the switch right after the address:\n%s", standalone)
			}
			if swarm := agents.ManualUpdateCommand("swarm", image, p); strings.Contains(swarm, name) {
				t.Errorf("swarm manual update carries the switch:\n%s", swarm)
			}
			hint := agents.UpdateHint(true, "standalone", p)
			if got := strings.Contains(hint, "-e '"+name+"=true'"); got != tc.want {
				t.Errorf("update hint carries the switch = %v, want %v:\n%s", got, tc.want, hint)
			}
			if hint := agents.UpdateHint(true, "swarm", p); strings.Contains(hint, name) {
				t.Errorf("swarm update hint carries the switch:\n%s", hint)
			}

			if strings.Contains(standalone, p.Token) || strings.Contains(hint, p.Token) {
				t.Error("a manual update command carries the join token")
			}
		})
	}

	// The golden endpoint's install is the production shape: it never changes.
	for label, text := range allInstallText(mustBuild(t, goldenParams())) {
		if strings.Contains(text, name) {
			t.Errorf("golden %s carries %s", label, name)
		}
	}
}

// A plain ws:// endpoint for a host that is not this machine is one agents refuse at
// start (plan 02-41) unless the server's development switch is on, so Coaming builds
// no text for it: BuildInstall refuses, and the standalone manual update and the
// update hint built on it are empty. The swarm manual update carries no address (a
// service update keeps the service's environment), so it stays. G-02-R4-3, T-02-144.
func TestBuildInstallRefusesAPlainEndpointForAnotherHostWithoutTheServerSwitch(t *testing.T) {
	const plain = "ws://10.0.0.5:7480/agent/connect"
	p := goldenParams()
	p.Endpoint, p.AllowInsecure = plain, false

	if _, err := agents.BuildInstall(p); !errors.Is(err, agents.ErrPlainEndpointRefused) {
		t.Fatalf("BuildInstall = %v, want ErrPlainEndpointRefused", err)
	}
	image := agents.AgentImage(p.ServerVersion)
	if got := agents.ManualUpdateCommand("standalone", image, p); got != "" {
		t.Errorf("the standalone manual update for a refused endpoint = %q, want none", got)
	}
	if got := agents.UpdateHint(true, "standalone", p); got != "" {
		t.Errorf("the update hint for a refused endpoint = %q, want none", got)
	}
	if got := agents.ManualUpdateCommand("swarm", image, p); got != " docker service update --image "+image+" coaming-agent" {
		t.Errorf("the swarm manual update = %q, want the service update", got)
	}
	if got := agents.UpdateHint(true, "swarm", p); !strings.Contains(got, "docker service update") {
		t.Errorf("the swarm update hint = %q, want the service update", got)
	}

	// The same endpoint builds with the development switch, and carries the agent's.
	p.AllowInsecure = true
	in := mustBuild(t, p)
	if !strings.Contains(in.StandaloneCommand, "-e 'COAMING_AGENT_ALLOW_INSECURE=true'") {
		t.Errorf("the development install lacks the agent's switch:\n%s", in.StandaloneCommand)
	}
	if got := agents.ManualUpdateCommand("standalone", image, p); !strings.Contains(got, "docker run") {
		t.Errorf("the development standalone manual update = %q, want a docker run", got)
	}

	// A plain endpoint on this machine builds without the switch, and so does https.
	for _, endpoint := range []string{"ws://127.0.0.1:7480/agent/connect", "ws://localhost:7480/agent/connect", "ws://[::1]:7480/agent/connect", goldenEndpoint} {
		p := goldenParams()
		p.Endpoint = endpoint
		in := mustBuild(t, p)
		for label, text := range allInstallText(in) {
			if strings.Contains(text, "COAMING_AGENT_ALLOW_INSECURE") {
				t.Errorf("%s for %s carries the agent's switch", label, endpoint)
			}
		}
		if got := agents.ManualUpdateCommand("standalone", agents.AgentImage(p.ServerVersion), p); !strings.Contains(got, "docker run") {
			t.Errorf("the standalone manual update for %s = %q, want a docker run", endpoint, got)
		}
	}
}
