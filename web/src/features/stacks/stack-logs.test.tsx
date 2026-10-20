// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
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
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/services`) return Response.json({ items: services });
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

function mount(params: LogViewParams = {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackLogs envId={ENV} stackId="s1" params={params} onParamsChange={() => {}} />
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
