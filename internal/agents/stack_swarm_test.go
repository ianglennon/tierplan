// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The stack suites (plan 07-11, STK-02, STK-09) run a stack deploy through Bayplan's API and a real
// agent on a swarm of three managers and a worker, and hold the result against what the engine does
// with the same compose file when the docker CLI deploys it.

// stackEnvReady installs the agents and waits until the swarm environment is online with all four.
func (f *swarmFix) stackEnvReady(t *testing.T) {
	t.Helper()
	f.install(t)
	f.waitSwarmOnline(t, 4)
}

// parityCase is one file of the parity corpus.
type parityCase struct {
	// file is the name under testdata/stacks.
	file string
	// files are written beside the compose file by both deploys.
	files []stackFile
	// external names a network the case makes first, as an admin would, for the file to name as
	// external (the value EXTERNAL_NETWORK).
	external string
}

// parityCorpus is every file the oracle deploys twice.
var parityCorpus = []parityCase{
	{file: "parity-basic.yaml"},
	{file: "parity-deploy.yaml"},
	{file: "parity-networks.yaml", external: "parity-ext"},
	{file: "parity-objects.yaml", files: []stackFile{
		{Path: "dbpass.txt", Content: "s3cret-db-password\n"},
		{Path: "token.txt", Content: "token-0123456789\n"},
		{Path: "app.conf", Content: "listen = 8080\nmode = parity\n"},
		{Path: "short.conf", Content: "short = yes\n"},
	}},
}

// sharedStackDeployParity is the differential oracle for the converter (STK-02, RESEARCH "Parity
// oracle", Pitfall 3). Each file of the corpus is deployed once by the real docker stack deploy
// --resolve-image=never on a manager and once by Bayplan, through the API and the agents, and what
// the engine holds (services, networks, secrets and configs) is compared after the normaliser. The
// image is named by digest, so neither deploy has a lookup to make and both write the same text. The
// two stacks run one after the other (the first is removed and gone before the second starts), so a
// published port or a subnet is the same in both. Each case leaves nothing of either stack behind.
//
// The agents are installed with the one setting a host owner adds to let services that give tasks
// host access through (BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES), because the corpus adds a
// capability (cap_add), which the default policy refuses on purpose; docker stack deploy has no such policy.
func sharedStackDeployParity(t *testing.T, f *swarmFix) {
	f.installPrivileged(t)
	f.waitSwarmOnline(t, 4)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/parity/app:1", nil)

	for i, c := range parityCorpus {
		// Bayplan keeps a stack's record after its services are gone, so each file has a stack name of its own.
		bayplanStack := fmt.Sprintf("%s%d", parityBayplanStack, i+1)
		t.Run(c.file, func(t *testing.T) {
			values := map[string]string{"IMAGE": addr + "/parity/app@" + digest, "EXTERNAL_NETWORK": c.external}
			if c.external != "" {
				sh(t, mgr, "docker network create --driver overlay --attachable "+c.external)
				t.Cleanup(func() { removeNetwork(t, f, c.external) })
			}
			// Whatever happens below, neither stack outlives the case.
			t.Cleanup(func() { removeStackCLI(t, f, bayplanStack) })
			t.Cleanup(func() { removeStackCLI(t, f, parityCLIStack) })
			src := paritySource{Compose: readCorpus(t, c.file, values), Files: c.files}
			askedPools := strings.Contains(src.Compose, "subnet:")

			if err := deployWithCLI(execCtx(t, 5*time.Minute), mgr, parityCLIStack, src); err != nil {
				t.Fatal(err)
			}
			cli, err := snapshotStack(execCtx(t, time.Minute), mgr, parityCLIStack, askedPools)
			if err != nil {
				t.Fatal(err)
			}
			removeStackCLI(t, f, parityCLIStack)

			deployWithBayplan(t, f, bayplanStack, src)
			bp, err := snapshotStack(execCtx(t, time.Minute), mgr, bayplanStack, askedPools)
			if err != nil {
				t.Fatal(err)
			}
			removeStackCLI(t, f, bayplanStack)

			compareSnapshots(t, cli, bp)
		})
	}
}

// idempotentCorpus are the corpus files a redeploy with no edit is proved on: the one with the
// most settings, the one with networks and the one with secrets and configs, which are the three
// places a converter can send something the engine reads as a change.
var idempotentCorpus = []parityCase{
	{file: "parity-deploy.yaml"},
	{file: "parity-networks.yaml", external: "idem-ext"},
	{file: "parity-objects.yaml", files: parityCorpus[3].files},
}

