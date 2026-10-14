// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package redact

import "github.com/moby/moby/api/types/swarm"

// MaskServiceSpec is not written yet (RED stub).
func MaskServiceSpec(spec swarm.ServiceSpec, reveal bool) (swarm.ServiceSpec, bool) {
	return spec, false
}

// MaskServiceInspect is not written yet (RED stub).
func MaskServiceInspect(raw []byte, reveal bool) ([]byte, bool, error) {
	return raw, false, nil
}
