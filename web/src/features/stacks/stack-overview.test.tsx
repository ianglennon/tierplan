// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  STACK_SERVICES_POLL_BUSY_MS,
  STACK_SERVICES_POLL_MS,
  type StackDetail,
  stackQuery,
  stackServicesQuery,
} from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DeployBanner } from "@/features/stacks/deploy-banner";
import { DraftBanner } from "@/features/stacks/draft-banner";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import { StackOverview } from "@/features/stacks/stack-overview";
import { StackStateCell } from "@/features/stacks/stack-state-cell";
import { StoppedAlert } from "@/features/stacks/stopped-alert";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const DIGEST = `sha256:${"a".repeat(64)}`;
const IMAGE_ID = `sha256:${"c".repeat(64)}`;

type Json = Record<string, unknown>;

let role: "viewer" | "operator";
let detail: Json;
let job: Json;
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
  services = { items: [swarmService()] };
  role = "operator";
  job = {
    id: "j1",
    stackId: "s1",
    mode: "deploy",
    state: "running",
    createdAt: "2026-10-19T12:00:00Z",
    finishedAt: "",
    startedBy: "ada",
    items: [
      { id: "service:a", kind: "service", state: "done", label: "a", current: 1, total: 1 },
      { id: "service:b", kind: "service", state: "working", label: "b", current: 0, total: 1 },
    ],
    output: [],
  };
  servicesCalls = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/deploys/j1`) return Response.json(job);
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

function stackOf(over: Partial<StackDetail> = {}): StackDetail {
  return {
    ...(detail as unknown as StackDetail),
    rollback: { available: false, deployedAt: "", deployedBy: "" },
    draftSavedAt: "",
    draftSavedBy: "",
    ...over,
  };
}

function operation(over: Partial<NonNullable<StackDetail["lastOperation"]>> = {}) {
  return {
    kind: "deploy",
    state: "running",
    code: "",
    failedService: "",
    jobId: "j1",
    startedBy: "ada",
    startedAt: "2026-10-19T12:00:00Z",
    endedAt: "",
    ...over,
  } as NonNullable<StackDetail["lastOperation"]>;
}

function mountLayout() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackDetailLayout envId={ENV} stackId="s1">
            <StackOverview envId={ENV} stackId="s1" />
          </StackDetailLayout>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("StackStateCell", () => {
  function cell(props: Parameters<typeof StackStateCell>[0]) {
    render(
      <TestRouter>
        <TooltipProvider>
          <StackStateCell {...props} />
        </TooltipProvider>
      </TestRouter>,
    );
  }

  it("words every state", () => {
    const words: Record<string, string> = {
      removing: "Removing",
      deploying: "Deploying",
      rolling_back: "Rolling back",
      starting: "Starting",
      stopped: "Stopped",
      deploy_failed: "Deploy failed",
      not_running: "Not running",
      degraded: "Degraded",
      running: "Running",
      not_deployed: "Not deployed",
      unknown: "Unknown",
    };
    for (const [state, word] of Object.entries(words)) {
      cell({ state: state as StackDetail["state"] });
      expect(screen.getByText(word)).toBeTruthy();
      cleanup();
    }
  });

  it("says in a Tooltip how many services run when the stack is Degraded", async () => {
    cell({ state: "degraded", running: 2, total: 3 });
    fireEvent.focus(screen.getByText("Degraded"));
    fireEvent.pointerMove(screen.getByText("Degraded"));
    expect((await screen.findAllByText("2 of 3 services running")).length).toBeGreaterThan(0);
  });

  it("says in a Tooltip where the last deploy stopped when it failed", async () => {
    cell({ state: "deploy_failed", failedService: "web_api" });
    fireEvent.focus(screen.getByText("Deploy failed"));
    fireEvent.pointerMove(screen.getByText("Deploy failed"));
    expect(
      (
        await screen.findAllByText(
          "The last deploy stopped at web_api. What is running is shown below.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });

  it("pulses the dot of an operation under way and not the dot of Degraded", () => {
    cell({ state: "deploying" });
    expect(document.querySelector(".animate-pulse")).not.toBeNull();
    cleanup();
    cell({ state: "degraded" });
    expect(document.querySelector(".animate-pulse")).toBeNull();
  });
});

describe("StackOverview details", () => {
  it("lists the details of a deployed stack with its last deploy and its draft", async () => {
    detail = {
      ...detail,
      hasDraft: true,
      draftSavedAt: "2026-10-19T13:00:00Z",
      draftSavedBy: "carol",
    };
    mount();

    const card = await screen.findByRole("region", { name: "Details" });
    const row = (label: string) => within(card).getByText(label).nextElementSibling as HTMLElement;
    expect(await within(row("Environment")).findByRole("link", { name: "Prod" })).toBeTruthy();
    expect(row("Target").textContent).toBe("Swarm");
    expect(row("Source").textContent).toBe("Inline");
    expect(row("Created").textContent).toContain("by ada");
    expect(row("Last deployed").textContent).toContain("by bob");
    expect(row("Draft").textContent).toMatch(/^Saved .* by carol$/);
    expect(row("Stack ID").textContent).toContain("s1");
  });

  it("reads Standalone host, Never and No draft where they apply", async () => {
    detail = { ...detail, target: "standalone", lastDeployedAt: "", lastDeployedBy: "" };
    mount();

    const card = await screen.findByRole("region", { name: "Details" });
    const row = (label: string) => within(card).getByText(label).nextElementSibling as HTMLElement;
    expect(row("Target").textContent).toBe("Standalone host");
    expect(row("Last deployed").textContent).toBe("Never");
    expect(row("Draft").textContent).toBe("No draft");
  });

  it("copies the stack ID and says so", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    mount();

    fireEvent.click(await screen.findByRole("button", { name: "Copy the stack ID" }));

    expect(writeText).toHaveBeenCalledWith("s1");
    expect(await screen.findByText("Copied the stack ID")).toBeTruthy();
  });

  it("draws a name and a user with markup in them as text", async () => {
    detail = { ...detail, createdBy: "<b>ada</b>" };
    mount();

    const card = await screen.findByRole("region", { name: "Details" });
    expect(within(card).getByText(/<b>ada<\/b>/)).toBeTruthy();
    expect(card.querySelector("b")).toBeNull();
  });
});

describe("StackOverview of a stack that was never deployed", () => {
  beforeEach(() => {
    detail = {
      ...detail,
      state: "not_deployed",
      lastDeployedAt: "",
      lastDeployedBy: "",
      lastOperation: undefined,
    };
  });

  it("shows Not deployed yet in place of the services, with Open editor and Deploy…, and the Details beside it", async () => {
    mount();

    expect(await screen.findByRole("heading", { name: "Not deployed yet" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open editor" })).toBeTruthy();
    expect((await screen.findByRole("link", { name: "Deploy…" })).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/review",
    );
    expect(screen.getByRole("region", { name: "Details" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Services" })).toBeNull();
    expect(servicesCalls).toBe(0);
  });

  it("offers a Viewer the editor to read and no Deploy…", async () => {
    role = "viewer";
    mount();

    expect(await screen.findByRole("link", { name: "Open editor" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Deploy…" })).toBeNull();
  });
});

describe("StackOverview of a stack with a draft and no deployment", () => {
  it("keeps the services of a stopped stack listed", async () => {
    detail = { ...detail, state: "stopped", stopped: true };
    services = { items: [swarmService({ running: 0, desired: 0, state: "stopped" })] };
    mount();

    expect(await screen.findByRole("table")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Not deployed yet" })).toBeNull();
  });
});

describe("AdoptedBanner on the Overview of an adopted stack", () => {
  beforeEach(() => {
    detail = {
      ...detail,
      source: "adopted",
      servicesRunning: 3,
      servicesTotal: 3,
      adoption: { pending: true, adoptedFrom: "tracer" },
    };
  });

  it("says the stack is adopted but not yet deployed, how many services the first deploy recreates and where to go", async () => {
    mount();

    const title = await screen.findByText("tracer is adopted but not yet deployed by Bayplan");
    const banner = title.closest('[data-slot="adopted-banner"]') as HTMLElement;
    expect(
      within(banner).getByText(
        "The first deploy adds Bayplan's ownership labels and recreates 3 services (brief restart). Review shows exactly what changes.",
      ),
    ).toBeTruthy();
    expect(
      (await within(banner).findByRole("link", { name: "Review and deploy" })).getAttribute("href"),
    ).toBe("/env/e1/stacks/s1/review");
    expect(within(banner).getByRole("link", { name: "Open history" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/history",
    );
  });

  it("counts one service in the singular", async () => {
    detail = { ...detail, servicesTotal: 1, servicesRunning: 1 };
    mount();

    expect(
      await screen.findByText(
        "The first deploy adds Bayplan's ownership labels and recreates 1 service (brief restart). Review shows exactly what changes.",
      ),
    ).toBeTruthy();
  });

  it("counts containers on a standalone host", async () => {
    detail = { ...detail, target: "standalone", servicesTotal: 2, servicesRunning: 2 };
    mount();

    expect(
      await screen.findByText(
        "The first deploy adds Bayplan's ownership labels and recreates 2 containers (brief restart). Review shows exactly what changes.",
      ),
    ).toBeTruthy();
  });

  it("counts one container in the singular on a standalone host", async () => {
    detail = { ...detail, target: "standalone", servicesTotal: 1, servicesRunning: 1 };
    mount();

    expect(
      await screen.findByText(
        "The first deploy adds Bayplan's ownership labels and recreates 1 container (brief restart). Review shows exactly what changes.",
      ),
    ).toBeTruthy();
  });

  it("does not guess a count when the engine could not be read", async () => {
    detail = { ...detail, servicesTotal: undefined, servicesRunning: undefined };
    mount();

    expect(
      await screen.findByText(
        "The first deploy adds Bayplan's ownership labels and recreates its services (brief restart). Review shows exactly what changes.",
      ),
    ).toBeTruthy();
  });

  it("offers a Viewer the history to read and no way to deploy", async () => {
    role = "viewer";
    mount();

    expect(await screen.findByRole("link", { name: "Open history" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Review and deploy" })).toBeNull();
  });

  it("is gone once the first deploy has ended good", async () => {
    detail = { ...detail, adoption: { pending: false, adoptedFrom: "tracer" } };
    mount();

    await screen.findByRole("region", { name: "Services" });
    expect(screen.queryByText(/is adopted but not yet deployed by Bayplan/)).toBeNull();
    expect(screen.queryByRole("link", { name: "Open history" })).toBeNull();
  });

  it("is never on a stack that was not adopted", async () => {
    detail = { ...detail, source: "inline", adoption: undefined };
    mount();

    await screen.findByRole("region", { name: "Services" });
    expect(screen.queryByText(/is adopted but not yet deployed by Bayplan/)).toBeNull();
  });

  it("draws a name with markup in it as text", async () => {
    detail = { ...detail, name: "<b>shop</b>" };
    mount();

    const title = await screen.findByText("<b>shop</b> is adopted but not yet deployed by Bayplan");
    expect(title.querySelector("b")).toBeNull();
  });
});

describe("DeployBanner", () => {
  it("names the operation, counts the services done and opens the console", async () => {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <DeployBanner stack={stackOf({ lastOperation: operation() })} />
        </TestRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Deploying tracer")).toBeTruthy();
    expect(await screen.findByText("1 of 2 services done.")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Deploy progress" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open console" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/deploys/j1",
    );
  });

  it("counts one service in the singular", async () => {
    job = {
      ...job,
      items: [
        { id: "service:a", kind: "service", state: "done", label: "a", current: 1, total: 1 },
      ],
    };
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <DeployBanner stack={stackOf({ lastOperation: operation() })} />
        </TestRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("1 of 1 service done.")).toBeTruthy();
  });

  it("words a roll back, a removal and a start", async () => {
    for (const [kind, title] of [
      ["rollback", "Rolling back tracer"],
      ["remove", "Removing tracer"],
      ["start", "Starting tracer"],
    ] as const) {
      render(
        <QueryClientProvider client={client}>
          <TestRouter>
            <DeployBanner stack={stackOf({ lastOperation: operation({ kind }) })} />
          </TestRouter>
        </QueryClientProvider>,
      );
      expect(await screen.findByText(title)).toBeTruthy();
      cleanup();
    }
  });

  it("shows nothing for a stack no operation is running on", () => {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <DeployBanner
            stack={stackOf({
              lastOperation: operation({ state: "done", endedAt: "2026-10-19T12:01:00Z" }),
            })}
          />
        </TestRouter>
      </QueryClientProvider>,
    );

    expect(screen.queryByText("Deploying tracer")).toBeNull();
    expect(screen.queryByText("Deploy complete")).toBeNull();
  });

  it("says Deploy complete when the operation ends well and collapses 4 s later", async () => {
    const view = (stack: StackDetail) => (
      <QueryClientProvider client={client}>
        <TestRouter>
          <DeployBanner stack={stack} />
        </TestRouter>
      </QueryClientProvider>
    );
    const { rerender } = render(view(stackOf({ lastOperation: operation() })));
    expect(await screen.findByText("Deploying tracer")).toBeTruthy();

    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      rerender(
        view(
          stackOf({ lastOperation: operation({ state: "done", endedAt: "2026-10-19T12:01:00Z" }) }),
        ),
      );
      expect(screen.getByText("Deploy complete")).toBeTruthy();
      expect(screen.queryByText("Deploying tracer")).toBeNull();

      act(() => {
        vi.advanceTimersByTime(3_900);
      });
      expect(screen.getByText("Deploy complete")).toBeTruthy();
      act(() => {
        vi.advanceTimersByTime(200);
      });
      expect(screen.queryByText("Deploy complete")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("collapses at once, with no Deploy complete, when the operation fails", async () => {
    const view = (stack: StackDetail) => (
      <QueryClientProvider client={client}>
        <TestRouter>
          <DeployBanner stack={stack} />
        </TestRouter>
      </QueryClientProvider>
    );
    const { rerender } = render(view(stackOf({ lastOperation: operation() })));
    expect(await screen.findByText("Deploying tracer")).toBeTruthy();

    rerender(
      view(
        stackOf({ lastOperation: operation({ state: "failed", endedAt: "2026-10-19T12:01:00Z" }) }),
      ),
    );

    expect(screen.queryByText("Deploy complete")).toBeNull();
    expect(screen.queryByText("Deploying tracer")).toBeNull();
  });
});

describe("DraftBanner", () => {
  function draft(stack: StackDetail) {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <DraftBanner stack={stack} envName="Prod" />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }
  const pending = () =>
    stackOf({ hasDraft: true, draftSavedAt: "2026-10-19T13:00:00Z", draftSavedBy: "carol" });

  it("says who saved the draft and that nothing has changed on the environment", async () => {
    draft(pending());

    expect(await screen.findByText("Draft saved, not deployed")).toBeTruthy();
    expect(screen.getByText(/carol saved changes/)).toBeTruthy();
    expect(screen.getByText(/Nothing has changed on Prod yet\./)).toBeTruthy();
  });

  it("offers an Operator the editor and the review", async () => {
    draft(pending());

    expect(await screen.findByRole("link", { name: "Open editor" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Review and deploy" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/review",
    );
  });

  it("shows a Viewer the text and no buttons", async () => {
    role = "viewer";
    draft(pending());

    expect(await screen.findByText("Draft saved, not deployed")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Review and deploy" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Open editor" })).toBeNull();
  });

  it("shows nothing when no draft is waiting", () => {
    draft(stackOf({ hasDraft: false }));

    expect(screen.queryByText("Draft saved, not deployed")).toBeNull();
  });
});

describe("StoppedAlert", () => {
  it("says the stack is stopped and offers Start stack once something can start it", async () => {
    const onStart = vi.fn();
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <StoppedAlert stack={stackOf({ stopped: true })} onStart={onStart} />
        </TestRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("tracer is stopped")).toBeTruthy();
    expect(
      screen.getByText(
        "Networks, volumes, secrets and configs are still there. Start brings back the last deployed version.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Start stack" }));
    expect(onStart).toHaveBeenCalledTimes(1);
  });

  it("shows nothing for a stack that is not stopped", () => {
    render(
      <TestRouter>
        <StoppedAlert stack={stackOf({ stopped: false })} onStart={() => {}} />
      </TestRouter>,
    );

    expect(screen.queryByText("tracer is stopped")).toBeNull();
  });

  it("draws no button that does nothing", async () => {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <StoppedAlert stack={stackOf({ stopped: true })} />
        </TestRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("tracer is stopped")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start stack" })).toBeNull();
  });
});

describe("StackDetailLayout alerts", () => {
  it("shows the alerts in the order deploy failed, operation lock, stopped, draft pending", async () => {
    detail = {
      ...detail,
      state: "deploy_failed",
      stopped: true,
      hasDraft: true,
      draftSavedAt: "2026-10-19T13:00:00Z",
      draftSavedBy: "carol",
      lastOperation: operation({
        state: "failed",
        code: "service_rejected",
        failedService: "tracer_web",
      }),
      lock: {
        stackId: "s2",
        stackName: "other",
        jobId: "j9",
        kind: "deploy",
        startedAt: "2026-10-19T12:30:00Z",
        startedBy: "bob",
      },
    };
    mountLayout();

    const failed = await screen.findByText("The last deploy failed");
    const lock = await waitFor(() => {
      const found = document.querySelector("[data-slot=operation-lock]");
      if (!found) throw new Error("no operation lock alert yet");
      return found;
    });
    const stopped = await screen.findByText("tracer is stopped");
    const drafted = await screen.findByText("Draft saved, not deployed");
    const follows = (a: Node, b: Node) =>
      Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(follows(failed, lock)).toBe(true);
    expect(follows(lock, stopped)).toBe(true);
    expect(follows(stopped, drafted)).toBe(true);
  });

  it("shows the deploy banner and the draft banner together and suppresses the stopped alert while a deploy runs", async () => {
    detail = {
      ...detail,
      state: "deploying",
      stopped: true,
      hasDraft: true,
      draftSavedAt: "2026-10-19T13:00:00Z",
      draftSavedBy: "carol",
      lastOperation: operation(),
    };
    mountLayout();

    const banner = await screen.findByText("Deploying tracer");
    const drafted = await screen.findByText("Draft saved, not deployed");
    expect(banner.compareDocumentPosition(drafted) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByText("tracer is stopped")).toBeNull();
  });

  it("says in the header how many services run when the stack is Degraded", async () => {
    detail = { ...detail, state: "degraded", servicesRunning: 2, servicesTotal: 3 };
    mountLayout();

    const dot = (await screen.findAllByText("Degraded"))[0] as HTMLElement;
    fireEvent.focus(dot);
    fireEvent.pointerMove(dot);
    expect((await screen.findAllByText("2 of 3 services running")).length).toBeGreaterThan(0);
  });

  it("names where the last deploy stopped in the Tooltip of Deploy failed", async () => {
    detail = {
      ...detail,
      state: "deploy_failed",
      lastOperation: operation({
        state: "failed",
        code: "service_rejected",
        failedService: "tracer_web",
      }),
    };
    mountLayout();

    const word = (await screen.findAllByText("Deploy failed"))[0] as HTMLElement;
    fireEvent.focus(word);
    fireEvent.pointerMove(word);
    expect(
      (
        await screen.findAllByText(
          "The last deploy stopped at tracer_web. What is running is shown below.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });
});

describe("stackQuery", () => {
  const interval = (data: unknown) =>
    (stackQuery(ENV, "s1").refetchInterval as (q: { state: { data: unknown } }) => number | false)({
      state: { data },
    });

  it("reads the stack every 2 s while an operation runs on it and at the lists' pace otherwise", () => {
    expect(interval({ lastOperation: { state: "running" } })).toBe(2_000);
    expect(interval({ lastOperation: { state: "done" } })).toBe(30_000);
    expect(interval(undefined)).toBe(30_000);
  });
});
