// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// convertLogText is the text a job logs for a conversion it could not make. A field fed by a secret variable
// is the fixed sentence (convert.GuardSecrets), and whole secret values are masked in what remains, so no
// converter text about a secret-fed field reaches the server log (07-42, D-02). The jobs answer their caller
// with an item code only, so the log is the one place this text goes. It is built only on that failure path,
// once per event.
func convertLogText(err error, c stacks.WorkingCopy) string {
	if err == nil {
		return ""
	}
	return stacks.NewMasker(c.Vars, c.MaskFiles()).Mask(convert.GuardSecrets(err, stacks.NewSecretUse(c)).Error())
}
