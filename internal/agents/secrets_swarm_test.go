// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// secretRefs are the names of the secrets a service's spec references, as "name>file" (the file the container sees).
func secretRefs(t *testing.T, f *swarmFix, service string) []string {
	t.Helper()
	out := sh(t, f.sw.Managers[0], "docker service inspect -f '{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{.SecretName}}>{{.File.Name}} {{end}}' "+service)
	return strings.Fields(out)
}

// secretNames are the names of the swarm's secrets other than the agents' join token.
func secretNames(t *testing.T, f *swarmFix) []string {
	t.Helper()
	var names []string
	for _, n := range strings.Fields(sh(t, f.sw.Managers[0], "docker secret ls --format '{{.Name}}'")) {
		if !strings.HasPrefix(n, "bayplan-agent-token") {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// secretInContainer is the content of a secret file in a running task of the service.
func secretInContainer(t *testing.T, f *swarmFix, service, file string) string {
	t.Helper()
	hosts := runningHosts(t, f, service)
	if len(hosts) == 0 {
		t.Fatalf("service %s has no running task to read %s from", service, file)
	}
	n := f.nodeNamed(t, hosts[0])
	return strings.TrimSpace(sh(t, n, "docker exec $(docker ps -q --filter name="+service+".) cat /run/secrets/"+file))
}

// TestSwarmSecretRotate rotates a secret that a standalone service and a stack's service use, on a real swarm (plan
// 06-37, RES-08, D-28, RESEARCH E16, E18): the standalone service ends on the new version of the secret with the same
// file and the new value inside its container, the stack's service is skipped and keeps the old one, so the old secret
// stays (object_in_use). Then the stack's service is moved onto the new version by hand and a second rotate, with
// another standalone user of the old secret, removes the old. The limits of the swarm hold on the real engine: a secret
// of 511,999 bytes and a config of 1,023,999 bytes are made through the routes and one byte more is refused.
func TestSwarmSecretRotate(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr := f.sw.Managers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	code, body := f.srv.Do(http.MethodPost, base+"/secrets", map[string]any{"name": "db_pass", "data": b64("value-one"), "labels": map[string]string{"app": "rotate"}})
	var old struct {
		ID, Name string
	}
	if code != http.StatusCreated || json.Unmarshal(body, &old) != nil || old.ID == "" {
		t.Fatalf("POST secrets = %d %s", code, body)
	}
	const secretFlag = " --secret source=db_pass,target=db_pass,mode=0440"
	app := sleepService(t, f, "rot-app", 1, secretFlag)
	stack := sleepService(t, f, "rot-stack_web", 1, secretFlag+" --label com.docker.stack.namespace=rot-stack")
	if got := secretInContainer(t, f, "rot-app", "db_pass"); got != "value-one" {
		t.Fatalf("the standalone service reads %q from its secret file, want value-one", got)
	}

	// The preview names both users and says which one a rotate would leave alone.
	code, body = f.srv.Do(http.MethodPost, base+"/secrets/"+old.ID+"/rotate/preview", map[string]any{})
	var preview struct {
		NewName string `json:"newName"`
		Users   []struct {
			Service string `json:"service"`
			Skipped bool   `json:"skipped"`
			Stack   string `json:"stack"`
			File    string `json:"file"`
			Mode    string `json:"mode"`
		} `json:"users"`
		Skipped int `json:"skipped"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &preview) != nil || preview.NewName != "db_pass_v2" || len(preview.Users) != 2 || preview.Skipped != 1 {
		t.Fatalf("POST rotate/preview = %d %s, want db_pass_v2 for two users, one of them skipped", code, body)
	}
	for _, u := range preview.Users {
		if u.Skipped != (u.Service == "rot-stack_web") || u.File != "db_pass" {
			t.Errorf("preview user %+v, want only the stack's service skipped and the file kept", u)
		}
	}

	// The first rotate: the standalone service moves, the stack's stays, the old secret stays.
	rotate := map[string]any{"name": "db_pass_v2", "data": b64("value-two"), "removeOld": true}
	code, body = f.srv.Do(http.MethodPost, base+"/secrets/"+old.ID+"/rotate", rotate)
	var rotated struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		OldName    string `json:"oldName"`
		Updated    int    `json:"updated"`
		Skipped    int    `json:"skipped"`
		OldRemoved bool   `json:"oldRemoved"`
		RemoveCode string `json:"removeCode"`
		Services   []struct {
			Service string `json:"service"`
			State   string `json:"state"`
			Stack   string `json:"stack"`
		} `json:"services"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rotated) != nil {
		t.Fatalf("POST rotate = %d %s", code, body)
	}
	if rotated.Name != "db_pass_v2" || rotated.OldName != "db_pass" || rotated.Updated != 1 || rotated.Skipped != 1 || rotated.OldRemoved || rotated.RemoveCode != "object_in_use" {
		t.Errorf("rotate answered %s, want one updated, one skipped, the old kept as object_in_use", body)
	}
	states := map[string]string{}
	for _, s := range rotated.Services {
		states[s.Service] = s.State
	}
	if states["rot-app"] != "updated" || states["rot-stack_web"] != "skipped" {
		t.Errorf("service states = %v, want rot-app updated and rot-stack_web skipped", states)
	}
	if got := secretRefs(t, f, "rot-app"); !slices.Equal(got, []string{"db_pass_v2>db_pass"}) {
		t.Errorf("rot-app references %v, want the new secret under the same file name", got)
	}
	if got := secretRefs(t, f, "rot-stack_web"); !slices.Equal(got, []string{"db_pass>db_pass"}) {
		t.Errorf("the stack's service references %v, want it left on the old secret", got)
	}
	if got := secretNames(t, f); !slices.Equal(got, []string{"db_pass", "db_pass_v2"}) {
		t.Errorf("secrets after the first rotate = %v, want both versions", got)
	}
	dockertest.WaitRunningTasks(t, mgr, app, 1, taskWait)
	dockertest.Wait(t, 2*time.Minute, "the standalone task to read the new value", func() (bool, string) {
		out, err := mgr.Sh(execCtx(t, time.Minute), "docker service ps --filter desired-state=running -q rot-app | wc -l")
		if err != nil || strings.TrimSpace(out) != "1" {
			return false, out
		}
		hosts := runningHosts(t, f, "rot-app")
		if len(hosts) != 1 {
			return false, strings.Join(hosts, ",")
		}
		got, err := f.nodeNamed(t, hosts[0]).Sh(execCtx(t, time.Minute), "docker exec $(docker ps -q --filter name=rot-app.) cat /run/secrets/db_pass")
		return err == nil && strings.TrimSpace(got) == "value-two", got
	})
	if got := secretInContainer(t, f, "rot-stack_web", "db_pass"); got != "value-one" {
		t.Errorf("the stack's service reads %q, want value-one: it was not touched", got)
	}
	if rows := f.srv.AuditRows("secret.rotated"); len(rows) != 1 || rows[0].Result != "success" {
		t.Errorf("secret.rotated rows = %+v, want one success", rows)
	}

	// The stack's service is moved onto the new version by hand, as a redeploy of the stack would, and another
	// standalone service still uses the old one. A second rotate moves it and now nothing names the old secret.
	sh(t, mgr, "docker service update --detach --secret-rm db_pass --secret-add source=db_pass_v2,target=db_pass,mode=0440 rot-stack_web")
	sleepService(t, f, "rot-app2", 1, secretFlag)
	code, body = f.srv.Do(http.MethodPost, base+"/secrets/"+old.ID+"/rotate", map[string]any{"data": b64("value-three"), "removeOld": true})
	rotated.Services = nil
	if code != http.StatusOK || json.Unmarshal(body, &rotated) != nil {
		t.Fatalf("POST second rotate = %d %s", code, body)
	}
	if rotated.Name != "db_pass_v3" || rotated.Updated != 1 || rotated.Skipped != 0 || !rotated.OldRemoved || rotated.RemoveCode != "" {
		t.Errorf("second rotate answered %s, want db_pass_v3, one updated, none skipped and the old removed", body)
	}
	if got := secretNames(t, f); !slices.Equal(got, []string{"db_pass_v2", "db_pass_v3"}) {
		t.Errorf("secrets after the second rotate = %v, want the old one gone", got)
	}
	if got := secretRefs(t, f, "rot-app2"); !slices.Equal(got, []string{"db_pass_v3>db_pass"}) {
		t.Errorf("rot-app2 references %v, want db_pass_v3", got)
	}
	if got := secretRefs(t, f, "rot-app"); !slices.Equal(got, []string{"db_pass_v2>db_pass"}) {
		t.Errorf("rot-app references %v after the second rotate, want it left on db_pass_v2", got)
	}
	if rows := f.srv.AuditRows("secret.rotated"); len(rows) != 2 {
		t.Errorf("secret.rotated rows = %d, want two", len(rows))
	}
	_ = stack

	// The limits of the swarm, through the routes and on the real engine (RESEARCH E18).
	big := func(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), n)) }
	if code, body = f.srv.Do(http.MethodPost, base+"/secrets", map[string]any{"name": "limit-secret", "data": big(511999)}); code != http.StatusCreated {
		t.Errorf("POST secrets of 511,999 bytes = %d %.200s, want 201", code, body)
	}
	if code, body = f.srv.Do(http.MethodPost, base+"/secrets", map[string]any{"name": "limit-secret-over", "data": big(512000)}); code != http.StatusUnprocessableEntity || problemType(body) != "value_too_large" {
		t.Errorf("POST secrets of 512,000 bytes = %d %.200s, want 422 value_too_large", code, body)
	}
	if code, body = f.srv.Do(http.MethodPost, base+"/configs", map[string]any{"name": "limit-config", "data": big(1023999)}); code != http.StatusCreated {
		t.Errorf("POST configs of 1,023,999 bytes = %d %.200s, want 201", code, body)
	}
	if code, body = f.srv.Do(http.MethodPost, base+"/configs", map[string]any{"name": "limit-config-over", "data": big(1024000)}); code != http.StatusUnprocessableEntity || problemType(body) != "value_too_large" {
		t.Errorf("POST configs of 1,024,000 bytes = %d %.200s, want 422 value_too_large", code, body)
	}
	if got := strings.TrimSpace(sh(t, mgr, "docker secret inspect -f '{{.Spec.Name}}' limit-secret")); got != "limit-secret" {
		t.Errorf("the engine does not hold the 511,999 byte secret: %q", got)
	}
	if got := strings.TrimSpace(sh(t, mgr, "docker config inspect -f '{{.Spec.Name}}' limit-config")); got != "limit-config" {
		t.Errorf("the engine does not hold the 1,023,999 byte config: %q", got)
	}
}
