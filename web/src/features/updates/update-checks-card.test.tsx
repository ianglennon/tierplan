// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { updateCheckDefaultsKey } from "@/api/updates";
import { TooltipProvider } from "@/components/ui/tooltip";
import { formatServerTime } from "@/features/janitor/status-text";
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

const problem = (type: string, status: number, errors: { location: string; message: string }[]) =>
  new Response(JSON.stringify({ type, title: "Error", status, errors }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

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
// What the stubbed server answers to schedule-preview for one expression, instead of previewBody.
let previewFor: Record<string, Record<string, unknown>>;
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
  previewFor = {};
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
        const schedule = ((await req.json()) as { schedule: string }).schedule;
        asked.push(schedule);
        return Response.json(previewFor[schedule] ?? previewBody);
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

const reasonOf = (el: HTMLElement) =>
  document.getElementById(el.getAttribute("aria-describedby")?.split(" ").pop() ?? "")?.textContent;

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

async function pickCustom(cron: string) {
  await pickCheck("Custom (cron)");
  fireEvent.change(await screen.findByLabelText("Cron expression"), { target: { value: cron } });
}

describe("UpdateChecksCardSchedulePreview", () => {
  it("shows the next three checks of a custom schedule as a status line", async () => {
    mount();
    await pickCustom("0 3 * * *");
    const line = await screen.findByText(/^Next checks: /);
    expect(line.getAttribute("role")).toBe("status");
    const times = (previewBody.nextRuns as string[]).map((at) =>
      formatServerTime(at, "Europe/London"),
    );
    expect(line.textContent).toBe(`Next checks: ${times.join("; ")}.`);
    expect(asked.at(-1)).toBe("0 3 * * *");
  });

  it("names the one-hour floor for an expression that fires more often", async () => {
    previewFor["*/30 * * * *"] = {
      valid: false,
      problem: "too_frequent",
      nextRuns: null,
      timezone: "Europe/London",
    };
    mount();
    await pickCustom("*/30 * * * *");
    expect(await screen.findByText("Checks can be at most once an hour.")).toBeTruthy();
    expect(screen.getByLabelText("Cron expression").getAttribute("aria-invalid")).toBe("true");
  });

  it("shows the cron example for an expression it cannot read", async () => {
    previewFor.bad = {
      valid: false,
      problem: "invalid",
      nextRuns: null,
      timezone: "Europe/London",
    };
    mount();
    await pickCustom("bad");
    expect(
      await screen.findByText(
        "Enter a cron expression like 0 3 * * * (minute hour day month weekday).",
      ),
    ).toBeTruthy();
  });

  it("shows the times the server sends across a daylight saving change, unshifted", async () => {
    // Europe/London springs forward on 29 March 2026: the 01:30 check of the 29th moves to 03:30 and
    // the offset goes from +00:00 to +01:00. The browser does no cron arithmetic, so it shows the
    // server's wall clock whatever time zone the browser is in.
    previewFor["30 1 * * *"] = {
      valid: true,
      problem: "",
      nextRuns: [
        "2026-03-28T01:30:00+00:00",
        "2026-03-29T03:30:00+01:00",
        "2026-03-30T01:30:00+01:00",
      ],
      timezone: "Europe/London",
    };
    mount();
    await pickCustom("30 1 * * *");
    const line = await screen.findByText(/^Next checks: /);
    const [first, second, third] = (line.textContent ?? "").replace(/\.$/, "").split("; ");
    expect(first).toMatch(/28/);
    expect(first).toMatch(/01:30/);
    expect(second).toMatch(/29/);
    expect(second).toMatch(/03:30/);
    expect(third).toMatch(/30/);
    expect(third).toMatch(/01:30/);
  });
});

describe("UpdateChecksCardRefusals", () => {
  it("shows the conflict with who changed it, and Reload reads the status again", async () => {
    saveAnswer = () =>
      problem("settings_changed", 409, [{ location: "changedBy", message: "Ada" }]);
    mount();
    await pickCheck("Weekly");
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    expect(
      await screen.findByText(
        "Ada changed this schedule while you were editing. Reload to see it.",
      ),
    ).toBeTruthy();
    const reads = statusReads;
    statusBody = updateCheckStatus({
      version: 4,
      schedule: { value: "0 * * * *", preset: "hourly", overridden: true },
    });
    saveAnswer = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(statusReads).toBe(reads + 1));
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Hourly"),
    );
    expect(screen.queryByText(/while you were editing/)).toBeNull();
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    // The next save carries the version it was reloaded to.
    await pickCheck("Daily");
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    await waitFor(() => expect(saves.at(-1)).toMatchObject({ version: 4 }));
  });

  it("says a failed save failed in an inline alert, and keeps the edit", async () => {
    saveAnswer = () => problem("internal_error", 500, []);
    mount();
    await pickCheck("Weekly");
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Couldn't save the update check settings. Try again.");
    expect(toasts.success).not.toHaveBeenCalled();
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
  });

  it("names the floor when the server refuses the schedule on save", async () => {
    saveAnswer = () =>
      problem("schedule_too_frequent", 422, [{ location: "body.schedule", message: "too often" }]);
    mount();
    await pickCheck("Hourly");
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    expect(await screen.findByText("Checks can be at most once an hour.")).toBeTruthy();
  });

  it("asks for a time when Daily has none, once Save is pressed", async () => {
    mount();
    await pickCheck("Daily");
    fireEvent.change(screen.getByLabelText("At"), { target: { value: "" } });
    expect(screen.queryByText("Pick a time of day.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    expect((await screen.findByText("Pick a time of day.")).getAttribute("role")).toBe("status");
    expect(saves).toHaveLength(0);
  });
});

describe("UpdateChecksCardInheritance", () => {
  it("shows Reset on an overridden field, and Reset returns it to the default with focus on its control", async () => {
    me = makeMe({ instanceAdmin: true });
    statusBody = updateCheckStatus({
      schedule: { value: "0 3 * * *", preset: "daily", overridden: true },
    });
    mount();
    await waitFor(() => expect(client.getQueryData(updateCheckDefaultsKey)).toBeTruthy());
    expect((await screen.findByRole("combobox", { name: "Check" })).textContent).toContain("Daily");
    expect(screen.getByText("Overridden")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Reset Schedule to default" }));
    const preset = screen.getByRole("combobox", { name: "Check" });
    expect(preset.textContent).toContain("Every 6 hours");
    expect(screen.queryByText("Overridden")).toBeNull();
    expect(screen.getAllByText("Default")).toHaveLength(2);
    await waitFor(() => expect(document.activeElement).toBe(preset));
    // The save hands the schedule back to the default.
    fireEvent.click(screen.getByRole("button", { name: "Save update check settings" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual({ version: 3, enabled: null, schedule: null, preset: null });
  });

  it("follows a changed default after a reload, and leaves an overridden field alone", async () => {
    statusBody = updateCheckStatus({
      enabled: { value: false, overridden: true },
      schedule: { value: "0 3 * * *", preset: "daily", overridden: false },
    });
    mount();
    expect((await screen.findByRole("combobox", { name: "Check" })).textContent).toContain("Daily");
    // The instance default moves to hourly: the schedule follows it, the environment's own switch does not.
    cleanup();
    client.clear();
    statusBody = updateCheckStatus({
      enabled: { value: false, overridden: true },
      schedule: { value: "0 * * * *", preset: "hourly", overridden: false },
    });
    mount();
    expect((await screen.findByRole("combobox", { name: "Check" })).textContent).toContain(
      "Hourly",
    );
    expect(
      screen
        .getByRole("switch", { name: "Check this environment for image updates" })
        .getAttribute("aria-checked"),
    ).toBe("false");
    expect(screen.getAllByText("Default")).toHaveLength(1);
    expect(screen.getAllByText("Overridden")).toHaveLength(1);
  });
});

describe("UpdateChecksCardStatusLine", () => {
  it("says Not checked yet when no check has run", async () => {
    statusBody = updateCheckStatus({ lastCheckedAt: "", nextCheckAt: null });
    mount();
    expect(await screen.findByText("Not checked yet.")).toBeTruthy();
  });

  it("gives the next check in the server's time zone", async () => {
    statusBody = updateCheckStatus({ nextCheckAt: "2026-11-04T06:07:00+00:00" });
    mount();
    const line = await screen.findByText(/Last checked/);
    expect(line.textContent).toContain(
      `Next check ${formatServerTime("2026-11-04T06:07:00+00:00", "Europe/London")}.`,
    );
  });

  it("leaves the next check out while scheduled checks are off", async () => {
    statusBody = updateCheckStatus({
      nextCheckAt: null,
      enabled: { value: false, overridden: true },
    });
    mount();
    const line = await screen.findByText(/Last checked/);
    expect(line.textContent).not.toContain("Next check");
  });
});

describe("UpdateChecksCardReadOnly", () => {
  const reason = "Needs Admin on Prod";

  beforeEach(() => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    statusBody = updateCheckStatus({
      schedule: { value: "0 3 * * *", preset: "daily", overridden: true },
    });
  });

  it("shows every field read-only with the reason on each, and sends nothing", async () => {
    mount();
    const toggle = await screen.findByRole("switch", {
      name: "Check this environment for image updates",
    });
    await waitFor(() => expect(toggle.getAttribute("aria-disabled")).toBe("true"));
    expect(reasonOf(toggle)).toBe(reason);
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(reasonOf(screen.getByRole("combobox", { name: "Check" }))).toBe(reason);
    expect(reasonOf(screen.getByLabelText("At"))).toBe(reason);
    const reset = screen.getByRole("button", { name: "Reset Schedule to default" });
    expect(reasonOf(reset)).toBe(reason);
    fireEvent.click(reset);
    expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Daily");
    const save = screen.getByRole("button", { name: "Save update check settings" });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(save)).toBe(reason);
    fireEvent.click(save);
    expect(saves).toHaveLength(0);
  });
});
