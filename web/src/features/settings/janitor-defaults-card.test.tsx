// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { JanitorDefaultsCard } from "@/features/settings/janitor-defaults-card";
import "@/i18n";
import { janitorDefaults } from "@/test-janitor";
import { makeMe } from "@/test-me";

// The "Janitor defaults" card on Settings > General (plan 11-24, JAN-01, JAN-10, D-04, D-16, D-21).

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

let me: ReturnType<typeof makeMe>;
let held: ReturnType<typeof janitorDefaults>;
let saves: Record<string, unknown>[];
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ instanceAdmin: true });
  held = janitorDefaults();
  saves = [];
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") return Response.json({ items: [] });
      if (url.pathname === "/api/settings/janitor" && req.method === "GET") {
        return Response.json(held);
      }
      if (url.pathname === "/api/settings/janitor" && req.method === "PUT") {
        const body = (await req.json()) as Record<string, unknown>;
        saves.push(body);
        held = { ...held, ...body, version: held.version + 1 } as typeof held;
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
        <JanitorDefaultsCard />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

async function pick(name: string, option: string) {
  fireEvent.keyDown(await screen.findByRole("combobox", { name }), { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("DefaultsChangeSchedule", () => {
  it("changes Daily 03:00 to Weekly Monday 04:00 and saves it with the cron, the preset and the version", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Janitor defaults" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Janitor defaults" }).id).toBe("janitor-defaults");
    expect((await screen.findByRole("combobox", { name: "Run" })).textContent).toBe("Daily");
    expect((screen.getByLabelText("At") as HTMLInputElement).value).toBe("03:00");
    expect(screen.queryByText("Unsaved changes")).toBeNull();

    await pick("Run", "Weekly");
    fireEvent.change(screen.getByLabelText("At"), { target: { value: "04:00" } });
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();
    // Monday is where Weekly starts.
    expect(screen.getByRole("combobox", { name: "On" }).textContent).toBe("Monday");

    fireEvent.click(screen.getByRole("button", { name: "Save janitor defaults" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual({
      version: 4,
      enabled: true,
      schedule: "0 4 * * 1",
      schedulePreset: "weekly",
      keepN: 2,
      graceHours: 24,
      removeUnusedNetworks: false,
      removeStoppedContainers: false,
      retentionDays: 90,
      retentionRuns: 100,
    });
    // A schedule change widens nothing, so no confirm stood in the way.
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Saved the janitor defaults."));
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
    expect(screen.getByRole("combobox", { name: "Run" }).textContent).toBe("Weekly");
  });
});
