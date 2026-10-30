// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RemoveEnvironmentCard } from "@/features/environments/remove-environment-card";
import "@/i18n";

const SWARM_CLEANUP =
  " docker service rm coaming-agent && docker secret rm coaming-agent-token-g2\n docker volume rm coaming-agent   # on every node that ran the agent";
const STANDALONE_CLEANUP = " docker rm -f coaming-agent && docker volume rm coaming-agent";

function agent(id: string, node: string, connected: boolean, extra: Record<string, unknown> = {}) {
  return {
    id,
    node,
    role: "worker",
    status: "active",
    blocked: false,
    connected,
    agentVersion: "0.1.0",
    certFingerprint: "ab".repeat(32),
    certNotAfter: "2027-01-01T00:00:00Z",
    clockOffsetSeconds: 0,
    readOnly: false,
    selfUpdate: true,
    updateAvailable: false,
    updateState: { state: "idle" },
    ...extra,
  };
}

function environment(extra: Record<string, unknown> = {}): EnvironmentItem {
  return {
    id: "e2",
    name: "edge",
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: { name: "Docker Engine", version: "28.5.1", apiVersion: "1.50" },
    status: { state: "online" },
    envType: "swarm",
    updateAvailable: false,
    ...extra,
  } as EnvironmentItem;
}

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let items: ReturnType<typeof agent>[];
let agentsAnswer: () => Response | Promise<Response>;
let deleteAnswer: () => Response | Promise<Response>;
let client: QueryClient;

