// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { formatCpu, formatMemory, formatRate, percentOf } from "./format";

describe("stats formatting", () => {
  it("shows a rate in binary units per second", () => {
    expect(formatRate(0)).toBe("0 B/s");
    expect(formatRate(1536)).toBe("1.5 KiB/s");
    expect(formatRate(1.2 * 1024 * 1024)).toBe("1.2 MiB/s");
  });

  it("shows a missing value as a dash, never NaN", () => {
    expect(formatRate(null)).toBe("—");
    expect(formatRate(Number.NaN)).toBe("—");
    expect(formatCpu(null)).toBe("—");
    expect(formatCpu(Number.NaN)).toBe("—");
    expect(formatMemory(null)).toBe("—");
  });

  it("shows cpu as whole percent of one core", () => {
    expect(formatCpu(142.4)).toBe("142%");
    expect(formatCpu(0)).toBe("0%");
  });

  it("shows memory as a size", () => {
    expect(formatMemory(1.2 * 1024 ** 3)).toBe("1.2 GiB");
  });

  it("works out how much of a limit is used, and nothing without a limit", () => {
    expect(percentOf(1.2 * 1024 ** 3, 2 * 1024 ** 3)).toBe(60);
    expect(percentOf(5, 0)).toBe(0);
  });
});
