// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// maxArchiveContent is the most bytes of file content an archive write may carry: the D-40
// ceiling for a stack config, the larger of the secret (512,000) and config (1,024,000) limits
// rounded down to a clean 1,000 KB.
const maxArchiveContent = 1_000 * 1_000

// maxArchiveBody caps a whole archive request. A tar of one file adds a 512-byte header, up to
// 511 bytes of padding and at least 1,024 bytes of end-of-archive blocks; the rest is head room
// for the zero padding some tar writers add to a blocking factor.
const maxArchiveBody = maxArchiveContent + 64<<10

// maxArchiveName caps the path of the file, the length Linux takes for a path.
const maxArchiveName = 4096

// composeProjectLabel is the label Compose puts on every container it creates. An archive is
// written only into a container that has it (STK-02).
const composeProjectLabel = "com.docker.compose.project"

// errContainerLookup is the engine not answering the lookup of the container an archive is for.
// Like errVolumeLookup it is an engine fault, not a policy refusal: the handler answers 502 and
// forwards nothing, and it never reads as permission.
var errContainerLookup = errors.New("the engine did not answer a container lookup")

// containerLookup asks the agent's own engine about one container: its labels, whether it is
// running, and whether it exists. A container the engine does not have is found false with no
// error; anything else the engine cannot answer is the error.
type containerLookup func(ctx context.Context, id string) (labels map[string]string, running, found bool, err error)

// validateArchive judges the body of PUT /containers/{id}/archive. The Compose SDK injects a
// stack file secret or config with one tar of exactly one regular file (createTar in its
// secrets.go), so that is all it accepts: one entry of type regular file, whose cleaned name has
// no .. segment and no NUL and is relative or has a single leading slash (the SDK writes the
// container path, /run/secrets/tok, as the name), a mode with no bit above rwxrwxrwx (so no
// setuid, setgid or sticky), no link target, device number or extended (PAX) record, at most
// maxArchiveContent bytes of content, then the end of the archive with nothing but zero padding
// after it. A symlink, hard link, device, FIFO or directory, a second entry and trailing data are
// all refusals. The write is made at path=/ (Allowed), so the name is the file's path inside the
// container.
//
// The body is not rebuilt: the engine reads it with the same archive/tar rules, and once it is
// valid it carries nothing the engine could read differently.
func validateArchive(raw []byte) error {
	rd := bytes.NewReader(raw)
	tr := tar.NewReader(rd)
	hdr, err := tr.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return refuse("the archive has no entry")
		}
		return refuse("not a tar archive: %v", err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return refuse("the archive entry is not a regular file")
	}
	if err := archiveNameOK(hdr.Name); err != nil {
		return err
	}
	if hdr.Mode&^0o777 != 0 {
		return refuse("the archive entry has a mode beyond rwxrwxrwx (setuid, setgid or sticky)")
	}
	if hdr.Linkname != "" || hdr.Devmajor != 0 || hdr.Devminor != 0 {
		return refuse("the archive entry names a link target or a device")
	}
	if len(hdr.PAXRecords) != 0 {
		// Extended records carry file capabilities and extended attributes, and can rename
		// the entry after the checks above.
		return refuse("the archive entry has extended records")
	}
	if hdr.Size < 0 || hdr.Size > maxArchiveContent {
		return refuse("the archive entry is over %d bytes", maxArchiveContent)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(tr, maxArchiveContent+1))
	if err != nil {
		return refuse("the archive entry cannot be read: %v", err)
	}
	if n != hdr.Size {
		return refuse("the archive entry is shorter than its header says")
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			return refuse("the archive has a second entry")
		}
		return refuse("the archive does not end after its one entry: %v", err)
	}
	// The end-of-archive blocks are read; whatever is left must be padding.
	rest := raw[len(raw)-rd.Len():]
	if len(bytes.TrimLeft(rest, "\x00")) != 0 {
		return refuse("the archive has data after its end")
	}
	return nil
}

// archiveNameOK reports whether the name of the one file is a clean path inside the container:
// path.Clean leaves it unchanged (so no //, no . or .. segment, no trailing slash), it has no
// NUL, it is not the root, and it has at most one leading slash.
func archiveNameOK(name string) error {
	if name == "" || len(name) > maxArchiveName || strings.ContainsRune(name, 0) {
		return refuse("the archive entry name is empty, too long or holds a NUL")
	}
	rel := strings.TrimPrefix(name, "/")
	if rel == "" || rel == "." || path.Clean(name) != name || strings.HasPrefix(rel, "/") {
		return refuse("the archive entry name is not a clean path")
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == ".." {
			return refuse("the archive entry name has a .. segment")
		}
	}
	return nil
}

// archiveTargetOK refuses an archive write into any container that Compose did not create or
// that is running. The Compose SDK writes a secret or config only between create and start; a
// write into a running container, or into one that is not a compose container, is not a thing
// Bayplan does (STK-02). A container the engine does not have is a refusal; an engine that does
// not answer is an error wrapping errContainerLookup, never an allow.
func archiveTargetOK(ctx context.Context, id string, inspect containerLookup) error {
	if inspect == nil {
		return refuse("no container lookup")
	}
	labels, running, found, err := inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: %w", errContainerLookup, err)
	}
	switch {
	case !found:
		return refuse("the container does not exist")
	case running:
		return refuse("the container is running")
	case labels[composeProjectLabel] == "":
		return refuse("the container was not created by Compose")
	}
	return nil
}
