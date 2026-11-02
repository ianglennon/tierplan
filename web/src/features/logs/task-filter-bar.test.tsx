// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { ComponentProps } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { NodeList } from "@/api/nodes";
import type { ServiceTasks } from "@/api/services";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  type FilterOptions,
  filterOptionsFrom,
  NO_FILTER_OPTIONS,
  TaskFilterBar,
} from "@/features/logs/task-filter-bar";
import "@/i18n";

// The third row of a service's log toolbar (plan 06-30, OBS-02, D-33; UI-SPEC "Filter
// popovers", "Filter chips").

afterEach(cleanup);

const T1 = "aaaaaaaa1111111111111111a";
const T2 = "bbbbbbbb2222222222222222b";
const T2_OLD = "cccccccc3333333333333333c";

const options: FilterOptions = {
  loaded: true,
  tasks: [
    { id: T1, slot: 1, node: "alpha", state: "running", ended: false },
    { id: T2, slot: 2, node: "beta", state: "running", ended: false },
    { id: T2_OLD, slot: 2, node: "beta", state: "failed", ended: true },
  ],
  nodes: [
    { name: "alpha", role: "manager", tasks: 1 },
    { name: "beta", role: "worker", tasks: 1 },
    { name: "gamma", role: "worker", tasks: 0 },
  ],
};

function bar(extra: Partial<ComponentProps<typeof TaskFilterBar>> = {}) {
  const props = {
    service: "web",
    options,
    tasks: [] as string[],
    nodes: [] as string[],
    stopped: false,
    onTasksChange: vi.fn(),
    onNodesChange: vi.fn(),
    onStoppedChange: vi.fn(),
    ...extra,
  };
  render(
    <TooltipProvider>
      <TaskFilterBar {...props} />
    </TooltipProvider>,
  );
  return props;
}

const open = (name: RegExp | string) => fireEvent.click(screen.getByRole("button", { name }));

