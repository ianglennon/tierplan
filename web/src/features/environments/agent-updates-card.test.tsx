// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { environmentsQuery } from "@/api/environments";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AgentUpdatesCard } from "@/features/environments/agent-updates-card";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

const NOW = new Date("2026-10-03T12:00:00Z");

function agent(id: string, node: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    node,
    role: "worker",
    status: "active",
    blocked: false,
    connected: true,
    agentVersion: "0.1.0",
    engineVersion: "29.8.2",
    certFingerprint: "ab".repeat(32),
    certNotAfter: "2027-01-01T00:00:00Z",
    clockOffsetSeconds: 0,
    lastSeen: "2026-10-03T11:59:00Z",
    readOnly: false,
    selfUpdate: true,
    updateAvailable: true,
    updateState: { state: "idle" } as { state: string; reason?: string },
    ...extra,
  };
}

function environment(autoUpdate: boolean, envType = "swarm") {
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
    envType,
    updateAvailable: true,
    autoUpdate,
  };
}

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

let sent: Sent[];
let autoUpdate: boolean;
let envType: string;
let agents: ReturnType<typeof agent>[];
let patchAnswer: () => Response | Promise<Response>;
let updateAllAnswer: () => Response | Promise<Response>;
let client: QueryClient;
let me = makeMe({ instanceAdmin: true });

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  sent = [];
  autoUpdate = true;
  envType = "swarm";
  agents = [
    agent("a1", "mgr-1", { role: "manager" }),
    agent("a2", "wrk-1"),
    agent("a3", "wrk-2"),
    agent("a4", "done-1", { updateAvailable: false, agentVersion: "0.2.1" }),
  ];
  patchAnswer = () => {
    return new Response(null, { status: 200 });
  };
  updateAllAnswer = () => Response.json({ started: 1 }, { status: 202 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "PATCH" || req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments" && req.method === "GET") {
        return Response.json({ items: [environment(autoUpdate, envType)] });
      }
      if (path === "/api/env/e2" && req.method === "PATCH") {
        const res = await patchAnswer();
        if (res.ok) autoUpdate = (body as { autoUpdate: boolean }).autoUpdate;
        return res;
      }
      if (path === "/api/env/e2/agents") {
        return Response.json({ items: agents, manualCleanup: { swarm: "", standalone: "" } });
      }
      if (path === "/api/env/e2/update-agents") return updateAllAnswer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
});

// The card is given the environment the page reads, so the harness reads it too.
function Harness() {
  const environments = useQuery(environmentsQuery);
  const env = environments.data?.items?.find((e) => e.id === "e2");
  return env ? <AgentUpdatesCard env={env} /> : null;
}

async function mount() {
  client.setQueryData(meQueryOptions.queryKey, me);
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <Harness />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await screen.findByRole("switch", { name: "Update agents automatically" });
  await waitFor(() => expect(sent.some((s) => s.path === "/api/env/e2/agents")).toBe(true));
}

const posts = () => sent.filter((s) => s.method === "POST");
const reads = () => sent.filter((s) => s.method === "GET" && s.path === "/api/env/e2/agents");
const updateAll = () => screen.queryByRole("button", { name: /^Update all agents/ });

describe("AgentUpdatesCard permission", () => {
  it("shows to an env Admin of that environment", async () => {
    me = makeMe({ envs: { e2: "admin" } });
    await mount();
    expect(screen.getByRole("heading", { level: 3, name: "Agent updates" })).toBeTruthy();
  });

  it("renders nothing for a Viewer of the environment, or an Admin of another one", async () => {
    for (const who of [makeMe({ envs: { e2: "viewer" } }), makeMe({ envs: { e9: "admin" } })]) {
      me = who;
      client.clear();
      client.setQueryData(meQueryOptions.queryKey, me);
      render(
        <QueryClientProvider client={client}>
          <TooltipProvider>
            <Harness />
          </TooltipProvider>
        </QueryClientProvider>,
      );
      await waitFor(() => expect(sent.some((s) => s.path === "/api/environments")).toBe(true));
      expect(screen.queryByRole("heading", { name: "Agent updates" })).toBeNull();
      expect(screen.queryByRole("switch")).toBeNull();
      cleanup();
    }
  });
});

