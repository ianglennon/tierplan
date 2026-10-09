// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
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
      if (entry.path === `/api/env/${ENV}/networks`) return Response.json(list);
      if (entry.path === "/api/environments") return Response.json({ items: [] });
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
    render(wrap(<NetworkList envId={ENV} onNodeChange={() => {}} />));
    const button = await screen.findByRole("button", { name: "Create network" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(button);
    expect(await screen.findByRole("dialog", { name: "Create network" })).toBeTruthy();
  });

  it("shows an Operator the button disabled with Needs Admin, and a click opens nothing", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    render(wrap(<NetworkList envId={ENV} onNodeChange={() => {}} />));
    const button = await screen.findByRole("button", { name: "Create network" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Needs Admin");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows a Viewer the same disabled button", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    render(wrap(<NetworkList envId={ENV} onNodeChange={() => {}} />));
    const button = await screen.findByRole("button", { name: "Create network" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
  });
});
