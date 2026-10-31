// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

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
import { CHECK_DEBOUNCE_MS, checkTiming } from "@/features/subnet-picker/use-subnet-check";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

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
// What the stubbed server answers to a subnet check and to the address space.
let checkAnswer: (req: Sent) => Response | Promise<Response>;
let addressSpace: unknown;

const cleanCheck = () =>
  Response.json({ findings: [], routeChecks: [], ipv6NotChecked: false, tooManyTasks: false });
const overlapCheck = () =>
  Response.json({
    findings: [
      {
        kind: "same_env",
        severity: "blocking",
        network: "shop_default",
        subnet: "10.0.2.0/24",
        node: "node-1",
      },
    ],
    routeChecks: [],
    ipv6NotChecked: false,
    tooManyTasks: false,
  });

const created = () => Response.json({ id: "net-1", name: "app-net" }, { status: 201 });
const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

beforeEach(() => {
  sent = [];
  checkTiming.debounceMs = 0;
  checkAnswer = cleanCheck;
  addressSpace = undefined;
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
      if (entry.path === `/api/env/${ENV}/address-space/subnet-check`) return checkAnswer(entry);
      if (entry.path === `/api/env/${ENV}/address-space` && addressSpace !== undefined) {
        return Response.json(addressSpace);
      }
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
  checkTiming.debounceMs = CHECK_DEBOUNCE_MS;
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

const posts = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/networks"));
const checks = () => sent.filter((s) => s.path.endsWith("/subnet-check"));

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

describe("CreateNetworkDialog (Advanced)", () => {
  it("keeps Advanced collapsed until it is opened, then shows the addressing fields", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    const trigger = within(dialog).getByRole("button", { name: "Advanced" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(within(dialog).queryByRole("textbox", { name: "Subnet" })).toBeNull();
    const subnet = await openAdvanced(dialog);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(subnet.getAttribute("placeholder")).toBe("10.0.5.0/26");
    expect(dialog.textContent).toContain(
      "Leave this empty and Docker picks one from the pool. Choose one to control how much address space the network uses.",
    );
    expect(within(dialog).getByRole("combobox", { name: "Size" })).toBeTruthy();
    expect(within(dialog).getByLabelText("Expected tasks")).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: /Suggest a free subnet/ })).toBeTruthy();
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

describe("CreateNetworkDialog (subnet picker)", () => {
  const suggestion = () =>
    Response.json({
      findings: [],
      routeChecks: [],
      ipv6NotChecked: false,
      tooManyTasks: false,
      suggestion: { subnet: "10.0.2.64/26", pool: "10.0.0.0/8" },
    });

  it("suggests a subnet and creates the network with it", async () => {
    checkAnswer = (req) => (req.body?.suggest ? suggestion() : cleanCheck());
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    const subnet = await openAdvanced(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: /Suggest a free subnet/ }));
    await waitFor(() => expect((subnet as HTMLInputElement).value).toBe("10.0.2.64/26"));
    expect(document.activeElement).toBe(subnet);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "app-net",
      driver: "bridge",
      subnet: "10.0.2.64/26",
    });
  });

  it("keeps the dialog's layout: 560px wide, Subnet and Suggest on one row", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    expect(dialog.className).toContain("sm:max-w-140");
    const subnet = await openAdvanced(dialog);
    const suggest = within(dialog).getByRole("button", { name: /Suggest a free subnet/ });
    expect(suggest.className).toContain("h-11");
    expect(suggest.className).toContain("md:h-8");
    expect(subnet.closest("[data-slot=field]")?.parentElement).toBe(suggest.parentElement);
  });

  it("asks the server about the subnet typed, with the driver, and shows the answer", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(await openAdvanced(dialog), { target: { value: "10.0.5.0/26" } });
    expect(await within(dialog).findByText("No overlaps found.")).toBeTruthy();
    expect(checks().at(-1)?.body).toEqual({ driver: "bridge", subnet: "10.0.5.0/26" });
  });

  it("blocks create until create anyway", async () => {
    checkAnswer = overlapCheck;
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    const subnet = await openAdvanced(dialog);
    fireEvent.change(subnet, { target: { value: "10.0.2.0/26" } });
    expect(
      await within(dialog).findByText(/Overlaps shop_default \(10\.0\.2\.0\/24\) on node-1\./),
    ).toBeTruthy();
    expect(subnet.getAttribute("aria-invalid")).toBe("true");

    const create = within(dialog).getByRole("button", { name: "Create network" });
    expect(create.getAttribute("aria-disabled")).toBe("true");
    expect(create.hasAttribute("disabled")).toBe(false);
    expect(create.getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Resolve the overlap, or tick Create it anyway.");
    fireEvent.click(create);
    expect(posts()).toEqual([]);

    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Create it anyway" }));
    expect(await within(dialog).findByText("Overlap accepted")).toBeTruthy();
    const enabled = within(dialog).getByRole("button", { name: "Create network" });
    expect(enabled.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(enabled);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "app-net",
      driver: "bridge",
      subnet: "10.0.2.0/26",
      createAnyway: true,
    });
  });

  it("does not send createAnyway once the overlap is gone, and asks again for a new one", async () => {
    checkAnswer = (req) => (req.body?.subnet === "10.0.2.0/26" ? overlapCheck() : cleanCheck());
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    const subnet = await openAdvanced(dialog);
    fireEvent.change(subnet, { target: { value: "10.0.2.0/26" } });
    fireEvent.click(await within(dialog).findByRole("checkbox", { name: "Create it anyway" }));
    fireEvent.change(subnet, { target: { value: "10.0.9.0/26" } });
    await within(dialog).findByText("No overlaps found.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "app-net", driver: "bridge", subnet: "10.0.9.0/26" });

    // The tick went with the overlap it was for.
    fireEvent.change(subnet, { target: { value: "10.0.2.0/26" } });
    expect(
      (await within(dialog).findByRole("checkbox", { name: "Create it anyway" })).getAttribute(
        "aria-checked",
      ),
    ).toBe("false");
  });

  it("does not block create for a warning", async () => {
    checkAnswer = () =>
      Response.json({
        findings: [{ kind: "common_range", severity: "warning", range: "192.168.0.0/16" }],
        routeChecks: [],
        ipv6NotChecked: false,
        tooManyTasks: false,
      });
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.change(await openAdvanced(dialog), { target: { value: "192.168.4.0/24" } });
    await within(dialog).findByText(/Falls inside 192\.168\.0\.0\/16/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).not.toHaveProperty("createAnyway");
  });

  it("shows the check error without blocking create", async () => {
    checkAnswer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.change(await openAdvanced(dialog), { target: { value: "10.0.5.0/26" } });
    expect(
      await within(dialog).findByText(
        "Couldn't check this subnet. You can still create the network.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  it("puts a refusal for an overlap the check missed in an alert, with Advanced open", async () => {
    answer = () => problem(422, "subnet_overlap");
    mount();
    const dialog = await screen.findByRole("dialog");
    await typeName(dialog, "app-net");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create network" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "Another network in this environment overlaps this subnet.",
    );
    expect(
      within(dialog).getByRole("button", { name: "Advanced" }).getAttribute("aria-expanded"),
    ).toBe("true");
  });

  it("opens advanced at 70 percent, with the line that says why", async () => {
    addressSpace = {
      envType: "swarm",
      pools: [
        { cidr: "10.0.0.0/8", mask: 24, percent: 72, state: "getting_full", source: "swarm" },
        { cidr: "172.20.0.0/16", mask: 24, percent: 10, state: "normal", source: "entered" },
      ],
    };
    mount({ swarm: true });
    const dialog = await screen.findByRole("dialog");
    expect(
      await within(dialog).findByText("This pool is 72% used. A smaller subnet keeps more room."),
    ).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Advanced" }).getAttribute("aria-expanded"),
    ).toBe("true");
    expect(within(dialog).getByRole("combobox", { name: "Size" }).textContent).toContain("/24");
  });

  it("leaves advanced closed below 70 percent", async () => {
    addressSpace = {
      envType: "swarm",
      pools: [{ cidr: "10.0.0.0/8", mask: 26, percent: 40, state: "normal", source: "swarm" }],
    };
    mount({ swarm: true });
    const dialog = await screen.findByRole("dialog");
    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/address-space"))).toBe(true));
    expect(
      within(dialog).getByRole("button", { name: "Advanced" }).getAttribute("aria-expanded"),
    ).toBe("false");
    expect(document.body.textContent).not.toContain("This pool is");
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    // The swarm's pool gives the Size its default.
    expect(within(dialog).getByRole("combobox", { name: "Size" }).textContent).toContain("/26");
  });

  it("keeps Create held back by a finding while Advanced is closed", async () => {
    checkAnswer = overlapCheck;
    mount();
    const dialog = await screen.findByRole("dialog");
    const subnet = await openAdvanced(dialog);
    fireEvent.change(subnet, { target: { value: "10.0.2.0/26" } });
    await within(dialog).findByText(/Overlaps shop_default/);
    // Closing it keeps the finding, and Create stays held back.
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    expect(
      within(dialog).getByRole("button", { name: "Create network" }).getAttribute("aria-disabled"),
    ).toBe("true");
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
  it("still offers every node", async () => {
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
  });
});
