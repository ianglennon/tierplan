// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import type { ContainerRow } from "@/api/resources";
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
  searchOpened: 0,
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
        openSearch: () => {
          term.searchOpened += 1;
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

vi.mock("@/components/agent-update-notice", () => ({
  AgentUpdateNotice: ({ envId }: { envId: string }) => (
    <div role="alert">The agent on {envId} needs an update</div>
  ),
}));

const ENV = "e1";
const text = (b: Uint8Array) => new TextDecoder().decode(b);

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}
let sent: Sent[];
// The shells the person has open, as the server lists them; null answers that it cannot.
let mineShells: Array<Record<string, unknown>> | null;
let me: ReturnType<typeof makeMe>;
let openAnswer: () => Response | Promise<Response>;
let actionAnswer: () => Response | Promise<Response>;
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
  mineShells = null;
  sessionCount = 0;
  me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["containers.exec"] } });
  openAnswer = session;
  actionAnswer = () =>
    Response.json({
      bulkId: "b1",
      results: [{ nodeId: "n1", id: "web-id", name: "web", result: "done" }],
    });
  term.mounts = 0;
  term.unmounts = 0;
  term.written = [];
  term.focused = 0;
  term.searchOpened = 0;
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
      if (url.pathname === `/api/env/${ENV}/containers/actions`) return actionAnswer();
      if (url.pathname === `/api/env/${ENV}/shells` && mineShells) {
        return Response.json({ items: mineShells });
      }
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
        "You get a terminal inside the container. Nothing you type or see is recorded. Coaming records that the session happened.",
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
        "You get a terminal inside the container, on node-1. Nothing you type or see is recorded. Coaming records that the session happened.",
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
      Response.json({ type: "stream_busy", title: "x", status: 503 }, { status: 503 });
    renderPanel();
    await clickOpen();
    expect(
      await screen.findByText(
        "This host's agent is serving as many logs and shells as it can. Close one and try again.",
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
    // No request carried it: the only traffic after the socket opened is the socket, and
    // the page asking again which shells the person has open (a GET with no body).
    const own = (x: Sent) => x.method === "GET" && x.path === `/api/env/${ENV}/shells`;
    expect(sent.slice(before).filter((x) => !own(x))).toEqual([]);
    expect(ws.inputText()).toBe("export TOKEN=hunter2\n");
  });
});