describe("AgentUpdatesCard", () => {
  it("has the switch on with the helper, and no update buttons, while automatic updates are on", async () => {
    await mount();
    const sw = screen.getByRole("switch", { name: "Update agents automatically" });
    expect(sw.getAttribute("aria-checked")).toBe("true");
    const helper =
      "After a TierPlan upgrade, agents here are updated within the compatibility window. Turn this off to update them yourself.";
    expect(screen.getByText(helper)).toBeTruthy();
    expect(sw.getAttribute("aria-describedby")).toBe(screen.getByText(helper).id);
    expect(updateAll()).toBeNull();
    expect(screen.getByRole("heading", { level: 3, name: "Agent updates" })).toBeTruthy();
  });

  it("PATCHes the environment when the switch is turned off, and then offers Update all agents (3)", async () => {
    await mount();
    fireEvent.click(screen.getByRole("switch", { name: "Update agents automatically" }));
    await waitFor(() =>
      expect(sent.find((s) => s.method === "PATCH")?.body).toEqual({ autoUpdate: false }),
    );
    expect(await screen.findByRole("button", { name: "Update all agents (3)" })).toBeTruthy();
    expect(
      screen
        .getByRole("switch", { name: "Update agents automatically" })
        .getAttribute("aria-checked"),
    ).toBe("false");
  });

  it("turns automatic updates back on and hides the buttons again", async () => {
    autoUpdate = false;
    await mount();
    expect(await screen.findByRole("button", { name: "Update all agents (3)" })).toBeTruthy();
    fireEvent.click(screen.getByRole("switch", { name: "Update agents automatically" }));
    await waitFor(() =>
      expect(sent.filter((s) => s.method === "PATCH").at(-1)?.body).toEqual({ autoUpdate: true }),
    );
    await waitFor(() => expect(updateAll()).toBeNull());
  });

  it("puts the switch back and says so when the change is refused", async () => {
    patchAnswer = () => Response.json({ type: "environment_not_found" }, { status: 404 });
    await mount();
    fireEvent.click(screen.getByRole("switch", { name: "Update agents automatically" }));
    expect(await screen.findByText("Couldn't change the setting. Try again.")).toBeTruthy();
    await waitFor(() =>
      expect(
        screen
          .getByRole("switch", { name: "Update agents automatically" })
          .getAttribute("aria-checked"),
      ).toBe("true"),
    );
  });

  it("counts only connected, active agents that are behind and allowed by their host", async () => {
    autoUpdate = false;
    agents = [
      agent("a1", "mgr-1", { role: "manager" }),
      agent("a2", "off-1", { connected: false }),
      agent("a3", "refuses-1", { selfUpdate: false }),
      agent("a4", "revoked-1", { status: "revoked", blocked: true, connected: false }),
      agent("a5", "current-1", { updateAvailable: false }),
    ];
    await mount();
    expect(await screen.findByRole("button", { name: "Update all agents (1)" })).toBeTruthy();
  });

  it("disables Update all agents when there is nothing to update", async () => {
    autoUpdate = false;
    agents = [agent("a1", "mgr-1", { updateAvailable: false })];
    await mount();
    const button = await screen.findByRole("button", { name: "Update all agents (0)" });
    expect(button.hasAttribute("disabled")).toBe(true);
  });

  it("starts the updates with one request and shows Updating agents (0 of 3), advancing as nodes report their new version", async () => {
    autoUpdate = false;
    await mount();
    const button = await screen.findByRole("button", { name: "Update all agents (3)" });
    // Timers are faked only now, so that the mount above did not wait on them.
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    vi.setSystemTime(NOW);

    // The server marks every agent that is behind as updating when it sends the message.
    updateAllAnswer = () => {
      agents = agents.map((a) =>
        a.updateAvailable ? { ...a, updateState: { state: "updating" } } : a,
      );
      return Response.json({ started: 1 }, { status: 202 });
    };
    await act(async () => {
      fireEvent.click(button);
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(posts().map((p) => p.path)).toEqual(["/api/env/e2/update-agents"]);
    expect(screen.getByText("Updating agents (0 of 3)")).toBeTruthy();

    // The list is read again every few seconds while anything is updating.
    const before = reads().length;
    agents = agents.map((a) =>
      a.id === "a2"
        ? { ...a, agentVersion: "0.2.1", updateAvailable: false, updateState: { state: "idle" } }
        : a,
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3100);
    });
    expect(reads().length).toBeGreaterThan(before);
    expect(screen.getByText("Updating agents (1 of 3)")).toBeTruthy();

    agents = agents.map((a) =>
      a.updateState.state === "updating"
        ? { ...a, agentVersion: "0.2.1", updateAvailable: false, updateState: { state: "idle" } }
        : a,
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3100);
    });
    // Everything is done: the progress is gone.
    expect(screen.queryByText(/^Updating agents/)).toBeNull();
  });

  it("counts a failed node out of done and keeps the progress until the others have finished", async () => {
    autoUpdate = false;
    agents = [agent("a1", "mgr-1"), agent("a2", "wrk-1")];
    await mount();
    await screen.findByRole("button", { name: "Update all agents (2)" });
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    vi.setSystemTime(NOW);
    agents = [
      agent("a1", "mgr-1", { updateState: { state: "updating" } }),
      agent("a2", "wrk-1", { updateState: { state: "updating" } }),
    ];
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["agents", "e2"] });
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(screen.getByText("Updating agents (0 of 2)")).toBeTruthy();
    agents = [
      agent("a1", "mgr-1", { updateState: { state: "failed", reason: "no_new_version" } }),
      agent("a2", "wrk-1", { updateState: { state: "updating" } }),
    ];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3100);
    });
    expect(screen.getByText("Updating agents (0 of 2)")).toBeTruthy();
  });

  it("shows progress for an update that was started before the page was opened", async () => {
    autoUpdate = false;
    agents = [
      agent("a1", "mgr-1", { updateState: { state: "updating" } }),
      agent("a2", "wrk-1", { updateState: { state: "updating" } }),
      agent("a3", "wrk-2", { updateState: { state: "updating" } }),
    ];
    await mount();
    expect(await screen.findByText("Updating agents (0 of 3)")).toBeTruthy();
  });

  it("explains a refused request in a toast: no manager is connected", async () => {
    autoUpdate = false;
    updateAllAnswer = () => Response.json({ type: "no_manager_connected" }, { status: 409 });
    await mount();
    fireEvent.click(await screen.findByRole("button", { name: "Update all agents (3)" }));
    expect(
      await screen.findByText(
        "No manager of this swarm is connected, and a swarm's agents are updated through a manager.",
      ),
    ).toBeTruthy();
    // And it is read again, so the card does not keep a stale view.
    await waitFor(() => expect(reads().length).toBeGreaterThan(1));
  });

  it("disables the button and says Starting… while the request is out, and sends it once", async () => {
    autoUpdate = false;
    let release: (r: Response) => void = () => {};
    updateAllAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    await mount();
    fireEvent.click(await screen.findByRole("button", { name: "Update all agents (3)" }));
    const pending = await screen.findByRole("button", { name: "Starting…" });
    expect(pending.hasAttribute("disabled")).toBe(true);
    fireEvent.click(pending);
    expect(posts()).toHaveLength(1);
    release(Response.json({ started: 1 }, { status: 202 }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Starting…" })).toBeNull());
  });

  it("says that one update rolls every node of a swarm, and not for a standalone host", async () => {
    autoUpdate = false;
    await mount();
    expect(
      await screen.findByText(
        "In a swarm one update rolls every node's agent, one after the other, so Update agent on one node updates them all.",
      ),
    ).toBeTruthy();
    cleanup();
    envType = "standalone";
    await mount();
    await screen.findByRole("button", { name: /^Update all agents/ });
    expect(screen.queryByText(/In a swarm one update/)).toBeNull();
  });

  it("gives the command to update by hand, with a copy button, for a host that turned self-update off", async () => {
    agents = [
      agent("a1", "mgr-1", { role: "manager", updateAvailable: false }),
      agent("a2", "locked-1", {
        selfUpdate: false,
        manualUpdateCommand:
          " docker service update --image ghcr.io/ianglennon/tierplan-agent:0.2.1 tierplan-agent",
      }),
    ];
    await mount();
    expect(
      await screen.findByText("Self-update is turned off on locked-1. Update it by hand:"),
    ).toBeTruthy();
    expect(
      screen.getByText(/docker service update --image ghcr\.io\/ianglennon\/tierplan-agent:0\.2\.1/),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Copy the update command for locked-1" }),
    ).toBeTruthy();
    expect(screen.queryByText(/mgr-1/)).toBeNull();
  });

  it("words the note differently for an agent TierPlan cannot update for another reason", async () => {
    agents = [
      agent("a2", "ancient-1", {
        agentVersion: "0.0.9",
        manualUpdateCommand: " docker service update --image x y",
      }),
    ];
    await mount();
    const note = await screen.findByText(
      "TierPlan can't update ancient-1 by itself. Update it by hand:",
    );
    expect(note.closest("div")?.textContent).toContain("docker service update --image x y");
    expect(within(note.closest("div") as HTMLElement).getByRole("button")).toBeTruthy();
  });
});
