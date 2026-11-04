//go:build integration || swarm

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// addFixtureCredential stores the one setting TierPlan's own calls to a plain-HTTP fixture registry need: the registry
// credential with Insecure set, which lets the server's manifest HEAD fall back to plain HTTP (an anonymous call never does, so
// a plain registry could not be checked without it). The fixture registry asks for no sign-in and ignores the one it is sent.
// The credential covers the repositories under prefix.
func addFixtureCredential(t *testing.T, srv *servertest.Server, host, prefix string) {
	t.Helper()
	creds := registry.NewStore(srv.Core().Store(), srv.Core().Vault(), nil)
	if _, err := creds.Create(context.Background(), registry.CreateInput{
		Name: "fixture", Type: registry.TypeGeneric, Host: host, Prefix: prefix, Username: "bot", Secret: "fixture-secret-0123456789", Insecure: true,
	}, "test", nil); err != nil {
		t.Fatalf("store the fixture registry's credential: %v", err)
	}
}

// updateView is the update object of a row of the services list.
type updateView struct {
	State         string `json:"state"`
	Reason        string `json:"reason"`
	RunningDigest string `json:"runningDigest"`
	RemoteDigest  string `json:"remoteDigest"`
	JobID         string `json:"jobId"`
	Failure       *struct {
		Digest         string `json:"digest"`
		Reason         string `json:"reason"`
		RollbackFailed bool   `json:"rollbackFailed"`
	} `json:"failure"`
}

// containerUpdate is the update state of the container of that name in the containers list now, and false when it is not listed
// or has no update object.
func containerUpdate(t *testing.T, srv *servertest.Server, envID, name string) (updateView, bool) {
	t.Helper()
	code, body := srv.Do(http.MethodGet, "/api/env/"+envID+"/containers", nil)
	var list struct {
		Items []struct {
			Name   string      `json:"name"`
			Update *updateView `json:"update"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET containers = %d %s", code, body)
	}
	for _, it := range list.Items {
		if it.Name == name && it.Update != nil {
			return *it.Update, true
		}
	}
	return updateView{}, false
}
