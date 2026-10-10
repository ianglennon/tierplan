// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import type { NetworkAttachment, NetworkDetail } from "@/api/networks";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The network page and its Attachments tab: what is attached, with addresses and
// aliases, and the states of loading, failing and being gone (plan 04-14, D-42).

const ENV = "e1";
const PAGE = `/env/${ENV}/networks/app0?node=n1`;

function attachment(name: string, extra: Partial<NetworkAttachment> = {}): NetworkAttachment {
  return {
    kind: "container",
    id: `${name}-id`,
    name,
    node: "node-1",
    nodeId: "n1",
    state: "running",
    ipv4: "",
    ipv6: "",
    mac: "",
    aliases: [],
    tasks: 0,
    ...extra,
  };
}

function detail(extra: Partial<NetworkDetail> = {}): NetworkDetail {
  const attachments = extra.attachments ?? [];
  return {
    id: "app0",
    name: "app-net",
    driver: "bridge",
    scope: "local",
    node: "node-1",
    nodeId: "n1",
    created: "2026-10-01T10:00:00Z",
    internal: false,
    attachable: false,
    ipv6: false,
    builtIn: false,
    owner: "",
    ownerKind: "",
    labels: {},
    options: {},
    ipam: [],
    attachments,
    attachedContainers: attachments.filter((a) => a.kind === "container").length,
    attachedServices: attachments.filter((a) => a.kind === "service").length,
    aliasesTruncated: false,
    notReporting: [],
    ...extra,
  };
}

function problem(status: number, type: string) {
  return Response.json({ type, title: "x", status }, { status });
}

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let answer: () => Response | Promise<Response>;
let envType: "swarm" | "standalone";
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "viewer" } });
  answer = () => Response.json(detail());
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
      sent.push({ method: req.method, path: url.pathname, search: url.search });
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
              networks: 1,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/networks/app0`:
          return answer();
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

describe("the network page frame", () => {
  it("opens on the Attachments tab, reads the network on its node and draws the header", async () => {
    answer = () =>
      Response.json(
        detail({ owner: "shop", ownerKind: "stack", attachments: [attachment("web")] }),
      );
    const router = renderAt(`/env/${ENV}/networks/app0?node=n1`);
    expect(await screen.findByRole("heading", { name: "app-net", level: 2 })).toBeTruthy();
    await waitFor(() => expect(router.state.location.pathname).toContain("/attachments"));
    expect(screen.getByText("bridge")).toBeTruthy();
    expect(screen.getByText("Local")).toBeTruthy();
    expect(screen.getByText("Stack shop")).toBeTruthy();
    const nav = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(nav).getByRole("link", { name: "Networks" })).toBeTruthy();
    const read = sent.find((s) => s.path === `/api/env/${ENV}/networks/app0`);
    expect(read?.search).toBe("?nodeId=n1");
    expect(document.title).toContain("app-net");
  });

  it("says a project owns the network when the label is a compose project", async () => {
    answer = () => Response.json(detail({ owner: "blog", ownerKind: "project" }));
    renderAt();
    expect(await screen.findByText("Project blog")).toBeTruthy();
  });

  it("shows the gone state with the way back when the network no longer exists", async () => {
    answer = () => problem(404, "network_not_found");
    renderAt();
    expect(await screen.findByRole("heading", { name: "This network is gone" })).toBeTruthy();
    expect(screen.getByText(/It was removed, or a deploy replaced it/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to networks" }).getAttribute("href")).toContain(
      `/env/${ENV}/networks`,
    );
    expect(screen.queryByRole("tab")).toBeNull();
  });

  it("shows a load error with Retry, which reads again", async () => {
    answer = () => problem(503, "environment_offline");
    renderAt();
    const retry = await screen.findByRole("button", { name: "Retry" });
    expect(screen.getByText(/Couldn't load app0/)).toBeTruthy();
    answer = () => Response.json(detail());
    fireEvent.click(retry);
    expect(await screen.findByRole("heading", { name: "app-net", level: 2 })).toBeTruthy();
  });
});

describe("Attachments", () => {
  it("lists each container with its address, MAC and aliases as badges, linked to its page", async () => {
    answer = () =>
      Response.json(
        detail({
          attachments: [
            attachment("db", {
              state: "exited",
              ipv4: "10.20.0.3",
              mac: "02:42:0a:14:00:03",
              aliases: ["db"],
            }),
            attachment("web", {
              ipv4: "10.20.0.2",
              ipv6: "fd00:20::2",
              mac: "02:42:0a:14:00:02",
              aliases: ["web", "api"],
            }),
          ],
        }),
      );
    renderAt();
    const table = await screen.findByRole("table", { name: "Attachments" });
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(
      within(rows[1] as HTMLElement)
        .getByRole("link", { name: "db" })
        .getAttribute("href"),
    ).toContain(`/env/${ENV}/containers/db-id`);
    const web = within(rows[2] as HTMLElement);
    expect(web.getByRole("link", { name: /^web$/ }).getAttribute("href")).toContain("node=n1");
    for (const text of ["Container", "10.20.0.2", "fd00:20::2", "02:42:0a:14:00:02", "api"]) {
      expect(web.getByText(text, { exact: true })).toBeTruthy();
    }
    expect(within(table).queryByRole("columnheader", { name: "Node" })).toBeNull();
  });

  it("shows a dash for what a container does not have and for aliases that were not read", async () => {
    answer = () =>
      Response.json(
        detail({ attachments: [attachment("job", { aliases: null })], aliasesTruncated: true }),
      );
    renderAt();
    const table = await screen.findByRole("table", { name: "Attachments" });
    expect(
      within(within(table).getAllByRole("row")[1] as HTMLElement).getAllByText("—").length,
    ).toBe(4);
    expect(screen.getByText("Aliases are shown for the first 50 containers only.")).toBeTruthy();
  });

  it("says nothing is attached when nothing is", async () => {
    renderAt();
    expect(await screen.findByText("Nothing is attached to this network.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a skeleton while the network is read", async () => {
    answer = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByText("Loading the attachments")).toBeTruthy();
  });

  it("adds the Node column on a swarm", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(detail({ attachments: [attachment("web", { node: "worker-2" })] }));
    renderAt();
    const table = await screen.findByRole("table", { name: "Attachments" });
    await waitFor(() =>
      expect(within(table).getByRole("columnheader", { name: "Node" })).toBeTruthy(),
    );
    expect(within(table).getByText("worker-2")).toBeTruthy();
  });
});
