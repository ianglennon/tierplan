// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	mobycontainer "github.com/moby/moby/api/types/container"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The standalone review against what is running (plan 07-22, STK-06, STK-09, D-11, Pitfall 5): each running
// container's inspect document is rebuilt as a compose service and compared field by field with the service
// the file prepares, so a change made on the host shows, and a review right after a deploy is empty.

const hostedCompose = `services:
  web:
    image: nginx:1.27-alpine
    ports:
      - "8080:80"
    restart: unless-stopped
    environment:
      MODE: prod
    depends_on: [db]
  db:
    image: postgres:16
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data: {}
`

// revisionList is a RevisionStore that holds revisions in memory.
type revisionList []stackops.StoredRevision

func (r revisionList) LatestRevisions(context.Context, string) ([]stackops.StoredRevision, error) {
	return r, nil
}

func TestAStandaloneReviewRightAfterADeployIsEmpty(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)

	rv := rig.reviewOf(t, "demo", hostedCompose, nil, nil)

	if len(rv.Services) != 0 || len(rv.Objects) != 0 {
		t.Errorf("services = %v objects = %v, want a review with nothing to do", changesOf(rv), objectsOf(rv))
	}
	if rv.Summary.Services != (stackops.Counts{}) {
		t.Errorf("summary = %+v", rv.Summary.Services)
	}
	if rv.Problems.Errors() != 0 {
		t.Errorf("problems = %v", rv.Problems)
	}
}

func TestStandaloneReviewShowsAnEditedPortAtThatPortAlone(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)
	edited := strings.Replace(hostedCompose, "8080:80", "9090:80", 1)

	rv := rig.reviewOf(t, "demo", edited, nil, nil)

	if got := changesOf(rv); !slices.Equal(got, []string{"changed web"}) {
		t.Fatalf("services = %v, want web changed", got)
	}
	web := rv.Services[0]
	if web.Settings != 1 {
		t.Errorf("settings = %d, want the published port alone", web.Settings)
	}
	if !strings.Contains(string(web.Current), `"published":"8080"`) || !strings.Contains(string(web.Next), `"published":"9090"`) {
		t.Errorf("current = %s\nnext = %s", web.Current, web.Next)
	}
	if len(rv.Images) != 2 || rv.Images[0].State != stackops.ImagePinned {
		t.Errorf("images = %+v, want the running digests kept (pinned)", rv.Images)
	}
	if rv.Summary.Services.Changed != 1 {
		t.Errorf("summary = %+v", rv.Summary.Services)
	}
}

func TestStandaloneReviewShowsAHandChangedContainer(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	// docker update --restart always: every compose label and the config hash stay.
	h.Docs["web"].HostConfig.RestartPolicy = mobycontainer.RestartPolicy{Name: "always"}
	rig := hostRig(t, h)

	rv := rig.reviewOf(t, "demo", hostedCompose, nil, nil)

	if got := changesOf(rv); !slices.Equal(got, []string{"changed web"}) {
		t.Fatalf("services = %v, want web changed though its hash matches", got)
	}
	web := rv.Services[0]
	if web.Settings != 1 || !strings.Contains(string(web.Current), `"restart":"always"`) || !strings.Contains(string(web.Next), `"restart":"unless-stopped"`) {
		t.Errorf("settings = %d\ncurrent = %s\nnext = %s", web.Settings, web.Current, web.Next)
	}
	if rv.Summary.Services.Changed != 1 {
		t.Errorf("summary = %+v", rv.Summary.Services)
	}
}

func TestStandaloneReviewHidesASecretVariableThatChangedAndNamesItsPath(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: ${API_KEY}\n      MODE: prod\n"
	oldVars := []stacks.Var{{Name: "API_KEY", Value: "oldsecret-value", Secret: true}}
	newVars := []stacks.Var{{Name: "API_KEY", Value: "newsecret-value", Secret: true}}
	h := hostStack(t, "demo", compose, oldVars)
	rig := hostRig(t, h)
	in := rig.standaloneReviewInput(t, "demo", compose, nil)
	in.Copy.Vars = newVars
	in.Previous = &stacks.WorkingCopy{Files: in.Copy.Files, Vars: oldVars}

	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}

	if len(rv.Services) != 1 || rv.Services[0].Change != "changed" {
		t.Fatalf("services = %v, want web changed: the secret changed", changesOf(rv))
	}
	if got := rv.Services[0].HiddenChanges; !slices.Equal(got, []string{"environment.API_KEY"}) {
		t.Errorf("hidden changes = %v", got)
	}
	for _, side := range []string{string(rv.Services[0].Current), string(rv.Services[0].Next)} {
		if strings.Contains(side, "secret-value") || !strings.Contains(side, `"API_KEY":"\u003chidden\u003e"`) || !strings.Contains(side, `"MODE":"prod"`) {
			t.Errorf("a side = %s, want API_KEY hidden and MODE shown", side)
		}
	}
}

