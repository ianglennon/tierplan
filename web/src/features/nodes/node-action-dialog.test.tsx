// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { NodeImpact, NodeImpactAction } from "@/api/nodes";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodeActionDialog } from "@/features/nodes/node-action-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

// Drain, pause, promote and demote (plan 06-15, RES-05, D-20, UI-SPEC "Nodes"): the dialog asks
// the server what the change will do, shows it, and asks for the node's name only when the server
// says it is needed.

const ENV = "e1";
const NODE = { id: "n3", hostname: "wkr-1" };

const impact = (extra: Partial<NodeImpact> = {}): NodeImpact => ({
  action: "drain",
  hostname: "wkr-1",
  version: 4,
  tasks: [],
  managers: { total: 3, reachable: 3, after: 3 },
  warnings: [],
  needsConfirm: false,
  ...extra,
});

interface Posted {
  path: string;
  body: Record<string, unknown>;
}

let impactSeen: string[];
let posted: Posted[];
let answerImpact: () => Response;
let answerPost: () => Response;
let impactGate: Promise<void> | null;
let postGate: Promise<void> | null;

beforeEach(() => {
  impactSeen = [];
  posted = [];
  answerImpact = () => Response.json(impact());
  answerPost = () => new Response(null, { status: 204 });
  impactGate = null;
  postGate = null;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      }
      if (url.pathname === `/api/env/${ENV}/nodes/${NODE.id}/impact`) {
        impactSeen.push(url.search);
        if (impactGate) await impactGate;
        return answerImpact();
      }
      if (req.method === "POST" && url.pathname.startsWith(`/api/env/${ENV}/nodes/${NODE.id}/`)) {
        posted.push({ path: url.pathname, body: (await req.json()) as Record<string, unknown> });
        if (postGate) await postGate;
        return answerPost();
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

function mount(mode: NodeImpactAction = "drain", node = NODE) {
  const onClose = vi.fn();
  const onDone = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <NodeActionDialog envId={ENV} node={node} mode={mode} onClose={onClose} onDone={onDone} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onClose, onDone };
}

// The dialog once the impact has loaded.
async function dialogOf(name: string) {
  const dialog = await screen.findByRole("dialog", { name });
  await waitFor(() => expect(within(dialog).queryByText("Checking what this will do…")).toBeNull());
  return dialog;
}

describe("NodeActionDialog loading", () => {
  it("shows a spinner row and keeps the confirm off until the impact arrives", async () => {
    let open: () => void = () => {};
    impactGate = new Promise<void>((resolve) => {
      open = resolve;
    });
    mount("drain");
    const dialog = await screen.findByRole("dialog", { name: "Drain wkr-1?" });
    expect(within(dialog).getByText("Checking what this will do…")).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Drain node" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(posted).toEqual([]);
    open();
    await waitFor(() =>
      expect(within(dialog).queryByText("Checking what this will do…")).toBeNull(),
    );
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
    expect(impactSeen).toEqual(["?action=drain"]);
  });

  it("says it could not check, keeps the confirm off and offers a retry", async () => {
    answerImpact = () => problem(503, "no_manager_connected");
    mount("drain");
    const dialog = await screen.findByRole("dialog", { name: "Drain wkr-1?" });
    expect(await within(dialog).findByText(/^Couldn't check what this will do/)).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Drain node" }).getAttribute("aria-disabled"),
    ).toBe("true");
    answerImpact = () => Response.json(impact());
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(within(dialog).queryByText(/^Couldn't check/)).toBeNull());
    expect(
      within(dialog).getByRole("button", { name: "Drain node" }).getAttribute("aria-disabled"),
    ).not.toBe("true");
  });
});

describe("NodeActionDialog drain", () => {
  it("asks Drain {node}? and says what Swarm does", async () => {
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(
      within(dialog).getByText(
        "Swarm stops sending tasks to it and moves the ones it has to other nodes.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
  });

  it("lists the tasks with Task, Service and What happens", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          tasks: [
            { id: "t1", serviceId: "s1", service: "web", slot: 1, outcome: "moves" },
            { id: "t2", serviceId: "s2", service: "agent", slot: 0, outcome: "stops" },
            { id: "t3", serviceId: "s3", service: "db", slot: 1, outcome: "cannot_place" },
          ],
        }),
      );
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    for (const header of ["Task", "Service", "What happens"]) {
      expect(within(dialog).getByRole("columnheader", { name: header })).toBeTruthy();
    }
    const web = within(dialog).getByText("web.1").closest("tr") as HTMLElement;
    expect(within(web).getByText("Moves to another node")).toBeTruthy();
    const agent = within(dialog)
      .getAllByRole("cell", { name: "agent" })[0]
      ?.closest("tr") as HTMLElement;
    expect(within(agent).getByText("Stops. Global services aren't moved.")).toBeTruthy();
    const db = within(dialog).getByText("db.1").closest("tr") as HTMLElement;
    expect(within(db).getByText("Can't be placed anywhere else")).toBeTruthy();
  });

  it("says no tasks are running there when the node has none", async () => {
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(within(dialog).getByText("No tasks are running here.")).toBeTruthy();
    expect(within(dialog).queryByRole("table")).toBeNull();
  });

  it("scrolls 300 tasks inside the 240px list", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          tasks: Array.from({ length: 300 }, (_, i) => ({
            id: `t${i}`,
            serviceId: "s1",
            service: "web",
            slot: i + 1,
            outcome: "moves",
          })),
        }),
      );
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    const list = dialog.querySelector('[data-slot="target-list"]') as HTMLElement;
    expect(list.className).toContain("max-h-60");
    expect(list.className).toContain("overflow-y-auto");
    expect(within(list).getAllByRole("row")).toHaveLength(301);
  });

  it("drains with the version it was shown, says so and closes", async () => {
    const { onClose, onDone } = mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    fireEvent.click(within(dialog).getByRole("button", { name: "Drain node" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      path: "/api/env/e1/nodes/n3/availability",
      body: { availability: "drain", version: 4 },
    });
    expect(await screen.findByText("Drained wkr-1.")).toBeTruthy();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onDone).toHaveBeenCalledWith({ nodeId: "n3", action: "drain" });
  });

  it("sends one request on a second click and cannot be closed while it is out", async () => {
    let open: () => void = () => {};
    postGate = new Promise<void>((resolve) => {
      open = resolve;
    });
    const { onClose } = mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    const confirm = within(dialog).getByRole("button", { name: "Drain node" });
    fireEvent.click(confirm);
    await waitFor(() => expect(within(dialog).getByText("Draining…")).toBeTruthy());
    fireEvent.click(confirm);
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
    open();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(posted).toHaveLength(1);
  });
});

