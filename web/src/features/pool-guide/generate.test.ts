// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import {
  daemonJsonSnippet,
  parseAvoidRanges,
  planSwarmPool,
  readSwarmInputs,
  stackRemoveCommands,
  swarmInitCommand,
} from "@/features/pool-guide/generate";

const plan = (input: Partial<Parameters<typeof planSwarmPool>[0]> = {}) =>
  planSwarmPool({ networks: 50, nodes: 5, mask: 24, avoid: [], ...input });

const avoid = (text: string) => parseAvoidRanges(text).ranges;

describe("planSwarmPool", () => {
  it("plans a /17 for 50 networks of /24", () => {
    // 1.5 x 50 = 75, and the next power of two is 128: a /17 holds 128 networks of /24.
    expect(plan()).toEqual({
      plan: expect.objectContaining({
        pool: "10.0.0.0/17",
        capacity: 128,
        spare: 78,
        mask: 24,
        networks: 50,
      }),
    });
  });

  it("gives a /26 network its own size, so the pool is as big as the networks need", () => {
    // 75 -> 128 networks of /26 is 8192 addresses, a /19.
    expect(plan({ mask: 26 })).toEqual({
      plan: expect.objectContaining({ pool: "10.0.0.0/19", capacity: 128 }),
    });
  });

  it("skips avoided ranges", () => {
    expect(plan({ avoid: avoid("10.0.0.0/8") })).toEqual({
      plan: expect.objectContaining({ pool: "172.16.0.0/17" }),
    });
  });

  it("steps past an avoided range to the next aligned block", () => {
    // 10.0.0.0/16 is taken, so the /17 starts where the next /17 does.
    expect(plan({ avoid: avoid("10.0.0.0/16") })).toEqual({
      plan: expect.objectContaining({ pool: "10.1.0.0/17" }),
    });
    // An avoided range inside the first block moves the pool on, not into it.
    expect(plan({ avoid: avoid("10.0.64.0/24") })).toEqual({
      plan: expect.objectContaining({ pool: "10.0.128.0/17" }),
    });
  });

  it("says when no range is big enough", () => {
    expect(plan({ avoid: avoid("10.0.0.0/8\n172.16.0.0/12") })).toEqual({ error: "no_fit" });
    // 100000 networks of /28 want a /11: only 10.0.0.0/8 holds one, and it is avoided.
    expect(plan({ networks: 100_000, mask: 28, avoid: avoid("10.0.0.0/8") })).toEqual({
      error: "no_fit",
    });
  });

  it("notes a size too small for the nodes", () => {
    const small = plan({ mask: 28, nodes: 20 });
    expect(small).toEqual({
      plan: expect.objectContaining({ usable: 13, tooSmallForNodes: true, nodes: 20 }),
    });
    expect(plan({ mask: 28, nodes: 13 })).toEqual({
      plan: expect.objectContaining({ tooSmallForNodes: false }),
    });
  });

  it("refuses inputs that are not whole numbers in range", () => {
    expect(plan({ networks: 0 })).toEqual({ error: "invalid" });
    expect(plan({ networks: 1.5 })).toEqual({ error: "invalid" });
    expect(plan({ nodes: -1 })).toEqual({ error: "invalid" });
    expect(plan({ mask: 23 })).toEqual({ error: "invalid" });
  });
});

describe("parseAvoidRanges", () => {
  it("reads one CIDR a line and ignores blank lines", () => {
    const read = parseAvoidRanges("192.168.0.0/16\n\n  10.5.0.1/24 \r\n");
    expect(read.badLines).toEqual([]);
    expect(read.ranges).toHaveLength(2);
  });

  it("refuses an injection attempt in a range line", () => {
    for (const bad of [
      "10.0.0.0/8; rm -rf /",
      "10.0.0.0/8 && curl evil",
      "$(id)/8",
      "10.0.0.0/8`id`",
      "10.0.0.0/8\\n",
    ]) {
      expect(parseAvoidRanges(bad).badLines).toEqual([1]);
    }
    expect(parseAvoidRanges("192.168.0.0/16\n10.0.0.0/8; rm -rf /").badLines).toEqual([2]);
  });

  it("refuses an IPv6 range and a line that is only an address", () => {
    expect(parseAvoidRanges("fd00::/8\n10.0.0.1").badLines).toEqual([1, 2]);
  });
});

