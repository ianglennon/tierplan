// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// The layout under the node root (D-12, D-14, D-39). dynamic/ holds exactly one file and is written only by
// one rename; every temporary file lives in tmp/, outside the directory Traefik watches.
const (
	dirDynamic = ingressbundle.DynamicDir
	dirCerts   = ingressbundle.CertsDir
	dirState   = "state"
	dirTmp     = "tmp"
	dirStatic  = "static"

	fileStatic  = "static/traefik.yml"
	filePending = "state/pending.json"
	fileApplied = "state/applied.json"

	dirMode     fs.FileMode = 0o700
	recordMode  fs.FileMode = 0o600
	tempPrefix              = ".part-"
	stepCerts               = "certs"
	stepPending             = "pending"
	stepDynamic             = "dynamic"
	stepCommit              = "commit"
	stepGC                  = "gc"
)

// record is what state/pending.json and state/applied.json hold: the identity and manifest of a bundle,
// and the certificate paths of the bundle it replaced, so a restart that finds a verified pending record
// can finish the work, certificate cleanup included. It holds no file data and no key.
type record struct {
	ID            string                 `json:"id"`
	Schema        int                    `json:"schema"`
	Version       ingressbundle.Version  `json:"version"`
	Manifest      ingressbundle.Manifest `json:"manifest"`
	PreviousCerts []string               `json:"previousCerts"`
}

// certsOf lists the certs/ paths of a manifest, sorted.
func certsOf(m ingressbundle.Manifest) []string {
	var out []string
	for _, e := range m.Files {
		if e.Step == ingressbundle.StepCerts {
			out = append(out, e.Path)
		}
	}
	slices.Sort(out)
	return out
}

// ApplyError is a failure to put a verified bundle on the volume. Step names where it stopped: certs,
// pending, dynamic, commit or gc.
type ApplyError struct {
	Step string
	Err  error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("ingress cache: apply, step %s: %v", e.Step, e.Err)
}

func (e *ApplyError) Unwrap() error { return e.Err }

// Store is the node's volume: what Traefik reads and what the supervisor serves after a restart. All
// disk access goes through the injected filesystem.
type Store struct {
	mu      sync.Mutex
	fsys    fsx.FS
	root    string
	loaded  bool
	applied *record
	gcErr   error
}

// NewStore returns a Store over root, which is ingressbundle.NodeRoot in production.
func NewStore(fsys fsx.FS, root string) *Store {
	return &Store{fsys: fsys, root: root}
}

// abs is the OS path of a slash path relative to the root.
func (s *Store) abs(rel string) string {
	return filepath.Join(s.root, filepath.FromSlash(rel))
}

// Prepare makes the root and its five directories, each 0700, and clears the temporary files a killed
// apply left in tmp/. It runs once at start, before Load.
func (s *Store) Prepare() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range []string{"", dirDynamic, dirCerts, dirState, dirTmp, dirStatic} {
		if err := s.fsys.MkdirAll(s.abs(d), dirMode); err != nil {
			return fmt.Errorf("ingress cache: make %s: %w", s.abs(d), err)
		}
	}
	entries, err := s.fsys.ReadDir(s.abs(dirTmp))
	if err != nil {
		return fmt.Errorf("ingress cache: read %s: %w", s.abs(dirTmp), err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), tempPrefix) {
			if err := s.fsys.Remove(filepath.Join(s.abs(dirTmp), e.Name())); err != nil {
				return fmt.Errorf("ingress cache: remove a leftover temporary file: %w", err)
			}
		}
	}
	return nil
}

// WriteStatic writes Traefik's static configuration to static/traefik.yml atomically. The supervisor
// writes it once at start, before Traefik runs.
func (s *Store) WriteStatic(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fsx.WriteAtomic(s.fsys, s.abs(dirTmp), s.abs(fileStatic), data, ingressbundle.ModeConfig); err != nil {
		return fmt.Errorf("ingress cache: write the static configuration: %w", err)
	}
	return nil
}

// AppliedID is the ID of the bundle in force, or "" when none is.
func (s *Store) AppliedID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applied == nil {
		return ""
	}
	return s.applied.ID
}

// GCErr is the error of the last certificate cleanup, or nil when it succeeded. A failed cleanup leaves
// extra certificates on the volume and never fails an apply, so the caller logs it.
func (s *Store) GCErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcErr
}

