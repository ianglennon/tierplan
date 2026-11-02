// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package registry

import (
	"testing"
)

// Which credential a pull uses (plan 05-08, IMG-02, D-02, D-03, D-10). Select is pure: the
// table is the whole contract.

func inst(id, host, prefix string) Candidate {
	return Candidate{ID: id, Name: id, Layer: LayerInstance, Host: host, Prefix: prefix}
}

func env(id, host, prefix string) Candidate {
	return Candidate{ID: id, Name: id, Layer: LayerEnvironment, EnvID: "e1", EnvName: "prod", Host: host, Prefix: prefix}
}

func fallback(c Candidate) Candidate {
	c.HubFallback = true
	return c
}

func TestSelect(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		cands   []Candidate
		wantID  string
		wantOn  string
		wantRes map[string]string // candidate ID to its Result, checked when set
	}{
		{
			name: "the longest prefix wins across layers", ref: "ghcr.io/acme/team/app:1",
			cands:  []Candidate{inst("inst-acme", "ghcr.io", "acme"), env("env-team", "ghcr.io", "acme/team")},
			wantID: "env-team", wantOn: OnPrefix,
			wantRes: map[string]string{"env-team": ResultUsed, "inst-acme": ResultShorterPrefix},
		},
		{
			name: "a longer instance prefix beats a shorter environment one", ref: "ghcr.io/acme/team/app:1",
			cands:  []Candidate{inst("inst-team", "ghcr.io", "acme/team"), env("env-acme", "ghcr.io", "acme")},
			wantID: "inst-team", wantOn: OnPrefix,
			wantRes: map[string]string{"inst-team": ResultUsed, "env-acme": ResultShorterPrefix},
		},
		{
			name: "with the same prefix the environment beats the instance", ref: "ghcr.io/acme/app",
			cands:  []Candidate{inst("inst-acme", "ghcr.io", "acme"), env("env-acme", "ghcr.io", "acme")},
			wantID: "env-acme", wantOn: OnPrefix,
			wantRes: map[string]string{"env-acme": ResultUsed, "inst-acme": ResultOverriddenByEnv},
		},
		{
			name: "a host-only credential matches its own host with a port", ref: "registry.example.com:5000/x/y",
			cands:  []Candidate{inst("reg", "registry.example.com:5000", "")},
			wantID: "reg", wantOn: OnHost,
		},
		{
			name: "a prefix beats a host-only credential", ref: "ghcr.io/acme/app",
			cands:  []Candidate{env("env-host", "ghcr.io", ""), inst("inst-acme", "ghcr.io", "acme")},
			wantID: "inst-acme", wantOn: OnPrefix,
			wantRes: map[string]string{"env-host": ResultShorterPrefix},
		},
		{
			name: "a Docker Hub namespace beats the fallback", ref: "acme/app",
			cands:  []Candidate{fallback(inst("hub-fb", "docker.io", "")), inst("hub-acme", "docker.io", "acme")},
			wantID: "hub-acme", wantOn: OnPrefix,
			wantRes: map[string]string{"hub-acme": ResultUsed, "hub-fb": ResultHubFallbackOverridden},
		},
		{
			name: "the Docker Hub fallback covers an official image", ref: "nginx",
			cands:  []Candidate{fallback(inst("hub-fb", "docker.io", "")), inst("hub-acme", "docker.io", "acme")},
			wantID: "hub-fb", wantOn: OnHubFallback,
			wantRes: map[string]string{"hub-fb": ResultUsed, "hub-acme": ResultNoMatch},
		},
		{
			name: "an environment fallback beats an instance fallback", ref: "nginx",
			cands:  []Candidate{fallback(inst("inst-fb", "docker.io", "")), fallback(env("env-fb", "docker.io", ""))},
			wantID: "env-fb", wantOn: OnHubFallback,
			wantRes: map[string]string{"env-fb": ResultUsed, "inst-fb": ResultOverriddenByEnv},
		},
		{
			name: "a credential with both a namespace and the flag uses the namespace where it can", ref: "acme/app",
			cands:  []Candidate{fallback(inst("hub-both", "docker.io", "acme"))},
			wantID: "hub-both", wantOn: OnPrefix,
		},
		{
			name: "a credential with both a namespace and the flag falls back elsewhere", ref: "other/app",
			cands:  []Candidate{fallback(inst("hub-both", "docker.io", "acme"))},
			wantID: "hub-both", wantOn: OnHubFallback,
		},
		{
			name: "a ghcr.io credential never matches a host that merely starts with it", ref: "ghcr.io.evil.example/acme/app",
			cands:  []Candidate{inst("ghcr", "ghcr.io", "")},
			wantID: "", wantOn: "",
		},
		{
			name: "a ghcr.io credential never matches a path that contains it", ref: "evil.example/ghcr.io/acme/app",
			cands:  []Candidate{inst("ghcr", "ghcr.io", "")},
			wantID: "", wantOn: "",
		},
		{
			name: "a prefix matches whole segments only", ref: "ghcr.io/acmecorp/app",
			cands:  []Candidate{inst("acme", "ghcr.io", "acme")},
			wantID: "", wantOn: "",
			wantRes: map[string]string{"acme": ResultNoMatch},
		},
		{
			name: "a prefix equal to the whole path matches", ref: "ghcr.io/acme",
			cands:  []Candidate{inst("acme", "ghcr.io", "acme")},
			wantID: "acme", wantOn: OnPrefix,
		},
		{
			name: "no candidates is anonymous", ref: "ghcr.io/acme/app",
			cands:  nil,
			wantID: "", wantOn: "",
		},
		{
			name: "a credential for another host is anonymous", ref: "ghcr.io/acme/app",
			cands:  []Candidate{inst("gitlab", "registry.gitlab.com", "")},
			wantID: "", wantOn: "",
		},
		{
			name: "an empty prefix covers a whole registry that is not Docker Hub", ref: "ghcr.io/anyone/anything",
			cands:  []Candidate{inst("ghcr", "ghcr.io", "")},
			wantID: "ghcr", wantOn: OnHost,
		},
		{
			name: "a docker.io candidate with an empty prefix and no flag never applies", ref: "nginx",
			cands:  []Candidate{inst("hub", "docker.io", "")},
			wantID: "", wantOn: "",
			wantRes: map[string]string{"hub": ResultNoMatch},
		},
		{
			name: "the host compares without regard to case", ref: "ghcr.io/acme/app",
			cands:  []Candidate{inst("ghcr", "GHCR.IO", "acme")},
			wantID: "ghcr", wantOn: OnPrefix,
		},
		{
			name: "a digest reference matches like a tag reference", ref: "ghcr.io/acme/app@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			cands:  []Candidate{inst("ghcr", "ghcr.io", "acme")},
			wantID: "ghcr", wantOn: OnPrefix,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParseRef(tc.ref)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tc.ref, err)
			}
			got := Select(tc.cands, ref)
			gotID := ""
			if got.Cred != nil {
				gotID = got.Cred.ID
			}
			if gotID != tc.wantID || got.On != tc.wantOn {
				t.Fatalf("Select = %q on %q, want %q on %q", gotID, got.On, tc.wantID, tc.wantOn)
			}
			for _, c := range got.Considered {
				if want, ok := tc.wantRes[c.Candidate.ID]; ok && c.Result != want {
					t.Errorf("result of %s = %q, want %q", c.Candidate.ID, c.Result, want)
				}
			}
			for id := range tc.wantRes {
				found := false
				for _, c := range got.Considered {
					found = found || c.Candidate.ID == id
				}
				if !found {
					t.Errorf("%s is not among the candidates considered", id)
				}
			}
		})
	}
}

