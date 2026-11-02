// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { KeptRow } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { KeptList } from "@/features/address-space/kept-list";
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
let answer: () => Response;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

const keptRow = (name: string, extra: Partial<KeptRow> = {}): KeptRow => ({
  keptAt: "2026-10-02T10:00:00Z",
  keptBy: "user",
  keptByName: "ian",
  name,
  node: "Cluster",
  nodeId: "",
  present: true,
  subnets: ["10.0.7.0/24"],
  ...extra,
});

function labelOnly(): KeptRow {
  const { keptAt: _keptAt, ...row } = keptRow("labelled", { keptBy: "label", keptByName: "" });
  return row;
}

beforeEach(() => {
  sent = [];
  answer = () => new Response(null, { status: 204 });
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      sent.push({ method: req.method, path: url.pathname, search: url.search });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname.startsWith(`/api/env/${ENV}/address-space/keeps/`)) return answer();
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

function mount(kept: KeptRow[] | undefined, swarm = true) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <KeptList envId={ENV} envName="Dev swarm" kept={kept} swarm={swarm} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const trigger = (n: number) => screen.getByRole("button", { name: `Kept networks (${n})` });

async function ready(name: string) {
  await waitFor(() =>
    expect(screen.getByRole("button", { name }).getAttribute("aria-disabled")).not.toBe("true"),
  );
}

describe("KeptList", () => {
  it("is left out when nothing is kept", () => {
    mount([]);
    expect(screen.queryByText(/Kept networks/)).toBeNull();
    cleanup();
    mount(undefined);
    expect(screen.queryByText(/Kept networks/)).toBeNull();
  });

  it("is closed until opened and says who kept each network", () => {
    mount([
      keptRow("mine"),
      // A keep by label alone has no time it was kept.
      labelOnly(),
    ]);
    expect(trigger(2).getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("table")).toBeNull();
    fireEvent.click(trigger(2));
    const table = screen.getByRole("table", { name: "Kept networks" });
    expect(
      screen.getByText(
        "TierPlan never lists these as orphaned or unused, and the janitor leaves them alone.",
      ),
    ).toBeTruthy();
    expect(within(table).getByText("ian in TierPlan")).toBeTruthy();
    expect(within(table).getByText("A label on the network")).toBeTruthy();
  });

  it("kept list stops keeping and refuses label-kept rows", async () => {
    mount([
      keptRow("mine"),
      keptRow("local", { nodeId: "w1", node: "worker-1" }),
      keptRow("labelled", { keptBy: "label", keptByName: "" }),
    ]);
    fireEvent.click(trigger(3));
    await ready("Stop keeping mine");
    // A label-kept row is aria-disabled and says why.
    const labelled = screen.getByRole("button", { name: "Stop keeping labelled" });
    expect(labelled.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen.getAllByText(
        "Kept by the tierplan.keep label on the network. Remove the label to stop keeping it.",
      ).length,
    ).toBeGreaterThan(0);
    fireEvent.click(labelled);
    expect(sent.some((s) => s.method === "DELETE")).toBe(false);

    // Stopping a keep sends the name, and the node of a local network; focus moves to the next row.
    fireEvent.click(screen.getByRole("button", { name: "Stop keeping mine" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Stop keeping local" }));
    expect(await screen.findByText("No longer keeping mine.")).toBeTruthy();
    const del = sent.find((s) => s.method === "DELETE");
    expect(del?.path).toBe(`/api/env/${ENV}/address-space/keeps/mine`);
    expect(del?.search).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Stop keeping local" }));
    await waitFor(() => expect(sent.filter((s) => s.method === "DELETE")).toHaveLength(2));
    expect(sent.filter((s) => s.method === "DELETE")[1]?.search).toBe("?nodeId=w1");
  });

  it("words a keep that was already gone and a label refusal", async () => {
    answer = () =>
      Response.json({ status: 404, type: "keep_not_found", title: "x" }, { status: 404 });
    mount([keptRow("mine")]);
    fireEvent.click(trigger(1));
    await ready("Stop keeping mine");
    fireEvent.click(screen.getByRole("button", { name: "Stop keeping mine" }));
    expect(await screen.findByText("mine wasn't being kept.")).toBeTruthy();
    toast.dismiss();
    answer = () =>
      Response.json({ status: 409, type: "kept_by_label", title: "x" }, { status: 409 });
    fireEvent.click(screen.getByRole("button", { name: "Stop keeping mine" }));
    expect(
      await screen.findByText(
        "mine is kept by a label on the network. Remove the label to stop keeping it.",
      ),
    ).toBeTruthy();
  });

  it("disables Stop keeping below Env Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount([keptRow("mine")]);
    fireEvent.click(trigger(1));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Stop keeping mine" }).getAttribute("aria-disabled"),
      ).toBe("true"),
    );
    expect(screen.getAllByText("Needs Admin on Dev swarm").length).toBeGreaterThan(0);
  });
});
