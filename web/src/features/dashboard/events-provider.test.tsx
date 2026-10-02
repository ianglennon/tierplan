// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { EnvEventsProvider, useEnvEvents } from "@/features/dashboard/events";

const instances: Array<{
  url: string;
  closed: boolean;
  fire: (type: string, data?: string) => void;
}> = [];

class StubEventSource {
  readyState = 0;
  closed = false;
  private listeners = new Map<string, Array<(e: Event) => void>>();
  constructor(readonly url: string) {
    instances.push(this);
  }
  addEventListener(type: string, fn: (e: Event) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }
  close() {
    this.closed = true;
  }
  fire(type: string, data?: string) {
    if (type === "open") this.readyState = 1;
    const event = data === undefined ? new Event(type) : new MessageEvent(type, { data });
    for (const fn of this.listeners.get(type) ?? []) fn(event);
  }
}

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
  instances.length = 0;
  vi.stubGlobal("EventSource", StubEventSource);
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
    expect(instances).toHaveLength(1);
    expect(instances[0]?.url).toBe("/api/env/e1/events/stream");

    await act(async () => {
      instances[0]?.fire("open");
      instances[0]?.fire(
        "docker",
        JSON.stringify({
          id: "1",
          time: "2026-10-02T10:00:00Z",
          type: "volume",
          action: "create",
          actorId: "t1",
          name: "t1",
          attributes: {},
        }),
      );
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
    expect(instances).toHaveLength(2);
    expect(instances[0]?.closed).toBe(true);
    expect(instances[1]?.url).toBe("/api/env/e2/events/stream");
    view.unmount();
    expect(instances[1]?.closed).toBe(true);
  });
});
