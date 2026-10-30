// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rebuild_test

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/stackops/stackopstest"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/rebuild"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// baseOf is the base of a deployed revision: its file loaded as every deploy loads it.
func baseOf(t *testing.T, compose string, vars ...stacks.Var) (*rebuild.Base, stacks.WorkingCopy) {
	t.Helper()
	copy := stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: stacks.ComposeFile, Content: []byte(compose)},
			{Path: "secrets/db_password", Content: []byte("hunter2"), Secret: true},
		},
		Vars: vars,
	}
	loaded, err := stacks.Deployable(testsupport.Context(t), stacks.CheckInput{Name: "shop", Target: stacks.TargetSwarm, Copy: copy})
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	base := &rebuild.Base{Project: loaded.Project, SecretValues: map[string]string{}, ImageText: rebuild.ImageTextOf([]byte(compose))}
	for _, v := range vars {
		if v.Secret && v.Value != "" {
			base.SecretValues[v.Value] = v.Name
		}
	}
	return base, copy
}

// loadWith reads a rebuilt file the way a deploy does, with the variables of the stack.
func loadWith(t *testing.T, raw []byte, vars ...stacks.Var) *stacks.Loaded {
	t.Helper()
	loaded, err := stacks.Load(testsupport.Context(t), stacks.LoadInput{
		Name: "shop", Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: raw}}, Vars: vars},
	})
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, raw)
	}
	return loaded
}

const baseCompose = `services:
  web:
    image: nginx:${TAG}
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
      MODE: prod
    secrets:
      - db_password
      - shared_token
    configs:
      - site
    networks:
      - back
    volumes:
      - data:/var/lib/data
secrets:
  db_password:
    file: ./secrets/db_password
  shared_token:
    external: true
configs:
  site:
    content: "hello"
networks:
  back:
    driver: overlay
    attachable: true
volumes:
  data: {}
`

func objectLabels(key string) map[string]string {
	return map[string]string{
		"com.docker.stack.namespace": "shop", "coaming.stack.id": "stack-1", "coaming.stack.object": key, "coaming.stack.object.hash": "abc123",
	}
}

// liveWeb is the service as the swarm runs it after somebody scaled it to 3 outside the stack.
func liveWeb(image string, env ...string) rebuild.SwarmService {
	svc := coreService()
	cs := svc.Spec.TaskTemplate.ContainerSpec
	cs.Image = image + "@" + digest
	cs.Command, cs.Args = nil, nil
	cs.Env = env
	svc.Spec.Labels["com.docker.stack.image"] = image
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "shop_back"}}
	cs.Secrets = []*swarm.SecretReference{
		{SecretName: "shop_db_password_abc123", File: &swarm.SecretReferenceFileTarget{Name: "db_password", UID: "0", GID: "0", Mode: 0o444}},
		{SecretName: "shared_token", File: &swarm.SecretReferenceFileTarget{Name: "shared_token", UID: "0", GID: "0", Mode: 0o444}},
	}
	cs.Configs = []*swarm.ConfigReference{
		{ConfigName: "shop_site_abc123", File: &swarm.ConfigReferenceFileTarget{Name: "/site", UID: "0", GID: "0", Mode: 0o444}},
	}
	cs.Mounts = []mount.Mount{{
		Type: mount.TypeVolume, Source: "shop_data", Target: "/var/lib/data",
		VolumeOptions: &mount.VolumeOptions{Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
	}}
	return svc
}

func liveInput(base *rebuild.Base, svc rebuild.SwarmService) rebuild.SwarmInput {
	return rebuild.SwarmInput{
		Namespace: "shop", Services: []rebuild.SwarmService{svc}, Base: base,
		Networks: []rebuild.NetworkInfo{{
			ID: "n1", Name: "shop_back", Driver: "overlay", Attachable: true,
			Labels: map[string]string{"com.docker.stack.namespace": "shop"},
		}},
		Secrets: []rebuild.ObjectInfo{
			{ID: "s1", Name: "shop_db_password_abc123", Labels: objectLabels("db_password")},
			{ID: "s2", Name: "shared_token"},
		},
		Configs: []rebuild.ObjectInfo{{ID: "c1", Name: "shop_site_abc123", Labels: objectLabels("site")}},
	}
}

func TestRebuildWithBaseKeepsStackObjects(t *testing.T) {
	t.Parallel()
	base, _ := baseOf(t, baseCompose,
		stacks.Var{Name: "TAG", Value: "1.27"}, stacks.Var{Name: "DB_PASSWORD", Value: "s3cr3t", Secret: true})

	// Without a base the stack's own secret and config are objects it only uses.
	plain := liveInput(nil, liveWeb("nginx:1.27", "MODE=prod"))
	res, err := rebuild.Swarm(plain)
	if err != nil {
		t.Fatalf("Swarm without a base: %v", err)
	}
	if loaded := loadWith(t, res.YAML); !bool(loaded.Project.Secrets["db_password"].External) {
		t.Fatalf("without a base the stack's secret should read external (the contrast this test rests on):\n%s", res.YAML)
	}

	res, err = rebuild.Swarm(liveInput(base, liveWeb("nginx:1.27", "MODE=prod")))
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}
	text := string(res.YAML)
	for _, want := range []string{"file: ./secrets/db_password", "content: hello", "attachable: true", "data: {}"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"shop_db_password_abc123", "shop_site_abc123", "shop_back", "shop_data", "name: shop_"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the file has %q, the name the engine gave an object the stack made:\n%s", unwanted, text)
		}
	}
	p := loadWith(t, res.YAML, stacks.Var{Name: "TAG", Value: "1.27"}).Project
	if p.Secrets["db_password"].External || p.Secrets["db_password"].File == "" {
		t.Errorf("db_password = %+v, want the stack's own file", p.Secrets["db_password"])
	}
	if !bool(p.Secrets["shared_token"].External) {
		t.Errorf("shared_token = %+v, want an external secret the stack only uses", p.Secrets["shared_token"])
	}
	if p.Configs["site"].External || p.Configs["site"].Content != "hello" {
		t.Errorf("site = %+v, want the stack's own config", p.Configs["site"])
	}
	kinds := map[string]string{}
	for _, o := range res.Objects {
		kinds[o.Kind+"/"+o.Name] = o.Treatment
	}
	if kinds["secret/shop_db_password_abc123"] != rebuild.TreatmentOwned || kinds["secret/shared_token"] != rebuild.TreatmentExternal {
		t.Errorf("objects = %v, want the stack's secret owned and the shared one external", kinds)
	}
}

