// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package backup

import "time"

// ManifestFormat is the version of manifest.json's field set. It is separate
// from FormatVersion (the container layout): fields can be added under the same
// number, and renaming or removing one needs a new number and a read path for
// the old (D-28).
const ManifestFormat = 1

// Counts are the headline numbers a restore shows before it replaces anything.
type Counts struct {
	Users        int64 `json:"users"`
	Teams        int64 `json:"teams"`
	Environments int64 `json:"environments"`
	AuditEntries int64 `json:"auditEntries"`
}

// Manifest is manifest.json, the first entry of every backup. It sits inside the
// encrypted stream, so it may carry the per-backup export key.
type Manifest struct {
	Format       int       `json:"format"`
	Created      time.Time `json:"created"`
	AppVersion   string    `json:"appVersion"`
	Schema       int64     `json:"schema"`
	InstanceName string    `json:"instanceName"`
	PublicURL    string    `json:"publicUrl"`
	Counts       Counts    `json:"counts"`
	// ChainVerified is whether the audit chain verified when the backup was
	// made. It is false only when the export was told to go on regardless.
	ChainVerified bool `json:"chainVerified"`
	// ExportKey is the base64 per-backup key every sealed value in the database
	// entry is sealed under. It is readable only inside the encrypted stream.
	ExportKey string `json:"exportKey"`
}