describe("a shell nobody is using (plan 04-23, D-29)", () => {
  const WARNING =
    "This session closes in 1 minute because nothing has happened in it. Press any key to keep it open.";

  it("shows the one-minute warning over the terminal without moving it, announced as a status", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    const before = screen.getByLabelText("Terminal in web");

    act(() => ws.control({ type: "idle_warning", seconds: 60 }));
    const banner = await screen.findByText(WARNING);
    expect(banner.closest("[role=status]")).toBeTruthy();
    // The terminal is the same element, still mounted once, and the banner lies over it.
    expect(screen.getByLabelText("Terminal in web")).toBe(before);
    expect(term.mounts).toBe(1);
    expect(before.parentElement?.contains(banner)).toBe(true);
    expect(banner.closest(".absolute")).toBeTruthy();
  });

  it("is dismissed by a key, and shown again by the next warning", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    act(() => ws.control({ type: "idle_warning", seconds: 60 }));
    await screen.findByText(WARNING);

    act(() => term.props?.onInput(new TextEncoder().encode("x")));
    expect(screen.queryByText(WARNING)).toBeNull();
    expect(ws.inputText()).toBe("x");

    act(() => ws.control({ type: "idle_warning", seconds: 60 }));
    expect(await screen.findByText(WARNING)).toBeTruthy();
  });

  it("is dismissed by output from the shell", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    act(() => ws.control({ type: "idle_warning", seconds: 60 }));
    await screen.findByText(WARNING);
    act(() => ws.output("tick\r\n"));
    expect(screen.queryByText(WARNING)).toBeNull();
  });

  it("closed for idleness: the message with the limit, the screen kept read only, Start a new session in focus", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    act(() => {
      ws.output("last words\r\n");
      ws.control({ type: "idle_warning", seconds: 60 });
      ws.control({ type: "closed", reason: "idle", minutes: 30 });
      ws.drop();
    });
    const message = await screen.findByText("Session closed after 30 minutes without activity.");
    expect(message.closest("output")).toBeTruthy();
    expect(screen.queryByText(WARNING)).toBeNull();
    expect(screen.queryByText("Disconnected")).toBeNull();
    const button = screen.getByRole("button", { name: "Start a new session" });
    expect(button.getAttribute("data-variant")).toBe("default");
    await waitFor(() => expect(document.activeElement).toBe(button));
    expect(screen.getByLabelText("Terminal in web").getAttribute("data-readonly")).toBe("true");
    expect(term.written.map(text).join("")).toContain("last words");
  });

  it("reads right at the smallest limit, 5 minutes, and at a limit of one", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    act(() => {
      ws.control({ type: "closed", reason: "idle", minutes: 5 });
      ws.drop();
    });
    expect(
      await screen.findByText("Session closed after 5 minutes without activity."),
    ).toBeTruthy();
    cleanup();

    FakeWebSocket.reset();
    renderPanel();
    const one = await openShell();
    act(() => one.open());
    await screen.findByText("Connected");
    act(() => {
      one.control({ type: "closed", reason: "idle", minutes: 1 });
      one.drop();
    });
    expect(await screen.findByText("Session closed after 1 minute without activity.")).toBeTruthy();
  });

  it("Start a new session after an idle close opens a fresh session without the banner", async () => {
    renderPanel();
    const first = await openShell();
    act(() => first.open());
    await screen.findByText("Connected");
    act(() => {
      first.control({ type: "idle_warning", seconds: 60 });
      first.control({ type: "closed", reason: "idle", minutes: 30 });
      first.drop();
    });
    fireEvent.click(await screen.findByRole("button", { name: "Start a new session" }));
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    expect(posts()).toHaveLength(2);
    await screen.findByText("Connecting…");
    expect(screen.queryByText(WARNING)).toBeNull();
  });
});

// The refusals (plan 04-23, D-31, D-34, D-35, D-36): each has a panel of its own, and the
// tab never hides.

const rowFixture = (over: Partial<ContainerRow> = {}): ContainerRow => ({
  id: "web-id",
  name: "web",
  node: "node-1",
  nodeId: "n1",
  image: "nginx:1.27",
  state: "running",
  status: "Up 2 hours",
  health: "none",
  isSelf: false,
  selfKind: "",
  readOnly: false,
  ports: [],
  project: "",
  stack: "",
  coamingStackId: "",
  service: "",
  ...over,
});

function refusal(type: string, status: number, errors?: { location: string; message: string }[]) {
  return () => Response.json({ type, title: "x", status, errors }, { status });
}

