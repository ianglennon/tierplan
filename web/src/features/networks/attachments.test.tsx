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
const ATTACHMENTS = `/env/${ENV}/networks/app0/attachments?node=n1`;

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
  body?: Record<string, unknown>;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let answer: () => Response | Promise<Response>;
let inspectAnswer: () => Response | Promise<Response>;
let deleteAnswer: () => Response | Promise<Response>;
let disconnectAnswer: () => Response | Promise<Response>;
let envType: "swarm" | "standalone";
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "viewer" } });
  answer = () => Response.json(detail());
  inspectAnswer = () =>
    Response.json({ inspect: { Id: "app0", Name: "app-net", Driver: "bridge" } });
  deleteAnswer = () => new Response(null, { status: 204 });
  disconnectAnswer = () => new Response(null, { status: 204 });
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
      sent.push({
        method: req.method,
        path: url.pathname,
        search: url.search,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      });
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
          return req.method === "DELETE" ? deleteAnswer() : answer();
        case `/api/env/${ENV}/networks/app0/inspect`:
          return inspectAnswer();
        case `/api/env/${ENV}/networks/app0/disconnect`:
          return disconnectAnswer();
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
  it("reads the network on its node and draws the header", async () => {
    answer = () =>
      Response.json(
        detail({ owner: "shop", ownerKind: "stack", attachments: [attachment("web")] }),
      );
    renderAt(ATTACHMENTS);
    expect(await screen.findByRole("heading", { name: "app-net", level: 2 })).toBeTruthy();
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
    renderAt(ATTACHMENTS);
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
    renderAt(ATTACHMENTS);
    const table = await screen.findByRole("table", { name: "Attachments" });
    expect(
      within(within(table).getAllByRole("row")[1] as HTMLElement).getAllByText("—").length,
    ).toBe(4);
    expect(screen.getByText("Aliases are shown for the first 50 containers only.")).toBeTruthy();
  });

  it("says nothing is attached when nothing is", async () => {
    renderAt(ATTACHMENTS);
    expect(await screen.findByText("Nothing is attached to this network.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a skeleton while the network is read", async () => {
    answer = () => new Promise<Response>(() => {});
    renderAt(ATTACHMENTS);
    expect(await screen.findByText("Loading the attachments")).toBeTruthy();
  });

  it("adds the Node column on a swarm", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(detail({ attachments: [attachment("web", { node: "worker-2" })] }));
    renderAt(ATTACHMENTS);
    const table = await screen.findByRole("table", { name: "Attachments" });
    await waitFor(() =>
      expect(within(table).getByRole("columnheader", { name: "Node" })).toBeTruthy(),
    );
    expect(within(table).getByText("worker-2")).toBeTruthy();
  });
});

const deletes = () => sent.filter((s) => s.method === "DELETE");

