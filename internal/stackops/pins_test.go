// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// ResolvePins decides which image each service is sent with and looks up only the ones that need
// a fresh digest (STK-09, D-15, D-18).

func plan(name, short, image string) convert.ServicePlan {
	return convert.ServicePlan{Name: name, Short: short, ImageAsWritten: image}
}

// running is a service the swarm has, running image and deployed from text.
func running(name, text, image string) dockerx.ServiceDetail {
	return dockerx.ServiceDetail{Name: name, Image: image, Labels: map[string]string{stacks.LabelSwarmImage: text}}
}

func TestResolvePinsKeepsARunningDigestAndLooksUpOnlyTheServiceThatNeedsOne(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("b")) // the tag has moved
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("c"))
	live := map[string]dockerx.ServiceDetail{"site_web": running("site_web", "nginx:1.27", "nginx:1.27@"+digestOf("a"))}

	out := rig.deps.ResolvePins(context.Background(), rig.envID,
		[]convert.ServicePlan{plan("site_cache", "cache", "redis:7"), plan("site_web", "web", "nginx:1.27")}, live, nil, nil)

	if out.Blocked() || len(out.Problems) != 0 {
		t.Fatalf("outcome = %+v, want no problem", out)
	}
	if got := out.Pins["site_web"]; got.Action != stacks.PinKeep || got.Ref != "nginx:1.27@"+digestOf("a") {
		t.Errorf("web pin = %+v, want the running digest kept", got)
	}
	if got := out.Pins["site_cache"]; got.Action != stacks.PinResolve || got.Ref != "redis:7@"+digestOf("c") {
		t.Errorf("cache pin = %+v, want a fresh digest", got)
	}
	if refs := rig.fake.DistributionRefs(); !slices.Equal(refs, []string{"docker.io/library/redis:7"}) {
		t.Errorf("lookups = %v, want only the service that needs one", refs)
	}
}

func TestResolvePinsLooksAReferenceUpOnceForSeveralServices(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	out := rig.deps.ResolvePins(context.Background(), rig.envID,
		[]convert.ServicePlan{plan("site_a", "a", "nginx:1.27"), plan("site_b", "b", "nginx:1.27")}, nil, nil, nil)
	if out.Blocked() {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Pins["site_a"].Ref != out.Pins["site_b"].Ref || out.Pins["site_a"].Ref != "nginx:1.27@"+digestOf("a") {
		t.Errorf("pins = %+v, want the same digest for both", out.Pins)
	}
	if n := len(rig.fake.DistributionRefs()); n != 1 {
		t.Errorf("lookups = %d, want one for one reference", n)
	}
}

func TestResolvePinsResolvesAServiceThatIsSelectedForAnUpdateEvenWhenUnchanged(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("b"))
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("c"))
	live := map[string]dockerx.ServiceDetail{
		"site_web":   running("site_web", "nginx:1.27", "nginx:1.27@"+digestOf("a")),
		"site_cache": running("site_cache", "redis:7", "redis:7@"+digestOf("d")),
	}
	out := rig.deps.ResolvePins(context.Background(), rig.envID,
		[]convert.ServicePlan{plan("site_cache", "cache", "redis:7"), plan("site_web", "web", "nginx:1.27")}, live, nil, map[string]bool{"web": true})
	if got := out.Pins["site_web"].Ref; got != "nginx:1.27@"+digestOf("b") {
		t.Errorf("web = %q, want the selected service moved to the newest digest", got)
	}
	if got := out.Pins["site_cache"].Ref; got != "redis:7@"+digestOf("d") {
		t.Errorf("cache = %q, want the service that was not selected to keep its digest", got)
	}
}

func TestResolvePinsUsesTheStoredPinOfAServiceTheSwarmLacksOnlyForTheSameText(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("c"))
	stored := map[string]stacks.LivePin{
		"site_web":   {AsWritten: "nginx:1.27", Image: "nginx:1.27@" + digestOf("a")},
		"site_cache": {AsWritten: "redis:6", Image: "redis:6@" + digestOf("d")},
	}
	out := rig.deps.ResolvePins(context.Background(), rig.envID,
		[]convert.ServicePlan{plan("site_cache", "cache", "redis:7"), plan("site_web", "web", "docker.io/library/nginx:1.27")}, nil, stored, nil)
	if got := out.Pins["site_web"]; got.Action != stacks.PinKeep || got.Ref != "nginx:1.27@"+digestOf("a") {
		t.Errorf("web = %+v, want the stored pin kept: docker.io/library/nginx:1.27 is the text nginx:1.27 names", got)
	}
	if got := out.Pins["site_cache"].Ref; got != "redis:7@"+digestOf("c") {
		t.Errorf("cache = %q, want a fresh digest for a changed text", got)
	}
	if refs := rig.fake.DistributionRefs(); !slices.Equal(refs, []string{"docker.io/library/redis:7"}) {
		t.Errorf("lookups = %v, want only the changed text", refs)
	}
}

