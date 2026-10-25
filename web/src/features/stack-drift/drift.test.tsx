// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type DriftView, driftKey, type StackDrift } from "@/api/stack-drift";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DriftPage } from "@/features/stack-drift/drift-page";
import { DriftServiceList } from "@/features/stack-drift/drift-service-list";
import { DriftSummary } from "@/features/stack-drift/drift-summary";
import { StackDetailsCard } from "@/features/stacks/stack-details-card";
import { StackOverview } from "@/features/stacks/stack-overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let detail: Json;
// What the drift route answers: the newest check, and a fresh one when it is asked for.
let cached: Json | "fail";
let fresh: Json | "fail";
let client: QueryClient;
let driftCalls: { fresh: boolean }[];
// When set, the fresh check waits for it: the page is then looking at a check that has not come back.
let holdFresh: Promise<void> | null;

function changed(short: string, over: Json = {}): Json {
  return {
    name: `tracer_${short}`,
    short,
    change: "changed",
    current: { Mode: { Replicated: { Replicas: 1 } } },
    next: { Mode: { Replicated: { Replicas: 3 } } },
    hiddenChanges: [],
    settings: 1,
    ...over,
  };
}

function drift(over: Json = {}): Json {
  return {
    state: "drifted",
    revision: 4,
    deployedAt: "2026-10-19T11:00:00Z",
    deployedBy: "bob",
    checkedAt: "2026-10-19T12:00:00Z",
    count: 1,
    services: [changed("web")],
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
    deployedRevision: 4,
    rollback: { available: false, deployedAt: "", deployedBy: "" },
  };
  cached = drift();
  fresh = drift();
  driftCalls = [];
  holdFresh = null;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = url.pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/services`) return Response.json({ items: [] });
      if (path === `/api/env/${ENV}/stacks/s1/drift`) {
        const isFresh = url.searchParams.get("fresh") === "true";
        driftCalls.push({ fresh: isFresh });
        if (isFresh && holdFresh) await holdFresh;
        const answer = isFresh ? fresh : cached;
        if (answer === "fail") {
          return Response.json(
            { type: "env_offline", title: "Bad Gateway", status: 502, detail: "down" },
            { status: 502 },
          );
        }
        return Response.json(answer);
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

function mount(ui: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>{ui}</TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("a service scaled outside the stack", () => {
  it("shows on the Overview as a status that links to the drift page", async () => {
    mount(<StackOverview envId={ENV} stackId="s1" />);

    const title = await screen.findByText("tracer has drifted from revision #4");
    const box = title.closest('[data-slot="drift-alert"]') as HTMLElement;
    expect(box.getAttribute("role")).toBe("status");
    expect(within(box).getByText("1 setting differs from what was deployed.")).toBeTruthy();
    expect(within(box).getByRole("link", { name: "View drift" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/drift",
    );
  });

  it("is checked afresh once when the Overview opens, and the fresh answer is the one shown", async () => {
    cached = drift({ count: 1 });
    fresh = drift({ count: 5 });
    mount(<StackOverview envId={ENV} stackId="s1" />);

    expect(await screen.findByText("5 settings differ from what was deployed.")).toBeTruthy();
    expect(driftCalls.filter((c) => c.fresh)).toHaveLength(1);
  });

  it("lists on the drift page what drifted, under the title, the revision it is compared with and the intro", async () => {
    fresh = drift({ services: [changed("web"), changed("api", { change: "missing" })], count: 2 });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByRole("heading", { name: "Drift in tracer" })).toBeTruthy();
    expect(screen.getByText(/compared with revision #4, deployed/)).toBeTruthy();
    expect(screen.getByText(/^Bayplan never fixes drift on its own/)).toBeTruthy();
    const list = await screen.findByRole("list", { name: "Services that drifted" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(within(list).getByText("web")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Check again" })).toBeTruthy();
    await waitFor(() => expect(driftCalls.some((c) => c.fresh)).toBe(true));
  });
});

function service(short: string, change: string, over: Json = {}): StackDrift["services"] {
  return [changed(short, { change, ...over })] as unknown as StackDrift["services"];
}

const SERVICES = [
  ...(service("web", "changed", { settings: 2 }) ?? []),
  ...(service("api", "changed", {
    current: { TaskTemplate: { ContainerSpec: { Env: ["X=1"] } } },
    next: { TaskTemplate: { ContainerSpec: { Env: [] } } },
    settings: 1,
  }) ?? []),
  ...(service("db", "missing", { current: null, settings: 0 }) ?? []),
  ...(service("cache", "extra", { next: null, settings: 0 }) ?? []),
];

describe("the summary of drift", () => {
  it("is a native table with a caption, the Services row and a column each for Changed, Missing and Extra", () => {
    mount(<DriftSummary services={SERVICES ?? []} target="swarm" />);

    const table = screen.getByRole("table", { name: "Summary of drift" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["", "Changed", "Missing", "Extra"]);
    const row = within(table).getByRole("row", { name: /Services/ });
    expect(
      within(row)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["2", "1", "1"]);
  });

  it("counts containers and not services for a standalone host", () => {
    mount(<DriftSummary services={SERVICES ?? []} target="standalone" />);

    const table = screen.getByRole("table", { name: "Summary of drift" });
    expect(within(table).getByRole("rowheader", { name: "Containers" })).toBeTruthy();
    expect(within(table).queryByRole("rowheader", { name: "Services" })).toBeNull();
  });
});

describe("the per-service list", () => {
  it("opens a changed service to a SpecDiff of Setting, Deployed and Running now when three or fewer changed", () => {
    mount(<DriftServiceList services={(SERVICES ?? []).slice(0, 2)} />);

    const web = screen.getByRole("table", { name: "Drift in web" });
    expect(
      within(web)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Change", "Setting", "Deployed", "Running now"]);
    const replicas = within(web).getByRole("row", { name: /Replicas/ });
    const cells = within(replicas).getAllByRole("cell");
    expect(cells.at(-2)?.textContent).toBe("1");
    expect(cells.at(-1)?.textContent).toBe("3");
    expect(screen.getByRole("table", { name: "Drift in api" })).toBeTruthy();
  });

  it("names each row by its change in words with the service in mono and how many settings differ", () => {
    mount(<DriftServiceList services={SERVICES ?? []} />);

    const items = screen.getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringContaining("Changedweb2 settings differ"),
      expect.stringContaining("Changedapi1 setting differs"),
      expect.stringContaining("Missingdb"),
      expect.stringContaining("Extracache"),
    ]);
    expect(within(items[0] as HTMLElement).getByText("web").className).toContain("font-mono");
  });

  it("gives a missing or an extra service a summary row and nothing to open", () => {
    mount(<DriftServiceList services={(SERVICES ?? []).slice(2)} />);

    expect(screen.queryAllByRole("button")).toHaveLength(0);
    expect(screen.queryAllByRole("table")).toHaveLength(0);
    expect(screen.getByText("Missing")).toBeTruthy();
    expect(screen.getByText("Extra")).toBeTruthy();
  });

  it("keeps the rows closed past three changed services and opens one when asked", () => {
    const many = ["a", "b", "c", "d"].flatMap((n) => service(n, "changed") ?? []);
    mount(<DriftServiceList services={many} />);

    const triggers = screen.getAllByRole("button");
    expect(triggers.map((b) => b.getAttribute("aria-expanded"))).toEqual([
      "false",
      "false",
      "false",
      "false",
    ]);
    expect(screen.queryAllByRole("table")).toHaveLength(0);
    fireEvent.click(triggers[0] as HTMLElement);
    expect(screen.getByRole("table", { name: "Drift in a" })).toBeTruthy();
  });

  it("lists a setting that changed to or from a hidden value by its path alone", () => {
    const hidden = service("web", "changed", {
      current: { Mode: { Replicated: { Replicas: 1 } } },
      next: { Mode: { Replicated: { Replicas: 1 } } },
      hiddenChanges: ["TaskTemplate.ContainerSpec.Env[API_KEY]"],
    });
    mount(<DriftServiceList services={hidden ?? []} />);

    expect(screen.getByText("TaskTemplate.ContainerSpec.Env[API_KEY]")).toBeTruthy();
  });
});

function view(over: Partial<DriftView> = {}): DriftView {
  return {
    data: undefined,
    checking: false,
    failed: false,
    checkedAt: 0,
    checks: 0,
    recheck: () => {},
    ...over,
  };
}

function stackDetail(over: Json = {}): StackDetail {
  return { ...detail, ...over } as unknown as StackDetail;
}

describe("the Details card", () => {
  it("links the deployed revision to the History tab with it selected, with when and by whom", async () => {
    mount(<StackDetailsCard stack={stackDetail()} drift={view()} />);

    const row = (await screen.findByText("Deployed revision")).nextElementSibling as HTMLElement;
    expect(within(row).getByRole("link", { name: "#4" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/history?rev=4",
    );
    expect(row.textContent).toContain("by bob");
  });

  it("says Never for a stack with no revision that deployed", async () => {
    mount(<StackDetailsCard stack={stackDetail({ deployedRevision: undefined })} drift={view()} />);

    const row = (await screen.findByText("Deployed revision")).nextElementSibling as HTMLElement;
    expect(row.textContent).toBe("Never");
  });

  it.each([
    ["is being checked", view({ checking: true }), "Checking…"],
    [
      "found none",
      view({ data: drift({ state: "none", count: 0, services: [] }) as unknown as StackDrift }),
      "None",
    ],
    ["could not be checked", view({ failed: true }), "Couldn't check"],
    [
      "could not be compared",
      view({ data: drift({ state: "unknown" }) as unknown as StackDrift }),
      "Couldn't check",
    ],
  ])("reads the drift of a stack that %s", async (_name, v, text) => {
    mount(<StackDetailsCard stack={stackDetail()} drift={v} />);

    const row = (await screen.findByText("Drift")).nextElementSibling as HTMLElement;
    expect(row.textContent).toBe(text);
  });

  it("links the number of differences to the drift page", async () => {
    mount(
      <StackDetailsCard
        stack={stackDetail()}
        drift={view({ data: drift({ count: 3 }) as unknown as StackDrift })}
      />,
    );

    const row = (await screen.findByText("Drift")).nextElementSibling as HTMLElement;
    expect(within(row).getByRole("link", { name: "3 differences" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/drift",
    );
  });

  it("reads the stack's own summary of the newest check when the page has not asked", async () => {
    mount(
      <StackDetailsCard
        stack={stackDetail({ drift: { state: "drifted", revision: 4, count: 1, checkedAt: "" } })}
      />,
    );

    const row = (await screen.findByText("Drift")).nextElementSibling as HTMLElement;
    expect(within(row).getByRole("link", { name: "1 difference" })).toBeTruthy();
  });
});

describe("Check again", () => {
  it("announces when the check has finished, politely, and not for the check that opened the page", async () => {
    mount(<DriftPage envId={ENV} stackId="s1" />);
    await screen.findByRole("list", { name: "Services that drifted" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Check again" })).toBeTruthy());
    // The live region is there from the start and says nothing until a check that was asked for has ended.
    expect(screen.getByRole("status").textContent).toBe("");
    const before = driftCalls.filter((c) => c.fresh).length;

    fireEvent.click(screen.getByRole("button", { name: "Check again" }));

    const live = screen.getByRole("status");
    await waitFor(() => expect(live.textContent).toMatch(/^Checked /));
    expect(live.getAttribute("aria-live")).toBe("polite");
    expect(driftCalls.filter((c) => c.fresh).length).toBe(before + 1);
  });
});

// The states the drift page has for a stack (UI-SPEC "Drift page"): populated by how many services drifted, empty,
// loading, failed, stopped, not deployed, and a value too long for its column.
function drifted(n: number): Json {
  const services = Array.from({ length: n }, (_, i) => changed(`svc${i}`));
  return drift({ services, count: n });
}

describe("the drift page", () => {
  it.each([
    [1, 1],
    [3, 3],
  ])("opens every service when %i drifted", async (n, tables) => {
    fresh = drifted(n);
    mount(<DriftPage envId={ENV} stackId="s1" />);

    const list = await screen.findByRole("list", { name: "Services that drifted" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(n);
    expect(within(list).getAllByRole("table")).toHaveLength(tables);
  });

  it("keeps 30 drifted services closed and counts them in the summary", async () => {
    fresh = drifted(30);
    mount(<DriftPage envId={ENV} stackId="s1" />);

    const list = await screen.findByRole("list", { name: "Services that drifted" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(30);
    expect(within(list).queryAllByRole("table")).toHaveLength(0);
    for (const b of within(list).getAllByRole("button")) {
      expect(b.getAttribute("aria-expanded")).toBe("false");
    }
    const summary = screen.getByRole("table", { name: "Summary of drift" });
    expect(
      within(within(summary).getByRole("row", { name: /Services/ })).getAllByRole("cell")[0]
        ?.textContent,
    ).toBe("30");
  });

  it("says there is no drift when what runs matches the revision", async () => {
    fresh = drift({ state: "none", count: 0, services: [] });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("No drift")).toBeTruthy();
    expect(screen.getByText("What's running matches revision #4.")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Summary of drift" })).toBeNull();
    expect(screen.queryByRole("list", { name: "Services that drifted" })).toBeNull();
  });

  it("shows a skeleton and says it is checking while nothing is known", async () => {
    cached = "fail";
    holdFresh = new Promise(() => {});
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("Checking for drift…")).toBeTruthy();
    expect(screen.getByTestId("drift-loading").getAttribute("aria-busy")).toBe("true");
    expect(screen.queryByText("No drift")).toBeNull();
    expect((screen.getByRole("button", { name: "Checking…" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it("shows the newest check the server holds while a fresh one runs", async () => {
    cached = drifted(2);
    holdFresh = new Promise(() => {});
    mount(<DriftPage envId={ENV} stackId="s1" />);

    const list = await screen.findByRole("list", { name: "Services that drifted" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Checking…" })).toBeTruthy();
    expect(screen.queryByText("Checking for drift…")).toBeNull();
  });

  it("says it could not check, and checks again when asked", async () => {
    cached = "fail";
    fresh = "fail";
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("Couldn't check tracer for drift.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    fresh = drift({ state: "none", count: 0, services: [] });
    fireEvent.click(screen.getByRole("button", { name: "Retry check" }));

    expect(await screen.findByText("No drift")).toBeTruthy();
    expect(screen.queryByText("Couldn't check tracer for drift.")).toBeNull();
  });

  it("keeps what it knew and says so when the fresh check fails", async () => {
    cached = drifted(1);
    fresh = "fail";
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("Couldn't check tracer for drift.")).toBeTruthy();
    expect(screen.getByRole("list", { name: "Services that drifted" })).toBeTruthy();
  });

  it("says a revision that cannot be compared could not be", async () => {
    fresh = drift({ state: "unknown", count: 0, services: [] });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("Couldn't compare tracer with revision #4.")).toBeTruthy();
    expect(screen.queryByText("No drift")).toBeNull();
  });

  it("says a stopped stack does not drift", async () => {
    fresh = drift({ state: "stopped", count: 0, services: [] });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("Stopped stacks don't drift.")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Summary of drift" })).toBeNull();
    expect(screen.queryByText("No drift")).toBeNull();
  });

  it("shows the Not deployed yet state for a stack with no revision", async () => {
    detail = { ...detail, state: "not_deployed", deployedRevision: undefined };
    fresh = drift({ state: "not_deployed", revision: 0, count: 0, services: [], deployedAt: "" });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByText("Not deployed yet")).toBeTruthy();
    expect(screen.queryByText("No drift")).toBeNull();
  });

  it("wraps a long value in mono instead of widening the page", async () => {
    const long = `registry.example.com/${"x".repeat(300)}:1`;
    fresh = drift({
      services: [
        changed("web", {
          current: { TaskTemplate: { ContainerSpec: { Image: long } } },
          next: { TaskTemplate: { ContainerSpec: { Image: "nginx:1" } } },
        }),
      ],
    });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    const cell = await screen.findByText(long);
    expect(cell.className).toContain("break-all");
    expect(cell.className).toContain("font-mono");
  });
});

describe("the Overview's drift alert", () => {
  it("is not drawn for a stopped stack, one never deployed, or one that does not drift", async () => {
    for (const state of ["stopped", "not_deployed", "none", "unknown"]) {
      cleanup();
      client.clear();
      fresh = drift({ state, count: 0, services: [] });
      mount(<StackOverview envId={ENV} stackId="s1" />);
      await screen.findByText("Details");
      await waitFor(() => expect(driftCalls.some((c) => c.fresh)).toBe(true));
      expect(document.querySelector('[data-slot="drift-alert"]')).toBeNull();
    }
  });

  it("appears and disappears without moving focus", async () => {
    fresh = drift({ state: "none", count: 0, services: [] });
    mount(<StackOverview envId={ENV} stackId="s1" />);
    const link = await screen.findByRole("link", { name: "Prod" });
    link.focus();
    expect(document.activeElement).toBe(link);
    const freshKey = [...driftKey(ENV, "s1"), "fresh"];
    // The check the Overview made when it opened has come back, so it cannot overwrite what is set next.
    await waitFor(() => {
      const state = client.getQueryState(freshKey);
      expect(state?.fetchStatus).toBe("idle");
      expect(state?.status).toBe("success");
    });

    act(() => client.setQueryData(freshKey, drift()));
    expect(await screen.findByText("tracer has drifted from revision #4")).toBeTruthy();
    expect(document.activeElement).toBe(link);

    act(() => client.setQueryData(freshKey, drift({ state: "none", count: 0, services: [] })));
    await waitFor(() => expect(document.querySelector('[data-slot="drift-alert"]')).toBeNull());
    expect(document.activeElement).toBe(link);
  });
});
