// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnstest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// Call is one call the FakeProvider received. It never holds a token.
type Call struct {
	// Op is "zone", "create", "get" or "delete".
	Op string
	// Name is the name asked about (zone lookup, create) and RecordID the record (get, delete).
	Name, ZoneID, RecordID string
}

// FakeProvider is an in-memory dns.Provider for tests (CHAL-03): it holds zones and TXT records,
// tells a TXTSink about every record it holds, can be scripted to fail, and deletes only by record
// ID, as a real provider's delete does. A second provider is proven to fit the seam when a whole
// order runs on it with no change to the ACME core.
type FakeProvider struct {
	// Sink, when set, is told about every record created, deleted or added as foreign.
	Sink TXTSink

	mu      sync.Mutex
	zones   []dns.Zone
	hidden  map[string]bool
	records []FakeRecord
	calls   []Call
	nextID  int

	createErr  error
	deleteErr  error
	deleteLeft int // failures still to serve; negative means every delete fails
}

// NewFakeProvider is an empty provider.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{hidden: map[string]bool{}}
}

// AddZone adds a zone with the nameservers a propagation check asks, and returns its ID.
func (f *FakeProvider) AddZone(name string, nameservers ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	z := dns.Zone{ID: fmt.Sprintf("zone-%d", f.nextID), Name: strings.ToLower(name), NameServers: slices.Clone(nameservers)}
	f.zones = append(f.zones, z)
	return z.ID
}

// HideZone makes the token unable to see the zone, as a zone-scoped token does not see another.
func (f *FakeProvider) HideZone(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hidden[strings.ToLower(name)] = true
}

// Factory builds providers over this fake. A token other than acceptToken is a token the provider
// does not accept: every call fails with dns_token_invalid.
func (f *FakeProvider) Factory(acceptToken string) dns.Factory {
	return func(token string) dns.Provider { return &fakeHandle{f: f, ok: token == acceptToken} }
}

// Records is a copy of every record the fake holds.
func (f *FakeProvider) Records() []FakeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.records)
}

// AddForeignTXT adds a record TierPlan did not create, in the zone that holds name, and returns its ID.
func (f *FakeProvider) AddForeignTXT(name, value string) string {
	f.mu.Lock()
	z, _ := f.zoneLocked(name, true)
	f.nextID++
	r := FakeRecord{ID: fmt.Sprintf("foreign-%d", f.nextID), Zone: z.ID, Name: strings.ToLower(name), Content: `"` + value + `"`}
	f.records = append(f.records, r)
	sink := f.Sink
	f.mu.Unlock()
	if sink != nil {
		sink.AddTXT(r.Name, value)
	}
	return r.ID
}

// FailCreate makes every create fail with err; nil ends it.
func (f *FakeProvider) FailCreate(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr = err
}

// FailDelete makes the next times deletes fail with err; a negative times makes every delete fail,
// and zero ends it.
func (f *FakeProvider) FailDelete(times int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr, f.deleteLeft = err, times
}

// Calls is a copy of every call received, in order.
func (f *FakeProvider) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallCount is how many calls of op were received.
func (f *FakeProvider) CallCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Op == op {
			n++
		}
	}
	return n
}

// zoneLocked is the zone with the longest name that holds name. When includeHidden is false a hidden
// zone is not seen.
func (f *FakeProvider) zoneLocked(name string, includeHidden bool) (dns.Zone, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	var best dns.Zone
	found := false
	for _, z := range f.zones {
		if !includeHidden && f.hidden[z.Name] {
			continue
		}
		if (name == z.Name || strings.HasSuffix(name, "."+z.Name)) && (!found || len(z.Name) > len(best.Name)) {
			best, found = z, true
		}
	}
	return best, found
}

func (f *FakeProvider) hasZoneLocked(id string) bool {
	for _, z := range f.zones {
		if z.ID == id && !f.hidden[z.Name] {
			return true
		}
	}
	return false
}

type fakeHandle struct {
	f  *FakeProvider
	ok bool
}

func (h *fakeHandle) Name() string { return "fake" }

func (h *fakeHandle) record(c Call) {
	h.f.calls = append(h.f.calls, c)
}

