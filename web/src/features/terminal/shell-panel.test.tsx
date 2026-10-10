// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeWebSocket } from "@/features/terminal/fake-web-socket";
import { ShellPanel } from "@/features/terminal/shell-panel";
import type { TerminalApi, TerminalProps } from "@/features/terminal/terminal";
import "@/i18n";
import { makeMe } from "@/test-me";

// The Shell tab's panel (plan 04-22, OBS-03): the launcher, the states of a session and
// every way one can end. xterm needs a real layout, so the terminal is a stand-in that
// records what the panel asks of it; the socket is a fake the test drives.

const term = vi.hoisted(() => ({
  mounts: 0,
  unmounts: 0,
  written: [] as Uint8Array[],
  focused: 0,
  props: null as TerminalProps | null,
  // When set, the stand-in registers only when told to, to test output that arrives first.
  holdRegister: false,
  registerNow: null as (() => void) | null,
}));

vi.mock("@/features/terminal/terminal", () => ({
  Terminal: (props: TerminalProps) => {
    term.props = props;
    useEffect(() => {
      term.mounts += 1;
      const api: TerminalApi = {
        write: (b) => term.written.push(b),
        focus: () => {
          term.focused += 1;
        },
      };
      term.registerNow = () => props.register(api);
      if (!term.holdRegister) props.register(api);
      return () => {
        term.unmounts += 1;
        props.register(null);
      };
    }, [props.register]);
    return (
      <section aria-label={`Terminal in ${props.name}`} data-readonly={String(props.readOnly)} />
    );
  },
}));

const ENV = "e1";
const text = (b: Uint8Array) => new TextDecoder().decode(b);

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}
let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let openAnswer: () => Response | Promise<Response>;
let sessionCount: number;

function session() {
  sessionCount += 1;
  return Response.json(
    {
      sessionId: `SID${sessionCount}`,
      wsPath: `/api/env/${ENV}/shell-sessions/SID${sessionCount}/ws`,
      command: ["/bin/sh"],
      user: "",
      workingDir: "",
    },
    { status: 201 },
  );
}

