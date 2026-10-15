// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";
import { getPath, setPath } from "@/features/service-editor/spec-patch";
import {
  AUTO_ROLLBACK_DEFAULTS,
  autoRollbackState,
  seedUpdateConfig,
  setAutoRollback,
} from "@/features/service-editor/update-policy";
import type { SpecValue } from "@/features/spec-editor/spec-format";

// The auto-rollback choice (plan 06-21, D-12 amended, RESEARCH Pitfall 5): one field,
// UpdateConfig.FailureAction, shown in the Update policy card and on Review.

const bare = (): SpecValue => ({
  Name: "web",
  TaskTemplate: { ContainerSpec: { Image: "nginx" } },
});

describe("setAutoRollback", () => {
  it("seeds the CLI defaults when ticked on a service with no UpdateConfig, so the speed does not change", () => {
    const next = setAutoRollback(bare(), true);
    expect(getPath(next, "UpdateConfig")).toEqual({
      Parallelism: 1,
      Delay: 0,
      FailureAction: "rollback",
      Monitor: 5_000_000_000,
      MaxFailureRatio: 0,
      Order: "stop-first",
    });
    expect(AUTO_ROLLBACK_DEFAULTS).toEqual({
      Parallelism: 1,
      Delay: 0,
      Monitor: 5_000_000_000,
      MaxFailureRatio: 0,
      Order: "stop-first",
    });
    expect(next).toMatchObject({ Name: "web" });
  });

  it("leaves UpdateConfig absent when unticked on a service with none", () => {
    const spec = bare();
    const next = setAutoRollback(spec, false);
    expect(next).toBe(spec);
    expect(getPath(next, "UpdateConfig")).toBeUndefined();
  });

  it("treats a null UpdateConfig as absent", () => {
    const next = setAutoRollback({ ...bare(), UpdateConfig: null }, true);
    expect(getPath(next, "UpdateConfig.Parallelism")).toBe(1);
    expect(getPath(next, "UpdateConfig.FailureAction")).toBe("rollback");
  });

  it("changes only FailureAction when ticked on an UpdateConfig that exists", () => {
    const spec: SpecValue = {
      ...bare(),
      UpdateConfig: { Parallelism: 3, FailureAction: "continue" },
    };
    const next = setAutoRollback(spec, true);
    expect(getPath(next, "UpdateConfig")).toEqual({ Parallelism: 3, FailureAction: "rollback" });
    expect(spec).toEqual({
      ...bare(),
      UpdateConfig: { Parallelism: 3, FailureAction: "continue" },
    });
  });

  it("does not seed over an UpdateConfig that states Parallelism 0", () => {
    const next = setAutoRollback({ ...bare(), UpdateConfig: { Parallelism: 0 } }, true);
    expect(getPath(next, "UpdateConfig")).toEqual({ Parallelism: 0, FailureAction: "rollback" });
  });

  it("sets pause when unticked from rollback or continue", () => {
    const rolled: SpecValue = {
      ...bare(),
      UpdateConfig: { Parallelism: 3, FailureAction: "rollback" },
    };
    expect(getPath(setAutoRollback(rolled, false), "UpdateConfig")).toEqual({
      Parallelism: 3,
      FailureAction: "pause",
    });
    const cont: SpecValue = {
      ...bare(),
      UpdateConfig: { Parallelism: 3, FailureAction: "continue" },
    };
    expect(getPath(setAutoRollback(cont, false), "UpdateConfig.FailureAction")).toBe("pause");
  });

  it("changes nothing when unticked and the action is already pause or unstated", () => {
    const paused: SpecValue = {
      ...bare(),
      UpdateConfig: { Parallelism: 3, FailureAction: "pause" },
    };
    expect(setAutoRollback(paused, false)).toBe(paused);
    const unstated: SpecValue = { ...bare(), UpdateConfig: { Parallelism: 3 } };
    expect(setAutoRollback(unstated, false)).toBe(unstated);
  });

  it("changes nothing when ticked and it is already rollback", () => {
    const rolled: SpecValue = {
      ...bare(),
      UpdateConfig: { Parallelism: 2, FailureAction: "rollback" },
    };
    expect(setAutoRollback(rolled, true)).toBe(rolled);
  });

  it("never touches RollbackConfig", () => {
    const spec: SpecValue = {
      ...bare(),
      RollbackConfig: { Parallelism: 1, FailureAction: "pause" },
    };
    expect(getPath(setAutoRollback(spec, true), "RollbackConfig")).toEqual({
      Parallelism: 1,
      FailureAction: "pause",
    });
  });
});

describe("setAutoRollback, ticked and unticked again", () => {
  it("takes the UpdateConfig it created back out, so the review shows nothing changed", () => {
    const spec = bare();
    const next = setAutoRollback(setAutoRollback(spec, true), false);
    expect(next).toEqual(spec);
    expect(getPath(next, "UpdateConfig")).toBeUndefined();
  });

  it("keeps an UpdateConfig that says anything beyond the defaults", () => {
    const seeded = setAutoRollback(bare(), true);
    const faster = setPath(seeded, "UpdateConfig.Parallelism", 3);
    expect(getPath(setAutoRollback(faster, false), "UpdateConfig")).toEqual({
      Parallelism: 3,
      Delay: 0,
      FailureAction: "pause",
      Monitor: 5_000_000_000,
      MaxFailureRatio: 0,
      Order: "stop-first",
    });
  });
});

describe("seedUpdateConfig", () => {
  it("gives a service with no UpdateConfig the CLI defaults, with pause as the failure action", () => {
    expect(getPath(seedUpdateConfig(bare()), "UpdateConfig")).toEqual({
      Parallelism: 1,
      Delay: 0,
      FailureAction: "pause",
      Monitor: 5_000_000_000,
      MaxFailureRatio: 0,
      Order: "stop-first",
    });
    expect(
      getPath(seedUpdateConfig({ ...bare(), UpdateConfig: null }), "UpdateConfig.Parallelism"),
    ).toBe(1);
  });

  it("leaves an UpdateConfig that exists alone", () => {
    const spec: SpecValue = { ...bare(), UpdateConfig: { Parallelism: 0 } };
    expect(seedUpdateConfig(spec)).toBe(spec);
  });
});

describe("autoRollbackState", () => {
  it("is ticked for rollback", () => {
    expect(autoRollbackState({ UpdateConfig: { FailureAction: "rollback" } })).toEqual({
      checked: true,
      continues: false,
    });
  });

  it("is unticked, with the continue note, for continue", () => {
    expect(autoRollbackState({ UpdateConfig: { FailureAction: "continue" } })).toEqual({
      checked: false,
      continues: true,
    });
  });

  it("is unticked for pause, for no action stated and for no UpdateConfig", () => {
    for (const spec of [
      { UpdateConfig: { FailureAction: "pause" } },
      { UpdateConfig: {} },
      {},
      null,
      "x",
    ]) {
      expect(autoRollbackState(spec)).toEqual({ checked: false, continues: false });
    }
  });
});