describe("NodeActionDialog warnings and the typed name", () => {
  it("warns that Bayplan runs on the node, and asks for the name before a drain", async () => {
    answerImpact = () => Response.json(impact({ warnings: ["runs_bayplan"], needsConfirm: true }));
    const { onClose } = mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(
      within(dialog).getByText(
        "Bayplan runs on wkr-1. Draining it can cut Bayplan off from this swarm.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByLabelText("Type wkr-1 to confirm")).toBeTruthy();
    // The confirm is destructive and carries the node's name.
    const confirm = within(dialog).getByRole("button", { name: "Drain wkr-1" });
    expect(confirm.className).toContain("bg-destructive-fill");
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(posted).toEqual([]);

    fireEvent.change(within(dialog).getByLabelText("Type wkr-1 to confirm"), {
      target: { value: "wkr-1" },
    });
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(confirm);
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toEqual({ availability: "drain", version: 4, confirm: "wkr-1" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("warns that the node has the only manager agent", async () => {
    answerImpact = () =>
      Response.json(impact({ warnings: ["only_manager_agent"], needsConfirm: true }));
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(
      within(dialog).getByText(
        "wkr-1 has the only manager agent connected to Bayplan. Draining it leaves Bayplan unable to change this swarm until another manager's agent connects.",
      ),
    ).toBeTruthy();
  });

  it("asks for no name when the server says none is needed", async () => {
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Drain node" }).className).not.toContain(
      "bg-destructive-fill",
    );
  });

  it("wraps a long node name in the title", async () => {
    const long = "w".repeat(80);
    answerImpact = () => Response.json(impact({ hostname: long }));
    mount("drain", { id: "n3", hostname: long });
    const title = await screen.findByRole("heading", { name: `Drain ${long}?` });
    expect(title.className).toContain("break-all");
  });
});

describe("NodeActionDialog pause", () => {
  it("says the tasks keep running, plural-aware, and sends a pause", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "pause",
          tasks: [
            { id: "t1", serviceId: "s1", service: "web", slot: 1, outcome: "stays" },
            { id: "t2", serviceId: "s1", service: "web", slot: 2, outcome: "stays" },
            { id: "t3", serviceId: "s2", service: "db", slot: 1, outcome: "stays" },
          ],
        }),
      );
    mount("pause");
    const dialog = await dialogOf("Pause wkr-1?");
    expect(
      within(dialog).getByText(
        "Swarm stops sending new tasks to it. The 3 tasks already there keep running.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Pause node" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toEqual({ availability: "pause", version: 4 });
    expect(await screen.findByText("Paused wkr-1.")).toBeTruthy();
  });

  it("says one task in the singular", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "pause",
          tasks: [{ id: "t1", serviceId: "s1", service: "web", slot: 1, outcome: "stays" }],
        }),
      );
    mount("pause");
    const dialog = await dialogOf("Pause wkr-1?");
    expect(within(dialog).getByText(/The 1 task already there keeps running\.$/)).toBeTruthy();
  });
});

