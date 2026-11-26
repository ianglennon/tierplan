// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DomainsTab } from "@/features/ingress/domains-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const BASE = `/api/env/${ENV}/ingress/domains/d1`;

type Json = Record<string, unknown>;
type Write = { method: string; path: string; body: string };

let items: Json[];
let writes: Write[];
let previews: string[];
let previewAnswer: () => Response;
let writeAnswer: (req: Write) => Promise<Response> | Response;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

function domain(over: Json = {}): Json {
  return {
    id: "d1",
    suffix: "apps.example.com",
    issuer: "production",
    dnsProvider: "cloudflare",
    tokenSet: true,
    tokenUpdatedAt: "2026-11-26T10:00:00Z",
    createdBy: "u1",
    createdAt: "2026-11-26T10:00:00Z",
    updatedAt: "2026-11-26T10:00:00Z",
    reserved: [],
    certificates: 4,
    ...over,
  };
}

function problem(status: number, type: string, detail: string): Response {
  return Response.json(
    { title: "Problem", status, type, detail },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

beforeEach(() => {
  setCsrfToken(null);
  items = [domain()];
  writes = [];
  previews = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  previewAnswer = () =>
    Response.json({
      issuer: "staging",
      certificates: 4,
      uploads: ["shop.apps.example.com"],
      confirmCount: 4,
    });
  writeAnswer = () => Response.json(domain());
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = url.pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json({ enabled: true });
      if (path === `/api/env/${ENV}/ingress/domains` && req.method === "GET") {
        return Response.json({ items });
      }
      if (path === `${BASE}/issuer` && req.method === "GET") {
        previews.push(url.search);
        return previewAnswer();
      }
      if (path.startsWith(BASE) && req.method !== "GET") {
        const write = { method: req.method, path, body: await req.clone().text() };
        writes.push(write);
        return writeAnswer(write);
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
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function openFromMenu(item: string, dialogName: string) {
  mount();
  const table = await screen.findByRole("table", { name: "Domains" });
  fireEvent.keyDown(within(table).getByRole("button", { name: "Actions for apps.example.com" }), {
    key: "Enter",
  });
  fireEvent.click(await screen.findByRole("menuitem", { name: item }));
  return screen.findByRole("dialog", { name: dialogName });
}

async function choose(dialog: HTMLElement, option: string) {
  fireEvent.keyDown(within(dialog).getByRole("combobox", { name: "Change to" }), {
    key: "Enter",
  });
  fireEvent.keyDown(await screen.findByRole("option", { name: option }), { key: "Enter" });
}

describe("the Change issuer dialog (D-07)", () => {
  const TITLE = "Change the issuer of apps.example.com";

  it("reads the preview once staging is chosen and shows what is ordered again and what is left alone", async () => {
    const dialog = await openFromMenu("Change issuer…", TITLE);
    // Nothing is read, and nothing can be sent, before a directory is chosen.
    expect(previews).toHaveLength(0);
    expect(
      within(dialog).getByRole("button", { name: "Change issuer" }).hasAttribute("disabled"),
    ).toBe(true);
    await choose(dialog, "Let's Encrypt staging");
    expect(await within(dialog).findByText("4 certificates will be ordered again.")).toBeTruthy();
    expect(previews).toEqual(["?issuer=staging"]);
    expect(
      within(dialog).getByText(
        "Certificates under apps.example.com that came from the other Let's Encrypt directory are retired and ordered again from Let's Encrypt staging.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByText("shop.apps.example.com").className).toContain("font-mono");
    expect(
      within(dialog).getByRole("button", { name: "Change issuer" }).hasAttribute("disabled"),
    ).toBe(false);
  });

  it("does not offer the directory the domain already uses", async () => {
    const dialog = await openFromMenu("Change issuer…", TITLE);
    fireEvent.keyDown(within(dialog).getByRole("combobox", { name: "Change to" }), {
      key: "Enter",
    });
    const current = await screen.findByRole("option", {
      name: "Let's Encrypt production (current)",
    });
    expect(current.getAttribute("aria-disabled")).toBe("true");
  });

  it("sends the issuer with the previewed count, closes, and reads the lists again", async () => {
    const dialog = await openFromMenu("Change issuer…", TITLE);
    await choose(dialog, "Let's Encrypt staging");
    await within(dialog).findByText("4 certificates will be ordered again.");
    items = [domain({ issuer: "staging" })];
    fireEvent.click(within(dialog).getByRole("button", { name: "Change issuer" }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ method: "PUT", path: `${BASE}/issuer` });
    expect(JSON.parse(writes[0]?.body ?? "{}")).toEqual({ issuer: "staging", confirmCount: 4 });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText(
      "apps.example.com now orders its certificates from Let's Encrypt staging.",
    );
  });

  it("shows the server's detail for a count mismatch, keeps the dialog open and reads the preview again", async () => {
    const detail =
      "The number of certificates this change affects is now 5, not 4. Review the preview again and confirm the new number.";
    writeAnswer = () => problem(409, "ingress_issuer_count_mismatch", detail);
    const dialog = await openFromMenu("Change issuer…", TITLE);
    await choose(dialog, "Let's Encrypt staging");
    await within(dialog).findByText("4 certificates will be ordered again.");
    previewAnswer = () =>
      Response.json({ issuer: "staging", certificates: 5, uploads: [], confirmCount: 5 });
    fireEvent.click(within(dialog).getByRole("button", { name: "Change issuer" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain(detail);
    expect(screen.getByRole("dialog", { name: TITLE })).toBeTruthy();
    expect(await within(dialog).findByText("5 certificates will be ordered again.")).toBeTruthy();
    expect(screen.queryByText(/Changed|now orders its certificates/)).toBeNull();
  });

  it("shows the server's refusal of the preview and sends nothing", async () => {
    previewAnswer = () => problem(404, "ingress_domain_not_found", "No such domain here.");
    const dialog = await openFromMenu("Change issuer…", TITLE);
    await choose(dialog, "Let's Encrypt staging");
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "No such domain here.",
    );
    expect(
      within(dialog).getByRole("button", { name: "Change issuer" }).hasAttribute("disabled"),
    ).toBe(true);
    expect(writes).toHaveLength(0);
  });

  it("sends nothing when the change is discarded", async () => {
    const dialog = await openFromMenu("Change issuer…", TITLE);
    await choose(dialog, "Let's Encrypt staging");
    await within(dialog).findByText("4 certificates will be ordered again.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard change" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toHaveLength(0);
  });
});
