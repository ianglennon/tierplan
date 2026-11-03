// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { janitorPolicyKey } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { PolicyCard } from "@/features/janitor/policy-card";
import "@/i18n";
import { janitorPolicy, preview, previewItem } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

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

// What the policy reads like after a save: each field the save set is the environment's own, each it
// sent as null follows the default, and the version moves on.
function applySave(
  from: ReturnType<typeof janitorPolicy>,
  body: Record<string, unknown>,
): ReturnType<typeof janitorPolicy> {
  const d = from.defaults;
  const field = <T,>(sent: unknown, fallback: T) =>
    sent === null || sent === undefined
      ? { value: fallback, overridden: false }
      : { value: sent as T, overridden: true };
  const schedule = field(body.schedule, d.schedule);
  return {
    ...from,
    version: from.version + 1,
    enabled: field(body.enabled, d.enabled),
    schedule: {
      ...schedule,
      preset: schedule.overridden
        ? (body.schedulePreset as typeof d.schedulePreset)
        : d.schedulePreset,
    },
    keepN: field(body.keepN, d.keepN),
    graceHours: field(body.graceHours, d.graceHours),
    removeUnusedNetworks: field(body.removeUnusedNetworks, d.removeUnusedNetworks),
    removeStoppedContainers: field(body.removeStoppedContainers, d.removeStoppedContainers),
  };
}

