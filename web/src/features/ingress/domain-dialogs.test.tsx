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

describe("the Replace token dialog", () => {
  const TITLE = "Replace the DNS token for apps.example.com";
  const TOKEN = "cf-NEW-SECRET-TOKEN-0123456789";

  it("keeps the primary unavailable while the field is empty, and the field is a password field", async () => {
    const dialog = await openFromMenu("Replace token…", TITLE);
    const field = within(dialog).getByLabelText("New DNS token") as HTMLInputElement;
    expect(field.type).toBe("password");
    expect(field.getAttribute("autocomplete")).toBe("off");
    const submit = within(dialog).getByRole("button", { name: "Test and replace token" });
    expect(submit.hasAttribute("disabled")).toBe(true);
    fireEvent.change(field, { target: { value: TOKEN } });
    expect(submit.hasAttribute("disabled")).toBe(false);
  });

  it("sends the token once, with a Spinner in the button, then closes with a toast that does not hold it", async () => {
    let release: (r: Response) => void = () => {};
    writeAnswer = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    const dialog = await openFromMenu("Replace token…", TITLE);
    fireEvent.change(within(dialog).getByLabelText("New DNS token"), { target: { value: TOKEN } });
    const submit = within(dialog).getByRole("button", { name: "Test and replace token" });
    fireEvent.click(submit);
    fireEvent.click(submit);
    await waitFor(() => expect(submit.hasAttribute("disabled")).toBe(true));
    expect(submit.querySelector("[role='status'], svg")).toBeTruthy();
    expect(writes).toHaveLength(1);
    expect(writes[0]).toMatchObject({ method: "PUT", path: `${BASE}/token` });
    expect(JSON.parse(writes[0]?.body ?? "{}")).toEqual({ token: TOKEN });
    release(Response.json(domain()));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText("Replaced the DNS token for apps.example.com.");
    for (const live of document.querySelectorAll("[aria-live], [role='status'], [role='alert']")) {
      expect(live.textContent).not.toContain(TOKEN);
    }
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it("shows the reason a token failed its test in a destructive alert and keeps the dialog open", async () => {
    writeAnswer = () =>
      problem(
        422,
        "ingress_token_invalid",
        "The DNS provider refused the token for apps.example.com.",
      );
    const dialog = await openFromMenu("Replace token…", TITLE);
    fireEvent.change(within(dialog).getByLabelText("New DNS token"), { target: { value: TOKEN } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Test and replace token" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("The DNS provider refused the token for apps.example.com.");
    expect(alert.getAttribute("data-variant") ?? alert.className).toMatch(/destructive/);
    expect(screen.getByRole("dialog", { name: TITLE })).toBeTruthy();
    for (const live of document.querySelectorAll("[aria-live], [role='status'], [role='alert']")) {
      expect(live.textContent).not.toContain(TOKEN);
    }
  });

  it("clears the field when the dialog closes and sends nothing on Discard token", async () => {
    const dialog = await openFromMenu("Replace token…", TITLE);
    fireEvent.change(within(dialog).getByLabelText("New DNS token"), { target: { value: TOKEN } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard token" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toHaveLength(0);
    const table = await screen.findByRole("table", { name: "Domains" });
    fireEvent.keyDown(within(table).getByRole("button", { name: "Actions for apps.example.com" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Replace token…" }));
    const again = await screen.findByRole("dialog", { name: TITLE });
    expect((within(again).getByLabelText("New DNS token") as HTMLInputElement).value).toBe("");
    for (const input of document.querySelectorAll("input")) expect(input.value).not.toBe(TOKEN);
  });
});

describe("the Delete domain dialog (U-09)", () => {
  const TITLE = "Delete apps.example.com?";

  async function open() {
    return openFromMenu("Delete domain…", TITLE);
  }

  it("says what happens to the certificates and keeps the destructive confirm disabled until the suffix is typed", async () => {
    const dialog = await open();
    expect(
      within(dialog).getByText(
        "Its 4 certificates stop renewing and expire. Hostnames under it can't be claimed any more.",
      ),
    ).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Delete domain" });
    expect(confirm.hasAttribute("disabled")).toBe(true);
    expect(confirm.getAttribute("data-variant") ?? confirm.className).toMatch(/destructive/);
    const field = within(dialog).getByLabelText("Type apps.example.com to confirm");
    fireEvent.change(field, { target: { value: "apps.example.co" } });
    expect(confirm.hasAttribute("disabled")).toBe(true);
    fireEvent.change(field, { target: { value: "apps.example.com" } });
    expect(confirm.hasAttribute("disabled")).toBe(false);
  });

  it("sends the delete once, closes, and reads the lists again", async () => {
    writeAnswer = () => Response.json({ certificatesLeft: 4 });
    const dialog = await open();
    fireEvent.change(within(dialog).getByLabelText("Type apps.example.com to confirm"), {
      target: { value: "apps.example.com" },
    });
    const confirm = within(dialog).getByRole("button", { name: "Delete domain" });
    items = [];
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ method: "DELETE", path: BASE });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText("Deleted apps.example.com.");
    expect(await screen.findByText("No domains yet")).toBeTruthy();
  });

  it("also closes when the server answers 204 because no certificates were left", async () => {
    writeAnswer = () => new Response(null, { status: 204 });
    const dialog = await open();
    fireEvent.change(within(dialog).getByLabelText("Type apps.example.com to confirm"), {
      target: { value: "apps.example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete domain" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toHaveLength(1);
  });

  it("shows the server's list of stacks under Stacks still use this domain and keeps the domain", async () => {
    const detail =
      "“apps.example.com” cannot be deleted while hostnames under it are claimed by: shop in Production. Remove those hostnames from the stacks first.";
    writeAnswer = () => problem(409, "ingress_domain_in_use", detail);
    const dialog = await open();
    fireEvent.change(within(dialog).getByLabelText("Type apps.example.com to confirm"), {
      target: { value: "apps.example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete domain" }));
    const alert = await within(dialog).findByRole("alert");
    expect(within(alert).getByText("Stacks still use this domain")).toBeTruthy();
    expect(alert.textContent).toContain(detail);
    expect(alert.getAttribute("data-variant") ?? alert.className).toMatch(/destructive/);
    expect(screen.getByRole("dialog", { name: TITLE })).toBeTruthy();
    expect(writes).toHaveLength(1);
  });

  it("shows any other refusal as the server worded it, without the stacks title", async () => {
    writeAnswer = () =>
      problem(409, "ingress_domain_cleanup_pending", "2 DNS records are still being removed.");
    const dialog = await open();
    fireEvent.change(within(dialog).getByLabelText("Type apps.example.com to confirm"), {
      target: { value: "apps.example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete domain" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("2 DNS records are still being removed.");
    expect(within(alert).queryByText("Stacks still use this domain")).toBeNull();
  });

  it("sends nothing on Keep domain", async () => {
    const dialog = await open();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep domain" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toHaveLength(0);
  });
});

describe("the row menu without ingress.manage", () => {
  it("draws the three items disabled for a Viewer and opens no dialog", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const table = await screen.findByRole("table", { name: "Domains" });
    fireEvent.keyDown(within(table).getByRole("button", { name: "Actions for apps.example.com" }), {
      key: "Enter",
    });
    const menu = await screen.findAllByRole("menuitem");
    expect(menu).toHaveLength(3);
    for (const item of menu) {
      expect(item.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(item);
    }
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(writes).toHaveLength(0);
  });

  it("draws them enabled for an Admin", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Domains" });
    fireEvent.keyDown(within(table).getByRole("button", { name: "Actions for apps.example.com" }), {
      key: "Enter",
    });
    for (const item of await screen.findAllByRole("menuitem")) {
      expect(item.getAttribute("aria-disabled")).toBeNull();
    }
  });
});