func (h *fakeHandle) denied(op string) error {
	return &dns.Error{Provider: "fake", Op: op, Class: dns.ClassTokenInvalid, Status: 403}
}

func (h *fakeHandle) Zone(_ context.Context, fqdn string) (dns.Zone, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.record(Call{Op: "zone", Name: fqdn})
	if !h.ok {
		return dns.Zone{}, h.denied("find zone")
	}
	z, found := h.f.zoneLocked(fqdn, false)
	if !found {
		return dns.Zone{}, &dns.Error{Provider: "fake", Op: "find zone", Class: dns.ClassZoneNotFound, Host: fqdn}
	}
	return z, nil
}

func (h *fakeHandle) CreateTXT(_ context.Context, z dns.Zone, fqdn, value, comment string) (dns.RecordRef, error) {
	h.f.mu.Lock()
	h.record(Call{Op: "create", Name: fqdn, ZoneID: z.ID})
	if !h.ok {
		h.f.mu.Unlock()
		return dns.RecordRef{}, h.denied("create record")
	}
	if h.f.createErr != nil {
		err := h.f.createErr
		h.f.mu.Unlock()
		return dns.RecordRef{}, err
	}
	if !h.f.hasZoneLocked(z.ID) {
		h.f.mu.Unlock()
		return dns.RecordRef{}, &dns.Error{Provider: "fake", Op: "create record", Class: dns.ClassZoneNotFound, Host: fqdn}
	}
	name := strings.ToLower(fqdn)
	for _, have := range h.f.records {
		if have.Zone == z.ID && have.Name == name && have.Value() == value {
			h.f.mu.Unlock()
			return dns.RecordRef{}, &dns.Error{Provider: "fake", Op: "create record", Class: dns.ClassUnavailable, Status: 400, Err: dns.ErrIdentical}
		}
	}
	h.f.nextID++
	rec := FakeRecord{ID: fmt.Sprintf("rec-%d", h.f.nextID), Zone: z.ID, Name: name, Content: `"` + value + `"`, Comment: comment}
	h.f.records = append(h.f.records, rec)
	sink := h.f.Sink
	h.f.mu.Unlock()
	if sink != nil {
		sink.AddTXT(rec.Name, value)
	}
	return dns.RecordRef{ZoneID: z.ID, RecordID: rec.ID}, nil
}

func (h *fakeHandle) GetTXT(_ context.Context, ref dns.RecordRef) (dns.Record, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.record(Call{Op: "get", ZoneID: ref.ZoneID, RecordID: ref.RecordID})
	if !h.ok {
		return dns.Record{}, h.denied("read record")
	}
	for _, r := range h.f.records {
		if r.ID == ref.RecordID && r.Zone == ref.ZoneID {
			return dns.Record{Name: r.Name, Content: r.Value(), Comment: r.Comment}, nil
		}
	}
	return dns.Record{}, &dns.Error{Provider: "fake", Op: "read record", Class: dns.ClassUnavailable, Status: 404}
}

// DeleteTXT deletes by ID and nothing else. A record that is not there is success.
func (h *fakeHandle) DeleteTXT(_ context.Context, ref dns.RecordRef) error {
	h.f.mu.Lock()
	h.record(Call{Op: "delete", ZoneID: ref.ZoneID, RecordID: ref.RecordID})
	if !h.ok {
		h.f.mu.Unlock()
		return h.denied("delete record")
	}
	if h.f.deleteLeft != 0 && h.f.deleteErr != nil {
		if h.f.deleteLeft > 0 {
			h.f.deleteLeft--
		}
		err := h.f.deleteErr
		h.f.mu.Unlock()
		return err
	}
	var gone FakeRecord
	found := false
	h.f.records = slices.DeleteFunc(h.f.records, func(r FakeRecord) bool {
		if r.ID == ref.RecordID && r.Zone == ref.ZoneID {
			gone, found = r, true
			return true
		}
		return false
	})
	sink := h.f.Sink
	h.f.mu.Unlock()
	if found && sink != nil {
		sink.RemoveTXT(gone.Name, gone.Value())
	}
	return nil
}