func TestStandaloneReviewListsAServiceTheFileNoLongerDeclaresAsRemoved(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)
	withoutDB := "services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - \"8080:80\"\n    restart: unless-stopped\n    environment:\n      MODE: prod\n"

	rv := rig.reviewOf(t, "demo", withoutDB, nil, nil)

	if got := changesOf(rv); !slices.Equal(got, []string{"removed db"}) {
		t.Errorf("services = %v, want db removed alone", got)
	}
	if rv.Summary.Services.Removed != 1 || rv.Summary.Services.Changed != 0 {
		t.Errorf("summary = %+v", rv.Summary.Services)
	}
}

func TestStandaloneReviewReadsWhatTheEngineDoesNotRecordFromTheStoredRevision(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)
	// The file now also starts a cache web depends on, and moves the port: the hash differs.
	stored := revisionList{{Seq: 1, Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(hostedCompose)}}}, Pins: h.Pins}}
	with := rig.reviewOf(t, "demo", cacheAfter(hostedCompose), nil, stored)
	without := rig.reviewOf(t, "demo", cacheAfter(hostedCompose), nil, nil)

	web := func(rv stackops.Review) stacks.ServiceChange {
		for _, c := range rv.Services {
			if c.Short == "web" {
				return c
			}
		}
		t.Fatalf("no row for web in %v", changesOf(rv))
		return stacks.ServiceChange{}
	}
	if cur := string(web(with).Current); !strings.Contains(cur, `"depends_on":{"db"`) || strings.Contains(cur, "cache") {
		t.Errorf("with the stored revision the running side reads depends_on %s", cur)
	}
	if next := string(web(with).Next); !strings.Contains(next, `"cache"`) {
		t.Errorf("the file's side lacks the cache: %s", next)
	}
	// With no stored revision to read them from, the settings the engine does not record are taken to be
	// the file's own: the review does not invent a difference it cannot know.
	if cur := string(web(without).Current); !strings.Contains(cur, `"cache"`) {
		t.Errorf("without a stored revision the running side reads depends_on %s, want the file's own", cur)
	}
}

// cacheAfter is the compose file with a cache service web depends on, and web's port moved.
func cacheAfter(compose string) string {
	out := strings.Replace(compose, "8080:80", "9090:80", 1)
	out = strings.Replace(out, "depends_on: [db]", "depends_on: [db, cache]", 1)
	return strings.Replace(out, "volumes:\n  data: {}\n", "  cache:\n    image: redis:7\nvolumes:\n  data: {}\n", 1)
}

func TestStandaloneReviewOfAServiceThatSharesAnothersNamespaceAndVolumesIsEmptyRightAfterADeploy(t *testing.T) {
	t.Parallel()
	compose := `services:
  web:
    image: nginx:1.27-alpine
    volumes: [data:/data]
  sidecar:
    image: nginx:1.27-alpine
    network_mode: service:web
    volumes_from: [web]
  agent:
    image: nginx:1.27-alpine
    ipc: service:web
    pid: service:web
volumes:
  data: {}
`
	h := hostStack(t, "demo", compose, nil)
	rig := hostRig(t, h)

	rv := rig.reviewOf(t, "demo", compose, nil, nil)

	if len(rv.Services) != 0 {
		t.Errorf("services = %v, want none: a service that shares another's namespace is not changed by every deploy", changesOf(rv))
	}
}

func TestStandaloneReviewJudgesAServiceWhoseImageIsGoneByItsHashAlone(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	h.Images = nil // the containers' images are no longer on the host
	rig := hostRig(t, h)

	same := rig.reviewOf(t, "demo", hostedCompose, nil, nil)
	edited := rig.reviewOf(t, "demo", strings.Replace(hostedCompose, "8080:80", "9090:80", 1), nil, nil)

	if len(same.Services) != 0 {
		t.Errorf("an unedited file: services = %v, want none", changesOf(same))
	}
	if got := changesOf(edited); !slices.Equal(got, []string{"changed web"}) {
		t.Fatalf("an edited port: services = %v, want web changed by its hash", got)
	}
	if web := edited.Services[0]; web.Settings != 0 || len(web.Current) != 0 {
		t.Errorf("web = %+v, want no field-level difference: its image cannot be read", web)
	}
}

func TestStandaloneReviewNeverWritesAndAsksOnlyForInspectDocuments(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)

	rig.reviewOf(t, "demo", hostedCompose, nil, nil)

	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("a review wrote to the engine: %v", w)
	}
}
