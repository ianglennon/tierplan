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
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { JanitorPage } from "@/features/janitor/janitor-page";
import { TaskHistoryLimitCard } from "@/features/janitor/task-history-limit-card";
import "@/i18n";
import { janitorPolicy, janitorStatus } from "@/test-janitor";
import { makeMe } from "@/test-me";

const ENV = "e1";
const TITLE = "Task history limit";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const problem = (type: string, status: number) =>
  new Response(JSON.stringify({ type, title: "Error", status }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

let me: ReturnType<typeof makeMe>;
let limit: number;
let reads: number;
let puts: Record<string, unknown>[];
// What the stubbed server answers instead of the usual, when a test needs it to.
let readAnswer: (() => Response) | undefined;
let putAnswer: (() => Response) | undefined;
let envBody: Record<string, unknown>;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  limit = 5;
  reads = 0;
  puts = [];
  readAnswer = undefined;
  putAnswer = undefined;
  envBody = { id: ENV, name: "Prod", envType: "swarm", nodes: { ready: 2, total: 2 } };
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") return Response.json({ items: [envBody] });
      if (url.pathname === `/api/env/${ENV}/janitor/task-history` && req.method === "GET") {
        reads++;
        if (readAnswer) return readAnswer();
        return Response.json({ limit, dockerDefault: 5, keepsAll: limit < 0 });
      }
      if (url.pathname === `/api/env/${ENV}/janitor/task-history` && req.method === "PUT") {
        const body = (await req.json()) as Record<string, unknown>;
        puts.push(body);
        if (putAnswer) return putAnswer();
        const asked = body.limit as number;
        const changed = asked !== limit;
        limit = asked;
        return Response.json({ changed, limit, dockerDefault: 5, keepsAll: false });
      }
      if (url.pathname === `/api/env/${ENV}/janitor`) return Response.json(janitorStatus());
      if (url.pathname === `/api/env/${ENV}/janitor/policy`) return Response.json(janitorPolicy());
      if (url.pathname === `/api/env/${ENV}/janitor/runs`) {
        return Response.json({ runs: [], total: 0 });
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

function mountCard() {
  const root = createRootRoute();
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: "/",
        component: () => <TaskHistoryLimitCard envId={ENV} envName="Prod" />,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

function mountPage() {
  const root = createRootRoute();
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: "/env/$envId/janitor",
        component: () => <JanitorPage envId={ENV} />,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: [`/env/${ENV}/janitor`] }),
  });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const field = () => screen.findByRole("spinbutton", { name: "Tasks kept per slot" });
const saveButton = () => screen.getByRole("button", { name: "Save task history limit" });
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });

