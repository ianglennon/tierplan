// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/moby/moby/api/types/swarm"
)

var _ = registerFakeHandler((*Fake).serveConfigList)
var _ = registerFakeHandler((*Fake).serveConfigCreate)
var _ = registerFakeHandler((*Fake).serveConfigInspect)
var _ = registerFakeHandler((*Fake).serveConfigRemove)

// configDataLimit is the size a manager refuses a config at: 1,023,999 bytes work and
// 1,024,000 do not (RESEARCH E18).
const configDataLimit = 1024000

// FakeConfigObject is one swarm config. A real manager lists a config with its content
// (RESEARCH E17), so the fake does too: that is what lets a test prove the client strips it.
type FakeConfigObject struct {
	ID, Name string
	Labels   map[string]string
	// Data is the content.
	Data []byte
	// Templating is the name of the templating driver, empty for none.
	Templating string
	// Version is the object version; zero is 1.
	Version              uint64
	CreatedAt, UpdatedAt time.Time
}

// FakeConfigCreate is one config create the fake daemon accepted.
type FakeConfigCreate struct {
	Name   string
	Labels map[string]string
	// Data is the decoded content.
	Data []byte
	// Templating is the name of the templating driver the request carried, empty for none.
	Templating string
}

var configPath = regexp.MustCompile(`^/configs/([^/]+)$`)

// asConfig is the config as the manager reports it: with its content.
func (fc FakeConfigObject) asConfig() swarm.Config {
	c := swarm.Config{ID: fc.ID}
	c.Version.Index = max(fc.Version, 1)
	c.CreatedAt, c.UpdatedAt = fc.CreatedAt, fc.UpdatedAt
	c.Spec.Name, c.Spec.Labels, c.Spec.Data = fc.Name, fc.Labels, fc.Data
	if fc.Templating != "" {
		c.Spec.Templating = &swarm.Driver{Name: fc.Templating}
	}
	return c
}

// resolveConfig finds a config by its ID or its name.
func (c FakeConfig) resolveConfig(ref string) (FakeConfigObject, bool) {
	for _, o := range c.Configs {
		if o.ID == ref {
			return o, true
		}
	}
	for _, o := range c.Configs {
		if o.Name == ref {
			return o, true
		}
	}
	return FakeConfigObject{}, false
}

// withConfigs returns c with the configs created through the daemon listed after its own.
// The slice is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withConfigs(created []FakeConfigObject) FakeConfig {
	if len(created) == 0 {
		return c
	}
	c.Configs = append(slices.Clone(c.Configs), created...)
	return c
}

// withoutConfigs returns c without the configs removed through the daemon.
func (c FakeConfig) withoutConfigs(gone map[string]bool) FakeConfig {
	if len(gone) == 0 {
		return c
	}
	c.Configs = slices.DeleteFunc(slices.Clone(c.Configs), func(o FakeConfigObject) bool { return gone[o.ID] })
	return c
}

// serveConfigList answers GET /configs with every config and its content, as a manager does.
func (f *Fake) serveConfigList(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet || p != "/configs" {
		return false
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	out := make([]swarm.Config, 0, len(cfg.Configs))
	for _, o := range cfg.Configs {
		out = append(out, o.asConfig())
	}
	writeJSON(w, http.StatusOK, out)
	return true
}

// serveConfigInspect answers GET /configs/{id}, by ID or name: the config with its content,
// or the 404 a real manager gives.
func (f *Fake) serveConfigInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := configPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	fc, ok := cfg.resolveConfig(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "config " + m[1] + " not found"})
		return true
	}
	writeJSON(w, http.StatusOK, fc.asConfig())
	return true
}

// serveConfigCreate answers POST /configs/create. It refuses content of no bytes or of
// 1,024,000 or more as a manager does, and a name that is taken. An accepted create joins the
// lists and is kept for ConfigCreates. The write is recorded whatever the answer.
func (f *Fake) serveConfigCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/configs/create" {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	cfg = f.config()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if cfg.WriteError != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": cfg.WriteError})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	var spec swarm.ConfigSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	if n := len(spec.Data); n == 0 || n >= configDataLimit {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("rpc error: code = InvalidArgument desc = config data must be larger than 0 and less than %d bytes", configDataLimit)})
		return true
	}
	if _, taken := cfg.resolveConfig(spec.Name); taken {
		writeJSON(w, http.StatusConflict, map[string]string{
			"message": "rpc error: code = AlreadyExists desc = config " + spec.Name + " already exists"})
		return true
	}
	rec := FakeConfigCreate{Name: spec.Name, Labels: spec.Labels, Data: slices.Clone(spec.Data)}
	if spec.Templating != nil {
		rec.Templating = spec.Templating.Name
	}
	f.mu.Lock()
	id := fmt.Sprintf("config%02d", len(f.configs)+len(f.cfg.Configs)+1)
	now := time.Now().UTC()
	f.configs = append(f.configs, FakeConfigObject{
		ID: id, Name: spec.Name, Labels: spec.Labels, Data: rec.Data, Templating: rec.Templating, CreatedAt: now, UpdatedAt: now,
	})
	f.configCreates = append(f.configCreates, rec)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, swarm.ConfigCreateResponse{ID: id})
	return true
}

// ConfigCreates returns every config create the daemon accepted, in order. A create it
// refused is not among them.
func (f *Fake) ConfigCreates() []FakeConfigCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.configCreates)
}

// serveConfigRemove answers DELETE /configs/{id}. A config the manager does not know is the
// 404 it gives; a config any service's spec references, whatever that service's tasks are
// doing, is the 400 "is in use by the following service" it gives (RESEARCH E16). Otherwise the
// config leaves every list. The write is recorded whatever the answer.
func (f *Fake) serveConfigRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := configPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	cfg = f.config()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if cfg.WriteError != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": cfg.WriteError})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	fc, ok := cfg.resolveConfig(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "config " + m[1] + " not found"})
		return true
	}
	for _, s := range cfg.serviceList(false) {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, ref := range cs.Configs {
			if ref != nil && (ref.ConfigID == fc.ID || ref.ConfigName == fc.Name) {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"message": "config '" + fc.Name + "' is in use by the following service: " + s.Spec.Name})
				return true
			}
		}
	}
	f.mu.Lock()
	if f.configGone == nil {
		f.configGone = map[string]bool{}
	}
	f.configGone[fc.ID] = true
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}
