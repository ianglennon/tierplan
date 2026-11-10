// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package dnstest holds fakes for the DNS side of DNS-01 (Phase 15, D-19): a Cloudflare v4 API
// served over TLS from memory, with the failures a token and an account can produce.
//
// The package is imported by tests only. Nothing in it is linked into the tierplan binary.
package dnstest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TXTSink is told about every TXT record the fake creates and deletes, so a DNS responder can serve
// what the fake holds.
type TXTSink interface {
	AddTXT(fqdn, value string)
	RemoveTXT(fqdn, value string)
}

// FakeZone is a zone the fake holds.
type FakeZone struct {
	ID          string
	Name        string
	NameServers []string
}

// FakeRecord is a copy of a record the fake holds.
type FakeRecord struct {
	ID, Zone, Name, Content, Comment string
}

// Value is the record's content with one pair of surrounding double quotes removed.
func (r FakeRecord) Value() string { return unquote(r.Content) }

// Request is one request the fake received. It never holds the token or any header value.
type Request struct {
	Method, Path, Query string
	// Bearer is set when the request carried an Authorization: Bearer header.
	Bearer bool
	// LegacyAuth is set when it carried X-Auth-Key or X-Auth-Email, which TierPlan never sends.
	LegacyAuth bool
}

// Operations a FakeResponse can be scripted for.
const (
	// OpZones is a zone lookup by name: GET /zones?name=...
	OpZones = "zones"
	// OpZonesAll is the unfiltered zone list: GET /zones
	OpZonesAll = "zones_all"
	OpCreate   = "create"
	OpGet      = "get"
	OpDelete   = "delete"
)

// FakeResponse is a scripted answer. It replaces the fake's own answer to the operation it is
// scripted for.
type FakeResponse struct {
	// Status is the HTTP status.
	Status int
	// Code and Message are the one entry of the errors array; Chain, when set, is the code of one
	// entry in its error_chain.
	Code    int
	Message string
	Chain   int
	// RetryAfter, when set, is sent as the Retry-After header.
	RetryAfter string
	// Body, when set, is sent as is instead of a Cloudflare error document.
	Body string
	// Times is how many calls get this answer; 0 means every call.
	Times int
}

type scripted struct {
	FakeResponse
	used int
}

// FakeCloudflare is an in-memory Cloudflare v4 API on an httptest TLS server.
type FakeCloudflare struct {
	// Token is the one bearer the fake accepts.
	Token string
	// Sink, when set, is told about every record created and deleted through the API.
	Sink TXTSink

	srv *httptest.Server

	mu      sync.Mutex
	zones   []FakeZone
	records []FakeRecord
	reqs    []Request

	scripts  map[string][]*scripted
	limited  bool
	visible  map[string]bool
	redirect string
	huge     map[string]bool
}

