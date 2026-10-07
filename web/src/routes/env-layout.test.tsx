// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { retryTiming } from "@/routes/_app/env/$envId";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";

const ME = makeMe({ instanceAdmin: true, permissions: splitPermissions([]) });

let me: typeof ME = ME;

// grant gives the signed-in user these permissions on every environment.
function grant(...perms: string[]) {
  me = { ...ME, permissions: splitPermissions(perms) };
}

const LAB = {
  id: "e1",
  name: "Lab host",
  kind: "local",
  type: "swarm-manager",
  enabled: true,
  isSelfHost: true,
  createdAt: "2026-10-01T10:00:00Z",
  engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.54" },
  status: { state: "online" },
};

const SUMMARY = {
  environment: { id: "e1", name: "Lab host", type: "swarm-manager", status: { state: "online" } },
  engine: {
    name: "Docker Engine",
    version: "29.8.2",
    apiVersion: "1.54",
    os: "linux",
    arch: "amd64",
    storageDriver: "overlayfs",
    ncpu: 8,
    memTotal: 17179869184,
  },
  counts: {
    containers: 7,
    containersRunning: 5,
    containersStopped: 2,
    images: 12,
    volumes: 0,
    networks: 4,
  },
  isWorker: false,
  fetchedAt: "2026-10-01T10:00:00Z",
};

function problem(status: number, type: string, extra: Record<string, unknown> = {}) {
  return Response.json({ type, title: "x", status, ...extra }, { status });
}

interface Stub {
  environments: () => Response;
  summary: () => Response;
  reconnect: () => Response | Promise<Response>;
  adopt?: () => Response | Promise<Response>;
}

let sent: { path: string; method: string; csrf: string | null }[] = [];
let stub: Stub;

function stubApi() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
      }
      if (path === "/api/environments") return stub.environments();
      if (path === "/api/env/e1/summary") return stub.summary();
      if (path === "/api/env/e1/reconnect") return stub.reconnect();
      if (path === "/api/environments/e1/adopt-cluster" && stub.adopt) return stub.adopt();
      return Response.json({}, { status: 404 });
    }),
  );
}

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
  return router;
}

const countSummaryReads = () => sent.filter((s) => s.path === "/api/env/e1/summary").length;

beforeEach(() => {
  sent = [];
  me = ME;
  setCsrfToken(null);
  retryTiming.delayMs = 1;
  retryTiming.polls = 2;
  stub = {
    environments: () => Response.json({ items: [LAB] }),
    summary: () => Response.json(SUMMARY),
    reconnect: () => Response.json(LAB, { status: 202 }),
  };
  stubApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.title = "";
});

