// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package backup owns the published TierPlan backup file format (D-28).
//
// The layout is a contract: a backup written by one 0.x release must stay
// readable by every later one, and a file from a newer format is refused by
// name instead of being misread. Changing any byte of the layout below needs a
// new FormatVersion and a reader for the old one.
//
//	"TIERPLANBK1\n"            magic
//	u16 big-endian            format version (1)
//	u32 big-endian            header length, at most 4096
//	header JSON               format, kdf{name,t,m_kib,p,salt}, created, app_version, schema
//	age v1 stream             one stanza of type "tierplan-argon2id", then the payload
//	  gzip                    compression happens inside the encryption, never outside
//	    tar                   exactly two entries: manifest.json, then tierplan.db
//
// The header is plaintext so a newer format or schema can be named before the
// expensive key derivation runs. It carries no instance name, URL or counts. Its
// bytes are bound into the key that unwraps the age file key, so changing any
// header byte makes the file unopenable, not merely suspicious.
//
// The package is a pure library: it reads and writes the container and knows
// nothing about the database, the vault or HTTP.
package backup
