// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { NetworkRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CreateNetworkDialog } from "@/features/networks/create-network-dialog";
import { NetworkList } from "@/features/networks/network-list";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let list: unknown;
let listFor: ((node: string) => unknown) | undefined;
let environments: unknown;

const created = () => Response.json({ id: "net-1", name: "app-net" }, { status: 201 });
const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = created;
  list = {
    items: [
      {
        id: "n1",
        name: "bridge",
        driver: "bridge",
        scope: "local",
        node: "node-1",
        nodeId: "node-1-id",
        subnets: ["172.17.0.0/16"],
        created: "2026-10-01T10:00:00Z",
      },
    ],
    total: 1,
    truncated: false,
    nodes: ["node-1"],
    notReporting: [],
  };
  listFor = undefined;
  environments = { items: [] };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        method: req.method,
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === `/api/env/${ENV}/networks` && req.method === "POST") return answer(entry);
      if (entry.path === `/api/env/${ENV}/networks`) {
        const node = new URL(req.url).searchParams.get("node");
        if (node && listFor) return Response.json(listFor(node));
        return Response.json(list);
      }
      if (entry.path === "/api/environments") return Response.json(environments);
      return Response.json({}, { status: 404 });
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

const posts = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/networks"));

function wrap(node: React.ReactNode) {
  return (
    <QueryClientProvider client={client}>
      <TooltipProvider>
        {node}
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>
  );
}

function mount(props: Partial<Parameters<typeof CreateNetworkDialog>[0]> = {}) {
  const onOpenChange = vi.fn();
  render(
    wrap(
      <CreateNetworkDialog
        envId={ENV}
        open
        onOpenChange={onOpenChange}
        swarm={false}
        networks={[]}
        {...props}
      />,
    ),
  );
  return { onOpenChange };
}

async function typeName(dialog: HTMLElement, name: string) {
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: name } });
}