describe("the Overview tab", () => {
  it("is the page's own address and lists the details, the subnets and gateways, the options and the labels", async () => {
    answer = () =>
      Response.json(
        detail({
          id: "app0-full-id",
          internal: true,
          ipv6: true,
          attachable: true,
          owner: "shop",
          ownerKind: "stack",
          ipam: [
            { subnet: "10.20.0.0/24", gateway: "10.20.0.1", ipRange: "10.20.0.128/25" },
            { subnet: "fd00:20::/64", gateway: "", ipRange: "" },
          ],
          options: { "com.docker.network.bridge.name": "br-app" },
          labels: { "com.docker.stack.namespace": "shop", team: "<b>core</b>" },
        }),
      );
    const router = renderAt(`/env/${ENV}/networks/app0?node=n1`);
    expect(await screen.findByRole("heading", { name: "Details", level: 3 })).toBeTruthy();
    expect(router.state.location.pathname).toBe(`/env/${ENV}/networks/app0`);
    const details = screen.getByRole("table", { name: "Details" });
    for (const [key, value] of [
      ["Driver", "bridge"],
      ["Scope", "Local"],
      ["IPv6", "Yes"],
      ["Internal", "Yes"],
      ["Attachable", "Yes"],
      ["Owner", "Stack shop"],
    ]) {
      const row = within(details)
        .getByText(key as string, { exact: true })
        .closest("tr") as HTMLElement;
      expect(within(row).getByText(value as string)).toBeTruthy();
    }
    expect(screen.getByRole("button", { name: "Copy the network ID" })).toBeTruthy();
    expect(within(details).getByText("app0-full-id")).toBeTruthy();
    const pools = screen.getByRole("table", { name: "Subnets and gateways" });
    expect(within(pools).getByText("10.20.0.1")).toBeTruthy();
    expect(within(pools).getByText("10.20.0.128/25")).toBeTruthy();
    expect(within(pools).getByText("fd00:20::/64")).toBeTruthy();
    expect(screen.getByText("com.docker.network.bridge.name")).toBeTruthy();
    expect(screen.getByText("br-app")).toBeTruthy();
    // Docker's text is never read as markup.
    expect(screen.getByText("<b>core</b>")).toBeTruthy();
  });

  it("says so when the network has no address pools, options or labels", async () => {
    renderAt();
    expect(await screen.findByText("No address pools.")).toBeTruthy();
    expect(screen.getByText("No options.")).toBeTruthy();
    expect(screen.getByText("No labels.")).toBeTruthy();
  });

  it("has the three tabs", async () => {
    renderAt();
    const tabs = await screen.findByRole("tablist", { name: "Network sections" });
    expect(
      within(tabs)
        .getAllByRole("tab")
        .map((t) => t.textContent),
    ).toEqual(["Overview", "Attachments", "Inspect"]);
  });
});

describe("the Inspect tab", () => {
  it("shows the engine's document as text with Copy and Download", async () => {
    renderAt(`/env/${ENV}/networks/app0/inspect?node=n1`);
    const block = await screen.findByLabelText("Inspect output for app-net");
    expect(block.textContent).toContain('"Driver": "bridge"');
    expect(screen.getByRole("button", { name: "Copy JSON" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download JSON" })).toBeTruthy();
    const read = sent.find((s) => s.path.endsWith("/inspect"));
    expect(read?.search).toBe("?nodeId=n1");
  });

  it("shows a load error with Retry", async () => {
    inspectAnswer = () => problem(503, "node_unavailable");
    renderAt(`/env/${ENV}/networks/app0/inspect?node=n1`);
    const retry = await screen.findByRole("button", { name: "Retry" });
    inspectAnswer = () => Response.json({ inspect: { Name: "app-net" } });
    fireEvent.click(retry);
    expect(await screen.findByLabelText("Inspect output for app-net")).toBeTruthy();
  });
});

describe("Delete and why it is unavailable", () => {
  it("blocks Delete on a network in use and lists what uses it above the tab, on every tab", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    answer = () =>
      Response.json(
        detail({
          attachments: [
            attachment("web", { node: "node-1" }),
            attachment("db", { node: "node-2", nodeId: "n2" }),
            attachment("api", { kind: "service", id: "svc1", node: "", nodeId: "", state: "" }),
          ],
        }),
      );
    renderAt(`/env/${ENV}/networks/app0/attachments?node=n1`);
    const callout = await screen.findByRole("alert");
    expect(within(callout).getByText("app-net is in use")).toBeTruthy();
    expect(
      within(callout).getByText(/Swarm services that mention this network count/),
    ).toBeTruthy();
    expect(within(callout).getByText("web on node-1")).toBeTruthy();
    expect(within(callout).getByText("db on node-2")).toBeTruthy();
    expect(within(callout).getByText("Service api")).toBeTruthy();
    const del = screen.getByRole("button", { name: "Delete network app-net" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(deletes()).toHaveLength(0);
  });

  it("shows the in-use callout on the Overview too", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    answer = () => Response.json(detail({ attachments: [attachment("web")] }));
    renderAt(`/env/${ENV}/networks/app0?node=n1`);
    expect(await screen.findByText("app-net is in use")).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Details", level: 3 })).toBeTruthy();
  });

  it("shows a built-in network the info note and a disabled Delete", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    answer = () =>
      Response.json(detail({ name: "bridge", builtIn: true, attachments: [attachment("web")] }));
    renderAt();
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("bridge is built in and can't be deleted.")).toBeTruthy();
    expect(screen.queryByText("bridge is in use")).toBeNull();
    const del = screen.getByRole("button", { name: "Delete network bridge" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
  });

  it("shows an Operator Delete disabled with Needs Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    renderAt();
    const del = await screen.findByRole("button", { name: "Delete network app-net" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(del);
    expect((await screen.findAllByText("Needs Admin")).length).toBeGreaterThan(0);
  });

  it("deletes an unused network after the confirm, with the node, and the page becomes the gone state", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Delete network app-net" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete app-net?")).toBeTruthy();
    answer = () => problem(404, "network_not_found");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete network" }));
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.path).toBe(`/api/env/${ENV}/networks/app0`);
    expect(deletes()[0]?.search).toBe("?nodeId=n1");
    expect(await screen.findByRole("heading", { name: "This network is gone" })).toBeTruthy();
  });
});