describe("NodeActionDialog promote and demote", () => {
  it("promotes, saying how many managers there will be", async () => {
    answerImpact = () =>
      Response.json(impact({ action: "promote", managers: { total: 3, reachable: 3, after: 4 } }));
    mount("promote");
    const dialog = await dialogOf("Promote wkr-1 to manager?");
    expect(
      within(dialog).getByText(
        "Managers keep the swarm's state and answer API calls. You'll have 4 managers.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Promote node" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      path: "/api/env/e1/nodes/n3/role",
      body: { role: "manager", version: 4 },
    });
    expect(await screen.findByText("Promoted wkr-1.")).toBeTruthy();
  });

  it("demotes with the even-count warning and no typed name", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "demote",
          managers: { total: 3, reachable: 3, after: 2 },
          warnings: ["even_count"],
        }),
      );
    mount("demote");
    const dialog = await dialogOf("Demote wkr-1 to worker?");
    expect(
      within(dialog).getByText("It stops being a manager. You'll have 2 managers."),
    ).toBeTruthy();
    expect(
      within(dialog).getByText(
        "With 2 managers the swarm tolerates losing 0, the same as with 1. An odd number is better.",
      ),
    ).toBeTruthy();
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Demote node" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toEqual({ role: "worker", version: 4 });
    expect(await screen.findByText("Demoted wkr-1.")).toBeTruthy();
  });

  it("says how many managers tolerate how many losses, one manager in the singular", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "demote",
          managers: { total: 4, reachable: 4, after: 3 },
          warnings: [],
        }),
      );
    mount("demote");
    const dialog = await dialogOf("Demote wkr-1 to worker?");
    expect(
      within(dialog).getByText("It stops being a manager. You'll have 3 managers."),
    ).toBeTruthy();

    cleanup();
    answerImpact = () =>
      Response.json(
        impact({
          action: "demote",
          managers: { total: 2, reachable: 2, after: 1 },
          warnings: [],
        }),
      );
    mount("demote");
    const single = await dialogOf("Demote wkr-1 to worker?");
    expect(
      within(single).getByText("It stops being a manager. You'll have 1 manager."),
    ).toBeTruthy();
  });

  it("warns that quorum is at risk and asks for the name before a demote", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "demote",
          managers: { total: 3, reachable: 2, after: 2 },
          warnings: ["quorum_at_risk", "even_count"],
          needsConfirm: true,
        }),
      );
    mount("demote");
    const dialog = await dialogOf("Demote wkr-1 to worker?");
    expect(
      within(dialog).getByText(
        "Only 2 of 3 managers are reachable. Demoting this one could leave the swarm without a majority, and it would stop accepting changes.",
      ),
    ).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Demote wkr-1" });
    expect(confirm.className).toContain("bg-destructive-fill");
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(within(dialog).getByLabelText("Type wkr-1 to confirm"), {
      target: { value: "wkr-1" },
    });
    fireEvent.click(confirm);
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toEqual({ role: "worker", version: 4, confirm: "wkr-1" });
  });

  it("says it could not check the managers and still asks for the name", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "demote",
          managers: { total: 3, reachable: 2, after: 2 },
          warnings: ["facts_unreadable", "even_count"],
          needsConfirm: true,
        }),
      );
    mount("demote");
    const dialog = await dialogOf("Demote wkr-1 to worker?");
    expect(
      within(dialog).getByText(
        "Bayplan couldn't check the managers' state, so it can't tell whether this is safe.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByLabelText("Type wkr-1 to confirm")).toBeTruthy();
  });

  it("warns that Bayplan runs on the node being demoted", async () => {
    answerImpact = () =>
      Response.json(
        impact({
          action: "demote",
          managers: { total: 4, reachable: 4, after: 3 },
          warnings: ["runs_bayplan"],
          needsConfirm: true,
        }),
      );
    mount("demote");
    const dialog = await dialogOf("Demote wkr-1 to worker?");
    expect(
      within(dialog).getByText(
        "Bayplan runs on wkr-1. Demoting it can cut Bayplan off from this swarm.",
      ),
    ).toBeTruthy();
  });
});

