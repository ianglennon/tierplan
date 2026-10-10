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
import { NetworkList } from "@/features/networks/network-list";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let items: NetworkRow[];
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
  ...extra,
});

beforeEach(() => {
  sent = [];
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

const deletes = () => sent.filter((s) => s.method === "DELETE");

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <NetworkList envId={ENV} onNodeChange={() => {}} />
        <Toaster />
      </TooltipProvider>
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
