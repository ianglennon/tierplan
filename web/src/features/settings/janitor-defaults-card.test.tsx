// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
let previews: string[];
// What the stubbed server answers instead of the usual, when a test needs it to.
let previewAnswer: () => Response;
let saveAnswer: (() => Response) | undefined;
let environments: { id: string }[];
let schedulePreviews: { envId: string; schedule: string }[];
let client: QueryClient;

const problem = (type: string, status: number, errors: { location: string; message: string }[]) =>
  new Response(JSON.stringify({ type, title: "Error", status, errors }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ instanceAdmin: true });
  held = janitorDefaults();
  saves = [];
  previews = [];
  previewAnswer = () => Response.json({ runs: 12, environments: 3 });
  saveAnswer = undefined;
  environments = [];
  schedulePreviews = [];
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") return Response.json({ items: environments });
      if (url.pathname === "/api/settings/janitor/retention-preview") {
        previews.push(url.search);
        return previewAnswer();
      }
      const previewOf = /^\/api\/env\/([^/]+)\/janitor\/schedule-preview$/.exec(url.pathname);
      if (previewOf) {
        const body = (await req.json()) as { schedule: string };
        schedulePreviews.push({ envId: previewOf[1] ?? "", schedule: body.schedule });
        return Response.json({
          valid: true,
          problem: "",
          nextRuns: ["2026-11-04T03:07:12Z"],
          timezone: "Europe/London",
        });
      }
      if (url.pathname === "/api/settings/janitor" && req.method === "GET") {
        return Response.json(held);
      }
      if (url.pathname === "/api/settings/janitor" && req.method === "PUT") {
        const body = (await req.json()) as Record<string, unknown>;
        saves.push(body);
        if (saveAnswer) return saveAnswer();
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

const save = () => screen.getByRole("button", { name: "Save janitor defaults" });
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
const field = async (name: string) =>
  (await screen.findByRole("spinbutton", { name })) as HTMLInputElement;
const reasonOf = (el: HTMLElement) =>
  document.getElementById(el.getAttribute("aria-describedby")?.split(" ").pop() ?? "")?.textContent;

describe("DefaultsRetentionConfirm", () => {
  it("asks before lowering the days removes runs, focused on Cancel, and saves nothing on Cancel", async () => {
    mount();
    type(await field("Keep run history for"), "30");
    fireEvent.click(save());

    const dialog = await screen.findByRole("alertdialog", { name: "Remove older run history?" });
    expect(previews).toEqual(["?days=30&runs=100"]);
    expect(
      within(dialog).getByText(
        "This removes 12 recorded runs across 3 environments, along with their crash evidence. This can't be undone.",
      ),
    ).toBeTruthy();
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    await waitFor(() => expect(document.activeElement).toBe(cancel));
    expect(within(dialog).getByRole("button", { name: "Remove 12 runs" })).toBeTruthy();

    fireEvent.click(cancel);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(saves).toHaveLength(0);
    // The edit is still there to change or save again.
    expect((await field("Keep run history for")).value).toBe("30");
  });

  it("saves the lowered retention when Remove is chosen", async () => {
    mount();
    type(await field("Keep at least the last"), "40");
    fireEvent.click(save());
    const dialog = await screen.findByRole("alertdialog", { name: "Remove older run history?" });
    expect(previews).toEqual(["?days=90&runs=40"]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove 12 runs" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ version: 4, retentionDays: 90, retentionRuns: 40 });
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Saved the janitor defaults."));
  });

  it("says 1 run and 1 environment in the singular", async () => {
    previewAnswer = () => Response.json({ runs: 1, environments: 1 });
    mount();
    type(await field("Keep run history for"), "30");
    fireEvent.click(save());
    const dialog = await screen.findByRole("alertdialog", { name: "Remove older run history?" });
    expect(
      within(dialog).getByText(/^This removes 1 recorded run across 1 environment, along/),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Remove 1 run" })).toBeTruthy();
  });

  it("saves with no dialog when the lowered retention would remove no runs", async () => {
    previewAnswer = () => Response.json({ runs: 0, environments: 0 });
    mount();
    type(await field("Keep run history for"), "60");
    fireEvent.click(save());
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(previews).toHaveLength(1);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("saves a raised limit with no dialog and without asking the server to count", async () => {
    mount();
    type(await field("Keep run history for"), "180");
    type(await field("Keep at least the last"), "500");
    fireEvent.click(save());
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ retentionDays: 180, retentionRuns: 500 });
    expect(previews).toHaveLength(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("saves nothing, and says why, when the count cannot be read", async () => {
    previewAnswer = () => Response.json({}, { status: 500 });
    mount();
    type(await field("Keep run history for"), "30");
    fireEvent.click(save());
    expect(
      await screen.findByText(/^Couldn't check how much run history this would remove/),
    ).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(saves).toHaveLength(0);
  });
});

describe("DefaultsWideningConfirm", () => {
  it("asks, naming how many environments follow, before an opt-in is turned on", async () => {
    mount();
    fireEvent.click(await screen.findByRole("switch", { name: "Also remove unused networks" }));
    fireEvent.click(save());
    const dialog = await screen.findByRole("alertdialog", { name: "Change the janitor defaults?" });
    expect(
      within(dialog).getByText("These defaults apply to 3 environments that follow them."),
    ).toBeTruthy();
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    await waitFor(() => expect(document.activeElement).toBe(cancel));
    // A widening change with no run history to lose does not ask the server to count.
    expect(previews).toHaveLength(0);

    fireEvent.click(cancel);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(saves).toHaveLength(0);

    fireEvent.click(save());
    const again = await screen.findByRole("alertdialog", { name: "Change the janitor defaults?" });
    fireEvent.click(within(again).getByRole("button", { name: "Save janitor defaults" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ removeUnusedNetworks: true, version: 4 });
  });

  it.each([
    ["a lower number of containers to keep", () => field("Exited task containers to keep"), "1"],
    ["a shorter grace period", () => field("Grace period"), "12"],
  ])("asks before %s", async (_name, control, value) => {
    mount();
    type(await control(), value);
    fireEvent.click(save());
    expect(
      await screen.findByRole("alertdialog", { name: "Change the janitor defaults?" }),
    ).toBeTruthy();
    expect(saves).toHaveLength(0);
  });

  it("asks before the janitor is turned on by default", async () => {
    held = janitorDefaults({ enabled: false });
    mount();
    fireEvent.click(
      await screen.findByRole("switch", {
        name: "Run the janitor on environments that follow this default",
      }),
    );
    fireEvent.click(save());
    expect(
      await screen.findByRole("alertdialog", { name: "Change the janitor defaults?" }),
    ).toBeTruthy();
  });

  it("saves a change that narrows at once, with no dialog", async () => {
    held = janitorDefaults({ removeStoppedContainers: true, keepN: 2 });
    mount();
    fireEvent.click(
      await screen.findByRole("switch", { name: "Also remove stopped standalone containers" }),
    );
    type(await field("Grace period"), "48");
    fireEvent.click(save());
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ removeStoppedContainers: false, graceHours: 48 });
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("does not ask when no environment follows the defaults", async () => {
    held = janitorDefaults({ followingEnvironments: 0 });
    mount();
    fireEvent.click(await screen.findByRole("switch", { name: "Also remove unused networks" }));
    fireEvent.click(save());
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("asks the retention question first and then the defaults one when a save does both", async () => {
    mount();
    fireEvent.click(await screen.findByRole("switch", { name: "Also remove unused networks" }));
    type(await field("Keep run history for"), "30");
    fireEvent.click(save());
    const first = await screen.findByRole("alertdialog", { name: "Remove older run history?" });
    fireEvent.click(within(first).getByRole("button", { name: "Remove 12 runs" }));
    const second = await screen.findByRole("alertdialog", { name: "Change the janitor defaults?" });
    expect(saves).toHaveLength(0);
    fireEvent.click(within(second).getByRole("button", { name: "Save janitor defaults" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ removeUnusedNetworks: true, retentionDays: 30 });
  });
});

describe("DefaultsRanges", () => {
  it("names the range of each number and saves nothing outside it", async () => {
    mount();
    const days = await field("Keep run history for");
    const runs = await field("Keep at least the last");
    type(days, "6");
    expect(await screen.findByText("Use a whole number from 7 to 3650.")).toBeTruthy();
    expect(days.getAttribute("aria-invalid")).toBe("true");
    type(runs, "1001");
    expect(screen.getByText("Use a whole number from 10 to 1000.")).toBeTruthy();
    type(await field("Exited task containers to keep"), "21");
    expect(screen.getByText("Use a whole number from 0 to 20.")).toBeTruthy();
    type(await field("Grace period"), "0");
    expect(screen.getByText("Use a whole number of hours from 1 to 720.")).toBeTruthy();

    fireEvent.click(save());
    expect(saves).toHaveLength(0);
    expect(previews).toHaveLength(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();

    type(days, "3651");
    expect(screen.getByText("Use a whole number from 7 to 3650.")).toBeTruthy();
    type(days, "7");
    type(runs, "10");
    expect(screen.queryByText("Use a whole number from 7 to 3650.")).toBeNull();
    expect(screen.queryByText("Use a whole number from 10 to 1000.")).toBeNull();
    expect(days.getAttribute("aria-invalid")).toBeNull();
  });

  it("treats a cleared or fractional number as out of range", async () => {
    mount();
    const days = await field("Keep run history for");
    type(days, "");
    expect(await screen.findByText("Use a whole number from 7 to 3650.")).toBeTruthy();
    type(days, "30.5");
    expect(screen.getByText("Use a whole number from 7 to 3650.")).toBeTruthy();
  });

  it("asks for a time when Daily has none, once Save is pressed", async () => {
    mount();
    await field("Keep run history for");
    fireEvent.change(screen.getByLabelText("At"), { target: { value: "" } });
    expect(screen.queryByText("Pick a time of day.")).toBeNull();
    fireEvent.click(save());
    expect(await screen.findByText("Pick a time of day.")).toBeTruthy();
    expect(saves).toHaveLength(0);
  });
});

describe("DefaultsReadOnly", () => {
  const reason = "Only an instance admin can do this";

  beforeEach(() => {
    me = makeMe({ permissions: { global: ["settings.read"], environments: {} } });
  });

  it("shows every field read-only with the reason on each, and saves nothing", async () => {
    mount();
    const days = await field("Keep run history for");
    await waitFor(() => expect(days.getAttribute("aria-disabled")).toBe("true"));
    for (const name of [
      "Keep run history for",
      "Keep at least the last",
      "Exited task containers to keep",
      "Grace period",
    ]) {
      const input = await field(name);
      expect(input.readOnly).toBe(true);
      expect(reasonOf(input)).toBe(reason);
    }
    for (const name of [
      "Run the janitor on environments that follow this default",
      "Also remove unused networks",
      "Also remove stopped standalone containers",
    ]) {
      const toggle = screen.getByRole("switch", { name });
      expect(toggle.getAttribute("aria-disabled")).toBe("true");
      expect(reasonOf(toggle)).toBe(reason);
    }
    expect(reasonOf(screen.getByRole("combobox", { name: "Run" }))).toBe(reason);
    expect(reasonOf(screen.getByLabelText("At"))).toBe(reason);

    const toggle = screen.getByRole("switch", { name: "Also remove unused networks" });
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    type(days, "30");
    expect(days.value).toBe("90");

    expect(save().getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(save())).toBe(reason);
    fireEvent.click(save());
    expect(saves).toHaveLength(0);
    expect(previews).toHaveLength(0);
    expect(screen.queryByText("Unsaved changes")).toBeNull();
  });
});

describe("DefaultsSaveFailures", () => {
  it("words a 409 as who saved last, with Reload, which reads the defaults again", async () => {
    saveAnswer = () =>
      problem("settings_changed", 409, [{ location: "changedBy", message: "Sam" }]);
    mount();
    fireEvent.click(
      await screen.findByRole("switch", { name: "Also remove stopped standalone containers" }),
    );
    fireEvent.click(save());
    fireEvent.click(
      within(
        await screen.findByRole("alertdialog", { name: "Change the janitor defaults?" }),
      ).getByRole("button", { name: "Save janitor defaults" }),
    );
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Sam changed the janitor defaults while you were editing. Reload to see them.",
    );

    // Someone else saved keep 5; Reload shows it and drops the edit.
    held = janitorDefaults({ version: 5, keepN: 5 });
    fireEvent.click(within(alert).getByRole("button", { name: "Reload" }));
    await waitFor(() =>
      expect(
        (
          screen.getByRole("spinbutton", {
            name: "Exited task containers to keep",
          }) as HTMLInputElement
        ).value,
      ).toBe("5"),
    );
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText("Unsaved changes")).toBeNull();
  });

  it("words the conflict without a name when the server did not know who", async () => {
    saveAnswer = () => problem("settings_changed", 409, [{ location: "changedBy", message: "" }]);
    mount();
    type(await field("Keep run history for"), "180");
    fireEvent.click(save());
    expect(
      await screen.findByText(
        "The janitor defaults were changed while you were editing. Reload to see them.",
      ),
    ).toBeTruthy();
  });

  it("says it could not save, in an alert, and keeps the edit to try again", async () => {
    saveAnswer = () => Response.json({}, { status: 500 });
    mount();
    type(await field("Keep run history for"), "180");
    fireEvent.click(save());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Couldn't save the janitor defaults. Try again.");
    expect(toasts.success).not.toHaveBeenCalled();
    expect((await field("Keep run history for")).value).toBe("180");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
  });

  it("shows the server's refusal of a schedule under the schedule", async () => {
    saveAnswer = () => problem("schedule_too_frequent", 422, []);
    mount();
    await pick("Run", "Hourly");
    fireEvent.click(save());
    expect(await screen.findByText("Runs can be at most once every 15 minutes.")).toBeTruthy();
    expect(toasts.success).not.toHaveBeenCalled();
  });
});

describe("DefaultsLoading", () => {
  it("says so, with Retry, when the defaults cannot be read", async () => {
    let fail = true;
    const base = globalThis.fetch;
    vi.stubGlobal("fetch", async (req: Request) =>
      fail && new URL(req.url).pathname === "/api/settings/janitor"
        ? Response.json({}, { status: 500 })
        : base(req),
    );
    mount();
    expect(await screen.findByText("Couldn't load the janitor defaults.")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading defaults" }));
    expect(await field("Keep run history for")).toBeTruthy();
  });
});

describe("DefaultsCustomSchedule", () => {
  it("asks the first environment the person may read for the next runs of a custom expression", async () => {
    environments = [{ id: "e1" }, { id: "e2" }];
    held = janitorDefaults({ schedule: "*/20 * * * *", schedulePreset: "custom" });
    mount();
    expect(await screen.findByText(/^Next runs: /)).toBeTruthy();
    expect(schedulePreviews).toEqual([{ envId: "e1", schedule: "*/20 * * * *" }]);
  });

  it("asks nothing when there is no environment to ask about", async () => {
    held = janitorDefaults({ schedule: "*/20 * * * *", schedulePreset: "custom" });
    mount();
    expect(await screen.findByLabelText("Cron expression")).toBeTruthy();
    expect(schedulePreviews).toEqual([]);
  });
});
