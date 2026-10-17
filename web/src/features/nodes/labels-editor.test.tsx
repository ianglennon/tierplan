// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { NodeLabelImpact } from "@/api/nodes";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NODE_ENV, NODE_ID, nodeDetail } from "@/features/nodes/fixtures";
import { LABEL_CHECK_DEBOUNCE_MS, NodeLabelsEditor } from "@/features/nodes/labels-editor";
import "@/i18n";
import { makeMe } from "@/test-me";

// The Labels tab (plan 06-16, RES-05, D-21, E11, E15): a key and value table whose edits are
// checked 400 ms after the last one, an Effect on services panel, and a save that asks first only
// when a constraint would no longer match.

const ROW_DB = {
  serviceId: "svc-db",
  service: "db",
  constraint: "node.labels.disk==ssd",
  before: true,
  after: false,
};

const impact = (extra: Partial<NodeLabelImpact> = {}): NodeLabelImpact => ({
  version: 12,
  rows: [],
  noLongerMatches: 0,
  tasksMayMove: 0,
  added: 0,
  changed: 0,
  removed: 0,
  ...extra,
});

interface Sent {
  method: string;
  body: Record<string, unknown>;
}

let role: "admin" | "operator" | "viewer";
let impactCalls: Sent[];
let puts: Sent[];
let answerImpact: (labels: Record<string, string>) => Response;
let answerPut: () => Response;
let impactGate: Promise<void> | null;

beforeEach(() => {
  role = "admin";
  impactCalls = [];
  puts = [];
  impactGate = null;
  answerImpact = () => Response.json(impact());
  answerPut = () => new Response(null, { status: 204 });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { [NODE_ENV]: role } }));
      }
      if (url.pathname === `/api/env/${NODE_ENV}/nodes/${NODE_ID}/labels/impact`) {
        const body = (await req.json()) as { labels: Record<string, string> };
        impactCalls.push({ method: req.method, body });
        if (impactGate) await impactGate;
        return answerImpact(body.labels);
      }
      if (req.method === "PUT" && url.pathname === `/api/env/${NODE_ENV}/nodes/${NODE_ID}/labels`) {
        puts.push({ method: req.method, body: (await req.json()) as Record<string, unknown> });
        return answerPut();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  cleanup();
  vi.unstubAllGlobals();
});

// The impact check is debounced by a timer of the component's own. A test about that timing
// runs it on a fully fake clock: real time passing between two statements must not be able to
// fire the debounce, or "not yet, one millisecond short" would be true only on a quiet machine.
// The test moves the clock by exactly the debounce when it means to, and under it waits with
// vi.waitFor, because Testing Library's waitFor and findBy schedule their polling on the faked
// timers and so never poll.
const debounceClock = () => vi.useFakeTimers();

function problem(status: number, type: string, detail?: string) {
  return Response.json({ status, type, title: "x", ...(detail ? { detail } : {}) }, { status });
}

async function mount(labels: Record<string, string> = { disk: "ssd", zone: "a" }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const node = nodeDetail({ labels });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <NodeLabelsEditor envId={NODE_ENV} node={node} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  // Who the person is arrives after the first paint, and the editor with it.
  if (role === "admin") await screen.findByRole("button", { name: "Add label" });
  return node;
}

const save = () => screen.getByRole("button", { name: "Save labels" });
const isOff = (b: HTMLElement) =>
  b.hasAttribute("disabled") || b.getAttribute("aria-disabled") === "true";
const names = () =>
  (screen.getAllByLabelText("Label name") as HTMLInputElement[]).map((i) => i.value);
const values = () =>
  (screen.getAllByLabelText("Label value") as HTMLInputElement[]).map((i) => i.value);

// Types a new value into the second row (zone).
function editZone(value: string) {
  fireEvent.change(screen.getAllByLabelText("Label value")[1] as HTMLElement, {
    target: { value },
  });
}