func TestResolvePinsSendsTheCredentialChosenForTheImageAndKeepsItOutOfTheOutcome(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st := storetest.New(t)
	v, err := vault.New(make([]byte, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	creds := registry.NewStore(st, v, time.Now)
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	if _, err := creds.Create(context.Background(), registry.CreateInput{
		Name: "ghcr-acme", Type: registry.TypeGHCR, Prefix: "acme", Username: "bot", Secret: secret,
	}, "admin-1", nil); err != nil {
		t.Fatal(err)
	}
	rig.deps.Resolver = registry.NewResolver(creds)

	out := rig.deps.ResolvePins(context.Background(), rig.envID,
		[]convert.ServicePlan{plan("site_app", "app", "ghcr.io/acme/app:1"), plan("site_web", "web", "nginx:1.27")}, nil, nil, nil)
	if out.Blocked() {
		t.Fatalf("outcome = %+v", out)
	}
	auths, refs := rig.fake.DistributionAuth(), rig.fake.DistributionRefs()
	if len(auths) != 2 {
		t.Fatalf("lookups = %v %v, want two", refs, auths)
	}
	for i, ref := range refs {
		switch ref {
		case "ghcr.io/acme/app:1":
			raw, err := base64.URLEncoding.DecodeString(auths[i])
			var got struct{ Username, Password string }
			if err != nil || json.Unmarshal(raw, &got) != nil || got.Username != "bot" || got.Password != secret {
				t.Errorf("the lookup of %s was sent %q, want the ghcr credential", ref, auths[i])
			}
		default:
			if auths[i] != "" {
				t.Errorf("the lookup of %s was sent a credential, want it anonymous", ref)
			}
		}
	}
	// Nothing the outcome holds carries the header or the secret.
	dump := fmt.Sprintf("%+v", out)
	if strings.Contains(dump, secret) {
		t.Errorf("the outcome holds the secret: %s", dump)
	}
	for _, header := range auths {
		if header != "" && strings.Contains(dump, header) {
			t.Errorf("the outcome holds the registry header: %s", dump)
		}
	}
}

// What a lookup that fails or finds no registry image does (D-18).

const (
	hostApp       = "registry.example:5000/app:1"
	registryDown  = `Get "https://registry.example:5000/v2/": dial tcp: lookup registry.example on 127.0.0.11:53: no such host`
	localImageID  = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	localImageRef = "myapp:1"
)

func TestPinAnUnreachableRegistryBlocksOnlyTheServiceThatNeedsAFreshLookup(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionFailure(hostApp, 500, registryDown)
	live := map[string]dockerx.ServiceDetail{"site_web": running("site_web", "nginx:1.27", "nginx:1.27@"+digestOf("a"))}

	out := rig.deps.ResolvePins(context.Background(), rig.envID,
		[]convert.ServicePlan{plan("site_app", "app", hostApp), plan("site_web", "web", "nginx:1.27")}, live, nil, nil)

	if out.Code != stackops.CodeRegistryUnreachable || out.FailedService != "site_app" {
		t.Fatalf("outcome = %+v, want registry_unreachable at site_app", out)
	}
	if len(out.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", out.Problems)
	}
	p := out.Problems[0]
	want := "TierPlan can't reach registry.example:5000, and app needs a fresh lookup. Services that are already pinned don't need the registry."
	if p.Severity != stacks.SeverityError || p.Code != stackops.CodeRegistryUnreachable || p.Message != want || p.Key != "services.app.image" {
		t.Errorf("problem = %+v, want the blocking sentence for app", p)
	}
	if got := out.Pins["site_web"]; got.Action != stacks.PinKeep {
		t.Errorf("web pin = %+v, an already pinned service must still pass", got)
	}
	if _, ok := out.Pins["site_app"]; ok {
		t.Errorf("app was given a pin %+v though its lookup failed", out.Pins["site_app"])
	}
	if refs := rig.fake.DistributionRefs(); !slices.Equal(refs, []string{hostApp}) {
		t.Errorf("lookups = %v, the pinned service must not reach the registry", refs)
	}
}

func TestPinAnImageTheRegistryCannotGiveIsUnresolvedWithTheCheckYourNameSentence(t *testing.T) {
	t.Parallel()
	for status, message := range map[int]string{404: "manifest unknown", 401: "unauthorized: authentication required"} {
		rig := newSwarmRig(t, nil)
		rig.fake.SetDistributionFailure(hostApp, status, message)
		out := rig.deps.ResolvePins(context.Background(), rig.envID, []convert.ServicePlan{plan("site_app", "app", hostApp)}, nil, nil, nil)
		want := "Couldn't look up registry.example:5000/app:1 on registry.example:5000 to pin it to a digest. Check the name, the tag and the credential, then try again."
		if out.Code != stackops.CodeImageUnresolved || len(out.Problems) != 1 || out.Problems[0].Message != want {
			t.Errorf("status %d: outcome = %+v, want image_unresolved with %q", status, out, want)
		}
		if strings.Contains(fmt.Sprintf("%+v", out), message) {
			t.Errorf("status %d: the outcome repeats the registry's own words: %+v", status, out)
		}
	}
}

func TestPinALocalOnlyImagePinsToItsImageIDWithAWarning(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Images = []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}}}
	})
	rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "pull access denied for myapp, repository does not exist or may require authorization")

	out := rig.deps.ResolvePins(context.Background(), rig.envID, []convert.ServicePlan{plan("site_app", "app", localImageRef)}, nil, nil, nil)

	if out.Blocked() {
		t.Fatalf("outcome = %+v, a local image must not block", out)
	}
	pin := out.Pins["site_app"]
	if pin.Action != stacks.PinLocal || pin.Ref != localImageID {
		t.Errorf("pin = %+v, want the local image ID", pin)
	}
	wantNote := "“myapp:1” isn't from a registry, so TierPlan pins it to the local image ID."
	if len(out.Problems) != 1 || out.Problems[0].Severity != stacks.SeverityWarning || out.Problems[0].Code != stackops.CodeLocalImage || out.Problems[0].Message != wantNote {
		t.Errorf("problems = %+v, want the local-image warning %q", out.Problems, wantNote)
	}
	if pin.Note != wantNote {
		t.Errorf("note = %q", pin.Note)
	}
}

