// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { NetworkRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UsedByList } from "@/components/used-by-list";
import { NetworkList } from "@/features/networks/network-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let items: NetworkRow[];
let onSearch: Mock<(q: string | undefined) => void>;
let swarmEnv: boolean;
let answerDelete: (req: Sent) => Response;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

const net = (name: string, extra: Partial<NetworkRow> = {}): NetworkRow => ({
  id: `id-${name}`,
  name,
  driver: "bridge",
  scope: "local",
  node: "node-1",
  nodeId: "node-1-id",
  subnets: [],
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

beforeEach(() => {
  sent = [];
  onSearch = vi.fn<(q: string | undefined) => void>();
  swarmEnv = false;
  me = makeMe({ envs: { [ENV]: "admin" } });
  items = [net("bridge", { builtIn: true }), net("app-net")];
  answerDelete = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const entry: Sent = { method: req.method, path: url.pathname, search: url.search };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === `/api/env/${ENV}/networks` && req.method === "GET") {
        return Response.json({
          items,
          total: items.length,
          truncated: false,
          nodes: ["node-1"],
          notReporting: [],
        });
      }
      if (entry.path.startsWith(`/api/env/${ENV}/networks/`) && req.method === "DELETE") {
        const res = answerDelete(entry);
        if (res.ok) items = items.filter((n) => `/api/env/${ENV}/networks/${n.id}` !== entry.path);
        return res;
      }
      if (entry.path === "/api/environments") {
        return Response.json({
          items: swarmEnv ? [{ id: ENV, name: "Swarm", envType: "swarm" }] : [],
        });
      }
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

const deletes = () => sent.filter((s) => s.method === "DELETE");

function mount(props: { q?: string } = {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <NetworkList envId={ENV} q={props.q} onNodeChange={() => {}} onSearchChange={onSearch} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const deleteButton = (name: string) =>
  screen.findByRole("button", { name: `Delete network ${name}` });

describe("NetworkList delete", () => {
  it("asks 'Delete {name}?', sends one DELETE with the node and toasts on success", async () => {
    mount();
    fireEvent.click(await deleteButton("app-net"));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete app-net?")).toBeTruthy();
    expect(
      within(dialog).getByText("Containers can no longer use it. This can't be undone."),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete network" }));
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]).toMatchObject({
      path: `/api/env/${ENV}/networks/id-app-net`,
      search: "?nodeId=node-1-id",
    });
    await waitFor(() => expect(document.body.textContent).toContain("Deleted network app-net."));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(screen.queryByText("app-net")).toBeNull());
  });

  it("cancels without sending anything", async () => {
    mount();
    fireEvent.click(await deleteButton("app-net"));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(deletes()).toHaveLength(0);
  });

  it("disables Delete on a built-in network with its reason and a click opens nothing", async () => {
    mount();
    const button = await deleteButton("bridge");
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("bridge is built in and can't be deleted.");
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(deletes()).toHaveLength(0);
  });

  it("shows an Operator and a Viewer Delete disabled with Needs Admin", async () => {
    for (const role of ["operator", "viewer"] as const) {
      cleanup();
      me = makeMe({ envs: { [ENV]: role } });
      mount();
      const button = await deleteButton("app-net");
      await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
      expect(document.body.textContent).toContain("Needs Admin");
      fireEvent.click(button);
      expect(screen.queryByRole("alertdialog")).toBeNull();
    }
    expect(deletes()).toHaveLength(0);
  });

  it("keeps the dialog open with the way forward when the server says it is still in use", async () => {
    answerDelete = () => problem(409, "network_in_use");
    mount();
    fireEvent.click(await deleteButton("app-net"));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete network" }));
    await within(dialog).findByText(
      "app-net is still in use. Disconnect or remove what uses it, then try again.",
    );
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("says a network that is already gone is gone, closes the dialog and refreshes the list", async () => {
    answerDelete = () => problem(404, "network_not_found");
    mount();
    fireEvent.click(await deleteButton("app-net"));
    const dialog = await screen.findByRole("alertdialog");
    const before = sent.filter((s) => s.method === "GET" && s.path.endsWith("/networks")).length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete network" }));
    await waitFor(() => expect(document.body.textContent).toContain("app-net is already gone."));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() =>
      expect(
        sent.filter((s) => s.method === "GET" && s.path.endsWith("/networks")).length,
      ).toBeGreaterThan(before),
    );
  });

  it("shows a refusal of the permission in the dialog", async () => {
    answerDelete = () => problem(403, "forbidden");
    mount();
    fireEvent.click(await deleteButton("app-net"));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete network" }));
    await within(dialog).findByText(
      "You no longer have permission to do that. Your access may have changed.",
    );
  });
});

// What is attached, who owns it, the search and the reasons Delete is disabled (plan
// 04-13 task 2, D-40, D-41).

describe("NetworkList columns", () => {
  it("links the name and an Open item to the network's page with the node it is on", async () => {
    items = [net("app-net")];
    mount();
    const name = await screen.findByRole("link", { name: "app-net" });
    const href = new URL(name.getAttribute("href") ?? "", "http://x");
    expect(href.pathname).toBe(`/env/${ENV}/networks/id-app-net`);
    expect(href.searchParams.get("node")).toBe("node-1-id");
    const open = screen.getByRole("link", { name: "Open network app-net" });
    expect(open.getAttribute("href")).toBe(name.getAttribute("href"));
  });

  it("shows Scope as Swarm or Local, the first subnet with +n, Attached, Owner and a dash for none", async () => {
    items = [
      net("app-net", {
        subnets: ["10.20.0.0/24", "10.21.0.0/24", "10.22.0.0/24"],
        attachedContainers: 2,
        owner: "shop",
        ownerKind: "stack",
      }),
      net("lonely"),
      net("ovl", { scope: "swarm", node: "Cluster", driver: "overlay" }),
    ];
    mount();
    const app = (await screen.findByText("app-net")).closest("tr") as HTMLElement;
    expect(within(app).getByText("Local")).toBeTruthy();
    expect(within(app).getByText("10.20.0.0/24")).toBeTruthy();
    expect(within(app).getByText("+2")).toBeTruthy();
    expect(within(app).getByText("2 containers")).toBeTruthy();
    expect(within(app).getByText("shop")).toBeTruthy();
    const lonely = screen.getByText("lonely").closest("tr") as HTMLElement;
    expect(within(lonely).getByText("None")).toBeTruthy();
    expect(within(lonely).getAllByText("—").length).toBeGreaterThanOrEqual(2);
    const ovl = screen.getByText("ovl").closest("tr") as HTMLElement;
    expect(within(ovl).getByText("Swarm")).toBeTruthy();
  });

  it("adds the Addresses used column for overlays", async () => {
    swarmEnv = true;
    items = [
      net("ovl", {
        scope: "swarm",
        node: "Cluster",
        driver: "overlay",
        addressesUsed: { used: 230, capacity: 254, percent: 90, state: "almost_full" },
      }),
      net("blind", { scope: "swarm", node: "Cluster", driver: "overlay" }),
      net("plain"),
    ];
    mount();
    const row = (await screen.findByText("ovl")).closest("tr") as HTMLElement;
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    // Between Attached and Owner.
    const at = headers.indexOf("Addresses used");
    expect(at).toBeGreaterThan(-1);
    expect(headers[at - 1]).toBe("Attached");
    expect(headers[at + 1]).toBe("Owner");
    expect(within(row).getByRole("meter", { name: "ovl" })).toBeTruthy();
    expect(within(row).getByText("230 / 254")).toBeTruthy();
    expect(within(row).getByText("Almost full")).toBeTruthy();
    const blind = screen.getByText("blind").closest("tr") as HTMLElement;
    expect(within(blind).queryByRole("meter")).toBeNull();
    const plain = screen.getByText("plain").closest("tr") as HTMLElement;
    expect(within(plain).queryByRole("meter")).toBeNull();
  });

  it("on a swarm counts services too and says Cluster for a swarm-wide network", async () => {
    swarmEnv = true;
    items = [
      net("ovl", {
        scope: "swarm",
        node: "Cluster",
        driver: "overlay",
        attachedContainers: 1,
        attachedServices: 3,
      }),
    ];
    mount();
    const row = (await screen.findByText("ovl")).closest("tr") as HTMLElement;
    await waitFor(() => expect(within(row).getByText("1 container, 3 services")).toBeTruthy());
    expect(within(row).getByText("Cluster")).toBeTruthy();
    expect(within(row).getByText("Swarm")).toBeTruthy();
  });

  it("keeps the rows in the order the server sent them", async () => {
    items = [net("zeta"), net("alpha"), net("mid")];
    mount();
    await screen.findByText("zeta");
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.textContent);
    expect(names).toEqual(["zeta", "alpha", "mid"]);
  });
});

describe("NetworkList search", () => {
  it("has the labelled box, sends the search to the server and tells the owner after the pause", async () => {
    mount({ q: "shop" });
    const box = await screen.findByLabelText("Search networks");
    expect(box.getAttribute("placeholder")).toBe("Search by name, driver, subnet or stack");
    expect((box as HTMLInputElement).value).toBe("shop");
    await waitFor(() =>
      expect(sent.some((s) => s.method === "GET" && s.search === "?q=shop")).toBe(true),
    );
    fireEvent.change(box, { target: { value: "blog" } });
    expect(onSearch).not.toHaveBeenCalled();
    await waitFor(() => expect(onSearch).toHaveBeenCalledWith("blog"));
  });

  it("says nothing matches with Clear search, and plain 'No networks' when there is nothing and no search", async () => {
    items = [];
    mount({ q: "zzz" });
    await screen.findByText("No networks match “zzz”.");
    // The box has its own clear button; the empty state's comes after it.
    fireEvent.click(screen.getAllByRole("button", { name: "Clear search" }).at(-1) as HTMLElement);
    expect(onSearch).toHaveBeenCalledWith(undefined);

    cleanup();
    mount();
    await screen.findByText("No networks");
    expect(screen.queryByRole("button", { name: "Clear search" })).toBeNull();
  });
});

describe("NetworkList delete reasons", () => {
  it("disables Delete on a network in use and says by what, and a click opens nothing", async () => {
    items = [
      net("app-net", { attachedContainers: 2 }),
      net("both", { attachedContainers: 1, attachedServices: 3 }),
    ];
    mount();
    const button = await deleteButton("app-net");
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain(
      "In use by 2 containers. Open it to see what uses it.",
    );
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const both = await deleteButton("both");
    expect(both.getAttribute("aria-disabled")).toBe("true");
    expect(document.body.textContent).toContain(
      "In use by 1 container and 3 services. Open it to see what uses it.",
    );
    expect(deletes()).toHaveLength(0);
  });

  it("puts the built-in reason before the in-use one", async () => {
    items = [net("bridge", { builtIn: true, attachedContainers: 4 })];
    mount();
    const button = await deleteButton("bridge");
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("bridge is built in and can't be deleted.");
    expect(document.body.textContent).not.toContain("In use by");
  });

  it("leaves Delete available on a network with nothing attached", async () => {
    items = [net("lonely")];
    mount();
    const button = await deleteButton("lonely");
    expect(button.getAttribute("aria-disabled")).toBeNull();
  });
});

describe("UsedByList", () => {
  it("lists each container with its node and each service", () => {
    render(
      <TooltipProvider>
        <UsedByList
          containers={[
            { name: "web", node: "node-1" },
            { name: "db", node: "node-2" },
          ]}
          services={[{ name: "api" }]}
        />
      </TooltipProvider>,
    );
    const items = screen.getAllByRole("listitem").map((li) => li.textContent);
    expect(items).toEqual(["web on node-1", "db on node-2", "Service api"]);
  });

  it("renders nothing for nothing", () => {
    const { container } = render(<UsedByList containers={[]} services={[]} />);
    expect(container.querySelector("ul")).toBeNull();
  });
});
