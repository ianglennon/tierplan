// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { OwnedResourcesCard } from "@/features/access/owned-resources-card";
import "@/i18n";

// The Owned automations card (plan 03-22, D-14, D-22, UI-SPEC "Owned automations").

function owned(over: Record<string, unknown> = {}) {
  return {
    kind: "git.sync",
    label: "Git sync",
    resourceId: "r1",
    name: "Sync r1",
    envId: "env-a",
    envName: "Alpha",
    state: "active",
    needsEnvAdmin: false,
    ...over,
  };
}

let items: Array<Record<string, unknown>>;
let status: number;
const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  items = [];
  status = 200;
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/users/u1/owned") {
        return status === 200 ? Response.json(items) : Response.json({ title: "boom" }, { status });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  queryClient.setDefaultOptions(defaultQueryOptions);
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderCard() {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <OwnedResourcesCard userId="u1" username="alice" />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

describe("the Owned automations card", () => {
  it("reads each state as text, paused ones in the warning colour, with Transfer… on every row", async () => {
    items = [
      owned({ resourceId: "a", name: "Alpha sync", state: "active" }),
      owned({
        resourceId: "b",
        name: "Beta hook",
        kind: "webhook",
        label: "Webhook",
        state: "paused_lacks_permission",
      }),
      owned({ resourceId: "c", name: "Gamma sync", state: "paused_disabled" }),
      owned({ resourceId: "d", name: "Delta sync", state: "paused_deleted" }),
    ];
    renderCard();
    const card = await screen.findByRole("region", { name: "Owned automations" });

    const active = within(card).getByText("Active");
    expect(active.className).not.toContain("text-status-warning-text");
    for (const paused of [
      "Paused: owner lacks permission",
      "Paused: owner disabled",
      "Paused: owner deleted",
    ]) {
      // The state is words, and the colour only backs them up.
      expect(within(card).getByText(paused).className).toContain("text-status-warning-text");
    }
    for (const name of ["Alpha sync", "Beta hook", "Gamma sync", "Delta sync"]) {
      expect(within(card).getByRole("button", { name: `Transfer ${name}…` })).toBeTruthy();
    }
    expect(
      within(card)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Automation", "Type", "Environment", "State", "Actions"]);
    expect(within(card).getByText("Webhook")).toBeTruthy();
  });

  it("names an auto-deploy and a webhook after their stack and environment, with the same Transfer… control", async () => {
    items = [
      owned({
        kind: "git_autodeploy",
        label: "Git auto-deploy",
        resourceId: "s1",
        name: "web",
        envName: "Alpha",
      }),
      owned({
        kind: "git_webhook",
        label: "Git webhook",
        resourceId: "s2",
        name: "api",
        envName: "Beta",
        state: "paused_lacks_permission",
      }),
    ];
    renderCard();
    const card = await screen.findByRole("region", { name: "Owned automations" });
    expect(within(card).getByText("Auto-deploy of web on Alpha")).toBeTruthy();
    expect(within(card).getByText("Webhook of api on Beta")).toBeTruthy();
    expect(
      within(card).getByRole("button", { name: "Transfer Auto-deploy of web on Alpha…" }),
    ).toBeTruthy();
    expect(
      within(card).getByRole("button", { name: "Transfer Webhook of api on Beta…" }),
    ).toBeTruthy();
  });

  it("is not on the page while the user owns nothing, nor while it loads", async () => {
    const { container } = renderCard();
    expect(container.querySelector("section")).toBeNull();
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(screen.queryByRole("region", { name: "Owned automations" })).toBeNull();
    expect(container.textContent).toBe("");
  });

  it("says it could not load and reads the list again on Retry, rather than hiding what the user owns", async () => {
    status = 500;
    renderCard();
    expect(await screen.findByText("Couldn't load the automations this user owns.")).toBeTruthy();
    status = 200;
    items = [owned()];
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("button", { name: "Transfer Sync r1…" })).toBeTruthy();
  });

  it("cuts long names to the column with the whole name in a tooltip on focus, and scrolls inside 320px", async () => {
    const long = "A".repeat(120);
    items = [owned({ name: long, envName: "E".repeat(90) })];
    renderCard();
    const card = await screen.findByRole("region", { name: "Owned automations" });
    const name = within(card).getByText(long);
    expect(name.className).toContain("truncate");
    expect(name.getAttribute("tabindex")).toBe("0");
    fireEvent.focus(name);
    expect(await screen.findByRole("tooltip")).toBeTruthy();
    expect(within(await screen.findByRole("tooltip")).getAllByText(long).length).toBeGreaterThan(0);
    // 50 rows scroll inside the card, not the page.
    const wrapper = card.querySelector("table")?.parentElement as HTMLElement;
    expect(wrapper.className).toContain("max-h-80");
    expect(wrapper.className).toContain("overflow-y-auto");
    expect(wrapper.className).toContain("overflow-x-auto");
  });
});