// sharedStackRedeployIdempotent deploys each file through Bayplan, waits for its tasks to run, deploys
// the same draft again with no edit and shows that no task of any service was replaced and no service
// spec moved (STK-09: an unchanged redeploy restarts nothing). The image is named by tag, so the
// first deploy pins it and the second keeps the pin through the lookup rules, not by luck of a file
// that already names a digest. The second deploy is followed by a service made after it and awaited:
// the swarm handles its events in order, so by the time that service runs the swarm has decided
// about the update, and a task it meant to replace would be listed.
func sharedStackRedeployIdempotent(t *testing.T, f *swarmFix) {
	f.installPrivileged(t)
	f.waitSwarmOnline(t, 4)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/idem/app:1", nil)

	for i, c := range idempotentCorpus {
		stack := fmt.Sprintf("idem%d", i+1)
		t.Run(c.file, func(t *testing.T) {
			values := map[string]string{"IMAGE": addr + "/idem/app:1", "EXTERNAL_NETWORK": c.external}
			if c.external != "" {
				sh(t, mgr, "docker network create --driver overlay --attachable "+c.external)
				t.Cleanup(func() { removeNetwork(t, f, c.external) })
			}
			t.Cleanup(func() { removeStackCLI(t, f, stack) })
			src := paritySource{Compose: readCorpus(t, c.file, values), Files: c.files}

			id := deployWithBayplan(t, f, stack, src)
			waitStackConverged(t, f, stack)
			tasksBefore := stackTaskIDs(t, f, stack)
			names, err := stackServiceNames(execCtx(t, time.Minute), mgr, stack)
			if err != nil {
				t.Fatal(err)
			}
			specsBefore := map[string]string{}
			for _, n := range names {
				specsBefore[n] = serviceField(t, f, n, "{{json .Spec}}")
			}
			if len(tasksBefore) == 0 {
				t.Fatalf("stack %s has no tasks after its deploy", stack)
			}

			f.deployStackBayplan(t, id)

			barrier := stack + "_barrier"
			sh(t, mgr, "docker service create --name "+barrier+" --detach --no-resolve-image --stop-grace-period 1s "+addr+"/idem/app:1 sleep 3600")
			testsupport.Poll(t, taskWait, "the service made after the redeploy to run", func() (bool, string) {
				out, err := mgr.Sh(execCtx(t, time.Minute), "docker service ls --filter name="+barrier+" --format '{{.Replicas}}'")
				return err == nil && strings.TrimSpace(out) == "1/1", strings.TrimSpace(out)
			})

			if got := stackTaskIDs(t, f, stack); !slices.Equal(got, tasksBefore) {
				t.Errorf("the redeploy with no edit changed the tasks:\n before: %v\n after:  %v", tasksBefore, got)
			}
			for _, n := range names {
				if got := serviceField(t, f, n, "{{json .Spec}}"); got != specsBefore[n] {
					t.Errorf("the redeploy with no edit changed the spec of %s:\n before: %s\n after:  %s", n, specsBefore[n], got)
				}
				if st := serviceField(t, f, n, "{{if .UpdateStatus}}{{.UpdateStatus.State}}{{end}}"); st != "" {
					t.Errorf("service %s has an update status %q after a redeploy with no edit", n, st)
				}
			}
			sh(t, mgr, "docker service rm "+barrier)
		})
	}
}

