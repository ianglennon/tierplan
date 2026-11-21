// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import { queryClient } from "@/api/query-client";
import type { components } from "@/api/schema";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The proxy join token's secret is TierPlan's (UI-SPEC S3, D-29, DEP-08): badged, shown with a
// neutral notice, and Delete and Rotate are aria-disabled with the reason on focus.

type SecretItem = components["schemas"]["SecretItem"];
type SecretDetail = components["schemas"]["SecretDetailBody"];

const ENV = "e1";
const BASE = `/api/env/${ENV}`;
const TOKEN = "tierplan-ingress-token-abc";
const REASON = "Managed by TierPlan. Switch ingress off to remove it.";

let sent: { method: string; path: string }[];

const item = (name: string, extra: Partial<SecretItem> = {}): SecretItem => ({
  id: `sid-${name}`,
  name,
  labels: {},
  owner: "",
  ownerKind: "",
  ingress: false,
  tierplanStackId: "",
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-02T10:00:00Z",
  usedBy: 0,
  ...extra,
});

const detailOf = (s: SecretItem): SecretDetail => ({ ...s, usedByRows: [] });

const secrets = () => [item(TOKEN, { ingress: true, usedBy: 1 }), item("db-password")];

beforeEach(() => {
  sent = [];
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  detailRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request): Promise<Response> => {
      const url = new URL(req.url);
      sent.push({ method: req.method, path: url.pathname });
      switch (url.pathname) {
        case "/api/instance":
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        case "/api/me":
          return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
        case "/api/version":
          return Response.json({
            commit: "",
            display: "v0.1.0",
            sourceTreeUrl: "https://example.test/tree",
            sourceUrl: "https://example.test",
            version: "v0.1.0",
          });
        case "/api/environments":
          return Response.json({
            items: [
              {
                id: ENV,
                name: "Lab swarm",
                kind: "local",
                type: "swarm-manager",
                envType: "swarm",
                enabled: true,
                isSelfHost: true,
                createdAt: "2026-10-01T10:00:00Z",
                status: { state: "online" },
              },
            ],
            total: 1,
          });
        case `${BASE}/summary`:
          return Response.json({
            environment: {
              id: ENV,
              name: "Lab swarm",
              type: "swarm-manager",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 0,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `${BASE}/secrets`:
          return Response.json({
            items: secrets(),
            total: 2,
            unfilteredTotal: 2,
            capped: false,
          });
      }
      for (const s of secrets()) {
        if (url.pathname === `${BASE}/secrets/${s.id}`) return Response.json(detailOf(s));
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  detailRetry.attempts = 1;
  document.title = "";
});

const writes = () => sent.filter((s) => s.method !== "GET");

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("the secrets list", () => {
  it("badges the join token's secret and disables Rotate and Delete with the reason", async () => {
    renderAt(`/env/${ENV}/secrets`);
    const row = (await screen.findByRole("link", { name: TOKEN })).closest("tr") as HTMLElement;
    expect(within(row).getByText("TierPlan ingress")).toBeTruthy();
    expect(within(row).getByRole("link", { name: new RegExp(`Open.*${TOKEN}`) })).toBeTruthy();

    const del = within(row).getByRole("button", { name: `Delete secret ${TOKEN}` });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    expect(del.hasAttribute("disabled")).toBe(false);
    const rotate = within(row).getByRole("button", { name: `New version of ${TOKEN}` });
    expect(rotate.getAttribute("aria-disabled")).toBe("true");

    // The reason opens on keyboard focus, not only on hover; it replaces the in-use reason.
    fireEvent.focus(del);
    expect((await screen.findAllByText(REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    fireEvent.click(rotate);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("leaves another secret without the badge and with Rotate and Delete open", async () => {
    renderAt(`/env/${ENV}/secrets`);
    const row = (await screen.findByRole("link", { name: "db-password" })).closest(
      "tr",
    ) as HTMLElement;
    expect(within(row).queryByText("TierPlan ingress")).toBeNull();
    const del = within(row).getByRole("button", { name: "Delete secret db-password" });
    expect(del.getAttribute("aria-disabled")).toBeNull();
    const rotate = within(row).getByRole("button", { name: "New version of db-password" });
    expect(rotate.getAttribute("aria-disabled")).toBeNull();
  });
});

describe("the join token secret's page", () => {
  it("draws the badge and the neutral notice and disables Rotate and Delete with the reason", async () => {
    renderAt(`/env/${ENV}/secrets/sid-${TOKEN}`);
    const heading = await screen.findByRole("heading", { level: 2, name: TOKEN });
    expect(heading).toBeTruthy();
    expect(screen.getByText("TierPlan ingress")).toBeTruthy();

    const notice = screen.getByText("Managed by TierPlan ingress").closest("[role]") as HTMLElement;
    // Plain status: an object TierPlan manages is expected, not a fault.
    expect(notice.getAttribute("role")).toBe("status");
    expect(
      within(notice).getByText(
        "TierPlan creates and removes this itself. To remove it, switch ingress off.",
      ),
    ).toBeTruthy();

    const del = screen.getByRole("button", { name: `Delete secret ${TOKEN}` });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    const rotate = screen.getByRole("button", { name: `New version of ${TOKEN}` });
    expect(rotate.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(rotate);
    expect((await screen.findAllByText(REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    fireEvent.click(rotate);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("leaves another secret's page without the badge and the notice", async () => {
    renderAt(`/env/${ENV}/secrets/sid-db-password`);
    await screen.findByRole("heading", { level: 2, name: "db-password" });
    expect(screen.queryByText("TierPlan ingress")).toBeNull();
    expect(screen.queryByText("Managed by TierPlan ingress")).toBeNull();
    const del = screen.getByRole("button", { name: "Delete secret db-password" });
    expect(del.getAttribute("aria-disabled")).toBeNull();
  });
});
