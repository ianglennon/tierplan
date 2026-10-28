// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	ctypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Redacting the changes of a review (plan 08-04, D-47, AUTH-07): a live value a reader may not reveal is
// replaced by the one placeholder, and a setting that differed only in such a value is still listed by its path.

func redactMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func redactSwarmSpec(env, command, args, test []string, logOpt, image string) swarm.ServiceSpec {
	return swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: image, Env: env, Command: command, Args: args,
				Healthcheck: &ctypes.HealthConfig{Test: test},
			},
			LogDriver: &swarm.Driver{Name: "syslog", Options: map[string]string{"syslog-address": logOpt}},
		},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{{TargetPort: 80, PublishedPort: 9090}}},
	}
}

// redactSecrets are the made-up live values no redacted change may carry.
var redactSecrets = []string{"old-secret-1a2b", "new-secret-3c4d", "hunter2-old", "hunter2-new", "tok-old-5e6f", "tok-new-7a8b", "curl-old-pw", "curl-new-pw", "tcp://old:pw@h", "tcp://new:pw@h"}

func redactAssertNoSecret(t *testing.T, label string, raw ...json.RawMessage) {
	t.Helper()
	for _, r := range raw {
		for _, s := range redactSecrets {
			if strings.Contains(string(r), s) {
				t.Errorf("%s still carries %q: %s", label, s, r)
			}
		}
	}
}

func TestRedactChangesWithholdsEveryLiveValue(t *testing.T) {
	t.Parallel()
	cur := redactSwarmSpec([]string{"API_KEY=old-secret-1a2b", "MODE=prod", "BARE"}, []string{"/bin/run", "--pw=hunter2-old"}, []string{"--token=tok-old-5e6f"},
		[]string{"CMD-SHELL", "curl -u me:curl-old-pw http://x"}, "tcp://old:pw@h", "nginx:1.28@sha256:aaa")
	next := redactSwarmSpec([]string{"API_KEY=new-secret-3c4d", "MODE=prod", "BARE"}, []string{"/bin/run", "--pw=hunter2-new"}, []string{"--token=tok-new-7a8b"},
		[]string{"CMD-SHELL", "curl -u me:curl-new-pw http://x"}, "tcp://new:pw@h", "nginx:1.27@sha256:bbb")
	curRaw, nextRaw := redactMarshal(t, cur), redactMarshal(t, next)
	in := []stacks.ServiceChange{
		{Name: "s_web", Short: "web", Change: stacks.ChangeChanged, Current: curRaw, Next: nextRaw, HiddenChanges: []string{"TaskTemplate.ContainerSpec.Env[API_KEY]"}, Settings: 7},
		{Name: "s_new", Short: "new", Change: stacks.ChangeAdded, Next: nextRaw, Settings: 0},
		{Name: "s_gone", Short: "gone", Change: stacks.ChangeRemoved},
	}
	inCopy := redactMarshal(t, in)

	out := stackops.RedactChanges(in, stacks.TargetSwarm)

	if after := redactMarshal(t, in); string(after) != string(inCopy) {
		t.Errorf("the input was modified:\n before %s\n after  %s", inCopy, after)
	}
	if len(out) != 3 {
		t.Fatalf("got %d changes, want 3", len(out))
	}
	for i, c := range out {
		redactAssertNoSecret(t, "change "+c.Name, c.Current, c.Next)
		if c.Name != in[i].Name || c.Change != in[i].Change || c.Settings != in[i].Settings {
			t.Errorf("change %d = %s %s settings %d, want the name, kind and count unchanged", i, c.Name, c.Change, c.Settings)
		}
	}

	var gotCur, gotNext swarm.ServiceSpec
	if err := json.Unmarshal(out[0].Current, &gotCur); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out[0].Next, &gotNext); err != nil {
		t.Fatal(err)
	}
	for side, spec := range map[string]swarm.ServiceSpec{"current": gotCur, "next": gotNext} {
		cs := spec.TaskTemplate.ContainerSpec
		if want := []string{"API_KEY=" + redact.Placeholder, "MODE=" + redact.Placeholder, "BARE"}; !slices.Equal(cs.Env, want) {
			t.Errorf("%s env = %v, want the names kept and the values withheld %v", side, cs.Env, want)
		}
		if want := []string{redact.Placeholder}; !slices.Equal(cs.Command, want) || !slices.Equal(cs.Args, want) {
			t.Errorf("%s command %v args %v, want one placeholder each", side, cs.Command, cs.Args)
		}
		if want := []string{"CMD-SHELL", redact.Placeholder}; !slices.Equal(cs.Healthcheck.Test, want) {
			t.Errorf("%s healthcheck = %v, want the mode kept and the command withheld", side, cs.Healthcheck.Test)
		}
		if got := spec.TaskTemplate.LogDriver.Options["syslog-address"]; got != redact.Placeholder {
			t.Errorf("%s log option = %q, want it withheld", side, got)
		}
		if spec.EndpointSpec == nil || len(spec.EndpointSpec.Ports) != 1 || spec.EndpointSpec.Ports[0].PublishedPort != 9090 {
			t.Errorf("%s ports = %+v, want them untouched", side, spec.EndpointSpec)
		}
	}
	if img := gotCur.TaskTemplate.ContainerSpec.Image; img != "nginx:1.28@sha256:aaa" {
		t.Errorf("current image = %q, want it untouched", img)
	}
	if img := gotNext.TaskTemplate.ContainerSpec.Image; img != "nginx:1.27@sha256:bbb" {
		t.Errorf("next image = %q, want it untouched", img)
	}

	// Every setting that differed only in a withheld value is listed by its path, once, in order; MODE did not
	// differ and is not listed; the path the review already listed is not listed twice.
	wantHidden := []string{
		"TaskTemplate.ContainerSpec.Args",
		"TaskTemplate.ContainerSpec.Command",
		"TaskTemplate.ContainerSpec.Env[API_KEY]",
		"TaskTemplate.ContainerSpec.Healthcheck.Test",
		"TaskTemplate.LogDriver.Options.syslog-address",
	}
	if !slices.Equal(out[0].HiddenChanges, wantHidden) {
		t.Errorf("hidden changes = %v, want %v", out[0].HiddenChanges, wantHidden)
	}
	if len(out[1].HiddenChanges) != 0 || len(out[2].HiddenChanges) != 0 {
		t.Errorf("an added or removed service lists hidden changes %v %v, it has no other side to differ from", out[1].HiddenChanges, out[2].HiddenChanges)
	}
	if out[1].Current != nil || len(out[2].Current) != 0 || len(out[2].Next) != 0 {
		t.Errorf("an added service gained a current spec or a removed one a spec: %s %s %s", out[1].Current, out[2].Current, out[2].Next)
	}
}

