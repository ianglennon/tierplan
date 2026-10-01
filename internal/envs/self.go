// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import "io"

// DetectSelfContainerID finds the container ID of this process.
func DetectSelfContainerID(mountinfo io.Reader, hostname string) string { return "" }

// ReadSelfContainerID reads /proc/self/mountinfo and the hostname.
func ReadSelfContainerID() string { return "" }
