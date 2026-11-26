// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DomainsTab } from "@/features/ingress/domains-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let domains: () => Response;
let status: Json;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;
// What the server is asked besides the reads: the method, the path and the body of each write.
let writes: { method: string; path: string; body: string }[];
// The answer to a write; the default is an empty success.
let answer: (method: string, path: string) => Response;

function domain(over: Json = {}): Json {
  return {
    id: "d1",
    suffix: "apps.example.com",
    issuer: "production",
    dnsProvider: "cloudflare",
    tokenSet: true,
    tokenUpdatedAt: "2026-11-20T10:00:00Z",
    createdBy: "u1",
    createdAt: "2026-11-01T10:00:00Z",
    updatedAt: "2026-11-20T10:00:00Z",
    reserved: [],
    certificates: 3,
    ...over,
  };
}

function serve(items: Json[]) {
  domains = () => Response.json({ items });
}

beforeEach(() => {
  setCsrfToken(null);
  serve([]);
  writes = [];
  answer = () => new Response(null, { status: 204 });
  status = { enabled: true };
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/domains`) return domains();
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json(status);
      if (req.method !== "GET") {
        writes.push({ method: req.method, path, body: await req.clone().text() });
        return answer(req.method, path);
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
          <DomainsTab envId={ENV} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function table() {
  return screen.findByRole("table", { name: "Domains" });
}

const rowOf = (t: HTMLElement, suffix: string) =>
  within(t).getByText(suffix).closest("tr") as HTMLElement;

describe("the Ingress page's Domains tab", () => {
  it("draws each domain with its issuer, DNS provider, token state and certificate count", async () => {
    serve([
      domain(),
      domain({
        id: "d2",
        suffix: "stage.example.org",
        issuer: "staging",
        tokenSet: false,
        tokenUpdatedAt: undefined,
        certificates: 0,
      }),
    ]);
    const { container } = mount();
    const t = await table();
    expect(
      within(t)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Domain", "Issuer", "DNS provider", "Token", "Certificates", "Actions"]);
    const prod = rowOf(t, "apps.example.com");
    expect(within(prod).getByText("apps.example.com").className).toContain("font-mono");
    expect(within(prod).getByText("Production")).toBeTruthy();
    expect(within(prod).getByText("Cloudflare")).toBeTruthy();
    expect(within(prod).getByText("Saved")).toBeTruthy();
    expect(within(prod).getByText("3")).toBeTruthy();
    const stage = rowOf(t, "stage.example.org");
    const badge = within(stage).getByText("Staging", { selector: "[data-slot='badge']" });
    expect(badge).toBeTruthy();
    expect(within(stage).getByText("No token saved")).toBeTruthy();
    expect(within(stage).getByText("0")).toBeTruthy();
    // The list never carries a token, and the page never draws one.
    expect(container.innerHTML).not.toMatch(/token":|secret|dop_v1|cf-token/i);
  });

  it("works while ingress is off: one line above the table and no ingress-off empty state", async () => {
    status = { enabled: false };
    serve([domain()]);
    mount();
    const t = await table();
    expect(await screen.findByText("Domains can be registered before ingress is on.")).toBeTruthy();
    expect(within(t).getByText("apps.example.com")).toBeTruthy();
    expect(screen.queryByText("Ingress is off")).toBeNull();
  });

  it("says nothing about ingress being off while it is on", async () => {
    serve([domain()]);
    mount();
    await table();
    await waitFor(() =>
      expect(screen.queryByText("Domains can be registered before ingress is on.")).toBeNull(),
    );
  });

  it("is empty with its words, and with no Add domain button for a reader", async () => {
    mount();
    expect(await screen.findByText("No domains yet")).toBeTruthy();
    expect(
      screen.getByText(
        "Register a domain to let stacks in this environment serve hostnames under it. TierPlan needs a DNS token to prove it owns the name.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add domain" })).toBeNull();
  });

  it("is empty and not ingress-off when ingress is off", async () => {
    status = { enabled: false };
    mount();
    expect(await screen.findByText("No domains yet")).toBeTruthy();
    expect(screen.queryByText("Ingress is off")).toBeNull();
  });

  it("shows a Skeleton while the first read is out", async () => {
    let release: (r: Response) => void = () => {};
    const pending = new Promise<Response>((resolve) => {
      release = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/me") return Response.json(me);
        if (path === `/api/env/${ENV}/ingress/domains`) return pending;
        if (path === `/api/env/${ENV}/ingress/status`) return Response.json(status);
        return Response.json({}, { status: 404 });
      }),
    );
    const { container } = mount();
    expect(await screen.findByText("Loading domains")).toBeTruthy();
    expect(container.querySelector("[data-slot='skeleton']")?.className).toContain("h-32");
    release(Response.json({ items: [] }));
    expect(await screen.findByText("No domains yet")).toBeTruthy();
  });

  it("says it could not read the domains and reads again on Retry", async () => {
    domains = () => Response.json({ title: "no" }, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't read the domains.")).toBeTruthy();
    serve([domain()]);
    fireEvent.click(screen.getByRole("button", { name: "Retry reading domains" }));
    const t = await table();
    expect(within(t).getByText("apps.example.com")).toBeTruthy();
  });

  it("opens the reserved names of a domain behind a disclosure", async () => {
    serve([domain({ reserved: ["held.apps.example.com"] })]);
    mount();
    const t = await table();
    const toggle = within(t).getByRole("button", {
      name: "Show reserved names for apps.example.com",
    });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(within(t).queryByText("held.apps.example.com")).toBeNull();
    fireEvent.click(toggle);
    await waitFor(() => expect(toggle.getAttribute("aria-expanded")).toBe("true"));
    expect(within(t).getByText("Reserved names")).toBeTruthy();
    expect(within(t).getByText("held.apps.example.com").className).toContain("font-mono");
  });

  it("draws the row menu items disabled for a reader, with the reason", async () => {
    serve([domain()]);
    mount();
    const t = await table();
    fireEvent.keyDown(within(t).getByRole("button", { name: "Actions for apps.example.com" }), {
      key: "Enter",
    });
    const items = await screen.findAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "Replace token…",
      "Change issuer…",
      "Delete domain…",
    ]);
    for (const item of items) expect(item.getAttribute("aria-disabled")).toBe("true");
  });
});

describe("the reserved names of a domain", () => {
  const BASE = `/api/env/${ENV}/ingress/domains/d1/reserved`;

  async function expand(suffix = "apps.example.com") {
    mount();
    const t = await table();
    fireEvent.click(within(t).getByRole("button", { name: `Show reserved names for ${suffix}` }));
    await within(t).findByText("Reserved names");
    return t;
  }

  it("says nothing is reserved for a domain with no names", async () => {
    serve([domain()]);
    const t = await expand();
    expect(within(t).getByText("Nothing is reserved.")).toBeTruthy();
  });

  it("releases a name with no confirmation and reads the list again", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    serve([domain({ reserved: ["held.apps.example.com"] })]);
    const t = await expand();
    serve([domain({ reserved: [] })]);
    fireEvent.click(within(t).getByRole("button", { name: "Release held.apps.example.com" }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      method: "DELETE",
      path: `${BASE}/held.apps.example.com`,
    });
    expect(await within(t).findByText("Nothing is reserved.")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("reserves the name an Admin types and reads the list again", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    serve([domain()]);
    const t = await expand();
    const field = within(t).getByLabelText("Reserve a name");
    expect(
      within(t).getByText("A hostname under apps.example.com. No stack can claim it."),
    ).toBeTruthy();
    expect(field.className).toContain("font-mono");
    expect(
      within(t).getByRole("button", { name: "Reserve hostname" }).getAttribute("disabled"),
    ).not.toBeNull();
    fireEvent.change(field, { target: { value: "new.apps.example.com" } });
    serve([domain({ reserved: ["new.apps.example.com"] })]);
    fireEvent.click(within(t).getByRole("button", { name: "Reserve hostname" }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ method: "POST", path: BASE });
    expect(JSON.parse(writes[0]?.body ?? "{}")).toEqual({ hostname: "new.apps.example.com" });
    expect(await within(t).findByText("new.apps.example.com")).toBeTruthy();
    expect((within(t).getByLabelText("Reserve a name") as HTMLInputElement).value).toBe("");
  });

  it("shows the server's detail under the field when it refuses a name", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    serve([domain()]);
    answer = () =>
      Response.json(
        {
          title: "Conflict",
          status: 409,
          detail: "The name is claimed by the stack shop.",
        },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    const t = await expand();
    fireEvent.change(within(t).getByLabelText("Reserve a name"), {
      target: { value: "shop.apps.example.com" },
    });
    fireEvent.click(within(t).getByRole("button", { name: "Reserve hostname" }));
    const detail = await within(t).findByText("The name is claimed by the stack shop.");
    expect(detail.className).toContain("text-destructive");
    expect(detail.className).toContain("text-xs");
  });

  it("draws Reserve and Release disabled with the reason for a Viewer", async () => {
    serve([domain({ reserved: ["held.apps.example.com"] })]);
    const t = await expand();
    const release = within(t).getByRole("button", { name: "Release held.apps.example.com" });
    expect(release.getAttribute("aria-disabled")).toBe("true");
    const reserve = within(t).getByRole("button", { name: "Reserve hostname" });
    expect(reserve.getAttribute("aria-disabled")).toBe("true");
    expect(within(t).getByLabelText("Reserve a name").getAttribute("disabled")).not.toBeNull();
    fireEvent.click(release);
    expect(writes).toHaveLength(0);
  });
});
