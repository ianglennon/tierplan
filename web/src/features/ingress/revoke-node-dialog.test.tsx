// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodesTab } from "@/features/ingress/nodes-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let items: Json[];
let revokeAnswer: () => Response;
let posts: { path: string; body: unknown }[];
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

function node(over: Json = {}): Json {
  return {
    id: "n1",
    name: "edge-1",
    swarmNodeId: "sw1",
    status: "active",
    version: "v0.4.2",
    schemas: [1],
    appliedBundleId: "9f8e7d6c5b4a",
    rejectedBundleId: "",
    rejectedReason: "",
    health: "ok",
    lastSeenAt: "2026-11-20T10:05:00Z",
    createdAt: "2026-11-01T10:00:00Z",
    revokedBy: "",
    revokeReason: "",
    reenrolBlocked: false,
    heldHostnames: [],
    incompatible: false,
    connected: true,
    sync: "current",
    ...over,
  };
}

beforeEach(() => {
  setCsrfToken(null);
  items = [node()];
  posts = [];
  revokeAnswer = () =>
    Response.json({
      heldHostnames: ["a.example.com"],
      rotateTokenRecommended: true,
      revokedAt: "2026-11-26T10:00:00Z",
    });
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/nodes`) {
        return Response.json({ items, currentBundleId: "9f8e7d6c5b4a" });
      }
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json({ enabled: true });
      if (path === `/api/env/${ENV}/ingress/nodes/n1/revoke`) {
        posts.push({ path, body: await req.json() });
        return revokeAnswer();
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

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <NodesTab envId={ENV} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const openDialog = async () => {
  fireEvent.click(await screen.findByRole("button", { name: "Revoke edge-1" }));
  return screen.findByRole("alertdialog");
};

describe("revoking an ingress node", () => {
  it("opens with what revoking does, and Keep node sends nothing", async () => {
    mount();
    const dialog = await openDialog();
    expect(within(dialog).getByText("Revoke edge-1?")).toBeTruthy();
    expect(
      within(dialog).getByText(/This node stops serving routes now and its key is refused/),
    ).toBeTruthy();
    expect(within(dialog).getByText("Shown in the node table and the audit log.")).toBeTruthy();
    expect(
      (
        within(dialog).getByRole("checkbox", {
          name: "Let this host enrol again with the join token it still holds",
        }) as HTMLElement
      ).getAttribute("aria-checked"),
    ).toBe("false");
    expect(within(dialog).getByLabelText("Reason").getAttribute("maxlength")).toBe("256");
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep node" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(posts).toEqual([]);
  });

  it("posts the reason once and replaces the body with what to do next", async () => {
    mount();
    const dialog = await openDialog();
    fireEvent.change(within(dialog).getByLabelText("Reason"), {
      target: { value: "decommissioned" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    await screen.findByText("Revoked edge-1");
    expect(posts).toEqual([
      {
        path: `/api/env/${ENV}/ingress/nodes/n1/revoke`,
        body: { reason: "decommissioned", allowReenrol: false },
      },
    ]);
    const done = screen.getByRole("alertdialog");
    expect(
      within(done).getByText(
        "Rotate the join token. It is still mounted in that node's container.",
      ),
    ).toBeTruthy();
    const host = within(done).getByText("a.example.com");
    expect(host.className).toContain("font-mono");
    expect(done.textContent).toContain("This node may still hold the keys for: a.example.com.");
    expect(done.textContent).toContain("Request new certificates for them.");
    expect(within(done).queryByRole("button", { name: "Revoke node" })).toBeNull();
    fireEvent.click(within(done).getByRole("button", { name: "Close details" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(posts).toHaveLength(1);
  });

  it("sends allowReenrol when the box is ticked and leaves the reason out when empty", async () => {
    mount();
    const dialog = await openDialog();
    fireEvent.click(
      within(dialog).getByRole("checkbox", {
        name: "Let this host enrol again with the join token it still holds",
      }),
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    await screen.findByText("Revoked edge-1");
    expect(posts[0]?.body).toEqual({ allowReenrol: true });
  });

  it("leaves out the hostnames paragraph when the node held none", async () => {
    revokeAnswer = () =>
      Response.json({ heldHostnames: null, rotateTokenRecommended: true, revokedAt: "x" });
    mount();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    await screen.findByText("Revoked edge-1");
    expect(screen.getByRole("alertdialog").textContent).not.toContain("may still hold the keys");
  });

  it("shows the server's detail in a destructive alert when it refuses, and stays open", async () => {
    revokeAnswer = () =>
      Response.json(
        {
          title: "Conflict",
          status: 409,
          detail: "That node was revoked while you were looking at it.",
        },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    mount();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    expect(
      await screen.findByText("That node was revoked while you were looking at it."),
    ).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(screen.queryByText("Revoked edge-1")).toBeNull();
  });

  it("disables the row's button with its reason without environments.revoke", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const button = await screen.findByRole("button", { name: "Revoke edge-1" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    expect(screen.getByText("Needs Admin")).toBeTruthy();
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
