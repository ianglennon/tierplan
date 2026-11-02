// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// PrepareStandalone (plan 07-20, STK-02, STK-09; RESEARCH Pitfalls 4 and 5): the project the Compose SDK is
// handed holds no file of the server, no tag nobody decided on, and nothing that differs between two
// deploys of the same stack.

const standaloneCompose = `services:
  app:
    image: nginx:1.27-alpine
    secrets: [tok, envsec]
    configs: [conf, inline]
    networks: [back]
    volumes:
      - data:/data
  db:
    image: postgres:16
secrets:
  tok:
    file: tok.txt
  envsec:
    environment: API_KEY
  unused:
    file: nothing.txt
configs:
  conf:
    file: ./conf.txt
  inline:
    content: "listen 80;"
networks:
  back: {}
  shared:
    external: true
volumes:
  data: {}
  old:
    external: true
`

func standaloneFiles() stacks.FileSet {
	return stacks.NewFileSet([]stacks.File{
		{Path: "tok.txt", Content: []byte("file-secret")},
		{Path: "conf.txt", Content: []byte("file-config")},
	})
}

func prepared(t *testing.T) *types.Project {
	t.Helper()
	p := loadProject(t, standaloneCompose)
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, p, standaloneFiles(), map[string]string{"API_KEY": "from-table"})
	if err != nil {
		t.Fatalf("ResolveObjectsFor: %v", err)
	}
	pins := map[string]string{"app": "nginx:1.27-alpine@sha256:" + hex64('a'), "db": "postgres:16@sha256:" + hex64('b')}
	if err := stacks.PrepareStandalone(p, "stack-1", pins, objs); err != nil {
		t.Fatalf("PrepareStandalone: %v", err)
	}
	return p
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func TestPrepareStandaloneSendsThePinAndRecordsTheTextAsWritten(t *testing.T) {
	t.Parallel()
	p := prepared(t)
	app := p.Services["app"]
	if want := "nginx:1.27-alpine@sha256:" + hex64('a'); app.Image != want {
		t.Errorf("image = %q, want the pin %q", app.Image, want)
	}
	if got := app.CustomLabels[stacks.LabelStackImage]; got != "nginx:1.27-alpine" {
		t.Errorf("%s = %q, want the image as written", stacks.LabelStackImage, got)
	}
	if got := app.CustomLabels[stacks.LabelStackID]; got != "stack-1" {
		t.Errorf("%s = %q, want the stack's ID", stacks.LabelStackID, got)
	}
	// The labels the SDK's own loader would have set, so its containers are found by project and service.
	for key, want := range map[string]string{
		stacks.LabelComposeProject: "web", stacks.LabelComposeService: "app", stacks.LabelComposeOneoff: "False",
		stacks.LabelComposeWorkingDir: stacks.NeutralWorkingDir, stacks.LabelComposeConfigFiles: stacks.NeutralConfigFiles,
	} {
		if got := app.CustomLabels[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestPrepareStandaloneTurnsEverySecretIntoAnEnvironmentValueAndEveryConfigIntoContent(t *testing.T) {
	t.Parallel()
	p := prepared(t)
	for name, s := range p.Secrets {
		if s.File != "" || s.Content != "" {
			t.Errorf("secret %s keeps a file or content: %+v", name, s)
		}
	}
	for name, c := range p.Configs {
		if c.File != "" || c.Environment != "" {
			t.Errorf("config %s keeps a file or an environment variable: %+v", name, c)
		}
	}
	if got := p.Secrets["tok"].Environment; got != stacks.SecretEnvPrefix+"tok" {
		t.Errorf("secret tok environment = %q", got)
	}
	if p.Environment[stacks.SecretEnvPrefix+"tok"] != "file-secret" || p.Environment[stacks.SecretEnvPrefix+"envsec"] != "from-table" {
		t.Errorf("the project environment does not hold the secret values: %v", p.Environment)
	}
	if p.Configs["conf"].Content != "file-config" || p.Configs["inline"].Content != "listen 80;" {
		t.Errorf("configs = %+v", p.Configs)
	}
	if _, ok := p.Secrets["unused"]; ok {
		t.Error("a secret no service uses was kept")
	}
}

func TestPrepareStandalonePutsTheContentHashOfEachSecretAndConfigOnTheService(t *testing.T) {
	t.Parallel()
	p := prepared(t)
	labels := p.Services["app"].Labels
	if got, want := labels[stacks.LabelSecretPrefix+"tok"], stacks.ObjectHash("tok", []byte("file-secret")); got == "" || got != want {
		t.Errorf("secret tok hash label = %q, want %q", got, want)
	}
	if got, want := labels[stacks.LabelConfigPrefix+"conf"], stacks.ObjectHash("conf", []byte("file-config")); got == "" || got != want {
		t.Errorf("config conf hash label = %q, want %q", got, want)
	}
	if len(p.Services["db"].Labels) != 0 {
		t.Errorf("a service that uses no secret or config got labels: %v", p.Services["db"].Labels)
	}
}

func TestPrepareStandaloneMarksTheNetworksAndVolumesTheProjectCreates(t *testing.T) {
	t.Parallel()
	p := prepared(t)
	if got := p.Networks["back"].CustomLabels[stacks.LabelStackID]; got != "stack-1" {
		t.Errorf("network back is labelled %q, want the stack's ID", got)
	}
	if got := p.Volumes["data"].CustomLabels[stacks.LabelStackID]; got != "stack-1" {
		t.Errorf("volume data is labelled %q, want the stack's ID", got)
	}
	if _, ok := p.Networks["shared"].CustomLabels[stacks.LabelStackID]; ok {
		t.Error("an external network was labelled as the stack's")
	}
	if _, ok := p.Volumes["old"].CustomLabels[stacks.LabelStackID]; ok {
		t.Error("an external volume was labelled as the stack's")
	}
}

func TestPrepareStandaloneIsTheSameTwiceSoARedeployRecreatesNothing(t *testing.T) {
	t.Parallel()
	a, b := prepared(t), prepared(t)
	for _, name := range []string{"app", "db"} {
		sa, sb := a.Services[name], b.Services[name]
		if sa.Image != sb.Image || len(sa.Labels) != len(sb.Labels) || len(sa.CustomLabels) != len(sb.CustomLabels) {
			t.Errorf("service %s differs between two preparations:\n%+v\n%+v", name, sa, sb)
		}
		for k, v := range sa.CustomLabels {
			if sb.CustomLabels[k] != v {
				t.Errorf("service %s label %s = %q then %q", name, k, v, sb.CustomLabels[k])
			}
		}
	}
}

func TestPrepareStandaloneRefusesAServiceWithNoPin(t *testing.T) {
	t.Parallel()
	p := loadProject(t, "services:\n  web:\n    image: nginx:1.27\n")
	err := stacks.PrepareStandalone(p, "stack-1", map[string]string{}, nil)
	if !errors.Is(err, stacks.ErrPinMissing) {
		t.Fatalf("err = %v, want ErrPinMissing", err)
	}
	if p.Services["web"].Image != "nginx:1.27" {
		t.Errorf("a refused preparation changed the image to %q", p.Services["web"].Image)
	}
}

func TestPrepareStandaloneRefusesAnExternalSecretOrConfig(t *testing.T) {
	t.Parallel()
	for name, compose := range map[string]string{ //nolint:gosec // compose files for a test, no credential
		"external secret": "services:\n  web:\n    image: nginx:1.27\n    secrets: [tok]\nsecrets:\n  tok:\n    external: true\n",
		"external config": "services:\n  web:\n    image: nginx:1.27\n    configs: [c]\nconfigs:\n  c:\n    external: true\n",
	} {
		p := loadProject(t, compose)
		objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, p, standaloneFiles(), nil)
		if err != nil {
			t.Fatalf("%s: ResolveObjectsFor: %v", name, err)
		}
		err = stacks.PrepareStandalone(p, "stack-1", map[string]string{"web": "nginx:1.27@sha256:" + hex64('a')}, objs)
		if !errors.Is(err, stacks.ErrExternalFileObject) {
			t.Errorf("%s: err = %v, want ErrExternalFileObject", name, err)
		}
	}
}

func TestPrepareStandaloneRefusesUseAPISocketWhichWouldMountTheHostsDockerSocket(t *testing.T) {
	t.Parallel()
	p := loadProject(t, "services:\n  web:\n    image: nginx:1.27\n    use_api_socket: true\n")
	err := stacks.PrepareStandalone(p, "stack-1", map[string]string{"web": "nginx:1.27@sha256:" + hex64('a')}, nil)
	if !errors.Is(err, stacks.ErrUseAPISocket) {
		t.Fatalf("err = %v, want ErrUseAPISocket", err)
	}
}

func TestPrepareStandaloneRefusesWhatTheSDKWouldRunOnTheServerOrThroughExec(t *testing.T) {
	t.Parallel()
	pins := map[string]string{"web": "nginx:1.27@sha256:" + hex64('a')}
	for name, compose := range map[string]string{
		"a provider":       "services:\n  web:\n    image: nginx:1.27\n    provider:\n      type: some-plugin\n",
		"a lifecycle hook": "services:\n  web:\n    image: nginx:1.27\n    post_start:\n      - command: echo hi\n",
		"a model":          "services:\n  web:\n    image: nginx:1.27\n    models: [llm]\nmodels:\n  llm:\n    model: ai/smollm2\n",
	} {
		p := loadProject(t, compose)
		if err := stacks.PrepareStandalone(p, "stack-1", pins, nil); !errors.Is(err, stacks.ErrUnsupportedKey) {
			t.Errorf("%s: err = %v, want ErrUnsupportedKey", name, err)
		}
	}
}

func TestPrepareStandaloneRefusesWhatWouldReadTheServer(t *testing.T) {
	t.Parallel()
	pins := map[string]string{"web": "nginx:1.27@sha256:" + hex64('a')}
	t.Run("a relative bind mount", func(t *testing.T) {
		t.Parallel()
		p := loadProject(t, "services:\n  web:\n    image: nginx:1.27\n    volumes:\n      - ./data:/data\n")
		if err := stacks.PrepareStandalone(p, "stack-1", pins, nil); !errors.Is(err, stacks.ErrNotSandboxed) {
			t.Errorf("err = %v, want ErrNotSandboxed", err)
		}
	})
	t.Run("a build", func(t *testing.T) {
		t.Parallel()
		p := loadProject(t, "services:\n  web:\n    image: nginx:1.27\n    build: .\n")
		if err := stacks.PrepareStandalone(p, "stack-1", pins, nil); !errors.Is(err, stacks.ErrNotSandboxed) {
			t.Errorf("err = %v, want ErrNotSandboxed", err)
		}
	})
}

func TestResolveObjectsForAStandaloneHostTakesWhatTheArchiveRouteTakes(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27\n    secrets: [tok]\nsecrets:\n  tok:\n    file: tok.txt\n"
	big := stacks.NewFileSet([]stacks.File{{Path: "tok.txt", Content: make([]byte, 600000)}})
	for i := range big["tok.txt"].Content {
		big["tok.txt"].Content[i] = byte('a' + i%26)
	}
	p := loadProject(t, compose)
	if _, err := stacks.ResolveObjectsFor(stacks.TargetSwarm, p, big, nil); !errors.Is(err, stacks.ErrObjectTooLarge) {
		t.Errorf("a 600,000 byte secret on a swarm: %v, want ErrObjectTooLarge (500 KB)", err)
	}
	if _, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, p, big, nil); err != nil {
		t.Errorf("a 600,000 byte secret on a standalone host: %v, want it accepted (1,000 KB)", err)
	}
	over := stacks.NewFileSet([]stacks.File{{Path: "tok.txt", Content: make([]byte, stacks.MaxStandaloneFileBytes+1)}})
	for i := range over["tok.txt"].Content {
		over["tok.txt"].Content[i] = 'x'
	}
	_, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, p, over, nil)
	var oe *stacks.ObjectError
	if !errors.Is(err, stacks.ErrObjectTooLarge) || !errors.As(err, &oe) || oe.Target != stacks.TargetStandalone {
		t.Errorf("a secret over 1,000,000 bytes on a standalone host: %v, want ErrObjectTooLarge for the standalone target", err)
	}
}