// readRecord reads and parses one record file. A missing file is (nil, nil).
func (s *Store) readRecord(rel string) (*record, error) {
	raw, err := s.fsys.ReadFile(s.abs(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// bundleFromDisk reads every file a record's manifest lists and returns the bundle they make, which may
// not verify.
func (s *Store) bundleFromDisk(r *record) (ingressbundle.Bundle, error) {
	files := make([]ingressbundle.File, 0, len(r.Manifest.Files))
	for _, e := range r.Manifest.Files {
		// A manifest path that climbs out of the root is never read; Check would refuse it as a path fault.
		if !filepath.IsLocal(filepath.FromSlash(e.Path)) {
			return ingressbundle.Bundle{}, fmt.Errorf("ingress cache: manifest path %q is not local", e.Path)
		}
		data, err := s.fsys.ReadFile(s.abs(e.Path))
		if err != nil {
			return ingressbundle.Bundle{}, err
		}
		files = append(files, ingressbundle.File{Path: e.Path, Data: data})
	}
	return ingressbundle.Bundle{Schema: r.Schema, ID: r.ID, Version: r.Version, Manifest: r.Manifest, Files: files}, nil
}

// verified returns the bundle a record names when the files on disk are exactly it and it passes Check.
func (s *Store) verified(r *record) (ingressbundle.Bundle, bool) {
	b, err := s.bundleFromDisk(r)
	if err != nil {
		return ingressbundle.Bundle{}, false
	}
	if err := Check(b); err != nil {
		return ingressbundle.Bundle{}, false
	}
	return b, true
}

// Load finds the bundle the volume serves. It tries state/pending.json first and then state/applied.json,
// and takes the first whose files on disk match its manifest and pass Check. A verified pending record is
// completed: renamed to applied.json, and the certificates that neither bundle needs removed. When neither
// record verifies Load returns no bundle and changes nothing: no partial serve and no deletion (D-13).
//
// A non-nil error with ok true means the bundle is in force but the pending record could not be completed
// (the next Load tries again). A non-nil error with ok false means a record could not be read for a reason
// other than not existing.
func (s *Store) Load() (ingressbundle.Bundle, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (ingressbundle.Bundle, bool, error) {
	s.loaded = true
	var readErr error
	for _, rel := range []string{filePending, fileApplied} {
		r, err := s.readRecord(rel)
		if err != nil {
			// A record that cannot be parsed is as good as none; one that cannot be read is worth saying.
			var syn *json.SyntaxError
			var typ *json.UnmarshalTypeError
			if readErr == nil && !errors.As(err, &syn) && !errors.As(err, &typ) {
				readErr = fmt.Errorf("ingress cache: read %s: %w", rel, err)
			}
			continue
		}
		if r == nil {
			continue
		}
		b, ok := s.verified(r)
		if !ok {
			continue
		}
		s.applied = r
		if rel == filePending {
			if err := s.completeLocked(r); err != nil {
				return b, true, err
			}
		}
		return b, true, nil
	}
	s.applied = nil
	return ingressbundle.Bundle{}, false, readErr
}

// completeLocked renames a verified pending record to applied.json and runs the certificate cleanup.
func (s *Store) completeLocked(r *record) error {
	if err := s.commit(); err != nil {
		return err
	}
	s.gc(r)
	return nil
}

func (s *Store) commit() error {
	if err := s.fsys.Rename(s.abs(filePending), s.abs(fileApplied)); err != nil {
		return &ApplyError{Step: stepCommit, Err: err}
	}
	if err := s.fsys.SyncDir(s.abs(dirState)); err != nil {
		return &ApplyError{Step: stepCommit, Err: err}
	}
	return nil
}

// gc removes every file in certs/ that neither the bundle r names nor the bundle it replaced names, so a
// Traefik that is still reading the previous YAML finds its certificates. A failure is kept for GCErr and
// is not an apply failure: the bundle is already live.
func (s *Store) gc(r *record) {
	s.gcErr = nil
	keep := make(map[string]bool)
	for _, p := range certsOf(r.Manifest) {
		keep[p] = true
	}
	for _, p := range r.PreviousCerts {
		keep[p] = true
	}
	entries, err := s.fsys.ReadDir(s.abs(dirCerts))
	if err != nil {
		s.gcErr = &ApplyError{Step: stepGC, Err: err}
		return
	}
	removed := false
	for _, e := range entries {
		if e.IsDir() || keep[dirCerts+"/"+e.Name()] {
			continue
		}
		if err := s.fsys.Remove(filepath.Join(s.abs(dirCerts), e.Name())); err != nil {
			s.gcErr = &ApplyError{Step: stepGC, Err: err}
			return
		}
		removed = true
	}
	if removed {
		if err := s.fsys.SyncDir(s.abs(dirCerts)); err != nil {
			s.gcErr = &ApplyError{Step: stepGC, Err: err}
		}
	}
}

// Apply checks a bundle and puts it in force. The order is fixed (D-31): each certificate and key not
// already on the volume with the same content is written to tmp/, flushed, set to its manifest mode and
// renamed into certs/, in manifest path order, and certs/ is flushed; state/pending.json is written by
// temp and rename and state/ flushed; the dynamic file is written to tmp/, flushed and renamed to
// dynamic/tierplan.yml, and dynamic/ flushed; pending.json is renamed to applied.json and state/ flushed;
// then the certificates that neither this bundle nor the one it replaced names are removed. Nothing but
// that one rename ever writes into dynamic/, and Traefik is not signalled: its file provider reloads on
// the rename. A rename that fails, across devices included, fails the apply; no file is copied instead.
//
// A failed Check is returned as the *ingressbundle.VerifyError and nothing is written. Any other failure
// is an *ApplyError naming the step. A failure after the dynamic rename (the flush of dynamic/, the commit)
// is an *ApplyError too, but the bundle is in force by then: AppliedID already names it. A failure of the final cleanup is not an error of Apply, because the
// bundle is live by then: it is kept for GCErr.
func (s *Store) Apply(b ingressbundle.Bundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(b)
}

func (s *Store) applyLocked(b ingressbundle.Bundle) error {
	if err := Check(b); err != nil {
		return err
	}
	if !s.loaded {
		// The bundle in force decides which certificates stay; a Store that was not loaded asks the volume.
		_, _, _ = s.loadLocked()
	}
	data := make(map[string][]byte, len(b.Files))
	for _, f := range b.Files {
		data[f.Path] = f.Data
	}
	entries := slices.Clone(b.Manifest.Files)
	slices.SortFunc(entries, func(x, y ingressbundle.FileEntry) int { return strings.Compare(x.Path, y.Path) })

	// Step 1: certificates and keys, before any file that names them.
	wrote := false
	for _, e := range entries {
		if e.Step != ingressbundle.StepCerts {
			continue
		}
		dst := s.abs(e.Path)
		if cur, err := s.fsys.ReadFile(dst); err == nil && bytes.Equal(cur, data[e.Path]) {
			continue
		}
		if err := fsx.WriteAtomicNoSync(s.fsys, s.abs(dirTmp), dst, data[e.Path], fs.FileMode(e.Mode)); err != nil {
			return &ApplyError{Step: stepCerts, Err: err}
		}
		wrote = true
	}
	if wrote {
		if err := s.fsys.SyncDir(s.abs(dirCerts)); err != nil {
			return &ApplyError{Step: stepCerts, Err: err}
		}
	}

	// The record, so a kill from here on can be recovered.
	rec := record{ID: b.ID, Schema: b.Schema, Version: b.Version, Manifest: b.Manifest}
	if s.applied != nil {
		rec.PreviousCerts = certsOf(s.applied.Manifest)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return &ApplyError{Step: stepPending, Err: err}
	}
	if err := fsx.WriteAtomic(s.fsys, s.abs(dirTmp), s.abs(filePending), raw, recordMode); err != nil {
		return &ApplyError{Step: stepPending, Err: err}
	}

	// Step 2: the one rename that Traefik reloads on.
	var mode fs.FileMode = ingressbundle.ModeConfig
	for _, e := range entries {
		if e.Path == ingressbundle.DynamicFile {
			mode = fs.FileMode(e.Mode)
		}
	}
	if err := fsx.WriteAtomicNoSync(s.fsys, s.abs(dirTmp), s.abs(ingressbundle.DynamicFile), data[ingressbundle.DynamicFile], mode); err != nil {
		return &ApplyError{Step: stepDynamic, Err: err}
	}
	// From the rename the volume serves this bundle (Traefik's file provider reloads on it), whatever
	// happens to the directory flush or the commit, so the state in memory follows the rename and not
	// the flush: a node that reported the previous bundle while serving this one would contradict what
	// it tells TierPlan and would change behaviour across a restart.
	s.applied = &rec
	if err := s.fsys.SyncDir(s.abs(dirDynamic)); err != nil {
		return &ApplyError{Step: stepDynamic, Err: fmt.Errorf("flush %s: %w", s.abs(dirDynamic), err)}
	}

	if err := s.commit(); err != nil {
		return err
	}
	s.gc(&rec)
	return nil
}

// Accept decodes a frame, checks it and applies it, and answers with the bundle ID and the reason it was
// refused: "" when the bundle is in force. The ID is the bundle's own, or for a frame that could not be
// decoded the safe ID ingresswire.PeekBundleID finds ("" when there is none). A bundle whose ID is the one
// in force is answered applied again without a single write (D-16); any other bundle is applied whatever
// its version, so a restore of an older configuration works. A refused bundle changes nothing under
// certs/ or dynamic/.
//
// cause is set only when reason is apply_failed: the *ApplyError naming the step and the errno, for the
// caller to log. It holds a step name, paths under the node root and the OS error, never bundle bytes
// (D-23). The verify codes need no cause: the reason is the whole story.
func (s *Store) Accept(frame []byte, pinned string) (id, reason string, cause error) {
	b, err := ingresswire.DecodeBundle(frame, pinned)
	if err != nil {
		return ingresswire.PeekBundleID(frame), ReasonOf(err), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		_, _, _ = s.loadLocked()
	}
	if s.applied != nil && s.applied.ID == b.ID {
		return b.ID, "", nil
	}
	if err := s.applyLocked(b); err != nil {
		var ve *ingressbundle.VerifyError
		if errors.As(err, &ve) {
			return b.ID, ReasonOf(err), nil
		}
		return b.ID, ingressbundle.ReasonApplyFailed, err
	}
	return b.ID, "", nil
}