describe("environment layout full-page states", () => {
  it("shows Docker isn't reachable, naming the environment and the time, with Retry (D-25)", async () => {
    stub.summary = () =>
      problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z", reason: "unreachable" });
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Docker isn't reachable" })).toBeTruthy();
    const body = screen.getByText(/Bayplan lost its connection to Lab host at \d{1,2}:\d{2}/);
    expect(body.textContent).toContain("/var/run/docker.sock is mounted");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(document.title).toBe("Docker isn't reachable · Lab");
  });

  it("leaves the time out when the engine never answered", async () => {
    stub.summary = () => problem(503, "environment_offline", { reason: "unreachable" });
    renderAt("/env/e1");
    const body = await screen.findByText(/Bayplan lost its connection to Lab host\./);
    expect(body.textContent).not.toMatch(/ at \d/);
  });

  it("keeps the rest of the UI working while the environment is down (D-26)", async () => {
    stub.summary = () => problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Docker isn't reachable" });
    expect(screen.getByRole("button", { name: "User menu" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Switch environment" })).toBeTruthy();
    expect(screen.getAllByRole("link", { name: "Source code (opens in a new tab)" })).toHaveLength(
      1,
    );
  });

  it("shows This engine isn't supported with the engine, version and API (D-58)", async () => {
    stub.summary = () =>
      problem(409, "environment_unsupported", {
        reason: "api_too_old",
        engine: { name: "Docker Engine", version: "23.0.1", apiVersion: "1.42" },
      });
    renderAt("/env/e1");
    expect(
      await screen.findByRole("heading", { name: "This engine isn't supported" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "Docker Engine 23.0.1 (API 1.42) is below the minimum Bayplan supports, Docker Engine 24. Upgrade Docker Engine, then retry.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("uses the Podman copy for Podman", async () => {
    stub.summary = () =>
      problem(409, "environment_unsupported", {
        reason: "podman",
        engine: { name: "Podman", version: "5.2.0", apiVersion: "1.41" },
      });
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Podman isn't supported" })).toBeTruthy();
    expect(screen.getByText("Bayplan manages Docker Engine only.")).toBeTruthy();
  });

  it("renders the not-found copy for an unknown environment and the permission page on 403", async () => {
    stub.summary = () => problem(404, "environment_not_found");
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Go to the dashboard" })).toBeTruthy();
    cleanup();
    queryClient.clear();

    stub.summary = () => problem(403, "about:blank");
    renderAt("/env/e1");
    expect(
      await screen.findByRole("heading", { name: "You don't have permission to view this page" }),
    ).toBeTruthy();
    expect(screen.getByText("Ask an admin for access.")).toBeTruthy();
  });

  it("does not take a plain 503 from a proxy for an offline environment", async () => {
    stub.summary = () => new Response("bad gateway", { status: 503 });
    renderAt("/env/e1");
    await waitFor(() => expect(countSummaryReads()).toBeGreaterThan(0));
    await screen.findByRole("button", { name: "Switch environment" });
    expect(screen.queryByRole("heading", { name: "Docker isn't reachable" })).toBeNull();
  });
});

describe("Retry", () => {
  it("posts the reconnect, shows Retrying…, and replaces the state when Docker answers", async () => {
    stub.summary = () => problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z" });
    renderAt("/env/e1");
    const button = await screen.findByRole("button", { name: "Retry" });
    // Docker answers from now on, but the reconnect request is held open so the
    // pending state can be seen.
    stub.summary = () => Response.json(SUMMARY);
    let release: () => void = () => {};
    stub.reconnect = () =>
      new Promise<Response>((resolve) => {
        release = () => resolve(Response.json(LAB, { status: 202 }));
      });
    fireEvent.click(button);
    expect(await screen.findByRole("button", { name: "Retrying…" })).toHaveProperty(
      "disabled",
      true,
    );
    release();
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Docker isn't reachable" })).toBeNull(),
    );
    const reconnect = sent.filter((s) => s.path === "/api/env/e1/reconnect");
    expect(reconnect).toHaveLength(1);
    expect(reconnect[0]?.method).toBe("POST");
    expect(reconnect[0]?.csrf).toBe("csrf-1");
  });

  it("re-renders the state with Attempted when it is still down", async () => {
    stub.summary = () => problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z" });
    renderAt("/env/e1");
    fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Attempted just now")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Docker isn't reachable" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toHaveProperty("disabled", false);
  });
});

describe("environment switcher", () => {
  // The list, the filter, Recent, the footer and navigation have their own
  // tests in components/env-switcher.test.tsx; these cover the trigger and the
  // list inside the full layout.
  it("shows the name, type badge and a health dot in the trigger, and the item in the list (D-54)", async () => {
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    await waitFor(() => expect(within(trigger).getByText("Lab host")).toBeTruthy());
    expect(within(trigger).getByText("Swarm")).toBeTruthy();
    expect(within(trigger).getByRole("img", { name: "Online" })).toBeTruthy();

    fireEvent.click(trigger);
    const list = await screen.findByRole("listbox");
    const item = within(list).getByRole("option", { name: /Lab host/ });
    expect(within(item).getByText("Swarm")).toBeTruthy();
    expect(within(item).getByRole("img", { name: "Online" })).toBeTruthy();
    expect(within(list).getByRole("option", { name: "All environments" })).toBeTruthy();
  });

  it("pairs an offline environment's dot with its health name", async () => {
    stub.environments = () =>
      Response.json({
        items: [{ ...LAB, status: { state: "offline", since: "2026-10-01T14:05:00Z" } }],
      });
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    await waitFor(() => expect(within(trigger).getByRole("img", { name: "Offline" })).toBeTruthy());
    fireEvent.click(trigger);
    const list = await screen.findByRole("listbox");
    expect(within(list).getByRole("img", { name: "Offline" })).toBeTruthy();
  });

  it("shows No environment and still offers All environments when there are none", async () => {
    stub.environments = () => Response.json({ items: [] });
    renderAt("/");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    expect(await within(trigger).findByText("No environment")).toBeTruthy();
    fireEvent.click(trigger);
    expect(await screen.findByRole("option", { name: "All environments" })).toBeTruthy();
  });

  it("truncates a long name with a title and keeps the badge and dot", async () => {
    const long = "a-very-long-environment-name-".repeat(4);
    stub.environments = () => Response.json({ items: [{ ...LAB, name: long }] });
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    const name = await within(trigger).findByText(long);
    expect(name.className).toContain("truncate");
    expect(within(trigger).getByText("Swarm").className).toContain("shrink-0");
    expect(within(trigger).getByRole("img", { name: "Online" }).className).toContain("shrink-0");
  });
});

// An environment whose hosts dial in through an agent (D-33): its full-page
// states come from the problem's state, not from the local socket's codes.
const AGENT = {
  ...LAB,
  kind: "agent",
  isSelfHost: false,
  isLocalSocket: false,
  envType: "standalone",
  type: "standalone",
};

function withAgentEnv(status: Record<string, unknown> = { state: "online" }, extra = {}) {
  stub.environments = () => Response.json({ items: [{ ...AGENT, status, ...extra }] });
}

describe("agent environment full-page states (ENV-08, D-33)", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  it("asks for an agent when none has connected, and opens the install dialog for a token holder", async () => {
    grant("environments.read", "environments.token");
    withAgentEnv({ state: "pending" });
    stub.summary = () => problem(503, "environment_offline", { state: "pending" });
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "No agent has connected yet" })).toBeTruthy();
    expect(
      screen.getByText("Install the Bayplan agent on a host to start managing Lab host."),
    ).toBeTruthy();
    expect(document.title).toBe("No agent has connected yet · Lab");
    fireEvent.click(screen.getByRole("button", { name: "Show install command" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("button", { name: "Generate new token" })).toBeTruthy();
    // Closing it puts focus back on the button that opened it.
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() =>
      expect(document.activeElement).toBe(
        screen.getByRole("button", { name: "Show install command" }),
      ),
    );
  });

  it("tells someone without environments.token to ask an admin, and offers no button", async () => {
    grant("environments.read");
    withAgentEnv({ state: "pending" });
    stub.summary = () => problem(503, "environment_offline", { state: "pending" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "No agent has connected yet" });
    expect(screen.getByText(/Ask an admin to finish setting it up\.$/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show install command" })).toBeNull();
  });

  it("shows Reconnecting with an amber pulsing icon, and no button (D-41)", async () => {
    grant("environments.read", "environments.token", "environments.delete");
    withAgentEnv({ state: "reconnecting" });
    stub.summary = () => problem(503, "environment_offline", { state: "reconnecting" });
    renderAt("/env/e1");
    const heading = await screen.findByRole("heading", { name: "Reconnecting to Lab host…" });
    expect(
      screen.getByText(
        "Bayplan just started and is waiting for the agent to reconnect. This usually takes under a minute.",
      ),
    ).toBeTruthy();
    const page = heading.closest("[data-slot=empty]");
    expect(page?.querySelector("button, a")).toBeNull();
    const icon = page?.querySelector("[data-slot=empty-icon] svg");
    expect(icon?.getAttribute("class")).toContain("animate-pulse");
    expect(icon?.getAttribute("class")).toContain("text-status-warning-text");
    expect(screen.queryByRole("heading", { name: /isn't reachable/ })).toBeNull();
  });

  it("explains an offline agent with its reason text and hint, the time, and Retry", async () => {
    grant("environments.read");
    withAgentEnv({ state: "offline", reason: "timeout", since: "2026-10-01T14:05:00Z" });
    stub.summary = () =>
      problem(503, "environment_offline", {
        state: "offline",
        reason: "timeout",
        since: "2026-10-01T14:05:00Z",
      });
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Lab host isn't reachable" })).toBeTruthy();
    expect(
      screen.getByText(/Bayplan lost contact with the agent on Lab host at \d{1,2}:\d{2}/),
    ).toBeTruthy();
    expect(screen.getByText("A node stopped answering.")).toBeTruthy();
    expect(screen.getByText(/The agent reconnects by itself\./)).toBeTruthy();
    // Phase 1's local-socket wording never appears for an agent environment.
    expect(screen.queryByText(/docker\.sock is mounted into the Bayplan container/)).toBeNull();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("runs Retry for an offline agent with Retrying… and then the Attempted line", async () => {
    grant("environments.read");
    withAgentEnv({ state: "offline", reason: "agent_offline" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "offline", reason: "agent_offline" });
    renderAt("/env/e1");
    const button = await screen.findByRole("button", { name: "Retry" });
    let release: () => void = () => {};
    stub.reconnect = () =>
      new Promise<Response>((resolve) => {
        release = () => resolve(Response.json(AGENT, { status: 202 }));
      });
    fireEvent.click(button);
    expect(await screen.findByRole("button", { name: "Retrying…" })).toHaveProperty(
      "disabled",
      true,
    );
    release();
    expect(await screen.findByText("Attempted just now")).toBeTruthy();
  });

  it("shows only the check command that fits the environment's type (swarm)", async () => {
    grant("environments.read");
    withAgentEnv({ state: "offline", reason: "agent_offline" }, { envType: "swarm" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "offline", reason: "agent_offline" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host isn't reachable" });
    expect(screen.getByText("docker service ps bayplan-agent")).toBeTruthy();
    expect(screen.queryByText("docker ps --filter name=bayplan-agent")).toBeNull();
  });

  it("offers Open node settings and Remove environment for a revoked environment, by permission", async () => {
    grant("environments.read", "environments.manage", "environments.delete");
    withAgentEnv({ state: "revoked", reason: "revoked" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "revoked", reason: "revoked" });
    renderAt("/env/e1");
    expect(
      await screen.findByRole("heading", { name: "This environment was revoked" }),
    ).toBeTruthy();
    expect(
      screen.getByText("An admin revoked every agent on Lab host, so Bayplan can't reach it."),
    ).toBeTruthy();
    const settings = screen.getByRole("link", { name: "Open node settings" });
    expect(settings.getAttribute("href")).toBe("/settings/environments/e1");
    const remove = screen.getByRole("link", { name: "Remove environment" });
    expect(remove.getAttribute("href")).toBe("/settings/environments/e1#remove");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("leaves out Remove environment without environments.delete, and the settings link without environments.manage", async () => {
    grant("environments.read");
    withAgentEnv({ state: "revoked", reason: "revoked" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "revoked", reason: "revoked" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "This environment was revoked" });
    expect(screen.queryByRole("link", { name: "Remove environment" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Open node settings" })).toBeNull();
  });

  it("says the swarm was re-initialised and offers Remove environment", async () => {
    grant("environments.read", "environments.delete");
    withAgentEnv({ state: "cluster_changed", reason: "cluster_changed" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "cluster_changed", reason: "cluster_changed" });
    renderAt("/env/e1");
    expect(
      await screen.findByRole("heading", { name: "This swarm was re-initialised" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "The agents on Lab host now report a different cluster, so Bayplan refused them. Nothing was changed on the old environment.",
      ),
    ).toBeTruthy();
    const remove = screen.getByRole("link", { name: "Remove environment" });
    expect(remove.getAttribute("href")).toBe("/settings/environments/e1#remove");
  });

  it("offers Adopt new cluster first and Remove environment second, and adopting posts once and brings the page back", async () => {
    grant("environments.read", "environments.manage", "environments.delete");
    withAgentEnv(
      { state: "cluster_changed", reason: "cluster_changed" },
      { pendingClusterId: "x3k2m9t0q4w5e6r7t8y9u0i1o" },
    );
    stub.summary = () =>
      problem(503, "environment_offline", { state: "cluster_changed", reason: "cluster_changed" });
    stub.adopt = () => Response.json({ clusterId: "x3k2m9t0q4w5e6r7t8y9u0i1o" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "This swarm was re-initialised" });
    const adopt = screen.getByRole("button", { name: "Adopt new cluster" });
    const remove = screen.getByRole("link", { name: "Remove environment" });
    // The primary action comes first.
    expect(adopt.compareDocumentPosition(remove) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    fireEvent.click(adopt);
    const dialog = await screen.findByRole("dialog", { name: "Adopt the new cluster?" });
    expect(dialog.textContent).toContain("x3k2m9t0q4w5e6r7t8y9u0i1o");

    // The adopt makes the environment whole again: the list and the summary say so.
    stub.environments = () => Response.json({ items: [{ ...AGENT, status: { state: "online" } }] });
    stub.summary = () => Response.json(SUMMARY);
    fireEvent.click(within(dialog).getByRole("button", { name: "Adopt new cluster" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(
      sent.filter((s) => s.method === "POST" && s.path.endsWith("/adopt-cluster")),
    ).toHaveLength(1);
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "This swarm was re-initialised" })).toBeNull(),
    );
  });

  it("does not offer Adopt new cluster to someone who may not manage the environment", async () => {
    grant("environments.read", "environments.delete");
    withAgentEnv({ state: "cluster_changed", reason: "cluster_changed" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "cluster_changed", reason: "cluster_changed" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "This swarm was re-initialised" });
    expect(screen.queryByRole("button", { name: "Adopt new cluster" })).toBeNull();
    expect(screen.getByRole("link", { name: "Remove environment" })).toBeTruthy();
  });

  it("returns to the page by itself when the stream reports the environment online", async () => {
    grant("environments.read");
    withAgentEnv({ state: "offline", reason: "agent_offline" });
    stub.summary = () =>
      problem(503, "environment_offline", { state: "offline", reason: "agent_offline" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host isn't reachable" });
    await waitFor(() =>
      expect(FakeEventSource.instances.some((s) => s.url === "/api/environments/stream")).toBe(
        true,
      ),
    );
    const stream = FakeEventSource.instances.find((s) => s.url === "/api/environments/stream");
    const reads = countSummaryReads();

    // The agent is back: the summary answers again and the stream says so. No click.
    stub.summary = () => Response.json(SUMMARY);
    act(() => {
      stream?.open();
      stream?.sendFrame(
        "env.status",
        {
          envId: "e1",
          at: "2026-10-01T14:10:00Z",
          environment: { ...AGENT, status: { state: "online" } },
        },
        "7",
      );
    });
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Lab host isn't reachable" })).toBeNull(),
    );
    expect(countSummaryReads()).toBeGreaterThan(reads);
    expect(await screen.findByRole("heading", { name: "Lab host" })).toBeTruthy();
  });

  it("moves from Reconnecting to the offline state when the stream says so", async () => {
    grant("environments.read");
    withAgentEnv({ state: "reconnecting" });
    stub.summary = () => problem(503, "environment_offline", { state: "reconnecting" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Reconnecting to Lab host…" });
    await waitFor(() =>
      expect(FakeEventSource.instances.some((s) => s.url === "/api/environments/stream")).toBe(
        true,
      ),
    );
    const stream = FakeEventSource.instances.find((s) => s.url === "/api/environments/stream");

    stub.summary = () =>
      problem(503, "environment_offline", { state: "offline", reason: "agent_offline" });
    act(() => {
      stream?.open();
      stream?.sendFrame(
        "env.status",
        {
          envId: "e1",
          at: "2026-10-01T14:10:00Z",
          environment: { ...AGENT, status: { state: "offline", reason: "agent_offline" } },
        },
        "8",
      );
    });
    expect(await screen.findByRole("heading", { name: "Lab host isn't reachable" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: /Reconnecting/ })).toBeNull();
  });
});

describe("degraded banner in the environment layout (D-33)", () => {
  const ISSUES = [
    { code: "clock_skew", node: "n3", since: "2026-10-01T09:30:00Z" },
    { code: "agent_offline", node: "n2", since: "2026-10-01T09:00:00Z" },
  ];

  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  it("sits on the dashboard of a degraded environment with the worst issue, and goes when the stream says online", async () => {
    grant("environments.read", "environments.manage");
    withAgentEnv({ state: "degraded", reason: "agent_offline", issues: ISSUES });
    renderAt("/env/e1");
    expect(await screen.findByText("Lab host is degraded")).toBeTruthy();
    expect(screen.getByText("The agent on n2 isn't connected.")).toBeTruthy();
    expect(screen.getByText("and 1 more issue")).toBeTruthy();
    expect(screen.getByRole("link", { name: "View nodes" }).getAttribute("href")).toBe(
      "/settings/environments/e1",
    );
    // The page itself is still there under it.
    expect(await screen.findByRole("heading", { name: "Lab host" })).toBeTruthy();

    await waitFor(() =>
      expect(FakeEventSource.instances.some((s) => s.url === "/api/environments/stream")).toBe(
        true,
      ),
    );
    const stream = FakeEventSource.instances.find((s) => s.url === "/api/environments/stream");
    act(() => {
      stream?.open();
      stream?.sendFrame(
        "env.status",
        {
          envId: "e1",
          at: "2026-10-01T14:10:00Z",
          environment: { ...AGENT, status: { state: "online" } },
        },
        "9",
      );
    });
    await waitFor(() => expect(screen.queryByText("Lab host is degraded")).toBeNull());
    expect(screen.getByRole("heading", { name: "Lab host" })).toBeTruthy();
  });

  it("shows no banner for an online environment", async () => {
    grant("environments.read");
    withAgentEnv({ state: "online" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(screen.queryByText(/is degraded/)).toBeNull();
  });
});
