// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import "@/i18n";
import { makeMe } from "@/test-me";

let me: ReturnType<typeof makeMe>;

beforeEach(() => {
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments")
        return Response.json({
          items: [
            {
              id: "e1",
              name: "Lab host",
              type: "swarm",
              envType: "swarm",
              status: { state: "online" },
            },
          ],
        });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute({ component: () => <AgentUpdateNotice envId="e1" /> });
  const settings = createRoute({
    getParentRoute: () => root,
    path: "/settings/environments/$envId",
    component: () => null,
  });
  const router = createRouter({
    routeTree: root.addChildren([settings]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await waitFor(() => expect(router.state.status).toBe("idle"));
}

describe("AgentUpdateNotice", () => {
  it("says the agent needs an update and links to the environment's agent settings for someone who can configure it", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    await mount();
    const notice = await screen.findByRole("alert");
    expect(notice.textContent).toContain("This host's agent needs an update");
    const link = await screen.findByRole("link", { name: "Open agent settings" });
    expect(link.getAttribute("href")).toBe("/settings/environments/e1");
    await waitFor(() =>
      expect(notice.textContent).toContain(
        "It's too old to do this. Update it in Settings → Environments → Lab host, then try again.",
      ),
    );
    expect(notice.textContent).not.toContain("Ask an administrator");
  });

  it("links for someone with settings.read too", async () => {
    me = makeMe({
      envs: { e1: "operator" },
      permissions: { global: ["settings.read"], environments: { e1: [] } },
    });
    await mount();
    expect(await screen.findByRole("link", { name: "Open agent settings" })).toBeTruthy();
  });

  it("ends by telling everyone else to ask an administrator, with no link", async () => {
    me = makeMe({ envs: { e1: "operator" } });
    await mount();
    const notice = await screen.findByRole("alert");
    await waitFor(() => expect(notice.textContent).toContain("Ask an administrator to update it."));
    expect(notice.textContent?.endsWith("Ask an administrator to update it.")).toBe(true);
    expect(screen.queryByRole("link")).toBeNull();
  });
});
