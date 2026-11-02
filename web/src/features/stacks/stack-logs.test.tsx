// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import type { LogLineData } from "@/features/logs/log-buffer";
import type { LogViewParams } from "@/features/logs/log-prefs";
import { StackLogs } from "@/features/stacks/stack-logs";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Logs tab of a stack (plan 07-29, STK-10, D-22). The stack and its services come from the
// API behind a fetch stub; the stream is the browser's EventSource, driven by hand.

const ENV = "e1";
const VIEWPORT = 400;

type Json = Record<string, unknown>;

let detail: Json;
let services: Json[];
let resourceTasks: Json[];
let client: QueryClient;

const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
const saved: Record<string, PropertyDescriptor | undefined> = {};

function lay(prop: string, value: number) {
  saved[prop] = Object.getOwnPropertyDescriptor(HTMLElement.prototype, prop);
  Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, value });
}

function service(short: string, over: Json = {}): Json {
  return {
    id: `svc-${short}`,
    name: `tracer_${short}`,
    short,
    mode: "replicated",
    state: "running",
    updateState: "",
    running: 2,
    desired: 2,
    image: "nginx",
    pinned: "nginx@sha256:aaaa",
    pinState: "pinned",
    ports: [],
    containers: [],
    ...over,
  };
}

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  lay("offsetHeight", VIEWPORT);
  lay("offsetWidth", 800);
  detail = {
    id: "s1",
    name: "tracer",
    target: "swarm",
    source: "inline",
    state: "running",
    lastDeployedAt: "2026-10-19T11:00:00Z",
    lastDeployedBy: "bob",
    hasDraft: false,
    envId: ENV,
    sourceRef: "",
    createdBy: "ada",
    createdAt: "2026-10-19T10:00:00Z",
    stopped: false,
    rollback: { available: false, deployedAt: "", deployedBy: "" },
  };
  services = [service("web"), service("api"), service("db")];
  resourceTasks = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/services`) return Response.json({ items: services });
      if (path === `/api/env/${ENV}/stacks/s1/resources`) {
        return Response.json({
          tasks: { items: resourceTasks, error: "" },
          networks: { items: [], error: "" },
          volumes: { items: [], error: "", notReporting: [] },
          secrets: { items: [], error: "" },
          configs: { items: [], error: "" },
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  client.clear();
  localStorage.clear();
  for (const [prop, desc] of Object.entries(saved)) {
    if (desc) Object.defineProperty(HTMLElement.prototype, prop, desc);
    else delete proto[prop];
  }
});

function mount(params: LogViewParams = {}, onParamsChange: (p: LogViewParams) => void = () => {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackLogs envId={ENV} stackId="s1" params={params} onParamsChange={onParamsChange} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function opened(): Promise<FakeEventSource> {
  return waitFor(() => {
    const found = FakeEventSource.instances[0];
    if (!found) throw new Error("no stream was opened");
    return found;
  });
}

async function connect(tasks: Json[] = []) {
  const s = await opened();
  await act(async () => {
    s.open();
    s.sendFrame("ready", { tty: false, target: "swarm", tasks });
  });
  return s;
}

async function send(s: FakeEventSource, batch: LogLineData[]) {
  await act(async () => {
    s.sendFrame("log", { lines: batch }, batch[batch.length - 1]?.t ?? "");
  });
}

const line = (
  service: string,
  slot: number,
  node: string,
  m: string,
  sec: number,
): LogLineData => ({
  t: `2026-10-20T12:00:${String(sec).padStart(2, "0")}.000000000Z`,
  s: "o",
  m,
  task: `task-${service}-${slot}`,
  slot,
  node,
  service,
});

describe("the stack's merged log", () => {
  it("opens the stack's stream on the last 500 lines with a viewer ID, and no stream before the stack is known", async () => {
    mount();
    expect(FakeEventSource.instances).toHaveLength(0);

    const s = await opened();
    const url = new URL(s.url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/stacks/s1/logs/stream");
    expect(url.searchParams.get("tail")).toBe("500");
    expect(url.searchParams.get("sid")).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(await screen.findByRole("log", { name: "Logs for tracer" })).toBeTruthy();
  });

  it("starts every line with its prefix: service.slot@node for a swarm task, service@node for a global one", async () => {
    mount();
    const s = await connect();
    await send(s, [
      line("web", 2, "mgr-2", "hello", 1),
      line("agent", 0, "mgr-1", "tick", 2),
      line("api", 1, "mgr-1", "ready", 3),
    ]);

    const surface = screen.getByRole("log");
    const prefixes = [...surface.querySelectorAll("[data-task-prefix]")].map((p) => p.textContent);
    expect(prefixes).toEqual(["web.2@mgr-2", "agent@mgr-1", "api.1@mgr-1"]);
  });

  it("colours a prefix by its service's place among the stack's services sorted by name, so every replica shares a colour", async () => {
    mount();
    const s = await connect();
    // api is first of api, db, web; web is third: the eight colours are 4, 5, 6, 3, 12 ...
    await send(s, [
      line("web", 1, "mgr-1", "one", 1),
      line("web", 2, "mgr-2", "two", 2),
      line("api", 1, "mgr-1", "three", 3),
    ]);

    const prefixes = [
      ...screen.getByRole("log").querySelectorAll<HTMLElement>("[data-task-prefix]"),
    ];
    const colours = prefixes.map((p) => p.style.color);
    expect(colours[0]).toBe("var(--ansi-6)");
    expect(colours[1]).toBe("var(--ansi-6)");
    expect(colours[2]).toBe("var(--ansi-4)");
  });

  it("shows no prefix when Task names is off in the address, and says so in the toolbar", async () => {
    mount({ prefix: false });
    const s = await connect();
    await send(s, [line("web", 1, "mgr-1", "one", 1)]);

    expect(screen.getByRole("log").querySelector("[data-task-prefix]")).toBeNull();
    const toggle = screen.getByRole("button", { name: "Task names" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
  });

  it("says how many tasks it follows beside the Live chip once connected", async () => {
    mount();
    const task = (id: string, state: string): Json => ({
      id,
      service: "web",
      slot: 1,
      node: "mgr-1",
      state,
    });
    await connect([task("t1", "running"), task("t2", "running"), task("t3", "failed")]);

    expect(await screen.findByText("Following 2 tasks")).toBeTruthy();
  });

  it("never reads a line as markup", async () => {
    mount();
    const s = await connect();
    const nasty = '<img src=x onerror="alert(1)"> https://evil.test';
    await send(s, [line("web", 1, "mgr-1", nasty, 1)]);

    const surface = screen.getByRole("log");
    expect(surface.querySelector("img")).toBeNull();
    expect(surface.querySelector("a")).toBeNull();
    expect(surface.textContent).toContain(nasty);
  });

  it("offers no earlier page: the Show range reaches further back", async () => {
    mount();
    const s = await connect();
    await send(s, [line("web", 1, "mgr-1", "one", 1)]);

    expect(screen.queryByText("Load earlier lines")).toBeNull();
    expect(screen.queryByText("Start of logs")).toBeNull();
  });

  it("closes the stream when the page goes away", async () => {
    mount();
    const s = await connect();
    cleanup();
    expect(s.closed).toBe(true);
  });
});

// ---- toolbar row 3, the cap and the replacement marker (plan 07-29, Task 2) ----

function swarmTask(id: string, service: string, slot: number, node: string, state = "running") {
  return {
    service,
    serviceId: `svc-${service}`,
    global: false,
    task: {
      id,
      slot,
      nodeId: `node-${node}`,
      node,
      nodeKnown: true,
      targetNodeId: "",
      state,
      dockerState: state,
      desiredState: "running",
      error: "",
      message: "",
      exitCode: null,
      containerId: "",
      createdAt: "2026-10-20T10:00:00Z",
      updatedAt: "2026-10-20T11:00:00Z",
      stateSince: "2026-10-20T10:30:00Z",
    },
  };
}

function container(id: string, name: string, state: string): Json {
  return { id, name, state, health: "none" };
}

function standalone() {
  detail = { ...detail, target: "standalone" };
  services = [
    service("web", {
      id: "",
      name: "web",
      mode: "",
      running: 1,
      desired: 2,
      state: "degraded",
      containers: [
        container("cweb1", "solo-web-1", "running"),
        container("cweb2", "solo-web-2", "exited"),
      ],
    }),
    service("db", {
      id: "",
      name: "db",
      mode: "",
      running: 1,
      desired: 1,
      containers: [container("cdb1", "solo-db-1", "running")],
    }),
  ];
}

describe("toolbar row 3 of a swarm stack", () => {
  it("has Services, Tasks and Nodes and the Include stopped switch, and no Containers", async () => {
    mount();
    await connect();

    expect(await screen.findByRole("button", { name: "Services" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Tasks" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Nodes" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Include stopped" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Containers" })).toBeNull();
  });

  it("explains the Include stopped switch and turns it on in the address", async () => {
    const changes: LogViewParams[] = [];
    mount({}, (p) => changes.push(p));
    await connect();

    const sw = await screen.findByRole("switch", { name: "Include stopped" });
    fireEvent.focus(sw);
    expect(
      (
        await screen.findAllByText(
          "Off shows only what is running now, and new ones as they start.",
        )
      ).length,
    ).toBeGreaterThan(0);
    fireEvent.click(sw);
    expect(changes.at(-1)?.stopped).toBe(true);
  });

  it("puts the number of services chosen on the button and shows a chip that removes the filter", async () => {
    const changes: LogViewParams[] = [];
    mount({ service: "web,api" }, (p) => changes.push(p));
    await connect();

    expect(await screen.findByRole("button", { name: "Services (2)" })).toBeTruthy();
    expect(screen.getByText("Service: web")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove service filter web" }));
    expect(changes.at(-1)?.service).toBe("api");
  });

  it("lists each service with its replicas and state, searches them, sets the filter and clears it", async () => {
    services = [
      service("web", { running: 2, desired: 2, state: "running" }),
      service("api", { running: 1, desired: 2, state: "degraded" }),
    ];
    const changes: LogViewParams[] = [];
    mount({}, (p) => changes.push(p));
    await connect();

    fireEvent.click(await screen.findByRole("button", { name: "Services" }));
    const list = await screen.findByRole("listbox");
    const items = within(list).getAllByRole("option");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringMatching(/api.*1\/2.*Degraded/),
      expect.stringMatching(/web.*2\/2.*Running/),
    ]);
    expect(screen.getByPlaceholderText("Find a service")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Clear service filter" }).hasAttribute("disabled"),
    ).toBe(true);

    fireEvent.click(items[1] as HTMLElement);
    expect(changes.at(-1)?.service).toBe("web");
  });

  it("enables Clear service filter once a service is chosen and clears it", async () => {
    const changes: LogViewParams[] = [];
    mount({ service: "web" }, (p) => changes.push(p));
    await connect();

    fireEvent.click(await screen.findByRole("button", { name: "Services (1)" }));
    const clear = await screen.findByRole("button", { name: "Clear service filter" });
    expect(clear.hasAttribute("disabled")).toBe(false);
    fireEvent.click(clear);
    expect(changes.at(-1)?.service).toBeUndefined();
  });

  it("lists the stack's tasks of every service by name and node, and sets the task filter", async () => {
    resourceTasks = [
      swarmTask("tttttttt-web-2", "web", 2, "mgr-2"),
      swarmTask("tttttttt-api-1", "api", 1, "mgr-1"),
    ];
    const changes: LogViewParams[] = [];
    mount({}, (p) => changes.push(p));
    await connect();

    fireEvent.click(await screen.findByRole("button", { name: "Tasks" }));
    const items = await within(await screen.findByRole("listbox")).findAllByRole("option");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringContaining("api.1"),
      expect.stringContaining("web.2"),
    ]);
    expect(items[1]?.textContent).toContain("mgr-2");
    fireEvent.click(items[1] as HTMLElement);
    expect(changes.at(-1)?.task).toBe("tttttttt-web-2");
  });
});

describe("toolbar row 3 of a standalone stack", () => {
  it("has Services, Containers and Include stopped, and no Tasks or Nodes", async () => {
    standalone();
    mount();
    await connect();

    expect(await screen.findByRole("button", { name: "Services" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Containers" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Include stopped" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Tasks" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Nodes" })).toBeNull();
  });

  it("lists the containers by name, the running ones first, and sets the container filter", async () => {
    standalone();
    const changes: LogViewParams[] = [];
    mount({}, (p) => changes.push(p));
    await connect();

    fireEvent.click(await screen.findByRole("button", { name: "Containers" }));
    const items = await within(await screen.findByRole("listbox")).findAllByRole("option");
    // The stopped one is offered only once stopped is included.
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringContaining("solo-db-1"),
      expect.stringContaining("solo-web-1"),
    ]);
    fireEvent.click(items[1] as HTMLElement);
    expect(changes.at(-1)?.container).toBe("cweb1");
  });

  it("says how many containers it follows", async () => {
    standalone();
    mount();
    const s = await opened();
    await act(async () => {
      s.open();
      s.sendFrame("ready", {
        tty: false,
        target: "standalone",
        tasks: [
          { id: "cweb1", service: "web", slot: 1, node: "", state: "running" },
          { id: "cdb1", service: "db", slot: 1, node: "", state: "running" },
          { id: "cweb2", service: "web", slot: 2, node: "", state: "exited" },
        ],
      });
    });

    expect(await screen.findByText("Following 2 containers")).toBeTruthy();
  });

  it("starts every line with service-number", async () => {
    standalone();
    mount();
    const s = await connect();
    await send(s, [line("web", 2, "", "hello", 1), line("db", 1, "", "ready", 2)]);

    const prefixes = [...screen.getByRole("log").querySelectorAll("[data-task-prefix]")];
    expect(prefixes.map((p) => p.textContent)).toEqual(["web-2", "db-1"]);
  });

  it("puts a row between the lines where a deploy replaced a container", async () => {
    standalone();
    mount();
    const s = await connect();
    await send(s, [line("web", 1, "", "before", 1)]);
    await act(async () => {
      s.sendFrame("marker", {
        kind: "replaced",
        service: "web",
        unit: "container",
        slot: 1,
        old: "cweb1-0123456789",
        new: "cweb1n-9876543210",
        at: "2026-10-20T12:00:02.000000000Z",
      });
    });
    await send(s, [line("web", 1, "", "after", 3)]);

    expect(
      screen.getByRole("separator", { name: "— web replaced: container cweb1-01 → cweb1n-9 —" }),
    ).toBeTruthy();
  });
});

describe("the follow cap", () => {
  it("says how many tasks the stack has and how many are followed", async () => {
    mount();
    const s = await opened();
    await act(async () => {
      s.open();
      s.sendFrame("ready", { tty: false, target: "swarm", tasks: [] });
      s.sendFrame("cap", { count: 50, cap: 32, unit: "tasks" });
    });

    expect(
      await screen.findByText(
        "This stack has 50 tasks. TierPlan follows 32 at a time. Filter by service, node or task to see the others.",
      ),
    ).toBeTruthy();
  });

  it("says containers for a standalone stack", async () => {
    standalone();
    mount();
    const s = await opened();
    await act(async () => {
      s.open();
      s.sendFrame("ready", { tty: false, target: "standalone", tasks: [] });
      s.sendFrame("cap", { count: 40, cap: 32, unit: "containers" });
    });

    expect(
      await screen.findByText(
        "This stack has 40 containers. TierPlan follows 32 at a time. Filter by service or container to see the others.",
      ),
    ).toBeTruthy();
  });
});

describe("a stack with nothing to read", () => {
  it("says a stopped stack isn't running and how to start it, and opens no stream", async () => {
    detail = { ...detail, state: "stopped", stopped: true };
    mount();

    expect(await screen.findByText("tracer isn't running")).toBeTruthy();
    expect(screen.getByText("Start it to see its logs.")).toBeTruthy();
    expect(screen.queryByRole("log")).toBeNull();
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("says a stack that was never deployed isn't running and how to deploy it, and opens no stream", async () => {
    detail = { ...detail, state: "not_deployed", lastDeployedAt: "" };
    mount();

    expect(await screen.findByText("tracer isn't running")).toBeTruthy();
    expect(screen.getByText("Deploy it to see its logs.")).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("opens the stream again as soon as the stack runs", async () => {
    detail = { ...detail, state: "stopped", stopped: true };
    mount();
    await screen.findByText("tracer isn't running");

    detail = { ...detail, state: "running", stopped: false };
    await act(async () => {
      await client.invalidateQueries();
    });

    expect(await screen.findByRole("log", { name: "Logs for tracer" })).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});
