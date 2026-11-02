// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { type ServiceStateInput, serviceState } from "@/features/services/service-state";

const row = (extra: Partial<ServiceStateInput> = {}): ServiceStateInput => ({
  mode: "replicated",
  running: 2,
  desired: 2,
  updateState: "",
  ...extra,
});

describe("serviceState", () => {
  it("reads a replicated service with every desired task running as Running", () => {
    expect(serviceState(row())).toEqual({
      key: "running",
      tone: "online",
      labelKey: "services.list.state.running",
    });
  });

  it("reads some but not all desired tasks running as Degraded, in the warning colour", () => {
    expect(serviceState(row({ running: 1, desired: 3 }))).toMatchObject({
      key: "degraded",
      tone: "degraded",
    });
  });

  it("reads desired above zero with none running as Not running, offline", () => {
    expect(serviceState(row({ running: 0, desired: 2 }))).toMatchObject({
      key: "notRunning",
      tone: "offline",
    });
  });

  it("reads desired zero as Scaled to 0, neutral", () => {
    expect(serviceState(row({ running: 0, desired: 0 }))).toMatchObject({
      key: "scaledToZero",
      tone: "unknown",
    });
  });

  it("reads a job with nothing running as Completed, neutral, and one still running as Running", () => {
    expect(serviceState(row({ mode: "replicated-job", running: 0, desired: 0 }))).toMatchObject({
      key: "completed",
      tone: "unknown",
    });
    expect(serviceState(row({ mode: "global-job", running: 0, desired: 0 }))).toMatchObject({
      key: "completed",
    });
    expect(serviceState(row({ mode: "replicated-job", running: 2, desired: 0 }))).toMatchObject({
      key: "running",
      tone: "online",
    });
  });

  it("reads an update in progress as Updating and a rollback in progress as Rolling back, pulsing", () => {
    expect(serviceState(row({ updateState: "updating" }))).toEqual({
      key: "updating",
      tone: "reconnecting",
      labelKey: "services.list.state.updating",
    });
    expect(serviceState(row({ updateState: "rollback_started" }))).toMatchObject({
      key: "rollingBack",
      tone: "reconnecting",
    });
  });

  it("reads a paused update or a paused rollback as offline", () => {
    expect(serviceState(row({ updateState: "paused" }))).toMatchObject({
      key: "updatePaused",
      tone: "offline",
    });
    expect(serviceState(row({ updateState: "rollback_paused" }))).toMatchObject({
      key: "rollbackPaused",
      tone: "offline",
    });
  });

  it("reads a finished rollback as Rolled back, in the warning colour", () => {
    expect(serviceState(row({ updateState: "rollback_completed" }))).toMatchObject({
      key: "rolledBack",
      tone: "degraded",
    });
  });

  it("reads a completed update by its replica counts", () => {
    expect(serviceState(row({ updateState: "completed" })).key).toBe("running");
    expect(serviceState(row({ updateState: "completed", running: 0 })).key).toBe("notRunning");
  });

  it("puts a paused update before an update in progress, a rollback before replica counts, and not running before degraded", () => {
    // Paused wins over everything: a paused update of a service with nothing running.
    expect(serviceState(row({ updateState: "paused", running: 0 })).key).toBe("updatePaused");
    expect(serviceState(row({ updateState: "rollback_paused", running: 0 })).key).toBe(
      "rollbackPaused",
    );
    // Updating wins over the replica counts.
    expect(serviceState(row({ updateState: "updating", running: 0 })).key).toBe("updating");
    expect(serviceState(row({ updateState: "rollback_started", running: 1, desired: 3 })).key).toBe(
      "rollingBack",
    );
    // Rolled back wins over not running, degraded and scaled to 0.
    expect(serviceState(row({ updateState: "rollback_completed", running: 0 })).key).toBe(
      "rolledBack",
    );
    expect(
      serviceState(row({ updateState: "rollback_completed", running: 0, desired: 0 })).key,
    ).toBe("rolledBack");
  });

  it("ignores an update state it does not know and reads the replica counts", () => {
    expect(serviceState(row({ updateState: "something_new" })).key).toBe("running");
  });
});