describe("NodeActionDialog refusals", () => {
  async function refused(response: () => Response, mode: NodeImpactAction = "demote") {
    answerPost = response;
    const { onClose } = mount(mode);
    const dialog = await dialogOf(mode === "demote" ? "Demote wkr-1 to worker?" : "Drain wkr-1?");
    fireEvent.click(
      within(dialog).getByRole("button", {
        name: mode === "demote" ? "Demote node" : "Drain node",
      }),
    );
    await waitFor(() => expect(posted).toHaveLength(1));
    return { dialog, onClose };
  }

  it("words Docker's refusal to demote the last manager and stays open", async () => {
    const { dialog, onClose } = await refused(() => problem(409, "last_manager"));
    expect(
      await within(dialog).findByText(
        "You can't demote the last manager. Promote another node first.",
      ),
    ).toBeTruthy();
    expect(onClose).not.toHaveBeenCalled();
    // The button is back for another try.
    expect(within(dialog).getByRole("button", { name: "Demote node" })).toBeTruthy();
  });

  it("words a node that changed in between and does not retry", async () => {
    const { dialog } = await refused(() => problem(409, "node_changed"));
    expect(
      await within(dialog).findByText(
        "That changed while you were looking at it. Reload the page and try again.",
      ),
    ).toBeTruthy();
    expect(posted).toHaveLength(1);
  });

  it("passes on Docker's own words for any other refusal", async () => {
    const { dialog } = await refused(() =>
      problem(502, "node_update_failed", "rpc error: no leader elected"),
    );
    expect(
      await within(dialog).findByText("Docker refused: rpc error: no leader elected"),
    ).toBeTruthy();
  });

  it("words a swarm with no manager connected", async () => {
    const { dialog } = await refused(() => problem(503, "no_manager_connected"), "drain");
    expect(
      await within(dialog).findByText(
        "No manager is connected for Swarm one, so this swarm can't be changed right now. Reconnect a manager's agent and try again.",
      ),
    ).toBeTruthy();
  });

  it("words the server asking for the name that a stale page skipped", async () => {
    const { dialog } = await refused(() => problem(422, "confirmation_required"), "drain");
    expect(await within(dialog).findByText("Type wkr-1 to confirm this.")).toBeTruthy();
  });

  it("says a connection that dropped may have gone through", async () => {
    const { dialog } = await refused(() => {
      throw new TypeError("network");
    });
    expect(
      await within(dialog).findByText(
        "The connection dropped before Bayplan answered. The list shows whether it happened.",
      ),
    ).toBeTruthy();
  });

  it("words a lost permission", async () => {
    const { dialog } = await refused(() => problem(403, "forbidden"));
    expect(
      await within(dialog).findByText(
        "You no longer have permission to do that. Your access may have changed.",
      ),
    ).toBeTruthy();
  });
});