describe("CreateNetworkDialog (common fields)", () => {
  it("opens with the title, the explanation, bridge as the driver off a swarm, and nothing sent", async () => {
    mount();
    const dialog = await screen.findByRole("dialog", { name: "Create network" });
    expect(dialog.textContent).toContain("A network lets containers reach each other by name.");
    expect(within(dialog).getByRole("combobox", { name: "Driver" }).textContent).toContain(
      "bridge",
    );
    expect(posts()).toEqual([]);
  });

  it("defaults the driver to overlay on a swarm", async () => {
    mount({ swarm: true });
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("combobox", { name: "Driver" }).textContent).toContain(
      "overlay",
    );
  });

  it("disables Attachable, with its helper, for any driver but overlay on a swarm", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    const attachable = within(dialog).getByRole("switch", { name: "Attachable" });
    expect(attachable.hasAttribute("disabled")).toBe(true);
    expect(dialog.textContent).toContain(
      "Lets standalone containers join an overlay network. Only for overlay networks on a swarm.",
    );
  });

  it("enables Attachable for an overlay network on a swarm", async () => {
    mount({ swarm: true });
    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByRole("switch", { name: "Attachable" }).hasAttribute("disabled"),
    ).toBe(false);
  });

  it("asks for a name and refuses one with a space, sending nothing", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("Enter a name.")).toBeTruthy();
    await typeName(dialog, "my net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(
      await within(dialog).findByText("Use letters, numbers, dot, dash or underscore."),
    ).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("posts the name, driver and labels, toasts, and closes", async () => {
    const { onOpenChange } = mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Add label" }));
    fireEvent.change(within(dialog).getByLabelText("Label name"), { target: { value: "team" } });
    fireEvent.change(within(dialog).getByLabelText("Label value"), { target: { value: "a" } });
    fireEvent.click(within(dialog).getByRole("switch", { name: "Internal" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "app-net",
      driver: "bridge",
      internal: true,
      labels: { team: "a" },
    });
    expect(await screen.findByText("Created network app-net.")).toBeTruthy();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("leaves labels and the switches out of the request when they are not used", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "plain");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "plain", driver: "bridge" });
  });

  it("sends one request when Create is pressed twice, and shows the pending label", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await within(dialog).findByRole("button", { name: "Creating…" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Creating…" }));
    expect(posts()).toHaveLength(1);
    release(created());
  });

  it("says the name is taken under the Name field and keeps the dialog open", async () => {
    answer = () => problem(409, "name_taken");
    const { onOpenChange } = mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("A network named app-net already exists.")).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("shows a read-only host inside the dialog as an alert", async () => {
    answer = () => problem(409, "host_read_only");
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("This host is read-only.");
  });

  it("shows a node that cannot be reached in the words of the spec", async () => {
    answer = () => problem(503, "node_unavailable");
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "isn't reachable right now",
    );
  });

  it("shows markup in a name as the validation message, never as markup", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "<img src=x onerror=alert(1)>");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await within(dialog).findByText("Use letters, numbers, dot, dash or underscore.");
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("NetworkList create button", () => {
  it("shows an env Admin an enabled Create network button that opens the dialog", async () => {
    render(wrap(<NetworkList envId={ENV} onNodeChange={() => {}} onSearchChange={() => {}} />));
    const button = await screen.findByRole("button", { name: "Create network" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(button);
    expect(await screen.findByRole("dialog", { name: "Create network" })).toBeTruthy();
  });

  it("shows an Operator the button disabled with Needs Admin, and a click opens nothing", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    render(wrap(<NetworkList envId={ENV} onNodeChange={() => {}} onSearchChange={() => {}} />));
    const button = await screen.findByRole("button", { name: "Create network" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Needs Admin");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows a Viewer the same disabled button", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    render(wrap(<NetworkList envId={ENV} onNodeChange={() => {}} onSearchChange={() => {}} />));
    const button = await screen.findByRole("button", { name: "Create network" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
  });
});

// Advanced addressing, the overlap warning and the node of a local network (plan
// 04-12 task 2).

const row = (name: string, subnets: string[], extra: Partial<NetworkRow> = {}): NetworkRow => ({
  id: `id-${name}`,
  name,
  driver: "bridge",
  scope: "local",
  node: "node-1",
  nodeId: "node-1-id",
  subnets,
  created: "2026-10-01T10:00:00Z",
  builtIn: false,
  attachedContainers: 0,
  attachedServices: 0,
  owner: "",
  ownerKind: "",
  internal: false,
  attachable: false,
  ipv6: false,
  ...extra,
});

async function openAdvanced(dialog: HTMLElement) {
  fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
  return await within(dialog).findByLabelText("Subnet");
}

function blurSubnet(input: HTMLElement, value: string) {
  fireEvent.change(input, { target: { value } });
  fireEvent.blur(input);
}

describe("CreateNetworkDialog (Advanced)", () => {
  it("keeps Advanced collapsed until it is opened, then shows the addressing fields", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByLabelText("Subnet")).toBeNull();
    const subnet = await openAdvanced(dialog);
    expect(subnet.getAttribute("placeholder")).toBe("10.20.0.0/24");
    expect(dialog.textContent).toContain(
      "In CIDR form. Bayplan warns if it overlaps another network but doesn't pick one for you.",
    );
    expect(within(dialog).getByLabelText("Gateway")).toBeTruthy();
    expect(within(dialog).getByLabelText("IP range")).toBeTruthy();
    expect(within(dialog).getByRole("switch", { name: "Enable IPv6" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Add option" })).toBeTruthy();
  });

  it("scrolls within 90svh", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    expect(dialog.className).toContain("max-h-[90svh]");
    expect(dialog.className).toContain("overflow-y-auto");
  });

  it("posts the subnet, gateway, IP range, IPv6 and driver options", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    const subnet = await openAdvanced(dialog);
    fireEvent.change(subnet, { target: { value: "10.20.0.0/24" } });
    fireEvent.change(within(dialog).getByLabelText("Gateway"), { target: { value: "10.20.0.1" } });
    fireEvent.change(within(dialog).getByLabelText("IP range"), {
      target: { value: "10.20.0.128/25" },
    });
    fireEvent.click(within(dialog).getByRole("switch", { name: "Enable IPv6" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Add option" }));
    fireEvent.change(within(dialog).getByLabelText("Option name"), {
      target: { value: "com.docker.network.bridge.name" },
    });
    fireEvent.change(within(dialog).getByLabelText("Option value"), {
      target: { value: "br-app" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "app-net",
      driver: "bridge",
      subnet: "10.20.0.0/24",
      gateway: "10.20.0.1",
      ipRange: "10.20.0.128/25",
      ipv6: true,
      options: { "com.docker.network.bridge.name": "br-app" },
    });
  });

  it("refuses a subnet that is not in CIDR form, a gateway or range outside it, and sends nothing", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    const subnet = await openAdvanced(dialog);
    fireEvent.change(subnet, { target: { value: "10.20.0.0" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("Enter a subnet like 10.20.0.0/24.")).toBeTruthy();

    fireEvent.change(subnet, { target: { value: "10.20.0.0/24" } });
    fireEvent.change(within(dialog).getByLabelText("Gateway"), { target: { value: "10.20.1.1" } });
    fireEvent.change(within(dialog).getByLabelText("IP range"), {
      target: { value: "10.20.1.0/25" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("The gateway must be inside the subnet.")).toBeTruthy();
    expect(within(dialog).getByText("The IP range must be inside the subnet.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("asks for a subnet when a gateway or an IP range is given without one", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    await openAdvanced(dialog);
    fireEvent.change(within(dialog).getByLabelText("Gateway"), { target: { value: "10.20.0.1" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("Enter a subnet like 10.20.0.0/24.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("puts a subnet problem the server found under the Subnet field", async () => {
    answer = () => problem(422, "gateway_outside_subnet");
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("The gateway must be inside the subnet.")).toBeTruthy();
  });
});

describe("CreateNetworkDialog (overlap warning)", () => {
  const OVERLAPS = "Containers on both networks may not reach each other. You can still create it.";

  it("warns on blur when the subnet overlaps a listed network, names it, and leaves Create enabled", async () => {
    mount({ networks: [row("bridge", ["172.17.0.0/16"])] });
    const dialog = await screen.findByRole("dialog");
    const subnet = await openAdvanced(dialog);
    fireEvent.change(subnet, { target: { value: "172.17.1.0/24" } });
    expect(within(dialog).queryByText(/This overlaps/)).toBeNull();
    fireEvent.blur(subnet);
    expect(
      await within(dialog).findByText(`This overlaps bridge (172.17.0.0/16). ${OVERLAPS}`),
    ).toBeTruthy();
    const create = within(dialog).getByRole("button", { name: "Create network" });
    expect(create.hasAttribute("disabled")).toBe(false);
    expect(create.getAttribute("aria-disabled")).toBeNull();
  });

  it("does not warn for an adjacent subnet", async () => {
    mount({ networks: [row("app", ["10.0.0.0/24"])] });
    const dialog = await screen.findByRole("dialog");
    blurSubnet(await openAdvanced(dialog), "10.0.1.0/24");
    expect(within(dialog).queryByText(/This overlaps/)).toBeNull();
  });

  it("names two networks, and for more names the first two and the rest as 'and {m} more'", async () => {
    mount({
      networks: [
        row("a", ["10.0.1.0/24"]),
        row("b", ["10.0.2.0/24"]),
        row("c", ["10.0.3.0/24"]),
        row("d", ["10.0.4.0/24"]),
      ],
    });
    const dialog = await screen.findByRole("dialog");
    const subnet = await openAdvanced(dialog);
    blurSubnet(subnet, "10.0.2.0/23");
    expect(await within(dialog).findByText(/This overlaps 2 networks: b and c\./)).toBeTruthy();
    blurSubnet(subnet, "10.0.0.0/16");
    expect(
      await within(dialog).findByText(/This overlaps 4 networks: a, b and 2 more\./),
    ).toBeTruthy();
  });

  it("takes the warning away when the subnet no longer overlaps, and still creates with one showing", async () => {
    mount({ networks: [row("bridge", ["172.17.0.0/16"])] });
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    const subnet = await openAdvanced(dialog);
    blurSubnet(subnet, "172.17.1.0/24");
    await within(dialog).findByText(/This overlaps bridge/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toMatchObject({ subnet: "172.17.1.0/24" });
  });

  it("clears the warning on a blur that finds no overlap", async () => {
    mount({ networks: [row("bridge", ["172.17.0.0/16"])] });
    const dialog = await screen.findByRole("dialog");
    const subnet = await openAdvanced(dialog);
    blurSubnet(subnet, "172.17.1.0/24");
    await within(dialog).findByText(/This overlaps bridge/);
    blurSubnet(subnet, "10.99.0.0/24");
    await waitFor(() => expect(within(dialog).queryByText(/This overlaps/)).toBeNull());
  });

  it("shows a network name as text, never as markup", async () => {
    mount({ networks: [row("<img src=x onerror=alert(1)>", ["10.0.0.0/24"])] });
    const dialog = await screen.findByRole("dialog");
    blurSubnet(await openAdvanced(dialog), "10.0.0.0/25");
    await within(dialog).findByText(/This overlaps/);
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("CreateNetworkDialog (node of a local network on a swarm)", () => {
  const SWARM_NETS = [
    row("bridge", ["172.17.0.0/16"], { node: "node-1", nodeId: "node-1-id" }),
    row("bridge", ["172.17.0.0/16"], { node: "node-2", nodeId: "node-2-id" }),
    row("ingress", ["10.0.0.0/24"], {
      driver: "overlay",
      scope: "swarm",
      node: "Cluster",
      nodeId: "mgr-id",
    }),
  ];

  async function chooseDriver(dialog: HTMLElement, name: string) {
    fireEvent.keyDown(within(dialog).getByRole("combobox", { name: "Driver" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name }));
  }

  it("shows no node select for an overlay network", async () => {
    mount({ swarm: true, networks: SWARM_NETS });
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByRole("combobox", { name: "Create on node" })).toBeNull();
  });

  it("asks which node for a local driver, and refuses Create until one is chosen", async () => {
    mount({ swarm: true, networks: SWARM_NETS });
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    await chooseDriver(dialog, "bridge");
    const node = await within(dialog).findByRole("combobox", { name: "Create on node" });
    expect(node.textContent).toContain("Choose a node");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect(await within(dialog).findByText("Choose a node.")).toBeTruthy();
    expect(posts()).toEqual([]);

    fireEvent.keyDown(node, { key: "Enter" });
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["node-1", "node-2"]);
    fireEvent.click(screen.getByRole("option", { name: "node-2" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "app-net", driver: "bridge", nodeId: "node-2-id" });
  });

  it("sends an overlay network with no node, attachable when asked", async () => {
    mount({ swarm: true, networks: SWARM_NETS });
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "ovl");
    fireEvent.click(within(dialog).getByRole("switch", { name: "Attachable" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "ovl", driver: "overlay", attachable: true });
  });

  it("drops the attachable choice when the driver changes to one that cannot have it", async () => {
    mount({ swarm: true, networks: SWARM_NETS });
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("switch", { name: "Attachable" }));
    await chooseDriver(dialog, "bridge");
    fireEvent.keyDown(await within(dialog).findByRole("combobox", { name: "Create on node" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("option", { name: "node-1" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "app-net", driver: "bridge", nodeId: "node-1-id" });
  });

  it("shows no node select off a swarm, whatever the driver", async () => {
    mount({ networks: [row("bridge", ["172.17.0.0/16"])] });
    const dialog = await screen.findByRole("dialog");
    await chooseDriver(dialog, "macvlan");
    expect(within(dialog).queryByRole("combobox", { name: "Create on node" })).toBeNull();
  });
});

describe("NetworkList create dialog on a swarm list narrowed to one node", () => {
  it("still offers every node and checks the subnet against every node's networks", async () => {
    const mk = (name: string, node: string, nodeId: string, subnet: string) => ({
      id: `id-${name}-${node}`,
      name,
      driver: "bridge",
      scope: "local",
      node,
      nodeId,
      subnets: [subnet],
      created: "2026-10-01T10:00:00Z",
    });
    const all = [
      mk("bridge", "node-1", "node-1-id", "172.17.0.0/16"),
      mk("other", "node-2", "node-2-id", "10.50.0.0/24"),
    ];
    environments = { items: [{ id: ENV, envType: "swarm" }] };
    list = {
      items: all,
      total: 2,
      truncated: false,
      nodes: ["node-1", "node-2"],
      notReporting: [],
    };
    listFor = () => ({
      items: [all[0]],
      total: 1,
      truncated: false,
      nodes: ["node-1"],
      notReporting: [],
    });
    render(
      wrap(
        <NetworkList envId={ENV} node="node-1" onNodeChange={() => {}} onSearchChange={() => {}} />,
      ),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Create network" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.keyDown(within(dialog).getByRole("combobox", { name: "Driver" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "bridge" }));
    fireEvent.keyDown(await within(dialog).findByRole("combobox", { name: "Create on node" }), {
      key: "Enter",
    });
    await waitFor(async () =>
      expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
        "node-1",
        "node-2",
      ]),
    );
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    const subnet = await within(dialog).findByLabelText("Subnet");
    fireEvent.change(subnet, { target: { value: "10.50.0.0/25" } });
    fireEvent.blur(subnet);
    expect(
      await within(dialog).findByText(/This overlaps other \(10\.50\.0\.0\/24\)/),
    ).toBeTruthy();
  });
});
