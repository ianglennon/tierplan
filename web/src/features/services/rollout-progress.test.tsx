// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  isRolloutRunning,
  ROLLOUT_REFETCH_MS,
  serviceRetry,
  serviceRolloutQuery,
} from "@/api/services";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RolloutProgress, rolloutTiming } from "@/features/services/rollout-progress";
import "@/i18n";
import { makeMe } from "@/test-me";
import { rollout, SERVICE_ENV, SERVICE_ID, type ServiceRolloutBody } from "@/test-service";

// The inline region of a running, paused or rolled-back update (plan 06-09, RES-02, D-03, D-12,
// UI-SPEC "Rollout progress").

let answer: ServiceRolloutBody;
let role: "viewer" | "operator";
let client: QueryClient;
const onRollback = vi.fn();
const onRetry = vi.fn();

beforeEach(() => {
  answer = rollout();
  role = "operator";
  onRollback.mockReset();
  onRetry.mockReset();
  rolloutTiming.completeMs = 40;
  serviceRetry.attempts = 0;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [SERVICE_ENV]: role } }));
      if (path === `/api/env/${SERVICE_ENV}/services/${SERVICE_ID}/rollout`) {
        return Response.json(answer);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  rolloutTiming.completeMs = 4000;
  serviceRetry.attempts = 1;
});

function mount() {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RolloutProgress
          envId={SERVICE_ENV}
          serviceId={SERVICE_ID}
          name="api"
          onRollback={onRollback}
          onRetry={onRetry}
        />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

// shown is how many times text is drawn on the page, not counting the screen reader's live
// region, which repeats a change in the same words.
function shown(text: string): number {
  return screen
    .queryAllByText(text)
    .filter((el) => !el.closest('[data-testid="rollout-announcement"]')).length;
}

// next makes the server answer with body from now on and reads the rollout again.
async function next(body: ServiceRolloutBody) {
  answer = body;
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["env", SERVICE_ENV] });
  });
}

describe("RolloutProgress while the swarm replaces tasks", () => {
  it("says how many tasks are on the new version, with a progress bar", async () => {
    answer = rollout({ state: "updating", message: "update in progress", updated: 1, total: 4 });
    mount();
    expect(await screen.findByText("Updating api")).toBeTruthy();
    expect(screen.getByText("1 of 4 tasks are on the new version.")).toBeTruthy();
    const bar = screen.getByRole("progressbar", { name: "Update progress" });
    expect(bar.getAttribute("aria-valuenow")).toBe("25");
    expect(screen.getByText("update in progress")).toBeTruthy();
  });

  it("words a rollback as tasks being back on the earlier version", async () => {
    answer = rollout({ state: "rollback_started", updated: 3, total: 4 });
    mount();
    expect(await screen.findByText("Rolling back api")).toBeTruthy();
    expect(screen.getByText("3 of 4 tasks are back on the earlier version.")).toBeTruthy();
    expect(
      screen.getByRole("progressbar", { name: "Update progress" }).getAttribute("aria-valuenow"),
    ).toBe("75");
  });

  it("does not divide by zero when the swarm wants no task running", async () => {
    answer = rollout({ state: "updating", updated: 0, total: 0 });
    mount();
    const bar = await screen.findByRole("progressbar", { name: "Update progress" });
    expect(bar.getAttribute("aria-valuenow")).toBe("0");
  });

  it("follows the rollout as it is read again and shows Update complete, then collapses", async () => {
    answer = rollout({ state: "updating", updated: 3, total: 4 });
    mount();
    await screen.findByText("Updating api");
    await next(rollout({ state: "completed", updated: 4, total: 4 }));
    await waitFor(() => expect(shown("Update complete")).toBe(1));
    expect(screen.queryByRole("progressbar")).toBeNull();
    await waitFor(() => expect(shown("Update complete")).toBe(0));
    expect(screen.queryByText("Updating api")).toBeNull();
  });

  it("announces the change to complete once, politely", async () => {
    answer = rollout({ state: "updating", updated: 3, total: 4 });
    mount();
    await screen.findByText("Updating api");
    const live = screen.getByTestId("rollout-announcement");
    expect(live.getAttribute("role")).toBe("status");
    expect(live.textContent).toBe("");
    await next(rollout({ state: "completed", updated: 4, total: 4 }));
    await waitFor(() => expect(live.textContent).toBe("Update complete"));
    // Reading the same answer again says nothing new.
    await next(rollout({ state: "completed", updated: 4, total: 4 }));
    expect(live.textContent).toBe("Update complete");
  });

  it("shows nothing for an update that finished before the page was opened", async () => {
    answer = rollout({ state: "completed", updated: 4, total: 4 });
    mount();
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.length).toBeGreaterThan(0));
    await act(async () => {});
    expect(shown("Update complete")).toBe(0);
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.getByTestId("rollout-announcement").textContent).toBe("");
  });

  it("shows nothing when there is no update, and when the rollout cannot be read", async () => {
    mount();
    await act(async () => {});
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.queryByText("Update paused")).toBeNull();
    cleanup();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(null, { status: 500 })),
    );
    mount();
    await act(async () => {});
    expect(screen.queryByRole("progressbar")).toBeNull();
  });
});