func TestCheckFlagsAConfigFileOverWhatAStandaloneHostWritesAtItsKey(t *testing.T) {
	t.Parallel()
	// A config and not a secret file: the masker of a secret file of a megabyte takes seconds (deferred
	// items, 07-18), and the check reads a secret and a config with the same loop.
	const compose = "services:\n  web:\n    image: nginx:1.27\n    configs: [conf]\nconfigs:\n  conf:\n    file: conf.txt\n"
	check := func(target stacks.Target, size int) stacks.Problems {
		content := make([]byte, size)
		for i := range content {
			content[i] = byte('a' + i%26)
		}
		copy := stacks.WorkingCopy{Files: []stacks.File{
			{Path: stacks.ComposeFile, Content: []byte(compose)}, {Path: "conf.txt", Content: content},
		}}
		return stacks.Check(context.Background(), stacks.CheckInput{Name: "web", Target: target, Copy: copy}).Problems
	}
	var found []stacks.Problem
	for _, p := range check(stacks.TargetStandalone, stacks.MaxStandaloneFileBytes+1) {
		if p.Code == stacks.CodeObjectTooLarge {
			found = append(found, p)
		}
	}
	if len(found) != 1 || found[0].Severity != stacks.SeverityError || found[0].Key != "configs.conf" || found[0].Line != 6 || !strings.Contains(found[0].Message, "1,000 KB") {
		t.Errorf("problems = %+v, want one blocking error at configs.conf (line 6) naming 1,000 KB", found)
	}
	// 1,000,000 bytes is the most the archive route takes; a swarm judges its own limit elsewhere.
	for _, tc := range []struct {
		target stacks.Target
		size   int
	}{{stacks.TargetStandalone, stacks.MaxStandaloneFileBytes}, {stacks.TargetSwarm, stacks.MaxStandaloneFileBytes + 1}} {
		for _, p := range check(tc.target, tc.size) {
			if p.Code == stacks.CodeObjectTooLarge {
				t.Errorf("%s with %d bytes flagged: %+v", tc.target, tc.size, p)
			}
		}
	}
}

