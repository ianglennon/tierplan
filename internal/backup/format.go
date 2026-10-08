// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
)

// The published layout (D-28). See the package documentation.
const (
	// Magic starts every backup file.
	Magic = "BAYPLANBK1\n"
	// FormatVersion is the layout version this binary writes and the highest
	// it reads.
	FormatVersion uint16 = 1
	// FileExtension is the suggested file extension, with the dot.
	FileExtension = ".bayplan"
	// MinPassphraseLen is the shortest backup passphrase accepted, counted in
	// characters. It is one constant so the server can hand it to the SPA.
	MinPassphraseLen = 16
	// MaxEntrySize caps one payload entry after decompression, so a small
	// upload cannot expand without limit (T-03-07).
	MaxEntrySize int64 = 16 << 30

	// EntryManifest and EntryDB are the two payload entries, in this order.
	EntryManifest = "manifest.json"
	EntryDB       = "bayplan.db"

	maxHeaderLen = 4096

	// argon2id bounds accepted from a header (T-03-06). The header is
	// attacker-supplied on the restore path, so they are checked before any key
	// derivation runs. They mirror the shape of the maxArgon* caps in
	// internal/auth/password.go.
	kdfName         = "argon2id"
	maxKDFTime      = 10
	maxKDFMemoryKiB = 512 * 1024
	minKDFMemoryKiB = 8 * 1024
	maxKDFThreads   = 16
	saltLen         = 16

	// drainLimit bounds what may follow the tar trailer inside the gzip
	// stream, which is nothing a writer of this package produces.
	drainLimit = 64 << 10
)

// Errors returned by the package. They are stable so the API can map each to a
// distinct problem type (D-30).
var (
	// ErrNotBackup is returned for a file that does not start with the magic.
	ErrNotBackup = errors.New("not a Bayplan backup file")
	// ErrNewerFormat is returned for a file whose format version is higher
	// than this binary understands.
	ErrNewerFormat = errors.New("backup was made by a newer Bayplan")
	// ErrHeaderLimits is returned for a header that asks for more than the
	// caps allow, before any key derivation runs.
	ErrHeaderLimits = errors.New("backup header is outside the allowed limits")
	// ErrUnreadable is returned for a damaged, truncated, extended or wrongly
	// unlocked file. A wrong passphrase and damage are deliberately not told
	// apart.
	ErrUnreadable = errors.New("backup is damaged or the passphrase is wrong")
	// ErrPassphraseTooShort is returned by NewWriter for a passphrase shorter
	// than MinPassphraseLen characters.
	ErrPassphraseTooShort = errors.New("backup passphrase is shorter than the minimum length")
)

// KDFParams are the argon2id parameters and salt stored in the header.
type KDFParams struct {
	Name string `json:"name"`
	T    uint32 `json:"t"`
	MKiB uint32 `json:"m_kib"`
	P    uint8  `json:"p"`
	Salt []byte `json:"salt"`
}

// Header is the plaintext part of a backup. It holds no instance name, URL or
// counts.
type Header struct {
	Format     uint16    `json:"format"`
	KDF        KDFParams `json:"kdf"`
	Created    time.Time `json:"created"`
	AppVersion string    `json:"app_version"`
	Schema     int64     `json:"schema"`
}

// DefaultKDF returns argon2id with t=3, m=64 MiB, p=4 (RFC 9106's second
// recommended option) and a fresh 16-byte salt.
func DefaultKDF() (KDFParams, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return KDFParams{}, fmt.Errorf("generate backup salt: %w", err)
	}
	return KDFParams{Name: kdfName, T: 3, MKiB: 64 * 1024, P: 4, Salt: salt}, nil
}

// validateKDF checks every cap on a header's argon2id parameters.
func validateKDF(p KDFParams) error {
	if p.Name != kdfName {
		return fmt.Errorf("%w: unknown key derivation %q", ErrUnreadable, p.Name)
	}
	switch {
	case p.T < 1 || p.T > maxKDFTime,
		p.MKiB < minKDFMemoryKiB || p.MKiB > maxKDFMemoryKiB,
		p.P < 1 || p.P > maxKDFThreads,
		len(p.Salt) != saltLen:
		return ErrHeaderLimits
	}
	return nil
}

// encodeHeader returns the outer header bytes: magic, version, length, JSON.
// They are what the wrap key is bound to. It checks only the length cap, so
// tests can build headers that break the other caps.
func encodeHeader(h Header) ([]byte, error) {
	body, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("encode backup header: %w", err)
	}
	if len(body) > maxHeaderLen {
		return nil, ErrHeaderLimits
	}
	out := make([]byte, 0, len(Magic)+2+4+len(body))
	out = append(out, Magic...)
	out = binary.BigEndian.AppendUint16(out, h.Format)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body))) //nolint:gosec // len(body) <= maxHeaderLen, checked above
	return append(out, body...), nil
}

