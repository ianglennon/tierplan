// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package authn

import (
	"crypto/sha256"
	"crypto/tls"
	"strconv"
)

const (
	// ingressTranscriptDomain separates a signature made for the ingress channel
	// from every other use of the same kind of key, the agent handshake included,
	// and carries the ingress transcript version. The bytes are frozen with the
	// first ingress release; a change is a new version (tierplan.ingress.v2), not
	// an edit.
	ingressTranscriptDomain = "tierplan.ingress.v1\x00"
	// IngressEKMLabel is the TLS exporter label of the ingress channel binding. It
	// differs from EKMLabel, so material exported for one channel is never the
	// material of the other.
	IngressEKMLabel = "EXPORTER-tierplan-ingress-auth-v1"
)

// IngressTranscript is what a node signs at /ingress/connect, in modes enrol and
// connect: SHA-256 over the domain tierplan.ingress.v1 and a NUL byte, then each
// field in the order mode, proto, serverNonce, nodeNonce, aud, pub, ekm, each
// preceded by its length as a two-byte big-endian integer. The framing and the
// rule for a field of 65535 bytes or more (0xFFFF followed by the SHA-256 of the
// field) are those of Transcript.
//
// The domain is the only difference from the agent's transcript, and it is the
// point: a signature made over Transcript never verifies over IngressTranscript
// for the same inputs, nor the reverse, so an agent credential cannot be replayed
// at the ingress endpoint nor an ingress one at the agent endpoint. The loop is
// repeated here, not shared, so the bytes of Transcript cannot move.
func IngressTranscript(mode string, proto int, serverNonce, nodeNonce []byte, aud string, pub, ekm []byte) []byte {
	h := sha256.New()
	h.Write([]byte(ingressTranscriptDomain))
	for _, f := range [][]byte{
		[]byte(mode), []byte(strconv.Itoa(proto)), serverNonce, nodeNonce, []byte(aud), pub, ekm,
	} {
		if len(f) >= maxField {
			sum := sha256.Sum256(f)
			h.Write([]byte{0xFF, 0xFF})
			h.Write(sum[:])
			continue
		}
		h.Write([]byte{byte(len(f) >> 8), byte(len(f))}) //nolint:gosec // len(f) < maxField, so it fits two bytes
		h.Write(f)
	}
	return h.Sum(nil)
}

// EKMFor is the channel-binding material of a TLS connection for one exporter
// label: ExportKeyingMaterial(label, nil, EKMLength), or nil when the connection
// has none (a nil state, or one that cannot export). EKM is EKMFor with EKMLabel;
// the ingress channel uses IngressEKMLabel.
func EKMFor(cs *tls.ConnectionState, label string) []byte {
	if cs == nil {
		return nil
	}
	out, err := cs.ExportKeyingMaterial(label, nil, EKMLength)
	if err != nil {
		return nil
	}
	return out
}
