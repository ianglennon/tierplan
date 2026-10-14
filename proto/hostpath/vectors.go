// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package hostpath

import "encoding/json"

// SpecVector is one service spec with the verdict every policy must agree on.
type SpecVector struct {
	Name           string          `json:"name"`
	Spec           json.RawMessage `json:"spec"`
	Escalates      bool            `json:"escalates"`
	HostNetworkIDs []string        `json:"hostNetworkIDs,omitempty"`
}

// Vectors returns the embedded service-spec vectors.
func Vectors() []SpecVector { return nil }