func TestPinAnImageWithARepositoryDigestIsNotTreatedAsLocalWhenTheRegistryLosesIt(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Images = []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}, RepoDigests: []string{"myapp@" + digestOf("f")}}}
	})
	rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "manifest unknown")
	out := rig.deps.ResolvePins(context.Background(), rig.envID, []convert.ServicePlan{plan("site_app", "app", localImageRef)}, nil, nil, nil)
	if out.Code != stackops.CodeImageUnresolved {
		t.Errorf("outcome = %+v, want image_unresolved: an image that came from a registry is not a local one", out)
	}
}

func TestPinAnUnreachableRegistryIsNeverRescuedByALocalImage(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Images = []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{hostApp}}}
	})
	rig.fake.SetDistributionFailure(hostApp, 500, registryDown)
	out := rig.deps.ResolvePins(context.Background(), rig.envID, []convert.ServicePlan{plan("site_app", "app", hostApp)}, nil, nil, nil)
	if out.Code != stackops.CodeRegistryUnreachable {
		t.Errorf("outcome = %+v, want registry_unreachable: a registry that is down says nothing about the image", out)
	}
}

// The standalone path of the pin decision (plan 07-22, D-17, D-18): the same table as a swarm, read through the
// host's own engine. An image no registry gave is pinned to its ID with a warning, and a registry that cannot
// be reached blocks only the services that need a fresh lookup.

func TestPinAStandaloneHostsLocalOnlyImagePinsToItsIDWithAWarning(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	rig.fake.Reconfigure(dockertest.FakeConfig{
		Images: []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}}},
	})
	rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "pull access denied for myapp, repository does not exist or may require authorization")

	rv := rig.standaloneReview(t, "demo", "services:\n  web:\n    image: "+localImageRef+"\n")

	if rv.Problems.Errors() != 0 {
		t.Fatalf("problems = %v, a local image must not block", rv.Problems)
	}
	if rv.Pins["web"] != localImageID {
		t.Errorf("pins = %v, want web pinned to the local image ID %s", rv.Pins, localImageID)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != stackops.ImageLocal {
		t.Errorf("images = %+v, want web local", rv.Images)
	}
	wantNote := "“myapp:1” isn't from a registry, so TierPlan pins it to the local image ID."
	found := false
	for _, p := range rv.Problems {
		found = found || (p.Severity == stacks.SeverityWarning && p.Code == stackops.CodeLocalImage && p.Message == wantNote)
	}
	if !found {
		t.Errorf("problems = %+v, want the local-image warning %q", rv.Problems, wantNote)
	}
}

