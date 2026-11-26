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
const TOKEN = "cf-SECRET-TOKEN-0123456789";

type Json = Record<string, unknown>;

let items: Json[];
let posts: { path: string; body: string }[];
let answer: () => Promise<Response> | Response;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

function created(): Json {
  return {
    id: "d9",
    suffix: "apps.example.com",
    issuer: "production",
    dnsProvider: "cloudflare",
    tokenSet: true,
    tokenUpdatedAt: "2026-11-26T10:00:00Z",
    createdBy: "u1",
    createdAt: "2026-11-26T10:00:00Z",
    updatedAt: "2026-11-26T10:00:00Z",
    reserved: [],
    certificates: 0,
  };
}

beforeEach(() => {
  setCsrfToken(null);
  items = [];
  posts = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = () => Response.json(created(), { status: 201 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json({ enabled: true });
      if (path === `/api/env/${ENV}/ingress/domains` && req.method === "GET") {
        return Response.json({ items });
      }
      if (path === `/api/env/${ENV}/ingress/domains` && req.method === "POST") {
        posts.push({ path, body: await req.clone().text() });
        const res = await answer();
        if (res.ok) items = [created()];
        return res;
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

async function openDialog() {
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Add domain" }));
  return screen.findByRole("dialog", { name: "Add domain" });
}

function fill(dialog: HTMLElement, suffix: string, token: string) {
  fireEvent.change(within(dialog).getByLabelText("Domain"), { target: { value: suffix } });
  fireEvent.change(within(dialog).getByLabelText("DNS token"), { target: { value: token } });
}

describe("the Add domain dialog", () => {
  it("labels every field with its helper and keeps the primary unavailable until the domain and token hold a value", async () => {
    const dialog = await openDialog();
    expect(within(dialog).getByLabelText("Domain").className).toContain("font-mono");
    expect(
      within(dialog).getByText("A suffix such as apps.example.com. Hostnames must be below it."),
    ).toBeTruthy();
    expect(
      within(dialog).getByRole("combobox", { name: "Certificates from" }).textContent,
    ).toContain("Let's Encrypt production");
    const token = within(dialog).getByLabelText("DNS token") as HTMLInputElement;
    expect(token.type).toBe("password");
    expect(token.getAttribute("autocomplete")).toBe("off");
    expect(
      within(dialog).getByText(
        "TierPlan tests the token before it saves the domain. It is stored sealed and never shown again.",
      ),
    ).toBeTruthy();
    const submit = within(dialog).getByRole("button", { name: "Test and add domain" });
    expect(submit.hasAttribute("disabled")).toBe(true);
    fireEvent.change(within(dialog).getByLabelText("Domain"), {
      target: { value: "apps.example.com" },
    });
    expect(submit.hasAttribute("disabled")).toBe(true);
    fireEvent.change(token, { target: { value: TOKEN } });
    expect(submit.hasAttribute("disabled")).toBe(false);
  });

  it("posts the domain, the production issuer and the token once, with a Spinner in the button, then closes and shows the domain", async () => {
    let release: (r: Response) => void = () => {};
    answer = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    const dialog = await openDialog();
    fill(dialog, "apps.example.com", TOKEN);
    const submit = within(dialog).getByRole("button", { name: "Test and add domain" });
    fireEvent.click(submit);
    fireEvent.click(submit);
    await waitFor(() => expect(submit.hasAttribute("disabled")).toBe(true));
    expect(submit.querySelector("[role='status'], svg")).toBeTruthy();
    expect(posts).toHaveLength(1);
    expect(JSON.parse(posts[0]?.body ?? "{}")).toEqual({
      suffix: "apps.example.com",
      issuer: "production",
      token: TOKEN,
    });
    release(Response.json(created(), { status: 201 }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const table = await screen.findByRole("table", { name: "Domains" });
    expect(within(table).getByText("apps.example.com")).toBeTruthy();
  });

  it("posts the staging issuer when it is chosen and explains it", async () => {
    const dialog = await openDialog();
    const select = within(dialog).getByRole("combobox", { name: "Certificates from" });
    fireEvent.keyDown(select, { key: "Enter" });
    fireEvent.keyDown(await screen.findByRole("option", { name: "Let's Encrypt staging" }), {
      key: "Enter",
    });
    expect(
      await within(dialog).findByText(
        "Staging certificates aren't trusted by browsers and TierPlan never serves them.",
      ),
    ).toBeTruthy();
    fill(dialog, "apps.example.com", TOKEN);
    fireEvent.click(within(dialog).getByRole("button", { name: "Test and add domain" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(JSON.parse(posts[0]?.body ?? "{}").issuer).toBe("staging");
  });

  it("shows the server's reason for a failed token test in a destructive alert and keeps the dialog open", async () => {
    answer = () =>
      Response.json(
        {
          title: "Unprocessable Entity",
          status: 422,
          type: "ingress_token_invalid",
          detail: "The DNS provider refused the token for apps.example.com.",
        },
        { status: 422, headers: { "Content-Type": "application/problem+json" } },
      );
    const dialog = await openDialog();
    fill(dialog, "apps.example.com", TOKEN);
    fireEvent.click(within(dialog).getByRole("button", { name: "Test and add domain" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("The DNS provider refused the token for apps.example.com.");
    expect(alert.getAttribute("data-variant") ?? alert.className).toMatch(/destructive/);
    expect(screen.getByRole("dialog", { name: "Add domain" })).toBeTruthy();
    // Nothing was saved: the list is still empty behind the dialog.
    expect(items).toHaveLength(0);
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it("clears the token when the dialog closes and never echoes it, toasts it or puts it in a live region", async () => {
    const dialog = await openDialog();
    fill(dialog, "apps.example.com", TOKEN);
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard domain" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posts).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Add domain" }));
    const again = await screen.findByRole("dialog", { name: "Add domain" });
    expect((within(again).getByLabelText("DNS token") as HTMLInputElement).value).toBe("");
    expect((within(again).getByLabelText("Domain") as HTMLInputElement).value).toBe("");

    fill(again, "apps.example.com", TOKEN);
    fireEvent.click(within(again).getByRole("button", { name: "Test and add domain" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText("Added apps.example.com.");
    for (const live of document.querySelectorAll("[aria-live], [role='status'], [role='alert']")) {
      expect(live.textContent).not.toContain(TOKEN);
    }
    expect(document.body.textContent).not.toContain(TOKEN);
    // The token is not in any field either: the one that held it is gone with the dialog.
    for (const input of document.querySelectorAll("input")) expect(input.value).not.toBe(TOKEN);
  });

  it("is not offered to a reader, and the toolbar button is disabled with the reason once a domain exists", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    items = [created()];
    mount();
    const button = await screen.findByRole("button", { name: "Add domain" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
