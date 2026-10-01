// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

// record is every field the HMAC covers.
type record struct {
	ID                                  string
	TS                                  int64
	ActorType, ActorID, ActorName       string
	Action, TargetType, TargetID, EnvID string
	Result, Reason, IP, UserAgent       string
	Details                             map[string]string
	KeyID                               string
}

func canonicalV1(r record) []byte { return nil }

func computeHMAC(key, prev []byte, r record) []byte { return nil }