describe("service rows", () => {
  it("read 'Service {name}' with the task count and no link", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(
        detail({
          scope: "swarm",
          node: "Cluster",
          attachments: [
            attachment("web", {
              kind: "service",
              id: "s1",
              node: "",
              nodeId: "",
              state: "",
              tasks: 2,
            }),
            attachment("idle", {
              kind: "service",
              id: "s2",
              node: "",
              nodeId: "",
              state: "",
              tasks: 0,
            }),
          ],
        }),
      );
    renderAt(`/env/${ENV}/networks/app0/attachments`);
    const table = await screen.findByRole("table", { name: "Attachments" });
    expect(within(table).getByText("Service web")).toBeTruthy();
    expect(within(table).getByText("Service idle")).toBeTruthy();
    expect(within(table).queryByRole("link")).toBeNull();
    expect(within(table).getAllByText("Service").length).toBe(2);
    expect(within(table).getByText("2 tasks")).toBeTruthy();
    expect(within(table).getByText("0 tasks")).toBeTruthy();
  });
});

describe("Connect container", () => {
  it("is disabled for a Viewer with 'Needs Operator' and opens nothing", async () => {
    answer = () => Response.json(detail({ attachments: [attachment("web")] }));
    renderAt(ATTACHMENTS);
    const button = await screen.findByRole("button", { name: "Connect container" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens the connect dialog for an Operator, also when nothing is attached yet", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    answer = () => Response.json(detail());
    renderAt(ATTACHMENTS);
    expect(await screen.findByText("Nothing is attached to this network.")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Connect container" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(button);
    expect(
      await screen.findByRole("dialog", { name: "Connect a container to app-net" }),
    ).toBeTruthy();
  });
});

describe("Disconnect…", () => {
  const disconnects = () => sent.filter((s) => s.path.endsWith("/disconnect"));

  function withRows() {
    answer = () =>
      Response.json(
        detail({
          attachments: [
            attachment("web"),
            attachment("db"),
            attachment("api-svc", { kind: "service", id: "", nodeId: "", node: "", tasks: 2 }),
          ],
        }),
      );
  }

  it("is disabled for a Viewer on every container row, with 'Needs Operator'", async () => {
    withRows();
    renderAt(ATTACHMENTS);
    const web = await screen.findByRole("button", { name: "Disconnect web from app-net" });
    expect(web.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen
        .getByRole("button", { name: "Disconnect db from app-net" })
        .getAttribute("aria-disabled"),
    ).toBe("true");
    fireEvent.click(web);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(disconnects()).toEqual([]);
  });

  it("has no action on a service row", async () => {
    withRows();
    renderAt(ATTACHMENTS);
    await screen.findByRole("button", { name: "Disconnect web from app-net" });
    const table = screen.getByRole("table", { name: "Attachments" });
    const row = within(table).getByText("Service api-svc").closest("tr") as HTMLElement;
    expect(within(row).queryByRole("button")).toBeNull();
  });

  it("asks first, and keeps Cancel focused, before anything is sent", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    withRows();
    renderAt(ATTACHMENTS);
    fireEvent.click(await screen.findByRole("button", { name: "Disconnect web from app-net" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Disconnect web from app-net?" });
    expect(dialog.textContent).toContain(
      "It loses access to everything on that network straight away.",
    );
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(disconnects()).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(disconnects()).toEqual([]);
  });

  it("disconnects the container on its node after the confirm, says so and refreshes the network", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    withRows();
    renderAt(ATTACHMENTS);
    fireEvent.click(await screen.findByRole("button", { name: "Disconnect web from app-net" }));
    const dialog = await screen.findByRole("alertdialog");
    const reads = sent.filter(
      (s) => s.method === "GET" && s.path === `/api/env/${ENV}/networks/app0`,
    ).length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    await waitFor(() => expect(disconnects()).toHaveLength(1));
    expect(disconnects()[0]).toMatchObject({
      method: "POST",
      path: `/api/env/${ENV}/networks/app0/disconnect`,
      body: { containerId: "web-id", nodeId: "n1" },
    });
    expect(await screen.findByText("Disconnected web from app-net.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() =>
      expect(
        sent.filter((s) => s.method === "GET" && s.path === `/api/env/${ENV}/networks/app0`).length,
      ).toBeGreaterThan(reads),
    );
  });

  it("shows the pending label while the request is out and cannot be dismissed", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    withRows();
    let release: (r: Response) => void = () => {};
    disconnectAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    renderAt(ATTACHMENTS);
    fireEvent.click(await screen.findByRole("button", { name: "Disconnect web from app-net" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    expect(await within(dialog).findByText("Disconnecting…")).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Cancel" }).hasAttribute("disabled")).toBe(
      true,
    );
    release(new Response(null, { status: 204 }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("asks for the container's name when the server says it is Bayplan's own, and sends it as confirm", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    withRows();
    disconnectAnswer = () => problem(422, "confirmation_required");
    renderAt(ATTACHMENTS);
    fireEvent.click(await screen.findByRole("button", { name: "Disconnect web from app-net" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    const input = await within(dialog).findByRole("textbox", { name: "Type web to confirm" });
    expect(disconnects()).toHaveLength(1);
    expect(disconnects()[0]?.body?.confirm).toBeUndefined();

    // Disabled until the name matches: a click sends nothing.
    const confirm = within(dialog).getByRole("button", { name: "Disconnect container" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(disconnects()).toHaveLength(1);

    disconnectAnswer = () => new Response(null, { status: 204 });
    fireEvent.change(input, { target: { value: "web" } });
    expect(confirm.getAttribute("aria-disabled")).toBe("false");
    fireEvent.click(confirm);
    await waitFor(() => expect(disconnects()).toHaveLength(2));
    expect(disconnects()[1]?.body).toMatchObject({ containerId: "web-id", confirm: "web" });
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("keeps a refusal inside the dialog, and treats 'not connected' as already done", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    withRows();
    disconnectAnswer = () => problem(503, "node_unavailable");
    renderAt(ATTACHMENTS);
    fireEvent.click(await screen.findByRole("button", { name: "Disconnect web from app-net" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "That node isn't reachable right now.",
    );
    expect(screen.getByRole("alertdialog")).toBeTruthy();

    disconnectAnswer = () => problem(409, "not_connected");
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    expect(await screen.findByText("web isn't on app-net any more.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(disconnects()).toHaveLength(2);
  });
});
