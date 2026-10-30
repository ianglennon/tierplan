// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  type EnvHealthEvent,
  EnvHealthStreamProvider,
  useEnvHealthEvents,
  useHealthLive,
} from "@/features/environments/health-stream";
import { waitUnderFakeClock } from "@/test-settle";

function env(id: string, state: EnvironmentItem["status"]["state"], name = id): EnvironmentItem {
  return {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
  };
}

const KEY = environmentsQuery.queryKey;
const sources = () => FakeEventSource.instances;
const first = () => {
  const s = FakeEventSource.instances[0];
  if (!s) throw new Error("no EventSource was created");
  return s;
};
const listOf = (client: QueryClient) =>
  client.getQueryData<{ items: EnvironmentItem[] }>(KEY)?.items ?? [];

let fetched: string[];
let client: QueryClient;

function Live() {
  return <p data-testid="live">{useHealthLive()}</p>;
}

function Listener({ envId, got }: { envId: string; got: EnvHealthEvent[] }) {
  useEnvHealthEvents(envId, (e) => got.push(e));
  return null;
}

function mount(children: React.ReactNode = <Live />) {
  return render(
    <QueryClientProvider client={client}>
      <EnvHealthStreamProvider>{children}</EnvHealthStreamProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  FakeEventSource.reset();
  fetched = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(KEY, { items: [env("a", "online"), env("b", "online")] });
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      fetched.push(new URL(req.url).pathname);
      return Response.json({ items: [env("a", "offline")] });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("EnvHealthStreamProvider", () => {
  it("reads the warnings again on a proxy.warning frame, which belongs to no environment", async () => {
    client.setQueryData(["warnings"], [{ id: "key_backup" }]);
    const heard: EnvHealthEvent[] = [];
    mount(<Listener envId="" got={heard} />);
    await act(async () => {
      first().open();
      first().sendFrame(
        "proxy.warning",
        { envId: "", at: "2026-10-03T10:00:00Z", kind: "url_mismatch" },
        "12",
      );
    });
    expect(client.getQueryState(["warnings"])?.isInvalidated).toBe(true);
    expect(fetched).toEqual([]);
    expect(heard).toEqual([]);
  });

  it("delivers a failover frame to the environment's listeners and reads its node list and the list again", async () => {
    client.setQueryData(["agents", "a"], {
      items: [],
      manualCleanup: { swarm: "", standalone: "" },
    });
    client.setQueryData(["environment-warnings"], []);
    const heard: EnvHealthEvent[] = [];
    mount(<Listener envId="a" got={heard} />);
    await act(async () => {
      first().open();
      first().sendFrame(
        "failover",
        {
          envId: "a",
          at: "2026-10-03T10:00:00Z",
          fromAgentId: "m1",
          fromNode: "mgr-1",
          toAgentId: "m2",
          toNode: "mgr-2",
        },
        "21",
      );
    });
    expect(heard.map((e) => [e.name, e.data.fromNode, e.data.toNode])).toEqual([
      ["failover", "mgr-1", "mgr-2"],
    ]);
    expect(client.getQueryState(["agents", "a"])?.isInvalidated).toBe(true);
    expect(client.getQueryState(["environment-warnings"])?.isInvalidated).toBe(true);
    await waitUnderFakeClock(() => expect(fetched).toContain("/api/environments"));
  });

  it("reads the environment problems again on a status change, an agent coming or going, but not on a refused enrolment", async () => {
    client.setQueryData(["environment-warnings"], []);
    mount();
    await act(async () => {
      first().open();
      first().sendFrame(
        "agent.refused",
        {
          envId: "a",
          at: "2026-10-03T10:00:00Z",
          code: "auth_rejected",
          mode: "enrol",
          node: "n",
          ip: "10.0.0.1",
        },
        "30",
      );
    });
    expect(client.getQueryState(["environment-warnings"])?.isInvalidated).toBe(false);
    await act(async () => {
      first().sendFrame(
        "agent.disconnected",
        { envId: "a", at: "2026-10-03T10:00:01Z", agentId: "x", node: "n", reason: "timeout" },
        "31",
      );
    });
    expect(client.getQueryState(["environment-warnings"])?.isInvalidated).toBe(true);
  });

  it("opens one EventSource to the environments stream for the whole app", () => {
    mount(
      <>
        <Live />
        <Live />
      </>,
    );
    expect(sources()).toHaveLength(1);
    expect(first().url).toBe("/api/environments/stream");
  });

  it("replaces the matching environment in the cache on env.status without a refetch", async () => {
    mount();
    await act(async () => {
      first().open();
      first().sendFrame(
        "env.status",
        {
          envId: "a",
          at: "2026-10-03T10:00:00Z",
          environment: env("a", "offline"),
        },
        "7",
      );
    });
    const items = listOf(client);
    expect(items.map((e) => [e.id, e.status.state])).toEqual([
      ["a", "offline"],
      ["b", "online"],
    ]);
    expect(fetched).toEqual([]);
  });

  it("ignores an event id it has already seen", async () => {
    mount();
    const frame = (state: EnvironmentItem["status"]["state"]) => ({
      envId: "a",
      at: "2026-10-03T10:00:00Z",
      environment: env("a", state),
    });
    await act(async () => {
      first().open();
      first().sendFrame("env.status", frame("offline"), "7");
      first().sendFrame("env.status", frame("degraded"), "7");
    });
    expect(listOf(client)[0]?.status.state).toBe("offline");
    // A new id is applied.
    await act(async () => {
      first().sendFrame("env.status", frame("degraded"), "8");
    });
    expect(listOf(client)[0]?.status.state).toBe("degraded");
  });

  it("takes the snapshot as the whole list, with or without an id", async () => {
    mount();
    await act(async () => {
      first().open();
      first().sendFrame("snapshot", { items: [env("c", "pending"), env("a", "online")] });
    });
    expect(listOf(client).map((e) => e.id)).toEqual(["c", "a"]);
  });

  it("reads the list again for an environment it does not know", async () => {
    mount();
    await act(async () => {
      first().open();
      first().sendFrame(
        "env.status",
        { envId: "z", at: "2026-10-03T10:00:00Z", environment: env("z", "pending") },
        "9",
      );
    });
    await waitUnderFakeClock(() => expect(fetched).toContain("/api/environments"));
  });

  it("reads Live once the stream opens, Reconnecting… while it retries, and polling after it gives up", async () => {
    mount();
    expect(screen.getByTestId("live").textContent).toBe("connecting");
    await act(async () => first().open());
    expect(screen.getByTestId("live").textContent).toBe("live");
    await act(async () => first().fail());
    expect(screen.getByTestId("live").textContent).toBe("reconnecting");
    await act(async () => first().open());
    expect(screen.getByTestId("live").textContent).toBe("live");
    // The browser gave up (readyState CLOSED).
    await act(async () => first().fail(true));
    expect(screen.getByTestId("live").textContent).toBe("polling");
    expect(first().closed).toBe(true);
  });

  it("gives up after three errors inside the window", async () => {
    mount();
    await act(async () => {
      first().fail();
      first().fail();
    });
    expect(screen.getByTestId("live").textContent).toBe("reconnecting");
    await act(async () => first().fail());
    expect(screen.getByTestId("live").textContent).toBe("polling");
  });

  it("polls the list every 30 seconds while the stream is given up", async () => {
    vi.useFakeTimers();
    mount();
    await act(async () => first().fail(true));
    const count = () => fetched.filter((p) => p === "/api/environments").length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100);
    });
    const start = count();
    expect(start).toBeGreaterThanOrEqual(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(count()).toBe(start + 1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(count()).toBe(start + 2);
  });

  it("does not poll while the stream is live", async () => {
    vi.useFakeTimers();
    mount();
    await act(async () => first().open());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(90_000);
    });
    expect(fetched).toEqual([]);
  });

  it("tries the stream again after a while and goes back to live when it opens", async () => {
    vi.useFakeTimers();
    mount();
    await act(async () => first().fail(true));
    expect(sources()).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(121_000);
    });
    expect(sources()).toHaveLength(2);
    await act(async () => sources()[1]?.open());
    expect(screen.getByTestId("live").textContent).toBe("live");
    const before = fetched.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(90_000);
    });
    expect(fetched.length).toBe(before);
  });

  it("hands agent events to a subscriber of that environment only", async () => {
    const forA: EnvHealthEvent[] = [];
    const forB: EnvHealthEvent[] = [];
    mount(
      <>
        <Listener envId="a" got={forA} />
        <Listener envId="b" got={forB} />
      </>,
    );
    await act(async () => {
      first().open();
      first().sendFrame(
        "agent.refused",
        {
          envId: "a",
          at: "2026-10-03T10:00:00Z",
          code: "auth_rejected",
          mode: "enrol",
          ip: "10.0.0.9",
        },
        "11",
      );
    });
    expect(forB).toEqual([]);
    expect(forA).toHaveLength(1);
    expect(forA[0]?.name).toBe("agent.refused");
    expect(forA[0]?.data.code).toBe("auth_rejected");
    expect(forA[0]?.id).toBe("11");
  });

  it("refreshes the counts when an agent connects or leaves", async () => {
    mount();
    await act(async () => {
      first().open();
      first().sendFrame(
        "agent.connected",
        { envId: "a", at: "2026-10-03T10:00:00Z", agentId: "g1", node: "n1", type: "manager" },
        "12",
      );
    });
    await waitUnderFakeClock(() => expect(fetched).toContain("/api/environments"));
  });

  it("stops listening when a subscriber unmounts", async () => {
    const got: EnvHealthEvent[] = [];
    const view = mount(<Listener envId="a" got={got} />);
    await act(async () => first().open());
    view.rerender(
      <QueryClientProvider client={client}>
        <EnvHealthStreamProvider />
      </QueryClientProvider>,
    );
    await act(async () => {
      first().sendFrame("agent.refused", { envId: "a", at: "t", code: "x" }, "13");
    });
    expect(got).toEqual([]);
  });

  it("closes the stream on unmount", () => {
    const view = mount();
    view.unmount();
    expect(first().closed).toBe(true);
  });
});
