// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { safeNext } from "@/lib/redirect";

describe("safeNext", () => {
  it("keeps a same-origin path with its query string", () => {
    expect(safeNext("/environments/e1?tab=x")).toBe("/environments/e1?tab=x");
    expect(safeNext("/")).toBe("/");
  });

  it("falls back to / when there is nothing to follow", () => {
    expect(safeNext(undefined)).toBe("/");
    expect(safeNext("")).toBe("/");
  });

  it.each([
    "//evil.example",
    "//evil.example/path",
    "/\\evil.example",
    "/path\\..\\..",
    "https://evil.example",
    "http://evil.example/",
    "javascript:alert(1)",
    "evil.example",
    "relative/path",
    "/ok\n//evil.example",
    "/ok\t",
    "/\u0000x",
  ])("refuses %j", (value) => {
    expect(safeNext(value)).toBe("/");
  });
});
