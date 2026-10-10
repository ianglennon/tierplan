// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import { queryClient } from "@/api/query-client";
import type { VolumeDetail } from "@/api/volumes";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The volume page: header, the in-use callout, Delete… and the states of loading, failing
// and being gone (plan 04-18, RES-07, D-46, D-47).

const ENV = "e1";
const PAGE = `/env/${ENV}/volumes/appdata?node=n1`;

function detail(extra: Partial<VolumeDetail> = {}): VolumeDetail {
  return {
    name: "appdata",
    driver: "local",
    mountpoint: "/var/lib/docker/volumes/appdata/_data",
    scope: "local",
    node: "node-1",
    nodeId: "n1",
    created: "2026-10-01T10:00:00Z",
    anonymous: false,
    options: [],
    labels: {},
    usedBy: [],
    usedByContainers: 0,
    usedByServices: 0,
    notReporting: [],
    ...extra,
  };
}

const inUse = detail({
  usedBy: [
    {
      kind: "container",
      id: "c1",
      name: "web",
      node: "node-1",
      nodeId: "n1",
      mountedAt: "/data",
      readOnly: false,
      state: "running",
    },
    {
      kind: "service",
      id: "s1",
      name: "idle",
      node: "",
      nodeId: "",
      mountedAt: "/srv",
      readOnly: true,
      state: "stopped",
    },
  ],
  usedByContainers: 1,
  usedByServices: 1,
});

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let answer: () => Response | Promise<Response>;
let deleteAnswer: () => Response;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = () => Response.json(detail());
  deleteAnswer = () =>
    Response.json({ results: [{ nodeId: "n1", name: "appdata", result: "done" }] });
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
                type: "standalone",
                envType: "standalone",
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
              type: "standalone",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 1,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/volumes/appdata`:
          return answer();
        case `/api/env/${ENV}/volumes/delete`:
          return deleteAnswer();
        case `/api/env/${ENV}/volumes`:
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

describe("volume page header", () => {
  it("shows the breadcrumb, the name in mono and the driver, and sets the document title", async () => {
    renderAt();
    const heading = await screen.findByRole("heading", { level: 2, name: "appdata" });
    expect(heading.className).toContain("font-mono");
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Volumes" }).getAttribute("href")).toBe(
      `/env/${ENV}/volumes`,
    );
    expect(within(crumb).getByText("appdata")).toBeTruthy();
    expect(screen.getAllByText("local").length).toBeGreaterThan(0);
    await waitFor(() => expect(document.title).toContain("appdata"));
    expect(sent.some((s) => s.path === `/api/env/${ENV}/volumes/appdata`)).toBe(true);
  });

  it("marks an anonymous volume and shows the Overview tab with the Details", async () => {
    answer = () => Response.json(detail({ anonymous: true }));
    renderAt();
    expect(await screen.findByText("Anonymous")).toBeTruthy();
    expect(await screen.findByRole("region", { name: "Details" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Overview" })).toBeTruthy();
  });
});

describe("volume page in use", () => {
  it("shows the in-use callout with who uses it and disables Delete… with the reason", async () => {
    answer = () => Response.json(inUse);
    renderAt();
    expect(await screen.findByText("appdata is in use")).toBeTruthy();
    expect(
      screen.getByText(
        "Stopped containers and swarm services that mount it count. Remove them first.",
      ),
    ).toBeTruthy();
    const alert = screen.getByText("appdata is in use").closest('[role="alert"]') as HTMLElement;
    expect(within(alert).getByText("web on node-1")).toBeTruthy();
    expect(within(alert).getByText("Service idle")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Delete appdata" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(
      (
        await screen.findAllByText(
          "In use by 1 container and 1 service. Remove what uses it first.",
        )
      ).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("shows no callout for a volume nothing uses, and Delete… opens the typed-name dialog", async () => {
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete appdata" });
    expect(screen.queryByText("appdata is in use")).toBeNull();
    expect(button.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(button);
    expect(await screen.findByRole("alertdialog")).toBeTruthy();
    expect(screen.getByText("Delete appdata?")).toBeTruthy();
  });

  it("disables Delete… with who may for a caller without the delete volumes permission", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete appdata" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(
      (await screen.findAllByText("Needs the delete volumes permission")).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("deletes the volume on its node with the name typed, then goes back to the list", async () => {
    const router = renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Delete appdata" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "appdata" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete volume" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/env/${ENV}/volumes`));
    const post = sent.find((s) => s.path === `/api/env/${ENV}/volumes/delete`);
    expect(post?.body).toMatchObject({
      confirm: "appdata",
      targets: [{ nodeId: "n1", name: "appdata" }],
    });
  });
});

describe("volume page states", () => {
  it("replaces the page with This volume is gone when the node has no such volume", async () => {
    answer = () =>
      Response.json({ type: "volume_not_found", title: "x", status: 404 }, { status: 404 });
    renderAt();
    expect(await screen.findByText("This volume is gone")).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(screen.getByRole("link", { name: "Back to volumes" })).toBeTruthy();
  });

  it("says it could not load the volume, with Retry, when the read fails", async () => {
    let fail = true;
    answer = () =>
      fail ? Response.json({ title: "x", status: 502 }, { status: 502 }) : Response.json(detail());
    renderAt();
    expect(await screen.findByText(/Couldn't load appdata/)).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 2, name: "appdata" })).toBeTruthy();
  });
});
