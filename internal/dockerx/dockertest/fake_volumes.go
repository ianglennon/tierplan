// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
)

var _ = registerFakeHandler((*Fake).serveVolumeCreate)
var _ = registerFakeHandler((*Fake).serveVolumeInspect)
var _ = registerFakeHandler((*Fake).serveVolumeRemove)

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
	if cfg.VolumeHostPolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: host_policy: " + r.Method + " " + r.URL.Path})
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
	delete(f.volGone, req.Name)
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

// serveVolumeRemove answers DELETE /volumes/{name}. A name the daemon does not have is the
// 404 a real one gives; a volume some container mounts, running or not, is the 409 "volume
// is in use" a real one gives, naming the container. Otherwise the volume leaves every
// list. The write is recorded whatever the answer, with its query, so a test can see that
// force was never sent.
func (f *Fake) serveVolumeRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := volumePath.FindStringSubmatch(p)
	if m == nil {
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
	if _, ok := cfg.resolveVolume(m[1]); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "get " + m[1] + ": no such volume"})
		return true
	}
	if slices.Contains(cfg.BusyVolumes, m[1]) {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "remove " + m[1] + ": volume is in use - [started-meanwhile]"})
		return true
	}
	for _, c := range cfg.Containers {
		if slices.Contains(c.Mounts, m[1]) {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "remove " + m[1] + ": volume is in use - [" + c.ID + "]"})
			return true
		}
	}
	f.mu.Lock()
	if f.volGone == nil {
		f.volGone = map[string]bool{}
	}
	f.volGone[m[1]] = true
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}

// withoutVolumes returns c without the volumes removed through the daemon. The slice is
// copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withoutVolumes(gone map[string]bool) FakeConfig {
	if len(gone) == 0 {
		return c
	}
	c.Volumes = slices.DeleteFunc(slices.Clone(c.Volumes), func(v FakeVolume) bool { return gone[v.Name] })
	return c
}

var _ = registerFakeHandler((*Fake).serveSystemDF)

// serveSystemDF answers GET /system/df for the volumes: with the shape of the API version
// the request names, the way a real daemon does. From API 1.52 that is VolumeUsage with the
// totals and the verbose items; before it, the legacy Volumes list. Each volume carries its
// UsageData: the size it was given (-1 for a driver other than local) and how many of the
// daemon's containers mount it.
func (f *Fake) serveSystemDF(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet || p != "/system/df" {
		return false
	}
	f.mu.Lock()
	f.dfCalls++
	f.mu.Unlock()
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if cfg.DiskUsageDelay > 0 {
		select {
		case <-time.After(cfg.DiskUsageDelay):
		case <-r.Context().Done():
			return true
		}
	}
	if cfg.DiskUsageFail {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "df: cannot walk the volumes"})
		return true
	}
	vols := make([]volume.Volume, 0, len(cfg.Volumes))
	var total int64
	for _, fv := range cfg.Volumes {
		v := fv.asVolume()
		size := fv.Size
		if fv.Driver != "" && fv.Driver != "local" {
			size = -1
		}
		refs := int64(0)
		for _, c := range cfg.Containers {
			if slices.Contains(c.Mounts, fv.Name) {
				refs++
			}
		}
		v.UsageData = &volume.UsageData{Size: size, RefCount: refs}
		if size > 0 {
			total += size
		}
		vols = append(vols, v)
	}
	version := cfg.APIVersion
	if m := versionPrefix.FindStringSubmatch(r.URL.Path); m != nil {
		version = m[1] + "." + m[2]
	}
	if cmpAPI(version, "1.52") < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"LayersSize": 0, "Volumes": vols})
		return true
	}
	writeJSON(w, http.StatusOK, system.DiskUsage{VolumeUsage: &volume.DiskUsage{
		TotalCount: int64(len(vols)), TotalSize: total, Items: vols,
	}})
	return true
}

// DiskUsageCalls is how many GET /system/df requests the daemon has been sent.
func (f *Fake) DiskUsageCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dfCalls
}
