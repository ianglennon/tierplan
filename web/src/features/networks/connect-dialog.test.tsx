// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { NetworkAttachment } from "@/api/networks";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ConnectDialog } from "@/features/networks/connect-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The connect dialog (plan 04-15, D-43, D-51): a container joins a network from the
// network's page, a network from the container's page; every refusal stays inside.

const ENV = "e1";
const NET = { id: "app0", name: "app-net", scope: "local", nodeId: "n1", attachable: false };

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let client: QueryClient;
let containers: unknown;
let attachments: NetworkAttachment[];
let networks: unknown;
let containerNets: Array<{ networkId: string; name: string }>;
let answer: () => Response | Promise<Response>;

function container(name: string, nodeId = "n1") {
  return {
    id: `${name}-id`,
    name,
    node: nodeId === "n1" ? "node-1" : "node-2",
    nodeId,
    image: "nginx:1",
    state: "running",
    status: "Up",
    health: "none",
    ports: [],
    service: "",
    stack: "",
    project: "",
    isSelf: false,
    selfKind: "",
    readOnly: false,
    created: "2026-10-01T10:00:00Z",
  };
}

function network(id: string, name: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    name,
    driver: "bridge",
    scope: "local",
    node: "node-1",
    nodeId: "n1",
    subnets: [],
    internal: false,
    attachable: false,
    ipv6: false,
    builtIn: false,
    owner: "",
    ownerKind: "",
    attachedContainers: 0,
    attachedServices: 0,
    ...extra,
  };
}

function page<T>(items: T[]) {
  return { items, total: items.length, truncated: false, nodes: ["node-1"], notReporting: [] };
}

function attached(name: string): NetworkAttachment {
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
  };
}

const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

