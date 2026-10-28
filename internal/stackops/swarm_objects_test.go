// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// The secrets and configs of a swarm deploy, against the fake manager.

// ownedLabels are the labels the secret a stack made from a file carries.
func ownedLabels(stack, id, object, content string) map[string]string {
	return map[string]string{
		"com.docker.stack.namespace": stack, "bayplan.stack.id": id,
		"bayplan.stack.object": object, "bayplan.stack.object.hash": stacks.ObjectHash(object, []byte(content)),
	}
}

func hashedName(stack, object, content string) string {
	name, _ := stacks.ObjectName(stack, object, []byte(content))
	return name
}

func countWrites(writes []string, prefix string) int {
	n := 0
	for _, w := range writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

const configCompose = `services:
  web:
    image: nginx:1.27-alpine
    configs:
      - source: conf
        target: /etc/nginx/conf.d/site.conf
configs:
  conf:
    content: "listen 80;"
    labels:
      team: web
`

func TestDeploySwarmCreatesAHashedConfigFromInlineContent(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", configCompose, nil)
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	name, hash := stacks.ObjectName("web", "conf", []byte("listen 80;"))
	creates := rig.fake.ConfigCreates()
	if len(creates) != 1 || creates[0].Name != name || string(creates[0].Data) != "listen 80;" {
		t.Fatalf("config creates = %+v, want one named %s", creates, name)
	}
	labels := creates[0].Labels
	if labels["bayplan.stack.object"] != "conf" || labels["bayplan.stack.object.hash"] != hash ||
		labels["com.docker.stack.namespace"] != "web" || labels["bayplan.stack.id"] != "stack-web" || labels["team"] != "web" {
		t.Errorf("config labels = %v, want the four stack labels over the file's own", labels)
	}
	refs := rig.fake.ServiceCreates()[0].Spec.TaskTemplate.ContainerSpec.Configs
	if len(refs) != 1 || refs[0].ConfigName != name || refs[0].ConfigID == "" {
		t.Errorf("config references = %+v", refs)
	}
}

func TestSecretContentNeverReachesAJobItemOrALog(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	var logs lockedBuffer
	rig.deps.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const secret = "correct-horse-battery-staple"
	_, in := stackInput(t, "web", tokCompose, tokFile(secret))
	res, job := rig.deploy(t, in)
	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) || strings.Contains(logs.String(), secret) {
		t.Error("the secret's content appears in the job or in the log")
	}
	// A failed deploy must not echo it either.
	rig2 := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.WriteError = "engine said: " + secret })
	rig2.deps.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, in2 := stackInput(t, "web", tokCompose, tokFile(secret))
	res2, job2 := rig2.deploy(t, in2)
	if res2.OK() {
		t.Fatal("a deploy against a failing engine succeeded")
	}
	for _, it := range job2.Items {
		if strings.Contains(it.Message, secret) {
			t.Errorf("item %s message %q holds the engine's words", it.ID, it.Message)
		}
	}
}

func TestExternalSecretIsReferencedAndNeverCreatedChangedOrRemoved(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    secrets: [shared]
secrets:
  shared:
    external: true
    name: org_shared_token
`
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "extsec", Name: "org_shared_token"}}
	})
	_, in := stackInput(t, "web", compose, nil)
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	if n := countWrites(rig.fake.Writes(), "POST /secrets") + countWrites(rig.fake.Writes(), "DELETE /secrets"); n != 0 {
		t.Errorf("writes = %v, want none to secrets", rig.fake.Writes())
	}
	refs := rig.fake.ServiceCreates()[0].Spec.TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 || refs[0].SecretID != "extsec" || refs[0].SecretName != "org_shared_token" {
		t.Errorf("secret references = %+v, want the external secret by its ID and name", refs)
	}
	// A redeploy with the secret still external removes nothing either.
	if res2, _ := rig.deploy(t, in); !res2.OK() || len(res2.RemovedObjects) != 0 {
		t.Errorf("redeploy = %+v", res2)
	}
	if n := countWrites(rig.fake.Writes(), "DELETE /secrets"); n != 0 {
		t.Errorf("writes = %v, an external secret must never be removed", rig.fake.Writes())
	}
}

func TestExternalSecretMustExistOrTheDeployStopsAtTheObjectsStepNamingIt(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    secrets: [shared]
configs:
  site:
    content: "x"
secrets:
  shared:
    external: true
`
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", compose, nil)
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeExternalMissing || res.FailedService != "shared" {
		t.Fatalf("result = %+v, want external_missing naming shared", res)
	}
	got := itemStates(job)
	if got[stackops.ItemObjects] != "failed:external_missing" || got[stackops.ItemServices] != "skipped:not_run" || got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
	if n := len(rig.fake.ServiceCreates()) + len(rig.fake.SecretCreates()); n != 0 {
		t.Errorf("a service or secret was created before the external secret was found missing")
	}
}

func TestADeployWithAnUnreadableSecretWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", tokCompose, nil) // tok.txt is not a file of the stack
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeObjectInvalid || res.FailedService != "tok" {
		t.Fatalf("result = %+v, want object_invalid naming tok", res)
	}
	if got := itemStates(job); got[stackops.ItemCheck] != "failed:object_invalid" {
		t.Errorf("items = %v", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestANameAnotherOwnerHoldsIsNotReusedOrReplaced(t *testing.T) {
	t.Parallel()
	name := hashedName("web", "tok", "s3cret")
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "foreign", Name: name}} // same name, no labels
	})
	_, in := stackInput(t, "web", tokCompose, tokFile("s3cret"))
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeObjectNameTaken || res.FailedService != name {
		t.Fatalf("result = %+v, want object_name_taken naming %s", res, name)
	}
	if got := itemStates(job); got[stackops.ItemObjects] != "failed:object_name_taken" {
		t.Errorf("items = %v", got)
	}
	// The deploy takes back the network it had made; it never touches the secret that is not its own.
	if countWrites(rig.fake.Writes(), "DELETE /secrets") != 0 {
		t.Errorf("writes = %v, nothing of another owner may be removed", rig.fake.Writes())
	}
	if !slices.Contains(rig.secretNames(t), name) {
		t.Errorf("secrets = %v, the other owner's secret %s is gone", rig.secretNames(t), name)
	}
}

