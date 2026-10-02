// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
)

// containerDir matches the per-container directory Docker keeps under its data
// root. The 64 hex digits are the full container ID.
var containerDir = regexp.MustCompile(`/var/lib/docker/containers/([0-9a-f]{64})/`)

// shortID is what Docker sets as the hostname of a container by default: the
// first 12 hex digits of its ID.
var shortID = regexp.MustCompile(`^[0-9a-f]{12}$`)

// identityMounts are the files Docker bind-mounts into every container from
// that container's own directory. A mount of one of them is the evidence that
// the directory belongs to this container; a mount of some other container's
// directory (a log shipper, say) is not.
var identityMounts = map[string]bool{
	"/etc/hostname":    true,
	"/etc/hosts":       true,
	"/etc/resolv.conf": true,
}

// DetectSelfContainerID returns the ID of the container this process runs in,
// so the UI can badge it "Bayplan (this instance)" (D-27). mountinfo is
// /proc/self/mountinfo.
//
// The full 64-character ID comes from the mount that supplies /etc/hostname,
// /etc/hosts or /etc/resolv.conf, whose source sits under
// /var/lib/docker/containers/<id>/. When no such mount exists (cgroup layouts
// and runtimes differ, RESEARCH A2) the hostname is used if it looks like the
// 12-character short ID Docker assigns. Otherwise the answer is empty: no badge
// is better than the wrong one.
func DetectSelfContainerID(mountinfo io.Reader, hostname string) string {
	sc := bufio.NewScanner(mountinfo)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		m := containerDir.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Fields: id parent major:minor root mountpoint options ... - fstype source superopts.
		fields := strings.Fields(line)
		if len(fields) > 4 && identityMounts[fields[4]] {
			return m[1]
		}
	}
	if shortID.MatchString(hostname) {
		return hostname
	}
	return ""
}

// ReadSelfContainerID reads /proc/self/mountinfo and the hostname and applies
// DetectSelfContainerID. It returns empty when Bayplan does not run in a
// container, or the files cannot be read. It must run before the privilege
// drop (A9): mountinfo is world-readable, but the call is kept next to the
// other start-up probes.
func ReadSelfContainerID() string {
	host, _ := os.Hostname()
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return DetectSelfContainerID(strings.NewReader(""), host)
	}
	defer func() { _ = f.Close() }()
	return DetectSelfContainerID(f, host)
}
