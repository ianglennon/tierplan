// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import {
  samePools,
  sameSmallSubnets,
  versionToSend,
} from "@/features/address-space/settings-version";

// The rebase rule both address-space settings cards share (G-10-9).

describe("versionToSend", () => {
  const same = (a: string, b: string) => a === b;

  it("sends the edit's version when nothing moved", () => {
    expect(versionToSend({ version: 4, base: "a" }, { version: 4, slice: "a" }, same)).toBe(4);
  });

  it("sends the edit's version without a newer read", () => {
    expect(versionToSend({ version: 4, base: "a" }, undefined, same)).toBe(4);
  });

  it("sends the newest version when the version moved and the slice is still the base", () => {
    expect(versionToSend({ version: 4, base: "a" }, { version: 6, slice: "a" }, same)).toBe(6);
  });

  it("sends the edit's version when the card's own slice changed", () => {
    expect(versionToSend({ version: 4, base: "a" }, { version: 5, slice: "b" }, same)).toBe(4);
  });
});

describe("sameSmallSubnets", () => {
  it("compares enabled and mask", () => {
    expect(sameSmallSubnets({ enabled: true, mask: 26 }, { enabled: true, mask: 26 })).toBe(true);
    expect(sameSmallSubnets({ enabled: true, mask: 26 }, { enabled: false, mask: 26 })).toBe(false);
    expect(sameSmallSubnets({ enabled: true, mask: 26 }, { enabled: true, mask: 27 })).toBe(false);
  });
});

describe("samePools", () => {
  const a = { base: "10.200.0.0/16", size: 24 };
  const b = { base: "10.201.0.0/16", size: 24 };

  it("compares length, order, base and size", () => {
    expect(samePools([a, b], [a, b])).toBe(true);
    expect(samePools([a], [a, b])).toBe(false);
    expect(samePools([a, b], [b, a])).toBe(false);
    expect(samePools([a], [{ ...a, size: 25 }])).toBe(false);
    expect(samePools([a], [{ ...a, base: "10.202.0.0/16" }])).toBe(false);
  });

  it("ignores surrounding spaces", () => {
    expect(samePools([a], [{ base: " 10.200.0.0/16 ", size: 24 }])).toBe(true);
  });

  it("treats two empty lists as the same", () => {
    expect(samePools([], [])).toBe(true);
  });
});
