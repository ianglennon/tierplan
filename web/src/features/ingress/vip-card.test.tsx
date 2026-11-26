// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { VIPCard } from "@/features/ingress/vip-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// The Virtual IP card of the Settings tab (D-05, D-07, U-17, UI-SPEC S6 card 2): the form in the review dialog, the
// server's preview with the fixed privileged-services warning first, the job, and the refusals drawn as the server's
// sentence above the form.

const ENV = "e1";
const BASE = `/api/env/${ENV}/ingress`;

// The sentence the server sends with every preview (internal/ingressdeploy/vip_text.go).
const WARNING =
  "The virtual IP needs `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` on every manager's agent. " +
  "That setting lets TierPlan run privileged, host-network services on every node of this swarm, so a compromised TierPlan could take over every node. " +
  "VRRP authentication only keeps honest neighbours apart; anything on this network segment can still contend for the address.";

type Json = Record<string, unknown>;
type Write = { method: string; path: string; body: Json };

let envType: "swarm" | "standalone";
let ingressOn: boolean;
let vip: Json | null;
let writes: Write[];
let previewAnswer: (body: Json) => Response;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

function problem(status: number, type: string, detail: string): Response {
  return Response.json(
    { title: "Problem", status, type, detail },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

function previewBody(over: Json = {}): Json {
  return {
    address: "192.168.1.50/24",
    change: false,
    derivedRouterId: 51,
    findings: [],
    items: ["Make the key"],
    managers: [{ nodeId: "sw-a", hostname: "node-a", state: "allows", localSocket: false }],
    routerDerived: true,
    routerId: 51,
    warning: WARNING,
    ...over,
  };
}

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
  });
  setCsrfToken(null);
  envType = "swarm";
  ingressOn = true;
  vip = null;
  writes = [];
  previewAnswer = () => Response.json(previewBody());
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
      if (path === `${BASE}/status`) return Response.json({ enabled: ingressOn });
      if (path === `${BASE}/vip` && req.method === "GET") {
        return vip ? Response.json(vip) : problem(404, "vip_not_enabled", "No virtual IP.");
      }
      const body = req.method === "GET" ? {} : ((await req.json().catch(() => ({}))) as Json);
      if (path === `${BASE}/vip/preview`) {
        writes.push({ method: req.method, path, body });
        return previewAnswer(body);
      }
      if (path === `${BASE}/vip` || path === `${BASE}/vip/disable/preview`) {
        writes.push({ method: req.method, path, body });
        if (path.endsWith("/preview")) {
          return Response.json({
            address: "192.168.10.250/24",
            items: [],
            secrets: [],
            service: "tierplan-ingress-vip",
            tasks: 2,
          });
        }
        return Response.json({ jobId: "j1" }, { status: 202 });
      }
      if (path === `${BASE}/jobs/j1`) {
        return Response.json({
          id: "j1",
          kind: "ingress_vip_enable",
          state: "done",
          items: [],
          output: [],
          createdAt: "2026-11-26T10:00:00Z",
          finishedAt: "2026-11-26T10:00:05Z",
          startedBy: "u1",
        });
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
          <VIPCard envId={ENV} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function button(name: string): Promise<HTMLButtonElement> {
  return waitUnderFakeClock(() => screen.getByRole("button", { name }) as HTMLButtonElement);
}

async function openForm(): Promise<HTMLElement> {
  mount();
  fireEvent.click(await button("Turn on virtual IP"));
  return waitUnderFakeClock(() => screen.getByRole("dialog", { name: "Turn on virtual IP?" }));
}

function typeAddress(dialog: HTMLElement, text: string) {
  fireEvent.change(within(dialog).getByLabelText("Address"), { target: { value: text } });
}

describe("turning the virtual IP on", () => {
  it("explains it, asks for the address, shows the fixed warning first, then the managers, and posts only on the confirm", async () => {
    const dialog = await openForm();
    // The form is the first state: the placeholder and both helpers, and nothing has been asked of the server.
    const address = within(dialog).getByLabelText("Address") as HTMLInputElement;
    expect(address.placeholder).toBe("192.168.1.50/24");
    expect(
      within(dialog).getByText(
        "The address with its prefix length. It must be unused and inside the network of every node.",
      ),
    ).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Leave empty and TierPlan chooses one. Change it if another router on the network uses the same ID.",
      ),
    ).toBeTruthy();
    expect(writes).toEqual([]);
    // Nothing to check yet.
    expect(
      (within(dialog).getByRole("button", { name: "Check address" }) as HTMLButtonElement).disabled,
    ).toBe(true);

    typeAddress(dialog, "192.168.1.50/24");
    fireEvent.click(within(dialog).getByRole("button", { name: "Check address" }));
    await waitUnderFakeClock(() => expect(within(dialog).getByText("node-a")).toBeTruthy());

    // One preview, no write.
    expect(writes).toEqual([
      { method: "POST", path: `${BASE}/vip/preview`, body: { address: "192.168.1.50/24" } },
    ]);
    // The privileged-services warning is the first alert, word for word, and the managers follow it.
    const first = dialog.querySelector('[role="alert"]') as HTMLElement;
    expect(within(first).getByText("Read this before you turn it on")).toBeTruthy();
    expect(first.textContent).toContain(WARNING);
    expect(
      first.compareDocumentPosition(within(dialog).getByRole("table", { name: "Managers" })) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();

    // The confirm posts once and the job is followed to its end.
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on virtual IP" }));
    await waitUnderFakeClock(() =>
      expect(within(dialog).getByText("Virtual IP is on.")).toBeTruthy(),
    );
    expect(writes.filter((w) => w.path === `${BASE}/vip`)).toEqual([
      { method: "POST", path: `${BASE}/vip`, body: { address: "192.168.1.50/24" } },
    ]);
  });

  it("sends a router ID only when one was typed", async () => {
    const dialog = await openForm();
    typeAddress(dialog, "192.168.1.50/24");
    fireEvent.change(within(dialog).getByLabelText("Router ID"), { target: { value: "77" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Check address" }));
    await waitUnderFakeClock(() => expect(within(dialog).getByText("node-a")).toBeTruthy());
    expect(writes[0]?.body).toEqual({ address: "192.168.1.50/24", routerId: 77 });
  });

  it("draws a refusal as the server's sentence in a destructive alert above the form, and not as a toast", async () => {
    const sentence =
      "192.168.2.50 is not inside the network of node-b. Pick an address every node can reach.";
    previewAnswer = () =>
      Response.json(
        previewBody({
          findings: [
            { code: "vip_outside_node_subnet", severity: "error", message: sentence, items: [] },
          ],
        }),
      );
    const dialog = await openForm();
    typeAddress(dialog, "192.168.2.50/24");
    fireEvent.click(within(dialog).getByRole("button", { name: "Check address" }));
    const refusal = await waitUnderFakeClock(() => within(dialog).getByText(sentence));
    const alert = refusal.closest('[role="alert"]') as HTMLElement;
    expect(alert).toBeTruthy();
    // Above the form, which is still there to correct, and the fixed warning is not drawn for a refused address.
    expect(
      alert.compareDocumentPosition(within(dialog).getByLabelText("Address")) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(within(dialog).queryByText("Read this before you turn it on")).toBeNull();
    expect(
      (within(dialog).getByRole("button", { name: "Check address" }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect(within(dialog).queryByRole("button", { name: "Turn on virtual IP" })).toBeNull();
    // No toast: the only alert outside the dialog is none.
    expect(document.querySelector("[data-sonner-toast]")).toBeNull();
    expect(writes.filter((w) => w.path === `${BASE}/vip`)).toEqual([]);
  });

  it("draws a 422 for an address that is not one in the same place", async () => {
    previewAnswer = () =>
      problem(
        422,
        "vip_address_invalid",
        "Give the address with its prefix length, such as 192.168.1.50/24.",
      );
    const dialog = await openForm();
    typeAddress(dialog, "nonsense");
    fireEvent.click(within(dialog).getByRole("button", { name: "Check address" }));
    await waitUnderFakeClock(() =>
      expect(
        within(dialog).getByText(
          "Give the address with its prefix length, such as 192.168.1.50/24.",
        ),
      ).toBeTruthy(),
    );
    expect(within(dialog).getByLabelText("Address")).toBeTruthy();
    expect(document.querySelector("[data-sonner-toast]")).toBeNull();
  });

  it("keeps the fixed warning in front of the person even when a preview came without the sentence", async () => {
    previewAnswer = () => Response.json(previewBody({ warning: "" }));
    const dialog = await openForm();
    typeAddress(dialog, "192.168.1.50/24");
    fireEvent.click(within(dialog).getByRole("button", { name: "Check address" }));
    await waitUnderFakeClock(() => expect(within(dialog).getByText("node-a")).toBeTruthy());
    const first = dialog.querySelector('[role="alert"]') as HTMLElement;
    expect(first.textContent).toContain(WARNING);
  });

  it("closes from Keep virtual IP off without asking anything", async () => {
    const dialog = await openForm();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep virtual IP off" }));
    await waitUnderFakeClock(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toEqual([]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
  });
});

describe("where the card is drawn", () => {
  it("explains the virtual IP and offers Turn on virtual IP when it is off and ingress is on", async () => {
    mount();
    expect(await button("Turn on virtual IP")).toBeTruthy();
    expect(
      screen.getByText(
        "A virtual IP is one address that always points at a working proxy. It moves to another node if the one holding it fails.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("Off")).toBeTruthy();
  });
});