describe("readSwarmInputs", () => {
  const raw = { networks: "50", nodes: "5", mask: "24", avoid: "" };

  it("gives the command for the defaults", () => {
    const read = readSwarmInputs(raw);
    expect(read).toMatchObject({
      kind: "ok",
      command:
        "docker swarm init --default-addr-pool 10.0.0.0/17 --default-addr-pool-mask-length 24",
    });
  });

  it("gives an error and no command when a field fails", () => {
    expect(readSwarmInputs({ ...raw, networks: "5; ls" })).toEqual({
      kind: "invalid",
      networks: true,
      nodes: false,
      avoidLines: [],
    });
    expect(readSwarmInputs({ ...raw, avoid: "10.0.0.0/8; rm -rf /" })).toMatchObject({
      kind: "invalid",
      avoidLines: [1],
    });
    expect(readSwarmInputs({ ...raw, avoid: "10.0.0.0/8\n172.16.0.0/12" })).toEqual({
      kind: "no_fit",
    });
  });
});

describe("swarmInitCommand", () => {
  it("builds the line from a canonical CIDR and an integer", () => {
    expect(swarmInitCommand("10.20.0.0/16", 24)).toBe(
      "docker swarm init --default-addr-pool 10.20.0.0/16 --default-addr-pool-mask-length 24",
    );
  });

  it("throws on anything else", () => {
    expect(() => swarmInitCommand("10.20.0.0/16; rm -rf /", 24)).toThrow();
    expect(() => swarmInitCommand("10.20.0.5/16", 24)).toThrow();
    expect(() => swarmInitCommand("fd00::/8", 24)).toThrow();
    expect(() => swarmInitCommand("10.20.0.0/16", 24.5)).toThrow();
    expect(() => swarmInitCommand("10.20.0.0/16", Number.NaN)).toThrow();
  });
});

describe("daemonJsonSnippet", () => {
  it("is valid JSON with one object a row", () => {
    const text = daemonJsonSnippet([
      { base: "10.20.0.0/16", size: "24" },
      { base: "10.30.0.0/16", size: " 26 " },
    ]);
    expect(JSON.parse(text)).toEqual({
      "default-address-pools": [
        { base: "10.20.0.0/16", size: 24 },
        { base: "10.30.0.0/16", size: 26 },
      ],
    });
    expect(text.startsWith('{\n  "default-address-pools": [\n    {')).toBe(true);
  });

  it("throws on a base that is not canonical or a size out of range", () => {
    expect(() => daemonJsonSnippet([{ base: '10.0.0.0/8"}], "x": [{"', size: "24" }])).toThrow();
    expect(() => daemonJsonSnippet([{ base: "10.0.0.1/16", size: "24" }])).toThrow();
    expect(() => daemonJsonSnippet([{ base: "10.0.0.0/16", size: "8" }])).toThrow();
    expect(() => daemonJsonSnippet([{ base: "10.0.0.0/16", size: "31" }])).toThrow();
    expect(() => daemonJsonSnippet([{ base: "10.0.0.0/16", size: "24; ls" }])).toThrow();
    expect(() => daemonJsonSnippet([])).toThrow();
  });
});

describe("stackRemoveCommands", () => {
  it("writes a line for each plain name and none for the rest", () => {
    expect(stackRemoveCommands(["web", "shop_v2", "a-b"])).toEqual([
      "docker stack rm web",
      "docker stack rm shop_v2",
      "docker stack rm a-b",
    ]);
    expect(stackRemoveCommands(["my stack", "x;rm -rf /", "$(id)", "ok", ""])).toEqual([
      "docker stack rm ok",
    ]);
  });
});