// NewFakeCloudflare starts the fake. It is closed when the test ends.
func NewFakeCloudflare(t testing.TB) *FakeCloudflare {
	t.Helper()
	f := &FakeCloudflare{Token: "cf-test-token-" + randHex(8)}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the API root to give the client as BaseURL.
func (f *FakeCloudflare) URL() string { return f.srv.URL + "/client/v4" }

// Client is an HTTP client that trusts the fake's certificate.
func (f *FakeCloudflare) Client() *http.Client { return f.srv.Client() }

// Close stops the server. Closing twice is harmless.
func (f *FakeCloudflare) Close() { f.srv.Close() }

// AddZone adds a zone and returns its ID.
func (f *FakeCloudflare) AddZone(name string, nameservers ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := FakeZone{ID: randHex(16), Name: strings.ToLower(name), NameServers: nameservers}
	f.zones = append(f.zones, z)
	return z.ID
}

// AddForeignTXT adds a record TierPlan did not create, in the zone that holds name.
func (f *FakeCloudflare) AddForeignTXT(name, content string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	best := ""
	for _, z := range f.zones {
		if (strings.EqualFold(name, z.Name) || strings.HasSuffix(strings.ToLower(name), "."+z.Name)) && len(z.Name) > len(best) {
			best = z.ID
		}
	}
	r := FakeRecord{ID: randHex(16), Zone: best, Name: name, Content: content}
	f.records = append(f.records, r)
	return r.ID
}

// RemoveZone removes a zone, so its ID becomes one Cloudflare does not know.
func (f *FakeCloudflare) RemoveZone(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name = strings.ToLower(name)
	f.zones = slices.DeleteFunc(f.zones, func(z FakeZone) bool { return z.Name == name })
}

// LimitZones makes the token a zone-scoped one: the filtered and the unfiltered zone lists show
// only the named zones. With no names it sees no zone at all.
func (f *FakeCloudflare) LimitZones(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limited = true
	f.visible = map[string]bool{}
	for _, n := range names {
		f.visible[strings.ToLower(n)] = true
	}
}

// Script answers op with r from now on (or for r.Times calls). Scripts for one op are used in the
// order they were added.
func (f *FakeCloudflare) Script(op string, r FakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scripts == nil {
		f.scripts = map[string][]*scripted{}
	}
	f.scripts[op] = append(f.scripts[op], &scripted{FakeResponse: r})
}

// RedirectTo makes every request answer 302 to url.
func (f *FakeCloudflare) RedirectTo(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirect = url
}

// HugeBody makes op answer with a body over 1 MiB.
func (f *FakeCloudflare) HugeBody(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.huge == nil {
		f.huge = map[string]bool{}
	}
	f.huge[op] = true
}

// Records is a copy of every record the fake holds.
func (f *FakeCloudflare) Records() []FakeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeRecord(nil), f.records...)
}

// Requests is a copy of every request the fake received, in order.
func (f *FakeCloudflare) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

func (f *FakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.reqs = append(f.reqs, Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Bearer:     strings.HasPrefix(auth, "Bearer "),
		LegacyAuth: r.Header.Get("X-Auth-Key") != "" || r.Header.Get("X-Auth-Email") != "",
	})
	redirect := f.redirect
	f.mu.Unlock()

	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}
	if f.intercept(w, r) {
		return
	}
	if auth != "Bearer "+f.Token {
		writeErr(w, http.StatusForbidden, 9109, "Invalid access token")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "zones" && r.Method == http.MethodGet:
		f.listZones(w, r)
	case len(parts) == 3 && parts[0] == "zones" && parts[2] == "dns_records" && r.Method == http.MethodPost:
		f.createRecord(w, r, parts[1])
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "dns_records" && r.Method == http.MethodGet:
		f.getRecord(w, parts[1], parts[3])
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "dns_records" && r.Method == http.MethodDelete:
		f.deleteRecord(w, parts[1], parts[3])
	default:
		writeErr(w, http.StatusNotFound, 7000, "No route for that URI")
	}
}

// opOf names the operation a request is.
func opOf(r *http.Request) string {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/client/v4"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "zones" && r.URL.Query().Get("name") != "":
		return OpZones
	case len(parts) == 1 && parts[0] == "zones":
		return OpZonesAll
	case len(parts) == 3 && r.Method == http.MethodPost:
		return OpCreate
	case len(parts) == 4 && r.Method == http.MethodGet:
		return OpGet
	case len(parts) == 4 && r.Method == http.MethodDelete:
		return OpDelete
	}
	return ""
}

// intercept answers a request from a script or the huge-body switch. It reports whether it did.
func (f *FakeCloudflare) intercept(w http.ResponseWriter, r *http.Request) bool {
	op := opOf(r)
	f.mu.Lock()
	var hit *FakeResponse
	for _, sc := range f.scripts[op] {
		if sc.Times > 0 && sc.used >= sc.Times {
			continue
		}
		sc.used++
		hit = &sc.FakeResponse
		break
	}
	huge := f.huge[op]
	f.mu.Unlock()

	switch {
	case hit != nil:
		if hit.RetryAfter != "" {
			w.Header().Set("Retry-After", hit.RetryAfter)
		}
		if hit.Body != "" {
			w.WriteHeader(hit.Status)
			_, _ = w.Write([]byte(hit.Body))
			return true
		}
		e := map[string]any{"code": hit.Code, "message": hit.Message}
		if hit.Chain != 0 {
			e["error_chain"] = []any{map[string]any{"code": hit.Chain, "message": "chain"}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(hit.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{e}, "messages": []any{}, "result": nil})
		return true
	case huge:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":[],"pad":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", 2<<20)))
		_, _ = w.Write([]byte(`"}`))
		return true
	}
	return false
}

