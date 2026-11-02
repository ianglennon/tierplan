// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { serviceRetry, serviceTasksQuery } from "@/api/services";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import type { TaskItem, TaskSlot } from "@/features/tasks/task-state";
import { TaskTable } from "@/features/tasks/task-table";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import {
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  serviceDetail,
  serviceFetch,
} from "@/test-service";

// The Tasks tab of a service: slots with their history, the failed filter, row actions and the
// states of E3 (plan 06-07, RES-04, D-17, UI-SPEC "Tasks tab").

const task = (extra: Partial<TaskItem> = {}): TaskItem => ({
  id: "task-run-1",
  slot: 1,
  nodeId: "node-1",
  node: "mgr-1",
  nodeKnown: true,
  targetNodeId: SERVICE_ENV,
  state: "running",
  dockerState: "running",
  desiredState: "running",
  error: "",
  message: "started",
  exitCode: null,
  containerId: "cont-1",
  createdAt: "2026-10-14T08:00:00Z",
  updatedAt: "2026-10-14T08:01:00Z",
  stateSince: "2026-10-14T08:01:00Z",
  ...extra,
});

const failedTask = (id: string, extra: Partial<TaskItem> = {}): TaskItem =>
  task({
    id,
    state: "failed",
    dockerState: "failed",
    desiredState: "shutdown",
    error: "task: non-zero exit (1)",
    message: "started",
    exitCode: 1,
    ...extra,
  });

const slotOf = (n: number, current: TaskItem, history: TaskItem[] = []): TaskSlot => ({
  slot: n,
  node: current.node,
  current,
  history,
});

const SLOTS: TaskSlot[] = [
  slotOf(1, task({ id: "t1-now" }), [
    failedTask("t1-failed"),
    task({
      id: "t1-rolled",
      state: "shutdown",
      dockerState: "shutdown",
      desiredState: "shutdown",
      exitCode: 137,
      message: "shutdown",
    }),
  ]),
  slotOf(2, task({ id: "t2-now", slot: 2, node: "wrk-ready", nodeId: "node-3", targetNodeId: "" })),
];

function renderTable(props: Partial<React.ComponentProps<typeof TaskTable>> = {}) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <TaskTable
            envId={SERVICE_ENV}
            serviceName="web"
            slots={SLOTS}
            retention={5}
            global={false}
            showFailedOnly={false}
            {...props}
          />
          <Toaster />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
});

// The words the State cells show, in row order. The Desired cells use some of the same words.
const stateWords = (root: ParentNode = document) =>
  Array.from(root.querySelectorAll("[data-slot=task-state-word]")).map((e) => e.textContent);

// rowOf finds the row of the task with this short ID.
function rowOf(id: string): HTMLElement {
  const row = screen.getByText(id).closest("tr");
  if (!row) throw new Error(`no row for ${id}`);
  return row;
}

