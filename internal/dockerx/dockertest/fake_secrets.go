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

var _ = registerFakeHandler((*Fake).serveSecretList)
var _ = registerFakeHandler((*Fake).serveSecretCreate)
var _ = registerFakeHandler((*Fake).serveSecretInspect)
var _ = registerFakeHandler((*Fake).serveSecretRemove)

// secretDataLimit is the size a manager refuses a secret at: 511,999 bytes work and
// 512,000 do not (RESEARCH E18).
const secretDataLimit = 512000

// FakeSecret is one swarm secret /secrets lists. The fake never holds a value: a real
// manager does not return one either (RESEARCH E17).
type FakeSecret struct {
	ID, Name string
	Labels   map[string]string
	// Version is the object version; zero is 1.
	Version              uint64
	CreatedAt, UpdatedAt time.Time
}

// FakeSecretCreate is one secret create the fake daemon accepted. Of the value it keeps
// only how many bytes there were, so a test can see a value was sent without the fake being
// somewhere the value could be found.
type FakeSecretCreate struct {
	Name   string
	Labels map[string]string
	// DataBytes is the length of the decoded value.
	DataBytes int
	// Driver and Templating are the names of the drivers the request carried, empty for none.
	Driver, Templating string
}

var secretPath = regexp.MustCompile(`^/secrets/([^/]+)$`)

// asSecret is the secret as the manager reports it: the spec has a name and labels and
// never the value.
func (fs FakeSecret) asSecret() swarm.Secret {
	s := swarm.Secret{ID: fs.ID}
	s.Version.Index = max(fs.Version, 1)
	s.CreatedAt, s.UpdatedAt = fs.CreatedAt, fs.UpdatedAt
	s.Spec.Name, s.Spec.Labels = fs.Name, fs.Labels
	return s
}

// resolveSecret finds a secret by its ID or its name.
func (c FakeConfig) resolveSecret(ref string) (FakeSecret, bool) {
	for _, s := range c.Secrets {
		if s.ID == ref {
			return s, true
		}
	}
	for _, s := range c.Secrets {
		if s.Name == ref {
			return s, true
		}
	}
	return FakeSecret{}, false
}

// withSecrets returns c with the secrets created through the daemon listed after its own.
// The slice is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withSecrets(created []FakeSecret) FakeConfig {
	if len(created) == 0 {
		return c
	}
	c.Secrets = append(slices.Clone(c.Secrets), created...)
	return c
}

// withoutSecrets returns c without the secrets removed through the daemon.
func (c FakeConfig) withoutSecrets(gone map[string]bool) FakeConfig {
	if len(gone) == 0 {
		return c
	}
	c.Secrets = slices.DeleteFunc(slices.Clone(c.Secrets), func(s FakeSecret) bool { return gone[s.ID] })
	return c
}

// serveSecretList answers GET /secrets with every secret that passes the request's label filter,
// none with a value.
func (f *Fake) serveSecretList(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet || p != "/secrets" {
		return false
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	keep := labelFilter(r.URL.Query().Get("filters"))
	out := make([]swarm.Secret, 0, len(cfg.Secrets))
	for _, s := range cfg.Secrets {
		if keep(s.Labels) {
			out = append(out, s.asSecret())
		}
	}
	writeJSON(w, http.StatusOK, out)
	return true
}

// serveSecretInspect answers GET /secrets/{id}, by ID or name: the secret without a value,
// or the 404 a real manager gives.
func (f *Fake) serveSecretInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := secretPath.FindStringSubmatch(p)
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
	fs, ok := cfg.resolveSecret(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "secret " + m[1] + " not found"})
		return true
	}
	writeJSON(w, http.StatusOK, fs.asSecret())
	return true
}

// serveSecretCreate answers POST /secrets/create. It refuses a value of no bytes or of
// 512,000 or more as a manager does, and a name that is taken. An accepted create joins
// the lists and is kept for SecretCreates, as a byte count only. The write is recorded
// whatever the answer.
func (f *Fake) serveSecretCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/secrets/create" {
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
	var spec swarm.SecretSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	if n := len(spec.Data); n == 0 || n >= secretDataLimit {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("rpc error: code = InvalidArgument desc = secret data must be larger than 0 and less than %d bytes", secretDataLimit)})
		return true
	}
	if _, taken := cfg.resolveSecret(spec.Name); taken {
		writeJSON(w, http.StatusConflict, map[string]string{
			"message": "rpc error: code = AlreadyExists desc = secret " + spec.Name + " already exists"})
		return true
	}
	rec := FakeSecretCreate{Name: spec.Name, Labels: spec.Labels, DataBytes: len(spec.Data)}
	if spec.Driver != nil {
		rec.Driver = spec.Driver.Name
	}
	if spec.Templating != nil {
		rec.Templating = spec.Templating.Name
	}
	f.mu.Lock()
	id := fmt.Sprintf("secret%02d", len(f.secrets)+len(f.cfg.Secrets)+1)
	now := time.Now().UTC()
	f.secrets = append(f.secrets, FakeSecret{ID: id, Name: spec.Name, Labels: spec.Labels, CreatedAt: now, UpdatedAt: now})
	f.secretCreates = append(f.secretCreates, rec)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, swarm.SecretCreateResponse{ID: id})
	return true
}

// SecretCreates returns every secret create the daemon accepted, in order. A create it
// refused is not among them, and none holds a value: only its length.
func (f *Fake) SecretCreates() []FakeSecretCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.secretCreates)
}

// serveSecretRemove answers DELETE /secrets/{id}. A secret the manager does not know is the
// 404 it gives; a secret any service's spec references, whatever that service's tasks are
// doing, is the 400 "is in use by the following service" it gives (RESEARCH E16). Otherwise
// the secret leaves every list. The write is recorded whatever the answer.
func (f *Fake) serveSecretRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := secretPath.FindStringSubmatch(p)
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
	fs, ok := cfg.resolveSecret(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "secret " + m[1] + " not found"})
		return true
	}
	if msg, bad := cfg.RemoveErrors[fs.Name]; bad {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": msg})
		return true
	}
	for _, s := range cfg.serviceList(false) {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, ref := range cs.Secrets {
			if ref != nil && (ref.SecretID == fs.ID || ref.SecretName == fs.Name) {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"message": "secret '" + fs.Name + "' is in use by the following service: " + s.Spec.Name})
				return true
			}
		}
	}
	f.mu.Lock()
	if f.secretGone == nil {
		f.secretGone = map[string]bool{}
	}
	f.secretGone[fs.ID] = true
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}
