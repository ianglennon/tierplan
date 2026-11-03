// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
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
let saves: Record<string, unknown>[];
let previews: (Record<string, unknown> | null)[];
let previewBody: ReturnType<typeof preview>;
// What the stubbed server answers to a save instead of the usual, when a test needs it to.
let saveAnswer: (() => Response) | undefined;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  policyBody = janitorPolicy();
  saves = [];
  previews = [];
  saveAnswer = undefined;
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
