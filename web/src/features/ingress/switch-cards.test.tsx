// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { SettingsTab } from "@/features/ingress/settings-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// The access log switch and the join token card of the Settings tab (D-05, U-10, U-20, UI-SPEC S6 cards 3 and 4):
// the switch commits at once and puts itself back on a refusal; the token is shown once, in the dialog that made it,
// and is nowhere in the page once that dialog has closed.

const ENV = "e1";
const BASE = `/api/env/${ENV}/ingress`;
const TOKEN = "ingress-secret-token-0123456789abcdef";

type Json = Record<string, unknown>;
type Write = { method: string; path: string; body: Json };

let accessLogs: boolean;
let envType: "swarm" | "standalone";
let tokenStatus: Json;
let writes: Write[];
let accessAnswer: () => Promise<Response> | Response;
let issueAnswer: () => Response;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

function problem(status: number, type: string, detail: string): Response {
  return Response.json(
    { title: "Problem", status, type, detail },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
  });
  setCsrfToken(null);
  accessLogs = false;
  envType = "standalone";
  tokenStatus = {
    exists: true,
    generation: 2,
    createdAt: "2026-11-24T10:00:00Z",
    createdBy: "alice",
    disabled: false,
  };
  writes = [];
  accessAnswer = () => {
    accessLogs = writes.at(-1)?.body.enabled === true;
    return Response.json({ accessLogs, applies: "rolling" });
  };
  issueAnswer = () => Response.json({ token: TOKEN, generation: 3, rotated: true });
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType }] });
      }
      if (path === `${BASE}/status`) {
        return Response.json({
          enabled: true,
          enableFailed: false,
          disablingIncomplete: false,
          paused: false,
          pendingWipes: 0,
          redeploy: [],
          accessLogs,
          nodes: { expected: 1, enrolled: 1, ready: 1 },
        });
      }
      if (path === `${BASE}/vip`) {
        return problem(404, "vip_not_enabled", "No virtual IP.");
      }
      if (req.method !== "GET") {
        const body = (await req.json().catch(() => ({}))) as Json;
        writes.push({ method: req.method, path, body });
      }
      if (path === `${BASE}/access-logs`) return accessAnswer();
      if (path === `${BASE}/join-token`) {
        if (req.method === "GET") return Response.json(tokenStatus);
        if (req.method === "POST") return issueAnswer();
        if (req.method === "DELETE") return new Response(null, { status: 204 });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <SettingsTab envId={ENV} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function accessSwitch(): Promise<HTMLElement> {
  mount();
  return waitUnderFakeClock(() =>
    screen.getByRole("switch", { name: "Write the proxy's access log" }),
  );
}

async function button(name: string): Promise<HTMLButtonElement> {
  return waitUnderFakeClock(() => screen.getByRole("button", { name }) as HTMLButtonElement);
}

describe("the access log switch", () => {
  it("commits at once, is disabled with a spinner in flight, and toasts when it is stored", async () => {
    let release: (answer: Response) => void = () => {};
    accessAnswer = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    const toggle = await accessSwitch();
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(
      screen.getByText(
        "Records each request, including the client's address and the path asked for. Read it in the proxy's logs.",
      ),
    ).toBeTruthy();
    // No preview and no other request first: the one PUT is the whole switch.
    fireEvent.click(toggle);
    await waitUnderFakeClock(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toEqual({
      method: "PUT",
      path: `${BASE}/access-logs`,
      body: { enabled: true },
    });
    await waitUnderFakeClock(() => expect((toggle as HTMLButtonElement).disabled).toBe(true));
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    const card = toggle.closest("section") as HTMLElement;
    expect(card.querySelector('[data-slot="spinner"]')).toBeTruthy();
    // A second press while it is in flight sends nothing more.
    fireEvent.click(toggle);
    expect(writes).toHaveLength(1);

    await act(async () => {
      accessLogs = true;
      release(Response.json({ accessLogs: true, applies: "rolling" }));
    });
    await waitUnderFakeClock(() => expect(screen.getByText("Access log on.")).toBeTruthy());
    expect((toggle as HTMLButtonElement).disabled).toBe(false);
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(card.querySelector('[data-slot="spinner"]')).toBeNull();
  });

  it("toasts Access log off. when it is switched off", async () => {
    accessLogs = true;
    const toggle = await accessSwitch();
    await waitUnderFakeClock(() => expect(toggle.getAttribute("aria-checked")).toBe("true"));
    fireEvent.click(toggle);
    await waitUnderFakeClock(() => expect(screen.getByText("Access log off.")).toBeTruthy());
    expect(writes[0]?.body).toEqual({ enabled: false });
  });

  it("puts the switch back and shows the server's detail in a toast when the write is refused", async () => {
    accessAnswer = () =>
      problem(
        409,
        "operation_locked",
        "The environment is busy: ingress is being turned on by alice.",
      );
    const toggle = await accessSwitch();
    fireEvent.click(toggle);
    await waitUnderFakeClock(() =>
      expect(
        screen.getByText("The environment is busy: ingress is being turned on by alice."),
      ).toBeTruthy(),
    );
    await waitUnderFakeClock(() => expect((toggle as HTMLButtonElement).disabled).toBe(false));
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByText("Access log on.")).toBeNull();
  });

  it("is disabled for a Viewer with the permission reason, and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    const toggle = await accessSwitch();
    expect(toggle.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs Admin", { selector: "span.sr-only" }).length).toBeGreaterThan(
      0,
    );
    fireEvent.click(toggle);
    expect(writes).toEqual([]);
    expect(toggle.getAttribute("aria-checked")).toBe("false");
  });

  it("links to the proxy's logs for someone who may read them", async () => {
    await accessSwitch();
    const link = await waitUnderFakeClock(() =>
      screen.getByRole("link", { name: "View proxy logs" }),
    );
    expect(link.getAttribute("href")).toBe(`/env/${ENV}/containers/tierplan-ingress-proxy-1/logs`);
  });
});

