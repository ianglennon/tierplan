// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/moby/moby/api/types/volume"
)

var _ = registerFakeHandler((*Fake).serveVolumeCreate)
var _ = registerFakeHandler((*Fake).serveVolumeInspect)

// serveVolumeCreate answers POST /volumes/create. Like a real daemon it answers a name
// it already has with the existing volume and no error, and it makes a volume of no
// name up. The volume joins the lists the daemon answers, the body is kept for
// VolumeCreates, and the write is recorded whatever the answer.
func (f *Fake) serveVolumeCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/volumes/create" {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	var req volume.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	if existing, ok := cfg.resolveVolume(req.Name); ok {
		writeJSON(w, http.StatusCreated, existing.asVolume())
		return true
	}
	driver := req.Driver
	if driver == "" {
		driver = "local"
	}
	fv := FakeVolume{
		Name: req.Name, Driver: driver, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Labels: req.Labels, Options: req.DriverOpts,
	}
	f.mu.Lock()
	f.vols = append(f.vols, fv)
	f.volCreates = append(f.volCreates, req)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, fv.asVolume())
	return true
}

// volumePath matches /volumes/{name}.
var volumePath = regexp.MustCompile(`^/volumes/([^/]+)$`)

// serveVolumeInspect answers GET /volumes/{name}: the volume with its driver options
// and labels, or the 404 a real daemon gives.
func (f *Fake) serveVolumeInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := volumePath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	fv, ok := cfg.resolveVolume(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "get " + m[1] + ": no such volume"})
		return true
	}
	writeJSON(w, http.StatusOK, fv.asVolume())
	return true
}

// asVolume is the volume as the daemon reports it.
func (fv FakeVolume) asVolume() volume.Volume {
	return volume.Volume{
		Name: fv.Name, Driver: fv.Driver, CreatedAt: fv.CreatedAt, Labels: fv.Labels, Options: fv.Options,
		Mountpoint: "/var/lib/docker/volumes/" + fv.Name + "/_data", Scope: "local",
	}
}

// resolveVolume finds a volume by its name.
func (c FakeConfig) resolveVolume(name string) (FakeVolume, bool) {
	for _, v := range c.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return FakeVolume{}, false
}

// VolumeCreates returns the body of every volume create the daemon accepted, in order,
// as the daemon decoded it. A create of a name that existed is not among them.
func (f *Fake) VolumeCreates() []volume.CreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.volCreates)
}

// withVolumes returns c with the volumes created through the daemon listed after its
// own. The slice is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withVolumes(created []FakeVolume) FakeConfig {
	if len(created) == 0 {
		return c
	}
	c.Volumes = append(slices.Clone(c.Volumes), created...)
	return c
}
