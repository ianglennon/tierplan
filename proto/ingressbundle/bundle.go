// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingressbundle is the bundle contract shared by TierPlan and the
// ingress supervisor. TierPlan builds a bundle in internal/ingressrender; the
// supervisor on each node verifies it and writes it to disk. The two live in
// different Go modules and the supervisor cannot import internal/, so the types,
// the node layout, the checksums and the content-derived ID are here. Standard
// library only; it must not import the wire package.
//
// A bundle is the whole of what one node serves: a set of files, a manifest that
// lists each file with its checksum, mode and write step, the readable identity
// of each route, and an ID derived from the content alone. The composite
// Version says which model state a bundle was rendered from; it is metadata and
// never part of the ID, so an unchanged model never makes a node reload.
//
// The contract is one-way. Once a supervisor in the field reports schema 1, any
// change to the layout, the manifest shape, the ID encoding or the static file's
// expectations needs a new schema number (D-05).
package ingressbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

const (
	// Schema1 is the first bundle schema. A bundle carries its schema as an
	// integer and ComputeID includes it. No node applies it any more (SupportedSchemas is [Schema2]):
	// it stays defined as the name of the old schema a node may still report or a test may offer.
	Schema1 = 1
	// Schema2 is the schema of Phase 28. The static file's redirect runs at priority 1, and the dynamic
	// file may carry a web-entry router for TierPlan's own challenge and a t-self transport whose rootCAs
	// name a bundle file. A proxy image that knows only Schema1 must not apply it, so it freezes on its
	// cache instead (A-01, D-13).
	Schema2 = 2

	// NodeRoot is the directory on each node that holds everything the
	// supervisor writes. It is mounted into the Traefik container at the same
	// absolute path, so the YAML can name a certificate by an absolute path.
	NodeRoot = "/var/lib/tierplan-ingress"
	// DynamicDir is the one directory Traefik watches, relative to NodeRoot. It
	// holds exactly one file, and no temporary file may be put in it.
	DynamicDir = "dynamic"
	// CertsDir holds certificate chains and keys, relative to NodeRoot. It is
	// outside the watched directory.
	CertsDir = "certs"
	// DynamicFile is the single dynamic file: every router, service,
	// middleware, serversTransport, the TLS options and the certificate list.
	DynamicFile = DynamicDir + "/tierplan.yml"
	// CertRoot is where the YAML finds certificates and keys, as an absolute
	// path. A relative path in Traefik's YAML loads nothing.
	CertRoot = NodeRoot + "/" + CertsDir
	// DynamicRoot is the absolute path of the watched directory.
	DynamicRoot = NodeRoot + "/" + DynamicDir

	// ModeConfig is the file mode of everything but a private key.
	ModeConfig = 0o644
	// ModeKey is the file mode of a private key (a path ending in .key).
	ModeKey = 0o600

	// StepCerts is the write step of every file under certs/. The supervisor
	// writes every step-1 file before any step-2 file, so a certificate is on
	// disk before the YAML that names it.
	StepCerts = 1
	// StepDynamic is the write step of the dynamic file. Write each step-2 file
	// to a temporary file outside the watched directory (on the same
	// filesystem, with a name that does not end in .yml, .yaml or .toml), flush
	// it, then rename it into place: one rename reloads a whole bundle.
	StepDynamic = 2

	// idPrefix starts the bytes ComputeID hashes. It names the encoding.
	idPrefix = "tierplan-ingress-bundle\n"
)

// Version says which states of the model a bundle was rendered from. It is
// metadata only and never enters the bundle ID.
type Version struct {
	// Config is the ingress configuration version.
	Config int64 `json:"config"`
	// Certs is the certificate set version.
	Certs int64 `json:"certs"`
	// Own is the version of the node's own settings.
	Own int64 `json:"own"`
}

// File is one file of a bundle. Path is relative to NodeRoot.
type File struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// FileEntry is a manifest line for one file.
type FileEntry struct {
	// Path is relative to NodeRoot and matches a File's Path.
	Path string `json:"path"`
	// SHA256 is the lower-case hex SHA-256 of the file's data.
	SHA256 string `json:"sha256"`
	// Mode is the file mode: ModeKey for a .key file, else ModeConfig.
	Mode uint32 `json:"mode"`
	// Step is the write step: StepCerts under certs/, StepDynamic for the
	// dynamic file.
	Step int `json:"step"`
}