func TestRedactChangesWithholdsAStandaloneServiceTheSameWay(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	svc := func(key, pw, image string) types.ServiceConfig {
		return types.ServiceConfig{
			Name: "web", Image: image,
			Environment: types.MappingWithEquals{"API_KEY": str(key), "MODE": str("prod"), "PASSTHROUGH": nil},
			Command:     types.ShellCommand{"/bin/run", "--pw=" + pw},
			Entrypoint:  types.ShellCommand{"/entry", "--tok=" + pw},
			HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD-SHELL", "curl -u me:" + pw + " http://x"}},
			Logging:     &types.LoggingConfig{Driver: "syslog", Options: map[string]string{"syslog-address": "tcp://" + pw}},
			Ports:       []types.ServicePortConfig{{Target: 80, Published: "9090"}},
		}
	}
	curRaw, err := stacks.ServiceJSON(svc("old-secret-1a2b", "hunter2-old", "nginx:1.28"))
	if err != nil {
		t.Fatal(err)
	}
	nextRaw, err := stacks.ServiceJSON(svc("new-secret-3c4d", "hunter2-new", "nginx:1.27"))
	if err != nil {
		t.Fatal(err)
	}
	in := []stacks.ServiceChange{{Name: "web", Short: "web", Change: stacks.ChangeChanged, Current: curRaw, Next: nextRaw, Settings: 6}}

	out := stackops.RedactChanges(in, stacks.TargetStandalone)

	redactAssertNoSecret(t, "the standalone change", out[0].Current, out[0].Next)
	for side, raw := range map[string]json.RawMessage{"current": out[0].Current, "next": out[0].Next} {
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			t.Fatal(err)
		}
		env, _ := tree["environment"].(map[string]any)
		if env["API_KEY"] != redact.Placeholder || env["MODE"] != redact.Placeholder {
			t.Errorf("%s environment = %v, want every value withheld", side, env)
		}
		if v, ok := env["PASSTHROUGH"]; !ok || v != nil {
			t.Errorf("%s environment = %v, want a variable with no value kept as it is", side, env)
		}
		for _, key := range []string{"command", "entrypoint"} {
			if got, _ := tree[key].([]any); len(got) != 1 || got[0] != redact.Placeholder {
				t.Errorf("%s %s = %v, want one placeholder", side, key, tree[key])
			}
		}
		hc, _ := tree["healthcheck"].(map[string]any)
		if got, _ := hc["test"].([]any); len(got) != 2 || got[0] != "CMD-SHELL" || got[1] != redact.Placeholder {
			t.Errorf("%s healthcheck test = %v, want the mode kept and the command withheld", side, hc["test"])
		}
		logging, _ := tree["logging"].(map[string]any)
		if opts, _ := logging["options"].(map[string]any); opts["syslog-address"] != redact.Placeholder {
			t.Errorf("%s logging options = %v, want them withheld", side, logging["options"])
		}
		if !strings.Contains(string(raw), `"published":"9090"`) {
			t.Errorf("%s ports were touched: %s", side, raw)
		}
	}
	wantHidden := []string{"command", "entrypoint", "environment.API_KEY", "healthcheck.test", "logging.options.syslog-address"}
	if !slices.Equal(out[0].HiddenChanges, wantHidden) {
		t.Errorf("hidden changes = %v, want %v", out[0].HiddenChanges, wantHidden)
	}
}

func TestRedactChangesDoesNotListASettingThatStillDiffers(t *testing.T) {
	t.Parallel()
	// The health check mode differs, so the redacted lists still differ and nothing is hidden about it.
	cur := redactSwarmSpec(nil, nil, nil, []string{"CMD", "x"}, "a", "img")
	next := redactSwarmSpec(nil, nil, nil, []string{"CMD-SHELL", "y"}, "a", "img")
	out := stackops.RedactChanges([]stacks.ServiceChange{{Name: "s_web", Short: "web", Change: stacks.ChangeChanged, Current: redactMarshal(t, cur), Next: redactMarshal(t, next), Settings: 2}}, stacks.TargetSwarm)
	for _, p := range out[0].HiddenChanges {
		if p == "TaskTemplate.ContainerSpec.Healthcheck.Test" {
			t.Errorf("hidden changes = %v, the mode differs in the open so the path is not hidden", out[0].HiddenChanges)
		}
	}
}
