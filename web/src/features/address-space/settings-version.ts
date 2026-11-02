// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { AddressSettings, PoolEntry } from "@/api/address-space";

// The server keeps one version for both address-space settings (Default stack subnets and Address
// pools), so saving one card moves the version the other card read. Each card therefore remembers
// the slice it began editing from (`base`) and re-bases its save onto the newest version only when
// its own slice is unchanged.

export interface VersionedEdit<S> {
  // The version the person started editing at.
  version: number;
  // The card's own slice of the settings as it was when the editing began.
  base: S;
}

export interface VersionedRead<S> {
  version: number;
  slice: S;
}

// versionToSend is the version a Save carries. When the settings have moved on since the edit
// began, but this card's own slice in the newest read still equals the one the edit began from,
// the version moved only because of the other card's setting: the newest version is sent, and the
// server's check still guards what the version is there for. In every other case (nothing moved,
// no newer read, or this card's own slice changed) the edit's own version is sent, so the server
// refuses a save over someone else's change to the same fields.
export function versionToSend<S>(
  edit: VersionedEdit<S>,
  current: VersionedRead<S> | undefined,
  same: (a: S, b: S) => boolean,
): number {
  if (current && current.version !== edit.version && same(current.slice, edit.base)) {
    return current.version;
  }
  return edit.version;
}

export type SmallSubnets = AddressSettings["smallSubnets"];

export function sameSmallSubnets(a: SmallSubnets, b: SmallSubnets): boolean {
  return a.enabled === b.enabled && a.mask === b.mask;
}

// samePools compares in order: a reordered list is a different setting. Bases and sizes are
// compared as trimmed text, so rows being typed and entries as saved line up.
export function samePools(a: readonly PoolEntry[], b: readonly PoolEntry[]): boolean {
  return (
    a.length === b.length &&
    a.every(
      (p, i) =>
        p.base.trim() === b[i]?.base.trim() && String(p.size).trim() === String(b[i]?.size).trim(),
    )
  );
}
