// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

// Crash points RotateKey reaches, for the external tests.
const (
	CrashNewKeyWritten = pointNewKeyWritten
	CrashBeforeCommit  = pointBeforeCommit
	CrashAfterCommit   = pointAfterCommit
	CrashFirstRename   = pointFirstRename
)

// SetRotateHook makes RotateKey call f at each crash point; an error from f
// stands in for the process being killed there. The returned func restores the
// default.
func SetRotateHook(f func(point string) error) (restore func()) {
	old := rotateHook
	rotateHook = f
	return func() { rotateHook = old }
}