// RecreateCascade (plan 07-22, STK-06): the services the Compose SDK's reconciler carries along when it
// recreates one (v5.6.0 parentNamespaceRecreated and planStopDependents), so the review can name them.

const cascadeCompose = `services:
  web:
    image: nginx:1.27-alpine
  sidecar:
    image: nginx:1.27-alpine
    network_mode: service:web
  agent:
    image: nginx:1.27-alpine
    ipc: service:web
  watcher:
    image: nginx:1.27-alpine
    pid: service:web
  mounts:
    image: nginx:1.27-alpine
    volumes_from: [web]
  nested:
    image: nginx:1.27-alpine
    network_mode: service:sidecar
  restarter:
    image: nginx:1.27-alpine
    depends_on:
      web:
        condition: service_started
        restart: true
  restarter_of_sidecar:
    image: nginx:1.27-alpine
    depends_on:
      sidecar:
        condition: service_started
        restart: true
  both:
    image: nginx:1.27-alpine
    network_mode: service:web
    depends_on:
      web:
        condition: service_started
        restart: true
  worker:
    image: nginx:1.27-alpine
    depends_on: [web]
  other:
    image: nginx:1.27-alpine
  idle:
    image: nginx:1.27-alpine
    network_mode: service:web
`

func cascadeRows(t *testing.T, recreated []string, notRunning ...string) []string {
	t.Helper()
	p := loadProject(t, cascadeCompose)
	running := map[string]bool{}
	for name := range p.Services {
		running[name] = !slices.Contains(notRunning, name)
	}
	var out []string
	for _, c := range stacks.RecreateCascade(p, recreated, running) {
		if c.Name != c.Short || c.Current != nil || c.Next != nil || c.Settings != 0 {
			t.Errorf("row %+v carries a name that differs from its short name, a spec or a count of settings", c)
		}
		out = append(out, c.Change+" "+c.Name+" because "+c.Because)
	}
	return out
}