describe("the row", () => {
  it("has the Tasks and Nodes buttons and the Include stopped tasks switch, and no chips", () => {
    bar();
    expect(screen.getByRole("button", { name: "Tasks" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Nodes" })).toBeTruthy();
    const sw = screen.getByRole("switch", { name: "Include stopped tasks" });
    expect(sw.getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByText(/^Task: /)).toBeNull();
    expect(screen.queryByText(/^Node: /)).toBeNull();
  });

  it("puts the number chosen on the buttons", () => {
    bar({ tasks: [T1, T2], nodes: ["alpha"] });
    expect(screen.getByRole("button", { name: "Tasks (2)" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Nodes (1)" })).toBeTruthy();
  });

  it("turns the switch on and off, and explains it in a tooltip", async () => {
    const props = bar();
    const sw = screen.getByRole("switch", { name: "Include stopped tasks" });
    fireEvent.click(sw);
    expect(props.onStoppedChange).toHaveBeenCalledWith(true);
    fireEvent.focus(sw);
    expect(
      (
        await screen.findAllByText(
          "Off shows only the tasks running now, and new ones as they start.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });

  it("shows the switch on when stopped tasks are included", () => {
    bar({ stopped: true });
    expect(
      screen.getByRole("switch", { name: "Include stopped tasks" }).getAttribute("aria-checked"),
    ).toBe("true");
  });
});

describe("the Tasks popover", () => {
  it("lists the tasks running now with their node and state, and not the stopped ones", () => {
    bar();
    open("Tasks");
    const list = screen.getByRole("listbox");
    const items = within(list).getAllByRole("option");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringContaining("web.1"),
      expect.stringContaining("web.2"),
    ]);
    expect(items[0]?.textContent).toContain("alpha");
    expect(items[0]?.textContent).toContain("Running");
    expect(screen.queryByText("Failed")).toBeNull();
  });

  it("lists the stopped tasks too when they are included", () => {
    bar({ stopped: true });
    open("Tasks");
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items).toHaveLength(3);
    expect(items[2]?.textContent).toContain("Failed");
    // The first eight characters of the task's ID tell two tasks of one slot apart.
    expect(items[1]?.textContent).toContain("bbbbbbbb");
    expect(items[2]?.textContent).toContain("cccccccc");
  });

  it("finds a task by what is typed", () => {
    bar();
    open("Tasks");
    fireEvent.change(screen.getByPlaceholderText("Find a task"), { target: { value: "web.2" } });
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toContain("web.2");
  });

  it("chooses a task by its ID, marks it, and takes it off again", () => {
    const props = bar();
    open("Tasks");
    fireEvent.click(within(screen.getByRole("listbox")).getAllByRole("option")[1] as HTMLElement);
    expect(props.onTasksChange).toHaveBeenCalledWith([T2]);
    cleanup();

    const chosen = bar({ tasks: [T1, T2] });
    open(/^Tasks/);
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items[0]?.getAttribute("data-checked")).toBe("true");
    fireEvent.click(items[0] as HTMLElement);
    expect(chosen.onTasksChange).toHaveBeenCalledWith([T2]);
  });

  it("takes a task off that the address named by the start of its ID", () => {
    const props = bar({ tasks: [T1.slice(0, 8)] });
    open(/^Tasks/);
    const first = within(screen.getByRole("listbox")).getAllByRole("option")[0] as HTMLElement;
    expect(first.getAttribute("data-checked")).toBe("true");
    fireEvent.click(first);
    expect(props.onTasksChange).toHaveBeenCalledWith([]);
  });

  it("clears every task with Clear", () => {
    const props = bar({ tasks: [T1, T2] });
    open(/^Tasks/);
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(props.onTasksChange).toHaveBeenCalledWith([]);
  });

  it("says there are no tasks when the service has none", () => {
    bar({ options: { loaded: true, tasks: [], nodes: [] } });
    open("Tasks");
    expect(screen.getByText("No tasks")).toBeTruthy();
  });
});

describe("the Nodes popover", () => {
  it("lists the nodes with the service's tasks on them, their role and how many tasks", () => {
    bar();
    open("Nodes");
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringContaining("alpha"),
      expect.stringContaining("beta"),
    ]);
    expect(items[0]?.textContent).toContain("Manager");
    expect(items[0]?.textContent).toContain("1 task");
    expect(items[1]?.textContent).toContain("Worker");
  });

  it("lists a node that only has stopped tasks when they are included", () => {
    bar({ stopped: true });
    open("Nodes");
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items).toHaveLength(3);
    expect(items[2]?.textContent).toContain("0 tasks");
  });

  it("finds a node by what is typed and chooses it by name", () => {
    const props = bar();
    open("Nodes");
    fireEvent.change(screen.getByPlaceholderText("Find a node"), { target: { value: "bet" } });
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items).toHaveLength(1);
    fireEvent.click(items[0] as HTMLElement);
    expect(props.onNodesChange).toHaveBeenCalledWith(["beta"]);
  });

  it("clears the nodes with Clear", () => {
    const props = bar({ nodes: ["alpha"] });
    open(/^Nodes/);
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(props.onNodesChange).toHaveBeenCalledWith([]);
  });
});

describe("the chips", () => {
  it("names a task by its service and slot, a node by its name, and takes each off with a button of its own", () => {
    const props = bar({ tasks: [T2], nodes: ["alpha", "beta"] });
    expect(screen.getByText("Task: web.2")).toBeTruthy();
    expect(screen.getByText("Node: alpha")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove task filter web.2" }));
    expect(props.onTasksChange).toHaveBeenCalledWith([]);
    fireEvent.click(screen.getByRole("button", { name: "Remove node filter alpha" }));
    expect(props.onNodesChange).toHaveBeenCalledWith(["beta"]);
  });

  it("finds the task of a chip by the start of the ID in the address", () => {
    bar({ tasks: [T2.slice(0, 8)] });
    expect(screen.getByText("Task: web.2")).toBeTruthy();
  });

  it("names a task of a global service by its node", () => {
    bar({
      options: {
        loaded: true,
        tasks: [{ id: T1, slot: 0, node: "alpha", state: "running", ended: false }],
        nodes: [],
      },
      tasks: [T1],
    });
    expect(screen.getByText("Task: web@alpha")).toBeTruthy();
  });

  it("says Task not found for an ID that matches no task", () => {
    const props = bar({ tasks: ["nope0000deadbeef"] });
    expect(screen.getByText("Task not found")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove task filter nope0000" }));
    expect(props.onTasksChange).toHaveBeenCalledWith([]);
  });

  it("does not call a task missing before the tasks have been read", () => {
    bar({ options: NO_FILTER_OPTIONS, tasks: ["nope0000deadbeef"] });
    expect(screen.queryByText("Task not found")).toBeNull();
    expect(screen.getByText("Task: nope0000")).toBeTruthy();
  });

  it("draws a node name as text", () => {
    bar({ nodes: ["<img src=x onerror=alert(1)>"] });
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText("Node: <img src=x onerror=alert(1)>")).toBeTruthy();
  });
});

describe("filterOptionsFrom", () => {
  const item = (id: string, node: string, dockerState: string, state = dockerState) => ({
    id,
    node,
    nodeId: `id-${node}`,
    state,
    dockerState,
  });
  const tasks = {
    global: false,
    retention: 5,
    total: 3,
    slots: [
      { slot: 1, node: "alpha", current: item(T1, "alpha", "running"), history: [] },
      {
        slot: 2,
        node: "beta",
        current: item(T2, "beta", "running", "unknown"),
        history: [item(T2_OLD, "beta", "failed")],
      },
    ],
  } as unknown as ServiceTasks;
  const nodes = {
    items: [
      { id: "id-alpha", hostname: "alpha", role: "manager" },
      { id: "id-beta", hostname: "beta", role: "worker" },
      { id: "id-gamma", hostname: "gamma", role: "worker" },
    ],
  } as unknown as NodeList;

  it("is not loaded before the tasks have been read", () => {
    expect(filterOptionsFrom(undefined, nodes)).toEqual(NO_FILTER_OPTIONS);
    expect(NO_FILTER_OPTIONS.loaded).toBe(false);
  });

  it("lists the current and the earlier tasks with the state to show, and whether Docker ended them", () => {
    const got = filterOptionsFrom(tasks, nodes);
    expect(got.loaded).toBe(true);
    expect(got.tasks).toEqual([
      { id: T1, slot: 1, node: "alpha", state: "running", ended: false },
      { id: T2, slot: 2, node: "beta", state: "unknown", ended: false },
      { id: T2_OLD, slot: 2, node: "beta", state: "failed", ended: true },
    ]);
  });

  it("lists the nodes the service has tasks on, with their role and the tasks not ended", () => {
    const got = filterOptionsFrom(tasks, nodes);
    expect(got.nodes).toEqual([
      { name: "alpha", role: "manager", tasks: 1 },
      { name: "beta", role: "worker", tasks: 1 },
    ]);
  });

  it("lists a node without a role when the nodes have not been read", () => {
    expect(filterOptionsFrom(tasks, undefined).nodes[0]).toEqual({ name: "alpha", tasks: 1 });
  });

  it("copes with a service that has no slots", () => {
    const got = filterOptionsFrom({ ...tasks, slots: null }, nodes);
    expect(got).toEqual({ loaded: true, tasks: [], nodes: [] });
  });
});

describe("a stack's row (plan 07-29)", () => {
  const stackOptions: FilterOptions = {
    loaded: true,
    tasks: [
      { id: T1, service: "api", slot: 1, node: "alpha", state: "running", ended: false },
      { id: T2, service: "web", slot: 2, node: "beta", state: "running", ended: false },
    ],
    nodes: [{ name: "alpha", tasks: 1 }],
  };

  it("puts what leads before the Tasks button and what trails after the chips, and words the switch for a stack", () => {
    bar({
      options: stackOptions,
      stack: true,
      tasks: [T1],
      leading: <span>LEAD</span>,
      trailing: <span>TRAIL</span>,
    });
    const lead = screen.getByText("LEAD");
    const tasksButton = screen.getByRole("button", { name: "Tasks (1)" });
    const chip = screen.getByText("Task: api.1");
    const trail = screen.getByText("TRAIL");
    expect(
      lead.compareDocumentPosition(tasksButton) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(chip.compareDocumentPosition(trail) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Include stopped" })).toBeTruthy();
  });

  it("names each task by its own service and orders them by service, then slot", () => {
    bar({ options: stackOptions, stack: true });
    fireEvent.click(screen.getByRole("button", { name: "Tasks" }));
    const items = within(screen.getByRole("listbox")).getAllByRole("option");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringContaining("api.1"),
      expect.stringContaining("web.2"),
    ]);
  });

  it("chooses containers on a host: a Containers button, the engine's names and states, and no Nodes", () => {
    bar({
      unit: "container",
      stack: true,
      options: {
        loaded: true,
        tasks: [
          {
            id: T1,
            service: "web",
            name: "solo-web-1",
            slot: 1,
            node: "",
            state: "running",
            stateText: "running",
            ended: false,
          },
        ],
        nodes: [],
      },
    });
    expect(screen.queryByRole("button", { name: "Nodes" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Containers" }));
    const item = within(screen.getByRole("listbox")).getByRole("option");
    expect(item.textContent).toContain("solo-web-1");
    expect(item.textContent).toContain("running");
  });
});
