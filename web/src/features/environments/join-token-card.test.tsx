// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { JoinTokenCard } from "@/features/environments/join-token-card";
import "@/i18n";

const NOW = "2026-10-03T12:00:00Z";
const CREATED = "2026-10-03T09:00:00Z";

function status(extra: Record<string, unknown> = {}) {
  return { generation: 3, createdAt: CREATED, createdBy: "admin", disabled: false, ...extra };
}

function install() {
  return {
    agentUrl: "wss://tierplan.example.com/agent/connect",
    generation: 4,
    image: "ghcr.io/ianglennon/tierplan-agent:0.1.0",
    joinToken: "cmjt_E1_s3cr3tvalue",
    manualCleanup: { standalone: " docker rm -f tierplan-agent", swarm: " docker service rm x" },
    secretName: "tierplan-agent-token-g4",
    standalone: { command: " docker run x", composeFile: "services: {}\n" },
    swarm: { command: " docker service create x", stackFile: "services: {}\n" },
  };
}

interface Sent {
  method: string;
  path: string;
}

let sent: Sent[];
let tokenGet: () => Response | Promise<Response>;
let tokenDelete: () => Response | Promise<Response>;
let client: QueryClient;
let disabled: boolean;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(NOW));
  sent = [];
  disabled = false;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  tokenGet = () => Response.json(status({ disabled }));
  tokenDelete = () => {
    disabled = true;
    return new Response(null, { status: 204 });
  };
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ method: req.method, path });
      if (path === "/api/environments/e1/token") {
        if (req.method === "GET") return tokenGet();
        if (req.method === "DELETE") return tokenDelete();
        if (req.method === "POST") return Response.json({ install: install(), hasAgents: false });
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

async function mount() {
  const root = createRootRoute({ component: () => <JoinTokenCard envId="e1" /> });
  const router = createRouter({
    routeTree: root,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <EnvHealthStreamProvider>
          <RouterProvider router={router} />
        </EnvHealthStreamProvider>
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await screen.findByRole("button", { name: "Show install command" });
  // The status is read separately from the buttons.
  await waitFor(() => expect(screen.queryByText("Loading the join token…")).toBeNull());
}

const count = (method: string) =>
  sent.filter((s) => s.method === method && s.path === "/api/environments/e1/token").length;

describe("JoinTokenCard", () => {
  it("says what the join token is and when and by whom it was last changed", async () => {
    await mount();
    expect(
      screen.getByText(
        "The join token lets a host enrol into this environment. It stays valid until you rotate or disable it. Enrolled agents keep working when it changes.",
      ),
    ).toBeTruthy();
    expect(screen.getByText(/^Last changed/).textContent).toBe("Last changed 3 h ago by admin.");
    expect(screen.getByRole("button", { name: "Disable token" })).toBeTruthy();
    expect(screen.queryByText("Disabled")).toBeNull();
  });

  it("shows a loading status first and a retry when the read fails", async () => {
    tokenGet = () => new Response("nope", { status: 500 });
    const root = createRootRoute({ component: () => <JoinTokenCard envId="e1" /> });
    const router = createRouter({
      routeTree: root,
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <EnvHealthStreamProvider>
            <RouterProvider router={router} />
          </EnvHealthStreamProvider>
        </TooltipProvider>
      </QueryClientProvider>,
    );
    expect(await screen.findByText("Couldn't load the join token.")).toBeTruthy();
    tokenGet = () => Response.json(status());
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(/^Last changed/)).toBeTruthy();
  });

  it("opens the install dialog in rotate mode from Show install command, and rotates nothing by opening it", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Show install command" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: "Show install command" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Generate new token" })).toBeTruthy();
    expect(count("POST")).toBe(0);
  });

  it("puts focus back on Show install command when the dialog closes", async () => {
    await mount();
    const show = screen.getByRole("button", { name: "Show install command" });
    fireEvent.click(show);
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(show));
  });

  it("reads the token status again when the install dialog closes, because a rotation re-enables enrolment", async () => {
    disabled = true;
    await mount();
    expect(await screen.findByText("Disabled")).toBeTruthy();
    expect(count("GET")).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Show install command" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Generate new token" }));
    await within(dialog).findByRole("heading", { name: "Install the agent" });
    disabled = false;
    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));

    await waitFor(() => expect(screen.queryByText("Disabled")).toBeNull());
    expect(count("GET")).toBe(2);
    expect(screen.getByRole("button", { name: "Disable token" })).toBeTruthy();
  });

  it("opens the Disable token alert-dialog with focus on Cancel, and Escape sends nothing", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Disable token" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Disable the join token?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "No new host can enrol until you generate a new token with Show install command. Enrolled agents aren't affected.",
      ),
    ).toBeTruthy();
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(count("DELETE")).toBe(0);
  });

  it("sends DELETE once on confirm, then shows the Disabled badge and the disabled copy", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Disable token" }));
    const dialog = await screen.findByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: "Disable token" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);

    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(count("DELETE")).toBe(1);
    expect(await screen.findByText("Disabled")).toBeTruthy();
    expect(
      screen.getByText("The join token is disabled. Show install command generates a new one."),
    ).toBeTruthy();
    // Already disabled, so there is nothing left to disable.
    expect(screen.queryByRole("button", { name: "Disable token" })).toBeNull();
    expect(screen.getByRole("button", { name: "Show install command" })).toBeTruthy();
  });

  it("shows the pending label and disables the buttons while the request runs", async () => {
    let release: (r: Response) => void = () => {};
    tokenDelete = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Disable token" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Disable token" }));

    const pending = await within(dialog).findByRole("button", { name: "Disabling…" });
    expect(pending.matches(":disabled")).toBe(true);
    expect(within(dialog).getByRole("button", { name: "Cancel" }).matches(":disabled")).toBe(true);
    expect(count("DELETE")).toBe(1);

    disabled = true;
    await act(async () => release(new Response(null, { status: 204 })));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("keeps the dialog open with an inline alert when the request fails", async () => {
    tokenDelete = () => Response.json({ status: 500, title: "boom" }, { status: 500 });
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Disable token" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Disable token" }));

    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("Something went wrong. Try again.");
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(screen.queryByText("Disabled")).toBeNull();
    // It can be tried again.
    expect(within(dialog).getByRole("button", { name: "Disable token" }).matches(":disabled")).toBe(
      false,
    );
  });

  it("shows a token that is disabled on the server as disabled from the start", async () => {
    disabled = true;
    await mount();
    expect(await screen.findByText("Disabled")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Disable token" })).toBeNull();
  });
});
