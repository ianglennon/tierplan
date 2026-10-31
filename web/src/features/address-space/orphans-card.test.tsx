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