describe("a stopped container (D-34)", () => {
  it("up front: says to start it, with Start container as the accent button, and offers no shell", async () => {
    renderPanel({ running: false, row: rowFixture({ state: "exited" }) });
    expect(
      await screen.findByRole("heading", { name: "Start the container to open a shell" }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
    const start = screen.getByRole("button", { name: "Start container" });
    expect(start.getAttribute("data-variant")).toBe("default");
  });

  it("starts the container with the same request the list uses, and shows the launcher once it runs", async () => {
    const view = renderPanel({ running: false, row: rowFixture({ state: "exited" }) });
    const start = await screen.findByRole("button", { name: "Start container" });
    await permissionsLoaded();
    fireEvent.click(start);
    await waitFor(() =>
      expect(posts().some((p) => p.path.endsWith("/containers/actions"))).toBe(true),
    );
    const action = posts().find((p) => p.path.endsWith("/containers/actions"));
    expect(action?.body).toMatchObject({
      action: "start",
      targets: [{ nodeId: "n1", id: "web-id" }],
    });

    // The page reads the container again and passes the new state down.
    view.rerender(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <ShellPanel
            envId={ENV}
            containerId="web-id"
            nodeId="n1"
            name="web"
            running
            row={rowFixture()}
            createSocket={(url) => new FakeWebSocket(url)}
          />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("button", { name: "Open shell" })).toBeTruthy();
  });

  it("is unavailable with the reason for someone who may not start containers", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    renderPanel({ running: false, row: rowFixture({ state: "exited" }) });
    await screen.findByRole("button", { name: "Start container" });
    await permissionsLoaded();
    const start = screen.getByRole("button", { name: "Start container" });
    expect(start.getAttribute("aria-disabled")).toBe("true");
    expect(document.body.textContent).toContain("Needs Operator");
    fireEvent.click(start);
    await act(async () => {});
    expect(posts()).toHaveLength(0);
  });

  it("when the server says it is not running, the same panel replaces the launcher, and gives way when the page sees it run", async () => {
    openAnswer = refusal("container_not_running", 409);
    const view = renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(
      await screen.findByRole("heading", { name: "Start the container to open a shell" }),
    ).toBeTruthy();
    expect(FakeWebSocket.instances).toHaveLength(0);
    // The page's reading was stale: it said running. It now says stopped, then running.
    for (const running of [false, true]) {
      view.rerender(
        <QueryClientProvider client={queryClient}>
          <TooltipProvider>
            <ShellPanel
              envId={ENV}
              containerId="web-id"
              nodeId="n1"
              name="web"
              running={running}
              row={rowFixture()}
              createSocket={(url) => new FakeWebSocket(url)}
            />
          </TooltipProvider>
        </QueryClientProvider>,
      );
    }
    expect(await screen.findByRole("button", { name: "Open shell" })).toBeTruthy();
    expect(
      screen.queryByRole("heading", { name: "Start the container to open a shell" }),
    ).toBeNull();
  });
});

describe("an image with no shell (D-34)", () => {
  it("answered by the server: the panel with the custom command field, focused", async () => {
    openAnswer = refusal("no_shell", 409);
    renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(await screen.findByRole("heading", { name: "No shell in this image" })).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming couldn't find bash or sh in this container. If the image has one somewhere else, enter its path.",
      ),
    ).toBeTruthy();
    const field = screen.getByLabelText("Command");
    expect(screen.getByText("The command to run, for example /bin/zsh.")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(field));
  });

  it("found out after attaching: the panel replaces the terminal, and a command opens a custom shell", async () => {
    renderPanel({ row: rowFixture() });
    const first = await openShell();
    act(() => first.open());
    await screen.findByText("Connected");
    act(() => {
      first.control({ type: "closed", reason: "no_shell" });
      first.drop();
    });
    const field = await screen.findByLabelText("Command");
    expect(screen.queryByLabelText("Terminal in web")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(field));
    expect(screen.queryByText("Disconnected")).toBeNull();

    const open = screen.getByRole("button", { name: "Open shell" });
    expect(open.hasAttribute("disabled")).toBe(true);
    fireEvent.change(field, { target: { value: "/opt/bin/zsh -l" } });
    expect(open.hasAttribute("disabled")).toBe(false);
    fireEvent.click(open);
    await waitFor(() => expect(posts()).toHaveLength(2));
    expect(posts()[1]?.body).toEqual({
      nodeId: "n1",
      shell: "custom",
      command: "/opt/bin/zsh -l",
      cols: 80,
      rows: 24,
    });
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    expect(await screen.findByLabelText("Terminal in web")).toBeTruthy();
  });
});