beforeEach(() => {
  sent = [];
  sessionCount = 0;
  me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["containers.exec"] } });
  openAnswer = session;
  term.mounts = 0;
  term.unmounts = 0;
  term.written = [];
  term.focused = 0;
  term.props = null;
  term.holdRegister = false;
  term.registerNow = null;
  FakeWebSocket.reset();
  setCsrfToken(null);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path: url.pathname, body });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/containers/web-id/shell`) return openAnswer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
});

function renderPanel(props: Partial<React.ComponentProps<typeof ShellPanel>> = {}) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <ShellPanel
          envId={ENV}
          containerId="web-id"
          nodeId="n1"
          name="web"
          running
          createSocket={(url) => new FakeWebSocket(url)}
          {...props}
        />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const posts = () => sent.filter((s) => s.method === "POST");

// permissionsLoaded waits for who the user is: until then the panel cannot tell whether
// the shell is theirs to open, and a click is ignored.
async function permissionsLoaded() {
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
  await act(async () => {});
}

// clickOpen clicks Open shell once the permission has loaded.
async function clickOpen() {
  const button = await screen.findByRole("button", { name: "Open shell" });
  await permissionsLoaded();
  fireEvent.click(button);
}

// openShell clicks Open shell and waits for the socket.
async function openShell(): Promise<FakeWebSocket> {
  await clickOpen();
  await waitFor(() => expect(FakeWebSocket.instances.length).toBeGreaterThan(0));
  return FakeWebSocket.instances[FakeWebSocket.instances.length - 1] as FakeWebSocket;
}

describe("the launcher", () => {
  it("offers a shell before any session: the heading, what is and is not recorded, and one accent button", async () => {
    renderPanel();
    expect(await screen.findByRole("heading", { name: "Open a shell in web" })).toBeTruthy();
    expect(
      screen.getByText(
        "You get a terminal inside the container. Nothing you type or see is recorded. Bayplan records that the session happened.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open shell" }).getAttribute("data-variant")).toBe(
      "default",
    );
    expect(screen.queryByLabelText("Terminal in web")).toBeNull();
    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it("names the node on a swarm, and not on a standalone host", async () => {
    renderPanel({ node: "node-1" });
    expect(
      await screen.findByText(
        "You get a terminal inside the container, on node-1. Nothing you type or see is recorded. Bayplan records that the session happened.",
      ),
    ).toBeTruthy();
  });

  it("is replaced by the Not running panel when the container is stopped", async () => {
    renderPanel({ running: false });
    expect(
      await screen.findByRole("heading", { name: "Start the container to open a shell" }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
  });

  it("is unavailable with the reason for a caller without the shell permission, and opens nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    await permissionsLoaded();
    const button = screen.getByRole("button", { name: "Open shell" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(document.body.textContent).toContain("Needs the container shell permission");
    fireEvent.click(button);
    await act(async () => {});
    expect(posts()).toHaveLength(0);
  });
});

describe("opening a shell", () => {
  it("shows Opening shell… with the launcher still in view while the server starts it", async () => {
    let release: (r: Response) => void = () => {};
    openAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    renderPanel();
    await clickOpen();
    const pending = await screen.findByRole("button", { name: /Opening shell/ });
    expect(pending.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("heading", { name: "Open a shell in web" })).toBeTruthy();
    expect(posts()[0]?.body).toEqual({ nodeId: "n1", shell: "auto", cols: 80, rows: 24 });
    expect(posts()[0]?.path).toBe("/api/env/e1/containers/web-id/shell");
    await act(async () => release(session()));
    await screen.findByLabelText("Terminal in web");
  });

  it("attaches to the session's WebSocket and shows the terminal with focus once it is open", async () => {
    renderPanel();
    const ws = await openShell();
    expect(ws.url).toMatch(/^ws:\/\/.*\/api\/env\/e1\/shell-sessions\/SID1\/ws$/);
    expect(await screen.findByLabelText("Terminal in web")).toBeTruthy();
    expect(screen.getByText("Connecting…")).toBeTruthy();
    act(() => ws.open());
    expect(await screen.findByText("Connected")).toBeTruthy();
    expect(term.focused).toBeGreaterThanOrEqual(1);
    expect(screen.queryByRole("heading", { name: "Open a shell in web" })).toBeNull();
  });

  it("writes output as bytes in order, sends typed keys as binary frames and the size as a text frame", async () => {
    renderPanel();
    const ws = await openShell();
    await screen.findByLabelText("Terminal in web");
    act(() => ws.open());

    act(() => {
      ws.output("prompt$ ");
      ws.output(new Uint8Array([0xe2, 0x82]));
      ws.output(new Uint8Array([0xac]));
    });
    expect(term.written.every((b) => b instanceof Uint8Array)).toBe(true);
    expect(term.written.map(text)).toEqual(["prompt$ ", "�", "�"]);
    expect(Array.from(term.written.flatMap((b) => Array.from(b)).slice(-3))).toEqual([
      0xe2, 0x82, 0xac,
    ]);

    act(() => {
      term.props?.onInput(new TextEncoder().encode("ls\n"));
      term.props?.onResize(120, 40);
    });
    expect(ws.inputText()).toBe("ls\n");
    expect(ws.controls()).toContainEqual({ type: "resize", cols: 120, rows: 40 });
  });

  it("sends the terminal's size as soon as the socket is open, when it was known first", async () => {
    renderPanel();
    const ws = await openShell();
    await screen.findByLabelText("Terminal in web");
    act(() => term.props?.onResize(132, 43));
    expect(ws.controls()).toEqual([]);
    act(() => ws.open());
    expect(ws.controls()).toEqual([{ type: "resize", cols: 132, rows: 43 }]);
  });

  it("keeps output that arrives before the terminal is up and shows it first", async () => {
    term.holdRegister = true;
    renderPanel();
    const ws = await openShell();
    await screen.findByLabelText("Terminal in web");
    act(() => {
      ws.open();
      ws.output("early ");
      ws.output("output");
    });
    expect(term.written).toEqual([]);
    act(() => term.registerNow?.());
    expect(term.written.map(text).join("")).toBe("early output");
  });

  it("says why when the server would not open it, and leaves the launcher to try again", async () => {
    openAnswer = () =>
      Response.json({ type: "host_read_only", title: "x", status: 409 }, { status: 409 });
    renderPanel();
    await clickOpen();
    expect(
      await screen.findByText(
        "This host is read-only (set on the host). Logs still work.",
        {},
        { timeout: 4000 },
      ),
    ).toBeTruthy();
    expect(FakeWebSocket.instances).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Open shell" }).hasAttribute("disabled")).toBe(false);
  });
});

describe("how a session ends", () => {
  it("a process that exited: the message with its code, the screen kept read only, and Start a new session in focus", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");

    act(() => {
      ws.output("bye\r\n");
      ws.control({ type: "exit", code: 2 });
      ws.drop();
    });
    const message = await screen.findByText("Session ended (exit code 2).");
    expect(message.closest("output")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Start a new session" });
    expect(button.getAttribute("data-variant")).toBe("default");
    await waitFor(() => expect(document.activeElement).toBe(button));
    const screenEl = screen.getByLabelText("Terminal in web");
    expect(screenEl.getAttribute("data-readonly")).toBe("true");
    expect(term.written.map(text).join("")).toContain("bye");
    expect(screen.queryByText("Disconnected")).toBeNull();
  });

  it("a dropped connection: Disconnected with its reason, and Reconnect shell in focus", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");

    act(() => ws.drop());
    expect(await screen.findByText("Disconnected")).toBeTruthy();
    expect(screen.getByText("The connection dropped, so the shell was closed.")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Reconnect shell" });
    expect(button.getAttribute("data-variant")).toBe("default");
    await waitFor(() => expect(document.activeElement).toBe(button));
    expect(screen.getByLabelText("Terminal in web").getAttribute("data-readonly")).toBe("true");
    expect(screen.getByText("Disconnected").closest("output")).toBeTruthy();
  });

  it("a socket that never opened is a disconnect too", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.drop());
    expect(await screen.findByText("Disconnected")).toBeTruthy();
  });

  it("a session the server closed (shutdown) is shown as disconnected", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    act(() => {
      ws.control({ type: "closed", reason: "server_shutdown" });
      ws.drop();
    });
    expect(await screen.findByText("Disconnected")).toBeTruthy();
  });

  it("Reconnect shell starts a new session and a new terminal; it never re-attaches", async () => {
    renderPanel();
    const first = await openShell();
    act(() => first.open());
    await screen.findByText("Connected");
    act(() => first.drop());
    fireEvent.click(await screen.findByRole("button", { name: "Reconnect shell" }));

    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    expect(posts()).toHaveLength(2);
    const second = FakeWebSocket.instances[1] as FakeWebSocket;
    expect(second.url).toContain("/shell-sessions/SID2/ws");
    expect(second.url).not.toBe(first.url);
    await screen.findByText("Connecting…");
    expect(term.mounts).toBe(2);
    expect(screen.getByLabelText("Terminal in web").getAttribute("data-readonly")).toBe("false");
    act(() => second.open());
    expect(await screen.findByText("Connected")).toBeTruthy();
  });

  it("Start a new session after an exit does the same", async () => {
    renderPanel();
    const first = await openShell();
    act(() => first.open());
    act(() => {
      first.control({ type: "exit", code: 0 });
      first.drop();
    });
    fireEvent.click(await screen.findByRole("button", { name: "Start a new session" }));
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    expect(posts()).toHaveLength(2);
  });

  it("leaving the page closes the socket", async () => {
    const { unmount } = renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    unmount();
    expect(ws.closedByPage).toBe(true);
    expect(term.unmounts).toBe(1);
  });

  it("leaving the page while the server is still starting the shell attaches to nothing", async () => {
    let release: (r: Response) => void = () => {};
    openAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    const { unmount } = renderPanel();
    await clickOpen();
    await waitFor(() => expect(posts()).toHaveLength(1));
    unmount();
    await act(async () => release(session()));
    expect(FakeWebSocket.instances).toHaveLength(0);
  });
});

describe("what is typed and printed", () => {
  it("never reaches the console, the page's storage or any request but the socket", async () => {
    const spies = (["log", "info", "warn", "error", "debug"] as const).map((m) =>
      vi.spyOn(console, m).mockImplementation(() => {}),
    );
    const storageWrites = vi.spyOn(Storage.prototype, "setItem");
    localStorage.clear();
    sessionStorage.clear();
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    const before = sent.length;

    act(() => {
      term.props?.onInput(new TextEncoder().encode("export TOKEN=hunter2\n"));
      ws.output("hunter2\r\n");
      ws.control({ type: "exit", code: 0 });
      ws.drop();
    });
    await screen.findByText("Session ended (exit code 0).");

    for (const spy of spies) {
      for (const call of spy.mock.calls) expect(JSON.stringify(call)).not.toContain("hunter2");
    }
    expect(storageWrites.mock.calls.flat().join(" ")).not.toContain("hunter2");
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    // No request carried it: the only traffic after the socket opened is the socket.
    expect(sent.slice(before)).toEqual([]);
    expect(ws.inputText()).toBe("export TOKEN=hunter2\n");
  });
});
