// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerRowActions } from "@/features/containers/row-actions";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

function row(extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: "web-id",
    name: "web",
    state: "running",
    status: "Up 2 hours",
    image: "nginx:1.27",
    created: "2026-10-01T10:00:00Z",
    ports: [],
    isSelf: false,
    project: "",
    stack: "",
    tierplanStackId: "",
    service: "",
    health: "none",
    selfKind: "",
    readOnly: false,
    ...extra,
  };
}

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let answer: () => Response | Promise<Response>;
let client: QueryClient;

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "operator" } });
  answer = () =>
    Response.json({ results: [{ nodeId: "n1", id: "web-id", name: "web", result: "done" }] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(me);
      if (path.startsWith(`/api/env/${ENV}/containers/`) && req.method === "POST") return answer();
      if (path.endsWith("/restart-loop-mute")) return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

const posts = () => sent.filter((s) => s.method === "POST");

function mount(r: ContainerRow = row()) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ContainerRowActions envId={ENV} row={r} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The menu needs the signed-in user, so it is opened once /api/me has answered.
async function openMenu(name = "web") {
  await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
  await act(async () => {});
  const trigger = screen.getByRole("button", { name: `Actions for ${name}` });
  await act(async () => {
    fireEvent.keyDown(trigger, { key: "Enter" });
  });
  return trigger;
}

function resultOf(extra: Record<string, unknown>) {
  return () =>
    Response.json({
      results: [{ nodeId: "n1", id: "web-id", name: "web", result: "failed", ...extra }],
    });
}

describe("ContainerRowActions menu", () => {
  it("offers Stop on a running row and no Start", async () => {
    mount(row({ state: "running" }));
    await openMenu();
    expect(await screen.findByRole("menuitem", { name: "Stop" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Start" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Stop" }).getAttribute("aria-disabled")).toBeNull();
  });

  it("offers Start on an exited row and no Stop", async () => {
    mount(row({ state: "exited", name: "db" }));
    await openMenu("db");
    expect(await screen.findByRole("menuitem", { name: "Start" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Stop" })).toBeNull();
  });

  it("offers nothing on a container that is being removed", async () => {
    mount(row({ state: "removing" }));
    await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
    expect(screen.queryByRole("button", { name: /^Actions for/ })).toBeNull();
  });

  it("shows the item disabled with the reason Needs Operator when the user may only read", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount(row({ state: "running" }));
    await openMenu();
    const stop = await screen.findByRole("menuitem", { name: /^Stop/ });
    expect(stop.getAttribute("aria-disabled")).toBe("true");
    expect(stop.textContent).toContain("Stop");
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
    fireEvent.click(stop);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(posts()).toEqual([]);
  });

  it("shows Start disabled for a viewer on an exited row", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount(row({ state: "exited" }));
    await openMenu();
    const start = await screen.findByRole("menuitem", { name: /^Start/ });
    expect(start.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(start);
    expect(posts()).toEqual([]);
  });
});

describe("Open", () => {
  it("is the first item of the menu and goes to the container's page on its node", async () => {
    mount();
    await openMenu();
    const items = await screen.findAllByRole("menuitem");
    expect(items[0]?.textContent).toBe("Open");
    expect(items[0]?.getAttribute("href")).toBe("/env/e1/containers/web-id?node=n1");
  });

  it("is there for a Viewer too, who can read the container", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    await openMenu();
    const open = await screen.findByRole("menuitem", { name: "Open" });
    expect(open.getAttribute("aria-disabled")).toBeNull();
  });
});

describe("Stop", () => {
  async function openStop() {
    mount();
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop" }));
    return screen.findByRole("alertdialog", { name: "Stop web?" });
  }

  it("asks first, with focus on Cancel, and sends nothing until confirmed", async () => {
    const dialog = await openStop();
    expect(dialog.textContent).toContain("using the container's own stop settings");
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    expect(posts()).toEqual([]);
  });

  it("posts the node ID and container ID, toasts Stopped, and closes", async () => {
    const dialog = await openStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe("/api/env/e1/containers/actions");
    expect(posts()[0]?.body).toEqual({
      action: "stop",
      targets: [{ nodeId: "n1", id: "web-id" }],
    });
    expect(await screen.findByText("Stopped web.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("shows Stopping… while the request is out and sends it once", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    const dialog = await openStop();
    const confirm = within(dialog).getByRole("button", { name: "Stop container" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    expect(await within(dialog).findByRole("button", { name: "Stopping…" })).toBeTruthy();
    expect(posts()).toHaveLength(1);
    release(
      Response.json({ results: [{ nodeId: "n1", id: "web-id", name: "web", result: "done" }] }),
    );
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  const refusals: [string, Record<string, unknown>, string][] = [
    [
      "own_container",
      { code: "own_container" },
      "web is one of TierPlan's own containers. Stopping it would cut TierPlan off from the host, so it can't be done from this list.",
    ],
    [
      "node_unreachable",
      { code: "node_unreachable" },
      "node-1 isn't reachable right now. Try again when it's back.",
    ],
    ["not_found", { code: "not_found" }, "web no longer exists. The list has been refreshed."],
    [
      "an engine refusal",
      { code: "action_failed", detail: "cannot stop container: permission denied" },
      "cannot stop container: permission denied",
    ],
  ];

  for (const [name, extra, copy] of refusals) {
    it(`keeps the dialog open with the mapped copy for ${name}, never a raw code`, async () => {
      answer = resultOf(extra);
      const dialog = await openStop();
      fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
      const alert = await within(dialog).findByRole("alert");
      expect(alert.textContent).toBe(copy);
      for (const code of [
        "own_container",
        "node_unreachable",
        "not_found",
        "agent_policy_denied",
      ]) {
        expect(dialog.textContent).not.toContain(code);
      }
      expect(screen.getByRole("alertdialog")).toBeTruthy();
      // No toast over the dialog.
      expect(screen.queryByText(/^Couldn't stop/)).toBeNull();
    });
  }

  it("shows the agent update notice, not a bare code, when the agent refuses by its own policy", async () => {
    answer = resultOf({ code: "agent_policy_denied" });
    const dialog = await openStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("This host's agent needs an update");
    expect(alert.textContent).not.toContain("agent_policy_denied");
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("says the permission was withdrawn when the whole request is refused with 403", async () => {
    answer = () => Response.json({ title: "Forbidden", status: 403 }, { status: 403 });
    const dialog = await openStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "You no longer have permission to do that. Your access may have changed.",
    );
  });

  it("says something went wrong on the server for a 500 and does not retry", async () => {
    answer = () => Response.json({ title: "Internal Server Error", status: 500 }, { status: 500 });
    const dialog = await openStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "Something went wrong on the server. Try again, and check the TierPlan logs if it keeps happening.",
    );
    expect(posts()).toHaveLength(1);
  });

  // Found by the end-to-end suite: Chromium abandons every request in flight when
  // the host's network changes, and the dialog said the server had failed although
  // the server went on to stop the container.
  it("closes and says the outcome is unknown when the connection drops before the answer, without retrying", async () => {
    answer = () => {
      throw new TypeError("Failed to fetch");
    };
    const dialog = await openStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    expect(
      await screen.findByText(
        "The connection dropped before TierPlan answered the request to stop web. The list shows whether it happened.",
      ),
    ).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(screen.queryByText(/Something went wrong on the server/)).toBeNull();
    expect(posts()).toHaveLength(1);
  });
});

describe("Start", () => {
  it("posts at once with no dialog and toasts Started", async () => {
    answer = () =>
      Response.json({ results: [{ nodeId: "n1", id: "db-id", name: "db", result: "done" }] });
    mount(row({ id: "db-id", name: "db", state: "exited" }));
    await openMenu("db");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Start" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ action: "start", targets: [{ nodeId: "n1", id: "db-id" }] });
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText("Started db.")).toBeTruthy();
  });

  it("toasts the reason when the start is refused", async () => {
    answer = resultOf({ code: "node_unreachable" });
    mount(row({ state: "exited" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Start" }));
    expect(
      await screen.findByText(
        "Couldn't start web. node-1 isn't reachable right now. Try again when it's back.",
      ),
    ).toBeTruthy();
  });

  it("says the outcome is unknown, not that it failed, when the connection drops", async () => {
    answer = () => {
      throw new TypeError("Failed to fetch");
    };
    mount(row({ state: "exited" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Start" }));
    expect(
      await screen.findByText(
        "The connection dropped before TierPlan answered the request to start web. The list shows whether it happened.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText(/Couldn't start web/)).toBeNull();
  });
});

describe("After an answer", () => {
  it("reads the containers and the summary again at once and once more 2000 ms later", async () => {
    const invalidate = vi.spyOn(client, "invalidateQueries");
    const timers = vi.spyOn(globalThis, "setTimeout");
    mount(row({ state: "exited" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Start" }));
    await waitFor(() => expect(invalidate).toHaveBeenCalledTimes(2));
    const keys = invalidate.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["env", ENV, "containers"]);
    expect(keys).toContainEqual(["env", ENV, "summary"]);

    const recheck = timers.mock.calls.find((c) => c[1] === 2000);
    if (!recheck) throw new Error("no recheck was scheduled for 2000 ms");
    (recheck[0] as () => void)();
    expect(invalidate).toHaveBeenCalledTimes(4);
  });

  it("reads the lists again after a failure too", async () => {
    answer = resultOf({ code: "not_found" });
    const invalidate = vi.spyOn(client, "invalidateQueries");
    mount(row({ state: "exited" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Start" }));
    await waitFor(() => expect(invalidate).toHaveBeenCalled());
  });
});

describe("Restart, Pause and Unpause", () => {
  // Open (plan 04-10) is in every menu; the rest follow the state.
  const itemNames = () =>
    screen
      .getAllByRole("menuitem")
      .map((i) => i.textContent)
      .filter((name) => name !== "Open");

  it("offers Stop, Restart and Pause on a running row", async () => {
    mount(row({ state: "running" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(itemNames()).toEqual(["Stop", "Restart", "Pause", "Kill…", "Remove…"]);
  });

  it("offers Start and Restart on an exited row", async () => {
    mount(row({ state: "exited" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(itemNames()).toEqual(["Start", "Restart", "Remove…"]);
  });

  it("offers Stop, Restart and Unpause on a paused row", async () => {
    mount(row({ state: "paused" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Unpause" });
    expect(itemNames()).toEqual(["Stop", "Restart", "Unpause", "Kill…", "Remove…"]);
  });

  it("offers Stop and Restart on a restarting row, and only Start on a created one", async () => {
    mount(row({ state: "restarting" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(itemNames()).toEqual(["Stop", "Restart", "Kill…", "Remove…"]);
    cleanup();
    mount(row({ state: "created" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Start" });
    expect(itemNames()).toEqual(["Start", "Remove…"]);
  });

  it.each([
    ["Restart", "restart", "Restarted web."],
    ["Pause", "pause", "Paused web."],
  ])("%s posts at once with no dialog and toasts %s", async (label, action, toastText) => {
    mount(row({ state: "running" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: label }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ action, targets: [{ nodeId: "n1", id: "web-id" }] });
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText(toastText)).toBeTruthy();
  });

  it("Unpause posts at once and toasts Unpaused", async () => {
    mount(row({ state: "paused" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unpause" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      action: "unpause",
      targets: [{ nodeId: "n1", id: "web-id" }],
    });
    expect(await screen.findByText("Unpaused web.")).toBeTruthy();
  });

  it("toasts the reason when the engine refuses a restart", async () => {
    answer = resultOf({ code: "engine_error", detail: "container is not running" });
    mount(row({ state: "running" }));
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Restart" }));
    expect(await screen.findByText("Couldn't restart web. container is not running")).toBeTruthy();
  });

  it("shows all three disabled with Needs Operator for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount(row({ state: "paused" }));
    await openMenu();
    for (const name of ["Restart", "Unpause"]) {
      const item = await screen.findByRole("menuitem", { name });
      expect(item.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(item);
    }
    expect(posts()).toEqual([]);
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
  });
});

describe("ContainerRowActions open state", () => {
  function track(r: ContainerRow = row()) {
    const seen: boolean[] = [];
    const view = render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <ContainerRowActions envId={ENV} row={r} onOpenChange={(o) => seen.push(o)} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    return { seen, view };
  }

  it("reports the menu opening and closing", async () => {
    const { seen } = track();
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(seen[seen.length - 1]).toBe(true);
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(seen[seen.length - 1]).toBe(false));
  });

  it("reports open while the Stop dialog is up", async () => {
    const { seen } = track();
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Stop web?" });
    expect(seen[seen.length - 1]).toBe(true);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(seen[seen.length - 1]).toBe(false));
  });

  it("reports closed when an open row goes away", async () => {
    const { seen, view } = track();
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    view.unmount();
    expect(seen[seen.length - 1]).toBe(false);
  });

  it("disables the menu button for a removed row", async () => {
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <ContainerRowActions envId={ENV} row={row()} disabled />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
    expect(
      (screen.getByRole("button", { name: "Actions for web" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});

describe("Kill and Remove", () => {
  async function choose(label: string, r: ContainerRow = row()) {
    mount(r);
    await openMenu(r.name);
    fireEvent.click(await screen.findByRole("menuitem", { name: label }));
  }

  it("puts Kill… and Remove… after a separator, in destructive text", async () => {
    mount();
    await openMenu();
    const kill = await screen.findByRole("menuitem", { name: "Kill…" });
    const remove = screen.getByRole("menuitem", { name: "Remove…" });
    expect(kill.getAttribute("data-variant")).toBe("destructive");
    expect(remove.getAttribute("data-variant")).toBe("destructive");
    expect(screen.getAllByRole("separator").length).toBeGreaterThan(0);
  });

  it("opens the kill dialog and posts the kill", async () => {
    await choose("Kill…");
    const dialog = await screen.findByRole("alertdialog", { name: "Kill web?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Kill container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe("/api/env/e1/containers/actions");
    expect(posts()[0]?.body).toEqual({ action: "kill", targets: [{ nodeId: "n1", id: "web-id" }] });
    expect(await screen.findByText("Killed web.")).toBeTruthy();
  });

  it("opens the remove dialog and posts the remove with Force", async () => {
    answer = () =>
      Response.json({ results: [{ nodeId: "n1", id: "web-id", name: "web", result: "done" }] });
    await choose("Remove…");
    const dialog = await screen.findByRole("alertdialog", { name: "Remove web?" });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Force (stop if running)" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe("/api/env/e1/containers/remove");
    expect(await screen.findByText("Removed web.")).toBeTruthy();
  });

  it("shows both disabled with Needs Operator for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    await openMenu();
    for (const name of ["Kill…", "Remove…"]) {
      const item = await screen.findByRole("menuitem", { name });
      expect(item.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(item);
    }
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(posts()).toEqual([]);
  });

  it("reports open while the Kill dialog is up", async () => {
    const seen: boolean[] = [];
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <ContainerRowActions envId={ENV} row={row()} onOpenChange={(o) => seen.push(o)} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    await openMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Kill…" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Kill web?" });
    expect(seen[seen.length - 1]).toBe(true);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(seen[seen.length - 1]).toBe(false));
  });
});

describe("TierPlan's own containers", () => {
  const own = (extra: Partial<ContainerRow> = {}) =>
    row({ name: "tierplan", id: "tp-id", selfKind: "server", isSelf: true, ...extra });

  it.each([
    ["Stop", "Stop tierplan?"],
    ["Restart", "Restart tierplan?"],
    ["Pause", "Pause tierplan?"],
    ["Kill…", "Kill tierplan?"],
    ["Remove…", "Remove tierplan?"],
  ])(
    "%s opens the own-container dialog with the name to type, and sends nothing",
    async (label, title) => {
      mount(own());
      await openMenu("tierplan");
      fireEvent.click(await screen.findByRole("menuitem", { name: label }));
      const dialog = await screen.findByRole("alertdialog", { name: title });
      expect(within(dialog).getByLabelText("Type tierplan to confirm")).toBeTruthy();
      expect(dialog.textContent).toContain("cuts TierPlan off from this host");
      expect(posts()).toEqual([]);
    },
  );

  it("starts an own container that is stopped at once, as for any other", async () => {
    answer = () =>
      Response.json({ results: [{ nodeId: "n1", id: "tp-id", name: "tierplan", result: "done" }] });
    mount(own({ state: "exited" }));
    await openMenu("tierplan");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Start" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});

// Muting the restart-loop alerts of a standalone container from its row menu (plan 06-36,
// RES-13, D-16): offered only for a row the server flagged.
describe("ContainerRowActions restart-loop mute", () => {
  const flagged = (state: "flagged" | "muted" = "flagged") =>
    row({
      id: "crashy-id",
      name: "crashy",
      state: "restarting",
      restartLoop: { state, count: 3, windowMinutes: 10, lastExitCode: "3" },
    });

  it("offers Mute restart-loop alerts for a flagged container and sends the node and container", async () => {
    answer = () => Response.json({ containerId: "crashy-id", muted: true, changed: true });
    mount(flagged());
    await openMenu("crashy");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" }));
    await waitFor(() =>
      expect(sent.some((s) => s.method === "PUT" && s.path.endsWith("/restart-loop-mute"))).toBe(
        true,
      ),
    );
    const put = sent.find((s) => s.method === "PUT");
    expect(put?.path).toBe(`/api/env/${ENV}/containers/crashy-id/restart-loop-mute`);
    expect(await screen.findByText("Muted restart-loop alerts for crashy.")).toBeTruthy();
  });

  it("offers Unmute for a muted loop", async () => {
    mount(flagged("muted"));
    await openMenu("crashy");
    expect(
      await screen.findByRole("menuitem", { name: "Unmute restart-loop alerts" }),
    ).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Mute restart-loop alerts" })).toBeNull();
  });

  it("offers nothing for a container that is not in a loop", async () => {
    mount();
    await openMenu();
    await screen.findByRole("menuitem", { name: "Stop" });
    expect(screen.queryByRole("menuitem", { name: /restart-loop alerts/ })).toBeNull();
  });
});