func TestAReplacedSecretIsKeptWhenAServiceUpdateFails(t *testing.T) {
	t.Parallel()
	oldName := hashedName("web", "tok", "one")
	owned := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "oldsec", Name: oldName, Labels: ownedLabels("web", "stack-web", "tok", "one")}}
	}, dockertest.FakeService{
		ID: "svc-web", Name: "web_web", Mode: "replicated", Image: "nginx:1.26", Version: 3, Labels: owned,
		Secrets: []dockertest.FakeServiceObject{{ID: "oldsec", Name: oldName}}, UpdateError: "the swarm rejected the update",
	})
	_, in := stackInput(t, "web", tokCompose, tokFile("two"))
	res, job := rig.deploy(t, in)

	if res.OK() || res.FailedService != "web_web" {
		t.Fatalf("result = %+v, want the service update to fail", res)
	}
	if got := itemStates(job); got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("items = %v, the cleanup must not run after a failed update", got)
	}
	// The new secret was made for this deploy and nothing uses it, so it goes again; the old one stays
	// because the service that was not updated still uses it.
	if w := rig.fake.Writes(); countWrites(w, "DELETE /secrets/oldsec") != 0 {
		t.Errorf("writes = %v, the replaced secret must stay while a service may still use it", w)
	}
	got := rig.secretNames(t)
	if !slices.Contains(got, oldName) || len(got) != 1 {
		t.Errorf("secrets = %v, want only the old secret %s: the new one was made by this deploy and nothing uses it", got, oldName)
	}
}

func TestAReplacedSecretAServiceOfAnotherOwnerUsesIsLeftInPlace(t *testing.T) {
	t.Parallel()
	oldName := hashedName("web", "tok", "one")
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "oldsec", Name: oldName, Labels: ownedLabels("web", "stack-web", "tok", "one")}}
	}, dockertest.FakeService{
		ID: "svc-other", Name: "other", Mode: "replicated", Image: "busybox", Version: 1,
		Secrets: []dockertest.FakeServiceObject{{ID: "oldsec", Name: oldName}},
	})
	_, in := stackInput(t, "web", tokCompose, tokFile("two"))
	res, job := rig.deploy(t, in)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	if !slices.Equal(res.LeftInPlace, []string{oldName}) || len(res.RemovedObjects) != 0 {
		t.Errorf("left %v removed %v, want %s left in place", res.LeftInPlace, res.RemovedObjects, oldName)
	}
}

func TestAnObjectTheStackDidNotMakeFromAFileIsNeverRemoved(t *testing.T) {
	t.Parallel()
	// Carries the stack's ID but not the object label: a person put it there.
	mine := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "byhand", Name: "web_added_by_hand", Labels: mine}}
	})
	_, in := stackInput(t, "web", tokCompose, tokFile("one"))
	res, _ := rig.deploy(t, in)
	if !res.OK() || len(res.RemovedObjects) != 0 || len(res.LeftInPlace) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if n := countWrites(rig.fake.Writes(), "DELETE"); n != 0 {
		t.Errorf("writes = %v, want no remove", rig.fake.Writes())
	}
}

// lockedBuffer is a log sink that is safe to write from the deploy's goroutine and read from the
// test's.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestEnsureNetworksOwnsARecordedNetwork: a network an adopted stack recorded when it was adopted is the stack's
// whatever bayplan.stack.id it carries, so the deploy neither creates it nor answers network_conflict; a network of
// the same name that a stack Bayplan holds does not own is a conflict (D-18, T-08-61).
func TestEnsureNetworksOwnsARecordedNetwork(t *testing.T) {
	t.Parallel()
	held := func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{
			ID: "net-1", Name: "tracer_default", Driver: "overlay", Scope: "swarm",
			Labels: map[string]string{stacks.LabelSwarmNamespace: "tracer", stacks.LabelStackID: "someone-else"},
		}}
	}

	// Recorded at adoption: the deploy uses the network as it is.
	rig := newSwarmRig(t, held)
	st, in := starterPlan(t, "tracer")
	st.Source = stacks.Source{Kind: stacks.SourceAdopted, Ref: "tracer"}
	st.Adoption = stacks.Adoption{Networks: []string{"tracer_default"}, Pending: true}
	in.Stack = st
	res, _ := rig.deploy(t, in)
	if !res.OK() || res.Networks != 0 {
		t.Fatalf("deploy = %+v, want it to use the recorded network as it is", res)
	}
	if got := writesNamed(rig.fake, "/networks"); len(got) != 0 {
		t.Errorf("the deploy wrote to networks: %v", got)
	}

	// Not recorded: a network of another owner under the stack's namespace is a clash.
	other := newSwarmRig(t, held)
	_, plain := starterPlan(t, "tracer")
	if res, _ := other.deploy(t, plain); res.Code != stackops.CodeNameClash {
		t.Fatalf("deploy = %+v, want name_clash for a network of another owner", res)
	}
}
