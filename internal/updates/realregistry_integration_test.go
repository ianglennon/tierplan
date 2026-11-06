//go:build integration

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// fixtureHeads asks a registry the way the server does for an image whose registry has a stored credential: through the real
// registry.ClientCache, with a manifest HEAD. It is updates.NewHeads without the credential resolver, which needs a database;
// the client is the one a credential with the insecure switch gets, which is how the server reaches a registry that speaks plain
// HTTP (an anonymous client never does). The fixture registry asks for no sign-in.
type fixtureHeads struct{ clients *registry.ClientCache }

func (h fixtureHeads) client(ref registry.Ref) *registry.Client {
	return h.clients.ForCredential("fixture", time.Time{}, registry.HostOptions{Host: ref.Domain, Insecure: true})
}

func (h fixtureHeads) Digest(ctx context.Context, _ string, ref registry.Ref) (string, string, error) {
	d, err := h.client(ref).ManifestDigest(ctx, ref.Path, ref.Tag)
	return d, "fixture", err
}

func (h fixtureHeads) Index(ctx context.Context, _ string, ref registry.Ref, digest string) (bool, map[string]string, error) {
	index, ps, err := h.client(ref).IndexPlatforms(ctx, ref.Path, digest)
	if err != nil || !index {
		return false, nil, err
	}
	platforms := make(map[string]string, len(ps))
	for _, p := range ps {
		platforms[registry.PlatformKey(p)] = p.Digest
	}
	return true, platforms, nil
}

// pushBuild puts a one-layer image into the registry at base with the registry's own HTTP API, as repo:tag, and returns the
// digest of its manifest. build is the content of its layer, so two builds differ in their digests, which is a maintainer
// pushing a tag again. The layer is a few bytes: a registry stores a blob without reading it, and nothing here pulls the image.
func pushBuild(t *testing.T, base, repo, tag, build string) string {
	t.Helper()
	// do makes one request and answers its status, its headers and its whole body, with the response closed.
	do := func(method, url, contentType string, body []byte) (int, http.Header, []byte) {
		req, err := http.NewRequestWithContext(testsupport.Context(t), method, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, out
	}
	blob := func(content []byte) (digest string, size int) {
		sum := sha256.Sum256(content)
		digest = "sha256:" + hex.EncodeToString(sum[:])
		status, header, _ := do(http.MethodPost, base+"/v2/"+repo+"/blobs/uploads/", "", nil)
		loc := header.Get("Location")
		if status != http.StatusAccepted || loc == "" {
			t.Fatalf("start a blob upload: %d", status)
		}
		if strings.HasPrefix(loc, "/") {
			loc = base + loc
		}
		sep := "?"
		if strings.Contains(loc, "?") {
			sep = "&"
		}
		status, _, out := do(http.MethodPut, loc+sep+"digest="+digest, "application/octet-stream", content)
		if status != http.StatusCreated {
			t.Fatalf("finish the blob upload: %d %s", status, out)
		}
		return digest, len(content)
	}
	cfgDigest, cfgSize := blob([]byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`))
	layerDigest, layerSize := blob([]byte(build))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json",`+
		`"config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":%d,"digest":%q},`+
		`"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":%d,"digest":%q}]}`,
		cfgSize, cfgDigest, layerSize, layerDigest)
	status, header, out := do(http.MethodPut, base+"/v2/"+repo+"/manifests/"+tag, "application/vnd.docker.distribution.manifest.v2+json", []byte(manifest))
	if status != http.StatusCreated {
		t.Fatalf("put the manifest of %s:%s: %d %s", repo, tag, status, out)
	}
	sum := sha256.Sum256([]byte(manifest))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if got := header.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("the registry answered the digest %s for a manifest of %s", got, digest)
	}
	return digest
}

