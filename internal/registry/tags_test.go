// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"
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
		"no access to the repo":   {func(f *fakeRegistry) { f.tagsStatus = http.StatusUnauthorized }, func(f *fakeRegistry) *ResolvedCredential { return f.cred("c1", f.pass) }, CodeLoginRefused},
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
