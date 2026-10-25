// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild

import "strings"

// The engine fills in some values when it stores a service or a network, and a file that repeated them would not
// read like a hand-written one. Each function here is one such rule and says which rule of stacks.NormalizeSpec
// (or which convert default) it mirrors: a value is left out only when converting the result gives it back.

// vxlanOption is the driver option the engine adds to every overlay network it creates: it names the VXLAN IDs
// it chose. A file that repeated it would fix them, so it is never written.
const vxlanOption = "com.docker.network.driver.overlay.vxlanid_list"

// networkOptions are the driver options of a network without the ones the engine chose itself.
func networkOptions(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if strings.EqualFold(k, vxlanOption) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// plainDefault says whether the stack's default network is the one convert makes when a file declares none: an
// overlay network that is neither attachable nor internal and carries no option or label of a person's. Its address
// pool is the engine's own pick, so it is not part of the test.
func plainDefault(info NetworkInfo) bool {
	return !info.Attachable && !info.Internal && len(networkOptions(info.Options)) == 0 && len(userLabels(info.Labels)) == 0 &&
		(info.IPAMDriver == "" || info.IPAMDriver == "default")
}
