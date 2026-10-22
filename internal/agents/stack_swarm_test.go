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
	// env_file is read from the stack's own file, and an entry of environment overrides the file's (CR-05).
	{file: "parity-envfile.yaml", files: []stackFile{
		{Path: "app.env", Content: "FROM_FILE=1\nMODE=parity\nGREETING=from-file\n"},
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

// sharedStackUpdateImages shows the explicit image update on a real swarm with a real registry (D-16,
// STK-09, the prohibition: Update images must not change the digest of a service the user did not
// choose). It deploys two services from the registry, moves both tags, then reviews and deploys an
// update of web alone through the API: web runs the new digest of its tag, app still runs the one it
// ran, none of app's tasks is replaced and the registry is asked about web alone. An update of a
// service the stack does not have is refused before anything is looked up or started.
func sharedStackUpdateImages(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	refWeb, refApp := addr+"/upd/web:1", addr+"/upd/app:1"
	webA := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, refWeb, "A", nil)
	appA := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, refApp, "A", nil)
	compose := "services:\n  web:\n    image: " + refWeb + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n    deploy:\n      replicas: 1\n" +
		"  app:\n    image: " + refApp + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n    deploy:\n      replicas: 1\n"
	id := f.createStackBayplan(t, "upd1", compose)
	f.deployStackBayplan(t, id)
	const web, app = "upd1_web", "upd1_app"
	image := func(svc string) string {
		return serviceField(t, f, svc, "{{.Spec.TaskTemplate.ContainerSpec.Image}}")
	}
	if got := image(web); got != refWeb+"@"+webA {
		t.Fatalf("web runs %q, want %s@%s", got, refWeb, webA)
	}
	if got := image(app); got != refApp+"@"+appA {
		t.Fatalf("app runs %q, want %s@%s", got, refApp, appA)
	}
	f.waitTasksOn(t, web, webA, 1, taskWait)
	f.waitTasksOn(t, app, appA, 1, taskWait)
	appTasks := serviceTaskIDs(t, f, app)

	// Both tags move; only web is chosen.
	webB := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, refWeb, "B", nil)
	appB := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, refApp, "B", nil)
	if webB == webA || appB == appA {
		t.Fatalf("a tag did not move: web %s -> %s, app %s -> %s", webA, webB, appA, appB)
	}

	// A name the stack does not have is refused, and so is no name at all.
	for _, services := range [][]string{{"web", "ghost"}, {}} {
		if code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/review", map[string]any{"mode": "update", "services": services}); code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "services_invalid") {
			t.Errorf("POST the review of an update of %v = %d %s, want 422 services_invalid", services, code, body)
		}
	}

	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/review", map[string]any{"mode": "update", "services": []string{"web"}})
	var rv struct {
		DraftVersion int64 `json:"draftVersion"`
		Images       []struct {
			Service, Digest, Current, State string
		} `json:"images"`
		Pins map[string]string `json:"pins"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of the update = %d %s", code, body)
	}
	for _, r := range rv.Images {
		switch r.Service {
		case "web":
			if r.State != "new" || r.Current != webA || r.Digest != webB {
				t.Errorf("the review's row for web = %+v, want new, from %s to %s", r, webA, webB)
			}
		case "app":
			if r.State != "pinned" || r.Current != appA || r.Digest != appA {
				t.Errorf("the review's row for app = %+v, want it kept at %s although its tag moved to %s", r, appA, appB)
			}
		}
	}

	code, body = f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/deploys", map[string]any{
		"draftVersion": rv.DraftVersion, "mode": "update", "services": []string{"web"}, "pins": rv.Pins,
	})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the update of stack %s = %d %s", id, code, body)
	}
	var job stackJob
	testsupport.Poll(t, stackDeployWait, "the update job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+id+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = stackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the update of stack %s ended %s, want done: %s", id, job.State, raw)
	}

	// web moved to the digest its tag has now; app is exactly as it was and none of its tasks was replaced.
	if got := image(web); got != refWeb+"@"+webB {
		t.Errorf("web runs %q after the update, want %s@%s", got, refWeb, webB)
	}
	if got := image(app); got != refApp+"@"+appA {
		t.Errorf("app runs %q after the update, want %s@%s: it was not chosen", got, refApp, appA)
	}
	f.waitTasksOn(t, web, webB, 1, taskWait)
	if got := serviceTaskIDs(t, f, app); !slices.Equal(got, appTasks) {
		t.Errorf("the update replaced a task of app, which was not chosen:\n before: %v\n after:  %v", appTasks, got)
	}
	waitStackConverged(t, f, "upd1")
}

// serviceTaskIDs are the IDs of every task a service has had, current and past, sorted.
func serviceTaskIDs(t *testing.T, f *swarmFix, service string) []string {
	t.Helper()
	ids := strings.Fields(sh(t, f.sw.Managers[0], "docker service ps -q --no-trunc "+service))
	sort.Strings(ids)
	return ids
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

// runStackOperation posts a stop or a start of the stack, waits until its job ends and fails the test
// unless every step is done. It returns whether the start created services again.
func (f *swarmFix) runStackOperation(t *testing.T, stackID, op string) (recreates bool) {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+stackID+"/"+op, nil)
	var started struct {
		JobID     string `json:"jobId"`
		Recreates bool   `json:"recreates"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the %s of stack %s = %d %s", op, stackID, code, body)
	}
	var job stackJob
	testsupport.Poll(t, stackDeployWait, "the "+op+" job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = stackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the %s of stack %s ended %s, want done: %s", op, stackID, job.State, raw)
	}
	return started.Recreates
}

