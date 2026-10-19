// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  STACK_SERVICES_POLL_BUSY_MS,
  STACK_SERVICES_POLL_MS,
  stackServicesQuery,
} from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackOverview } from "@/features/stacks/stack-overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const DIGEST = `sha256:${"a".repeat(64)}`;
const IMAGE_ID = `sha256:${"c".repeat(64)}`;

type Json = Record<string, unknown>;

let detail: Json;
let services: Json | "fail";
let client: QueryClient;
let servicesCalls: number;

function swarmService(over: Json = {}): Json {
  return {
    id: "svc-1",
    name: "tracer_web",
    short: "web",
    mode: "replicated",
    state: "running",
    updateState: "",
    running: 2,
    desired: 3,
    image: "nginx:1.27",
    pinned: `nginx:1.27@${DIGEST}`,
    pinState: "pinned",
    ports: ["8080->80/tcp"],
    containers: [],
    ...over,
  };
}

beforeEach(() => {
  detail = {
    id: "s1",
    name: "tracer",
    target: "swarm",
    source: "inline",
    state: "deployed",
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
  services = { items: [swarmService()] };
  servicesCalls = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/services`) {
        servicesCalls++;
        if (services === "fail") {
          return Response.json(
            { type: "env_offline", title: "Bad Gateway", status: 502, detail: "down" },
            { status: 502 },
          );
        }
        return Response.json(services);
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
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackOverview envId={ENV} stackId="s1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("StackOverview services card on a swarm", () => {
  it("lists each service with its state, replicas, image, digest and ports, the name linking to the service", async () => {
    mount();

    const card = await screen.findByRole("region", { name: "Services" });
    const table = await within(card).findByRole("table");
    const headers = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(headers).toEqual(["Service", "State", "Replicas", "Image", "Ports"]);
    const link = within(table).getByRole("link", { name: "web" });
    expect(link.getAttribute("href")).toBe("/env/e1/services/svc-1");
    expect(within(table).getByText("2/3")).toBeTruthy();
    expect(within(table).getByText("nginx:1.27")).toBeTruthy();
    expect(within(table).getByRole("button", { name: /^Pinned to sha256:a{64}/ })).toBeTruthy();
    expect(within(table).getByText("8080->80/tcp")).toBeTruthy();
  });

  it("reads the state of a swarm service with the service list's own words", async () => {
    services = {
      items: [
        swarmService({ id: "a", name: "tracer_a", short: "a", running: 1, desired: 3 }),
        swarmService({ id: "b", name: "tracer_b", short: "b", running: 3, desired: 3 }),
        swarmService({ id: "c", name: "tracer_c", short: "c", running: 0, desired: 3 }),
      ],
    };
    mount();

    const table = await screen.findByRole("table");
    expect(within(table).getByText("Degraded")).toBeTruthy();
    expect(within(table).getByText("Running")).toBeTruthy();
    expect(within(table).getByText("Not running")).toBeTruthy();
  });

  it("marks an image with no digest New and an image from no registry with the warning in words", async () => {
    services = {
      items: [
        swarmService({
          id: "a",
          name: "tracer_a",
          short: "a",
          pinned: "nginx:1.27",
          pinState: "new",
        }),
        swarmService({
          id: "b",
          name: "tracer_b",
          short: "b",
          image: "built:local",
          pinned: IMAGE_ID,
          pinState: "local",
        }),
      ],
    };
    mount();

    const table = await screen.findByRole("table");
    expect(within(table).getByText("New")).toBeTruthy();
    expect(within(table).getByText("Not from a registry")).toBeTruthy();
    // A pinned chip is for a pinned image only.
    expect(within(table).queryByRole("button", { name: /^Pinned to/ })).toBeNull();
  });

  it("draws Docker's text as text", async () => {
    services = {
      items: [swarmService({ image: "<img src=x onerror=alert(1)>", ports: ["<b>1</b>->2/tcp"] })],
    };
    mount();

    const table = await screen.findByRole("table");
    expect(within(table).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(table.querySelector("img")).toBeNull();
    expect(table.querySelector("b")).toBeNull();
  });

  it("says nothing is running for a deployed stack with no service", async () => {
    services = { items: [] };
    mount();

    const card = await screen.findByRole("region", { name: "Services" });
    expect(await within(card).findByText("Nothing is running.")).toBeTruthy();
    expect(within(card).queryByRole("table")).toBeNull();
  });
});

describe("StackOverview services card on a standalone host", () => {
  beforeEach(() => {
    detail = { ...detail, target: "standalone" };
    services = {
      items: [
        {
          id: "",
          name: "web",
          short: "web",
          mode: "",
          state: "degraded",
          updateState: "",
          running: 1,
          desired: 2,
          image: "nginx:1.27",
          pinned: `nginx:1.27@${DIGEST}`,
          pinState: "pinned",
          ports: [],
          containers: [
            { id: "c1", name: "tracer-web-1", state: "running", health: "none" },
            { id: "c2", name: "tracer-web-2", state: "exited", health: "none" },
          ],
        },
      ],
    };
  });

  it("rolls the containers up under the service and opens them on request, each linking to its container", async () => {
    mount();

    const table = await screen.findByRole("table");
    expect(within(table).getByRole("columnheader", { name: "Containers" })).toBeTruthy();
    expect(within(table).getByText("1/2")).toBeTruthy();
    expect(within(table).getByText("Degraded")).toBeTruthy();
    expect(within(table).queryByRole("link", { name: "tracer-web-1" })).toBeNull();

    const toggle = within(table).getByRole("button", { name: "Show containers for web" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);

    expect(
      within(table)
        .getByRole("button", { name: "Hide containers for web" })
        .getAttribute("aria-expanded"),
    ).toBe("true");
    expect(within(table).getByRole("link", { name: "tracer-web-1" }).getAttribute("href")).toBe(
      "/env/e1/containers/c1",
    );
    expect(within(table).getByRole("link", { name: "tracer-web-2" }).getAttribute("href")).toBe(
      "/env/e1/containers/c2",
    );
    expect(within(table).getByText("Exited")).toBeTruthy();
  });
});

describe("StackOverview when the engine cannot be read", () => {
  it("says so in the Services card with its own retry line while the Details still render", async () => {
    services = "fail";
    mount();

    expect(await screen.findByText("Couldn't read what tracer is running.")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Details" })).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();

    services = { items: [swarmService()] };
    fireEvent.click(screen.getByRole("button", { name: "Retry reading services" }));

    expect(await screen.findByRole("table")).toBeTruthy();
    expect(screen.queryByText("Couldn't read what tracer is running.")).toBeNull();
  });
});

describe("stackServicesQuery", () => {
  function interval(target: "swarm" | "standalone", busy: boolean): number | false {
    const fn = stackServicesQuery(ENV, "s1", { target, busy }).refetchInterval as () =>
      | number
      | false;
    return fn();
  }

  it("polls a swarm every 5 s, every 2 s during an operation, and a standalone host at the lists' pace", () => {
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");

    expect(interval("swarm", false)).toBe(STACK_SERVICES_POLL_MS);
    expect(interval("swarm", true)).toBe(STACK_SERVICES_POLL_BUSY_MS);
    expect(STACK_SERVICES_POLL_MS).toBe(5_000);
    expect(STACK_SERVICES_POLL_BUSY_MS).toBe(2_000);
    expect(interval("standalone", false)).toBe(30_000);
  });

  it("stops polling while the page is hidden", () => {
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");

    expect(interval("swarm", false)).toBe(false);
    expect(interval("swarm", true)).toBe(false);
    expect(interval("standalone", false)).toBe(false);
  });

  it("reads the services once on mount", async () => {
    mount();
    await screen.findByRole("table");
    expect(servicesCalls).toBe(1);
  });
});