func TestSelectListsTheUsedCandidateFirstAndOnlyCandidatesOnTheSameHost(t *testing.T) {
	ref, err := ParseRef("ghcr.io/acme/team/app:1")
	if err != nil {
		t.Fatal(err)
	}
	got := Select([]Candidate{
		inst("other-host", "registry.gitlab.com", ""),
		inst("short", "ghcr.io", "acme"),
		env("long", "ghcr.io", "acme/team"),
		inst("elsewhere", "ghcr.io", "other"),
	}, ref)
	if len(got.Considered) != 3 {
		t.Fatalf("considered = %+v, want the three ghcr.io candidates", got.Considered)
	}
	if got.Considered[0].Candidate.ID != "long" || got.Considered[0].Result != ResultUsed || !got.Considered[0].Matches {
		t.Errorf("first = %+v, want the used one", got.Considered[0])
	}
	if got.Considered[1].Candidate.ID != "short" || !got.Considered[1].Matches {
		t.Errorf("second = %+v, want short, which matches", got.Considered[1])
	}
	if got.Considered[2].Candidate.ID != "elsewhere" || got.Considered[2].Matches || got.Considered[2].Result != ResultNoMatch {
		t.Errorf("third = %+v, want elsewhere, which does not", got.Considered[2])
	}
}

func TestSelectIsDeterministicWhateverTheOrderOfTheCandidates(t *testing.T) {
	ref, err := ParseRef("ghcr.io/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	a, b := inst("a", "ghcr.io", "acme"), env("b", "ghcr.io", "acme")
	c := inst("c", "ghcr.io", "")
	for _, order := range [][]Candidate{{a, b, c}, {c, b, a}, {b, c, a}} {
		if got := Select(order, ref); got.Cred == nil || got.Cred.ID != "b" {
			t.Errorf("order %v chose %+v", order, got.Cred)
		}
	}
}

func TestSelectDoesNotChangeTheCandidatesItWasGiven(t *testing.T) {
	ref, _ := ParseRef("ghcr.io/acme/app")
	cands := []Candidate{inst("a", "ghcr.io", ""), inst("b", "ghcr.io", "acme")}
	Select(cands, ref)
	if cands[0].ID != "a" || cands[1].ID != "b" {
		t.Errorf("candidates reordered: %+v", cands)
	}
}
