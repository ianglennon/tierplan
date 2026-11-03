// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { JanitorKept } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CleanupPreviewSheet } from "@/features/janitor/cleanup-preview-sheet";
import { KeptItemsList } from "@/features/janitor/kept-items-list";
import "@/i18n";
import { preview, previewItem } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

interface Sent {
  method: string;
  path: string;
  search: string;
  body?: unknown;
}

const kept = (over: Partial<JanitorKept> = {}): JanitorKept => ({
  keptAt: "2026-11-02T10:00:00Z",
  keptBy: "user",
  keptByName: "Ada",
  kind: "container",
  name: "web.1.abc",
  node: "node-1",
  nodeId: "n1",
  present: true,
  ref: "c1",
  ...over,
});

let me: ReturnType<typeof makeMe>;
let keeps: JanitorKept[];
let sent: Sent[];
// What the stubbed server answers to a DELETE instead of removing the keep.
let deleteAnswer: (() => Response) | undefined;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  keeps = [];
  sent = [];
  deleteAnswer = undefined;
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const body = req.method === "PUT" ? await req.json() : undefined;
      sent.push({ method: req.method, path: url.pathname, search: url.search, body });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/janitor/preview`) {
        return Response.json(preview({ items: [previewItem({ name: "web.1.abc", ref: "c1" })] }));
      }
      if (url.pathname === `/api/env/${ENV}/janitor/keeps`) {
        if (req.method === "GET") return Response.json({ items: keeps });
        if (req.method === "PUT") {
          const target = (body as { targets: { kind: JanitorKept["kind"]; ref: string }[] })
            .targets[0];
          if (target) keeps = [...keeps, kept({ kind: target.kind, ref: target.ref })];
          return Response.json({ kept: 1 });
        }
        if (req.method === "DELETE") {
          if (deleteAnswer) return deleteAnswer();
          const ref = url.searchParams.get("ref");
          keeps = keeps.filter((k) => k.ref !== ref);
          return new Response(null, { status: 204 });
        }
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(children: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>{children}</TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const open = async (count: number) =>
  fireEvent.click(await screen.findByRole("button", { name: `Kept items (${count})` }));

describe("Kept items", () => {
  it("is left out when nothing is kept", async () => {
    mount(<KeptItemsList envId={ENV} envName="Prod" />);
    await waitFor(() =>
      expect(sent.some((s) => s.path === `/api/env/${ENV}/janitor/keeps`)).toBe(true),
    );
    expect(screen.queryByRole("button", { name: /^Kept items/ })).toBeNull();
  });

  it("is closed to start with, and lists each keep by type, name, node, who and since", async () => {
    keeps = [
      kept(),
      kept({
        keptBy: "label",
        keptByName: "",
        keptAt: undefined,
        kind: "network",
        name: "shop_net",
        node: "Cluster",
        nodeId: "",
        ref: "shop_net",
      }),
    ];
    mount(<KeptItemsList envId={ENV} envName="Prod" />);
    const trigger = await screen.findByRole("button", { name: "Kept items (2)" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.getElementById("kept")).toBeTruthy();

    fireEvent.click(trigger);
    expect(
      await screen.findByText(
        "The janitor never removes these. Networks you kept also appear on Address space.",
      ),
    ).toBeTruthy();
    const table = screen.getByRole("table", { name: "Kept items" });
    for (const header of ["Type", "Name", "Node", "Kept by", "Kept since"]) {
      expect(within(table).getByRole("columnheader", { name: header })).toBeTruthy();
    }
    const person = within(table).getByText("web.1.abc").closest("tr") as HTMLElement;
    expect(within(person).getByText("Container")).toBeTruthy();
    expect(within(person).getByText("node-1")).toBeTruthy();
    expect(within(person).getByText("Ada in TierPlan")).toBeTruthy();
    const label = within(table).getByText("shop_net").closest("tr") as HTMLElement;
    expect(within(label).getByText("Network")).toBeTruthy();
    expect(within(label).getByText("A label on the object")).toBeTruthy();
  });

  it("takes a keep back with Stop keeping and says so", async () => {
    keeps = [kept()];
    mount(<KeptItemsList envId={ENV} envName="Prod" />);
    await open(1);
    fireEvent.click(await screen.findByRole("button", { name: "Stop keeping web.1.abc" }));

    await waitFor(() =>
      expect(toasts.success).toHaveBeenCalledWith("No longer keeping web.1.abc."),
    );
    const del = sent.find((s) => s.method === "DELETE");
    expect(del?.path).toBe(`/api/env/${ENV}/janitor/keeps`);
    const query = new URLSearchParams(del?.search);
    expect(Object.fromEntries(query)).toEqual({ kind: "container", nodeId: "n1", ref: "c1" });
    // The list is read again and, with nothing left, goes away.
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Kept items/ })).toBeNull());
  });

  it("does not offer Stop keeping on a keep the label makes", async () => {
    keeps = [kept({ keptBy: "label", keptByName: "" })];
    mount(<KeptItemsList envId={ENV} envName="Prod" />);
    await open(1);
    const stop = await screen.findByRole("button", { name: "Stop keeping web.1.abc" });
    expect(stop.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(stop.getAttribute("aria-describedby") ?? "")?.textContent).toBe(
      "Kept by the tierplan.keep label on the object. Remove the label from your compose file to stop keeping it.",
    );
    fireEvent.click(stop);
    expect(sent.some((s) => s.method === "DELETE")).toBe(false);
  });
});

// The page's own arrangement: the list, and the sheet that closes when it is cancelled.
function WithSheet() {
  const [shown, setShown] = useState(true);
  return (
    <>
      <KeptItemsList envId={ENV} envName="Prod" />
      {shown ? (
        <CleanupPreviewSheet
          envId={ENV}
          envName="Prod"
          swarm
          nodeCount={2}
          onOpenChange={(next) => setShown(next)}
        />
      ) : null}
    </>
  );
}

describe("a keep made in the preview", () => {
  it("shows in Kept items, and Stop keeping takes it back", async () => {
    mount(<WithSheet />);
    expect(screen.queryByRole("button", { name: /^Kept items/ })).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Keep web.1.abc" }));
    await waitFor(() => expect(sent.some((s) => s.method === "PUT")).toBe(true));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    await open(1);
    expect(sent.find((s) => s.method === "PUT")?.body).toEqual({
      targets: [{ kind: "container", nodeId: "n1", ref: "c1" }],
    });

    fireEvent.click(await screen.findByRole("button", { name: "Stop keeping web.1.abc" }));
    await waitFor(() => expect(sent.some((s) => s.method === "DELETE")).toBe(true));
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Kept items/ })).toBeNull());
  });
});
