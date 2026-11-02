// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { EnvEventsProvider, useEnvEvents } from "@/features/dashboard/events";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";

const instances = () => FakeEventSource.instances;

function Probe() {
  const { events, state } = useEnvEvents();
  return (
    <p>
      {state}:{events.map((e) => e.name).join(",")}
    </p>
  );
}

function mount(envId: string, client: QueryClient) {
  return (
    <QueryClientProvider client={client}>
      <EnvEventsProvider envId={envId}>
        <Probe />
      </EnvEventsProvider>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ items: [] })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("EnvEventsProvider", () => {
  it("opens one stream per environment, shows its events and invalidates the matching queries", async () => {
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(mount("e1", client));
    expect(instances()).toHaveLength(1);
    expect(instances()[0]?.url).toBe("/api/env/e1/events/stream");

    await act(async () => {
      instances()[0]?.open();
      instances()[0]?.send({
        id: "1",
        node: "node-1",
        time: "2026-10-02T10:00:00Z",
        type: "volume",
        action: "create",
        actorId: "t1",
        name: "t1",
        attributes: {},
      });
    });
    expect(await screen.findByText("live:t1")).toBeTruthy();
    const keys = invalidate.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["env", "e1", "summary"]);
    expect(keys).toContainEqual(["env", "e1", "volumes"]);
  });

  it("closes the stream on unmount and reopens it for another environment", async () => {
    const client = new QueryClient();
    const view = render(mount("e1", client));
    view.rerender(mount("e2", client));
    expect(instances()).toHaveLength(2);
    expect(instances()[0]?.closed).toBe(true);
    expect(instances()[1]?.url).toBe("/api/env/e2/events/stream");
    view.unmount();
    expect(instances()[1]?.closed).toBe(true);
  });

  it("opens nothing until it is enabled", () => {
    const client = new QueryClient();
    const view = render(
      <QueryClientProvider client={client}>
        <EnvEventsProvider envId="e1" enabled={false}>
          <Probe />
        </EnvEventsProvider>
      </QueryClientProvider>,
    );
    expect(instances()).toHaveLength(0);
    view.rerender(
      <QueryClientProvider client={client}>
        <EnvEventsProvider envId="e1" enabled>
          <Probe />
        </EnvEventsProvider>
      </QueryClientProvider>,
    );
    expect(instances()).toHaveLength(1);
  });
});
