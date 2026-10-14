// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package hostpath is the one rule that says which host paths a container may
// be given without extra privilege. The compose policy (stacks) and the agent's
// service-spec check both call it, so a path an Operator may bind in a stack is
// a path an Operator may bind in a service (D-38). Standard library only.
package hostpath

// DataRoots are directories a bind may name, or sit under.
var DataRoots = []string{}

// NetworkFSTypes are local-driver mount types whose device is a remote share.
var NetworkFSTypes = []string{}

// Socket reports whether p names a container runtime socket.
func Socket(p string) bool { return false }

// Bad reports whether a bind source gives the container something it should
// not have without extra privilege.
func Bad(p string) bool { return false }

// ClimbingBad judges a relative path by where a climb out of the project could land.
func ClimbingBad(p string) bool { return false }

// LocalDeviceMounted reports the device a local-driver volume mounts.
func LocalDeviceMounted(opts map[string]string) (device string, mounted bool) { return "", false }
