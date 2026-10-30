// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { EventDTO, EventSourceLike } from "@/features/dashboard/events";

// A browser EventSource that a test drives by hand. jsdom has none, so tests
// install it with vi.stubGlobal("EventSource", FakeEventSource).
export class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = [];
  static reset(): void {
    FakeEventSource.instances = [];
  }

  readyState = 0;
  closed = false;
  private listeners = new Map<string, Array<(event: Event) => void>>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  close(): void {
    this.closed = true;
    this.readyState = 2;
  }

  private emit(event: Event): void {
    for (const l of this.listeners.get(event.type) ?? []) l(event);
  }

  open(): void {
    this.readyState = 1;
    this.emit(new Event("open"));
  }

  // The browser retries by itself (readyState 0) unless it gave up (2).
  fail(final = false): void {
    this.readyState = final ? 2 : 0;
    this.emit(new Event("error"));
  }

  send(event: EventDTO): void {
    this.emit(new MessageEvent("docker", { data: JSON.stringify(event) }));
  }

  // Any named frame, with the id the server put on it (the SSE id field,
  // which a browser exposes as lastEventId). The environments stream uses it.
  sendFrame(type: string, data: unknown, lastEventId = ""): void {
    this.emit(new MessageEvent(type, { data: JSON.stringify(data), lastEventId }));
  }
}