beforeEach(() => {
  sent = [];
  items = [agent("a1", "mgr-1", true), agent("a2", "wrk-1", true), agent("a3", "wrk-2", false)];
  agentsAnswer = () =>
    Response.json({
      items,
      manualCleanup: { swarm: SWARM_CLEANUP, standalone: STANDALONE_CLEANUP },
    });
  deleteAnswer = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      sent.push({ method: req.method, path: url.pathname, search: url.search });
      if (url.pathname === "/api/env/e2/agents" && req.method === "GET") return agentsAnswer();
      if (url.pathname === "/api/environments/e2" && req.method === "DELETE") return deleteAnswer();
      if (url.pathname === "/api/environments") return Response.json({ items: [] });
      if (url.pathname === "/api/me/recent-environments")
        return Response.json({ environmentIds: [] });
      if (url.pathname === "/api/local-host") {
        return Response.json({
          socketPath: "/var/run/docker.sock",
          detected: true,
          managed: false,
        });
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

async function mount(env: EnvironmentItem = environment(), waitForAgents = true) {
  const root = createRootRoute();
  const index = createRoute({
    getParentRoute: () => root,
    path: "/",
    component: () => <RemoveEnvironmentCard env={env} />,
  });
  const list = createRoute({
    getParentRoute: () => root,
    path: "/environments",
    component: () => <p>The environments list</p>,
  });
  const router = createRouter({
    routeTree: root.addChildren([index, list]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RouterProvider router={router} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  const button = await screen.findByRole("button", {
    name: env.kind === "local" ? "Remove host" : "Remove environment",
  });
  if (waitForAgents && env.kind !== "local") {
    await waitFor(() => expect(sent.some((s) => s.path === "/api/env/e2/agents")).toBe(true));
  }
  return { router, button };
}

async function openDialog(env?: EnvironmentItem) {
  const { router, button } = await mount(env);
  fireEvent.click(button);
  const dialog = await screen.findByRole("alertdialog", { name: `Remove ${env?.name ?? "edge"}?` });
  return { router, button, dialog };
}

const deletes = () => sent.filter((s) => s.method === "DELETE");

describe("RemoveEnvironmentCard", () => {
  it("is a card with the anchor remove whose dialog puts focus on Cancel and says what removal does", async () => {
    await mount();
    expect(document.getElementById("remove")?.tagName).toBe("SECTION");
    fireEvent.click(screen.getByRole("button", { name: "Remove environment" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Remove edge?" });
    expect(dialog.textContent).toContain(
      "Coaming will stop managing this environment and revoke every agent and the join token. Containers, images, volumes and networks on the hosts are not changed.",
    );
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    expect(within(dialog).getByRole("button", { name: "Remove environment" })).toBeTruthy();
  });

  it("checks the uninstall box when an agent is online and says how many are, and sends uninstallAgents=true", async () => {
    // The lists that show the environment are cached, as they are on a real page.
    client.setQueryData(["environments"], { items: [environment()] });
    client.setQueryData(["recent-environments"], ["e2"]);
    const { router } = await openDialog();
    const box = await screen.findByRole("checkbox", {
      name: "Also uninstall the agent from the hosts",
    });
    await waitFor(() => expect(box.getAttribute("aria-checked")).toBe("true"));
    expect(box.hasAttribute("disabled")).toBe(false);
    expect(
      screen.getByText("2 of 3 agents are online. The others need the manual command below."),
    ).toBeTruthy();

    fireEvent.click(
      within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove environment" }),
    );
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.path).toBe("/api/environments/e2");
    expect(deletes()[0]?.search).toBe("?uninstallAgents=true");
    // It lands on the list, and what was cached about the environment is dropped.
    await screen.findByText("The environments list");
    expect(router.state.location.pathname).toBe("/environments");
    expect(client.getQueryData(["agents", "e2"])).toBeUndefined();
    await waitFor(() => expect(client.getQueryState(["environments"])?.isInvalidated).toBe(true));
    expect(client.getQueryState(["recent-environments"])?.isInvalidated).toBe(true);
    expect(await screen.findByText("edge was removed.")).toBeTruthy();
  });

  it("sends uninstallAgents=false when the box is unchecked", async () => {
    await openDialog();
    const box = await screen.findByRole("checkbox", {
      name: "Also uninstall the agent from the hosts",
    });
    await waitFor(() => expect(box.getAttribute("aria-checked")).toBe("true"));
    fireEvent.click(box);
    expect(box.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(
      within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove environment" }),
    );
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.search).toBe("?uninstallAgents=false");
  });

  it("disables and unchecks the box with the no-agents helper when none is online, and never asks for an uninstall", async () => {
    items = [agent("a1", "mgr-1", false), agent("a3", "wrk-2", false)];
    await openDialog();
    const box = await screen.findByRole("checkbox", {
      name: "Also uninstall the agent from the hosts",
    });
    expect(
      await screen.findByText("No agents are online. Use the manual command below."),
    ).toBeTruthy();
    expect(box.hasAttribute("disabled")).toBe(true);
    expect(box.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(
      within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove environment" }),
    );
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.search).toBe("?uninstallAgents=false");
  });

  it("counts only agents that are not revoked, and says so plainly when all of them are online", async () => {
    items = [
      agent("a1", "mgr-1", true),
      agent("a2", "wrk-1", true),
      agent("a9", "old", false, { status: "revoked", blocked: true }),
    ];
    await openDialog();
    expect(await screen.findByText("All 2 agents are online.")).toBeTruthy();
  });

  it("says it is checking while the agents are being read and leaves the box unchecked", async () => {
    agentsAnswer = () => new Promise<Response>(() => {});
    const { button } = await mount(environment(), false);
    fireEvent.click(button);
    await screen.findByRole("alertdialog", { name: "Remove edge?" });
    expect(screen.getByText("Checking which agents are online…")).toBeTruthy();
    expect(
      screen
        .getByRole("checkbox", { name: "Also uninstall the agent from the hosts" })
        .getAttribute("aria-checked"),
    ).toBe("false");
  });

  it("says it cannot tell who is online when the agents cannot be read, and leaves the box to the person", async () => {
    agentsAnswer = () => new Response("nope", { status: 500 });
    await openDialog();
    expect(
      await screen.findByText(
        "Couldn't tell which agents are online. Use the manual command below.",
      ),
    ).toBeTruthy();
    const box = screen.getByRole("checkbox", { name: "Also uninstall the agent from the hosts" });
    expect(box.hasAttribute("disabled")).toBe(false);
    expect(box.getAttribute("aria-checked")).toBe("false");
  });

  it("always has the manual cleanup command, the one that fits a swarm", async () => {
    await openDialog();
    const dialog = screen.getByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Manual cleanup command" }));
    expect(
      await within(dialog).findByText(
        "If an agent is offline, or you leave the box unchecked, run this on each host:",
      ),
    ).toBeTruthy();
    expect(dialog.textContent).toContain("docker secret rm coaming-agent-token-g2");
    expect(dialog.textContent).not.toContain("docker rm -f coaming-agent");
    expect(
      within(dialog).getByRole("button", { name: "Copy the swarm cleanup command" }),
    ).toBeTruthy();
  });

  it("shows the standalone command for a standalone host and both, labelled, while the type is not known", async () => {
    await openDialog(environment({ envType: "standalone" }));
    let dialog = screen.getByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Manual cleanup command" }));
    await within(dialog).findByRole("button", { name: "Copy the standalone cleanup command" });
    expect(dialog.textContent).not.toContain("docker service rm");
    cleanup();

    await openDialog(environment({ envType: undefined }));
    dialog = screen.getByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Manual cleanup command" }));
    await within(dialog).findByRole("button", { name: "Copy the swarm cleanup command" });
    expect(
      within(dialog).getByRole("button", { name: "Copy the standalone cleanup command" }),
    ).toBeTruthy();
    expect(within(dialog).getByText("Swarm")).toBeTruthy();
    expect(within(dialog).getByText("Standalone host")).toBeTruthy();
  });

  it("keeps the dialog open with an alert when the removal fails, and works on a second try", async () => {
    deleteAnswer = () => new Response("boom", { status: 500 });
    await openDialog();
    const confirm = () =>
      within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove environment" });
    fireEvent.click(confirm());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Couldn't remove the environment. Try again.");
    expect(screen.getByRole("alertdialog", { name: "Remove edge?" })).toBeTruthy();
    expect(confirm().hasAttribute("disabled")).toBe(false);

    deleteAnswer = () => new Response(null, { status: 204 });
    fireEvent.click(confirm());
    await screen.findByText("The environments list");
    expect(deletes()).toHaveLength(2);
  });

  it("treats an environment that is already gone as removed", async () => {
    deleteAnswer = () => Response.json({ type: "environment_not_found" }, { status: 404 });
    await openDialog();
    fireEvent.click(
      within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove environment" }),
    );
    await screen.findByText("The environments list");
  });

  it("shows a pending state with what it is waiting for, and cannot be cancelled or sent twice", async () => {
    let release: (r: Response) => void = () => {};
    deleteAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    await openDialog();
    const dialog = screen.getByRole("alertdialog");
    await waitFor(() =>
      expect(
        screen
          .getByRole("checkbox", { name: "Also uninstall the agent from the hosts" })
          .getAttribute("aria-checked"),
      ).toBe("true"),
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove environment" }));
    const pending = await within(dialog).findByRole("button", { name: "Removing…" });
    expect(pending.hasAttribute("disabled")).toBe(true);
    expect(within(dialog).getByRole("button", { name: "Cancel" }).hasAttribute("disabled")).toBe(
      true,
    );
    expect(
      within(dialog).getByText("Telling the agents to uninstall. This can take up to 30 seconds."),
    ).toBeTruthy();
    fireEvent.click(pending);
    expect(deletes()).toHaveLength(1);
    release(new Response(null, { status: 204 }));
    await screen.findByText("The environments list");
  });

  it("gives focus back to the Remove environment button when the dialog is cancelled", async () => {
    const { button } = await openDialog();
    fireEvent.click(
      within(screen.getByRole("alertdialog")).getByRole("button", { name: "Cancel" }),
    );
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(document.activeElement).toBe(button);
    expect(deletes()).toHaveLength(0);
  });

  it("keeps the Phase 1 Remove host dialog for the local socket: no box, no command, no uninstall", async () => {
    const local = environment({
      id: "e2",
      name: "lab-1",
      kind: "local",
      isLocalSocket: true,
      isSelfHost: true,
    });
    const { button } = await mount(local);
    fireEvent.click(button);
    const dialog = await screen.findByRole("alertdialog", { name: "Remove lab-1?" });
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
    expect(within(dialog).queryByText("Manual cleanup command")).toBeNull();
    expect(sent.some((s) => s.path === "/api/env/e2/agents")).toBe(false);
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove host" }));
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.search).toBe("?uninstallAgents=false");
    await screen.findByText("The environments list");
  });
});
