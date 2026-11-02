// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The terminal's WebSocket (RESEARCH Pattern 5). Binary frames are the raw terminal
// bytes both ways; text frames are JSON control: resize from the browser, and exit
// and closed from the server. Nothing that crosses this module is logged, stored or
// handed to anything but the callbacks of the one terminal that owns the socket.

// What the server sends as a text frame. "closed" carries minutes (the idle limit) when
// the reason is idle.
export type TerminalControl =
  | { type: "exit"; code: number }
  | { type: "closed"; reason: string; minutes?: number }
  | { type: "idle_warning"; seconds: number };

// The part of a browser WebSocket this module uses, so a test can drive a fake.
export interface WebSocketLike {
  binaryType: BinaryType;
  readonly readyState: number;
  send(data: string | Uint8Array): void;
  close(code?: number, reason?: string): void;
  // open, message, close and error. A message event carries the frame in data.
  addEventListener(type: string, listener: (event: Event) => void): void;
}

// How the socket ended: "ended" when the server had said so first (an exit code or a
// reason), "disconnected" when it just went away, which is a drop, a refused attach
// or a server that stopped (D-30: nothing re-attaches).
export type TerminalClose = "ended" | "disconnected";

export interface TerminalSocketOptions {
  url: string;
  onOpen?: () => void;
  // Output of the process, as the bytes the server read.
  onData: (bytes: Uint8Array) => void;
  onControl: (control: TerminalControl) => void;
  // Called once, when the socket has closed and not because close() was called.
  onClose: (how: TerminalClose) => void;
  // The real WebSocket unless a test supplies one.
  createSocket?: (url: string) => WebSocketLike;
}

export interface TerminalSocket {
  sendInput: (bytes: Uint8Array) => void;
  sendResize: (cols: number, rows: number) => void;
  // Ends the connection. Nothing is reported after it, and a second call does nothing.
  close: () => void;
}

const OPEN = 1;

// A frame is an ArrayBuffer by its tag as well as its constructor, since one made in
// another realm (a worker, an iframe) is not an instance of this window's.
function isArrayBuffer(v: unknown): v is ArrayBuffer {
  return v instanceof ArrayBuffer || Object.prototype.toString.call(v) === "[object ArrayBuffer]";
}

function isControl(v: unknown): v is TerminalControl {
  if (typeof v !== "object" || v === null) return false;
  const c = v as Record<string, unknown>;
  switch (c.type) {
    case "exit":
      return typeof c.code === "number";
    case "closed":
      return typeof c.reason === "string";
    case "idle_warning":
      return typeof c.seconds === "number";
  }
  return false;
}

export function createTerminalSocket(o: TerminalSocketOptions): TerminalSocket {
  // The browser's WebSocket takes more kinds of buffer than the bytes this module sends.
  const ws = (o.createSocket ?? ((url) => new WebSocket(url) as unknown as WebSocketLike))(o.url);
  ws.binaryType = "arraybuffer";
  let done = false;
  let told = false;

  ws.addEventListener("open", () => {
    if (!done) o.onOpen?.();
  });
  ws.addEventListener("message", (event) => {
    if (done) return;
    const data: unknown = (event as MessageEvent).data;
    if (typeof data === "string") {
      let parsed: unknown;
      try {
        parsed = JSON.parse(data);
      } catch {
        return;
      }
      if (isControl(parsed)) {
        if (parsed.type !== "idle_warning") told = true;
        o.onControl(parsed);
      }
      return;
    }
    if (isArrayBuffer(data)) o.onData(new Uint8Array(data));
    else if (ArrayBuffer.isView(data)) {
      o.onData(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
    }
  });
  ws.addEventListener("close", () => {
    if (done) return;
    done = true;
    o.onClose(told ? "ended" : "disconnected");
  });

  return {
    sendInput(bytes) {
      if (!done && ws.readyState === OPEN) ws.send(bytes);
    },
    sendResize(cols, rows) {
      if (!done && ws.readyState === OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
    },
    close() {
      if (done) return;
      done = true;
      ws.close(1000);
    },
  };
}
