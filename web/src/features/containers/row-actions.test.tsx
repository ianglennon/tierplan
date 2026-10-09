// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
      if (path === `/api/env/${ENV}/containers/actions` && req.method === "POST") return answer();
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
      <TooltipProvider>
        <ContainerRowActions envId={ENV} row={r} />
        <Toaster />
      </TooltipProvider>
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
      "web is one of Bayplan's own containers. Stopping it would cut Bayplan off from the host, so it can't be done from this list.",
    ],
    [
      "node_unreachable",
      { code: "node_unreachable" },
      "node-1 isn't reachable right now. Try again when it's back.",
    ],
    ["not_found", { code: "not_found" }, "web no longer exists. The list has been refreshed."],
    ["agent_policy_denied", { code: "agent_policy_denied" }, "This host's agent needs an update"],
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
      "Something went wrong on the server. Try again, and check the Bayplan logs if it keeps happening.",
    );
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
  const itemNames = () => screen.getAllByRole("menuitem").map((i) => i.textContent);

  it("offers Stop, Restart and Pause on a running row", async () => {
    mount(row({ state: "running" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(itemNames()).toEqual(["Stop", "Restart", "Pause"]);
  });

  it("offers Start and Restart on an exited row", async () => {
    mount(row({ state: "exited" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(itemNames()).toEqual(["Start", "Restart"]);
  });

  it("offers Stop, Restart and Unpause on a paused row", async () => {
    mount(row({ state: "paused" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Unpause" });
    expect(itemNames()).toEqual(["Stop", "Restart", "Unpause"]);
  });

  it("offers Stop and Restart on a restarting row, and only Start on a created one", async () => {
    mount(row({ state: "restarting" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Restart" });
    expect(itemNames()).toEqual(["Stop", "Restart"]);
    cleanup();
    mount(row({ state: "created" }));
    await openMenu();
    await screen.findByRole("menuitem", { name: "Start" });
    expect(itemNames()).toEqual(["Start"]);
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
        <TooltipProvider>
          <ContainerRowActions envId={ENV} row={r} onOpenChange={(o) => seen.push(o)} />
        </TooltipProvider>
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
