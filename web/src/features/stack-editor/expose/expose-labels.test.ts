// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { type ExposeTarget, setRouteField } from "@/features/stack-editor/expose/expose-labels";

// The label editor of the Expose panel (D-18, D-19). Its writer and the server's parser share one
// golden file (A-08): this suite applies each case's ops to its input and wants its output byte for
// byte; internal/stacks/expose_golden_test.go runs the real check on the same output and wants the
// routes the case names. A key written here that the server read differently would fail there.

interface Op {
  kind: "set";
  service: string;
  route: string;
  field: "host" | "port" | "path" | "scheme" | "strip";
  value: string;
}

interface GoldenCase {
  name: string;
  target: ExposeTarget;
  input: string;
  ops: Op[];
  output: string;
}

const golden: GoldenCase[] = JSON.parse(
  readFileSync(
    new URL("../../../../../internal/stacks/testdata/expose-golden.json", import.meta.url),
    "utf8",
  ),
);

function apply(text: string, target: ExposeTarget, op: Op): string {
  return setRouteField(text, op.service, target, op.route, op.field, op.value);
}

describe("the golden file", () => {
  it("has cases with unique names", () => {
    const names = golden.map((c) => c.name);
    expect(names.length).toBeGreaterThan(0);
    expect(new Set(names).size).toBe(names.length);
  });

  for (const c of golden) {
    it(`case ${c.name}: the ops turn the input into the output`, () => {
      const got = c.ops.reduce((text, op) => apply(text, c.target, op), c.input);
      expect(got).toBe(c.output);
    });
  }
});
