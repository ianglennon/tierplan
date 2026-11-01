// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { OrphanRow, OrphansWithheld } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { groupOrphans, OrphansCard } from "@/features/address-space/orphans-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  search: string;
  body?: unknown;
}

let sent: Sent[];
// What the stubbed server answers to a delete or a keep.
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

const orphan = (name: string, extra: Partial<OrphanRow> = {}): OrphanRow => ({
  class: "orphaned",
  formerOwner: "shop",
  formerOwnerKind: "stack",
  idleSince: "2026-10-01T10:00:00Z",
  idleSinceSource: "seen",
  name,
  networkId: `id-${name}`,
  node: "Cluster",
  nodeId: "mgr-1",
  scope: "swarm",
  subnets: ["10.0.9.0/24"],
  ...extra,
});

const deleted = () => Response.json({ name: "x", outcome: "deleted", reason: "" });
const kept = (reason: string) => () => Response.json({ name: "x", outcome: "kept", reason });
const problem = (status: number, type: string) => () =>
  Response.json({ status, type, title: type }, { status });

beforeEach(() => {
  sent = [];
  answer = deleted;
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const entry: Sent = {
        method: req.method,
        path: url.pathname,
        search: url.search,
        body: req.method === "PUT" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Dev swarm", type: "swarm" }] });
      }
      if (entry.path.startsWith(`/api/env/${ENV}/address-space/`)) return answer(entry);
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

interface MountProps {
  orphans?: OrphanRow[] | undefined;
  withheld?: OrphansWithheld;
  swarm?: boolean;
}

function mount(props: MountProps = {}) {
  const ui = (p: MountProps) => (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <OrphansCard
            envId={ENV}
            envName="Dev swarm"
            orphans={"orphans" in p ? p.orphans : []}
            withheld={p.withheld}
            swarm={p.swarm ?? true}
          />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
  const view = render(ui(props));
  return { rerender: (next: MountProps) => view.rerender(ui(next)) };
}

function openMenu(name: string) {
  fireEvent.pointerDown(screen.getByRole("button", { name: `Actions for ${name}` }), {
    button: 0,
    ctrlKey: false,
  });
}

const deletes = () => sent.filter((s) => s.method === "DELETE");

async function confirmDelete(name: string) {
  openMenu(name);
  await screen.findByRole("menuitem", { name: "Delete…" });
  // Who the person is arrives with /api/me; until then the action reads as unavailable.
  await waitFor(() =>
    expect(
      screen.getByRole("menuitem", { name: "Delete…" }).getAttribute("aria-disabled"),
    ).not.toBe("true"),
  );
  fireEvent.click(screen.getByRole("menuitem", { name: "Delete…" }));
  const dialog = await screen.findByRole("alertdialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete network" }));
  return dialog;
}

describe("groupOrphans", () => {
  it("sorts the server's answer into three groups without judging anything", () => {
    const groups = groupOrphans([
      orphan("a"),
      orphan("b", { class: "unused" }),
      orphan("c", { class: "cant_tell" }),
      orphan("d"),
    ]);
    expect(groups.orphaned.map((r) => r.name)).toEqual(["a", "d"]);
    expect(groups.unused.map((r) => r.name)).toEqual(["b"]);
    expect(groups.cant_tell.map((r) => r.name)).toEqual(["c"]);
  });
});

describe("OrphansCard", () => {
  it("shows the groups with their captions and the idle tooltip words", async () => {
    mount({
      orphans: [
        orphan("old-net"),
        orphan("loose", { class: "unused", formerOwner: "", idleSinceSource: "created" }),
        orphan("maybe", { class: "cant_tell" }),
      ],
      withheld: { managerMissing: false, nodes: ["worker-2"], stacks: [] },
    });
    expect(
      screen.getByText("Orphaned and unused networks", { selector: "[data-slot=card-title]" }),
    ).toBeTruthy();
    const table = screen.getByRole("table", { name: "Orphaned and unused networks" });
    expect(within(table).getByText("Orphaned (1)")).toBeTruthy();
    expect(
      within(table).getByText(
        "The stack that created these no longer exists, and nothing uses them.",
      ),
    ).toBeTruthy();
    expect(within(table).getByText("Unused (1)")).toBeTruthy();
    expect(within(table).getByText("Can't tell yet (1)")).toBeTruthy();
    expect(within(table).getByText("Waiting for worker-2 to report.")).toBeTruthy();
    expect(within(table).getByRole("columnheader", { name: "Former stack" })).toBeTruthy();
    expect(
      within(table).getAllByRole("img", {
        name: /The last time Coaming saw this network in use/,
      }),
    ).toHaveLength(2);
    expect(
      within(table).getByRole("img", {
        name: /Coaming hasn't seen this network in use. This is when it was created./,
      }),
    ).toBeTruthy();
  });

  it("words the former owner as a project on a standalone host", () => {
    mount({ swarm: false, orphans: [orphan("p", { formerOwnerKind: "project", scope: "local" })] });
    expect(screen.getByRole("columnheader", { name: "Former project" })).toBeTruthy();
    expect(screen.queryByRole("columnheader", { name: "Node" })).toBeNull();
  });

  it("deletes one orphan and words the kept outcomes", async () => {
    mount({ orphans: [orphan("old-net", { scope: "local", node: "worker-1", nodeId: "w1" })] });
    const dialog = await confirmDelete("old-net");
    expect(within(dialog).getByText("Delete “old-net”?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        /Coaming checks again just before it deletes the network, and keeps it if something started using it\. The network's details are saved in the audit log first\./,
      ),
    ).toBeTruthy();
    await waitFor(() => expect(deletes()).toHaveLength(1));
    // A local network is deleted on its own node.
    expect(deletes()[0]?.path).toBe(`/api/env/${ENV}/address-space/orphans/id-old-net`);
    expect(deletes()[0]?.search).toBe("?nodeId=w1");
    expect(await screen.findByText("Deleted old-net.")).toBeTruthy();
    cleanup();
    toast.dismiss();

    for (const [reason, text] of [
      ["in_use_again", "old-net is in use again, so Coaming kept it."],
      ["not_orphaned", "old-net is no longer orphaned, so Coaming kept it."],
      [
        "node_missing",
        "Couldn't confirm old-net is unused: worker-2 isn't reporting. Coaming kept it.",
      ],
    ] as const) {
      sent = [];
      answer = kept(reason);
      mount({
        orphans: [orphan("old-net")],
        withheld: { managerMissing: false, nodes: ["worker-2"], stacks: [] },
      });
      await confirmDelete("old-net");
      // An info toast, never an error one.
      expect(await screen.findByText(text)).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
      expect(deletes()).toHaveLength(1);
      cleanup();
      toast.dismiss();
    }
  });

  it("treats the engine's in-use refusal as a kept answer and sends the delete once", async () => {
    answer = problem(409, "network_in_use");
    mount({ orphans: [orphan("old-net")] });
    await confirmDelete("old-net");
    expect(await screen.findByText("old-net is in use again, so Coaming kept it.")).toBeTruthy();
    expect(deletes()).toHaveLength(1);
  });

  it("shows the refusal when the details are too large to save", async () => {
    answer = problem(409, "inspect_too_large");
    mount({ orphans: [orphan("old-net")] });
    await confirmDelete("old-net");
    expect(
      await screen.findByText(
        "Coaming didn't delete old-net: its details are over 64 KB, too large to save in the audit log first.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(deletes()).toHaveLength(1);
  });

  it("uses the generic line for any other refusal and never retries", async () => {
    answer = problem(500, "internal");
    mount({ orphans: [orphan("old-net")] });
    await confirmDelete("old-net");
    expect(await screen.findByText(/Something went wrong on the server\. Try again/)).toBeTruthy();
    expect(deletes()).toHaveLength(1);
  });
});

// A checkbox of the table by its name, once who the person is has arrived.
const box = (name: string) => screen.getByRole("checkbox", { name });

async function ready(label: string) {
  await waitFor(() => {
    const el = screen.getByRole("button", { name: label });
    expect(el.getAttribute("aria-disabled")).not.toBe("true");
  });
}

describe("OrphansCard (several)", () => {
  it("bulk deletes each target once and summarises", async () => {
    answer = (req) => {
      if (req.path.endsWith("/id-a")) return deleted();
      if (req.path.endsWith("/id-b")) return kept("in_use_again")();
      return problem(500, "internal")();
    };
    mount({
      orphans: [orphan("a"), orphan("b", { class: "unused", formerOwner: "" }), orphan("c")],
    });
    fireEvent.click(box("Select all in Orphaned"));
    fireEvent.click(box("Select b"));
    expect(screen.getByRole("status").textContent).toContain("3 selected");
    await ready("Delete…");
    fireEvent.click(screen.getByRole("button", { name: "Delete…" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete 3 networks?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        /Coaming checks each one again just before it deletes it, and keeps any that something started using\./,
      ),
    ).toBeTruthy();
    // The targets are listed, and Cancel has focus.
    expect(within(dialog).getByRole("cell", { name: "b" })).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 3 networks" }));

    expect(await screen.findByText("Deleted 1, kept 1, 1 failed")).toBeTruthy();
    // Each network went out once, in order, and none was retried.
    expect(deletes().map((d) => d.path.split("/").pop())).toEqual(["id-a", "id-c", "id-b"]);
    // A failure is an error toast; Details shows each network's outcome.
    fireEvent.click(screen.getByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog");
    const rows = within(results).getAllByRole("row").slice(1);
    expect(
      rows.map((r) =>
        within(r)
          .getAllByRole("cell")
          .map((c) => c.textContent),
      ),
    ).toEqual([
      ["a", "Cluster", "Deleted", "—"],
      [
        "c",
        "Cluster",
        "Failed",
        "Something went wrong on the server. Try again, and check the Coaming logs if it keeps happening.",
      ],
      ["b", "Cluster", "Kept", "b is in use again, so Coaming kept it."],
    ]);
    // The selection is cleared.
    expect(screen.queryByRole("toolbar")).toBeNull();
  });

  it("counts a too-large refusal as failed with its own line in Details", async () => {
    answer = (req) =>
      req.path.endsWith("/id-a") ? problem(409, "inspect_too_large")() : deleted();
    mount({ orphans: [orphan("a"), orphan("b")] });
    fireEvent.click(box("Select all in Orphaned"));
    await ready("Delete…");
    fireEvent.click(screen.getByRole("button", { name: "Delete…" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 2 networks" }));
    expect(await screen.findByText("Deleted 1, kept 0, 1 failed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog");
    expect(
      within(results).getByText(
        "Coaming didn't delete a: its details are over 64 KB, too large to save in the audit log first.",
      ),
    ).toBeTruthy();
    expect(deletes()).toHaveLength(2);
  });

  it("says Deleted n networks when every one went", async () => {
    mount({ orphans: [orphan("a"), orphan("b")] });
    fireEvent.click(box("Select all in Orphaned"));
    await ready("Delete…");
    fireEvent.click(screen.getByRole("button", { name: "Delete…" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 2 networks" }));
    expect(await screen.findByText("Deleted 2 networks.")).toBeTruthy();
  });

  it("keeps selected networks", async () => {
    answer = () => Response.json({ kept: 2 });
    mount({
      orphans: [
        orphan("a"),
        orphan("b", { class: "unused", scope: "local", node: "worker-1", nodeId: "w1" }),
      ],
    });
    fireEvent.click(box("Select a"));
    fireEvent.click(box("Select b"));
    await ready("Keep");
    fireEvent.click(screen.getByRole("button", { name: "Keep" }));
    expect(await screen.findByText("Keeping 2 networks.")).toBeTruthy();
    const put = sent.find((s) => s.method === "PUT");
    expect(put?.path).toBe(`/api/env/${ENV}/address-space/keeps`);
    // A local network is kept on its node; a swarm-wide one for the cluster.
    expect(put?.body).toEqual({ targets: [{ name: "a" }, { name: "b", nodeId: "w1" }] });
    expect(screen.queryByRole("toolbar")).toBeNull();
  });

  it("keeps one network from its row", async () => {
    answer = () => Response.json({ kept: 1 });
    mount({ orphans: [orphan("a")] });
    openMenu("a");
    await screen.findByRole("menuitem", { name: "Keep" });
    await waitFor(() =>
      expect(screen.getByRole("menuitem", { name: "Keep" }).getAttribute("aria-disabled")).not.toBe(
        "true",
      ),
    );
    fireEvent.click(screen.getByRole("menuitem", { name: "Keep" }));
    expect(await screen.findByText("Keeping a. Coaming won't list it again.")).toBeTruthy();
  });

  it("disables cant-tell rows with the reason", async () => {
    mount({
      orphans: [orphan("a"), orphan("maybe", { class: "cant_tell" })],
      withheld: { managerMissing: false, nodes: ["worker-2"], stacks: [] },
    });
    const reason =
      "Waiting for worker-2 to report. Coaming won't remove a network until every node has answered.";
    // The row cannot be chosen, and the reason is its description.
    const maybe = box("Select maybe");
    expect(maybe.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(maybe);
    expect(maybe.getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByRole("checkbox", { name: "Select all in Can't tell yet" })).toBeNull();
    openMenu("maybe");
    const del = await screen.findByRole("menuitem", { name: /Delete…/ });
    await waitFor(() =>
      expect(screen.getByRole("menuitem", { name: /Delete…/ }).getAttribute("aria-disabled")).toBe(
        "true",
      ),
    );
    expect(del).toBeTruthy();
    expect(screen.getAllByText(reason).length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole("menuitem", { name: /Delete…/ }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(deletes()).toHaveLength(0);
  });
});

describe("OrphansCard (states)", () => {
  it("shows the empty state", () => {
    mount({ orphans: [] });
    expect(screen.getByText("No orphaned or unused networks")).toBeTruthy();
    expect(
      screen.getByText("Coaming lists a network here when nothing has used it for 24 hours."),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows the loading skeleton", () => {
    mount({ orphans: undefined });
    const table = screen.getByRole("table", { name: "Orphaned and unused networks" });
    expect(table.querySelectorAll("[data-slot=skeleton-row]")).toHaveLength(8);
    expect(screen.getByText("Loading orphaned and unused networks")).toBeTruthy();
    expect(table.closest("[aria-busy=true]")).toBeTruthy();
  });

  it("says the judgement is withheld when a node is not reporting", () => {
    mount({
      orphans: [orphan("maybe", { class: "cant_tell" })],
      withheld: { managerMissing: false, nodes: ["worker-2", "worker-3"], stacks: [] },
    });
    expect(screen.getByText("Waiting for worker-2, worker-3 to report.")).toBeTruthy();
    expect(screen.queryByText(/^Orphaned \(/)).toBeNull();
  });

  it("disables actions below Env Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount({ orphans: [orphan("a")] });
    openMenu("a");
    for (const label of ["Keep", "Delete…"]) {
      await waitFor(() =>
        expect(
          screen.getByRole("menuitem", { name: new RegExp(label) }).getAttribute("aria-disabled"),
        ).toBe("true"),
      );
    }
    expect(screen.getAllByText("Needs Admin on Dev swarm").length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole("menuitem", { name: /Delete…/ }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(sent.some((s) => s.method === "DELETE" || s.method === "PUT")).toBe(false);
  });

  it("disables the bulk actions below Env Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount({ orphans: [orphan("a")] });
    fireEvent.click(box("Select a"));
    const bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    await waitFor(() =>
      expect(within(bar).getByRole("button", { name: "Keep" }).getAttribute("aria-disabled")).toBe(
        "true",
      ),
    );
    expect(within(bar).getByRole("button", { name: "Delete…" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
  });
});
