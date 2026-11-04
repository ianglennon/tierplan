// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type ContainerDetail, detailRetry } from "@/api/container-detail";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The container page: header, actions that follow the state, the swarm-task
// banner, and the states of loading, failing and being gone (plan 04-10, D-04,
// D-07, D-10, D-11).

const ENV = "e1";
const PAGE = `/env/${ENV}/containers/web-id?node=n1`;

function detail(extra: Partial<ContainerDetail> = {}): ContainerDetail {
  return {
    nodeId: "n1",
    node: "node-1",
    readOnly: false,
    id: "web-id",
    name: "web",
    image: "nginx:1.27",
    imageId: "sha256:abc",
    created: "2026-10-01T10:00:00Z",
    state: "running",
    health: "none",
    startedAt: "2026-10-01T10:00:05Z",
    exitCode: 0,
    oomKilled: false,
    restartCount: 0,
    command: [],
    entrypoint: [],
    commandMasked: false,
    restartPolicy: "no",
    restartMaxRetries: 0,
    ports: [],
    mounts: [],
    networks: [],
    labels: {},
    env: [],
    logConfig: { type: "json-file", options: [] },
    masked: false,
    isSelf: false,
    selfKind: "",
    project: "",
    stack: "",
    tierplanStackId: "",
    service: "",
    muted: false,
    ...extra,
  };
}