beforeEach(() => {
  sent = [];
  containers = page([container("web"), container("db"), container("far", "n2"), container("api")]);
  attachments = [attached("db")];
  networks = page([
    network("app0", "app-net"),
    network("back0", "back-net"),
    network("h0", "host", { driver: "host" }),
  ]);
  containerNets = [{ networkId: "br0", name: "bridge" }];
  answer = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const entry: Sent = {
        method: req.method,
        path: url.pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      switch (url.pathname) {
        case "/api/me":
          return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
        case `/api/env/${ENV}/containers`:
          return Response.json(containers);
        case `/api/env/${ENV}/networks`:
          return Response.json(networks);
        case `/api/env/${ENV}/networks/app0`:
          return Response.json({
            ...network("app0", "app-net"),
            created: "2026-10-01T10:00:00Z",
            labels: {},
            options: {},
            ipam: [],
            attachments,
            attachedContainers: attachments.length,
            attachedServices: 0,
            aliasesTruncated: false,
            notReporting: [],
          });
        case `/api/env/${ENV}/networks/app0/connect`:
        case `/api/env/${ENV}/networks/back0/connect`:
          return answer();
        case `/api/env/${ENV}/containers/web-id`:
          return Response.json({
            id: "web-id",
            name: "web",
            networks: containerNets.map((n) => ({
              ...n,
              ipv4: "",
              ipv6: "",
              mac: "",
              aliases: [],
            })),
          });
        case "/api/environments":
          return Response.json({ items: [] });
        default:
          return Response.json({}, { status: 404 });
      }
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

const connects = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/connect"));

function wrap(node: React.ReactNode) {
  return (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          {node}
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
}

function mountNetwork(extra: Partial<typeof NET> = {}) {
  const onOpenChange = vi.fn();
  render(
    wrap(
      <ConnectDialog envId={ENV} open onOpenChange={onOpenChange} network={{ ...NET, ...extra }} />,
    ),
  );
  return { onOpenChange };
}

function mountContainer(selfKind?: string) {
  const onOpenChange = vi.fn();
  render(
    wrap(
      <ConnectDialog
        envId={ENV}
        open
        onOpenChange={onOpenChange}
        container={{ id: "web-id", name: "web", nodeId: "n1", ...(selfKind ? { selfKind } : {}) }}
      />,
    ),
  );
  return { onOpenChange };
}

// choose opens the combobox and picks the row with the text.
async function choose(dialog: HTMLElement, label: string, option: string) {
  fireEvent.click(within(dialog).getByRole("combobox", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: new RegExp(option) }));
}

describe("ConnectDialog from a network", () => {
  it("opens with the title, the explanation and Connect container disabled until a container is chosen", async () => {
    mountNetwork();
    const dialog = await screen.findByRole("dialog", { name: "Connect a container to app-net" });
    expect(dialog.textContent).toContain("The container is on the network straight away.");
    expect(
      within(dialog).getByRole("button", { name: "Connect container" }).hasAttribute("disabled"),
    ).toBe(true);
    expect(connects()).toEqual([]);
  });

  it("offers only containers that are not on the network, and for a local network only those of its node", async () => {
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await waitFor(() =>
      expect(within(dialog).getByRole("combobox", { name: "Container" })).toBeTruthy(),
    );
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Container" }));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options.some((o) => o?.startsWith("web"))).toBe(true);
    expect(options.some((o) => o?.startsWith("api"))).toBe(true);
    // db is already on it; far is on another node.
    expect(options.some((o) => o?.startsWith("db"))).toBe(false);
    expect(options.some((o) => o?.startsWith("far"))).toBe(false);
  });

  it("connects the chosen container with its alias and addresses, says so, and closes", async () => {
    const { onOpenChange } = mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Container", "web");
    fireEvent.change(within(dialog).getByLabelText("Alias (optional)"), {
      target: { value: " db " },
    });
    fireEvent.change(within(dialog).getByLabelText("IPv4 address (optional)"), {
      target: { value: "10.20.0.5" },
    });
    fireEvent.change(within(dialog).getByLabelText("IPv6 address (optional)"), {
      target: { value: "fd00:20::5" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));

    await waitFor(() => expect(connects()).toHaveLength(1));
    expect(connects()[0]).toEqual({
      method: "POST",
      path: `/api/env/${ENV}/networks/app0/connect`,
      body: {
        containerId: "web-id",
        nodeId: "n1",
        alias: "db",
        ipv4: "10.20.0.5",
        ipv6: "fd00:20::5",
      },
    });
    expect(await screen.findByText("Connected web to app-net.")).toBeTruthy();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("sends only the container and node when the optional fields are left empty", async () => {
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Container", "api");
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    await waitFor(() => expect(connects()).toHaveLength(1));
    expect(connects()[0]?.body).toEqual({ containerId: "api-id", nodeId: "n1" });
  });

  it("shows 'Every container is already on this network.' and keeps Connect container disabled when nothing is left", async () => {
    containers = page([container("db")]);
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    expect(
      await within(dialog).findByText("Every container is already on this network."),
    ).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Connect container" }).hasAttribute("disabled"),
    ).toBe(true);
  });

  it("shows a spinner row in the list while the containers load", async () => {
    containers = new Promise(() => {});
    vi.mocked(fetch).mockImplementation(async (req) => {
      const url = new URL((req as Request).url);
      if (url.pathname === `/api/env/${ENV}/containers`) return new Promise<Response>(() => {});
      return Response.json({}, { status: 404 });
    });
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Container" }));
    expect(await screen.findByText("Loading containers")).toBeTruthy();
  });

  it("refuses a bad IPv4 or IPv6 before anything is sent", async () => {
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Container", "web");
    fireEvent.change(within(dialog).getByLabelText("IPv4 address (optional)"), {
      target: { value: "10.20.0" },
    });
    fireEvent.change(within(dialog).getByLabelText("IPv6 address (optional)"), {
      target: { value: "10.20.0.5" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    expect(await within(dialog).findByText("Enter an address like 10.20.0.5.")).toBeTruthy();
    expect(within(dialog).getByText("Enter an address like fd00:20::5.")).toBeTruthy();
    expect(connects()).toEqual([]);
  });

  it("keeps the dialog open with the engine's refusal in words when the server says no", async () => {
    answer = () => problem(409, "network_unavailable_on_node");
    const { onOpenChange } = mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Container", "web");
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("That node doesn't know app-net yet");
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(connects()).toHaveLength(1);
  });

  it("words the other refusals and never retries", async () => {
    answer = () => problem(422, "address_invalid");
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Container", "web");
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "didn't accept that address",
    );

    answer = () => problem(409, "already_connected");
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    await waitFor(() =>
      expect(within(dialog).getByRole("alert").textContent).toContain("web is already on app-net."),
    );
    expect(connects()).toHaveLength(2);
  });

  it("offers nothing and says why for a swarm overlay that is not attachable", async () => {
    mountNetwork({ scope: "swarm", nodeId: "", attachable: false });
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/isn't attachable/)).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Connect container" }).hasAttribute("disabled"),
    ).toBe(true);
  });
});

describe("ConnectDialog from a container", () => {
  it("offers the networks the container can join and connects the chosen one", async () => {
    const { onOpenChange } = mountContainer();
    const dialog = await screen.findByRole("dialog", { name: "Connect web to a network" });
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Network" }));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options.some((o) => o?.startsWith("app-net"))).toBe(true);
    expect(options.some((o) => o?.startsWith("back-net"))).toBe(true);
    // The host network takes no endpoint.
    expect(options.some((o) => o?.startsWith("host"))).toBe(false);
    fireEvent.click(await screen.findByRole("option", { name: /back-net/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));

    await waitFor(() => expect(connects()).toHaveLength(1));
    expect(connects()[0]).toEqual({
      method: "POST",
      path: `/api/env/${ENV}/networks/back0/connect`,
      body: { containerId: "web-id", nodeId: "n1" },
    });
    expect(await screen.findByText("Connected web to back-net.")).toBeTruthy();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("leaves out the networks the container is on, a non-attachable overlay and another node's bridge", async () => {
    containerNets = [{ networkId: "app0", name: "app-net" }];
    networks = page([
      network("app0", "app-net"),
      network("back0", "back-net"),
      network("ovl0", "closed-ovl", {
        scope: "swarm",
        driver: "overlay",
        nodeId: "",
        node: "Cluster",
      }),
      network("ovl1", "open-ovl", {
        scope: "swarm",
        driver: "overlay",
        nodeId: "",
        node: "Cluster",
        attachable: true,
      }),
      network("far0", "far-net", { nodeId: "n2", node: "node-2" }),
    ]);
    mountContainer();
    const dialog = await screen.findByRole("dialog");
    await waitFor(() =>
      expect(within(dialog).getByRole("combobox", { name: "Network" })).toBeTruthy(),
    );
    // Wait for the container's networks so app-net is known to be taken.
    await waitFor(() =>
      expect(sent.some((s) => s.path === `/api/env/${ENV}/containers/web-id`)).toBe(true),
    );
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Network" }));
    await waitFor(async () => {
      const names = (await screen.findAllByRole("option")).map((o) => o.textContent ?? "");
      expect(names.some((o) => o.startsWith("app-net"))).toBe(false);
    });
    const names = (await screen.findAllByRole("option")).map((o) => o.textContent ?? "");
    expect(names.some((o) => o.startsWith("back-net"))).toBe(true);
    expect(names.some((o) => o.startsWith("open-ovl"))).toBe(true);
    expect(names.some((o) => o.startsWith("closed-ovl"))).toBe(false);
    expect(names.some((o) => o.startsWith("far-net"))).toBe(false);
  });
});

describe("ConnectDialog for Bayplan's own containers", () => {
  it("needs the name of an own container typed before it connects, from the container's page", async () => {
    mountContainer("server");
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Network", "back-net");
    const connect = within(dialog).getByRole("button", { name: "Connect container" });
    expect(within(dialog).getByText(/one of Bayplan's own containers/)).toBeTruthy();
    expect(connect.hasAttribute("disabled")).toBe(true);
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Type web to confirm" }), {
      target: { value: "web" },
    });
    expect(connect.hasAttribute("disabled")).toBe(false);
    fireEvent.click(connect);
    await waitFor(() => expect(connects()).toHaveLength(1));
    expect(connects()[0]?.body).toMatchObject({ containerId: "web-id", confirm: "web" });
  });

  it("needs the name of an own container chosen on a network's page", async () => {
    containers = page([
      { ...container("bayplan"), selfKind: "server", isSelf: true },
      container("web"),
    ]);
    mountNetwork();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Container", "web");
    expect(within(dialog).queryByRole("textbox", { name: /to confirm/ })).toBeNull();
    await choose(dialog, "Container", "bayplan");
    const input = await within(dialog).findByRole("textbox", { name: "Type bayplan to confirm" });
    expect(
      within(dialog).getByRole("button", { name: "Connect container" }).hasAttribute("disabled"),
    ).toBe(true);
    fireEvent.change(input, { target: { value: "bayplan" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    await waitFor(() => expect(connects()).toHaveLength(1));
    expect(connects()[0]?.body).toMatchObject({ containerId: "bayplan-id", confirm: "bayplan" });
  });

  it("asks for the name when the server says the container is Bayplan's own though the page did not know", async () => {
    mountContainer();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Network", "back-net");
    expect(within(dialog).queryByRole("textbox", { name: /to confirm/ })).toBeNull();
    answer = () => problem(422, "confirmation_required");
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    await within(dialog).findByRole("textbox", { name: "Type web to confirm" });
    expect(connects()).toHaveLength(1);
    expect(connects()[0]?.body?.confirm).toBeUndefined();
  });

  it("sends no confirm for an ordinary container", async () => {
    mountContainer();
    const dialog = await screen.findByRole("dialog");
    await choose(dialog, "Network", "back-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Connect container" }));
    await waitFor(() => expect(connects()).toHaveLength(1));
    expect(connects()[0]?.body).not.toHaveProperty("confirm");
  });
});
