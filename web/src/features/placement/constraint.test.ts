// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import {
  matchesAll,
  matchesConstraint,
  type NodeFacts,
  parseConstraint,
} from "@/features/placement/constraint";
import vectorData from "@/features/placement/constraint-vectors.json";

// The same vectors internal/envs/constraint_test.go reads, so the browser and the server
// cannot disagree about which nodes a constraint matches (RES-12, RESEARCH Pattern 9).

interface Vector {
  constraint: string;
  node: NodeFacts;
  matches?: boolean;
  error?: boolean;
}

const vectors = vectorData as unknown as readonly Vector[];

describe("the shared constraint vectors", () => {
  it("hold at least 30 cases", () => {
    expect(vectors.length).toBeGreaterThanOrEqual(30);
  });

  for (const v of vectors) {
    it(`${JSON.stringify(v.constraint)} on ${v.node.id}`, () => {
      const got = matchesAll([v.constraint], v.node);
      if (v.error) {
        expect(got.ok).toBe(false);
        expect(parseConstraint(v.constraint).ok).toBe(false);
        return;
      }
      expect(got).toEqual({ ok: true, matches: v.matches });
    });
  }
});

describe("parseConstraint", () => {
  it("keeps the key, the value and the operator, trimmed", () => {
    expect(parseConstraint("  node.labels.disk != SSD ")).toEqual({
      ok: true,
      constraint: { key: "node.labels.disk", value: "SSD", equal: false },
    });
    expect(parseConstraint("node.role==manager")).toMatchObject({
      ok: true,
      constraint: { equal: true },
    });
  });

  it("explains a constraint that is not key, an operator and a value", () => {
    const bad = parseConstraint("node.role=manager");
    expect(bad.ok).toBe(false);
  });
});

describe("matchesConstraint", () => {
  const node: NodeFacts = {
    id: "n1",
    hostname: "wrk-1",
    ip: "10.0.0.7",
    role: "worker",
    os: "linux",
    arch: "x86_64",
    labels: { disk: "ssd" },
    engineLabels: {},
  };

  it("applies one parsed constraint to a node", () => {
    const eq = parseConstraint("node.labels.disk==SSD");
    const ne = parseConstraint("node.labels.disk!=SSD");
    if (!eq.ok || !ne.ok) throw new Error("constraints should parse");
    expect(matchesConstraint(eq.constraint, node)).toBe(true);
    expect(matchesConstraint(ne.constraint, node)).toBe(false);
  });
});

describe("matchesAll", () => {
  const node: NodeFacts = {
    id: "n1",
    hostname: "wrk-1",
    ip: "10.0.0.7",
    role: "worker",
    os: "linux",
    arch: "x86_64",
    labels: { disk: "ssd" },
    engineLabels: {},
  };

  it("matches when there is nothing to match", () => {
    expect(matchesAll([], node)).toEqual({ ok: true, matches: true });
  });

  it("needs every constraint to hold", () => {
    expect(matchesAll(["node.role==worker", "node.labels.disk==ssd"], node)).toEqual({
      ok: true,
      matches: true,
    });
    expect(matchesAll(["node.role==worker", "node.labels.disk==hdd"], node)).toEqual({
      ok: true,
      matches: false,
    });
  });

  it("refuses the whole list when one constraint is malformed, naming it", () => {
    const got = matchesAll(["node.role==worker", "node.role=manager"], node);
    expect(got.ok).toBe(false);
    if (!got.ok) expect(got.error).toContain("node.role=manager");
  });
});
