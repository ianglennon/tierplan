// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { ServiceMarkerEvent, SwarmMark } from "@/features/logs/log-source";
import { ReplacementMarker, swarmMarkerText } from "@/features/logs/replacement-marker";
import i18n from "@/i18n";
import "@/i18n";

// The rows between a service's lines where a task was replaced or a node dropped out (plan
// 06-30, OBS-02, D-34, D-37; UI-SPEC "Replacement marker", "Node gap").

afterEach(cleanup);

const t = (key: string, options?: Record<string, unknown>) =>
  String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

const TASK = "abcdefghijklmnopqrstuvwxy";
const NEXT = "zyxwvutsrqponmlkjihgfedcb";

describe("swarmMarkerText", () => {
  const replaced = (extra: Partial<Omit<ServiceMarkerEvent, "at">> = {}): SwarmMark => ({
    kind: "replaced",
    slot: 2,
    old: TASK,
    oldState: "failed",
    oldNode: "alpha",
    exitCode: 1,
    new: NEXT,
    node: "beta",
    ...extra,
  });

  it("says a task failed with its exit code, and what took over on which node", () => {
    expect(swarmMarkerText(t, replaced(), "web")).toBe(
      "— web.2 replaced: task abcdefgh failed (exit 1) → task zyxwvuts starting on beta —",
    );
  });

  it("leaves the exit code out when the task has none", () => {
    expect(swarmMarkerText(t, replaced({ exitCode: undefined }), "web")).toBe(
      "— web.2 replaced: task abcdefgh failed → task zyxwvuts starting on beta —",
    );
  });

  it("says a task shut down, and a task was rejected", () => {
    expect(swarmMarkerText(t, replaced({ oldState: "shutdown", exitCode: 0 }), "web")).toBe(
      "— web.2 replaced: task abcdefgh shut down → task zyxwvuts starting on beta —",
    );
    expect(swarmMarkerText(t, replaced({ oldState: "rejected" }), "web")).toBe(
      "— web.2 replaced: task abcdefgh was rejected → task zyxwvuts starting on beta —",
    );
  });

  it("names the task of a global service by the service and its node", () => {
    expect(swarmMarkerText(t, replaced({ slot: 0 }), "web")).toBe(
      "— web@alpha replaced: task abcdefgh failed (exit 1) → task zyxwvuts starting on beta —",
    );
  });

  it("says a slot stopped and is waiting for a new task", () => {
    const stopped: SwarmMark = {
      kind: "stopped",
      slot: 3,
      old: TASK,
      oldState: "rejected",
      oldNode: "alpha",
    };
    expect(swarmMarkerText(t, stopped, "web")).toBe(
      "— web.3 stopped: task abcdefgh was rejected. Waiting for a new task. —",
    );
    expect(swarmMarkerText(t, { ...stopped, oldState: "failed", exitCode: 137 }, "web")).toBe(
      "— web.3 stopped: task abcdefgh failed (exit 137). Waiting for a new task. —",
    );
  });

  it("says a node cannot send logs, and that it is back", () => {
    expect(swarmMarkerText(t, { kind: "down", node: "beta" }, "web")).toBe(
      "— beta is down, so new lines from its tasks can't reach Bayplan. —",
    );
    expect(swarmMarkerText(t, { kind: "disconnected", node: "beta" }, "web")).toBe(
      "— beta is disconnected, so new lines from its tasks can't reach Bayplan. —",
    );
    expect(swarmMarkerText(t, { kind: "back", node: "beta" }, "web")).toBe("— beta is back. —");
  });

  it("shows a state it does not know as it was sent, as text", () => {
    expect(
      swarmMarkerText(t, replaced({ oldState: "<b>odd</b>", exitCode: undefined }), "web"),
    ).toContain("task abcdefgh <b>odd</b> →");
  });
});

describe("ReplacementMarker", () => {
  it("is a separator named by its text", () => {
    render(
      <ReplacementMarker
        text="— web.2 replaced: task a failed → task b starting on beta —"
        kind="replaced"
      />,
    );
    const row = screen.getByRole("separator", {
      name: "— web.2 replaced: task a failed → task b starting on beta —",
    });
    expect(row.getAttribute("data-swarm-marker")).toBe("replaced");
    expect(row.textContent).toBe("— web.2 replaced: task a failed → task b starting on beta —");
    expect(row.className).toContain("text-muted-foreground");
  });

  it("is not announced as the log changes", () => {
    render(<ReplacementMarker text="— beta is back. —" kind="back" />);
    const row = screen.getByRole("separator");
    expect(row.getAttribute("aria-live")).toBeNull();
    expect(row.closest("[aria-live]")).toBeNull();
  });

  it("draws markup in its text as text", () => {
    render(<ReplacementMarker text={"— <img src=x onerror=alert(1)> —"} kind="down" />);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByRole("separator").textContent).toContain("<img src=x onerror=alert(1)>");
  });
});
