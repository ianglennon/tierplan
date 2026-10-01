// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"slices"
)

// genesisLen is the length of an HMAC-SHA256 value and of the genesis
// prev_hmac: 32 zero bytes (D-40).
const genesisLen = sha256.Size

// canonicalVersion is the first byte of every canonical encoding. A future
// format change adds a new version; entries already written keep verifying
// under the one they were written with.
const canonicalVersion = 0x01

// record is every field the HMAC covers. Writer and Verify both build one, so
// they cannot disagree about what was signed.
type record struct {
	ID                            string
	TS                            int64
	ActorType, ActorID, ActorName string
	Action, TargetType, TargetID  string
	EnvID                         string // empty when the column is NULL
	Result, Reason, IP, UserAgent string
	Details                       map[string]string
	KeyID                         string
}

// canonicalV1 is the fixed-order, length-prefixed binary encoding that is
// signed. It is deliberately not JSON: re-marshalling would make the signature
// depend on a serializer. Layout (confirmed at plan 01-07 Task 1, one-way):
//
//	0x01
//	id, ts (8 bytes big-endian), actor type, actor id, actor name, action,
//	target type, target id, env id, result, reason, ip, user agent,
//	details: uvarint pair count, then each key and value in key order,
//	key id
//
// Every field except ts is a uvarint length followed by its bytes.
func canonicalV1(r record) []byte {
	b := []byte{canonicalVersion}
	b = appendString(b, r.ID)
	b = binary.BigEndian.AppendUint64(b, uint64(r.TS)) //nolint:gosec // two's complement of the unix nanosecond count
	for _, s := range []string{
		r.ActorType, r.ActorID, r.ActorName, r.Action,
		r.TargetType, r.TargetID, r.EnvID,
		r.Result, r.Reason, r.IP, r.UserAgent,
	} {
		b = appendString(b, s)
	}
	keys := make([]string, 0, len(r.Details))
	for k := range r.Details {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	b = binary.AppendUvarint(b, uint64(len(keys)))
	for _, k := range keys {
		b = appendString(b, k)
		b = appendString(b, r.Details[k])
	}
	return appendString(b, r.KeyID)
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// computeHMAC is HMAC-SHA256(key, prev || canonicalV1(r)).
func computeHMAC(key, prev []byte, r record) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(prev)
	m.Write(canonicalV1(r))
	return m.Sum(nil)
}