describe("RolloutProgress when Docker stops", () => {
  it("offers Roll back and Retry update for a paused update, with Docker's words", async () => {
    answer = rollout({
      state: "paused",
      message: "update paused due to failure or early termination of task t1",
    });
    mount();
    expect(await screen.findByText("Update paused")).toBeTruthy();
    expect(
      screen.getByText(
        "Docker paused the update: update paused due to failure or early termination of task t1",
      ),
    ).toBeTruthy();
    const rollBack = screen.getByRole("button", { name: "Roll back" });
    const retry = screen.getByRole("button", { name: "Retry update" });
    await waitFor(() => expect(rollBack.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(rollBack);
    expect(onRollback).toHaveBeenCalledTimes(1);
    fireEvent.click(retry);
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("offers only Retry update for a paused rollback", async () => {
    answer = rollout({ state: "rollback_paused", message: "rollback paused" });
    mount();
    expect(await screen.findByText("Rollback paused")).toBeTruthy();
    expect(screen.getByText("Docker paused the rollback: rollback paused")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Roll back" })).toBeNull();
    expect(screen.getByRole("button", { name: "Retry update" })).toBeTruthy();
  });

  it("keeps Update rolled back, with Docker's message and Retry update, until the next update", async () => {
    answer = rollout({ state: "rollback_completed", message: "rollback completed" });
    mount();
    expect(await screen.findByText("Update rolled back")).toBeTruthy();
    expect(
      screen.getByText("Docker returned the service to its earlier settings: rollback completed"),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry update" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Roll back" })).toBeNull();
    await next(rollout({ state: "updating", updated: 0, total: 2 }));
    expect(await screen.findByText("Updating api")).toBeTruthy();
    expect(screen.queryByText("Update rolled back")).toBeNull();
  });

  it("leaves out the colon when Docker gave no message", async () => {
    answer = rollout({ state: "paused", message: "" });
    mount();
    expect(await screen.findByText("Docker paused the update.")).toBeTruthy();
  });

  it("announces a pause once, politely", async () => {
    answer = rollout({ state: "updating", updated: 1, total: 4 });
    mount();
    await screen.findByText("Updating api");
    await next(rollout({ state: "paused", message: "stopped" }));
    await waitFor(() =>
      expect(screen.getByTestId("rollout-announcement").textContent).toBe("Update paused"),
    );
    // The alert is not a second, assertive announcement.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("draws Docker's message as text, never as markup (T-06-29)", async () => {
    answer = rollout({
      state: "paused",
      message: '<img src=x onerror="alert(1)"> <a href="http://evil">go</a>',
    });
    mount();
    await screen.findByText("Update paused");
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector('a[href="http://evil"]')).toBeNull();
    expect(
      screen.getByText(
        /Docker paused the update: <img src=x onerror="alert\(1\)"> <a href="http:\/\/evil">go<\/a>/,
      ),
    ).toBeTruthy();
  });

  it("shows the buttons unavailable, with the reason, to a Viewer", async () => {
    role = "viewer";
    answer = rollout({ state: "paused", message: "stopped" });
    mount();
    await screen.findByText("Update paused");
    const retry = screen.getByRole("button", { name: "Retry update" });
    await waitFor(() => expect(retry.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(retry);
    expect(onRetry).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Roll back" }));
    expect(onRollback).not.toHaveBeenCalled();
  });
});

describe("serviceRolloutQuery polling", () => {
  const poll = (state: ServiceRolloutBody["state"] | undefined) => {
    const interval = serviceRolloutQuery(SERVICE_ENV, SERVICE_ID).refetchInterval;
    if (typeof interval !== "function") throw new Error("no refetchInterval function");
    return (interval as (q: unknown) => number | false | undefined)({
      state: { data: state ? rollout({ state }) : undefined },
    });
  };

  it("reads every 2 s while an update or a rollback runs", () => {
    expect(ROLLOUT_REFETCH_MS).toBe(2000);
    expect(poll("updating")).toBe(2000);
    expect(poll("rollback_started")).toBe(2000);
    expect(isRolloutRunning("paused")).toBe(false);
  });

  it("reads at the slow pace when nothing runs", () => {
    expect(poll("paused")).toBeGreaterThan(2000);
    expect(poll("none")).toBeGreaterThan(2000);
    expect(poll(undefined)).toBeGreaterThan(2000);
  });

  it("does not read while the page is hidden", () => {
    const spy = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    try {
      expect(poll("updating")).toBe(false);
    } finally {
      spy.mockRestore();
    }
  });
});