let me: ReturnType<typeof makeMe>;
let policyBody: ReturnType<typeof janitorPolicy>;
let policyReads: number;
let saves: Record<string, unknown>[];
let previews: (Record<string, unknown> | null)[];
let previewBody: ReturnType<typeof preview>;
// What the stubbed server answers to a save instead of the usual, when a test needs it to.
let saveAnswer: (() => Response) | undefined;
let readAnswer: (() => Response) | undefined;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  policyBody = janitorPolicy();
  policyReads = 0;
  saves = [];
  previews = [];
  saveAnswer = undefined;
  readAnswer = undefined;
  previewBody = preview({
    items: [
      previewItem({ name: "web.1.aaa", ref: "c1" }),
      previewItem({ name: "web.2.bbb", ref: "c2" }),
    ],
  });
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/janitor/policy` && req.method === "GET") {
        policyReads++;
        if (readAnswer) return readAnswer();
        return Response.json(policyBody);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/policy` && req.method === "PUT") {
        saves.push((await req.json()) as Record<string, unknown>);
        if (saveAnswer) return saveAnswer();
        policyBody = applySave(policyBody, saves[saves.length - 1] ?? {});
        return Response.json(policyBody);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/preview`) {
        const text = await req.text();
        previews.push(text ? (JSON.parse(text) as Record<string, unknown>) : null);
        return Response.json(previewBody);
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

function mount(props: { swarm?: boolean; firstRunDone?: boolean } = {}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <TestRouter>
          <PolicyCard
            envId={ENV}
            envName="Prod"
            swarm={props.swarm ?? true}
            nodeCount={2}
            firstRunDone={props.firstRunDone}
          />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const keepField = () => screen.findByRole("spinbutton", { name: "Exited task containers to keep" });
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });

describe("PolicyLowerKeepReview", () => {
  it("reads Review changes for a lower keep-N, previews the edited policy and saves it with the version", async () => {
    mount();
    const keep = (await keepField()) as HTMLInputElement;
    expect(keep.value).toBe("2");
    expect(screen.queryByText("Unsaved changes")).toBeNull();

    type(keep, "0");
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();
    const review = screen.getByRole("button", { name: "Review changes" });
    fireEvent.click(review);

    // The sheet asks for the preview of the edited policy: keep 0, everything else following the default.
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    await waitFor(() => expect(previews).toHaveLength(1));
    expect(previews[0]).toEqual({
      policy: {
        enabled: null,
        schedule: null,
        schedulePreset: null,
        keepN: 0,
        graceHours: null,
        removeUnusedNetworks: null,
        removeStoppedContainers: null,
      },
    });
    expect(await within(sheet).findAllByText(/^Would remove 2 containers/)).not.toHaveLength(0);
    expect(saves).toHaveLength(0);

    fireEvent.click(within(sheet).getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual({
      version: 3,
      enabled: null,
      schedule: null,
      schedulePreset: null,
      keepN: 0,
      graceHours: null,
      removeUnusedNetworks: null,
      removeStoppedContainers: null,
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(toasts.success).toHaveBeenCalledWith("Saved the janitor policy.");
    // Saved: the field is its own now, and nothing is unsaved.
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
    expect(await screen.findByText("Overridden")).toBeTruthy();
  });

  it("saves at once, with no preview, when the edit cannot remove more", async () => {
    mount();
    type(await keepField(), "5");
    const save = await screen.findByRole("button", { name: "Save policy" });
    expect(screen.queryByRole("button", { name: "Review changes" })).toBeNull();
    fireEvent.click(save);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]?.keepN).toBe(5);
    expect(previews).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(toasts.success).toHaveBeenCalledWith("Saved the janitor policy.");
  });

  it("does nothing when Save policy is pressed with nothing changed", async () => {
    mount();
    await keepField();
    const save = screen.getByRole("button", { name: "Save policy" });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(save);
    expect(saves).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("PolicyAnchorAndRules", () => {
  it("is the #policy section and carries the rules note with the real keep and grace", async () => {
    policyBody = janitorPolicy({
      keepN: { value: 5, overridden: true },
      graceHours: { value: 48, overridden: true },
    });
    mount({ firstRunDone: false });
    const section = await screen.findByRole("region", { name: "Policy" });
    expect(section.id).toBe("policy");
    expect(
      await within(section).findByText(
        "Exited task containers beyond the newest 5 per service slot.",
      ),
    ).toBeTruthy();
    expect(within(section).getByText(/older than 48 hours/)).toBeTruthy();
  });

  it("leaves the rules note out until the page knows whether the janitor has run", async () => {
    mount();
    await keepField();
    expect(
      screen.queryByRole("button", { name: "What the janitor removes and never touches" }),
    ).toBeNull();
  });
});

describe("PolicyScheduleEdits", () => {
  async function pickRun(option: string) {
    fireEvent.keyDown(await screen.findByRole("combobox", { name: "Run" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: option }));
  }

  it("keeps the button as Save policy for a schedule-only change and saves the preset with its cron", async () => {
    mount();
    await pickRun("Weekly");
    const save = await screen.findByRole("button", { name: "Save policy" });
    expect(screen.queryByRole("button", { name: "Review changes" })).toBeNull();
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(save);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({
      version: 3,
      schedule: "0 3 * * 1",
      schedulePreset: "weekly",
      keepN: null,
    });
    // A schedule change alone never opens the sheet.
    expect(previews).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("is no change at all to pick the schedule it already has", async () => {
    mount();
    await pickRun("Weekly");
    await pickRun("Daily");
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
  });
});

const reasonOf = (el: HTMLElement) =>
  document.getElementById(el.getAttribute("aria-describedby")?.split(" ").pop() ?? "")?.textContent;
const alertText = () => screen.getAllByRole("alert").map((a) => a.textContent);

describe("PolicyRangeErrors", () => {
  it("names the range of keep-N and refuses to save a number outside it", async () => {
    mount();
    const keep = await keepField();
    type(keep, "21");
    expect(await screen.findByText("Use a whole number from 0 to 20.")).toBeTruthy();
    expect(keep.getAttribute("aria-invalid")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    expect(saves).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();

    type(keep, "");
    expect(screen.getByText("Use a whole number from 0 to 20.")).toBeTruthy();
    type(keep, "1.5");
    expect(screen.getByText("Use a whole number from 0 to 20.")).toBeTruthy();
    type(keep, "20");
    expect(screen.queryByText("Use a whole number from 0 to 20.")).toBeNull();
    expect(keep.getAttribute("aria-invalid")).toBeNull();
  });

  it("names the range of the grace period in hours", async () => {
    mount();
    await keepField();
    const grace = screen.getByRole("spinbutton", { name: "Grace period" });
    type(grace, "0");
    expect(await screen.findByText("Use a whole number of hours from 1 to 720.")).toBeTruthy();
    type(grace, "721");
    expect(screen.getByText("Use a whole number of hours from 1 to 720.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    expect(saves).toHaveLength(0);
    type(grace, "720");
    expect(screen.queryByText("Use a whole number of hours from 1 to 720.")).toBeNull();
  });

  it("asks for a time when Daily has none, once Save is pressed", async () => {
    mount();
    await keepField();
    fireEvent.change(screen.getByLabelText("At"), { target: { value: "" } });
    expect(screen.queryByText("Pick a time of day.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    expect((await screen.findByText("Pick a time of day.")).getAttribute("role")).toBe("status");
    expect(saves).toHaveLength(0);
  });
});

describe("PolicyConflictAndFailures", () => {
  it("names who changed the policy, and Reload reads it again and drops the edits", async () => {
    saveAnswer = () =>
      problem("settings_changed", 409, [{ location: "changedBy", message: "Ada" }]);
    mount();
    const keep = await keepField();
    type(keep, "5");
    fireEvent.click(await screen.findByRole("button", { name: "Save policy" }));
    const line = await screen.findByText(
      "Ada changed this policy while you were editing. Reload to see it.",
    );
    expect(line.closest('[role="alert"]')).toBeTruthy();
    expect(toasts.success).not.toHaveBeenCalled();
    // The edit is still there to copy from.
    expect((keep as HTMLInputElement).value).toBe("5");

    const reads = policyReads;
    policyBody = janitorPolicy({ version: 4, keepN: { value: 3, overridden: true } });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(policyReads).toBeGreaterThan(reads));
    await waitFor(() => expect((keep as HTMLInputElement).value).toBe("3"));
    expect(screen.queryByText(/changed this policy while you were editing/)).toBeNull();
    expect(screen.queryByText("Unsaved changes")).toBeNull();
  });

  it("words the conflict without a name when the server did not know who", async () => {
    saveAnswer = () => problem("settings_changed", 409, [{ location: "changedBy", message: "" }]);
    mount();
    type(await keepField(), "5");
    fireEvent.click(await screen.findByRole("button", { name: "Save policy" }));
    expect(
      await screen.findByText("This policy was changed while you were editing. Reload to see it."),
    ).toBeTruthy();
  });

  it("sends the version the edit began from, even when the policy is read again meanwhile", async () => {
    mount();
    type(await keepField(), "5");
    policyBody = janitorPolicy({ version: 9 });
    await client.invalidateQueries({ queryKey: janitorPolicyKey(ENV) });
    await waitFor(() => expect(policyReads).toBeGreaterThan(1));
    fireEvent.click(await screen.findByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]?.version).toBe(3);
  });

  it("says it couldn't save and keeps the edit when the save fails", async () => {
    saveAnswer = () => problem("internal", 500, []);
    mount();
    const keep = await keepField();
    type(keep, "5");
    fireEvent.click(await screen.findByRole("button", { name: "Save policy" }));
    await waitFor(() =>
      expect(alertText()).toContain("Couldn't save the janitor policy. Try again."),
    );
    expect((keep as HTMLInputElement).value).toBe("5");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("shows the server's answer for a schedule it refuses on the schedule", async () => {
    saveAnswer = () => problem("invalid_schedule", 422, []);
    mount();
    fireEvent.change(await screen.findByLabelText("At"), { target: { value: "05:00" } });
    fireEvent.click(await screen.findByRole("button", { name: "Save policy" }));
    expect(
      await screen.findByText(
        "Enter a cron expression like 0 3 * * * (minute hour day month weekday).",
      ),
    ).toBeTruthy();
    cleanup();

    saveAnswer = () => problem("schedule_too_frequent", 422, []);
    mount();
    fireEvent.change(await screen.findByLabelText("At"), { target: { value: "05:00" } });
    fireEvent.click(await screen.findByRole("button", { name: "Save policy" }));
    expect(await screen.findByText("Runs can be at most once every 15 minutes.")).toBeTruthy();
  });

  it("closes the sheet on a refused save and words the refusal on the card", async () => {
    saveAnswer = () =>
      problem("settings_changed", 409, [{ location: "changedBy", message: "Grace" }]);
    mount();
    type(await keepField(), "0");
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    const save = await within(sheet).findByRole("button", { name: "Save policy" });
    await waitFor(() => expect(save.hasAttribute("disabled")).toBe(false));
    fireEvent.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(
      await screen.findByText(
        "Grace changed this policy while you were editing. Reload to see it.",
      ),
    ).toBeTruthy();
  });
});

describe("PolicyReset", () => {
  it("returns an overridden field to the default, moves focus to its control and saves null", async () => {
    policyBody = janitorPolicy({ keepN: { value: 1, overridden: true } });
    mount();
    const keep = (await keepField()) as HTMLInputElement;
    expect(keep.value).toBe("1");
    expect(screen.getByText("Overridden")).toBeTruthy();
    fireEvent.click(
      screen.getByRole("button", { name: "Reset Exited task containers to keep to default" }),
    );
    await waitFor(() => expect(document.activeElement).toBe(keep));
    expect(keep.value).toBe("2");
    expect(screen.queryByText("Overridden")).toBeNull();
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    // Keeping more cannot remove more, so it saves without a preview.
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toMatchObject({ version: 3, keepN: null });
    expect(previews).toHaveLength(0);
  });

  it("moves focus to the switch after a reset", async () => {
    policyBody = janitorPolicy({ enabled: { value: false, overridden: true } });
    mount();
    const toggle = await screen.findByRole("switch", {
      name: "Run the janitor on this environment",
    });
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: "Reset Janitor to default" }));
    await waitFor(() => expect(document.activeElement).toBe(toggle));
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    // Turning the janitor back on could remove more, so it is reviewed first.
    expect(screen.getByRole("button", { name: "Review changes" })).toBeTruthy();
  });

  it("lets the opt-ins be turned on, and reviewed, and be reset", async () => {
    mount();
    const networks = await screen.findByRole("switch", { name: "Also remove unused networks" });
    expect(networks.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(networks);
    expect(networks.getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("button", { name: "Review changes" })).toBeTruthy();
    fireEvent.click(
      screen.getByRole("button", { name: "Reset Also remove unused networks to default" }),
    );
    expect(networks.getAttribute("aria-checked")).toBe("false");
    // Back to what was saved: nothing is unsaved.
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
    expect(
      screen.getByRole("switch", { name: "Also remove stopped standalone containers" }),
    ).toBeTruthy();
  });
});

describe("PolicyInheritance", () => {
  it("shows exactly one of Default and Overridden for each of the six fields", async () => {
    policyBody = janitorPolicy({
      keepN: { value: 1, overridden: true },
      removeUnusedNetworks: { value: true, overridden: true },
    });
    mount();
    await keepField();
    expect(screen.getAllByText("Overridden")).toHaveLength(2);
    expect(screen.getAllByText("Default")).toHaveLength(4);
    expect(screen.getAllByRole("button", { name: /^Reset .* to default$/ })).toHaveLength(2);
  });

  it("follows a changed default after a reload for a Default field and not for an Overridden one", async () => {
    policyBody = janitorPolicy({ graceHours: { value: 12, overridden: true } });
    mount();
    const keep = (await keepField()) as HTMLInputElement;
    const grace = screen.getByRole("spinbutton", { name: "Grace period" }) as HTMLInputElement;
    expect([keep.value, grace.value]).toEqual(["2", "12"]);

    policyBody = janitorPolicy({
      keepN: { value: 4, overridden: false },
      graceHours: { value: 12, overridden: true },
      defaults: { ...janitorPolicy().defaults, keepN: 4, graceHours: 72 },
    });
    await client.invalidateQueries({ queryKey: janitorPolicyKey(ENV) });
    await waitFor(() => expect(keep.value).toBe("4"));
    expect(grace.value).toBe("12");
    // The helper quotes the default that applies now.
    expect(screen.getByText(/Default 4\./)).toBeTruthy();
    expect(screen.getByText(/Default 3 days|Default 72 hours/)).toBeTruthy();
  });

  it("calls the row for exited containers on a standalone host by its own name", async () => {
    mount({ swarm: false });
    expect(
      await screen.findByRole("spinbutton", { name: "Exited containers to keep" }),
    ).toBeTruthy();
    expect(screen.queryByRole("spinbutton", { name: "Exited task containers to keep" })).toBeNull();
  });
});

describe("PolicyReadOnly", () => {
  const reason = "Needs the janitor policy permission on Prod";

  beforeEach(() => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    policyBody = janitorPolicy({ keepN: { value: 1, overridden: true } });
  });

  it("shows every field read-only with the reason on each, and changes nothing", async () => {
    mount();
    const keep = (await keepField()) as HTMLInputElement;
    await waitFor(() => expect(keep.getAttribute("aria-disabled")).toBe("true"));
    expect(keep.readOnly).toBe(true);
    expect(reasonOf(keep)).toBe(reason);

    const toggle = screen.getByRole("switch", { name: "Run the janitor on this environment" });
    expect(toggle.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(toggle)).toBe(reason);
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("true");

    const grace = screen.getByRole("spinbutton", { name: "Grace period" });
    expect(reasonOf(grace)).toBe(reason);
    expect(reasonOf(screen.getByRole("combobox", { name: "Run" }))).toBe(reason);
    expect(reasonOf(screen.getByLabelText("At"))).toBe(reason);

    const reset = screen.getByRole("button", {
      name: "Reset Exited task containers to keep to default",
    });
    expect(reasonOf(reset)).toBe(reason);
    fireEvent.click(reset);
    expect(keep.value).toBe("1");

    const save = screen.getByRole("button", { name: "Save policy" });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(save)).toBe(reason);
    fireEvent.click(save);
    expect(saves).toHaveLength(0);
    expect(screen.queryByText("Unsaved changes")).toBeNull();
  });

  it("reads the same for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const keep = await keepField();
    await waitFor(() => expect(keep.getAttribute("aria-disabled")).toBe("true"));
    expect(reasonOf(keep)).toBe(reason);
  });

  it("leaves an Admin, who holds the extra, free to edit", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    mount();
    const keep = await keepField();
    await waitFor(() => expect(keep.getAttribute("aria-disabled")).toBeNull());
    expect((keep as HTMLInputElement).readOnly).toBe(false);
  });
});

describe("PolicyLoading", () => {
  it("says it couldn't load the policy and reads again on request", async () => {
    readAnswer = () => problem("internal", 500, []);
    mount();
    expect(await screen.findByText("Couldn't load the janitor policy.")).toBeTruthy();
    expect(screen.queryByRole("spinbutton")).toBeNull();
    readAnswer = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading policy" }));
    expect(await keepField()).toBeTruthy();
    expect(screen.queryByText("Couldn't load the janitor policy.")).toBeNull();
  });

  it("keeps the rules note while the policy cannot be read", async () => {
    readAnswer = () => problem("internal", 500, []);
    mount({ firstRunDone: false });
    expect(await screen.findByText("Couldn't load the janitor policy.")).toBeTruthy();
    expect(
      screen.getByText("Exited task containers beyond the newest 2 per service slot."),
    ).toBeTruthy();
  });
});

describe("PolicyLeaveGuard", () => {
  function mountRouted() {
    const root = createRootRoute();
    const router = createRouter({
      routeTree: root.addChildren([
        createRoute({
          getParentRoute: () => root,
          path: "/policy",
          component: () => <PolicyCard envId={ENV} envName="Prod" swarm nodeCount={2} />,
        }),
        createRoute({
          getParentRoute: () => root,
          path: "/registries",
          component: () => <p>Elsewhere</p>,
        }),
      ]),
      history: createMemoryHistory({ initialEntries: ["/policy"] }),
    });
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <RouterProvider router={router} />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    return router;
  }

  it("asks before leaving with unsaved edits, and Keep editing stays", async () => {
    const router = mountRouted();
    type(await keepField(), "5");
    void router.navigate({ to: "/registries" });
    const dialog = await screen.findByRole("alertdialog", { name: "Discard your policy changes?" });
    expect(within(dialog).getByText("You haven't saved them.")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(router.state.location.pathname).toBe("/policy");
    expect((await keepField()) as HTMLInputElement).toHaveProperty("value", "5");
  });

  it("leaves when the edits are discarded", async () => {
    const router = mountRouted();
    type(await keepField(), "5");
    void router.navigate({ to: "/registries" });
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/registries"));
  });

  it("leaves without asking when nothing is unsaved", async () => {
    const router = mountRouted();
    await keepField();
    void router.navigate({ to: "/registries" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/registries"));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
