// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { WaitingPanel } from "@/features/environments/waiting-panel";
import "@/i18n";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
}));

const URL_ = "wss://bayplan.example.com/agent/connect";
const KEY = environmentsQuery.queryKey;

function item(extra: Partial<EnvironmentItem> = {}): EnvironmentItem {
  return {
    id: "e9",
    name: "edge",
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-03T10:00:00Z",
    engine: {},
    status: { state: "pending" },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    ...extra,
  };
}

let client: QueryClient;
const stream = () => {
  const s = FakeEventSource.instances[0];
  if (!s) throw new Error("no stream");
  return s;
};

function mount(props: { envName?: string } = {}) {
  return render(
    <QueryClientProvider client={client}>
      <EnvHealthStreamProvider>
        <WaitingPanel envId="e9" envName={props.envName} agentUrl={URL_} />
      </EnvHealthStreamProvider>
    </QueryClientProvider>,
  );
}

let nextId = 0;
async function frame(type: string, data: Record<string, unknown>, envId = "e9") {
  nextId += 1;
  await act(async () => {
    stream().sendFrame(type, { envId, at: new Date().toISOString(), ...data }, String(nextId));
    await vi.advanceTimersByTimeAsync(5);
  });
}

const connected = (extra: Record<string, unknown> = {}) =>
  frame("agent.connected", {
    agentId: "a1",
    node: "edge-1",
    type: "standalone",
    version: "0.1.0",
    ...extra,
  });