// RouteEntry is the readable identity of one route in the bundle. Traefik names
// carry only hashes (D-18); this keeps the hostname and stack in view for a
// person reading the manifest. Hash is the full route hash.
type RouteEntry struct {
	Router     string `json:"router"`
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"pathPrefix"`
	Stack      string `json:"stack"`
	Service    string `json:"service"`
	Hash       string `json:"hash"`
}

// Manifest lists a bundle's files and routes. Routes are informational and are
// not part of the ID.
type Manifest struct {
	Schema int          `json:"schema"`
	Files  []FileEntry  `json:"files"`
	Routes []RouteEntry `json:"routes"`
}

// Bundle is the complete, self-verifying input of one node.
type Bundle struct {
	Schema   int      `json:"schema"`
	ID       string   `json:"id"`
	Version  Version  `json:"version"`
	Manifest Manifest `json:"manifest"`
	Files    []File   `json:"files"`
}

// Checksum returns the lower-case hex SHA-256 of data.
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ComputeID returns the bundle ID: the lower-case hex SHA-256 of the bytes
// "tierplan-ingress-bundle\n", then "schema <n>\n", then for each entry sorted
// by Path the line "<path> <sha256>\n". It depends on the schema and on each
// file's path and checksum, and on nothing else: no version, time or random
// value (PROH-21-07), so the same content always has the same ID.
func ComputeID(schema int, entries []FileEntry) string {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%sschema %d\n", idPrefix, schema)
	for _, e := range sorted {
		_, _ = fmt.Fprintf(h, "%s %s\n", e.Path, e.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// modeFor is the file mode a path must have.
func modeFor(p string) uint32 {
	if strings.HasSuffix(p, ".key") {
		return ModeKey
	}
	return ModeConfig
}

// stepFor is the write step a path must have, or 0 for a path in neither
// directory (Verify refuses such a path).
func stepFor(p string) int {
	switch {
	case strings.HasPrefix(p, CertsDir+"/"):
		return StepCerts
	case strings.HasPrefix(p, DynamicDir+"/"):
		return StepDynamic
	}
	return 0
}

// Build makes a verified bundle of the files. It computes each file's checksum,
// mode and step, sorts the files and entries by path and the routes by router,
// and derives the ID. It keeps the caller's data slices. A bundle that does not
// verify, including one with a path outside the layout or a schema below 1, is
// returned as the error of Verify.
func Build(schema int, v Version, files []File, routes []RouteEntry) (Bundle, error) {
	fs := slices.Clone(files)
	slices.SortFunc(fs, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	entries := make([]FileEntry, 0, len(fs))
	for _, f := range fs {
		entries = append(entries, FileEntry{
			Path:   f.Path,
			SHA256: Checksum(f.Data),
			Mode:   modeFor(f.Path),
			Step:   stepFor(f.Path),
		})
	}
	rs := slices.Clone(routes)
	if rs == nil {
		rs = []RouteEntry{}
	}
	slices.SortStableFunc(rs, func(a, b RouteEntry) int {
		return strings.Compare(a.Router+"\x00"+a.Hostname+"\x00"+a.PathPrefix, b.Router+"\x00"+b.Hostname+"\x00"+b.PathPrefix)
	})
	b := Bundle{
		Schema:   schema,
		ID:       ComputeID(schema, entries),
		Version:  v,
		Manifest: Manifest{Schema: schema, Files: entries, Routes: rs},
		Files:    fs,
	}
	if err := b.Verify(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// paths lists the paths of the bundle's files, never their data.
func (b Bundle) paths() []string {
	out := make([]string, 0, len(b.Files))
	for _, f := range b.Files {
		out = append(out, f.Path)
	}
	return out
}

// String prints the first 12 characters of the ID, the schema, the version and
// the file paths. It never prints file data: a bundle holds private keys.
func (b Bundle) String() string {
	id := b.ID
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("bundle %s schema %d version %d/%d/%d files [%s]",
		id, b.Schema, b.Version.Config, b.Version.Certs, b.Version.Own, strings.Join(b.paths(), " "))
}

// GoString is String, so %#v shows no file data either.
func (b Bundle) GoString() string { return b.String() }

// LogValue is a group of the ID, the schema, the version and the paths, so a
// bundle passed to slog never writes file data.
func (b Bundle) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", b.ID),
		slog.Int("schema", b.Schema),
		slog.Group("version",
			slog.Int64("config", b.Version.Config),
			slog.Int64("certs", b.Version.Certs),
			slog.Int64("own", b.Version.Own)),
		slog.Any("paths", b.paths()),
	)
}

// String prints the path and the byte length, never the data.
func (f File) String() string { return fmt.Sprintf("file %s (%d bytes)", f.Path, len(f.Data)) }

// GoString is String, so %#v shows no data either.
func (f File) GoString() string { return f.String() }

// LogValue is a group of the path and the byte length. Without it a File given
// to a slog JSON handler would be written with its data as base64.
func (f File) LogValue() slog.Value {
	return slog.GroupValue(slog.String("path", f.Path), slog.Int("bytes", len(f.Data)))
}