// sharedStackPin shows a pin holding on a real swarm with a real registry (STK-09, the prohibition:
// an unchanged redeploy never changes a running digest, even after the tag moved). It pushes app:1,
// deploys it, pushes another image to the same tag, redeploys with no edit (the service keeps the
// first digest and no task is replaced), then changes the reference to app:2 and deploys (the service
// moves to that image's digest). A last case runs an image no registry has and the nodes hold under a
// name of their own, which Bayplan pins to the image ID: the swarm has to take that as a service image.
func sharedStackPin(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	ref1, ref2 := addr+"/pin/app:1", addr+"/pin/app:2"
	composeOf := func(image string) string {
		return "services:\n  app:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n    deploy:\n      replicas: 1\n"
	}

	t.Run("TagMoves", func(t *testing.T) {
		digestA := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref1, "A", nil)
		id := f.createStackBayplan(t, "pin1", composeOf(ref1))
		f.deployStackBayplan(t, id)
		const svc = "pin1_app"
		wantImage := func(ref, digest string) {
			t.Helper()
			if got := serviceField(t, f, svc, "{{.Spec.TaskTemplate.ContainerSpec.Image}}"); got != ref+"@"+digest {
				t.Errorf("the service runs the image %q, want %s@%s", got, ref, digest)
			}
			if got := serviceField(t, f, svc, `{{index .Spec.Labels "com.docker.stack.image"}}`); got != ref {
				t.Errorf("the image label is %q, want the text as written, %s", got, ref)
			}
		}
		wantImage(ref1, digestA)
		f.waitTasksOn(t, svc, digestA, 1, taskWait)
		tasks := stackTaskIDs(t, f, "pin1")

		// The tag moves; the file does not.
		digestB := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref1, "B", nil)
		if digestB == digestA {
			t.Fatalf("the second push gave the same digest %s: the tag did not move", digestA)
		}
		f.deployStackBayplan(t, id)
		wantImage(ref1, digestA)
		f.waitTasksOn(t, svc, digestA, 1, taskWait)
		if got := stackTaskIDs(t, f, "pin1"); !slices.Equal(got, tasks) {
			t.Errorf("the redeploy after the tag moved replaced a task:\n before: %v\n after:  %v", tasks, got)
		}

		// The reference changes; the new text is looked up once and the service moves to it.
		digestC := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref2, "C", nil)
		f.saveStackBayplan(t, id, paritySource{Compose: composeOf(ref2)})
		f.deployStackBayplan(t, id)
		wantImage(ref2, digestC)
		f.waitTasksOn(t, svc, digestC, 1, taskWait)
	})

	t.Run("LocalOnlyImage", func(t *testing.T) {
		// The image is busybox under a name of its own, on every node, in a registry that does not have
		// the repository (and answers 404 manifest unknown for it, as Docker Hub answers 401 for a
		// repository that is not there). Bayplan then has no digest to pin and falls back to the image ID
		// (D-18, plan 07-09), if the engine says the image has no repository digest.
		local := addr + "/local/only:1"
		dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
		for _, n := range f.sw.Nodes() {
			sh(t, n, "docker tag "+dockertest.ServiceTestImage+" "+local)
		}
		imageID := strings.TrimSpace(sh(t, mgr, "docker image inspect --format '{{.Id}}' "+local))

		// What the swarm itself does with an image ID as a service image, whatever Bayplan decides: it
		// takes it, and the tasks run on nodes that hold the image.
		sh(t, mgr, "docker service create --name byid --detach --no-resolve-image --stop-grace-period 1s --replicas 2 "+imageID+" sleep 3600")
		testsupport.Poll(t, taskWait, "the service whose image is an image ID to run", func() (bool, string) {
			out, err := mgr.Sh(execCtx(t, time.Minute), "docker service ls --filter name=byid --format '{{.Replicas}}'")
			return err == nil && strings.TrimSpace(out) == "2/2", strings.TrimSpace(out)
		})
		if got := serviceField(t, f, "byid", "{{.Spec.TaskTemplate.ContainerSpec.Image}}"); got != imageID {
			t.Errorf("the swarm holds the image %q for a service created from %s", got, imageID)
		}
		sh(t, mgr, "docker service rm byid")

		// Bayplan's part needs an engine that says a never-pushed image has no repository digest. The
		// containerd image store (the default of a new Engine 29) lists name@digest for every image,
		// built here or not, so the rule cannot tell this image from one a registry lost, and the lookup
		// is refused as image_unresolved instead (reported in the 07-11 summary for a decision).
		for _, n := range f.sw.Nodes() {
			if digests := strings.TrimSpace(sh(t, n, "docker image inspect --format '{{.RepoDigests}}' "+local)); digests != "[]" {
				t.Skipf("%s lists repository digests %s for an image no registry ever gave it, so Bayplan cannot take it for a local image on this engine's image store", n.Hostname, digests)
			}
		}
		id := f.createStackBayplan(t, "pin2", composeOf(local))
		f.deployStackBayplan(t, id)
		const svc = "pin2_app"
		if got := serviceField(t, f, svc, "{{.Spec.TaskTemplate.ContainerSpec.Image}}"); got != imageID {
			t.Errorf("the service image is %q, want the local image ID %s", got, imageID)
		}
		if got := serviceField(t, f, svc, `{{index .Spec.Labels "com.docker.stack.image"}}`); got != local {
			t.Errorf("the image label is %q, want the text as written, %s", got, local)
		}
		waitStackConverged(t, f, "pin2")
	})
}

