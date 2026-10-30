// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { beforeEach, describe, expect, it, vi } from "vitest";

import { FakeWebSocket } from "@/features/terminal/fake-web-socket";
import { createTerminalSocket, type TerminalControl } from "@/features/terminal/terminal-socket";

// The terminal's framing (plan 04-22, RESEARCH Pattern 5): raw bytes in binary frames
// both ways, JSON control in text frames.

beforeEach(() => FakeWebSocket.reset());

function setup() {
  const data: Uint8Array[] = [];
  const controls: TerminalControl[] = [];
  const closes: string[] = [];
  let opened = 0;
  const sock = createTerminalSocket({
    url: "ws://x/ws",
    createSocket: (url) => new FakeWebSocket(url),
    onOpen: () => {
      opened += 1;
    },
    onData: (b) => data.push(b),
    onControl: (c) => controls.push(c),
    onClose: (how) => closes.push(how),
  });
  const ws = FakeWebSocket.instances[0] as FakeWebSocket;
  return { sock, ws, data, controls, closes, opened: () => opened };
}

describe("createTerminalSocket", () => {
  it("asks the browser for binary frames as ArrayBuffers", () => {
    const { ws } = setup();
    expect(ws.binaryType).toBe("arraybuffer");
    expect(ws.url).toBe("ws://x/ws");
  });

  it("sends typed input as a binary frame and a resize as a JSON text frame", () => {
    const { sock, ws } = setup();
    ws.open();
    sock.sendInput(new TextEncoder().encode("ls\n"));
    sock.sendResize(100, 30);
    expect(ws.sent).toHaveLength(2);
    const [input, resize] = ws.sent;
    expect(input).toBeInstanceOf(Uint8Array);
    expect(new TextDecoder().decode(input as Uint8Array)).toBe("ls\n");
    expect(JSON.parse(resize as string)).toEqual({ type: "resize", cols: 100, rows: 30 });
  });

  it("sends nothing before the socket is open", () => {
    const { sock, ws } = setup();
    sock.sendInput(new Uint8Array([97]));
    sock.sendResize(80, 24);
    expect(ws.sent).toEqual([]);
  });

  it("hands incoming binary frames to onData as bytes, exactly as sent", () => {
    const { ws, data, opened } = setup();
    ws.open();
    ws.output(new Uint8Array([0x68, 0x69, 0xe2, 0x82, 0xac]));
    expect(opened()).toBe(1);
    expect(data).toHaveLength(1);
    expect(data[0]).toBeInstanceOf(Uint8Array);
    expect(Array.from(data[0] as Uint8Array)).toEqual([0x68, 0x69, 0xe2, 0x82, 0xac]);
  });

  it("hands exit and closed frames to onControl", () => {
    const { ws, controls } = setup();
    ws.open();
    ws.control({ type: "exit", code: 2 });
    ws.control({ type: "closed", reason: "server_shutdown" });
    ws.control({ type: "idle_warning", seconds: 60 });
    expect(controls).toEqual([
      { type: "exit", code: 2 },
      { type: "closed", reason: "server_shutdown" },
      { type: "idle_warning", seconds: 60 },
    ]);
  });

  it("ignores text frames that are not control it knows", () => {
    const { ws, controls, data } = setup();
    ws.open();
    ws.control({ type: "hello" });
    ws.control({ type: "exit" });
    ws.control("just a string");
    ws.control(null);
    ws.output("");
    const raw = new MessageEvent("message", { data: "{not json" });
    (ws as unknown as { emit(e: Event): void }).emit(raw);
    expect(controls).toEqual([]);
    expect(data.every((b) => b.length === 0)).toBe(true);
  });

  it("reports a close after an exit or a closed frame as ended", () => {
    for (const frame of [
      { type: "exit", code: 0 },
      { type: "closed", reason: "no_shell" },
    ]) {
      FakeWebSocket.reset();
      const { ws, closes } = setup();
      ws.open();
      ws.control(frame);
      ws.drop();
      expect(closes).toEqual(["ended"]);
    }
  });

  it("reports a close with neither as disconnected, once", () => {
    const { ws, closes } = setup();
    ws.open();
    ws.drop();
    ws.drop();
    expect(closes).toEqual(["disconnected"]);
  });

  it("reports a socket that closed before it opened as disconnected", () => {
    const { ws, closes, opened } = setup();
    ws.drop();
    expect(opened()).toBe(0);
    expect(closes).toEqual(["disconnected"]);
  });

  it("an idle warning alone does not make the close an ended one", () => {
    const { ws, closes } = setup();
    ws.open();
    ws.control({ type: "idle_warning", seconds: 60 });
    ws.drop();
    expect(closes).toEqual(["disconnected"]);
  });

  it("close() ends the connection and reports nothing after it", () => {
    const { sock, ws, closes, data, controls } = setup();
    ws.open();
    sock.close();
    sock.close();
    expect(ws.closedByPage).toBe(true);
    ws.output("late");
    ws.control({ type: "exit", code: 1 });
    ws.drop();
    sock.sendInput(new Uint8Array([1]));
    expect(closes).toEqual([]);
    expect(data).toEqual([]);
    expect(controls).toEqual([]);
    expect(ws.sent).toEqual([]);
  });

  it("never writes what it carries to the console", () => {
    const spies = (["log", "info", "warn", "error", "debug"] as const).map((m) =>
      vi.spyOn(console, m).mockImplementation(() => {}),
    );
    const { sock, ws } = setup();
    ws.open();
    sock.sendInput(new TextEncoder().encode("hunter2"));
    ws.output("hunter2");
    ws.control({ type: "exit", code: 0 });
    ws.drop();
    for (const spy of spies) expect(spy).not.toHaveBeenCalled();
    vi.restoreAllMocks();
  });
});