describe("TaskHistorySave", () => {
  it("shows the live value, sends the new limit and reads Currently 10 afterwards", async () => {
    mountCard();
    expect(await screen.findByRole("region", { name: TITLE })).toBeTruthy();
    const input = (await field()) as HTMLInputElement;
    expect(input.value).toBe("5");
    expect(screen.getByRole("status").textContent).toBe("Currently 5.");
    expect(screen.getByText(/^Docker's default is 5\./)).toBeTruthy();
    // Nothing changed yet: Save is off.
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");

    type(input, "10");
    expect(saveButton().getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(saveButton());

    await waitFor(() => expect(puts).toEqual([{ limit: 10 }]));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Currently 10."));
    expect(toasts.success).toHaveBeenCalledWith("Task history limit set to 10.");
    expect(((await field()) as HTMLInputElement).value).toBe("10");
    // Saved: nothing is left to save.
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
  });

  it("refuses a number outside 0 to 1000 and sends nothing", async () => {
    mountCard();
    const input = await field();
    type(input, "1001");
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Use a whole number from 0 to 1000.",
    );
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(saveButton());
    type(input, "2.5");
    expect(screen.getByRole("alert").textContent).toBe("Use a whole number from 0 to 1000.");
    expect(puts).toHaveLength(0);

    // 0 and 1000 are both allowed.
    type(input, "0");
    expect(screen.queryByRole("alert")).toBeNull();
    type(input, "1000");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(saveButton().getAttribute("aria-disabled")).toBeNull();
  });

  it("treats typing the swarm's own number back as no change", async () => {
    mountCard();
    const input = await field();
    type(input, "7");
    expect(saveButton().getAttribute("aria-disabled")).toBeNull();
    type(input, "5");
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
  });

  it("words a swarm that changed under the save and keeps what was typed", async () => {
    putAnswer = () => problem("swarm_changed", 409);
    mountCard();
    const input = (await field()) as HTMLInputElement;
    type(input, "10");
    fireEvent.click(saveButton());
    expect(
      (
        await screen.findByText("The swarm changed while TierPlan was saving. Try again.")
      ).getAttribute("role"),
    ).toBe("alert");
    expect(input.value).toBe("10");
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("words an agent that refuses the change by its own policy", async () => {
    putAnswer = () => problem("agent_policy_denied", 502);
    mountCard();
    type(await field(), "10");
    fireEvent.click(saveButton());
    expect(
      await screen.findByText("Update the agent on a manager to change this limit."),
    ).toBeTruthy();
  });

  it("words any other failed save with the generic line", async () => {
    putAnswer = () => problem("task_history_failed", 502);
    mountCard();
    type(await field(), "10");
    fireEvent.click(saveButton());
    const alert = await screen.findByText(/^Couldn't change the task history limit\./);
    expect(alert.textContent).toBe("Couldn't change the task history limit. Try again.");
    expect(alert.getAttribute("role")).toBe("alert");
  });

  it("says the limit could not be read, and reads it again on Retry", async () => {
    readAnswer = () => problem("internal", 500);
    mountCard();
    expect(await screen.findByText("Couldn't read the task history limit.")).toBeTruthy();
    readAnswer = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Retry reading the limit" }));
    expect(((await field()) as HTMLInputElement).value).toBe("5");
    expect(reads).toBe(2);
  });
});

describe("TaskHistoryKeepsAll", () => {
  it("reads a live -1 as Docker keeping every task, read-only and not savable", async () => {
    limit = -1;
    mountCard();
    const input = (await screen.findByLabelText("Tasks kept per slot")) as HTMLInputElement;
    expect(input.value).toBe("Docker keeps every task (-1)");
    expect(input.readOnly).toBe(true);
    expect(screen.getByRole("status").textContent).toBe("Currently -1.");
    type(input, "10");
    expect(input.value).toBe("Docker keeps every task (-1)");
    fireEvent.click(saveButton());
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
    expect(puts).toHaveLength(0);
  });
});

describe("TaskHistoryNoManager", () => {
  it("shows Not measured with the reason and cannot be saved when no manager is connected", async () => {
    readAnswer = () => problem("no_manager", 503);
    mountCard();
    const input = (await screen.findByLabelText("Tasks kept per slot")) as HTMLInputElement;
    expect(input.value).toBe("Not measured");
    expect(input.readOnly).toBe(true);
    expect(input.getAttribute("aria-disabled")).toBe("true");
    // The reason sits under the field and, for a screen reader, on the disabled button too.
    expect(
      screen.getAllByText("No manager is connected, so TierPlan can't read or change the limit.")
        .length,
    ).toBeGreaterThan(0);
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(saveButton());
    expect(toasts.info).toHaveBeenCalledWith(
      "No manager is connected, so TierPlan can't read or change the limit.",
    );
    expect(puts).toHaveLength(0);
  });

  it("words an agent too old to read the limit the same way", async () => {
    readAnswer = () => problem("agent_policy_denied", 502);
    mountCard();
    const input = (await screen.findByLabelText("Tasks kept per slot")) as HTMLInputElement;
    expect(input.value).toBe("Not measured");
    expect(screen.getByText("Update the agent on a manager to change this limit.")).toBeTruthy();
  });
});

describe("TaskHistoryNotAdmin", () => {
  it("shows the number read-only and disables Save with what it needs", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mountCard();
    const input = (await field()) as HTMLInputElement;
    expect(input.value).toBe("5");
    expect(input.readOnly).toBe(true);
    type(input, "10");
    expect(input.value).toBe("5");
    const button = saveButton();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs Admin on Prod").length).toBeGreaterThan(0);
    fireEvent.click(button);
    expect(toasts.info).toHaveBeenCalledWith("Needs Admin on Prod");
    expect(puts).toHaveLength(0);
  });
});

describe("TaskHistoryStandaloneHidden", () => {
  it("draws no card, and asks the server nothing, on a standalone environment", async () => {
    envBody = { id: ENV, name: "Prod", envType: "standalone", nodes: { ready: 1, total: 1 } };
    mountPage();
    expect(await screen.findByRole("heading", { level: 1, name: "Janitor" })).toBeTruthy();
    // Wait for the environment to be read: the Policy card is on the page by then.
    expect(await screen.findByRole("region", { name: "Policy" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: TITLE })).toBeNull();
    expect(document.getElementById("task-history")).toBeNull();
    expect(reads).toBe(0);
  });

  it("draws the card between Policy and Run history on a swarm", async () => {
    mountPage();
    expect(await screen.findByRole("region", { name: TITLE })).toBeTruthy();
    const ids = Array.from(document.querySelectorAll("section[id]")).map((s) => s.id);
    expect(ids.indexOf("policy")).toBeLessThan(ids.indexOf("task-history"));
    expect(ids.indexOf("task-history")).toBeLessThan(ids.indexOf("history"));
  });
});
