// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";
import { z } from "zod";

import "./zod-config";

describe("zod configuration", () => {
  it("turns the eval-based compiler off, so the CSP is never asked about eval", () => {
    expect(z.config().jitless).toBe(true);
  });

  it("still parses", () => {
    const schema = z.object({ name: z.string().min(1) });
    expect(schema.safeParse({ name: "x" }).success).toBe(true);
    expect(schema.safeParse({ name: "" }).success).toBe(false);
  });

  // A schema built from "zod" itself can run before the entry has set jitless (WR-C02). Biome's
  // noRestrictedImports says the same at lint time; this keeps it true where the linter is not run.
  it("is the only module a source file imports z from", () => {
    const sources = import.meta.glob(["/src/**/*.{ts,tsx}", "!/src/**/*.test.{ts,tsx}"], {
      query: "?raw",
      import: "default",
      eager: true,
    }) as Record<string, string>;
    expect(Object.keys(sources).length).toBeGreaterThan(100);
    const offenders = Object.entries(sources)
      .filter(
        ([path, code]) => /from\s+["']zod["']/.test(code) && path !== "/src/lib/zod-config.ts",
      )
      .map(([path]) => path);
    expect(offenders).toEqual([]);
  });
});