describe("a read-only host (D-35)", () => {
  it("up front: Shells are off on this host, with no way to open one", async () => {
    renderPanel({ row: rowFixture({ readOnly: true }) });
    expect(
      await screen.findByRole("heading", { name: "Shells are off on this host" }),
    ).toBeTruthy();
    expect(
      screen.getByText("This host is read-only (set on the host). Logs still work."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
    expect(posts()).toHaveLength(0);
  });

  it("when the server refuses with host_read_only, the same panel replaces the launcher", async () => {
    openAnswer = refusal("host_read_only", 409);
    renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(
      await screen.findByRole("heading", { name: "Shells are off on this host" }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
  });
});

describe("Coaming's own containers (D-36)", () => {
  it("says Coaming does not open shells in them, for the server and for an agent, with no way to open one", async () => {
    for (const kind of ["server", "agent"] as const) {
      const view = renderPanel({ row: rowFixture({ selfKind: kind }) });
      expect(
        await screen.findByText("Coaming doesn't open shells in its own containers."),
      ).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
      view.unmount();
    }
    expect(posts()).toHaveLength(0);
  });

  it("when the server refuses with own_container, says so", async () => {
    openAnswer = refusal("own_container", 409);
    renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(
      await screen.findByText("Coaming doesn't open shells in its own containers."),
    ).toBeTruthy();
  });
});

describe("the per-person cap (D-31)", () => {
  it("says how many shells are open and that it is the most allowed, in the launcher, which stays", async () => {
    openAnswer = refusal("shell_limit_reached", 429, [{ location: "limit", message: "3" }]);
    renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(
      await screen.findByText(
        "You already have 3 shells open, which is the most allowed. Close one to open another.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open shell" }).hasAttribute("disabled")).toBe(false);
    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it("reads right at the smallest cap, one shell", async () => {
    openAnswer = refusal("shell_limit_reached", 429, [{ location: "limit", message: "1" }]);
    renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(
      await screen.findByText(
        "You already have 1 shell open, which is the most allowed. Close one to open another.",
      ),
    ).toBeTruthy();
  });
});

describe("an agent that is too old (Pitfall 13)", () => {
  it("shows the agent update notice in place of the launcher", async () => {
    openAnswer = refusal("agent_policy_denied", 502);
    renderPanel({ row: rowFixture() });
    await clickOpen();
    expect(await screen.findByText("The agent on e1 needs an update")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
  });
});

describe("a session the server ended (plan 04-23, D-29, P3 D-10)", () => {
  const cases = [
    ["access_lost", "Your access changed. This session was closed."],
    ["ended_by_admin", "An administrator ended this session."],
    ["ended_by_user", "You ended this session from your list of open shells."],
  ] as const;

  it.each(cases)(
    "%s: the message, the screen kept read only, and Start a new session in focus",
    async (reason, message) => {
      renderPanel();
      const ws = await openShell();
      act(() => ws.open());
      await screen.findByText("Connected");
      act(() => {
        ws.output("last screen\r\n");
        ws.control({ type: "closed", reason });
        ws.drop();
      });
      const shown = await screen.findByText(message);
      expect(shown.closest("output")).toBeTruthy();
      expect(screen.queryByText("Disconnected")).toBeNull();
      const button = screen.getByRole("button", { name: "Start a new session" });
      expect(button.getAttribute("data-variant")).toBe("default");
      await waitFor(() => expect(document.activeElement).toBe(button));
      expect(screen.getByLabelText("Terminal in web").getAttribute("data-readonly")).toBe("true");
      expect(term.written.map(text).join("")).toContain("last screen");
    },
  );

  it("access lost reads who the user is again, so what else changed with it shows", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    const meReads = () => sent.filter((s) => s.path === "/api/me").length;
    const before = meReads();
    act(() => {
      ws.control({ type: "closed", reason: "access_lost" });
      ws.drop();
    });
    await screen.findByText("Your access changed. This session was closed.");
    await waitFor(() => expect(meReads()).toBeGreaterThan(before));
  });

  it("the server stopping is the disconnected state: the screen kept, Reconnect shell in focus", async () => {
    renderPanel();
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByText("Connected");
    act(() => {
      ws.output("before the stop\r\n");
      ws.control({ type: "closed", reason: "server_shutdown" });
      ws.drop();
    });
    expect(await screen.findByText("Disconnected")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Reconnect shell" });
    await waitFor(() => expect(document.activeElement).toBe(button));
    expect(screen.getByLabelText("Terminal in web").getAttribute("data-readonly")).toBe("true");
    expect(term.written.map(text).join("")).toContain("before the stop");
  });
});

describe("the toolbar (plan 04-24, D-33)", () => {
  async function connected(props: Partial<React.ComponentProps<typeof ShellPanel>> = {}) {
    renderPanel(props);
    const ws = await openShell();
    act(() => ws.open());
    await screen.findByRole("toolbar", { name: "Terminal controls" });
    await waitFor(() => expect(term.props).not.toBeNull());
    return ws;
  }

  beforeEach(() => {
    localStorage.clear();
  });

  it("shows Connected, what runs and the tools, with End session as an outline button", async () => {
    await connected();
    const bar = screen.getByRole("toolbar", { name: "Terminal controls" });
    expect(within(bar).getByText("Connected")).toBeTruthy();
    expect(within(bar).getByText("bash or sh as default user in default directory")).toBeTruthy();
    expect(within(bar).getByRole("combobox", { name: "Font size" }).textContent).toBe("14");
    const search = within(bar).getByRole("button", { name: /Search/ });
    expect(search.textContent).toContain("Ctrl+Shift+F");
    expect(within(bar).getByRole("button", { name: "Keyboard" })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Open in new window" })).toBeTruthy();
    const end = within(bar).getByRole("button", { name: "End session" });
    expect(end.getAttribute("data-variant")).toBe("outline");
  });

  it("says Connecting… until the socket is open", async () => {
    renderPanel();
    await openShell();
    const bar = await screen.findByRole("toolbar", { name: "Terminal controls" });
    expect(within(bar).getByText("Connecting…")).toBeTruthy();
  });

  it("summarises the shell, user and directory the server started it with, truncated with a tooltip", async () => {
    openAnswer = () =>
      Response.json(
        {
          sessionId: "SID1",
          wsPath: `/api/env/${ENV}/shell-sessions/SID1/ws`,
          command: ["/bin/sh"],
          user: "www-data",
          workingDir: "/var/www/html",
        },
        { status: 201 },
      );
    await connected();
    const summary = screen.getByText("bash or sh as www-data in /var/www/html");
    expect(summary.className).toContain("truncate");
    expect(summary.getAttribute("tabindex")).toBe("0");
  });

  it("changes the font size: the terminal gets it and the choice is kept for this browser", async () => {
    await connected();
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Font size" }), { key: "Enter" });
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "12",
      "14",
      "16",
    ]);
    fireEvent.click(screen.getByRole("option", { name: "16" }));
    await waitFor(() => expect(term.props?.fontSize).toBe(16));
    expect(localStorage.getItem("coaming-terminal-font-size")).toBe("16");

    // The next terminal in this browser starts there.
    cleanup();
    await connected();
    expect(term.props?.fontSize).toBe(16);
    expect(screen.getByRole("combobox", { name: "Font size" }).textContent).toBe("16");
  });

  it("starts at 14 when the stored size is not one of the three", async () => {
    localStorage.setItem("coaming-terminal-font-size", "40");
    await connected();
    expect(term.props?.fontSize).toBe(14);
  });

  it("lists the keyboard shortcuts and says copy on select is off", async () => {
    await connected();
    fireEvent.click(screen.getByRole("button", { name: "Keyboard" }));
    const rows = await screen.findAllByRole("term");
    expect(rows.map((r) => r.textContent)).toEqual([
      "Copy",
      "Paste",
      "Search",
      "Leave the terminal",
    ]);
    const keys = screen.getAllByRole("definition").map((d) => d.textContent);
    expect(keys).toEqual(["Ctrl+Shift+C", "Ctrl+Shift+V", "Ctrl+Shift+F", "Ctrl+Shift+Home"]);
    expect(screen.getByText("Copy on select is off.")).toBeTruthy();
  });

  it("shows ⌘C and ⌘V on a Mac", async () => {
    vi.spyOn(navigator, "userAgent", "get").mockReturnValue(
      "Mozilla/5.0 (Macintosh; Intel Mac OS X)",
    );
    await connected();
    fireEvent.click(screen.getByRole("button", { name: "Keyboard" }));
    const keys = (await screen.findAllByRole("definition")).map((d) => d.textContent);
    expect(keys.slice(0, 2)).toEqual(["⌘C", "⌘V"]);
  });

  it("turns screen reader support on from the Keyboard popover and keeps the choice", async () => {
    await connected();
    expect(term.props?.screenReader).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Keyboard" }));
    fireEvent.click(await screen.findByRole("switch", { name: "Screen reader support" }));
    await waitFor(() => expect(term.props?.screenReader).toBe(true));
    expect(localStorage.getItem("coaming-terminal-screen-reader")).toBe("true");
  });

  it("opens the terminal's search from the Search button", async () => {
    await connected();
    fireEvent.click(within(screen.getByRole("toolbar")).getByRole("button", { name: /Search/ }));
    expect(term.searchOpened).toBe(1);
  });

  it("moves focus to the first toolbar control when the terminal asks to be left", async () => {
    await connected();
    act(() => term.props?.onLeave());
    expect(document.activeElement).toBe(screen.getByRole("combobox", { name: "Font size" }));
  });

  it("opens the shell in a new window without moving this tab's session", async () => {
    const opened = vi.spyOn(window, "open").mockReturnValue(null);
    const ws = await connected();
    fireEvent.click(screen.getByRole("button", { name: "Open in new window" }));
    expect(opened).toHaveBeenCalledTimes(1);
    const [url, target, features] = opened.mock.calls[0] ?? [];
    expect(String(url)).toBe("/popout/e1/containers/web-id/shell?node=n1");
    expect(target).toBe("_blank");
    expect(String(features)).toContain("noopener");
    expect(ws.closedByPage).toBe(false);
    expect(screen.getByLabelText("Terminal in web")).toBeTruthy();
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("has no Open in new window in the pop-out itself", async () => {
    await connected({ popout: true });
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("End session closes the socket, which ends the shell, and shows the launcher again", async () => {
    const ws = await connected();
    fireEvent.click(screen.getByRole("button", { name: "End session" }));
    expect(ws.closedByPage).toBe(true);
    expect(await screen.findByRole("heading", { name: "Open a shell in web" })).toBeTruthy();
    expect(screen.queryByLabelText("Terminal in web")).toBeNull();
    expect(screen.queryByRole("toolbar")).toBeNull();
  });

  it("keeps the tools but drops the status and End session once the session is over", async () => {
    const ws = await connected();
    act(() => {
      ws.control({ type: "exit", code: 0 });
      ws.drop();
    });
    await screen.findByText("Session ended (exit code 0).");
    const bar = screen.getByRole("toolbar", { name: "Terminal controls" });
    expect(within(bar).queryByText("Connected")).toBeNull();
    expect(within(bar).queryByRole("button", { name: "End session" })).toBeNull();
    expect(within(bar).getByRole("button", { name: /Search/ })).toBeTruthy();
  });

  it("stores only the font size and the screen reader choice, never what is typed or shown", async () => {
    const set = vi.spyOn(Storage.prototype, "setItem");
    const ws = await connected();
    act(() => {
      term.props?.onInput(new TextEncoder().encode("export TOKEN=hunter2\n"));
      ws.output("hunter2\r\n");
    });
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Font size" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "12" }));
    await waitFor(() => expect(set).toHaveBeenCalled());
    expect(set.mock.calls.map((c) => c[0])).toEqual(["coaming-terminal-font-size"]);
    expect(JSON.stringify(set.mock.calls)).not.toContain("hunter2");
    expect(sessionStorage.length).toBe(0);
  });
});

describe("the person's other open shells (plan 04-25, D-31, D-37)", () => {
  const open = (id: string) => ({
    id,
    user: "olive",
    containerId: "web-id",
    container: "web",
    node: "n1",
    startedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
    idleSeconds: 0,
  });

  it("shows Your shells (n) in the panel's header when they have shells open in this container", async () => {
    mineShells = [open("a"), open("b")];
    renderPanel();
    expect(await screen.findByRole("button", { name: "Your shells (2)" })).toBeTruthy();
    // The launcher is still there under it.
    expect(screen.getByRole("button", { name: "Open shell" })).toBeTruthy();
  });

  it("shows no Your shells button when they have none", async () => {
    mineShells = [];
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    await waitFor(() => expect(sent.some((x) => x.path === `/api/env/${ENV}/shells`)).toBe(true));
    expect(screen.queryByRole("button", { name: /^Your shells/ })).toBeNull();
  });
});
