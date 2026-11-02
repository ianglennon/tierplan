// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"time"

	"github.com/ianglennon/tierplan/internal/redact"
)

// The limits on the crash evidence a run keeps (D-06, RESEARCH F8).
const (
	// EvidenceLines is how many of a crashed container's last log lines are read and kept.
	EvidenceLines = 200
	// MaxEvidenceBytes is the most log text kept for one container. The newest lines that fit are kept.
	MaxEvidenceBytes = 32 << 10
	// MaxEvidencePerRun is how many containers of one run have their log tail kept. A crashed
	// container beyond it is still removed, with its facts and the state EvidenceCapReached.
	MaxEvidencePerRun = 200
)

// What the log text of a piece of evidence is.
const (
	// EvidenceSaved: the lines are kept.
	EvidenceSaved = "saved"
	// EvidenceEmpty: the container wrote no log.
	EvidenceEmpty = "empty"
	// EvidenceUnavailable: the container's log driver cannot be read back.
	EvidenceUnavailable = "unavailable"
	// EvidenceCapReached: the run already holds the most evidence it may, so no log text was kept.
	EvidenceCapReached = "cap_reached"
)

// CodeEvidenceUnsaved is the code of a crashed container the run did not remove because its
// evidence could not be written to the record (D-06: evidence before removal).
const CodeEvidenceUnsaved = "evidence_unsaved"

// Evidence is why a crashed container stopped and the tail of its log, as a run keeps it.
type Evidence struct {
	ExitCode              int
	Error                 string
	OOMKilled             bool
	StartedAt, FinishedAt time.Time
	// Image is the image reference the container was created with, ImageID the image it ran and
	// ImageDigest the repo@sha256 name of that image on its node, empty when unknown.
	Image, ImageID, ImageDigest string
	// Node is the name of the node it ran on.
	Node string
	// Lines are the last lines of its log, oldest first, masked and capped. State says what an empty
	// list means.
	Lines []string
	State string
}

// BuildEvidence makes the evidence of a crashed container from what its node said. Every line is
// passed through redact.MaskLogLine with the source's secrets, then the newest lines that fit
// EvidenceLines and MaxEvidenceBytes are kept, so nothing reaches the record that was not masked
// and capped (T-11-35).
func BuildEvidence(src EvidenceSource) Evidence {
	ev := evidenceOf(src.End, src.Node)
	// The engine's start error is text a remote system wrote, so it is masked like a log line.
	ev.Error, _ = redact.MaskLogLine(ev.Error, src.Secrets)
	if src.ImageDigest != "" {
		ev.ImageDigest = src.ImageDigest
	}
	if src.LogsUnavailable {
		ev.State = EvidenceUnavailable
		return ev
	}
	lines := src.Lines
	if len(lines) > EvidenceLines {
		lines = lines[len(lines)-EvidenceLines:]
	}
	masked := make([]string, 0, len(lines))
	for _, l := range lines {
		m, _ := redact.MaskLogLine(l, src.Secrets)
		masked = append(masked, m)
	}
	// Keep the newest lines that fit: each costs its text and the line break that separates it.
	size, first := 0, len(masked)
	for first > 0 && size+len(masked[first-1])+1 <= MaxEvidenceBytes {
		first--
		size += len(masked[first]) + 1
	}
	ev.Lines = masked[first:]
	ev.State = EvidenceSaved
	if len(ev.Lines) == 0 {
		ev.State = EvidenceEmpty
	}
	return ev
}

// evidenceOf is the facts of a container's end, with no log: the evidence of a container past the
// run's cap, and the start of every other.
func evidenceOf(end ContainerEnd, node string) Evidence {
	return Evidence{
		ExitCode: end.ExitCode, Error: end.Error, OOMKilled: end.OOMKilled,
		StartedAt: end.StartedAt, FinishedAt: end.FinishedAt,
		Image: end.Image, ImageID: end.ImageID, Node: node,
	}
}