// ReadHeader reads and validates the outer header (magic, version, length,
// JSON and every cap) without the passphrase and without any key derivation. It
// returns the header and the raw header bytes the wrap key is bound to. src is
// left at the first byte of the encrypted stream.
func ReadHeader(src io.Reader) (Header, []byte, error) {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(src, magic); err != nil || string(magic) != Magic {
		return Header{}, nil, ErrNotBackup
	}
	var fixed [6]byte
	if _, err := io.ReadFull(src, fixed[:]); err != nil {
		return Header{}, nil, fmt.Errorf("%w: header cut short", ErrUnreadable)
	}
	version := binary.BigEndian.Uint16(fixed[:2])
	if version > FormatVersion {
		return Header{}, nil, ErrNewerFormat
	}
	if version == 0 {
		return Header{}, nil, fmt.Errorf("%w: format version 0", ErrUnreadable)
	}
	n := binary.BigEndian.Uint32(fixed[2:])
	if n == 0 || n > maxHeaderLen {
		return Header{}, nil, ErrHeaderLimits
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(src, body); err != nil {
		return Header{}, nil, fmt.Errorf("%w: header cut short", ErrUnreadable)
	}
	var h Header
	if err := json.Unmarshal(body, &h); err != nil {
		return Header{}, nil, fmt.Errorf("%w: header is not valid", ErrUnreadable)
	}
	if h.Format != version {
		return Header{}, nil, fmt.Errorf("%w: header format disagrees with the file", ErrUnreadable)
	}
	if err := validateKDF(h.KDF); err != nil {
		return Header{}, nil, err
	}
	raw := make([]byte, 0, len(magic)+len(fixed)+len(body))
	raw = append(raw, magic...)
	raw = append(raw, fixed[:]...)
	return h, append(raw, body...), nil
}

// entryNames are the only payload entries, in order.
var entryNames = [...]string{EntryManifest, EntryDB}

// Writer writes one backup file.
type Writer struct {
	ageW     io.WriteCloser
	gz       *gzip.Writer
	tw       *tar.Writer
	next     int
	maxEntry int64
	anyName  bool // tests only: skip the entry-name rule to build hostile payloads
	closed   bool
}

// NewWriter writes the outer header for h to dst and returns a Writer for the
// payload. It refuses a passphrase shorter than MinPassphraseLen characters and
// a header outside the caps. h.Format is set to FormatVersion.
func NewWriter(dst io.Writer, h Header, passphrase []byte) (*Writer, error) {
	if utf8.RuneCount(passphrase) < MinPassphraseLen {
		return nil, ErrPassphraseTooShort
	}
	h.Format = FormatVersion
	if err := validateKDF(h.KDF); err != nil {
		return nil, err
	}
	return newWriter(dst, h, passphrase)
}

func newWriter(dst io.Writer, h Header, passphrase []byte) (*Writer, error) {
	raw, err := encodeHeader(h)
	if err != nil {
		return nil, err
	}
	if _, err := dst.Write(raw); err != nil {
		return nil, fmt.Errorf("write backup header: %w", err)
	}
	rec := &argonRecipient{params: h.KDF, pass: passphrase, headerHash: sha256.Sum256(raw)}
	ageW, err := age.Encrypt(dst, rec)
	if err != nil {
		return nil, fmt.Errorf("start backup stream: %w", err)
	}
	gz := gzip.NewWriter(ageW)
	return &Writer{ageW: ageW, gz: gz, tw: tar.NewWriter(gz), maxEntry: MaxEntrySize}, nil
}

// Add writes the next payload entry. Only manifest.json first and bayplan.db
// second are accepted, and size must be exactly the number of bytes r yields.
func (w *Writer) Add(name string, size int64, r io.Reader) error {
	if w.closed {
		return errors.New("backup writer is closed")
	}
	if !w.anyName && (w.next >= len(entryNames) || name != entryNames[w.next]) {
		return fmt.Errorf("backup entry %q is not the expected next entry", name)
	}
	if size < 0 || size > w.maxEntry {
		return fmt.Errorf("backup entry %q size %d is outside the allowed range", name, size)
	}
	if err := w.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600}); err != nil {
		return fmt.Errorf("write backup entry header: %w", err)
	}
	n, err := io.Copy(w.tw, io.LimitReader(r, size))
	if err != nil {
		return fmt.Errorf("write backup entry %q: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("backup entry %q: expected %d bytes, got %d", name, size, n)
	}
	w.next++
	return nil
}