func TestRebuildWithBaseWritesSecretVariablesByName(t *testing.T) {
	t.Parallel()
	vars := []stacks.Var{{Name: "TAG", Value: "1.27"}, {Name: "DB_PASSWORD", Value: "s3cr3t", Secret: true}}
	base, _ := baseOf(t, baseCompose, vars...)
	svc := liveWeb("nginx:1.27", "DB_PASSWORD=s3cr3t", "MODE=prod", "NOTE=has $dollar")
	svc.Spec.TaskTemplate.ContainerSpec.Args = []string{"serve", "s3cr3t"}

	res, err := rebuild.Swarm(liveInput(base, svc))
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}
	text := string(res.YAML)
	if strings.Contains(text, "s3cr3t") {
		t.Fatalf("the secret variable's value is in the file:\n%s", text)
	}
	for _, want := range []string{"DB_PASSWORD: ${DB_PASSWORD}", "MODE: prod", "NOTE: has $$dollar", "${DB_PASSWORD}\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lacks %q:\n%s", want, text)
		}
	}
	web := loadWith(t, res.YAML, vars...).Project.Services["web"]
	if v := web.Environment["DB_PASSWORD"]; v == nil || *v != "s3cr3t" {
		t.Errorf("DB_PASSWORD loads as %v, want the variable's value back", v)
	}
	if len(web.Command) != 2 || web.Command[1] != "s3cr3t" {
		t.Errorf("command loads as %v, want the variable's value in the second word", web.Command)
	}
	if v := web.Environment["NOTE"]; v == nil || *v != "has $dollar" {
		t.Errorf("NOTE loads as %v, want a literal dollar sign", v)
	}
	// The project the result carries holds no placeholder either.
	for k, v := range res.Project.Services["web"].Environment {
		if v != nil && strings.Contains(*v, "SUBSTITUTION") {
			t.Errorf("%s = %q in the result's project", k, *v)
		}
	}
}

func TestRebuildWithBaseKeepsImageText(t *testing.T) {
	t.Parallel()
	vars := []stacks.Var{{Name: "TAG", Value: "1.27"}, {Name: "DB_PASSWORD", Value: "s3cr3t", Secret: true}}
	base, _ := baseOf(t, baseCompose, vars...)

	res, err := rebuild.Swarm(liveInput(base, liveWeb("nginx:1.27", "MODE=prod")))
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}
	if !strings.Contains(string(res.YAML), "image: nginx:${TAG}\n") {
		t.Errorf("the image lost the text it was written with:\n%s", res.YAML)
	}

	// An image somebody changed outside the stack is what runs, not what the revision said.
	res, err = rebuild.Swarm(liveInput(base, liveWeb("nginx:1.28", "MODE=prod")))
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}
	if !strings.Contains(string(res.YAML), "image: nginx:1.28\n") || strings.Contains(string(res.YAML), "TAG") {
		t.Errorf("a changed image should be kept as it runs:\n%s", res.YAML)
	}
}

func TestImageTextOfLeavesOutWhatIsNotOnePlainWord(t *testing.T) {
	t.Parallel()
	got := rebuild.ImageTextOf([]byte("services:\n  a:\n    image: nginx:${TAG:-1}\n  b:\n    image: \"x y\"\n  c:\n    build: .\n"))
	if got["a"] != "nginx:${TAG:-1}" || len(got) != 1 {
		t.Errorf("ImageTextOf = %v, want only a", got)
	}
}

func TestStandaloneRebuildWithBaseWritesSecretVariablesByName(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n    environment:\n      API_KEY: s3cr3t\n      MODE: prod\n")
	file := "services:\n  web:\n    image: nginx:1.27\n    environment:\n      API_KEY: ${API_KEY}\n      MODE: prod\n"
	vars := []stacks.Var{{Name: "API_KEY", Value: "s3cr3t", Secret: true}}
	loaded, err := stacks.Deployable(testsupport.Context(t), stacks.CheckInput{
		Name: "shop", Target: stacks.TargetStandalone,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(file)}}, Vars: vars},
	})
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	in := hostInputOf(t, h)
	in.Base = &rebuild.Base{Project: loaded.Project, SecretValues: map[string]string{"s3cr3t": "API_KEY"}, ImageText: rebuild.ImageTextOf([]byte(file))}

	res, err := rebuild.Standalone(in)
	if err != nil {
		t.Fatalf("Standalone: %v", err)
	}
	text := string(res.YAML)
	if strings.Contains(text, "s3cr3t") || !strings.Contains(text, "API_KEY: ${API_KEY}") || !strings.Contains(text, "MODE: prod") {
		t.Errorf("the file should name the secret variable and hold no secret value:\n%s", text)
	}
}
