// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerList } from "@/features/containers/container-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// A task of the virtual IP service (selfKind ingress_vip) is TierPlan's own, as a proxy task is
// (UI-SPEC S1, U-03): the same badge, a typed name for a single action, never in a bulk. Only the
// confirmation's words are its own.

const ENV = "e1";

function row(name: string, extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: `id-${name}`,
    name,
    state: "running",
    status: "Up 2 hours",
    image: "tierplan-keepalived:1",
    created: "2026-10-01T10:00:00Z",
    ports: [],
    isSelf: false,
    ingressOwned: false,
    project: "",
    stack: "",
    tierplanStackId: "",
    service: "",
    health: "none",
    selfKind: "",
    readOnly: false,
    ...extra,
  };
}

const KEEPALIVED = "tierplan-keepalived.x1.abc";
const SECOND = "tierplan-keepalived.x2.def";

let client: QueryClient;
// Removing a running container also needs Force, which is not what is under test; the engine
// takes an exited one without.
let firstState: ContainerRow["state"];
let posts: { path: string; body: Record<string, unknown> }[];

beforeEach(() => {
  posts = [];
  firstState = "running";
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (url.pathname === "/api/environments")
        return Response.json({
          items: [
            {
              id: ENV,
              name: "Swarm",
              type: "swarm",
              envType: "swarm",
              status: { state: "online" },
            },
          ],
        });
      if (url.pathname === `/api/env/${ENV}/summary`)
        return Response.json({ environment: { id: ENV, type: "swarm" } });
      if (url.pathname.startsWith(`/api/env/${ENV}/containers/`) && req.method === "POST") {
        posts.push({ path: url.pathname, body: await req.clone().json() });
        return Response.json({ bulkId: "018f0000-0000-7000-8000-000000000001", results: [] });
      }
      if (url.pathname === `/api/env/${ENV}/containers`) {
        const items = [
          row(KEEPALIVED, { selfKind: "ingress_vip", state: firstState }),
          row(SECOND, { selfKind: "ingress_vip", node: "node-2", nodeId: "n2" }),
          row("web"),
        ];
        return Response.json({
          items,
          total: items.length,
          truncated: false,
          unfilteredTotal: items.length,
          statusCounts: { running: 3, exited: 0, paused: 0, restarting: 0, unhealthy: 0 },
          nodes: [],
          notReporting: [],
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

async function mountList() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ContainerList envId={ENV} search={{ group: "none" }} onSearchChange={() => {}} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  await screen.findByText("web");
  await waitFor(() => expect(screen.getAllByRole("checkbox").length).toBeGreaterThan(0));
}

const nameCell = (name: string) =>
  (screen.getByText(name).closest("td") as HTMLElement).textContent;

async function openRowMenu(name: string) {
  await waitFor(() =>
    expect(screen.getByRole("button", { name: `Actions for ${name}` })).toBeTruthy(),
  );
  await act(async () => {});
  await act(async () => {
    fireEvent.keyDown(screen.getByRole("button", { name: `Actions for ${name}` }), {
      key: "Enter",
    });
  });
  await screen.findByRole("menu");
}

describe("a keepalived task in the container list", () => {
  it("wears the TierPlan ingress badge on every node's task and an ordinary container does not", async () => {
    await mountList();
    expect(nameCell(KEEPALIVED)).toBe(`${KEEPALIVED}TierPlan ingress`);
    expect(nameCell(SECOND)).toBe(`${SECOND}TierPlan ingress`);
    expect(nameCell("web")).toBe("web");
  });

  it("keeps every keepalived task out of a bulk: a checkbox that cannot be ticked, and select all skips it", async () => {
    await mountList();
    for (const name of [KEEPALIVED, SECOND]) {
      const own = screen.getByRole("checkbox", { name: `Select ${name}` });
      expect(own.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(own);
      expect(own.getAttribute("aria-checked")).toBe("false");
    }
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all containers" }));
    expect(within(screen.getByRole("toolbar")).getByText("1 selected")).toBeTruthy();
    for (const name of [KEEPALIVED, SECOND]) {
      expect(
        screen.getByRole("checkbox", { name: `Select ${name}` }).getAttribute("aria-checked"),
      ).toBe("false");
    }
  });

  it.each([
    ["Stop", "Stopping"],
    ["Kill", "Killing"],
    ["Restart", "Restarting"],
    ["Pause", "Pausing"],
    ["Remove", "Removing"],
  ] as const)(
    "%s opens the type-to-confirm with the virtual IP words, and the button waits for the name",
    async (word, gerund) => {
      if (word === "Remove") firstState = "exited";
      await mountList();
      await openRowMenu(KEEPALIVED);
      fireEvent.click(await screen.findByRole("menuitem", { name: new RegExp(`^${word}`) }));
      const dialog = await screen.findByRole("alertdialog", { name: `${word} ${KEEPALIVED}?` });
      expect(dialog.textContent).toContain(
        `This is the virtual IP service on node-1. ${gerund} it takes node-1 out of the virtual IP's candidates, and moves the address away if node-1 holds it, until Swarm replaces it.`,
      );
      expect(dialog.textContent).not.toContain("HTTPS");
      const confirm = within(dialog).getByRole("button", {
        name: `${word} virtual IP service`,
      });
      expect(confirm.getAttribute("aria-disabled")).toBe("true");
      const field = within(dialog).getByLabelText(`Type ${KEEPALIVED} to confirm`);
      fireEvent.change(field, { target: { value: `${KEEPALIVED}-x` } });
      expect(confirm.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(confirm);
      expect(posts).toEqual([]);
      fireEvent.change(field, { target: { value: KEEPALIVED } });
      expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
      expect(within(dialog).queryByRole("button", { name: "Advanced" })).toBeNull();
      fireEvent.click(confirm);
      await waitFor(() => expect(posts).toHaveLength(1));
      // Remove is its own endpoint and carries no action field; the others name theirs.
      expect(posts[0]?.body).toMatchObject({
        ...(word === "Remove" ? {} : { action: word.toLowerCase() }),
        confirm: KEEPALIVED,
        targets: [{ nodeId: "n1", id: `id-${KEEPALIVED}` }],
      });
    },
  );
});
