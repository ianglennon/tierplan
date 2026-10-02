// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
});
