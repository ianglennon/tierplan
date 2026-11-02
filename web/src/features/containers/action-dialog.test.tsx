// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ActionDialog } from "@/features/containers/action-dialog";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { type PendingApi, PendingContext } from "@/features/containers/pending";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

function row(name: string, extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: `id-${name}`,
    name,
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
  path: string;
  body?: { targets: { id: string }[]; [k: string]: unknown };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let marks: { keys: string[]; verb: string }[];
let released: string[][];
let me: ReturnType<typeof makeMe>;

const api: PendingApi = {
  mark: (rows, verb) => marks.push({ keys: rows.map((r) => r.key), verb }),
  release: (keys) => released.push([...keys]),
};

const allDone = (req: Sent) =>
  Response.json({
    bulkId: "018f0000-0000-7000-8000-000000000001",
    results: (req.body?.targets ?? []).map((t) => ({
      nodeId: "n1",
      id: t.id,
      name: t.id.replace("id-", ""),
      result: "done",
    })),
  });

beforeEach(() => {
  sent = [];
  marks = [];
  released = [];
  me = makeMe({ envs: { [ENV]: "operator" } });
  answer = allDone;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path.startsWith(`/api/env/${ENV}/containers/`) && req.method === "POST")
        return answer(entry);
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

const posts = () => sent.filter((s) => s.path.includes("/containers/") && s.body !== undefined);

function mount(rows: ContainerRow[], props: Partial<Parameters<typeof ActionDialog>[0]> = {}) {
  const onClose = vi.fn();
  const onDone = vi.fn();
  function Host() {
    const [report, setReport] = useState<BulkReport | null>(null);
    return (
      <>
        <ActionDialog
          envId={ENV}
          verb="stop"
          rows={rows}
          open
          onClose={onClose}
          onDone={onDone}
          onDetails={setReport}
          {...props}
        />
        <BulkResultDialog envId={ENV} report={report} onClose={() => setReport(null)} />
      </>
    );
  }
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <PendingContext.Provider value={api}>
          <Host />
        </PendingContext.Provider>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onClose, onDone };
}

const TWO = [row("web"), row("db", { node: "node-2", nodeId: "n2" })];

describe("ActionDialog stop", () => {
  it("asks 'Stop {name}?' for one container and lists nothing", async () => {
    mount([row("web")]);
    const dialog = await screen.findByRole("alertdialog", { name: "Stop web?" });
    expect(dialog.textContent).toContain("using the container's own stop settings");
    expect(within(dialog).queryByRole("table")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Stop container" })).toBeTruthy();
  });

  it("asks 'Stop 2 containers?' with every target and its node, focus on Cancel and nothing sent yet", async () => {
    mount(TWO, { swarm: true });
    const dialog = await screen.findByRole("alertdialog", { name: "Stop 2 containers?" });
    expect(dialog.textContent).toContain("Each one uses its own stop settings.");
    const table = within(dialog).getByRole("table");
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Container", "Node", "Note"]);
    const body = within(table).getAllByRole("row").slice(1);
    expect(body.map((r) => r.textContent)).toEqual(["webnode-1", "dbnode-2"]);
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    expect(posts()).toEqual([]);
  });

  it("leaves the Node column out on a standalone host", async () => {
    mount(TWO);
    const dialog = await screen.findByRole("alertdialog");
    expect(
      within(dialog)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Container", "Note"]);
  });

  it("posts one request with both targets, toasts once, closes and tells the list", async () => {
    const { onClose, onDone } = mount(TWO);
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      action: "stop",
      targets: [
        { nodeId: "n1", id: "id-web" },
        { nodeId: "n2", id: "id-db" },
      ],
    });
    expect(await screen.findByText("Stopped 2 containers.")).toBeTruthy();
    expect(onClose).toHaveBeenCalled();
    expect(onDone).toHaveBeenCalled();
  });

  it("marks both rows as stopping when the request goes out", async () => {
    mount(TWO);
    fireEvent.click(await screen.findByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(marks).toHaveLength(1));
    expect(marks[0]).toEqual({ keys: ["node-1|id-web", "node-2|id-db"], verb: "stop" });
  });

  it("shows Stopping… and sends once however often the button is pressed", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount(TWO);
    const confirm = await screen.findByRole("button", { name: "Stop 2 containers" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    const pending = await screen.findByRole("button", { name: "Stopping…" });
    expect(pending.getAttribute("aria-disabled")).toBe("true");
    expect(posts()).toHaveLength(1);
    // The dialog stays open until the server answers.
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    await act(async () =>
      release(allDone({ path: "", body: { targets: [{ id: "id-web" }, { id: "id-db" }] } })),
    );
    await waitFor(() => expect(screen.getByText("Stopped 2 containers.")).toBeTruthy());
  });

  it("sums a partial result in one error toast and takes the failed row's label off at once", async () => {
    answer = (req) =>
      Response.json({
        results: (req.body?.targets ?? []).map((t, i) => ({
          nodeId: "n1",
          id: t.id,
          name: t.id,
          result: i === 0 ? "done" : "failed",
          code: i === 0 ? undefined : "node_unreachable",
        })),
      });
    mount(TWO);
    fireEvent.click(await screen.findByRole("button", { name: "Stop 2 containers" }));
    expect(await screen.findByText("Stopped 1, 1 failed")).toBeTruthy();
    expect(released).toEqual([["node-2|id-db"]]);
  });

  it("keeps a bulk dialog open with an inline alert when the whole request is refused", async () => {
    answer = () => Response.json({ status: 403 }, { status: 403 });
    const { onClose } = mount(TWO);
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "You no longer have permission to do that. Your access may have changed.",
    );
    expect(onClose).not.toHaveBeenCalled();
    // Every row goes back to its real state.
    expect(released).toEqual([["node-1|id-web", "node-2|id-db"]]);
    expect(screen.queryByText(/^Couldn't stop/)).toBeNull();
  });

  it("keeps a single dialog open with the mapped reason when the container fails", async () => {
    answer = (req) =>
      Response.json({
        results: [
          {
            nodeId: "n1",
            id: req.body?.targets[0]?.id,
            name: "web",
            result: "failed",
            code: "node_unreachable",
          },
        ],
      });
    mount([row("web")]);
    fireEvent.click(await screen.findByRole("button", { name: "Stop container" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("node-1 isn't reachable right now. Try again when it's back.");
  });

  it("has a name that wraps rather than runs off the dialog", async () => {
    const long = `${"a".repeat(80)}-service-1`;
    mount([row(long)]);
    const title = await screen.findByRole("heading", { name: `Stop ${long}?` });
    expect(title.className).toContain("break-all");
  });
});

// ---- Task 2 -----------------------------------------------------------------

const body = () => posts()[0]?.body as Record<string, unknown>;
const confirmButton = (name: string | RegExp) => screen.getByRole("button", { name });

async function dialogOf(name: string | RegExp) {
  return screen.findByRole("alertdialog", { name });
}

describe("ActionDialog kill", () => {
  it("asks 'Kill {name}?' with the SIGKILL warning and a destructive confirm", async () => {
    mount([row("web")], { verb: "kill" });
    const dialog = await dialogOf("Kill web?");
    expect(dialog.textContent).toContain(
      "Sends SIGKILL now. The process gets no chance to shut down cleanly, and data it was writing can be lost.",
    );
    const confirm = within(dialog).getByRole("button", { name: "Kill container" });
    expect(confirm.className).toContain("bg-destructive-fill");
    fireEvent.click(confirm);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(body()).toEqual({ action: "kill", targets: [{ nodeId: "n1", id: "id-web" }] });
    expect(await screen.findByText("Killed web.")).toBeTruthy();
  });

  it("lists a container that is not running as skipped, leaves it out of the count and the request, and counts it in the toast", async () => {
    mount([row("a"), row("b", { state: "exited" }), row("c")], { verb: "kill" });
    const dialog = await dialogOf("Kill 2 containers?");
    const rows = within(dialog).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual(["a", "bNot running, skipped", "c"]);
    expect(rows[1]?.hasAttribute("data-skipped")).toBe(true);
    fireEvent.click(within(dialog).getByRole("button", { name: "Kill 2 containers" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect((body().targets as { id: string }[]).map((t) => t.id)).toEqual(["id-a", "id-c"]);
    expect(await screen.findByText("Killed 2 containers. 1 skipped")).toBeTruthy();
  });

  it("sends a signal only when one is typed in Advanced, with SIGKILL as its placeholder", async () => {
    mount([row("web")], { verb: "kill" });
    const dialog = await dialogOf("Kill web?");
    expect(within(dialog).queryByLabelText("Signal")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    const signal = await within(dialog).findByLabelText("Signal");
    expect(signal.getAttribute("placeholder")).toBe("SIGKILL");
    expect(dialog.textContent).toContain("Leave empty to use the container's own settings.");
    fireEvent.change(signal, { target: { value: "SIGTERM" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Kill container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(body()).toEqual({
      action: "kill",
      signal: "SIGTERM",
      targets: [{ nodeId: "n1", id: "id-web" }],
    });
  });
});

describe("ActionDialog stop options", () => {
  it("sends neither timeout nor signal when Advanced is left empty", async () => {
    mount([row("web")]);
    const dialog = await dialogOf("Stop web?");
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    await within(dialog).findByLabelText("Timeout (seconds)");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(body()).toEqual({ action: "stop", targets: [{ nodeId: "n1", id: "id-web" }] });
  });

  it("sends the timeout and signal that were filled, the placeholders showing the container's own", async () => {
    mount([row("web")], { seconds: 15 });
    const dialog = await dialogOf("Stop web?");
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    const timeout = await within(dialog).findByLabelText("Timeout (seconds)");
    expect(timeout.getAttribute("placeholder")).toBe("15");
    fireEvent.change(timeout, { target: { value: "30" } });
    fireEvent.change(within(dialog).getByLabelText("Signal"), { target: { value: "sigint" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(body()).toEqual({
      action: "stop",
      timeout: 30, // wall-clock: request body field, not a wait
      signal: "SIGINT",
      targets: [{ nodeId: "n1", id: "id-web" }],
    });
  });

  it("refuses a timeout that is not a whole number without sending anything", async () => {
    mount([row("web")]);
    const dialog = await dialogOf("Stop web?");
    fireEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    fireEvent.change(await within(dialog).findByLabelText("Timeout (seconds)"), {
      target: { value: "soon" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop container" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Enter a whole number of seconds, 0 to 86400.",
    );
    expect(posts()).toEqual([]);
  });

  it("has no Advanced section for several containers, and never sends options", async () => {
    mount(TWO);
    const dialog = await dialogOf("Stop 2 containers?");
    expect(within(dialog).queryByRole("button", { name: "Advanced" })).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(Object.keys(body()).sort()).toEqual(["action", "targets"]);
  });
});

describe("ActionDialog remove", () => {
  it("asks 'Remove {name}?' with both options unticked", async () => {
    mount([row("web", { state: "exited" })], { verb: "remove" });
    const dialog = await dialogOf("Remove web?");
    expect(dialog.textContent).toContain(
      "The container is deleted. Its image and its named volumes stay.",
    );
    expect(dialog.textContent).toContain("Needed to remove a running container.");
    expect(dialog.textContent).toContain(
      "Volumes Docker made for this container without a name. Named volumes are never removed.",
    );
    expect(
      within(dialog)
        .getByRole("checkbox", { name: "Force (stop if running)" })
        .getAttribute("aria-checked"),
    ).toBe("false");
    expect(
      within(dialog)
        .getByRole("checkbox", { name: "Also remove anonymous volumes" })
        .getAttribute("aria-checked"),
    ).toBe("false");
    expect(confirmButton("Remove container").className).toContain("bg-destructive-fill");
  });

  it("keeps Remove container disabled for a running container until Force is ticked", async () => {
    mount([row("web")], { verb: "remove" });
    const dialog = await dialogOf("Remove web?");
    const confirm = within(dialog).getByRole("button", { name: "Remove container" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    expect(dialog.textContent).toContain("Tick Force to remove a running container.");
    fireEvent.click(confirm);
    expect(posts()).toEqual([]);
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Force (stop if running)" }));
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).not.toBe("true"));
    expect(dialog.textContent).not.toContain("Tick Force");
    fireEvent.click(confirm);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe(`/api/env/${ENV}/containers/remove`);
    expect(body()).toEqual({
      force: true,
      removeVolumes: false,
      targets: [{ nodeId: "n1", id: "id-web" }],
    });
    expect(await screen.findByText("Removed web.")).toBeTruthy();
  });

  it("needs no Force for a container that is not running", async () => {
    mount([row("web", { state: "exited" })], { verb: "remove" });
    const dialog = await dialogOf("Remove web?");
    const confirm = within(dialog).getByRole("button", { name: "Remove container" });
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
    expect(dialog.textContent).not.toContain("Tick Force");
  });

  it("sends removeVolumes when Also remove anonymous volumes is ticked and the caller may delete volumes", async () => {
    me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["volumes.delete"] } });
    mount([row("web", { state: "exited" })], { verb: "remove" });
    const dialog = await dialogOf("Remove web?");
    const volumes = () =>
      within(dialog).getByRole("checkbox", { name: "Also remove anonymous volumes" });
    // Once the permissions are known the checkbox is rebuilt without its reason.
    await waitFor(() => expect(volumes().getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(volumes());
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove container" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(body().removeVolumes).toBe(true);
  });

  it("disables Also remove anonymous volumes with a reason when the caller may not delete volumes", async () => {
    mount([row("web", { state: "exited" })], { verb: "remove" });
    const dialog = await dialogOf("Remove web?");
    const volumes = within(dialog).getByRole("checkbox", { name: "Also remove anonymous volumes" });
    await waitFor(() => expect(volumes.getAttribute("aria-disabled")).toBe("true"));
    expect(dialog.textContent).toContain("Needs the delete volumes permission");
    fireEvent.click(volumes);
    expect(volumes.getAttribute("aria-checked")).toBe("false");
  });

  it("removes several with one title, one button and one toast, and lists a running one needing Force", async () => {
    mount([row("a", { state: "exited" }), row("b")], { verb: "remove" });
    const dialog = await dialogOf("Remove 2 containers?");
    expect(
      within(dialog)
        .getByRole("button", { name: "Remove 2 containers" })
        .getAttribute("aria-disabled"),
    ).toBe("true");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Force (stop if running)" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove 2 containers" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(await screen.findByText("Removed 2 containers.")).toBeTruthy();
  });

  it("does not skip any container: every state can be removed", async () => {
    mount([row("a", { state: "exited" }), row("b", { state: "dead" })], { verb: "remove" });
    const dialog = await dialogOf("Remove 2 containers?");
    expect(
      within(dialog)
        .getAllByRole("row")
        .slice(1)
        .every((r) => !r.hasAttribute("data-skipped")),
    ).toBe(true);
  });
});

describe("ActionDialog warnings for swarm tasks and projects", () => {
  it("warns that Swarm will replace a single task", async () => {
    mount([row("web.1", { service: "web" })]);
    const dialog = await dialogOf("Stop web.1?");
    expect(dialog.textContent).toContain(
      "This is a task of service web. Swarm will replace it. To stop it for good, scale the service.",
    );
  });

  it("warns that the next deploy will recreate a single container of a project", async () => {
    mount([row("shop-web", { project: "shop" })]);
    const dialog = await dialogOf("Stop shop-web?");
    expect(dialog.textContent).toContain("Part of project shop. The next deploy will recreate it.");
  });

  it("counts them in the plural forms, and notes each row in the list", async () => {
    mount([
      row("a.1", { service: "a" }),
      row("b.1", { service: "b" }),
      row("c-1", { project: "c" }),
      row("d-1", { project: "d" }),
      row("e"),
    ]);
    const dialog = await dialogOf("Stop 5 containers?");
    expect(dialog.textContent).toContain(
      "2 of these are tasks of swarm services. Swarm will replace them.",
    );
    expect(dialog.textContent).toContain(
      "2 of these belong to projects. The next deploy will recreate them.",
    );
    const notes = within(dialog)
      .getAllByRole("row")
      .slice(1)
      .map((r) => (r as HTMLTableRowElement).cells[1]?.textContent);
    expect(notes).toEqual([
      "Task of service a",
      "Task of service b",
      "Part of project c",
      "Part of project d",
      "",
    ]);
  });

  it("has the singular-of-several form for exactly one", async () => {
    mount([row("a.1", { service: "a" }), row("b")]);
    const dialog = await dialogOf("Stop 2 containers?");
    expect(dialog.textContent).toContain(
      "1 of these is a task of a swarm service. Swarm will replace it.",
    );
  });

  it("shows no warning for a plain container", async () => {
    mount([row("web")]);
    const dialog = await dialogOf("Stop web?");
    expect(dialog.textContent).not.toContain("Swarm will replace");
    expect(dialog.textContent).not.toContain("next deploy");
  });
});

describe("ActionDialog on TierPlan's own containers", () => {
  const own = (extra: Partial<ContainerRow> = {}) =>
    row("tierplan", { selfKind: "server", isSelf: true, ...extra });

  it("warns, asks for the name, keeps the destructive confirm disabled until it matches, and sends it", async () => {
    mount([own()]);
    const dialog = await dialogOf("Stop tierplan?");
    expect(dialog.textContent).toContain(
      "This is TierPlan's own container. Stopping it cuts TierPlan off from this host, and you can't start it again from here.",
    );
    const confirm = within(dialog).getByRole("button", { name: "Stop TierPlan" });
    expect(confirm.className).toContain("bg-destructive-fill");
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    const field = within(dialog).getByLabelText("Type tierplan to confirm");
    // Focus starts on Cancel, not on the field.
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    fireEvent.click(confirm);
    expect(posts()).toEqual([]);
    fireEvent.change(field, { target: { value: "tierplan-x" } });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.keyDown(field, { key: "Enter" });
    expect(posts()).toEqual([]);
    fireEvent.change(field, { target: { value: "tierplan" } });
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(confirm);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(body()).toEqual({
      action: "stop",
      confirm: "tierplan",
      targets: [{ nodeId: "n1", id: "id-tierplan" }],
    });
  });

  it("has no Advanced section for it", async () => {
    mount([own()]);
    const dialog = await dialogOf("Stop tierplan?");
    expect(within(dialog).queryByRole("button", { name: "Advanced" })).toBeNull();
  });

  it("names the node for an agent and its own confirm 'Kill agent'", async () => {
    mount([row("tierplan-agent", { selfKind: "agent" })], { verb: "kill" });
    const dialog = await dialogOf("Kill tierplan-agent?");
    expect(dialog.textContent).toContain(
      "This is the TierPlan agent on node-1. Killing it cuts TierPlan off from node-1.",
    );
    expect(within(dialog).getByRole("button", { name: "Kill agent" })).toBeTruthy();
  });

  it.each([
    ["restart", "Restart tierplan?", "Restarting it", "Restart TierPlan"],
    ["pause", "Pause tierplan?", "Pausing it", "Pause TierPlan"],
    ["remove", "Remove tierplan?", "Removing it", "Remove TierPlan"],
  ] as const)("words %s for it", async (verb, title, gerund, button) => {
    mount([own({ state: verb === "remove" ? "exited" : "running" })], { verb });
    const dialog = await dialogOf(title);
    expect(dialog.textContent).toContain(`${gerund} cuts TierPlan off from this host`);
    expect(within(dialog).getByRole("button", { name: button })).toBeTruthy();
  });

  it("says to type the name when the server still wants it", async () => {
    answer = () => Response.json({ status: 422, type: "confirmation_required" }, { status: 422 });
    mount([own()]);
    const dialog = await dialogOf("Stop tierplan?");
    fireEvent.change(within(dialog).getByLabelText("Type tierplan to confirm"), {
      target: { value: "tierplan" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop TierPlan" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Type the container's name to confirm this.",
    );
  });
});

describe("ActionDialog results", () => {
  const partial = (req: Sent) =>
    Response.json({
      results: (req.body?.targets ?? []).map((t, i) => ({
        nodeId: i === 1 ? "n2" : "n1",
        id: t.id,
        name: t.id.replace("id-", ""),
        result: i === 0 ? "done" : i === 1 ? "failed" : "skipped",
        code: i === 0 ? undefined : i === 1 ? "node_unreachable" : "own_container",
      })),
    });

  it("sums a partial result in a toast with Details, which opens the results with a reason for each", async () => {
    answer = partial;
    mount([row("a"), row("b", { node: "node-2", nodeId: "n2" }), row("c")], { swarm: true });
    fireEvent.click(await screen.findByRole("button", { name: "Stop 3 containers" }));
    expect(await screen.findByText("Stopped 1, 1 failed, 1 skipped")).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog", { name: "Stop results" });
    expect(
      within(results)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Container", "Node", "Result", "Reason"]);
    const rows = within(results)
      .getAllByRole("row")
      .slice(1)
      .map((r) => [...(r as HTMLTableRowElement).cells].map((c) => c.textContent));
    expect(rows).toEqual([
      ["a", "node-1", "Done", "—"],
      ["b", "node-2", "Failed", "node-2 isn't reachable right now. Try again when it's back."],
      [
        "c",
        "node-1",
        "Skipped",
        "c is one of TierPlan's own containers. Stopping it would cut TierPlan off from the host, so it can't be done from this list.",
      ],
    ]);
    expect(results.textContent).not.toContain("node_unreachable");
    fireEvent.click(within(results).getByRole("button", { name: "Close results" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Stop results" })).toBeNull());
  });

  it("includes the rows that were skipped before the request in the results", async () => {
    mount([row("a"), row("b", { state: "exited" })], { verb: "kill" });
    // Only one is left to kill, so this is a single request in a bulk dialog.
    fireEvent.click(await screen.findByRole("button", { name: /^Kill/ }));
    expect(await screen.findByText(/^Killed 1 container\./)).toBeTruthy();
  });

  it("shows the agent update notice inside the dialog when the agent refuses by its own policy", async () => {
    answer = (req) =>
      Response.json({
        results: [
          {
            nodeId: "n1",
            id: req.body?.targets[0]?.id,
            name: "web",
            result: "failed",
            code: "agent_policy_denied",
          },
        ],
      });
    mount([row("web")]);
    fireEvent.click(await screen.findByRole("button", { name: "Stop container" }));
    const notice = await screen.findByRole("alert");
    expect(notice.textContent).toContain("This host's agent needs an update");
    await waitFor(() => expect(notice.textContent).toContain("Ask an administrator to update it."));
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("shows the notice when the whole request is refused for the agent's policy", async () => {
    answer = () => Response.json({ status: 502, type: "agent_policy_denied" }, { status: 502 });
    mount(TWO);
    fireEvent.click(await screen.findByRole("button", { name: "Stop 2 containers" }));
    expect((await screen.findByRole("alert")).textContent).toContain(
      "This host's agent needs an update",
    );
  });
});