function problem(status: number, type: string) {
  return Response.json({ type, title: "x", status }, { status });
}

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let answer: () => Response | Promise<Response>;
let actionAnswer: () => Response | Promise<Response>;
let inspectAnswer: () => Response | Promise<Response>;
let envType: "swarm" | "standalone";
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "operator" } });
  answer = () => Response.json(detail());
  actionAnswer = () =>
    Response.json({ results: [{ nodeId: "n1", id: "web-id", name: "web", result: "done" }] });
  inspectAnswer = () =>
    Response.json({ masked: true, inspect: { Id: "web-id", Config: { Env: ["MODE=<hidden>"] } } });
  envType = "standalone";
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  detailRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path: url.pathname, body });
      switch (url.pathname) {
        case "/api/instance":
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        case "/api/me":
          return Response.json(me);
        case "/api/version":
          return Response.json({
            commit: "",
            display: "v0.1.0",
            sourceTreeUrl: "https://example.test/tree",
            sourceUrl: "https://example.test",
            version: "v0.1.0",
          });
        case "/api/environments":
          return Response.json({
            items: [
              {
                id: ENV,
                name: "Lab host",
                kind: "local",
                type: envType === "swarm" ? "swarm-manager" : "standalone",
                envType,
                enabled: true,
                isSelfHost: true,
                createdAt: "2026-10-01T10:00:00Z",
                status: { state: "online" },
              },
            ],
            total: 1,
          });
        case `/api/env/${ENV}/summary`:
          return Response.json({
            environment: {
              id: ENV,
              name: "Lab host",
              type: envType === "swarm" ? "swarm-manager" : "standalone",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 0,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/containers/web-id`:
          return answer();
        case `/api/env/${ENV}/containers/web-id/inspect`:
          return inspectAnswer();
        case `/api/env/${ENV}/containers/web-id/restart-loop-mute`:
          return Response.json({
            containerId: "web-id",
            muted: req.method === "PUT",
            changed: true,
          });
        case `/api/env/${ENV}/containers/actions`:
        case `/api/env/${ENV}/containers/remove`:
          return actionAnswer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  detailRetry.attempts = 1;
  document.title = "";
});

function renderAt(path = PAGE) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

const posts = () => sent.filter((s) => s.method === "POST");

async function openMore() {
  const more = await screen.findByRole("button", { name: "More actions" });
  await act(async () => {
    fireEvent.keyDown(more, { key: "Enter" });
  });
  return screen.findAllByRole("menuitem");
}

const labels = (items: HTMLElement[]) => items.map((i) => i.textContent);

describe("the page frame", () => {
  it("shows the breadcrumb, the name with its state and node, and the Overview tab", async () => {
    envType = "swarm";
    renderAt();
    expect(await screen.findByRole("heading", { name: "web" })).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Containers" }).getAttribute("href")).toBe(
      "/env/e1/containers",
    );
    expect(crumb.textContent).toContain("web");
    // The state word is in the header and in the State card.
    expect(screen.getAllByText("Running").length).toBeGreaterThanOrEqual(1);
    expect(await screen.findByText("node-1")).toBeTruthy();
    const tab = screen.getByRole("tab", { name: "Overview" });
    expect(tab.getAttribute("href")).toBe("/env/e1/containers/web-id?node=n1");
    await waitFor(() => expect(tab.getAttribute("data-active")).toBeTruthy());
    expect(document.title).toBe("web · Lab");
  });

  it("asks for the container on the node named in the address", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(sent.some((s) => s.path === "/api/env/e1/containers/web-id")).toBe(true);
  });

  it("shows the tabs and skeletons at once, before the container has loaded", async () => {
    answer = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(
      screen.getAllByRole("status").some((s) => s.textContent === "Loading the container"),
    ).toBe(true);
    expect(screen.queryByRole("heading", { name: "web" })).toBeNull();
  });

  it("marks TierPlan's own containers with the badge", async () => {
    answer = () => Response.json(detail({ selfKind: "server", isSelf: true }));
    renderAt();
    expect(await screen.findByText("TierPlan (this instance)")).toBeTruthy();
  });
});

describe("the header shows the container's update state (12-08)", () => {
  const info = (extra: Record<string, unknown> = {}) => ({
    state: "update_available" as const,
    reason: "" as const,
    registry: "registry.example.com",
    repository: "acme/web",
    tag: "1.4",
    runningDigest: `sha256:${"a".repeat(64)}`,
    remoteDigest: `sha256:${"b".repeat(64)}`,
    changedPlatforms: [],
    checkedAt: new Date().toISOString(),
    successAt: "",
    note: "" as const,
    task: false,
    ...extra,
  });

  it("puts the badge of the update object beside the name", async () => {
    answer = () => Response.json(detail({ update: info() }));
    renderAt();
    const heading = await screen.findByRole("heading", { name: "web" });
    const header = heading.parentElement as HTMLElement;
    expect(within(header).getByText("Update available")).toBeTruthy();
  });

  it("shows no badge, and never Up to date, when there is no update object", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.queryByText("Update available")).toBeNull();
    expect(screen.queryByText("Up to date")).toBeNull();
    expect(screen.queryByText("Not checked yet")).toBeNull();
  });

  it("shows a task container's badge as the same state word", async () => {
    answer = () => Response.json(detail({ service: "web-svc", update: info({ task: true }) }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.getAllByText("Update available").length).toBeGreaterThanOrEqual(1);
  });

  it("renders markup in the name and in the registry's words as text", async () => {
    answer = () =>
      Response.json(
        detail({
          name: "<img src=x onerror=alert(1)>",
          update: info({
            state: "check_failed",
            reason: "other",
            registry: "<b>evil</b>.example.com",
            repository: "<img src=y>",
          }),
        }),
      );
    renderAt();
    await screen.findByRole("heading", { name: "<img src=x onerror=alert(1)>" });
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });
});

describe("the header line and the swarm task banner", () => {
  it("names the stack, the project or the service as plain text", async () => {
    answer = () => Response.json(detail({ stack: "blog", project: "shop", service: "blog_web" }));
    renderAt();
    expect(await screen.findByText("Stack blog")).toBeTruthy();
    cleanup();
    queryClient.clear();
    answer = () => Response.json(detail({ project: "shop" }));
    renderAt();
    expect(await screen.findByText("Project shop")).toBeTruthy();
    cleanup();
    queryClient.clear();
    answer = () => Response.json(detail({ service: "api" }));
    renderAt();
    expect(await screen.findByText("Task of service api")).toBeTruthy();
    // The sidebar's Stacks item is a link too; the header line's words are followed by the name.
    expect(screen.queryByRole("link", { name: /^(Task of service|Stack|Project) \S/ })).toBeNull();
  });

  it("shows the info banner for a task of a service, and none otherwise", async () => {
    answer = () => Response.json(detail({ service: "api" }));
    renderAt();
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toContain(
      "This is a task of service api. Swarm replaces it if it stops.",
    );
    cleanup();
    queryClient.clear();
    answer = () => Response.json(detail());
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.queryByText(/Swarm replaces it/)).toBeNull();
  });
});

describe("the header actions follow the state", () => {
  it("a running container: Stop and Restart, and More with Pause, Kill and Remove", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Restart" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start container" })).toBeNull();
    expect(labels(await openMore())).toEqual(["Pause", "Kill…", "Remove…"]);
  });

  it("a paused container: Unpause and Stop, and More with Restart, Kill and Remove", async () => {
    answer = () => Response.json(detail({ state: "paused" }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.getByRole("button", { name: "Unpause" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Restart" })).toBeNull();
    expect(labels(await openMore())).toEqual(["Restart", "Kill…", "Remove…"]);
  });

  it("a stopped container: Start container as the one accent button, and More with Restart and Remove", async () => {
    answer = () => Response.json(detail({ state: "exited" }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const start = screen.getByRole("button", { name: "Start container" });
    expect(start.getAttribute("data-variant")).toBe("default");
    for (const name of ["Stop", "Restart", "Unpause"]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
    // Every other header button is an outline: the accent is the one way forward.
    expect(labels(await openMore())).toEqual(["Restart", "Remove…"]);
  });

  it("a running container has no accent button", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    for (const name of ["Stop", "Restart"]) {
      expect(screen.getByRole("button", { name }).getAttribute("data-variant")).toBe("outline");
    }
  });

  it("restarts at once, posting the node ID and container ID", async () => {
    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Restart" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      action: "restart",
      targets: [{ nodeId: "n1", id: "web-id" }],
    });
    expect(await screen.findByText("Restarted web.")).toBeTruthy();
  });

  it("asks first for Stop, with the list's dialog", async () => {
    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Stop web?" });
    expect(posts()).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ action: "stop", targets: [{ nodeId: "n1", id: "web-id" }] });
  });

  it("asks for the name to be typed before touching TierPlan's own container", async () => {
    answer = () => Response.json(detail({ name: "tierplan", selfKind: "server", isSelf: true }));
    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Restart" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Restart tierplan?" });
    expect(within(dialog).getByLabelText("Type tierplan to confirm")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("disables the buttons while an action is pending", async () => {
    actionAnswer = () => new Promise<Response>(() => {});
    renderAt();
    const restart = await screen.findByRole("button", { name: "Restart" });
    fireEvent.click(restart);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Stop" })).toHaveProperty("disabled", true),
    );
    expect(screen.getByRole("button", { name: "More actions" })).toHaveProperty("disabled", true);
  });

  it("shows the buttons to a Viewer as unavailable, with the reason, and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const stop = await screen.findByRole("button", { name: "Stop" });
    await waitFor(() => expect(stop.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(stop);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(posts()).toEqual([]);
  });
});

describe("a container that fails to load or is gone", () => {
  it("says it is gone, with a way back, when the container does not exist", async () => {
    answer = () => problem(404, "container_not_found");
    renderAt();
    expect(await screen.findByRole("heading", { name: "This container is gone" })).toBeTruthy();
    const back = screen.getByRole("link", { name: "Back to containers" });
    expect(back.getAttribute("href")).toBe("/env/e1/containers");
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  });

  it("turns into the gone state without a reload when the container disappears", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    answer = () => problem(404, "container_not_found");
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["env", ENV, "containers"] });
    });
    expect(await screen.findByRole("heading", { name: "This container is gone" })).toBeTruthy();
  });

  it("shows the load error with Retry, and retries", async () => {
    answer = () => problem(500, "internal");
    renderAt();
    expect(await screen.findByText("Couldn't load web-id.")).toBeTruthy();
    answer = () => Response.json(detail());
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { name: "web" })).toBeTruthy();
  });

  it("keeps showing a container it has read when a later read fails for another reason", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    answer = () => problem(503, "node_unavailable");
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["env", ENV, "containers"] });
    });
    expect(screen.getByRole("heading", { name: "web" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "This container is gone" })).toBeNull();
  });
});

describe("the Logs tab", () => {
  it("follows Overview and keeps the node in its address", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const tabs = screen.getAllByRole("tab").map((tab) => tab.textContent);
    expect(tabs).toEqual(["Overview", "Logs", "Stats", "Shell", "Inspect"]);
    expect(screen.getByRole("tab", { name: "Logs" }).getAttribute("href")).toBe(
      "/env/e1/containers/web-id/logs?node=n1",
    );
  });

  it("opens the live log of the container on its node, named for it", async () => {
    renderAt(`/env/${ENV}/containers/web-id/logs?node=n1`);
    const surface = await screen.findByRole("log", { name: "Logs for web" });
    expect(surface).toBeTruthy();
    const source = FakeEventSource.instances.find((i) => i.url.includes("/logs/stream"));
    expect(source?.url).toContain("/api/env/e1/containers/web-id/logs/stream?");
    expect(source?.url).toContain("nodeId=n1");
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "Logs" }).getAttribute("data-active")).toBeTruthy(),
    );
    expect(screen.getByRole("tab", { name: "Overview" }).getAttribute("data-active")).toBeNull();
  });

  it("opens the way its address says: range, search and toggles (D-22)", async () => {
    renderAt(
      `/env/${ENV}/containers/web-id/logs?node=n1&since=1h&q=boom&ts=true&utc=true&wrap=true`,
    );
    await screen.findByRole("log", { name: "Logs for web" });
    const source = FakeEventSource.instances.find((i) => i.url.includes("/logs/stream"));
    const url = new URL(source?.url ?? "", "http://x");
    expect(url.searchParams.get("tail")).toBe("50000");
    expect(url.searchParams.get("since")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Show" }).textContent).toContain("Last hour");
    expect((screen.getByRole("searchbox") as HTMLInputElement).value).toBe("boom");
    for (const name of ["Timestamps", "UTC", "Wrap"]) {
      expect(screen.getByRole("button", { name }).getAttribute("aria-pressed")).toBe("true");
    }
  });

  it("writes a change into the address, replacing the entry and keeping the node", async () => {
    const router = renderAt(`/env/${ENV}/containers/web-id/logs?node=n1`);
    await screen.findByRole("log", { name: "Logs for web" });
    const before = router.history.length;
    fireEvent.click(screen.getByRole("button", { name: "Wrap" }));
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ node: "n1", wrap: true }),
    );
    expect(router.history.length).toBe(before);
  });
});

describe("the Shell tab", () => {
  it("sits between Logs and Inspect, keeps the node in its address and is available to a holder of the shell permission", async () => {
    me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["containers.exec"] } });
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const tab = screen.getByRole("tab", { name: "Shell" });
    expect(tab.getAttribute("href")).toBe("/env/e1/containers/web-id/shell?node=n1");
    expect(tab.getAttribute("aria-disabled")).toBeNull();
  });

  it("is shown to anyone who can read the container, unavailable with the reason when the shell permission is missing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const tab = screen.getByRole("tab", { name: "Shell" });
    expect(tab.getAttribute("aria-disabled")).toBe("true");
    expect(tab.textContent).toBe("Shell");
    expect(document.body.textContent).toContain("Needs the container shell permission");
  });
});

describe("the Shell tab on TierPlan's own containers (plan 04-23, D-36)", () => {
  it.each(["server", "agent"] as const)(
    "is unavailable for %s containers, with the reason, and still shown",
    async (kind) => {
      me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["containers.exec"] } });
      answer = () =>
        Response.json(detail({ name: "tierplan", selfKind: kind, isSelf: kind === "server" }));
      renderAt();
      await screen.findByRole("heading", { name: "tierplan" });
      const tab = await screen.findByRole("tab", { name: "Shell" });
      await waitFor(() => expect(tab.getAttribute("aria-disabled")).toBe("true"));
      expect(tab.textContent).toBe("Shell");
      expect(document.body.textContent).toContain(
        "TierPlan doesn't open shells in its own containers.",
      );
    },
  );

  it("stays available for an ordinary container", async () => {
    me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["containers.exec"] } });
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.getByRole("tab", { name: "Shell" }).getAttribute("aria-disabled")).toBeNull();
    expect(document.body.textContent).not.toContain("TierPlan doesn't open shells");
  });
});

describe("the Stats tab", () => {
  it("follows Logs and keeps the node in its address (plan 06-32)", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const tabs = screen.getAllByRole("tab").map((tab) => tab.textContent);
    expect(tabs.indexOf("Stats")).toBe(tabs.indexOf("Logs") + 1);
    expect(screen.getByRole("tab", { name: "Stats" }).getAttribute("href")).toBe(
      "/env/e1/containers/web-id/stats?node=n1",
    );
  });
});

describe("the Inspect tab", () => {
  it("is the last tab and keeps the node in its address", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const tabs = screen.getAllByRole("tab").map((tab) => tab.textContent);
    expect(tabs).toEqual(["Overview", "Logs", "Stats", "Shell", "Inspect"]);
    expect(screen.getByRole("tab", { name: "Inspect" }).getAttribute("href")).toBe(
      "/env/e1/containers/web-id/inspect?node=n1",
    );
  });

  it("shows the raw JSON the server sent, as text, in a focusable block named for the container", async () => {
    renderAt(`/env/${ENV}/containers/web-id/inspect?node=n1`);
    const block = await screen.findByLabelText("Inspect output for web");
    expect(block.tagName).toBe("SECTION");
    expect(block.getAttribute("tabindex")).toBe("0");
    expect(block.textContent).toContain('"Id": "web-id"');
    // Whatever the server put in place of a value is shown as it was sent.
    expect(block.textContent).toContain("MODE=<hidden>");
    expect(
      sent.some((s) => s.path === "/api/env/e1/containers/web-id/inspect" && s.method === "GET"),
    ).toBe(true);
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "Inspect" }).getAttribute("data-active")).toBeTruthy(),
    );
    expect(screen.getByRole("tab", { name: "Overview" }).getAttribute("data-active")).toBeNull();
  });

  it("says when the document could not be read and tries again on Retry", async () => {
    inspectAnswer = () => problem(503, "node_unavailable");
    renderAt(`/env/${ENV}/containers/web-id/inspect?node=n1`);
    expect(await screen.findByText("Couldn't load the inspect output of web.")).toBeTruthy();
    inspectAnswer = () => Response.json({ masked: false, inspect: { Id: "web-id" } });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByLabelText("Inspect output for web")).toBeTruthy();
  });
});

// Restart loops of a standalone container (plan 06-36, RES-13, D-15, D-16): the badge beside the
// state in the header, and the mute in the More menu.
describe("the restart loop", () => {
  const loop = (state: "flagged" | "muted" = "flagged") => ({
    state,
    count: 3,
    windowMinutes: 10,
    lastExitCode: "3",
  });

  it("flags the container beside its state while it keeps restarting", async () => {
    answer = () => Response.json(detail({ restartLoop: loop(), muted: false }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(await screen.findByText("Restart loop")).toBeTruthy();
  });

  it("shows Muted, and no Restart loop, while the alerts are muted", async () => {
    answer = () => Response.json(detail({ restartLoop: loop("muted"), muted: true }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(await screen.findByText("Muted")).toBeTruthy();
    expect(screen.queryByText("Restart loop")).toBeNull();
  });

  it("keeps the Muted badge on a muted container that is not looping now", async () => {
    answer = () => Response.json(detail({ muted: true }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(await screen.findByText("Muted")).toBeTruthy();
  });

  it("shows no badge for a container that is neither", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(screen.queryByText("Restart loop")).toBeNull();
    expect(screen.queryByText("Muted")).toBeNull();
  });

  it("offers Mute restart-loop alerts in More and mutes on the container's node", async () => {
    answer = () => Response.json(detail({ restartLoop: loop(), muted: false }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const items = await openMore();
    expect(labels(items)).toEqual(["Mute restart-loop alerts", "Pause", "Kill…", "Remove…"]);
    fireEvent.click(screen.getByRole("menuitem", { name: "Mute restart-loop alerts" }));
    await waitFor(() =>
      expect(sent.some((s) => s.method === "PUT" && s.path.endsWith("/restart-loop-mute"))).toBe(
        true,
      ),
    );
    expect(await screen.findByText("Muted restart-loop alerts for web.")).toBeTruthy();
  });

  it("offers Unmute in More while muted", async () => {
    answer = () => Response.json(detail({ muted: true }));
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    const items = await openMore();
    expect(labels(items)[0]).toBe("Unmute restart-loop alerts");
  });

  it("leaves More as it was for a container that is neither", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "web" });
    expect(labels(await openMore())).toEqual(["Pause", "Kill…", "Remove…"]);
  });
});