// TestRealRegistryMovingTag asks a real registry:2 about a tag that moves (plan 12-35, IMG-04, D-03): through the real registry
// client, a tag that was not pushed again stays up to date on a second check; the same tag pushed again with another build reads
// update available with the new digest; and a repository that is deleted reads check failed, not found, never up to date, even
// though the last good answer is still stored.
func TestRealRegistryMovingTag(t *testing.T) {
	ctx, cancel := context.WithTimeout(testsupport.Context(t), 5*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        dockertest.RegistryImage,
			ExposedPorts: []string{"5000/tcp"},
			WaitingFor:   wait.ForListeningPort("5000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start %s: %v", dockertest.RegistryImage, err)
	}
	port, err := ctr.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("find the registry port: %v", err)
	}
	host := "127.0.0.1:" + port.Port()
	base := "http://" + host
	const repo, tag = "tp/moving", "1"

	digestA := pushBuild(t, base, repo, tag, "build A")

	st := storetest.New(t)
	sql := updates.SQLStore{Q: st.Q, W: st.Write}
	clients := registry.NewClientCache(registry.NewEgressPolicy(nil), "test", slog.New(slog.DiscardHandler))
	chk := &updates.Checker{Heads: fixtureHeads{clients}, Store: sql, Log: slog.New(slog.DiscardHandler)}
	ref, err := registry.ParseRef(host + "/" + repo + ":" + tag)
	if err != nil {
		t.Fatalf("parse the reference: %v", err)
	}
	// The workload runs build A, named by its tag, as a service pinned to tag@digest does.
	w := updates.Workload{EnvID: "env", Kind: "service", ID: "svc", Name: "moving", Written: ref.String(), Ref: ref, RunningDigest: digestA}

	// verdict checks the environment as a person's Check now does, then judges the workload from the stored answer.
	verdict := func() updates.Verdict {
		t.Helper()
		if _, err := chk.CheckEnv(ctx, "env", []updates.Workload{w}); err != nil {
			t.Fatalf("check: %v", err)
		}
		key := updates.Key{Host: ref.Domain, Repo: ref.Path, Tag: ref.Tag, CredID: "fixture"}
		stored, err := sql.Checks(ctx, []updates.Key{key})
		if err != nil {
			t.Fatalf("read the stored check: %v", err)
		}
		var c *updates.Check
		if got, ok := stored[key]; ok {
			c = &got
		}
		return updates.Judge(updates.JudgeInput{Workload: w, Check: c, Now: time.Now(), StaleAfter: 24 * time.Hour})
	}

	// 1. Nothing was pushed again: up to date, and again on a second check.
	for i := range 2 {
		if v := verdict(); v.State != updates.StateUpToDate || v.RemoteDigest != digestA {
			t.Fatalf("check %d of an unchanged tag = %+v, want up_to_date at %s", i+1, v, digestA)
		}
	}

	// 2. The tag is pushed again with another build.
	digestB := pushBuild(t, base, repo, tag, "build B")
	if digestB == digestA {
		t.Fatalf("the two builds have one digest %s", digestA)
	}
	if v := verdict(); v.State != updates.StateUpdateAvailable || v.RemoteDigest != digestB || v.RunningDigest != digestA {
		t.Fatalf("check of a tag pushed again = %+v, want update_available from %s to %s", v, digestA, digestB)
	}

	// 3. The repository is deleted. The last good answer is still stored, and must not read as up to date.
	w.RunningDigest = digestB
	if v := verdict(); v.State != updates.StateUpToDate {
		t.Fatalf("the workload on build B = %+v, want up_to_date before the repository goes", v)
	}
	if code, out, err := ctr.Exec(ctx, []string{"rm", "-rf", "/var/lib/registry/docker/registry/v2/repositories/tp"}); err != nil || code != 0 {
		o, _ := io.ReadAll(out)
		t.Fatalf("delete the repository: exit %d %v %s", code, err, o)
	}
	v := verdict()
	if v.State != updates.StateCheckFailed || v.Reason != updates.ReasonNotFound {
		t.Fatalf("check of a deleted repository = %+v, want check_failed not_found", v)
	}
	if v.State == updates.StateUpToDate {
		t.Fatalf("a deleted repository reads up to date")
	}
}
