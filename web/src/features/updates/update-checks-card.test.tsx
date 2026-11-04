// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateChecksCard } from "@/features/updates/update-checks-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { updateCheckDefaults, updateCheckStatus } from "@/test-update-checks";

const ENV = "e1";
const env = { id: ENV, name: "Prod" } as EnvironmentItem;

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

// What the status reads like after a save: each field the save set is the environment's own, each it
// sent as null follows the default, and the version moves on.
function applySave(
  from: ReturnType<typeof updateCheckStatus>,
  body: Record<string, unknown>,
): ReturnType<typeof updateCheckStatus> {
  const d = updateCheckDefaults();
  const sent = (v: unknown) => v !== null && v !== undefined;
  return {
    ...from,
    version: from.version + 1,
    enabled: sent(body.enabled)
      ? { value: body.enabled as boolean, overridden: true }
      : { value: d.enabled, overridden: false },
    schedule: sent(body.schedule)
      ? {
          value: body.schedule as string,
          preset: body.preset as typeof d.preset,
          overridden: true,
        }
      : { value: d.schedule, preset: d.preset, overridden: false },
  };
}

let me: ReturnType<typeof makeMe>;
let statusBody: ReturnType<typeof updateCheckStatus>;
let defaultsBody: ReturnType<typeof updateCheckDefaults>;
let statusReads: number;
let saves: Record<string, unknown>[];
let asked: string[];
// What the stubbed server answers to schedule-preview, and to a save instead of the usual.
let previewBody: Record<string, unknown>;
let saveAnswer: (() => Response) | undefined;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  statusBody = updateCheckStatus();
  defaultsBody = updateCheckDefaults();
  statusReads = 0;
  saves = [];
  asked = [];
  saveAnswer = undefined;
  previewBody = {
    valid: true,
    problem: "",
    nextRuns: ["2026-11-05T03:07:00Z", "2026-11-06T03:07:00Z", "2026-11-07T03:07:00Z"],
    timezone: "Europe/London",
  };
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/updates` && req.method === "GET") {
        statusReads++;
        return Response.json(statusBody);
      }
      if (url.pathname === `/api/env/${ENV}/updates/policy` && req.method === "PUT") {
        saves.push((await req.json()) as Record<string, unknown>);
        if (saveAnswer) return saveAnswer();
        statusBody = applySave(statusBody, saves[saves.length - 1] ?? {});
        return Response.json(statusBody);
      }
      if (url.pathname === `/api/env/${ENV}/updates/schedule-preview`) {
        asked.push(((await req.json()) as { schedule: string }).schedule);
        return Response.json(previewBody);
      }
      if (url.pathname === "/api/settings/update-checks") return Response.json(defaultsBody);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <UpdateChecksCard env={env} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

async function pickCheck(option: string) {
  fireEvent.keyDown(await screen.findByRole("combobox", { name: "Check" }), { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("UpdateChecksCardTracer", () => {
  it("shows the card with its switch, schedule and status line", async () => {
    mount();
    expect(await screen.findByText("Image update checks")).toBeTruthy();
    const toggle = await screen.findByRole("switch", {
      name: "Check this environment for image updates",
    });
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Every 6 hours");
    // Both fields follow the default.
    expect(screen.getAllByText("Default")).toHaveLength(2);
    expect(document.getElementById("update-checks")).not.toBeNull();
    expect(screen.getByText(/Last checked/).textContent).toMatch(
      /Last checked .* ago\. Next check /,
    );
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    // One read of the status, and no question about a preset's times.
    expect(statusReads).toBe(1);
    expect(asked).toEqual([]);
  });

  it("saves Daily at 03:00 with its cron, the preset and the version it read", async () => {
    mount();
    await pickCheck("Daily");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    expect((screen.getByLabelText("At") as HTMLInputElement).value).toBe("03:00");
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual({
      version: 3,
      enabled: null,
      schedule: "0 3 * * *",
      preset: "daily",
    });
    await waitFor(() =>
      expect(toasts.success).toHaveBeenCalledWith("Saved the update check settings."),
    );
    // The answer replaces the form: the schedule is now the environment's own.
    expect(await screen.findByText("Overridden")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Daily");
    expect(screen.queryByText("Unsaved changes")).toBeNull();
  });

  it("is no change at all to pick the schedule it already has", async () => {
    mount();
    await pickCheck("Weekly");
    await pickCheck("Every 6 hours");
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
  });
});
