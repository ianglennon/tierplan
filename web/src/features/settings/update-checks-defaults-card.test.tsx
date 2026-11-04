// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateChecksDefaultsCard } from "@/features/settings/update-checks-defaults-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { updateCheckDefaults } from "@/test-update-checks";

// The "Image update check defaults" card on Settings > General (plan 12-12, IMG-04, D-01, D-22).

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

let me: ReturnType<typeof makeMe>;
let held: ReturnType<typeof updateCheckDefaults>;
let saves: Record<string, unknown>[];
let previews: { envId: string; schedule: string }[];
// What the stubbed server answers to a save instead of the usual, when a test needs it to.
let saveAnswer: (() => Response) | undefined;
let client: QueryClient;

const problem = (type: string, status: number, errors: { location: string; message: string }[]) =>
  new Response(JSON.stringify({ type, title: "Error", status, errors }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ instanceAdmin: true });
  held = updateCheckDefaults();
  saves = [];
  previews = [];
  saveAnswer = undefined;
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") return Response.json({ items: [{ id: "e1" }] });
      const previewOf = /^\/api\/env\/([^/]+)\/updates\/schedule-preview$/.exec(url.pathname);
      if (previewOf) {
        const body = (await req.json()) as { schedule: string };
        previews.push({ envId: previewOf[1] ?? "", schedule: body.schedule });
        return Response.json({
          valid: true,
          problem: "",
          nextRuns: ["2026-11-05T03:07:00Z"],
          timezone: "Europe/London",
        });
      }
      if (url.pathname === "/api/settings/update-checks" && req.method === "GET") {
        return Response.json(held);
      }
      if (url.pathname === "/api/settings/update-checks" && req.method === "PUT") {
        const body = (await req.json()) as Record<string, unknown>;
        saves.push(body);
        if (saveAnswer) return saveAnswer();
        held = {
          ...held,
          version: held.version + 1,
          enabled: body.enabled as boolean,
          schedule: body.schedule as string,
          preset: body.preset as typeof held.preset,
        };
        return Response.json(held);
      }
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
        <UpdateChecksDefaultsCard />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

async function pickCheck(option: string) {
  fireEvent.keyDown(await screen.findByRole("combobox", { name: "Check" }), { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

const reasonOf = (el: HTMLElement) =>
  document.getElementById(el.getAttribute("aria-describedby")?.split(" ").pop() ?? "")?.textContent;
const SAVE = "Save update check defaults";

describe("UpdateChecksDefaultsEdit", () => {
  it("shows the defaults with the anchor and no Default or Overridden state", async () => {
    mount();
    expect(await screen.findByText("Image update check defaults")).toBeTruthy();
    expect(document.getElementById("update-checks")).not.toBeNull();
    expect(
      (
        await screen.findByRole("switch", {
          name: "Check environments that follow this default for image updates",
        })
      ).getAttribute("aria-checked"),
    ).toBe("true");
    expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Every 6 hours");
    expect(screen.queryByText("Default")).toBeNull();
    expect(screen.queryByText("Overridden")).toBeNull();
  });

  it("saves the schedule with its preset and the version it read, then says so", async () => {
    mount();
    await pickCheck("Daily");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual({
      version: 2,
      enabled: true,
      schedule: "0 3 * * *",
      preset: "daily",
    });
    await waitFor(() =>
      expect(toasts.success).toHaveBeenCalledWith("Saved the update check defaults."),
    );
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
    expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Daily");
  });

  it("turns checks off for environments that follow the default", async () => {
    mount();
    const toggle = await screen.findByRole("switch", {
      name: "Check environments that follow this default for image updates",
    });
    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ enabled: false, version: 2 });
  });

  it("asks the first readable environment for the next checks of a custom expression", async () => {
    mount();
    await pickCheck("Custom (cron)");
    const cron = await screen.findByLabelText("Cron expression");
    fireEvent.change(cron, { target: { value: "0 3 * * *" } });
    const line = await screen.findByText(/^Next checks: /);
    expect(line.getAttribute("role")).toBe("status");
    expect(previews.at(-1)).toEqual({ envId: "e1", schedule: "0 3 * * *" });
  });
});

describe("UpdateChecksDefaultsRefusals", () => {
  it("names the one-hour floor when the server refuses a schedule as too frequent", async () => {
    saveAnswer = () =>
      problem("schedule_too_frequent", 422, [{ location: "body.schedule", message: "too often" }]);
    mount();
    await pickCheck("Hourly");
    fireEvent.click(screen.getByRole("button", { name: SAVE }));
    expect(await screen.findByText("Checks can be at most once an hour.")).toBeTruthy();
  });

  it("shows the conflict with who changed it and reloads the defaults on Reload", async () => {
    saveAnswer = () =>
      problem("settings_changed", 409, [{ location: "changedBy", message: "Ada" }]);
    mount();
    await pickCheck("Weekly");
    fireEvent.click(screen.getByRole("button", { name: SAVE }));
    const alert = await screen.findByText(
      "Ada changed the update check defaults while you were editing. Reload to see them.",
    );
    expect(alert).toBeTruthy();
    // Someone else's save is what Reload shows.
    held = updateCheckDefaults({ version: 3, preset: "hourly", schedule: "0 * * * *" });
    saveAnswer = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Check" }).textContent).toContain("Hourly"),
    );
    expect(screen.queryByText(/while you were editing/)).toBeNull();
    // The next save carries the version it was reloaded to.
    await pickCheck("Daily");
    fireEvent.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(saves.at(-1)).toMatchObject({ version: 3 }));
  });

  it("says the save failed in an inline alert", async () => {
    saveAnswer = () => problem("internal_error", 500, []);
    mount();
    await pickCheck("Weekly");
    fireEvent.click(screen.getByRole("button", { name: SAVE }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Couldn't save the update check defaults. Try again.");
    expect(toasts.success).not.toHaveBeenCalled();
  });
});

describe("UpdateChecksDefaultsReadOnly", () => {
  beforeEach(() => {
    me = makeMe({ permissions: { global: ["settings.read"], environments: {} } });
  });

  it("shows the fields read-only with the reason, and sends nothing", async () => {
    mount();
    const toggle = await screen.findByRole("switch", {
      name: "Check environments that follow this default for image updates",
    });
    await waitFor(() => expect(toggle.getAttribute("aria-disabled")).toBe("true"));
    const reason = reasonOf(toggle);
    expect(reason).toBeTruthy();
    expect(reasonOf(screen.getByRole("combobox", { name: "Check" }))).toBe(reason);
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    const save = screen.getByRole("button", { name: SAVE });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(save);
    expect(saves).toHaveLength(0);
  });
});
