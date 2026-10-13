// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"
)

// Listing the tags of a repository (plan 05-18, IMG-03, D-19, D-24a). The registry is a real
// TLS server in the test.

var tagsT0 = time.Unix(1_700_000_000, 0)

func tagServiceFor(f *fakeRegistry) *TagService {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewTagService(NewClientCache(f.egress(), "test", log), f.egress(), "test", log)
}

func (f *fakeRegistry) cred(id, pass string) *ResolvedCredential {
	return &ResolvedCredential{
		Candidate:   Candidate{ID: id, Name: "name-" + id, Layer: LayerInstance},
		HostOptions: HostOptions{Host: f.host(), Username: f.user, Password: pass},
		UpdatedAt:   tagsT0,
	}
}

func (f *fakeRegistry) ref(t *testing.T, repo string) Ref {
	t.Helper()
	r, err := ParseRef(f.host() + "/" + repo)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func tagNames(p TagPage) []string {
	out := make([]string, len(p.Tags))
	for i, tg := range p.Tags {
		out[i] = tg.Name
	}
	return out
}

func TestListReadsTheTagNamesWithTheCredential(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tags = []string{"1.0", "2.0", "latest"}
	page, err := tagServiceFor(f).List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Repo != "acme/app" || page.Host != f.host() || page.Capped {
		t.Errorf("page = %+v", page)
	}
	got := tagNames(page)
	slices.Sort(got)
	if !slices.Equal(got, []string{"1.0", "2.0", "latest"}) {
		t.Errorf("names = %v", got)
	}
}

func TestListServesASecondCallFromTheCacheWithoutAskingTheRegistry(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	s := tagServiceFor(f)
	ref := f.ref(t, "acme/app")
	for range 3 {
		if _, err := s.List(context.Background(), f.cred("c1", f.pass), ref, "", true, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.tagRequests(); n != 2 { // the first call: one unauthenticated try, one with the token
		t.Errorf("tag list requests = %d, want the 2 of the first call alone", n)
	}
}

func TestListKeepsTheListFetchedWithACredentialAwayFromAnonymousCalls(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	s := tagServiceFor(f)
	ref := f.ref(t, "acme/app")
	if _, err := s.List(context.Background(), f.cred("c1", f.pass), ref, "", true, 0); err != nil {
		t.Fatal(err)
	}
	// The registry only shows this repository to a signed-in client: an anonymous call must
	// reach it and be refused, not be handed the cached names (T-05-60).
	_, err := s.List(context.Background(), nil, ref, "", true, 0)
	if got := TagErrorCode(err); got != CodeNeedsLogin {
		t.Fatalf("anonymous after a credentialed call: %v (%q), want needs_login", err, got)
	}
	// Another credential is another cache entry too.
	before := f.tagRequests()
	if _, err := s.List(context.Background(), f.cred("c2", f.pass), ref, "", true, 0); err != nil {
		t.Fatal(err)
	}
	if f.tagRequests() == before {
		t.Error("a second credential was served the first one's cached list")
	}
}

func TestListMakesANewRequestAfterTheCredentialIsEdited(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	s := tagServiceFor(f)
	ref := f.ref(t, "acme/app")
	c := f.cred("c1", f.pass)
	if _, err := s.List(context.Background(), c, ref, "", true, 0); err != nil {
		t.Fatal(err)
	}
	edited := f.cred("c1", "a-wrong-password")
	edited.UpdatedAt = tagsT0.Add(time.Hour)
	_, err := s.List(context.Background(), edited, ref, "", true, 0)
	if got := TagErrorCode(err); got != CodeLoginRefused {
		t.Errorf("after an edit: %v (%q), want the new sign-in tried and login_refused", err, got)
	}
}

func TestListNamesTheReasonARegistryRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		tune func(*fakeRegistry)
		cred func(*fakeRegistry) *ResolvedCredential
		want string
	}{
		"anonymous 401":           {func(*fakeRegistry) {}, func(*fakeRegistry) *ResolvedCredential { return nil }, CodeNeedsLogin},
		"wrong secret, 401":       {func(*fakeRegistry) {}, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", "wrong") }, CodeLoginRefused},
		"wrong secret, 403":       {func(f *fakeRegistry) { f.refuseStatus = http.StatusForbidden }, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", "wrong") }, CodeLoginRefused},
		"no access to the repo":   {func(f *fakeRegistry) { f.tagsStatus = http.StatusUnauthorized }, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", f.pass) }, CodeRepositoryUnavailable},
		"repository unknown":      {func(f *fakeRegistry) { f.tagsStatus = http.StatusNotFound }, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", f.pass) }, CodeRepositoryNotFound},
		"rate limited":            {func(f *fakeRegistry) { f.tagsStatus = http.StatusTooManyRequests }, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", f.pass) }, CodeRateLimited},
		"not trusted certificate": {func(*fakeRegistry) {}, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", f.pass) }, CodeRegistryTLS},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRegistry(t, http.StatusUnauthorized)
			tc.tune(f)
			s := tagServiceFor(f)
			if tc.want == CodeRegistryTLS {
				// A service whose egress does not trust the fake's certificate.
				log := slog.New(slog.NewTextHandler(io.Discard, nil))
				s = NewTagService(nil, NewEgressPolicy(nil), "test", log)
			}
			_, err := s.List(context.Background(), tc.cred(f), f.ref(t, "acme/app"), "", true, 0)
			var te *TagError
			if !errors.As(err, &te) || te.Code != tc.want {
				t.Fatalf("err = %v, want code %s", err, tc.want)
			}
			if got := err.Error(); got != "registry: list tags: "+tc.want {
				t.Errorf("Error() = %q; the text must be the code alone", got)
			}
		})
	}
}

// A refused listing is worded by the sign-in check that follows it: a check that fails some
// way other than a refusal gives that failure's code and never login_refused (G-05-17a).
func TestListWordsAFailedSignInCheckByItsOwnReason(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tagsStatus = http.StatusUnauthorized
	f.signedInV2 = func(w http.ResponseWriter, r *http.Request) {
		// The connection drops: the registry cannot be reached for the check.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the fake cannot drop the connection")
			return
		}
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
	s := tagServiceFor(f)
	_, err := s.List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), "", true, 0)
	if got := TagErrorCode(err); got != CodeRegistryUnreachable {
		t.Errorf("err = %v (%q), want registry_unreachable", err, got)
	}
}

// A registry that serves /v2/ to anyone and enforces access per repository passes a sign-in
// check whatever the password is, so the check proves nothing there: a wrong password stays
// login_refused instead of reading as the repository's refusal (WR-G01).
func TestListKeepsLoginRefusedOnARegistryWhoseV2IsOpen(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.open = true // GET /v2/ answers 200 with no challenge; the tag list needs the token
	s := tagServiceFor(f)
	_, err := s.List(context.Background(), f.cred("c1", "wrong"), f.ref(t, "acme/app"), "", true, 0)
	if got := TagErrorCode(err); got != CodeLoginRefused {
		t.Fatalf("wrong secret on an open /v2/: %v (%q), want login_refused", err, got)
	}
	// The right secret still lists.
	if _, err := s.List(context.Background(), f.cred("c2", f.pass), f.ref(t, "acme/app"), "", true, 0); err != nil {
		t.Fatalf("right secret on an open /v2/: %v", err)
	}
}

func TestRefusalCodeChecksTheSignInOnlyForARefusalWithACredential(t *testing.T) {
	refused := fmt.Errorf("wrapped: %w", errs.ErrHTTPUnauthorized)
	pass := func() error { return nil }
	fail := func() error { t.Error("the sign-in check ran when it was not needed"); return nil }
	for name, tc := range map[string]struct {
		err      error
		signedIn bool
		signIn   func() error
		want     string
	}{
		"refused, sign-in works":      {refused, true, pass, CodeRepositoryUnavailable},
		"refused, sign-in refused":    {refused, true, func() error { return refused }, CodeLoginRefused},
		"refused, sign-in rate limit": {refused, true, func() error { return errs.ErrHTTPRateLimit }, CodeRateLimited},
		"refused, sign-in down":       {refused, true, func() error { return context.DeadlineExceeded }, CodeRegistryUnreachable},
		"anonymous refusal":           {refused, false, fail, CodeNeedsLogin},
		"not found":                   {errs.ErrNotFound, true, fail, CodeRepositoryNotFound},
		"rate limited":                {errs.ErrHTTPRateLimit, true, fail, CodeRateLimited},
	} {
		t.Run(name, func(t *testing.T) {
			if got := refusalCode(tc.err, tc.signedIn, tc.signIn); got != tc.want {
				t.Errorf("refusalCode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListReportsAClosedPortAsUnreachable(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	ref := f.ref(t, "acme/app")
	s := tagServiceFor(f)
	f.srv.Close()
	_, err := s.List(context.Background(), nil, ref, "", true, 0)
	if got := TagErrorCode(err); got != CodeRegistryUnreachable {
		t.Errorf("err = %v (%q), want registry_unreachable", err, got)
	}
}

func TestListStopsAtTheNameCap(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tags = make([]string, nameListMax+500)
	for i := range f.tags {
		f.tags[i] = fmt.Sprintf("t%05d", i)
	}
	page, err := tagServiceFor(f).List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Capped || page.Total != nameListMax {
		t.Errorf("capped = %v, total = %d, want the list cut at %d", page.Capped, page.Total, nameListMax)
	}
}

func TestCapNamesCutsAListByItsSize(t *testing.T) {
	big := string(make([]byte, 1<<20))
	got := capNames([]string{big, big, big, big, big, big})
	if !got.capped || len(got.names) != 5 { // 5 MiB is allowed, the sixth megabyte is not
		t.Errorf("capped = %v, names = %d, want 5 names under the byte cap", got.capped, len(got.names))
	}
	if small := capNames([]string{"a", "b"}); small.capped || len(small.names) != 2 {
		t.Errorf("a short list was cut: %+v", small)
	}
}

func TestListFollowsLinkPagesOfTheRegistry(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.pageSize = 40
	f.tags = make([]string, 250)
	for i := range f.tags {
		f.tags[i] = fmt.Sprintf("v%03d", i)
	}
	page, err := tagServiceFor(f).List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 250 {
		t.Errorf("total = %d, want all 250 tags read through the Link pages", page.Total)
	}
}

func TestHiddenTag(t *testing.T) {
	sha := "sha256-" + strings.Repeat("ab12", 16)
	for name, want := range map[string]bool{
		sha:                                 true, // cosign's fallback tag for a signature or a referrer
		sha + ".sig":                        true,
		sha + ".att":                        true,
		sha + ".sbom":                       true,
		sha + ".sbom.json":                  true,
		sha + ".prov":                       true,
		"app.sig":                           true,
		"1.2.3.att":                         true,
		"v1.sbom":                           true,
		"buildcache":                        true,
		"cache":                             true,
		"app-buildcache":                    true,
		"registry.cache":                    true,
		"latest":                            false,
		"1.2.3":                             false,
		"1.2.3-alpine":                      false,
		"sha256-abc":                        false, // not 64 hex characters: a tag someone chose
		sha + "0":                           false, // 65 characters
		"sha256-" + strings.Repeat("g", 64): false, // not hex
		"cached":                            false,
		"mycache":                           false, // only -buildcache and .cache end a cache tag
		"signed":                            false,
		"attestation":                       false,
		"":                                  false,
	} {
		if got := HiddenTag(name); got != want {
			t.Errorf("HiddenTag(%q) = %v, want %v", name, got, want)
		}
	}
}

// bigRepo is 245 release tags, latest, and the four tags hidden by default.
func bigRepo() []string {
	sha := "sha256-" + strings.Repeat("0f", 32)
	tags := []string{"latest", sha + ".sig", sha + ".att", "buildcache", "app-buildcache"}
	for i := range 245 {
		tags = append(tags, fmt.Sprintf("1.%d.0", i))
	}
	rand.Shuffle(len(tags), func(i, j int) { tags[i], tags[j] = tags[j], tags[i] }) //nolint:gosec // G404: a test shuffle, not a secret
	return tags
}

func TestListSortsNewestVersionFirstAndPagesByAHundred(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.pageSize = 40
	f.tags = bigRepo()
	s := tagServiceFor(f)
	ctx, c, ref := context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app")

	var all []string
	for page, wantLen := range []int{100, 100, 46} {
		got, err := s.List(ctx, c, ref, "", true, page)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Tags) != wantLen || got.Total != 246 || got.Hidden != 4 || got.More != (page < 2) {
			t.Fatalf("page %d: %d tags, total %d, hidden %d, more %v; want %d, 246, 4, %v", page, len(got.Tags), got.Total, got.Hidden, got.More, wantLen, page < 2)
		}
		all = append(all, tagNames(got)...)
	}
	if all[0] != "1.244.0" || all[1] != "1.243.0" || all[244] != "1.0.0" || all[245] != "latest" {
		t.Errorf("order = %v ... %v, want 1.244.0 first down to 1.0.0 and then latest", all[:2], all[243:])
	}
	if beyond, err := s.List(ctx, c, ref, "", true, 3); err != nil || len(beyond.Tags) != 0 || beyond.More || beyond.Total != 246 {
		t.Errorf("a page past the end = %+v, %v; want none, no more", beyond, err)
	}
	if negative, err := s.List(ctx, c, ref, "", true, -5); err != nil || len(negative.Tags) != 100 || negative.Tags[0].Name != "1.244.0" {
		t.Errorf("a negative page = %d tags, %v; want the first page", len(negative.Tags), err)
	}
}

func TestListHidesSignatureAttestationAndCacheTagsUnlessAskedToShowThem(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tags = bigRepo()
	s := tagServiceFor(f)
	ctx, c, ref := context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app")

	hidden, err := s.List(ctx, c, ref, "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range tagNames(hidden) {
		if HiddenTag(n) {
			t.Errorf("%q is shown with the hide switch on", n)
		}
	}
	shown, err := s.List(ctx, c, ref, "", false, 0)
	if err != nil || shown.Total != 250 || shown.Hidden != 4 {
		t.Fatalf("with hide off: total %d hidden %d, %v; want 250 shown and 4 counted as hidden-type", shown.Total, shown.Hidden, err)
	}
	// The count of hidden-type tags follows the filter text, so "only hidden tags left" can be told.
	onlyCache, err := s.List(ctx, c, ref, "buildcache", true, 0)
	if err != nil || onlyCache.Total != 0 || onlyCache.Hidden != 2 || len(onlyCache.Tags) != 0 {
		t.Errorf("filter buildcache: %+v, %v; want nothing shown and 2 hidden", onlyCache, err)
	}
}

func TestListFiltersByTextIgnoringCase(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tags = []string{"1.0.0", "1.0.0-Alpine", "1.0.0-slim", "2.0.0", "latest", "Edge"}
	s := tagServiceFor(f)
	got, err := s.List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), " ALPINE ", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if names := tagNames(got); !slices.Equal(names, []string{"1.0.0-Alpine"}) || got.Total != 1 {
		t.Errorf("filter ALPINE = %v (total %d), want the one name", names, got.Total)
	}
	got, _ = s.List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), "eDGE", true, 0)
	if names := tagNames(got); !slices.Equal(names, []string{"Edge"}) {
		t.Errorf("filter eDGE = %v", names)
	}
	got, _ = s.List(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), "1.0.0", true, 0)
	if names := tagNames(got); !slices.Equal(names, []string{"1.0.0", "1.0.0-slim", "1.0.0-Alpine"}) {
		t.Errorf("filter 1.0.0 = %v, want the plain release first and then its variants by name, descending", names)
	}
}

func TestListDoesNotReorderTheCachedListBetweenCalls(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tags = bigRepo()
	s := tagServiceFor(f)
	ctx, c, ref := context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app")
	first, _ := s.List(ctx, c, ref, "", true, 0)
	_, _ = s.List(ctx, c, ref, "1.1", false, 1)
	second, err := s.List(ctx, c, ref, "", true, 0)
	if err != nil || !slices.Equal(tagNames(first), tagNames(second)) {
		t.Errorf("a later call changed the first page: %v", err)
	}
	if n := f.tagRequests(); n > 12 {
		t.Errorf("tag list requests = %d, want one fetch (a handful of Link pages), not one per call", n)
	}
}