// reviewBody is the part of the answer of POST .../review these tests read.
type reviewBody struct {
	Problems []struct {
		Severity, Code, Message string
		Line                    int
	} `json:"problems"`
	Summary struct {
		Services, Networks, Volumes, Secrets, Configs struct{ Added, Changed, Removed int }
	} `json:"summary"`
	Services []struct {
		Name, Change  string
		Current, Next map[string]any
		HiddenChanges []string `json:"hiddenChanges"`
	} `json:"services"`
	Objects []struct {
		Change, Kind, Name, Detail string
	} `json:"objects"`
}

// reviewStackBayplan reviews the stack's saved draft through the API.
func (f *swarmFix) reviewStackBayplan(t *testing.T, stackID string) reviewBody {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+stackID+"/review", map[string]any{})
	var rv reviewBody
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of stack %s = %d %s", stackID, code, body)
	}
	return rv
}

// leafDiff lists the leaves of two decoded JSON values that differ, as "path: a != b", sorted. A
// leaf on one side only reads "(absent)" on the other.
func leafDiff(path string, a, b any, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, _ := b.(map[string]any)
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		for k := range keys {
			leafDiff(path+"."+k, av[k], bv[k], out)
		}
		return
	case []any:
		bv, _ := b.([]any)
		for i := 0; i < max(len(av), len(bv)); i++ {
			var x, y any
			if i < len(av) {
				x = av[i]
			}
			if i < len(bv) {
				y = bv[i]
			}
			leafDiff(fmt.Sprintf("%s[%d]", path, i), x, y, out)
		}
		return
	}
	if b != nil {
		switch b.(type) {
		case map[string]any, []any:
			leafDiff(path, nil, b, out)
			return
		}
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		show := func(v any) string {
			if v == nil {
				return "(absent)"
			}
			return fmt.Sprintf("%v", v)
		}
		*out = append(*out, fmt.Sprintf("%s: %s != %s", path, show(a), show(b)))
	}
	sort.Strings(*out)
}

// sharedStackReviewEmptyAfterDeploy is the idempotence oracle of the review (STK-06): each file of
// the parity corpus is deployed through Bayplan, and a review of the same draft right afterwards
// lists no service, network, secret, config or volume change. The engine stores a service with
// defaults of its own (a runtime, an isolation, a virtual IP endpoint, network IDs for names), and
// the review has to read the running spec and the one a deploy would send as the same thing. When
// a field differs the failure prints its path and both values, so the default can be added to
// stacks.NormalizeSpec with a row of its unit test. The image is named by tag, so the review also
// shows that a pinned image stays pinned.
func sharedStackReviewEmptyAfterDeploy(t *testing.T, f *swarmFix) {
	f.installPrivileged(t)
	f.waitSwarmOnline(t, 4)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/rev/app:1", nil)

	for i, c := range parityCorpus {
		stack := fmt.Sprintf("rev%d", i+1)
		t.Run(c.file, func(t *testing.T) {
			values := map[string]string{"IMAGE": addr + "/rev/app:1", "EXTERNAL_NETWORK": c.external}
			if c.external != "" {
				sh(t, mgr, "docker network create --driver overlay --attachable "+c.external)
				t.Cleanup(func() { removeNetwork(t, f, c.external) })
			}
			t.Cleanup(func() { removeStackCLI(t, f, stack) })
			src := paritySource{Compose: readCorpus(t, c.file, values), Files: c.files}
			id := deployWithBayplan(t, f, stack, src)

			rv := f.reviewStackBayplan(t, id)
			for _, p := range rv.Problems {
				if p.Severity == "error" {
					t.Errorf("the review of %s found an error: %s (%s, line %d)", c.file, p.Message, p.Code, p.Line)
				}
			}
			for _, sc := range rv.Services {
				var diffs []string
				leafDiff("", sc.Current, sc.Next, &diffs)
				t.Errorf("the review right after a deploy lists %s %s:\n  %s\n  hidden changes: %v", sc.Change, sc.Name, strings.Join(diffs, "\n  "), sc.HiddenChanges)
			}
			for _, o := range rv.Objects {
				if o.Change != "external" {
					t.Errorf("the review right after a deploy lists %s %s %s: %s", o.Change, o.Kind, o.Name, o.Detail)
				}
			}
			for kind, n := range map[string]struct{ Added, Changed, Removed int }{
				"services": rv.Summary.Services, "networks": rv.Summary.Networks, "volumes": rv.Summary.Volumes,
				"secrets": rv.Summary.Secrets, "configs": rv.Summary.Configs,
			} {
				if n.Added+n.Changed+n.Removed != 0 {
					t.Errorf("the summary right after a deploy counts %s %+v", kind, n)
				}
			}
		})
	}
}