// Close finishes the tar, the gzip and the age stream, in that order. It
// reports an error if both entries were not written, but still finishes the
// stream.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	var errs []error
	if err := w.tw.Close(); err != nil {
		errs = append(errs, fmt.Errorf("finish backup tar: %w", err))
	}
	if err := w.gz.Close(); err != nil {
		errs = append(errs, fmt.Errorf("finish backup compression: %w", err))
	}
	if err := w.ageW.Close(); err != nil {
		errs = append(errs, fmt.Errorf("finish backup encryption: %w", err))
	}
	if !w.anyName && w.next != len(entryNames) {
		errs = append(errs, errors.New("backup is incomplete: both entries are required"))
	}
	return errors.Join(errs...)
}

// Reader reads one backup file.
type Reader struct {
	header   Header
	gz       *gzip.Reader
	tr       *tar.Reader
	next     int
	maxEntry int64
	err      error // sticky
}

// Open reads the outer header, checks every cap, derives the key (at most two
// derivations run at once) and starts the payload. Every decrypt, gzip or tar
// failure, including a wrong passphrase, is ErrUnreadable.
func Open(src io.Reader, passphrase []byte) (*Reader, error) {
	h, raw, err := ReadHeader(src)
	if err != nil {
		return nil, err
	}
	id := &argonIdentity{params: h.KDF, pass: passphrase, headerHash: sha256.Sum256(raw)}
	plain, err := age.Decrypt(src, id)
	if err != nil {
		return nil, unreadable(err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return nil, unreadable(err)
	}
	return &Reader{header: h, gz: gz, tr: tar.NewReader(gz), maxEntry: MaxEntrySize}, nil
}

// unreadable wraps a low-level failure so the cause is logged but only
// ErrUnreadable can be matched.
func unreadable(err error) error {
	return fmt.Errorf("%w: %s", ErrUnreadable, strings.TrimSpace(err.Error()))
}

// Header returns the validated outer header.
func (r *Reader) Header() Header { return r.header }

// Next returns the next payload entry: manifest.json, then bayplan.db, then
// io.EOF once the whole stream, including its final authenticated chunk, has
// been read. The returned reader is valid until the next call to Next.
func (r *Reader) Next() (string, io.Reader, error) {
	if r.err != nil {
		return "", nil, r.err
	}
	hdr, err := r.tr.Next()
	if errors.Is(err, io.EOF) {
		if r.next != len(entryNames) {
			return "", nil, r.fail(errors.New("payload ended early"))
		}
		// Read to the end so a truncated, extended or damaged tail is caught.
		n, err := io.Copy(io.Discard, io.LimitReader(r.gz, drainLimit+1))
		switch {
		case err != nil:
			return "", nil, r.fail(err)
		case n > drainLimit:
			return "", nil, r.fail(errors.New("payload has trailing data"))
		}
		r.err = io.EOF
		return "", nil, io.EOF
	}
	if err != nil {
		return "", nil, r.fail(err)
	}
	if r.next >= len(entryNames) {
		return "", nil, r.fail(errors.New("payload has an extra entry"))
	}
	if hdr.Name != entryNames[r.next] {
		return "", nil, r.fail(fmt.Errorf("unexpected payload entry %q", hdr.Name))
	}
	if hdr.Typeflag != tar.TypeReg {
		return "", nil, r.fail(errors.New("payload entry is not a regular file"))
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return "", nil, r.fail(errors.New("payload entry is sparse"))
		}
	}
	if hdr.Size < 0 || hdr.Size > r.maxEntry {
		return "", nil, r.fail(errors.New("payload entry is larger than the allowed size"))
	}
	r.next++
	return hdr.Name, &entryReader{r: r, limit: r.maxEntry}, nil
}

func (r *Reader) fail(err error) error {
	r.err = unreadable(err)
	return r.err
}

// entryReader reads one entry through a counting limit and maps every
// non-EOF failure, including a short entry, to ErrUnreadable.
type entryReader struct {
	r     *Reader
	limit int64
	n     int64
}

func (e *entryReader) Read(p []byte) (int, error) {
	if e.r.err != nil {
		return 0, e.r.err
	}
	n, err := e.r.tr.Read(p)
	e.n += int64(n)
	if e.n > e.limit {
		return 0, e.r.fail(errors.New("payload entry is larger than the allowed size"))
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, e.r.fail(err)
	}
	return n, err
}