describe("the join token card", () => {
  it("says when the token was made and, for an Admin, by whom", async () => {
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText(/Active, made/)).toBeTruthy());
    const line = screen.getByText(/Active, made/).closest("span") as HTMLElement;
    expect(line.textContent).toContain("by alice");
    expect(document.getElementById("join-token")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Rotate token" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Disable token" })).toBeTruthy();
    // The status read carries no token and the page draws none.
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it("draws no creator for a Viewer, whom the server does not tell, and disables the buttons with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    tokenStatus = { ...tokenStatus, createdBy: "" };
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText(/Active, made/)).toBeTruthy());
    const line = screen.getByText(/Active, made/).closest("span") as HTMLElement;
    expect(line.textContent).not.toContain(" by ");
    for (const name of ["Rotate token", "Disable token"]) {
      expect((await button(name)).getAttribute("aria-disabled")).toBe("true");
    }
    fireEvent.click(await button("Rotate token"));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(writes).toEqual([]);
  });

  it("rotates after the confirm, shows the token once, and holds no copy of it once the dialog closes", async () => {
    mount();
    fireEvent.click(await button("Rotate token"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("alertdialog", { name: "Rotate the join token?" }),
    );
    expect(
      within(dialog).getByText(
        "Nodes that are enrolled keep working. A node that enrols later needs the new token. The old one stops working at once.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Keep current token" })).toBeTruthy();
    // Nothing is written before the confirm.
    expect(writes).toEqual([]);

    fireEvent.click(within(dialog).getByRole("button", { name: "Rotate token" }));
    await waitUnderFakeClock(() => expect(within(dialog).getByText(TOKEN)).toBeTruthy());
    expect(writes).toEqual([{ method: "POST", path: `${BASE}/join-token`, body: {} }]);
    expect(within(dialog).getByText("Copy it now. It isn't shown again.")).toBeTruthy();
    // Shown in the code block, and never in a toast or a live region of its own.
    expect(within(dialog).getByText(TOKEN).closest("code")).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitUnderFakeClock(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(document.body.textContent).not.toContain(TOKEN);
    // Opening it again shows the question, not the token.
    fireEvent.click(await button("Rotate token"));
    const again = await waitUnderFakeClock(() =>
      screen.getByRole("alertdialog", { name: "Rotate the join token?" }),
    );
    expect(again.textContent).not.toContain(TOKEN);
  });

  it("sends nothing when Keep current token is pressed", async () => {
    mount();
    fireEvent.click(await button("Rotate token"));
    const dialog = await waitUnderFakeClock(() => screen.getByRole("alertdialog"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep current token" }));
    await waitUnderFakeClock(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(writes).toEqual([]);
  });

  it("disables the token after its confirm, and not before", async () => {
    mount();
    fireEvent.click(await button("Disable token"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("alertdialog", { name: "Disable the join token?" }),
    );
    expect(
      within(dialog).getByText(
        "No further node can enrol until you make a new one. Enrolled nodes keep working.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Keep token active" })).toBeTruthy();
    expect(writes).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Disable token" }));
    await waitUnderFakeClock(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(writes).toEqual([{ method: "DELETE", path: `${BASE}/join-token`, body: {} }]);
  });

  it("shows Disabled and Make a new token when the token is disabled, and shows the new token once", async () => {
    tokenStatus = { ...tokenStatus, disabled: true };
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Disabled")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Rotate token" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Disable token" })).toBeNull();
    fireEvent.click(await button("Make a new token"));
    const dialog = await waitUnderFakeClock(() => screen.getByRole("alertdialog"));
    expect(within(dialog).getByRole("button", { name: "Keep token disabled" })).toBeTruthy();
    issueAnswer = () => Response.json({ token: TOKEN, generation: 3, rotated: false });
    fireEvent.click(within(dialog).getByRole("button", { name: "Make a new token" }));
    await waitUnderFakeClock(() => expect(within(dialog).getByText(TOKEN)).toBeTruthy());
    expect(within(dialog).getByText("Copy it now. It isn't shown again.")).toBeTruthy();
  });

  it("offers Make a new token and says so when no token was ever made", async () => {
    tokenStatus = { exists: false, generation: 0, createdBy: "", disabled: false };
    mount();
    await waitUnderFakeClock(() =>
      expect(screen.getByText("No token has been made yet")).toBeTruthy(),
    );
    expect(await button("Make a new token")).toBeTruthy();
  });

  it("shows the server's sentence in the dialog when the proxy has not enrolled, and no token", async () => {
    issueAnswer = () =>
      problem(
        409,
        "ingress_token_not_enrolled",
        "The proxy hasn't enrolled with the token it has yet, so no new token was made.",
      );
    mount();
    fireEvent.click(await button("Rotate token"));
    const dialog = await waitUnderFakeClock(() => screen.getByRole("alertdialog"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Rotate token" }));
    await waitUnderFakeClock(() =>
      expect(
        within(dialog).getByText(
          "The proxy hasn't enrolled with the token it has yet, so no new token was made.",
        ),
      ).toBeTruthy(),
    );
    expect(dialog.textContent).not.toContain("Copy it now");
    expect(document.querySelector("[data-sonner-toast]")).toBeNull();
    // It can be tried again or dismissed.
    expect(
      (within(dialog).getByRole("button", { name: "Rotate token" }) as HTMLButtonElement).disabled,
    ).toBe(false);
  });
});

describe("the Settings tab", () => {
  it("stacks Ingress, Virtual IP, Proxy access log and Join token in that order on a swarm", async () => {
    envType = "swarm";
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Join token")).toBeTruthy());
    await waitUnderFakeClock(() => expect(screen.getByText("Virtual IP")).toBeTruthy());
    const titles = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);
    expect(titles).toEqual(["Ingress", "Virtual IP", "Proxy access log", "Join token"]);
  });

  it("leaves the Virtual IP card out on a standalone environment", async () => {
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Join token")).toBeTruthy());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100);
    });
    const titles = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);
    expect(titles).toEqual(["Ingress", "Proxy access log", "Join token"]);
  });
});
