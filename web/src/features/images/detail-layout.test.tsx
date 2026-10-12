// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import type { ImageDetail } from "@/api/images";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The image page: header, Dangling and node-count badges, Pull… and More, the Overview
// tab and the states of loading, failing and being gone (plan 05-14, RES-09, D-18).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;
const PAGE = `/env/${ENV}/images/${ID}`;

function detail(extra: Partial<ImageDetail> = {}): ImageDetail {
  return {
    id: ID,
    shortId: "a".repeat(12),
    names: [
      { name: "nginx:1.27", kind: "tag" },
      { name: "nginx:latest", kind: "tag" },
      { name: `nginx@sha256:${"c".repeat(64)}`, kind: "digest" },
    ],
    dangling: false,
    size: 4096,
    created: "2026-10-01T10:00:00Z",
    os: "linux",
    architecture: "arm64",
    variant: "v8",
    labels: { maintainer: "ops" },
    nodes: [{ nodeId: "n1", name: "node-1", present: true, names: ["nginx:1.27"] }],
    notReporting: [],
    ...extra,
  };
}

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let answer: () => Response | Promise<Response>;
let inspectAnswer: () => Response | Promise<Response>;
let historyAnswer: () => Response | Promise<Response>;
let usedByAnswer: () => Response | Promise<Response>;
let envType: "standalone" | "swarm";
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  sent = [];
  envType = "standalone";
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = () => Response.json(detail());
  inspectAnswer = () =>
    Response.json({
      masked: true,
      inspect: { Id: ID, Config: { Env: ["API_KEY=<hidden>"] } },
    });
  usedByAnswer = () =>
    Response.json({
      items: [],
      usedByContainers: 0,
      usedByServices: 0,
      notReporting: [],
    });
  historyAnswer = () =>
    Response.json({
      layers: [
        {
          id: `sha256:${"b".repeat(64)}`,
          createdBy: "RUN build",
          size: 1024,
          created: "2026-10-01T10:00:00Z",
          comment: "",
        },
      ],
    });
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  detailRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      // The client writes the colon of an image ID as %3A.
      const path = decodeURIComponent(url.pathname);
      sent.push({ method: req.method, path, search: url.search });
      switch (path) {
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
                type: envType,
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
            environment: { id: ENV, name: "Lab host", type: envType, status: { state: "online" } },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 0,
              containersRunning: 0,
              containersStopped: 0,
              images: 1,
              volumes: 0,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/images/${ID}`:
          return answer();
        case `/api/env/${ENV}/images/${ID}/inspect`:
          return inspectAnswer();
        case `/api/env/${ENV}/images/${ID}/history`:
          return historyAnswer();
        case `/api/env/${ENV}/images/${ID}/used-by`:
          return usedByAnswer();
        case `/api/env/${ENV}/images/pull-targets`:
          return Response.json({ nodes: [] });
        case `/api/env/${ENV}/images`:
          return Response.json({
            items: [],
            total: 0,
            truncated: false,
            nodes: [],
            notReporting: [],
          });
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

describe("image page header", () => {
  it("shows the breadcrumb and the first tag as the title in mono, and sets the document title", async () => {
    renderAt();
    const heading = await screen.findByRole("heading", { level: 2, name: "nginx:1.27" });
    expect(heading.className).toContain("font-mono");
    expect(heading.className).toContain("line-clamp-2");
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Images" }).getAttribute("href")).toBe(
      `/env/${ENV}/images`,
    );
    expect(within(crumb).getByText("nginx:1.27")).toBeTruthy();
    expect(screen.queryByText("Dangling")).toBeNull();
    await waitFor(() => expect(document.title).toContain("nginx:1.27"));
    expect(sent.some((s) => s.path === `/api/env/${ENV}/images/${ID}`)).toBe(true);
  });

  it("shows the tabs and a skeleton before the image has loaded, then the Overview with its cards", async () => {
    let release: (r: Response) => void = () => undefined;
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    renderAt();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 2 })).toBeNull();
    release(Response.json(detail()));
    expect(await screen.findByRole("region", { name: "Names" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Details" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Labels" })).toBeTruthy();
  });

  it("calls an untagged image by its short ID and badges it Dangling", async () => {
    answer = () => Response.json(detail({ names: [], dangling: true }));
    renderAt();
    expect(await screen.findByRole("heading", { level: 2, name: "a".repeat(12) })).toBeTruthy();
    expect(screen.getByText("Dangling")).toBeTruthy();
    expect(await screen.findByText("No names. This image is dangling.")).toBeTruthy();
  });

  it("badges a swarm image with the nodes that have it and shows the On nodes card", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(
        detail({
          nodes: [
            { nodeId: "n1", name: "mgr-1", present: true, names: ["nginx:1.27"] },
            { nodeId: "n2", name: "mgr-2", present: true, names: ["nginx:1.27", "nginx:latest"] },
            { nodeId: "n3", name: "wrk-1", present: false, names: [] },
          ],
        }),
      );
    renderAt();
    expect(await screen.findByText("on 2/3 nodes")).toBeTruthy();
    const card = await screen.findByRole("region", { name: "On nodes" });
    expect(within(card).getByText("mgr-2")).toBeTruthy();
    expect(within(card).getByText("Not on this node")).toBeTruthy();
  });

  it("has no On nodes card on a standalone host", async () => {
    renderAt();
    await screen.findByRole("region", { name: "Names" });
    expect(screen.queryByRole("region", { name: "On nodes" })).toBeNull();
  });

  it("passes the node of the address on to the read", async () => {
    renderAt(`${PAGE}?node=n1`);
    await screen.findByRole("heading", { level: 2, name: "nginx:1.27" });
    expect(
      sent.some((s) => s.path === `/api/env/${ENV}/images/${ID}` && s.search === "?node=n1"),
    ).toBe(true);
  });
});

describe("image page actions", () => {
  it("opens the pull form with the first tag from Pull…", async () => {
    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Pull nginx:1.27 again" }));
    const dialog = await screen.findByRole("dialog");
    expect((within(dialog).getByLabelText("Image") as HTMLInputElement).value).toBe("nginx:1.27");
  });

  it("makes Pull… unavailable, with the reason, for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const button = await screen.findByRole("button", { name: "Pull nginx:1.27 again" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect((await screen.findAllByText("Needs Operator")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("has Copy image ID under More", async () => {
    const write = vi.fn(async () => undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: write } });
    renderAt();
    const more = await screen.findByRole("button", { name: "More actions" });
    fireEvent.pointerDown(more, { button: 0, ctrlKey: false });
    fireEvent.click(more);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Copy image ID" }));
    await waitFor(() => expect(write).toHaveBeenCalledWith(ID));
  });
});

describe("image page states", () => {
  it("replaces the page with This image is gone when no node has the image", async () => {
    answer = () =>
      Response.json({ type: "image_not_found", title: "x", status: 404 }, { status: 404 });
    renderAt();
    expect(await screen.findByText("This image is gone")).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(screen.getByRole("link", { name: "Back to images" })).toBeTruthy();
  });

  it("says it could not load the image, with Retry, when the read fails", async () => {
    let fail = true;
    answer = () =>
      fail ? Response.json({ title: "x", status: 502 }, { status: 502 }) : Response.json(detail());
    renderAt();
    expect(await screen.findByText(/Couldn't load aaaaaaaaaaaa/)).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 2, name: "nginx:1.27" })).toBeTruthy();
  });

  it("shows No permission to a caller without images.read", async () => {
    me = makeMe({ envs: {} });
    renderAt();
    await waitFor(() =>
      expect(screen.queryByRole("heading", { level: 2, name: "nginx:1.27" })).toBeNull(),
    );
    expect(sent.some((s) => s.path === `/api/env/${ENV}/images/${ID}`)).toBe(false);
  });
});

describe("image Inspect tab", () => {
  it("is a tab beside Overview and shows the document with the masking alert", async () => {
    const router = renderAt(`${PAGE}?node=n1`);
    const tab = await screen.findByRole("tab", { name: "Inspect" });
    expect(screen.getByRole("tab", { name: "Overview" })).toBeTruthy();
    fireEvent.click(tab);
    await waitFor(() =>
      expect(decodeURIComponent(router.state.location.pathname)).toBe(
        `/env/${ENV}/images/${ID}/inspect`,
      ),
    );
    expect(router.state.location.search).toMatchObject({ node: "n1" });
    const doc = await screen.findByRole("region", { name: /nginx:1.27/i });
    expect(doc.textContent).toContain("API_KEY=<hidden>");
    expect(
      screen.getByText(
        "Environment variable values are hidden. Needs the reveal secrets permission to show them.",
      ),
    ).toBeTruthy();
    expect(
      sent.some(
        (s) => s.path === `/api/env/${ENV}/images/${ID}/inspect` && s.search === "?node=n1",
      ),
    ).toBe(true);
  });

  it("shows no alert when nothing was hidden", async () => {
    inspectAnswer = () => Response.json({ masked: false, inspect: { Id: ID } });
    renderAt(`${PAGE}/inspect`);
    await screen.findByRole("region", { name: /nginx:1.27/i });
    expect(screen.queryByText(/Environment variable values are hidden/)).toBeNull();
  });

  it("says it could not load the inspect output, with Retry", async () => {
    let fail = true;
    inspectAnswer = () =>
      fail
        ? Response.json({ title: "x", status: 502 }, { status: 502 })
        : Response.json({ masked: false, inspect: { Id: ID } });
    renderAt(`${PAGE}/inspect`);
    expect(await screen.findByText(/Couldn't load the inspect output of nginx:1.27/)).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: /nginx:1.27/i })).toBeTruthy();
  });
});

describe("image usage line", () => {
  it("says Not in use under the title when nothing uses the image", async () => {
    renderAt();
    expect(await screen.findByText("Not in use")).toBeTruthy();
  });

  it("says what uses it: containers alone off a swarm, both on one", async () => {
    usedByAnswer = () =>
      Response.json({ items: [], usedByContainers: 2, usedByServices: 1, notReporting: [] });
    renderAt();
    expect(await screen.findByText("Used by 2 containers")).toBeTruthy();
    cleanup();
    queryClient.clear();
    envType = "swarm";
    renderAt();
    expect(await screen.findByText("Used by 2 containers, 1 service")).toBeTruthy();
  });

  it("counts a node's containers in the On nodes card, and a dash where the node lacks the image", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(
        detail({
          nodes: [
            { nodeId: "n1", name: "node-1", present: true, names: ["nginx:1.27"] },
            { nodeId: "n2", name: "node-2", present: false, names: [] },
          ],
        }),
      );
    const item = (id: string, name: string) => ({
      kind: "container",
      id,
      name,
      nodeId: "n1",
      node: "node-1",
      state: "running",
      matchedBy: "image_id",
    });
    usedByAnswer = () =>
      Response.json({
        items: [item("c1", "web"), item("c2", "old")],
        usedByContainers: 2,
        usedByServices: 0,
        notReporting: [],
      });
    renderAt();
    const card = await screen.findByRole("region", { name: "On nodes" });
    await waitFor(() => {
      const rows = within(card).getAllByRole("row").slice(1);
      expect(within(rows[0] as HTMLElement).getByText("2")).toBeTruthy();
    });
    const rows = within(card).getAllByRole("row").slice(1);
    expect(within(rows[1] as HTMLElement).getByText("—")).toBeTruthy();
  });

  it("leaves the line out when the read fails, and still shows the page", async () => {
    usedByAnswer = () => Response.json({ title: "x", status: 502 }, { status: 502 });
    renderAt();
    expect(await screen.findByRole("heading", { level: 2, name: "nginx:1.27" })).toBeTruthy();
    expect(screen.queryByText("Not in use")).toBeNull();
  });
});

describe("image Used by tab", () => {
  it("is the second tab and shows what uses the image", async () => {
    usedByAnswer = () =>
      Response.json({
        items: [
          {
            kind: "container",
            id: "c1",
            name: "web",
            nodeId: "n1",
            node: "node-1",
            state: "exited",
            matchedBy: "image_id",
          },
        ],
        usedByContainers: 1,
        usedByServices: 0,
        notReporting: [],
      });
    const router = renderAt(PAGE);
    fireEvent.click(await screen.findByRole("tab", { name: "Used by" }));
    await waitFor(() =>
      expect(decodeURIComponent(router.state.location.pathname)).toBe(
        `/env/${ENV}/images/${ID}/used-by`,
      ),
    );
    expect(await screen.findByRole("link", { name: "Open container web" })).toBeTruthy();
  });
});

describe("image Layers tab", () => {
  it("is a tab between Used by and Inspect and shows the layers", async () => {
    const router = renderAt(PAGE);
    const tabs = (await screen.findAllByRole("tab")).map((tab) => tab.textContent);
    expect(tabs).toEqual(["Overview", "Used by", "Layers", "Inspect"]);
    fireEvent.click(screen.getByRole("tab", { name: "Layers" }));
    await waitFor(() =>
      expect(decodeURIComponent(router.state.location.pathname)).toBe(
        `/env/${ENV}/images/${ID}/layers`,
      ),
    );
    expect(await screen.findByRole("table", { name: "Layers" })).toBeTruthy();
    expect(await screen.findByText("RUN build")).toBeTruthy();
  });
});

describe("image page going away", () => {
  it("turns into This image is gone when a later read finds no node with the image", async () => {
    const router = renderAt(PAGE);
    expect(await screen.findByRole("heading", { level: 2, name: "nginx:1.27" })).toBeTruthy();
    answer = () =>
      Response.json({ type: "image_not_found", title: "x", status: 404 }, { status: 404 });
    await queryClient.invalidateQueries({ queryKey: ["env", ENV, "images"] });
    expect(await screen.findByText("This image is gone")).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(router.state.location.pathname).toContain("/images/");
  });

  it("turns the Inspect tab into the gone state too, without a reload", async () => {
    renderAt(`${PAGE}/inspect`);
    await screen.findByRole("region", { name: /nginx:1.27/i });
    answer = () =>
      Response.json({ type: "image_not_found", title: "x", status: 404 }, { status: 404 });
    await queryClient.invalidateQueries({ queryKey: ["env", ENV, "images"] });
    expect(await screen.findByText("This image is gone")).toBeTruthy();
  });
});