// sharedStackLifecycle shows Stop and Start on a real swarm keep what a stack runs (STK-11, D-19, the
// prohibition: Start never changes an image digest): a stack of two replicated services (2 and 1 replicas) and
// a global one is deployed, stopped through the API (every task gone, the global service removed, the counts
// remembered on the services) and started again (the services back at the same counts, the global one made
// again from the last revision), and the digest and the replica count of every service are the same as before.
// The image is named by tag, so the deploy pins it and a start that looked a tag up again would show.
func sharedStackLifecycle(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	ref := addr + "/life/app:1"
	dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, ref, nil)
	const stack = "life"
	t.Cleanup(func() { removeStackCLI(t, f, stack) })

	service := func(mode string) string {
		return "    image: " + ref + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n    deploy:\n" + mode
	}
	// Removal (below) needs things to keep and things to delete: web mounts a volume of the stack, an external
	// volume and joins an external network, and api mounts a second volume of the stack. The external network
	// is made first, as Docker requires of any stack that names one.
	const external, externalVolume = "lifeext", "lifeextvol"
	sh(t, mgr, "docker network create --driver overlay --attachable "+external)
	t.Cleanup(func() { removeNetwork(t, f, external) })
	mounts := func(vols ...string) string {
		out := "    volumes:\n"
		for _, v := range vols {
			out += "      - " + v + "\n"
		}
		return out
	}
	compose := "services:\n  web:\n" + service("      replicas: 2\n") + mounts("data:/data", "extvol:/ext") + "    networks: [back, ext]\n" +
		"  api:\n" + service("      replicas: 1\n") + mounts("keep:/keep") +
		"  mon:\n" + service("      mode: global\n") +
		"networks:\n  back: {}\n  ext:\n    external: true\n    name: " + external + "\n" +
		"volumes:\n  data: {}\n  keep: {}\n  extvol:\n    external: true\n    name: " + externalVolume + "\n"
	id := f.createStackBayplan(t, stack, compose)
	f.deployStackBayplan(t, id)
	waitStackConverged(t, f, stack)

	type shape struct{ image, replicas string }
	read := func(name string) shape {
		return shape{
			image:    serviceField(t, f, name, "{{.Spec.TaskTemplate.ContainerSpec.Image}}"),
			replicas: serviceField(t, f, name, "{{if .Spec.Mode.Replicated}}{{.Spec.Mode.Replicated.Replicas}}{{end}}"),
		}
	}
	before := map[string]shape{}
	for _, n := range []string{"life_web", "life_api", "life_mon"} {
		before[n] = read(n)
		if !strings.Contains(before[n].image, "@sha256:") {
			t.Fatalf("%s runs %q, want the image pinned to a digest by the deploy", n, before[n].image)
		}
	}
	if before["life_web"].replicas != "2" || before["life_api"].replicas != "1" {
		t.Fatalf("replicas before = %+v, want 2 and 1", before)
	}

	if f.runStackOperation(t, id, "stop") {
		t.Errorf("a stop answered that it recreates services")
	}
	testsupport.Poll(t, taskWait, "every task of the stopped stack to be gone", func() (bool, string) {
		out, err := mgr.Sh(execCtx(t, time.Minute), "docker service ls --filter label=com.docker.stack.namespace="+stack+" --format '{{.Name}} {{.Replicas}}'")
		if err != nil {
			return false, err.Error()
		}
		rows := strings.Fields(strings.ReplaceAll(strings.TrimSpace(out), "\n", " "))
		return slices.Equal(rows, []string{"life_api", "0/0", "life_web", "0/0"}) || slices.Equal(rows, []string{"life_web", "0/0", "life_api", "0/0"}), strings.TrimSpace(out)
	})
	for n, want := range map[string]string{"life_web": "2", "life_api": "1"} {
		if got := serviceField(t, f, n, `{{index .Spec.Labels "bayplan.stack.stopped-replicas"}}`); got != want {
			t.Errorf("%s remembers %q after a stop, want %s", n, got, want)
		}
	}

	if !f.runStackOperation(t, id, "start") {
		t.Errorf("a start after a stop that removed the global service answered that it creates nothing")
	}
	waitStackConverged(t, f, stack)
	for n, was := range before {
		got := read(n)
		if got != was {
			t.Errorf("%s after a stop and a start: %+v, want exactly what it was, %+v", n, got, was)
		}
		if label := serviceField(t, f, n, `{{index .Spec.Labels "bayplan.stack.stopped-replicas"}}`); label != "" && label != "<no value>" {
			t.Errorf("%s still carries the stopped-replicas label %q after a start", n, label)
		}
	}

	removeLifecycleStack(t, f, id, stack, external, externalVolume)
}

