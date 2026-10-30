// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { scaleEligibility } from "@/features/services/eligibility";

describe("scaleEligibility", () => {
  it("accepts a replicated service, with no note", () => {
    expect(scaleEligibility({ mode: "replicated", stack: "" })).toEqual({
      eligible: true,
      note: undefined,
      stack: undefined,
    });
  });

  it("skips a global service with the global note", () => {
    expect(scaleEligibility({ mode: "global", stack: "" })).toMatchObject({
      eligible: false,
      note: "global",
    });
  });

  it("skips both kinds of job with the job note", () => {
    for (const mode of ["replicated-job", "global-job"]) {
      expect(scaleEligibility({ mode, stack: "" })).toMatchObject({
        eligible: false,
        note: "job",
      });
    }
  });

  it("skips a mode it does not know, without a note", () => {
    expect(scaleEligibility({ mode: "", stack: "" })).toMatchObject({
      eligible: false,
      note: undefined,
    });
  });

  it("carries the stack of a service that has one, eligible or not", () => {
    expect(scaleEligibility({ mode: "replicated", stack: "shop" })).toMatchObject({
      eligible: true,
      stack: "shop",
    });
    expect(scaleEligibility({ mode: "global", stack: "shop" })).toMatchObject({
      eligible: false,
      note: "global",
      stack: "shop",
    });
  });
});