func TestRecreateCascadeCarriesWhatSharesTheRecreatedServicesNamespacesAndVolumes(t *testing.T) {
	t.Parallel()
	got := cascadeRows(t, []string{"web"}, "idle")
	want := []string{
		// recreated because web shares nothing of its own: they share web's network, IPC, PID or volumes
		"recreated agent because web", "recreated both because web", "recreated mounts because web",
		"recreated nested because sidecar", "recreated sidecar because web", "recreated watcher because web",
		// restarted: depends_on web with restart: true, and the one whose parent was carried along
		"restarted restarter because web", "restarted restarter_of_sidecar because sidecar",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestRecreateCascadeLeavesAPlainDependsOnAndEveryOtherServiceAlone(t *testing.T) {
	t.Parallel()
	for _, row := range cascadeRows(t, []string{"web"}) {
		for _, left := range []string{" worker ", " other "} {
			if strings.Contains(row, left) {
				t.Errorf("row %q: a service with a plain depends_on, or none of web's, keeps its container", row)
			}
		}
	}
}

func TestRecreateCascadeRecreatesAServiceThatAlsoRestartsOnceAndNeverRestartsIt(t *testing.T) {
	t.Parallel()
	got := cascadeRows(t, []string{"web"})
	recreated, restarted := 0, 0
	for _, row := range got {
		if strings.Contains(row, " both ") {
			if strings.HasPrefix(row, "recreated") {
				recreated++
			} else {
				restarted++
			}
		}
	}
	if recreated != 1 || restarted != 0 {
		t.Errorf("both: recreated %d times, restarted %d times, want recreated once and not restarted: %v", recreated, restarted, got)
	}
}

func TestRecreateCascadeGivesNothingForAServiceWithNoContainer(t *testing.T) {
	t.Parallel()
	for _, row := range cascadeRows(t, []string{"web"}, "idle", "restarter", "mounts") {
		for _, none := range []string{" idle ", " restarter ", " mounts "} {
			if strings.Contains(row, none) {
				t.Errorf("row %q: a service with no container is neither recreated nor restarted", row)
			}
		}
	}
}

func TestRecreateCascadeStartsFromTheServicesRecreatedAndNotFromWeb(t *testing.T) {
	t.Parallel()
	if got := cascadeRows(t, nil); len(got) != 0 {
		t.Errorf("nothing recreated: rows = %v, want none", got)
	}
	// Only the sidecar is recreated (its own change): what shares its namespace follows, web does not.
	got := cascadeRows(t, []string{"sidecar"})
	want := []string{"recreated nested because sidecar", "restarted restarter_of_sidecar because sidecar"}
	if !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}
