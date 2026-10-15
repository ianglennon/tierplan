// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodeRemoveDialog } from "@/features/nodes/node-remove-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

// Remove a node that is down (plan 06-17, RES-05, D-22, UI-SPEC "Nodes"): an alert dialog with the
// node's name to type, a destructive confirm, and the refusals in words inside the dialog.

const ENV = "e1";
const NODE = { id: "n3", hostname: "wkr-1" };

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let answer: () => Response;
let gate: Promise<void> | null;

beforeEach(() => {
  sent = [];
  answer = () => new Response(null, { status: 204 });
  gate = null;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      }
      if (req.method === "DELETE" && url.pathname.startsWith(`/api/env/${ENV}/nodes/`)) {
        sent.push({ method: req.method, path: url.pathname, search: url.search });
        if (gate) await gate;
        return answer();
      }
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm one", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function problem(status: number, type: string, detail?: string) {
  return Response.json({ status, type, title: "x", ...(detail ? { detail } : {}) }, { status });
}

function mount(node: { id: string; hostname: string } | null = NODE) {
  const onClose = vi.fn();
  const onRemoved = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <NodeRemoveDialog envId={ENV} node={node} onClose={onClose} onRemoved={onRemoved} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onClose, onRemoved };
}

async function dialogOpen() {
  return screen.findByRole("alertdialog", { name: "Remove wkr-1?" });
}

function typeName(dialog: HTMLElement, value: string) {
  fireEvent.change(within(dialog).getByRole("textbox"), { target: { value } });
}

describe("NodeRemoveDialog", () => {
  it("is an alert dialog that says what removing a down node does, with focus on Cancel", async () => {
    mount();
    const dialog = await dialogOpen();
    expect(
      within(dialog).getByText(
        "wkr-1 is down, so Swarm forgets it. If it comes back, it has to join the swarm again. Bayplan also marks its agent as gone.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByText("Type wkr-1 to confirm")).toBeTruthy();
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
  });

  it("is not drawn while there is no node", () => {
    mount(null);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("keeps the destructive confirm off until the name is typed, and sends nothing", async () => {
    mount();
    const dialog = await dialogOpen();
    const confirm = within(dialog).getByRole("button", { name: "Remove node" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    expect(confirm.className).toContain("bg-destructive-fill");
    fireEvent.click(confirm);
    typeName(dialog, "wkr-");
    fireEvent.click(confirm);
    expect(sent).toEqual([]);
    typeName(dialog, "wkr-1");
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
  });

  it("removes the node with its name, says so, closes and reports it", async () => {
    const { onClose, onRemoved } = mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(await screen.findByText("Removed wkr-1.")).toBeTruthy();
    expect(sent).toEqual([
      { method: "DELETE", path: "/api/env/e1/nodes/n3", search: "?confirm=wkr-1" },
    ]);
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onRemoved).toHaveBeenCalledWith({ nodeId: "n3" });
  });

  it("confirms from the keyboard once the name matches", async () => {
    mount();
    const dialog = await dialogOpen();
    const input = within(dialog).getByRole("textbox");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(sent).toEqual([]);
    typeName(dialog, "wkr-1");
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(sent).toHaveLength(1));
  });

  it("sends once while the request is out and cannot be dismissed", async () => {
    let open: () => void = () => {};
    gate = new Promise<void>((resolve) => {
      open = resolve;
    });
    const { onClose } = mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    const confirm = within(dialog).getByRole("button", { name: "Remove node" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() => expect(within(dialog).getByText("Removing…")).toBeTruthy());
    expect(within(dialog).getByRole("button", { name: "Cancel" }).hasAttribute("disabled")).toBe(
      true,
    );
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
    expect(sent).toHaveLength(1);
    open();
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(sent).toHaveLength(1);
  });

  it("cancels without sending anything", async () => {
    const { onClose } = mount();
    const dialog = await dialogOpen();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(sent).toEqual([]);
  });

  it("stays open with the way forward when the node is not down", async () => {
    answer = () => problem(409, "node_not_down");
    const { onClose, onRemoved } = mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(
      await within(dialog).findByText("wkr-1 isn't down. Only a node that is down can be removed."),
    ).toBeTruthy();
    expect(onClose).not.toHaveBeenCalled();
    expect(onRemoved).not.toHaveBeenCalled();
  });

  it("tells a manager to be demoted first", async () => {
    answer = () => problem(409, "node_is_manager");
    mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(await within(dialog).findByText("Demote wkr-1 before removing it.")).toBeTruthy();
  });

  it("passes on Docker's own words when it refused", async () => {
    answer = () => problem(502, "node_remove_failed", "rpc error: no leader elected");
    mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(
      await within(dialog).findByText("Docker refused: rpc error: no leader elected"),
    ).toBeTruthy();
  });

  it("says a node that is already gone is gone, and closes", async () => {
    answer = () => problem(404, "node_not_found");
    const { onClose, onRemoved } = mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(await screen.findByText("wkr-1 is already gone.")).toBeTruthy();
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onRemoved).toHaveBeenCalledWith({ nodeId: "n3" });
  });

  it("says a request that got no answer may have gone through", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const url = new URL(req.url);
        if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
        if (req.method === "DELETE") throw new TypeError("network down");
        return Response.json({}, { status: 404 });
      }),
    );
    mount();
    const dialog = await dialogOpen();
    typeName(dialog, "wkr-1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(await within(dialog).findByRole("alert")).toBeTruthy();
    expect(within(dialog).getByRole("alert").textContent).toBe(
      "The connection dropped before Bayplan answered. The list shows whether it happened.",
    );
  });
});
