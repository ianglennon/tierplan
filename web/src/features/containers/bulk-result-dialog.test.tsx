// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";
const BULK = "018f0000-0000-7000-8000-000000000001";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;

beforeEach(() => {
  setCsrfToken(null);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/me") return Response.json(me);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function report(bulkId: string | undefined): BulkReport {
  return {
    verb: "stop",
    bulkId,
    rows: [{ name: "web", node: "node-1", result: "failed", reason: "x", code: "action_failed" }],
  };
}

async function mount(r: BulkReport) {
  const root = createRootRoute({
    component: () => <BulkResultDialog envId={ENV} report={r} onClose={() => {}} />,
  });
  const audit = createRoute({ getParentRoute: () => root, path: "/audit", component: () => null });
  const router = createRouter({
    routeTree: root.addChildren([audit]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await screen.findByRole("dialog", { name: "Stop results" });
}

describe("BulkResultDialog audit link", () => {
  it("links to the audit log filtered by the bulk ID for someone who can read the audit log", async () => {
    me = makeMe({ instanceAdmin: true });
    await mount(report(BULK));
    const link = await screen.findByRole("link", { name: "View in audit log" });
    const url = new URL(link.getAttribute("href") ?? "", "http://x");
    expect(url.pathname).toBe("/audit");
    expect(url.searchParams.get("bulk")).toBe(BULK);
  });

  it("has no link for someone who cannot read the audit log", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(report(BULK));
    // The user is known once /api/me has answered; give it that, then look.
    await vi.waitFor(() => expect(client.getQueryState(["me"])?.status).not.toBe("pending"));
    expect(screen.queryByRole("link", { name: "View in audit log" })).toBeNull();
  });

  it("has no link when the operation has no bulk ID", async () => {
    me = makeMe({ instanceAdmin: true });
    await mount(report(undefined));
    await vi.waitFor(() => expect(client.getQueryState(["me"])?.status).not.toBe("pending"));
    expect(screen.queryByRole("link", { name: "View in audit log" })).toBeNull();
  });
});