func (f *FakeCloudflare) listZones(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.URL.Query().Get("name"))
	f.mu.Lock()
	out := []map[string]any{}
	for _, z := range f.zones {
		if name != "" && z.Name != name {
			continue
		}
		if f.limited && !f.visible[z.Name] {
			continue
		}
		out = append(out, map[string]any{"id": z.ID, "name": z.Name, "name_servers": z.NameServers, "status": "active"})
	}
	f.mu.Unlock()
	writeOK(w, out)
}

func (f *FakeCloudflare) hasZoneLocked(id string) bool {
	for _, z := range f.zones {
		if z.ID == id {
			return true
		}
	}
	return false
}

func (f *FakeCloudflare) createRecord(w http.ResponseWriter, r *http.Request, zoneID string) {
	var in struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		TTL     int    `json:"ttl"`
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, 9207, "Request body is invalid")
		return
	}
	f.mu.Lock()
	if !f.hasZoneLocked(zoneID) {
		f.mu.Unlock()
		writeErr(w, http.StatusNotFound, 7003, "Could not route to /zones/"+zoneID+"/dns_records, perhaps your object identifier is invalid?")
		return
	}
	for _, have := range f.records {
		if have.Zone == zoneID && strings.EqualFold(have.Name, in.Name) && have.Value() == unquote(in.Content) {
			f.mu.Unlock()
			writeErr(w, http.StatusBadRequest, 81058, "An identical record already exists.")
			return
		}
	}
	rec := FakeRecord{ID: randHex(16), Zone: zoneID, Name: in.Name, Content: in.Content, Comment: in.Comment}
	f.records = append(f.records, rec)
	sink := f.Sink
	f.mu.Unlock()
	if sink != nil {
		sink.AddTXT(rec.Name, rec.Value())
	}
	writeOK(w, recordView(rec))
}

func (f *FakeCloudflare) findRecordLocked(zoneID, id string) (int, bool) {
	for i, rec := range f.records {
		if rec.ID == id && rec.Zone == zoneID {
			return i, true
		}
	}
	return 0, false
}

func (f *FakeCloudflare) getRecord(w http.ResponseWriter, zoneID, id string) {
	f.mu.Lock()
	if !f.hasZoneLocked(zoneID) {
		f.mu.Unlock()
		writeErr(w, http.StatusNotFound, 7003, "Could not route to /zones/"+zoneID+"/dns_records/"+id+", perhaps your object identifier is invalid?")
		return
	}
	i, ok := f.findRecordLocked(zoneID, id)
	var rec FakeRecord
	if ok {
		rec = f.records[i]
	}
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	writeOK(w, recordView(rec))
}

func (f *FakeCloudflare) deleteRecord(w http.ResponseWriter, zoneID, id string) {
	f.mu.Lock()
	i, ok := f.findRecordLocked(zoneID, id)
	var rec FakeRecord
	if ok {
		rec = f.records[i]
		f.records = append(f.records[:i:i], f.records[i+1:]...)
	}
	sink := f.Sink
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	if sink != nil {
		sink.RemoveTXT(rec.Name, rec.Value())
	}
	writeOK(w, map[string]any{"id": rec.ID})
}

func recordView(r FakeRecord) map[string]any {
	return map[string]any{
		"id": r.ID, "type": "TXT", "name": r.Name, "content": r.Content,
		"comment": r.Comment, "ttl": 60, "zone_id": r.Zone,
	}
}

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
}

func writeErr(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false, "errors": []any{map[string]any{"code": code, "message": msg}},
		"messages": []any{}, "result": nil,
	})
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