// removalPreview is the part of the removal preview these tests read.
type removalPreview struct {
	Services []string        `json:"services"`
	Networks []string        `json:"networks"`
	Volumes  []removalVolume `json:"volumes"`
}

type removalVolume struct {
	Name     string `json:"name"`
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
}

// removeLifecycleStack removes the stack of sharedStackLifecycle through the API and shows what a removal
// deletes and what it keeps on a real swarm (STK-11, D-20, Pitfall 19): the services go, the wait for their
// tasks ends, the stack's own network goes, a volume the person ticked is deleted on every node that holds it,
// the one they did not tick stays, and the external network and the external volume are not touched.
func removeLifecycleStack(t *testing.T, f *swarmFix, id, stack, external, externalVolume string) {
	t.Helper()
	// What the removal would delete is read from the engine: the stack's own volumes carry its labels, the
	// external one does not, so only the two the stack made are offered.
	code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+id+"/removal-preview", nil)
	var preview removalPreview
	if code != http.StatusOK || json.Unmarshal(body, &preview) != nil {
		t.Fatalf("GET the removal preview = %d %s", code, body)
	}
	if want := []string{"life_api", "life_mon", "life_web"}; !slices.Equal(preview.Services, want) {
		t.Errorf("preview services = %v, want %v", preview.Services, want)
	}
	for _, n := range preview.Networks {
		if n == external {
			t.Errorf("preview networks = %v, the external network %s is not the stack's to remove", preview.Networks, external)
		}
	}
	var ticked []map[string]string
	keptOn := map[string]bool{}
	for _, v := range preview.Volumes {
		switch v.Name {
		case stack + "_data":
			ticked = append(ticked, map[string]string{"name": v.Name, "nodeId": v.NodeID})
		case stack + "_keep":
			keptOn[v.NodeName] = true
		default:
			t.Errorf("preview offers the volume %s on %s, want only the stack's own data and keep volumes", v.Name, v.NodeName)
		}
	}
	if len(ticked) == 0 || len(keptOn) == 0 {
		t.Fatalf("preview volumes = %+v, want the stack's data and keep volumes on the nodes that ran them", preview.Volumes)
	}

	code, body = f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/remove", map[string]any{"confirmName": stack, "volumes": ticked, "keepDefinition": false})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the removal = %d %s", code, body)
	}
	var job stackJob
	testsupport.Poll(t, stackDeployWait, "the removal job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+id+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = stackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the removal ended %s, want done: %s", job.State, raw)
	}

	mgr := f.sw.Managers[0]
	if out := strings.TrimSpace(sh(t, mgr, "docker service ls -q --filter label=com.docker.stack.namespace="+stack)); out != "" {
		t.Errorf("services of the removed stack = %q, want none", out)
	}
	if out := strings.TrimSpace(sh(t, mgr, "docker network ls -q --filter label=com.docker.stack.namespace="+stack)); out != "" {
		t.Errorf("networks of the removed stack = %q, want none: a removal leaves nothing behind", out)
	}
	for _, n := range f.sw.Nodes() {
		if out := strings.TrimSpace(sh(t, n, "docker ps -aq --filter label=com.docker.stack.namespace="+stack)); out != "" {
			t.Errorf("containers of the removed stack on a node = %q, want none", out)
		}
		if out := strings.TrimSpace(sh(t, n, "docker volume ls -q --filter name=^"+stack+"_data$")); out != "" {
			t.Errorf("the ticked volume %s_data is still on a node: %q", stack, out)
		}
	}
	if ok, err := networkNamed(execCtx(t, time.Minute), mgr, external); err != nil || !ok {
		t.Errorf("the external network %s was removed with the stack (%v)", external, err)
	}
	keptSomewhere, externalSomewhere := false, false
	for _, n := range f.sw.Nodes() {
		t.Cleanup(func() {
			_, _ = n.Sh(execCtx(t, time.Minute), "docker volume rm "+stack+"_keep "+externalVolume+" 2>&1")
		})
		if strings.TrimSpace(sh(t, n, "docker volume ls -q --filter name=^"+stack+"_keep$")) != "" {
			keptSomewhere = true
		}
		if strings.TrimSpace(sh(t, n, "docker volume ls -q --filter name=^"+externalVolume+"$")) != "" {
			externalSomewhere = true
		}
	}
	if !keptSomewhere {
		t.Errorf("the unticked volume %s_keep was removed with the stack", stack)
	}
	if !externalSomewhere {
		t.Errorf("the external volume %s was removed with the stack", externalVolume)
	}
	if code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+id, nil); code != http.StatusNotFound {
		t.Errorf("GET the removed stack = %d %s, want 404", code, body)
	}
	rows := f.srv.AuditRows("stack.removed")
	if len(rows) != 1 {
		t.Errorf("stack.removed audit rows = %+v, want one", rows)
	}
}
