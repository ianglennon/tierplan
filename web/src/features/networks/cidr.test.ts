// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import {
  containsAddress,
  containsCidr,
  overlaps,
  overlapsWith,
  parseCidr,
} from "@/features/networks/cidr";

describe("parseCidr", () => {
  it("reads an IPv4 prefix and drops the host bits", () => {
    expect(parseCidr("10.20.0.0/24")).toMatchObject({ family: 4, bits: 24 });
    expect(parseCidr("10.20.0.5/24")).toEqual(parseCidr("10.20.0.0/24"));
    expect(parseCidr(" 10.20.0.0/24 ")).toEqual(parseCidr("10.20.0.0/24"));
  });

  it("reads an IPv6 prefix, with and without compression", () => {
    expect(parseCidr("2001:db8::/32")).toMatchObject({ family: 6, bits: 32 });
    expect(parseCidr("2001:0db8:0000:0000:0000:0000:0000:0000/32")).toEqual(
      parseCidr("2001:db8::/32"),
    );
    expect(parseCidr("::/0")).toMatchObject({ family: 6, bits: 0 });
    expect(parseCidr("fd00::1/64")).toEqual(parseCidr("fd00::/64"));
  });

  it("refuses anything that is not a prefix", () => {
    for (const bad of [
      "",
      "abc",
      "10.20.0.0",
      "10.20.0.0/",
      "10.20.0.0/33",
      "10.20.0.0/-1",
      "10.20.0.0/024",
      "10.20.0/24",
      "10.20.0.0.0/24",
      "256.0.0.0/8",
      "010.0.0.0/8",
      "1:2:3/64",
      "2001:db8::/129",
      "2001:db8:::1/64",
      "[2001:db8::]/32",
      "10.20.0.0/24/8",
    ]) {
      expect(parseCidr(bad), bad).toBeNull();
    }
  });
});

describe("overlaps", () => {
  it("is true when one prefix contains the other, whichever comes first", () => {
    expect(overlaps("10.0.0.0/24", "10.0.0.128/25")).toBe(true);
    expect(overlaps("10.0.0.128/25", "10.0.0.0/24")).toBe(true);
    expect(overlaps("10.0.0.0/8", "10.200.1.0/24")).toBe(true);
    expect(overlaps("0.0.0.0/0", "192.168.1.0/24")).toBe(true);
  });

  it("is true for equal prefixes, however the host bits are written", () => {
    expect(overlaps("10.0.0.0/24", "10.0.0.0/24")).toBe(true);
    expect(overlaps("10.0.0.7/24", "10.0.0.200/24")).toBe(true);
  });

  it("is false for adjacent prefixes that share no address", () => {
    expect(overlaps("10.0.0.0/24", "10.0.1.0/24")).toBe(false);
    expect(overlaps("10.0.1.0/24", "10.0.0.0/24")).toBe(false);
    expect(overlaps("192.168.0.0/25", "192.168.0.128/25")).toBe(false);
  });

  it("handles IPv6 the same way", () => {
    expect(overlaps("2001:db8::/32", "2001:db8:1::/48")).toBe(true);
    expect(overlaps("2001:db8::/32", "2001:db9::/32")).toBe(false);
    expect(overlaps("fd00::/8", "fd12:3456::/32")).toBe(true);
    expect(overlaps("::/0", "2001:db8::/32")).toBe(true);
  });

  it("never overlaps across families, and a malformed input overlaps nothing", () => {
    expect(overlaps("0.0.0.0/0", "::/0")).toBe(false);
    expect(overlaps("10.0.0.0", "10.0.0.0/24")).toBe(false);
    expect(overlaps("10.0.0.0/24", "garbage")).toBe(false);
    expect(overlaps("", "")).toBe(false);
  });
});

describe("overlapsWith", () => {
  const net = (name: string, ...subnets: string[]) => ({ name, subnets });

  it("is null when nothing overlaps, the subnet is not a prefix, or the list is empty", () => {
    expect(overlapsWith("10.0.1.0/24", [net("a", "10.0.0.0/24")])).toBeNull();
    expect(overlapsWith("10.0.1", [net("a", "10.0.1.0/24")])).toBeNull();
    expect(overlapsWith("10.0.1.0/24", [])).toBeNull();
    expect(overlapsWith("10.0.1.0/24", [{ name: "none", subnets: null }])).toBeNull();
  });

  it("names the one network it overlaps with its subnet", () => {
    expect(
      overlapsWith("172.17.1.0/24", [net("bridge", "172.17.0.0/16"), net("b", "10.0.0.0/8")]),
    ).toEqual({
      total: 1,
      shown: [{ name: "bridge", cidr: "172.17.0.0/16" }],
      more: 0,
    });
  });

  it("names the first two and counts the rest", () => {
    const got = overlapsWith("10.0.0.0/16", [
      net("a", "10.0.1.0/24"),
      net("b", "10.0.2.0/24"),
      net("c", "10.0.3.0/24"),
      net("d", "10.0.4.0/24"),
      net("x", "192.168.0.0/24"),
    ]);
    expect(got).toEqual({
      total: 4,
      shown: [
        { name: "a", cidr: "10.0.1.0/24" },
        { name: "b", cidr: "10.0.2.0/24" },
      ],
      more: 2,
    });
  });

  it("counts a network that is listed on every node once", () => {
    const got = overlapsWith("172.17.0.0/16", [
      net("bridge", "172.17.0.0/16"),
      net("bridge", "172.17.0.0/16"),
      net("bridge", "172.17.0.0/16"),
    ]);
    expect(got?.total).toBe(1);
    expect(got?.shown).toEqual([{ name: "bridge", cidr: "172.17.0.0/16" }]);
  });

  it("reports the subnet of a network that overlaps, not its other subnets", () => {
    const got = overlapsWith("10.0.0.0/24", [net("multi", "192.168.0.0/24", "10.0.0.0/25")]);
    expect(got?.shown).toEqual([{ name: "multi", cidr: "10.0.0.0/25" }]);
  });
});

describe("containsAddress and containsCidr", () => {
  it("tells an address inside a prefix from one outside, at both edges", () => {
    expect(containsAddress("10.20.0.0/24", "10.20.0.1")).toBe(true);
    expect(containsAddress("10.20.0.0/24", "10.20.0.0")).toBe(true);
    expect(containsAddress("10.20.0.0/24", "10.20.0.255")).toBe(true);
    expect(containsAddress("10.20.0.0/24", "10.20.1.0")).toBe(false);
    expect(containsAddress("10.20.0.0/24", "10.19.255.255")).toBe(false);
    expect(containsAddress("fd00:20::/64", "fd00:20::1")).toBe(true);
    expect(containsAddress("fd00:20::/64", "fd00:21::1")).toBe(false);
  });

  it("is false across families and for anything malformed", () => {
    expect(containsAddress("10.20.0.0/24", "fd00::1")).toBe(false);
    expect(containsAddress("10.20.0.0/24", "gw")).toBe(false);
    expect(containsAddress("10.20.0.0", "10.20.0.1")).toBe(false);
  });

  it("accepts a range no wider than the subnet and starting inside it", () => {
    expect(containsCidr("10.20.0.0/24", "10.20.0.128/25")).toBe(true);
    expect(containsCidr("10.20.0.0/24", "10.20.0.0/24")).toBe(true);
    expect(containsCidr("10.20.0.0/24", "10.20.0.0/16")).toBe(false);
    expect(containsCidr("10.20.0.0/24", "10.20.1.0/25")).toBe(false);
    expect(containsCidr("10.20.0.0/24", "10.20.0.5")).toBe(false);
  });
});
