// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { withTypedValues } from "@/features/service-editor/typed-values";

describe("withTypedValues", () => {
  it("puts a typed value where the server withheld one", () => {
    const masked = { Env: ["A=1", "TOKEN=<hidden>"] };
    const typed = { Env: ["A=1", "TOKEN=new"] };
    expect(withTypedValues(masked, typed)).toEqual(typed);
  });

  it("keeps what the server wrote everywhere it did not withhold a value", () => {
    const masked = { Image: "web:1@sha256:abc", Env: ["TOKEN=<hidden>"] };
    const typed = { Image: "web:1", Env: ["TOKEN=<hidden>"] };
    expect(withTypedValues(masked, typed)).toEqual(masked);
  });

  it("keeps a withheld value that was left alone", () => {
    const masked = { Env: ["TOKEN=<hidden>"] };
    expect(withTypedValues(masked, masked)).toEqual(masked);
  });

  it("goes into maps, and leaves a key the person removed as the server has it", () => {
    const masked = { LogDriver: { Options: { token: "<hidden>", size: "10m" } } };
    const typed = { LogDriver: { Options: { token: "fresh" } } };
    expect(withTypedValues(masked, typed)).toEqual({
      LogDriver: { Options: { token: "fresh", size: "10m" } },
    });
  });

  it("keeps a withheld list whose length changed as the server masked it", () => {
    const masked = { Command: ["<hidden>"] };
    const typed = { Command: ["/bin/app", "--flag"] };
    expect(withTypedValues(masked, typed)).toEqual(masked);
  });

  it("does not take a typed value of another kind", () => {
    expect(withTypedValues({ A: "<hidden>" }, { A: 5 })).toEqual({ A: "<hidden>" });
    expect(withTypedValues({ A: "<hidden>" }, "x")).toEqual({ A: "<hidden>" });
  });
});
