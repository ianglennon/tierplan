// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { WebSocketLike } from "@/features/terminal/terminal-socket";

// A browser WebSocket that a test drives by hand. It records what the page sends
// (binary frames as Uint8Array, text frames as strings) and delivers what the test
// pushes, the way a socket with binaryType "arraybuffer" would.
export class FakeWebSocket implements WebSocketLike {
  static instances: FakeWebSocket[] = [];
  static reset(): void {
    FakeWebSocket.instances = [];
  }

  binaryType: BinaryType = "blob";
  readyState = 0;
  closedByPage = false;
  sent: Array<string | Uint8Array> = [];
  private listeners = new Map<string, Array<(event: Event) => void>>();

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  send(data: string | Uint8Array): void {
    this.sent.push(typeof data === "string" ? data : new Uint8Array(data));
  }

  close(): void {
    this.closedByPage = true;
    this.readyState = 3;
  }

  private emit(event: Event): void {
    for (const l of this.listeners.get(event.type) ?? []) l(event);
  }

  // The server accepted the upgrade.
  open(): void {
    this.readyState = 1;
    this.emit(new Event("open"));
  }

  // The server sent terminal output.
  output(bytes: Uint8Array | string): void {
    const u8 = typeof bytes === "string" ? new TextEncoder().encode(bytes) : bytes;
    const buf = u8.buffer.slice(u8.byteOffset, u8.byteOffset + u8.byteLength) as ArrayBuffer;
    this.emit(new MessageEvent("message", { data: buf }));
  }

  // The server sent a control frame.
  control(frame: unknown): void {
    this.emit(new MessageEvent("message", { data: JSON.stringify(frame) }));
  }

  // The connection ended without the page asking.
  drop(): void {
    this.readyState = 3;
    this.emit(new Event("close"));
  }

  // The text frames the page sent, parsed.
  controls(): unknown[] {
    return this.sent.filter((s): s is string => typeof s === "string").map((s) => JSON.parse(s));
  }

  // The binary frames the page sent, as text.
  inputText(): string {
    return this.sent
      .filter((s): s is Uint8Array => typeof s !== "string")
      .map((b) => new TextDecoder().decode(b))
      .join("");
  }
}