describe("TaskTable", () => {
  it("lists one row per slot, in the columns of the spec, named after the service and the slot", () => {
    renderTable();
    const table = screen.getByRole("table", { name: "Tasks" });
    for (const name of ["Task", "State", "Desired", "Node", "Updated", "Error"]) {
      expect(within(table).getByRole("columnheader", { name })).toBeTruthy();
    }
    expect(within(table).getByText("web.1")).toBeTruthy();
    expect(within(table).getByText("web.2")).toBeTruthy();
    expect(within(table).getByText("mgr-1")).toBeTruthy();
    expect(within(table).getByText("wrk-ready")).toBeTruthy();
    expect(stateWords(table)).toEqual(["Running", "Running"]);
  });

  it("names the rows of a global service after the service and puts the node in the Node column", () => {
    renderTable({
      global: true,
      slots: [
        slotOf(0, task({ id: "g1", slot: 0 })),
        slotOf(0, task({ id: "g2", slot: 0, node: "wrk-ready", nodeId: "node-3" })),
      ],
    });
    expect(screen.getAllByText("web")).toHaveLength(2);
    expect(screen.queryByText("web.0")).toBeNull();
    expect(screen.getByText("2 tasks on 2 nodes")).toBeTruthy();
  });

  it("says how many tasks are in how many slots, in the singular where it is one", () => {
    renderTable();
    expect(screen.getByText("4 tasks in 2 slots")).toBeTruthy();
    cleanup();
    renderTable({ slots: [slotOf(1, task())] });
    expect(screen.getByText("1 task in 1 slot")).toBeTruthy();
    cleanup();
    renderTable({ global: true, slots: [slotOf(0, task({ slot: 0 }))] });
    expect(screen.getByText("1 task on 1 node")).toBeTruthy();
  });

  it("expands a slot to its earlier tasks with their exit code, start and finish", () => {
    renderTable();
    const expander = screen.getByRole("button", { name: "Show earlier tasks for web.1" });
    expect(expander.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(expander);
    expect(expander.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("Earlier tasks in this slot. Docker keeps up to 5.")).toBeTruthy();
    const history = screen.getByRole("table", { name: "Earlier tasks for web.1" });
    for (const name of ["Exit code", "Started", "Finished"]) {
      expect(within(history).getByRole("columnheader", { name })).toBeTruthy();
    }
    expect(stateWords(history)).toEqual(["Failed", "Shut down"]);
    expect(within(rowOf("t1-failed")).getByText("1")).toBeTruthy();
    expect(within(rowOf("t1-rolled")).getByText("137")).toBeTruthy();
    fireEvent.click(expander);
    expect(screen.queryByRole("table", { name: "Earlier tasks for web.1" })).toBeNull();
  });

  it("says a slot with no earlier task has none", () => {
    renderTable();
    fireEvent.click(screen.getByRole("button", { name: "Show earlier tasks for web.2" }));
    expect(screen.getByText("No earlier tasks in this slot.")).toBeTruthy();
  });

  it("shows a missing exit code and a missing error as a dash", () => {
    renderTable({
      slots: [
        slotOf(1, task(), [
          task({ id: "old", state: "complete", dockerState: "complete", message: "" }),
        ]),
      ],
    });
    fireEvent.click(screen.getByRole("button", { name: "Show earlier tasks for web.1" }));
    const history = screen.getByRole("table", { name: "Earlier tasks for web.1" });
    expect(within(history).getAllByText("—").length).toBeGreaterThanOrEqual(2);
  });

  it("draws a failed task's error in the offline colour, in full in the DOM and in the row", () => {
    const long = `task: non-zero exit (1) ${"x".repeat(300)}`;
    renderTable({ slots: [slotOf(1, failedTask("f", { error: long }))] });
    const cell = screen.getByText(long);
    expect(cell.className).toContain("text-status-offline-text");
    expect(cell.className).toContain("truncate");
  });

  it("wraps the whole error in an expanded row", () => {
    const long = `boom ${"y".repeat(300)}`;
    renderTable({ slots: [slotOf(1, task(), [failedTask("old", { error: long })])] });
    fireEvent.click(screen.getByRole("button", { name: "Show earlier tasks for web.1" }));
    const cell = within(screen.getByRole("table", { name: "Earlier tasks for web.1" })).getByText(
      long,
    );
    expect(cell.className).toContain("break-all");
    expect(cell.className).not.toContain("truncate");
  });

  it("reads Unknown for a task Docker still calls running on a node that is down", () => {
    renderTable({
      slots: [slotOf(1, task({ state: "unknown", dockerState: "running", node: "wrk-down" }))],
    });
    expect(stateWords()).toEqual(["Unknown"]);
  });

  it("shows a removed node as such and an unplaced task's node as a dash", () => {
    renderTable({
      slots: [
        slotOf(
          1,
          task({ id: "gone", node: "", nodeId: "node-gone", nodeKnown: false, targetNodeId: "" }),
        ),
        slotOf(
          2,
          task({
            id: "unplaced",
            slot: 2,
            node: "",
            nodeId: "",
            targetNodeId: "",
            state: "pending",
            dockerState: "pending",
            containerId: "",
          }),
        ),
      ],
    });
    expect(screen.getByText("Removed node")).toBeTruthy();
    const unplaced = screen.getByText("web.2").closest("tr");
    if (!unplaced) throw new Error("no row");
    expect(within(unplaced).getAllByText("—").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("Pending")).toBeTruthy();
  });

  describe("Show only failed", () => {
    it("keeps the slots with a failed or rejected task and shows only those tasks when expanded", () => {
      renderTable({ showFailedOnly: true });
      expect(screen.getByText("web.1")).toBeTruthy();
      expect(screen.queryByText("web.2")).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Show earlier tasks for web.1" }));
      const history = screen.getByRole("table", { name: "Earlier tasks for web.1" });
      expect(stateWords(history)).toEqual(["Failed"]);
      expect(within(history).queryByText("t1-rolled")).toBeNull();
    });

    it("says nothing has failed when no slot has", () => {
      renderTable({ showFailedOnly: true, slots: [slotOf(1, task())] });
      expect(screen.getByText("No failed tasks")).toBeTruthy();
      expect(
        screen.getByText("Nothing has failed in the history Docker keeps for this service."),
      ).toBeTruthy();
    });

    it("is a switch that asks for the change", () => {
      const change = vi.fn();
      renderTable({ onShowFailedOnlyChange: change });
      const toggle = screen.getByRole("switch", { name: "Show only failed" });
      expect(toggle.getAttribute("aria-checked")).toBe("false");
      fireEvent.click(toggle);
      expect(change).toHaveBeenCalledWith(true);
    });

    it("has no switch when nothing can take the change", () => {
      renderTable();
      expect(screen.queryByRole("switch")).toBeNull();
    });
  });

  describe("the empty state", () => {
    it("says a service scaled to 0 has no tasks, with Scale… when it is allowed", () => {
      const scale = vi.fn();
      renderTable({ slots: [], onScale: scale });
      expect(screen.getByText("No tasks")).toBeTruthy();
      expect(
        screen.getByText(
          "This service is scaled to 0, so nothing is running. Scale it up to start tasks.",
        ),
      ).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Scale…" }));
      expect(scale).toHaveBeenCalledOnce();
    });

    it("leaves Scale… out when it is not allowed", () => {
      renderTable({ slots: [] });
      expect(screen.getByText("No tasks")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Scale…" })).toBeNull();
    });
  });

  describe("row actions", () => {
    async function openMenu(row: string) {
      await act(async () => {
        fireEvent.keyDown(screen.getByRole("button", { name: `Actions for ${row}` }), {
          key: "Enter",
        });
      });
    }

    it("opens the task's container on its node", async () => {
      renderTable();
      await openMenu("web.1");
      const link = await screen.findByRole("menuitem", { name: "Open container" });
      expect(link.getAttribute("href")).toBe(
        `/env/${SERVICE_ENV}/containers/cont-1?node=${SERVICE_ENV}`,
      );
    });

    it("opens the Logs tab narrowed to the task, when the table knows its service", async () => {
      renderTable({ serviceId: SERVICE_ID });
      await openMenu("web.1");
      const link = await screen.findByRole("menuitem", { name: "Logs for this task" });
      expect(link.getAttribute("href")).toBe(
        `/env/${SERVICE_ENV}/services/${SERVICE_ID}/logs?task=t1-now`,
      );
    });

    it("leaves Logs for this task out where the service is not known, as on a node's page", async () => {
      renderTable();
      await openMenu("web.1");
      await screen.findByRole("menuitem", { name: "Open container" });
      expect(screen.queryByRole("menuitem", { name: "Logs for this task" })).toBeNull();
    });

    it("disables Open container with the reason when the task has no container yet", async () => {
      renderTable({
        slots: [slotOf(1, task({ containerId: "", state: "pending", dockerState: "pending" }))],
      });
      await openMenu("web.1");
      const item = await screen.findByRole("menuitem", { name: /Open container/ });
      expect(item.getAttribute("aria-disabled")).toBe("true");
      expect(item.getAttribute("href")).toBeNull();
      expect(within(item).getByText("No container yet")).toBeTruthy();
    });

    it("disables Open container with the node's name when nothing reaches the node", async () => {
      renderTable({ slots: [slotOf(2, task({ slot: 2, node: "wrk-ready", targetNodeId: "" }))] });
      await openMenu("web.2");
      const item = await screen.findByRole("menuitem", { name: /Open container/ });
      expect(item.getAttribute("aria-disabled")).toBe("true");
      expect(within(item).getByText("wrk-ready isn't reporting right now.")).toBeTruthy();
    });

    it("says a removed node is gone rather than not reporting", async () => {
      renderTable({
        slots: [
          slotOf(1, task({ node: "", nodeId: "node-gone", nodeKnown: false, targetNodeId: "" })),
        ],
      });
      await openMenu("web.1");
      const item = await screen.findByRole("menuitem", { name: /Open container/ });
      expect(within(item).getByText("That node is no longer in the swarm.")).toBeTruthy();
    });

    it("copies the task ID", async () => {
      const writeText = vi.fn().mockResolvedValue(undefined);
      vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
      renderTable();
      await openMenu("web.1");
      fireEvent.click(await screen.findByRole("menuitem", { name: "Copy task ID" }));
      await waitFor(() => expect(writeText).toHaveBeenCalledWith("t1-now"));
    });
  });
});

describe("serviceTasksQuery", () => {
  const original = Object.getOwnPropertyDescriptor(Document.prototype, "visibilityState");
  afterEach(() => {
    if (original) Object.defineProperty(Document.prototype, "visibilityState", original);
  });

  it("polls every 5 s while the page is visible and stops while it is hidden", () => {
    const interval = serviceTasksQuery(SERVICE_ENV, SERVICE_ID).refetchInterval;
    if (typeof interval !== "function") throw new Error("refetchInterval is not a function");
    Object.defineProperty(Document.prototype, "visibilityState", {
      configurable: true,
      get: () => "visible",
    });
    expect(interval(undefined as never)).toBe(5000);
    Object.defineProperty(Document.prototype, "visibilityState", {
      configurable: true,
      get: () => "hidden",
    });
    expect(interval(undefined as never)).toBe(false);
  });

  it("extends the service's key, so an event that refreshes the service refreshes its tasks", () => {
    const key = serviceTasksQuery(SERVICE_ENV, SERVICE_ID).queryKey;
    expect(key.slice(0, 4)).toEqual(["env", SERVICE_ENV, "services", "detail"]);
    expect(key.at(-1)).toBe("tasks");
  });
});

// --- The page ---------------------------------------------------------------------------------

const PAGE = `/env/${SERVICE_ENV}/services/${SERVICE_ID}/tasks`;
const defaults = queryClient.getDefaultOptions();
let state: ServiceApiState;
let router: ReturnType<typeof createRouter>;

function tasksBody(slots: TaskSlot[] = SLOTS, extra: Record<string, unknown> = {}) {
  return {
    slots,
    retention: 5,
    total: slots.reduce((n, s) => n + 1 + (s.history?.length ?? 0), 0),
    global: false,
    ...extra,
  };
}

function renderPage(url = PAGE) {
  router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [url] }) });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("the Tasks tab", () => {
  beforeEach(() => {
    state = {
      sent: [],
      me: makeMe({ envs: { [SERVICE_ENV]: "operator" } }),
      detail: () => Response.json(serviceDetail({ running: 2, desired: 2 })),
      inspect: () => Response.json({ masked: false, document: {} }),
      scale: () => Response.json({ results: [] }),
      tasks: () => Response.json(tasksBody()),
    };
    queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
    serviceRetry.attempts = 0;
    setCsrfToken(null);
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
    vi.stubGlobal("fetch", vi.fn(serviceFetch(state)));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    queryClient.clear();
    queryClient.setDefaultOptions(defaults);
    serviceRetry.attempts = 1;
  });

  it("is the tab after Overview and shows the service's slots", async () => {
    renderPage();
    const tabs = await screen.findAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual([
      "Overview",
      "Tasks",
      "Placement",
      "Logs",
      "Stats",
      "Inspect",
    ]);
    expect(screen.getByRole("tab", { name: "Tasks" }).getAttribute("href")).toBe(PAGE);
    expect(await screen.findByText("api.1")).toBeTruthy();
    expect(screen.getByText("api.2")).toBeTruthy();
    expect(screen.getByText("4 tasks in 2 slots")).toBeTruthy();
  });

  it("shows eight skeleton rows while the tasks load, with the tabs already there", async () => {
    state.tasks = () => new Promise<Response>(() => {});
    renderPage();
    expect(await screen.findByRole("tab", { name: "Tasks" })).toBeTruthy();
    expect(await screen.findByText("Loading tasks")).toBeTruthy();
    const table = screen.getByRole("table", { name: "Tasks" });
    expect(table.querySelectorAll("tbody tr")).toHaveLength(8);
  });

  it("shows a load error with Retry loading tasks, which asks again", async () => {
    state.tasks = () => new Response(null, { status: 500 });
    renderPage();
    expect(await screen.findByText("Couldn't load tasks.")).toBeTruthy();
    state.tasks = () => Response.json(tasksBody());
    fireEvent.click(screen.getByRole("button", { name: "Retry loading tasks" }));
    expect(await screen.findByText("api.1")).toBeTruthy();
  });

  it("reads failed=1 from the URL as the switch on, and the switch writes it back", async () => {
    renderPage(`${PAGE}?failed=1`);
    const toggle = await screen.findByRole("switch", { name: "Show only failed" });
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("true"));
    expect(screen.getByText("api.1")).toBeTruthy();
    expect(screen.queryByText("api.2")).toBeNull();
    fireEvent.click(toggle);
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    expect(await screen.findByText("api.2")).toBeTruthy();
    fireEvent.click(screen.getByRole("switch", { name: "Show only failed" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ failed: 1 }));
  });

  it("drops a failed value it does not know", async () => {
    renderPage(`${PAGE}?failed=banana`);
    expect(await screen.findByText("api.2")).toBeTruthy();
    expect(
      screen.getByRole("switch", { name: "Show only failed" }).getAttribute("aria-checked"),
    ).toBe("false");
  });

  it("offers Scale… to an Operator when the service is scaled to 0", async () => {
    state.tasks = () => Response.json(tasksBody([]));
    state.detail = () => Response.json(serviceDetail({ running: 0, desired: 0 }));
    renderPage();
    expect(await screen.findByText("No tasks")).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Scale…" });
    // The header's and the empty state's.
    expect(buttons.length).toBe(2);
  });

  it("does not offer Scale… to a Viewer", async () => {
    state.me = makeMe({ envs: { [SERVICE_ENV]: "viewer" } });
    state.tasks = () => Response.json(tasksBody([]));
    renderPage();
    expect(await screen.findByText("No tasks")).toBeTruthy();
    const empty = screen.getByText("No tasks").closest("[data-slot=empty]");
    if (!empty) throw new Error("no empty state");
    expect(within(empty as HTMLElement).queryByRole("button", { name: "Scale…" })).toBeNull();
  });
});
