// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package netspace is the address arithmetic of networks and address pools (NET-02): how
// much of a pool is used, what the largest free block is, which state a meter is in, and
// later the suggestions, overlaps, headroom and orphan classes that are decided from the
// same occupied set. It is pure. It reads prefixes and numbers, and returns prefixes and
// numbers; it opens no file, calls no engine and imports no Docker, store or HTTP type, so
// every rule in it is a table test.
//
// Only IPv4 is counted for 0.1: every function skips a prefix that is not IPv4. Arithmetic
// works on index intervals and never on enumerated blocks, because a 10.0.0.0/8 pool cut
// into /30 blocks has 4,194,304 of them.
package netspace