describe("NodeLabelsEditor", () => {
  it("shows the node's labels by name, with the helper, and Save labels off until something changes", async () => {
    await mount();
    expect(screen.getByRole("heading", { name: "Node labels" })).toBeTruthy();
    expect(
      screen.getByText(
        "Services use labels in placement constraints, such as node.labels.disk==ssd.",
      ),
    ).toBeTruthy();
    expect(names()).toEqual(["disk", "zone"]);
    expect(values()).toEqual(["ssd", "a"]);
    expect(isOff(save())).toBe(true);
    expect(screen.getByRole("button", { name: "Discard changes" })).toBeTruthy();
    expect(
      screen.getByText("No service uses these labels in a placement constraint."),
    ).toBeTruthy();
    expect(impactCalls).toHaveLength(0);
  });

  it("says No labels. for a node with none, and still offers Add label", async () => {
    await mount({});
    expect(screen.getByText("No labels.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    expect(screen.getAllByLabelText("Label name")).toHaveLength(1);
  });

  it("checks 400 ms after the last edit, once, with every label, and holds Save labels until it answers", async () => {
    let open: () => void = () => {};
    impactGate = new Promise<void>((resolve) => {
      open = resolve;
    });
    await mount();
    debounceClock();
    editZone("b");
    editZone("bb");
    editZone("bbb");
    // The check is pending from the first edit, and has not been asked yet.
    expect(isOff(save())).toBe(true);
    expect(screen.getByText("Checking services…")).toBeTruthy();
    await vi.advanceTimersByTimeAsync(LABEL_CHECK_DEBOUNCE_MS - 1);
    expect(impactCalls).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    await vi.waitFor(() => expect(impactCalls).toHaveLength(1));
    expect(impactCalls[0]?.body).toEqual({ labels: { disk: "ssd", zone: "bbb" } });
    // Asked but not answered: still held.
    expect(isOff(save())).toBe(true);
    expect(screen.getByText("Checking services…")).toBeTruthy();
    open();
    await vi.waitFor(() => expect(isOff(save())).toBe(false));
    expect(screen.queryByText("Checking services…")).toBeNull();
    expect(impactCalls).toHaveLength(1);
  });

  it("lists the services whose constraints the change affects", async () => {
    answerImpact = () =>
      Response.json(impact({ rows: [ROW_DB], noLongerMatches: 1, tasksMayMove: 3, removed: 1 }));
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove label disk" }));
    const table = await screen.findByRole("table", { name: "Effect on services" });
    expect(within(table).getByText("No longer matches")).toBeTruthy();
    expect(screen.getByText("Swarm may move 3 tasks off this node.")).toBeTruthy();
    expect(impactCalls[0]?.body).toEqual({ labels: { zone: "a" } });
  });

  it("refuses a reserved name on its row, holds Save labels and asks nothing of the server", async () => {
    await mount();
    debounceClock();
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    const fields = screen.getAllByLabelText("Label name");
    fireEvent.change(fields[2] as HTMLElement, { target: { value: "COM.Docker.swarm.x" } });
    expect(screen.getByText("Labels starting with com.docker. are reserved.")).toBeTruthy();
    expect((fields[2] as HTMLElement).getAttribute("aria-invalid")).toBe("true");
    // Past the debounce the check would have been asked; it is not.
    await vi.advanceTimersByTimeAsync(LABEL_CHECK_DEBOUNCE_MS * 2);
    expect(isOff(save())).toBe(true);
    expect(impactCalls).toHaveLength(0);
    // Fixing it releases the hold.
    fireEvent.change(fields[2] as HTMLElement, { target: { value: "com.dockerish" } });
    expect(screen.queryByText(/are reserved/)).toBeNull();
    await vi.advanceTimersByTimeAsync(LABEL_CHECK_DEBOUNCE_MS);
    await vi.waitFor(() => expect(isOff(save())).toBe(false));
  });

  it("refuses a name used twice and more than 64 labels", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.change(screen.getAllByLabelText("Label name")[2] as HTMLElement, {
      target: { value: "disk" },
    });
    expect(screen.getByText("This name is used twice.")).toBeTruthy();
    expect(isOff(save())).toBe(true);
  });

  it("saves at once, with the version the check carried, when no constraint stops matching", async () => {
    answerImpact = () => Response.json(impact({ version: 13, rows: [], changed: 1 }));
    await mount();
    editZone("b");
    await waitFor(() => expect(isOff(save())).toBe(false));
    fireEvent.click(save());
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]?.body).toEqual({ labels: { disk: "ssd", zone: "b" }, version: 13 });
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText("Saved labels on mgr-1.")).toBeTruthy();
    // What was saved is now what the node has: nothing left to save.
    await waitFor(() => expect(isOff(save())).toBe(true));
  });

  it("asks Apply label changes? first when a constraint would no longer match, and sends nothing on Cancel", async () => {
    answerImpact = () =>
      Response.json(
        impact({ version: 13, rows: [ROW_DB], noLongerMatches: 1, tasksMayMove: 1, removed: 1 }),
      );
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove label disk" }));
    await waitFor(() => expect(isOff(save())).toBe(false));
    fireEvent.click(save());
    const dialog = await screen.findByRole("alertdialog", { name: "Apply label changes?" });
    expect(
      within(dialog).getByText(
        "1 service has a constraint this node would no longer meet. Swarm moves its tasks to other nodes, or leaves them pending if no node matches.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(puts).toHaveLength(0);
    expect(names()).toEqual(["zone"]);

    fireEvent.click(save());
    const again = await screen.findByRole("alertdialog", { name: "Apply label changes?" });
    fireEvent.click(within(again).getByRole("button", { name: "Apply labels" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]?.body).toEqual({ labels: { zone: "a" }, version: 13 });
    expect(await screen.findByText("Saved labels on mgr-1.")).toBeTruthy();
  });

  it("words a stale node, keeps what was typed and checks again for the node as it is now", async () => {
    await mount();
    editZone("b");
    await waitFor(() => expect(isOff(save())).toBe(false));
    answerPut = () => problem(409, "node_changed");
    fireEvent.click(save());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "This node changed after you started editing. Nothing was saved.",
    );
    expect(values()).toEqual(["ssd", "b"]);
    expect(puts).toHaveLength(1);
    // The check is asked again, so that the next save carries the version now.
    await waitFor(() => expect(impactCalls).toHaveLength(2));
  });

  it("shows Docker's own words for a refusal the swarm gave", async () => {
    await mount();
    editZone("b");
    await waitFor(() => expect(isOff(save())).toBe(false));
    answerPut = () => problem(502, "node_update_failed", "no leader elected");
    fireEvent.click(save());
    expect((await screen.findByRole("alert")).textContent).toContain(
      "Docker refused: no leader elected",
    );
    expect(puts).toHaveLength(1);
  });

  it("words a check that failed and runs it again on Retry", async () => {
    answerImpact = () => problem(503, "no_manager_connected");
    await mount();
    editZone("b");
    const retry = await screen.findByRole("button", { name: "Retry checking services" });
    expect(isOff(save())).toBe(true);
    answerImpact = () => Response.json(impact());
    fireEvent.click(retry);
    await waitFor(() => expect(isOff(save())).toBe(false));
    expect(impactCalls).toHaveLength(2);
  });

  it("restores the loaded labels on Discard changes", async () => {
    await mount();
    editZone("b");
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect(names()).toEqual(["disk", "zone"]);
    expect(values()).toEqual(["ssd", "a"]);
    expect(isOff(save())).toBe(true);
    expect(
      screen.getByText("No service uses these labels in a placement constraint."),
    ).toBeTruthy();
  });

  it.each(["operator", "viewer"] as const)(
    "shows %s the table read-only and Save labels off for Needs Admin",
    async (r) => {
      role = r;
      await mount();
      await waitFor(() => expect(isOff(save())).toBe(true));
      await waitFor(() => expect(save().getAttribute("aria-disabled")).toBe("true"));
      expect(screen.queryAllByLabelText("Label name")).toHaveLength(0);
      expect(screen.queryByRole("button", { name: "Add label" })).toBeNull();
      expect(screen.getByRole("table", { name: "Node labels" })).toBeTruthy();
      fireEvent.click(save());
      expect(await screen.findAllByText("Needs Admin")).not.toHaveLength(0);
      expect(puts).toHaveLength(0);
      expect(impactCalls).toHaveLength(0);
    },
  );
});