func TestPinAStandaloneHostsImageThatCameFromARegistryIsNeverTakenForALocalOne(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	rig.fake.Reconfigure(dockertest.FakeConfig{
		Images: []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}, RepoDigests: []string{"myapp@" + digestOf("f")}}},
	})
	rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "manifest unknown")

	rv := rig.standaloneReview(t, "demo", "services:\n  web:\n    image: "+localImageRef+"\n")

	if len(rv.Pins) != 0 || rv.Problems.Errors() == 0 || rv.Problems[0].Code != stackops.CodeImageUnresolved {
		t.Errorf("pins = %v problems = %+v, want image_unresolved: an image a registry gave is not a local one", rv.Pins, rv.Problems)
	}
}

func TestPinAStandaloneHostsUnreachableRegistryBlocksOnlyTheServiceThatNeedsAFreshLookup(t *testing.T) {
	t.Parallel()
	// web runs, pinned; cache is new and its registry is down.
	h := hostStack(t, "demo", "services:\n  web:\n    image: nginx:1.27-alpine\n", nil)
	rig := hostRig(t, h)
	rig.fake.SetDistributionFailure(hostApp, 500, registryDown)
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n  cache:\n    image: " + hostApp + "\n"

	rv := rig.reviewOf(t, "demo", compose, nil, nil)

	var blocking []string
	for _, p := range rv.Problems {
		if p.Severity == stacks.SeverityError {
			blocking = append(blocking, p.Code+" "+p.Key)
		}
	}
	if !slices.Equal(blocking, []string{stackops.CodeRegistryUnreachable + " services.cache.image"}) {
		t.Errorf("blocking problems = %v, want the cache alone", blocking)
	}
	if rv.Pins["web"] != h.Pins["web"] {
		t.Errorf("pins = %v, want web to keep %s", rv.Pins, h.Pins["web"])
	}
	if refs := rig.fake.DistributionRefs(); !slices.Equal(refs, []string{hostApp}) {
		t.Errorf("the registry was asked about %v, want only the cache's image: web keeps its digest", refs)
	}
}

func TestDeployStandaloneOfAServiceThatKeepsItsDigestNeedsNoRegistry(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", "services:\n  web:\n    image: nginx:1.27-alpine\n", nil)
	rig := hostRig(t, h)
	// Every registry is down: web runs the text it is deployed with, so it is not asked.
	rig.fake.SetDistributionFailure("docker.io/library/nginx:1.27-alpine", 500, registryDown)
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", "services:\n  web:\n    image: nginx:1.27-alpine\n"))

	if !res.OK() || stand.calls != 1 {
		t.Fatalf("result = %+v (the SDK ran %d times), want a deploy that never needed the registry", res, stand.calls)
	}
	if refs := rig.fake.DistributionRefs(); len(refs) != 0 {
		t.Errorf("the registry was asked about %v", refs)
	}
}

func TestDeployStandaloneOfALocalOnlyImageHandsTheSDKTheImageIDAndKeepsTheTextAsWritten(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	rig.fake.Reconfigure(dockertest.FakeConfig{
		Images: []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}}},
	})
	rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "pull access denied for myapp, repository does not exist or may require authorization")
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	pins := &memPins{}
	in := standaloneInput("demo", "services:\n  web:\n    image: "+localImageRef+"\n")
	in.Pins, in.RevisionID = pins, "rev-1"

	res, _ := rig.deployStandalone(t, in)

	if !res.OK() || stand.project == nil {
		t.Fatalf("result = %+v, want a deploy of the local image", res)
	}
	web := stand.project.Services["web"]
	if web.Image != localImageID || web.CustomLabels[stacks.LabelStackImage] != localImageRef {
		t.Errorf("image = %q, as written = %q, want the image ID and the text as written", web.Image, web.CustomLabels[stacks.LabelStackImage])
	}
	if got := pins.pinsOf("rev-1"); got["web"] != localImageID {
		t.Errorf("stored pins = %v, want the image ID", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, want none: the image is on the host and is not pulled", w)
	}
}