const refused = (extra: Record<string, unknown> = {}) =>
  frame("agent.refused", {
    code: "auth_rejected",
    mode: "enrol",
    node: "edge-1",
    ip: "203.0.113.7",
    ...extra,
  });

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  nextId = 0;
  navigate.mockClear();
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ items: [item()] })),
  );
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(KEY, { items: [item()] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("WaitingPanel", () => {
  it("is a polite live region that says it is waiting, with a spinner", () => {
    mount({ envName: "edge" });
    const region = screen.getByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(within(region).getByText("Waiting for the first agent…")).toBeTruthy();
    expect(
      within(region).getByText("This updates by itself when the agent connects."),
    ).toBeTruthy();
    expect(region.querySelector("[role=status][aria-label], svg")).toBeTruthy();
    expect(region.textContent).not.toContain("Still waiting?");
  });

  it("moves from Agent connecting… to Connected once the type is known", async () => {
    mount({ envName: "edge" });
    await connected();
    expect(screen.getByText("Agent connecting…")).toBeTruthy();
    expect(screen.queryByText("Waiting for the first agent…")).toBeNull();

    // The stream refreshes the list on agent.connected: now the type is in it.
    await act(async () => {
      client.setQueryData(KEY, { items: [item({ envType: "standalone" })] });
      await vi.advanceTimersByTimeAsync(5);
    });
    expect(screen.getByText("Connected: Standalone host, edge-1.")).toBeTruthy();
  });

  it("names a swarm manager's environment a Swarm", async () => {
    mount({ envName: "edge" });
    await connected({ type: "manager", node: "mgr-1" });
    await act(async () => {
      client.setQueryData(KEY, { items: [item({ envType: "swarm" })] });
      await vi.advanceTimersByTimeAsync(5);
    });
    expect(screen.getByText("Connected: Swarm, mgr-1.")).toBeTruthy();
  });

  it("says the environment is online, then opens it after 2 seconds", async () => {
    mount({ envName: "edge" });
    await connected();
    await frame("env.status", {
      environment: item({
        envType: "standalone",
        status: { state: "online" },
        nodes: { ready: 1, total: 1 },
      }),
    });
    const region = screen.getByRole("status");
    expect(within(region).getByText("edge is online")).toBeTruthy();
    expect(
      within(region).getByText("Detected a standalone host with 1 node. Opening it now…"),
    ).toBeTruthy();

    await advance(1900);
    expect(navigate).not.toHaveBeenCalled();
    await advance(200);
    expect(navigate).toHaveBeenCalledTimes(1);
    expect(navigate).toHaveBeenCalledWith({ to: "/env/$envId", params: { envId: "e9" } });
  });

  it("counts the nodes of a swarm and uses the name the first agent gave it", async () => {
    mount({ envName: undefined });
    await frame("env.status", {
      environment: item({
        name: "prod swarm",
        envType: "swarm",
        status: { state: "online" },
        nodes: { ready: 3, total: 3 },
      }),
    });
    expect(screen.getByText("prod swarm is online")).toBeTruthy();
    expect(screen.getByText("Detected a swarm with 3 nodes. Opening it now…")).toBeTruthy();
  });

  it("goes online from the list when the env.status event never reached it", async () => {
    mount({ envName: "edge" });
    await connected();
    // A reconnect resyncs the list; the env.status sent while it was down is gone.
    await act(async () => {
      client.setQueryData(KEY, {
        items: [
          item({
            envType: "standalone",
            status: { state: "online" },
            nodes: { ready: 1, total: 1 },
          }),
        ],
      });
      await vi.advanceTimersByTimeAsync(5);
    });
    expect(screen.getByText("edge is online")).toBeTruthy();
    await advance(2100);
    expect(navigate).toHaveBeenCalledWith({ to: "/env/$envId", params: { envId: "e9" } });
  });

  it("does not navigate when it is closed before the 2 seconds are up", async () => {
    const { unmount } = mount({ envName: "edge" });
    await frame("env.status", {
      environment: item({ envType: "standalone", status: { state: "online" }, nodes: null }),
    });
    await advance(1000);
    unmount();
    await advance(5000);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("ignores another environment's events", async () => {
    mount({ envName: "edge" });
    await frame("agent.connected", { agentId: "a2", node: "other", type: "standalone" }, "e2");
    await frame(
      "agent.refused",
      { code: "auth_rejected", mode: "enrol", node: "x", ip: "1.1.1.1" },
      "e2",
    );
    expect(screen.queryByRole("listitem")).toBeNull();
    expect(screen.queryByText("Agent connecting…")).toBeNull();
    expect(screen.getByText("Waiting for the first agent…")).toBeTruthy();
  });

  describe("refused attempts", () => {
    it("lists a refusal with its reason text, the fix hint, the time and the address", async () => {
      mount({ envName: "edge" });
      const at = new Date(2026, 9, 3, 14, 5);
      await refused({ at: at.toISOString() });
      const shown = new Intl.DateTimeFormat(undefined, {
        hour: "2-digit",
        minute: "2-digit",
      }).format(at);
      const row = screen.getByRole("listitem");
      expect(
        within(row).getByText("Refused: Bayplan rejected the agent's credentials."),
      ).toBeTruthy();
      expect(
        within(row).getByText(
          "Generate a new join token and reinstall the agent with the new command.",
        ),
      ).toBeTruthy();
      expect(within(row).getByText(`${shown} from 203.0.113.7`)).toBeTruthy();
      // The waiting heading stays: a refusal is not a connection.
      expect(screen.getByText("Waiting for the first agent…")).toBeTruthy();
    });

    it("shows the newest five, newest first, and counts the rest", async () => {
      mount({ envName: "edge" });
      for (let n = 1; n <= 7; n++) await refused({ ip: `203.0.113.${n}` });
      const rows = screen.getAllByRole("listitem");
      expect(rows).toHaveLength(5);
      expect(rows.map((r) => /from (\S+)/.exec(r.textContent ?? "")?.[1])).toEqual([
        "203.0.113.7",
        "203.0.113.6",
        "203.0.113.5",
        "203.0.113.4",
        "203.0.113.3",
      ]);
      expect(screen.getByText("and 2 more attempts")).toBeTruthy();
    });

    it("says 1 more attempt for six, and nothing for five", async () => {
      mount({ envName: "edge" });
      for (let n = 1; n <= 5; n++) await refused({ ip: `203.0.113.${n}` });
      expect(screen.queryByText(/more attempt/)).toBeNull();
      await refused({ ip: "203.0.113.6" });
      expect(screen.getByText("and 1 more attempt")).toBeTruthy();
    });

    it("explains a host that is already enrolled elsewhere without naming the wrong environment", async () => {
      mount({ envName: "edge" });
      await refused({ code: "duplicate_environment", node: "h1" });
      const row = screen.getByRole("listitem");
      expect(
        within(row).getByText("Refused: h1 already belongs to another environment."),
      ).toBeTruthy();
      expect(row.textContent).not.toContain("belongs to edge");
    });

    it("gives a code it does not know a generic line", async () => {
      mount({ envName: "edge" });
      await refused({ code: "rate_limited" });
      expect(screen.getByText("Refused: Connection problem (rate_limited)")).toBeTruthy();
    });

    it("shows a hostile node name and address as text only", async () => {
      mount({ envName: "edge" });
      await refused({
        code: "clock_skew",
        node: "<img src=x onerror=alert(1)>",
        ip: "<script>alert(1)</script>",
      });
      const row = screen.getByRole("listitem");
      expect(row.querySelector("img, script")).toBeNull();
      expect(row.textContent).toContain("<script>alert(1)</script>");
    });
  });

  describe("still waiting", () => {
    it("adds the troubleshooting line with the agent URL and the log commands after 2 minutes", async () => {
      mount({ envName: "edge" });
      await advance(119_000);
      expect(screen.queryByText(/Still waiting\?/)).toBeNull();
      await advance(2_000);
      const line = screen.getByText(/Still waiting\?/);
      expect(line.textContent).toBe(
        `Still waiting? Check that the host can reach ${URL_}, then look at the agent's log: docker logs bayplan-agent (standalone) or docker service logs bayplan-agent (swarm).`,
      );
      expect(Array.from(line.querySelectorAll("code")).map((c) => c.textContent)).toEqual([
        "docker logs bayplan-agent",
        "docker service logs bayplan-agent",
      ]);
    });

    it("never times out and never deletes the environment", async () => {
      mount({ envName: "edge" });
      await advance(60 * 60_000);
      expect(screen.getByText("Waiting for the first agent…")).toBeTruthy();
      expect(navigate).not.toHaveBeenCalled();
      const calls = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls;
      expect(calls.filter(([req]) => (req as Request).method === "DELETE")).toEqual([]);
    });

    it("drops the line once an agent has connected", async () => {
      mount({ envName: "edge" });
      await advance(121_000);
      expect(screen.getByText(/Still waiting\?/)).toBeTruthy();
      await connected();
      expect(screen.queryByText(/Still waiting\?/)).toBeNull();
    });
  });
});
