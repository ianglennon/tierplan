// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"errors"
	"strings"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/tunnel"
)

// How a pull is told in a job, shared by the pull routes of Phase 5 (internal/api) and the pre-pull of a
// standalone deploy: the same progress folded into an item, the same failure codes and the same sentences.

const (
	// CodeNodeUnreachable is the code of a pull whose node is not connected to Coaming.
	CodeNodeUnreachable = "node_unreachable"

	// maxPullLayers bounds the layers one item lists; an image has a few dozen.
	maxPullLayers = 256
	// maxPullText bounds a layer ID or status taken from the engine's progress.
	maxPullText = 80
)

// PullMessages are the sentences an item carries for a pull failure code. They are built here from the
// code, never from the engine's or the registry's answer, so nothing a remote system said reaches a browser
// through a job (T-05-02, T-05-06). The SPA words a code itself and uses these only as a fallback.
var PullMessages = map[string]string{
	dockerx.PullCodeDenied:               "The registry refused the pull.",
	dockerx.PullCodeUnauthorized:         "The registry asked for a sign-in the pull did not have.",
	dockerx.PullCodeNotFound:             "The registry has no such image.",
	dockerx.PullCodeInvalidReference:     "The engine did not accept this image reference.",
	dockerx.PullCodeRateLimited:          "The registry is limiting pulls. Try again later.",
	dockerx.PullCodeRegistryUnreachable:  "The registry could not be reached from this node.",
	dockerx.PullCodeRegistryTLS:          "The registry's certificate was not accepted by this node.",
	dockerx.PullCodeRegistryHTTP:         "The registry does not speak HTTPS and this node requires it.",
	dockerx.PullCodeRegistryHTTPFallback: "The registry's real answer was hidden by a retry over plain HTTP. Check the name, the tag and the sign-in.",
	dockerx.PullCodeCancelled:            "The pull was cancelled.",
	dockerx.PullCodeFailed:               "The pull failed.",
	CodeNodeUnreachable:                  "The node is not connected to Coaming.",
	CodeHostReadOnly:                     "This host is read-only, so Coaming cannot pull on it.",
	CodeAgentPolicyDenied:                "The node's agent refused the request.",
}

// PullErrorCode maps the error of one node's pull to an item code.
func PullErrorCode(err error) string {
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, envs.ErrHostReadOnly):
		return CodeHostReadOnly
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound), errors.Is(err, envs.ErrOffline),
		errors.Is(err, envs.ErrUnsupported), errors.Is(err, tunnel.ErrAgentGone):
		return CodeNodeUnreachable
	case errors.As(envs.ClassifyDockerError(err), &denied):
		return CodeAgentPolicyDenied
	}
	return dockerx.PullFailure(err)
}

// ApplyPullEvent folds one line of the engine's progress into the item: the layer it is about, and the
// totals across the layers. The item is the hub's own, held under its lock. index maps a layer to its place
// in the item and belongs to the one pull.
func ApplyPullEvent(it *jobs.Item, index map[string]int, ev dockerx.PullEvent) {
	it.State = jobs.ItemRunning
	if ev.Layer == "" {
		return
	}
	id := ClipPullText(ev.Layer)
	i, ok := index[id]
	if !ok {
		if len(it.Subs) >= maxPullLayers {
			return
		}
		it.Subs = append(it.Subs, jobs.Sub{ID: id})
		i = len(it.Subs) - 1
		index[id] = i
	}
	s := &it.Subs[i]
	s.Status = ClipPullText(ev.Status)
	if ev.Bytes {
		s.Current, s.Total = ev.Current, ev.Total
	}
	if layerFinished(s.Status) && s.Total > 0 {
		s.Current = s.Total
	}
	it.Count = len(it.Subs)
	it.Done, it.Current, it.Total = 0, 0, 0
	for _, l := range it.Subs {
		if layerFinished(l.Status) {
			it.Done++
		}
		it.Current += l.Current
		it.Total += l.Total
	}
	it.Unit = ""
	if it.Total > 0 {
		it.Unit = "bytes"
	}
}

func layerFinished(status string) bool {
	return status == "Pull complete" || status == "Already exists"
}

// ClipPullText cuts a layer ID or status to maxPullText bytes on a rune boundary and drops control
// characters: it is shown to a browser as text.
func ClipPullText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxPullText {
		s = strings.ToValidUTF8(s[:maxPullText], "")
	}
	return s
}
