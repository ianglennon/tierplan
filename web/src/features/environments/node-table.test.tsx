// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { NodeTable } from "@/features/environments/node-table";
import "@/i18n";

const NOW = new Date("2026-10-03T12:00:00Z");
const DAY = 86_400_000;
const iso = (ms: number) => new Date(NOW.getTime() + ms).toISOString();
const hex = (c: string) => c.repeat(64);

function agent(id: string, node: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    node,
    role: "worker",
    status: "active",
    blocked: false,
    connected: true,
    agentVersion: "0.1.0",
    engineVersion: "29.8.2",
    certFingerprint: hex("ab"),
    certNotAfter: iso(90 * DAY),
    clockOffsetSeconds: 2,
    lastSeen: iso(-5 * 60_000),
    readOnly: false,
    selfUpdate: true,
    ...extra,
  };
}

const MGR = agent("a1", "mgr-1", {
  role: "manager",
  certFingerprint: hex("0123456789abcdef").slice(0, 64),
});
const EXPIRING = agent("a2", "wrk-1", {
  certNotAfter: iso(3 * DAY),
  clockOffsetSeconds: -45,
  certFingerprint: hex("c4"),
});
const DOWN = agent("a3", "wrk-2", { connected: false, engineVersion: undefined });
const REVOKED = agent("a4", "wrk-3", {
  status: "revoked",
  blocked: true,
  connected: false,
  revokedAt: iso(-DAY),
  revokeReason: "lost",
  engineVersion: undefined,
});
const UNBLOCKED = agent("a5", "wrk-4", {
  status: "revoked",
  blocked: false,
  connected: false,
  revokedAt: iso(-2 * DAY),
  engineVersion: undefined,
});
const READONLY = agent("a6", "ro-1", { readOnly: true });

function env(kind: "agent" | "local") {
  return {
    id: "e2",
    name: "edge",
    kind,
    enabled: true,
    isSelfHost: kind === "local",
    isLocalSocket: kind === "local",
    createdAt: "2026-10-01T10:00:00Z",
    engine: { name: "Docker Engine", version: "28.5.1", apiVersion: "1.50" },
    status: { state: "online" },
    envType: "swarm",
    lastSeen: iso(-60_000),
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    nodes: null,
    updateAvailable: false,
  };
}

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

const ALL = ["environments.read", "environments.revoke"];
let perms: string[];
let sent: Sent[];
let agents: ReturnType<typeof agent>[];
let kind: "agent" | "local";
let agentsAnswer: () => Response | Promise<Response>;
let revokeAnswer: () => Response | Promise<Response>;
let unblockAnswer: () => Response | Promise<Response>;
let client: QueryClient;
let writeText: ReturnType<typeof vi.fn<(text: string) => Promise<void>>>;

beforeEach(() => {
  perms = ALL;
  kind = "agent";
  sent = [];
  agents = [MGR, EXPIRING, DOWN, REVOKED, UNBLOCKED, READONLY];
  agentsAnswer = () => Response.json({ items: agents });
  revokeAnswer = () => new Response(null, { status: 204 });
  unblockAnswer = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
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
      if (path === "/api/me") {
        return Response.json({
          user: { id: "u1", username: "admin", role: "admin" },
          csrfToken: "csrf-1",
          permissions: { global: [], environments: { "*": perms } },
        });
      }
      if (path === "/api/environments") return Response.json({ items: [env(kind)] });
      if (path === "/api/env/e2/agents") return agentsAnswer();
      if (/\/api\/environments\/e2\/agents\/[^/]+\/revoke$/.test(path)) return revokeAnswer();
      if (/\/api\/environments\/e2\/agents\/[^/]+\/unblock$/.test(path)) return unblockAnswer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  // The toast store is shared by every test in the file.
  toast.dismiss();
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
});

function mount(props: { onRotateToken?: () => void } = {}) {
  return render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <EnvHealthStreamProvider>
          <NodeTable envId="e2" {...props} />
          <Toaster />
        </EnvHealthStreamProvider>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

async function mountLoaded(props: { onRotateToken?: () => void } = {}) {
  mount(props);
  await screen.findByRole("table");
  await waitFor(() => expect(screen.queryByText("Loading nodes…")).toBeNull());
  await screen.findByRole("button", { name: "Actions for mgr-1" });
}

const rows = () => within(screen.getByRole("table")).getAllByRole("row").slice(1);
const cells = (row: HTMLElement) => within(row).getAllByRole("cell");
const rowFor = (node: string) => {
  const row = rows().find((r) => within(r).queryByText(node));
  if (!row) throw new Error(`no row for ${node}`);
  return row;
};
const nodeNames = () => rows().map((r) => cells(r)[0]?.textContent ?? "");
const posts = () => sent.filter((s) => s.method === "POST");
const agentReads = () => sent.filter((s) => s.method === "GET" && s.path === "/api/env/e2/agents");

async function openMenu(node: string) {
  const trigger = screen.getByRole("button", { name: `Actions for ${node}` });
  await act(async () => {
    fireEvent.keyDown(trigger, { key: "Enter" });
  });
  return trigger;
}

describe("NodeTable columns and order", () => {
  it("has the nine UI-SPEC columns", async () => {
    await mountLoaded();
    const heads = within(screen.getByRole("table"))
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(heads).toEqual([
      "Node",
      "Role",
      "Agent",
      "Agent version",
      "Engine",
      "Certificate",
      "Clock",
      "Last seen",
      "Actions",
    ]);
  });

  it("lists problems first: revoked, then not connected, then warnings, then the rest by name", async () => {
    await mountLoaded();
    expect(
      nodeNames().map((n) => n.replace(/Revoked|Blocked|Read-only \(set on host\)/g, "")),
    ).toEqual(["wrk-3", "wrk-4", "wrk-2", "wrk-1", "mgr-1", "ro-1"]);
  });

  it("shows roles, the connection state, versions and a missing value as a dash", async () => {
    await mountLoaded();
    const mgr = cells(rowFor("mgr-1"));
    expect(mgr[1]?.textContent).toBe("Manager");
    expect(mgr[2]?.textContent).toBe("Connected");
    expect(mgr[3]?.textContent).toBe("0.1.0");
    expect(mgr[4]?.textContent).toBe("29.8.2");
    expect(cells(rowFor("wrk-1"))[1]?.textContent).toBe("Worker");
    const down = cells(rowFor("wrk-2"));
    expect(down[2]?.textContent).toBe("Disconnected");
    // The engine version is reported only while connected.
    expect(down[4]?.textContent).toBe("—");
    expect(cells(rowFor("mgr-1"))[7]?.textContent).toBe("5 min ago");
  });

  it("shows No agents have connected yet. for an environment with none", async () => {
    agents = [];
    mount();
    expect(await screen.findByText("No agents have connected yet.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows 36px skeleton rows while loading", async () => {
    agentsAnswer = () => new Promise<Response>(() => {});
    mount();
    const table = await screen.findByRole("table");
    expect(table.getAttribute("aria-busy")).toBe("true");
    const skeleton = table.querySelectorAll("tbody tr");
    expect(skeleton.length).toBeGreaterThanOrEqual(3);
    expect((skeleton[0] as HTMLElement).className).toContain("h-9");
    expect(screen.queryByText("No agents have connected yet.")).toBeNull();
  });

  it("offers a retry when the list fails", async () => {
    agentsAnswer = () => new Response("nope", { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load the nodes.")).toBeTruthy();
    agentsAnswer = () => Response.json({ items: agents });
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("mgr-1")).toBeTruthy();
  });

  it("reads the list again when an agent connects or disconnects", async () => {
    await mountLoaded();
    const before = agentReads().length;
    const stream = FakeEventSource.instances.find((i) => i.url === "/api/environments/stream");
    expect(stream).toBeTruthy();
    agents = [...agents, agent("a7", "wrk-9")];
    await act(async () => {
      stream?.sendFrame(
        "agent.connected",
        { envId: "e2", at: NOW.toISOString(), agentId: "a7", node: "wrk-9" },
        "1",
      );
    });
    expect(await screen.findByText("wrk-9")).toBeTruthy();
    expect(agentReads().length).toBeGreaterThan(before);
  });
});

describe("NodeTable certificate and clock", () => {
  it("shows the first 12 hex characters, with the full SHA-256 in a tooltip", async () => {
    await mountLoaded();
    const cell = cells(rowFor("mgr-1"))[5] as HTMLElement;
    const short = within(cell).getByText(MGR.certFingerprint.slice(0, 12));
    expect(short.className).toContain("font-mono");
    expect(cell.textContent).not.toContain(MGR.certFingerprint);
    await act(async () => {
      fireEvent.focus(short);
    });
    expect(screen.getAllByText(MGR.certFingerprint).length).toBeGreaterThan(0);
  });

  it("copies the full fingerprint from a button beside it", async () => {
    await mountLoaded();
    const cell = cells(rowFor("mgr-1"))[5] as HTMLElement;
    await act(async () => {
      fireEvent.click(
        within(cell).getByRole("button", { name: "Copy certificate fingerprint of mgr-1" }),
      );
    });
    expect(writeText).toHaveBeenCalledWith(MGR.certFingerprint);
  });

  it("shows the expiry and, under 7 days, in the warning colour", async () => {
    await mountLoaded();
    const ok = within(cells(rowFor("mgr-1"))[5] as HTMLElement).getByText("Expires in 90 days");
    expect(ok.classList.contains("text-status-warning-text")).toBe(false);
    const soon = within(cells(rowFor("wrk-1"))[5] as HTMLElement).getByText("Expires in 3 days");
    expect(soon.classList.contains("text-status-warning-text")).toBe(true);
  });

  it("shows the clock as a signed value and flags one above 30 seconds as Clock off", async () => {
    await mountLoaded();
    expect(cells(rowFor("mgr-1"))[6]?.textContent).toBe("+2 s");
    const off = cells(rowFor("wrk-1"))[6] as HTMLElement;
    expect(off.textContent).toContain("−45 s");
    const label = within(off).getByText("Clock off");
    expect(label.className).toContain("text-status-warning-text");
    expect(within(cells(rowFor("mgr-1"))[6] as HTMLElement).queryByText("Clock off")).toBeNull();
  });

  it("shows a dash for the clock of an agent that has not connected since the server started", async () => {
    agents = [agent("a9", "fresh", { connected: false, clockOffsetSeconds: 0 })];
    mount();
    await screen.findByText("fresh");
    expect(cells(rowFor("fresh"))[6]?.textContent).toBe("—");
  });
});

describe("NodeTable badges", () => {
  it("shows Revoked and Blocked and a dash for the agent, certificate and clock", async () => {
    await mountLoaded();
    const row = rowFor("wrk-3");
    const c = cells(row);
    expect(within(c[0] as HTMLElement).getByText("Revoked")).toBeTruthy();
    expect(within(c[0] as HTMLElement).getByText("Blocked")).toBeTruthy();
    expect(c[2]?.textContent).toBe("—");
    expect(c[5]?.textContent).toBe("—");
    expect(c[6]?.textContent).toBe("—");
    // Revoked but no longer blocked: only the first badge.
    const unblocked = cells(rowFor("wrk-4"))[0] as HTMLElement;
    expect(within(unblocked).getByText("Revoked")).toBeTruthy();
    expect(within(unblocked).queryByText("Blocked")).toBeNull();
  });

  it("shows Read-only (set on host) with its tooltip for an agent that reports it", async () => {
    await mountLoaded();
    const badge = within(cells(rowFor("ro-1"))[0] as HTMLElement).getByText(
      "Read-only (set on host)",
    );
    await act(async () => {
      fireEvent.focus(badge);
    });
    expect(
      screen.getAllByText(
        "BAYPLAN_AGENT_READ_ONLY is set on this host. The agent refuses every change Bayplan asks for.",
      ).length,
    ).toBeGreaterThan(0);
    expect(within(cells(rowFor("mgr-1"))[0] as HTMLElement).queryByText(/Read-only/)).toBeNull();
  });

  it("renders hostile hostnames and versions as text, never as markup", async () => {
    agents = [
      agent("x1", "<img src=x onerror=alert(1)>", {
        agentVersion: "<script>alert(2)</script>",
        engineVersion: "<b>29</b>",
      }),
    ];
    mount();
    const node = await screen.findByText("<img src=x onerror=alert(1)>");
    expect(node.tagName).not.toBe("IMG");
    const table = screen.getByRole("table");
    expect(table.querySelector("img, script, b")).toBeNull();
    expect(screen.getByText("<script>alert(2)</script>")).toBeTruthy();
  });
});

describe("NodeTable local socket", () => {
  it("shows one Local socket row with dashes and no revoke action", async () => {
    kind = "local";
    agents = [];
    mount();
    await screen.findByRole("table");
    await screen.findByText("Local socket");
    const [row] = rows();
    const c = cells(row as HTMLElement);
    expect(c[0]?.textContent).toBe("edge");
    expect(c[1]?.textContent).toBe("Local socket");
    expect(c[2]?.textContent).toBe("—");
    expect(c[5]?.textContent).toBe("—");
    expect(c[6]?.textContent).toBe("—");
    expect(within(row as HTMLElement).queryByRole("button")).toBeNull();
    expect(screen.queryByText("No agents have connected yet.")).toBeNull();
  });
});

describe("NodeTable row actions", () => {
  it("names the trigger Actions for {node} and offers Revoke node on an active row", async () => {
    await mountLoaded();
    await openMenu("mgr-1");
    expect(await screen.findByRole("menuitem", { name: "Revoke node" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Unblock node" })).toBeNull();
  });

  it("offers Unblock node, and not Revoke node, on a blocked row", async () => {
    await mountLoaded();
    await openMenu("wrk-3");
    expect(await screen.findByRole("menuitem", { name: "Unblock node" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Revoke node" })).toBeNull();
  });

  it("offers nothing on a revoked row that is not blocked", async () => {
    await mountLoaded();
    expect(screen.queryByRole("button", { name: "Actions for wrk-4" })).toBeNull();
  });

  it("has no actions at all without environments.revoke", async () => {
    perms = ["environments.read"];
    mount();
    await screen.findByText("mgr-1");
    expect(screen.queryByRole("button", { name: /^Actions for/ })).toBeNull();
  });

  it("makes the trigger 44px high under 768px", async () => {
    await mountLoaded();
    const trigger = screen.getByRole("button", { name: "Actions for mgr-1" });
    expect(trigger.className).toContain("max-md:size-11");
  });
});

describe("NodeTable revoke", () => {
  async function openRevoke(node = "mgr-1") {
    await openMenu(node);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Revoke node" }));
    return screen.findByRole("alertdialog");
  }

  it("opens an alert-dialog naming the node, with focus on Cancel and the UI-SPEC body", async () => {
    await mountLoaded();
    const dialog = await openRevoke();
    expect(within(dialog).getByRole("heading", { name: "Revoke mgr-1?" })).toBeTruthy();
    expect(
      within(dialog).getByText(
        "mgr-1 is disconnected now and its certificate is rejected for good. It can't enrol again until you unblock it. If the host itself may be compromised, also rotate the join token.",
      ),
    ).toBeTruthy();
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    expect(posts()).toEqual([]);
  });

  it("posts the revoke once, with a spinner, then closes and toasts with Rotate join token", async () => {
    let release: (r: Response) => void = () => {};
    revokeAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    const onRotateToken = vi.fn();
    await mountLoaded({ onRotateToken });
    const dialog = await openRevoke();
    const confirm = within(dialog).getByRole("button", { name: "Revoke node" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);

    const pending = await within(dialog).findByRole("button", { name: "Revoking…" });
    expect(pending.matches(":disabled")).toBe(true);
    expect(within(dialog).getByRole("button", { name: "Cancel" }).matches(":disabled")).toBe(true);
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.path).toBe("/api/environments/e2/agents/a1/revoke");

    await act(async () => release(new Response(null, { status: 204 })));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(await screen.findByText("mgr-1 was revoked.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Rotate join token" }));
    expect(onRotateToken).toHaveBeenCalledTimes(1);
  });

  it("reads the list again after a revoke so the row changes at once", async () => {
    await mountLoaded();
    const before = agentReads().length;
    agents = agents.map((a) =>
      a.id === "a1" ? { ...a, status: "revoked", blocked: true, connected: false } : a,
    );
    const dialog = await openRevoke();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(agentReads().length).toBeGreaterThan(before));
    await waitFor(() =>
      expect(within(cells(rowFor("mgr-1"))[0] as HTMLElement).getByText("Revoked")).toBeTruthy(),
    );
  });

  it("keeps the dialog open with the concurrency message on 409 agent_already_revoked, and shows no toast", async () => {
    revokeAnswer = () =>
      Response.json(
        { status: 409, type: "agent_already_revoked", title: "Conflict" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    await mountLoaded();
    const dialog = await openRevoke();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("That node was revoked while you were looking at it.");
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(screen.queryByText("mgr-1 was revoked.")).toBeNull();
  });

  it("keeps the dialog open with a general message on any other failure", async () => {
    revokeAnswer = () => Response.json({ status: 500, title: "boom" }, { status: 500 });
    await mountLoaded();
    const dialog = await openRevoke();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("Couldn't revoke the node. Try again.");
    expect(within(dialog).getByRole("button", { name: "Revoke node" }).matches(":disabled")).toBe(
      false,
    );
  });

  it("cancels on Escape without sending anything", async () => {
    await mountLoaded();
    const dialog = await openRevoke();
    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(posts()).toEqual([]);
  });

  it("styles only the confirm as destructive", async () => {
    await mountLoaded();
    const dialog = await openRevoke();
    const confirm = within(dialog).getByRole("button", { name: "Revoke node" });
    expect(confirm.getAttribute("data-variant")).toBe("destructive");
    expect(
      within(dialog).getByRole("button", { name: "Cancel" }).getAttribute("data-variant"),
    ).not.toBe("destructive");
  });
});

describe("NodeTable unblock", () => {
  async function openUnblock() {
    await openMenu("wrk-3");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unblock node" }));
    return screen.findByRole("dialog");
  }

  it("opens a dialog (not an alert-dialog) that says the revoked agent is not revived", async () => {
    await mountLoaded();
    const dialog = await openUnblock();
    expect(within(dialog).getByRole("heading", { name: "Unblock wrk-3?" })).toBeTruthy();
    expect(dialog.textContent).toContain("wrk-3 can enrol again with the join token.");
    expect(dialog.textContent).toContain("install the agent on the host again");
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(
      within(dialog).getByRole("button", { name: "Unblock node" }).getAttribute("data-variant"),
    ).not.toBe("destructive");
  });

  it("posts the unblock once and closes", async () => {
    await mountLoaded();
    const dialog = await openUnblock();
    const confirm = within(dialog).getByRole("button", { name: "Unblock node" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.path).toBe("/api/environments/e2/agents/a4/unblock");
    expect(await screen.findByText("wrk-3 was unblocked.")).toBeTruthy();
  });

  it("keeps the dialog open with an inline alert on not_blocked", async () => {
    unblockAnswer = () =>
      Response.json(
        { status: 409, type: "not_blocked", title: "Conflict" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    await mountLoaded();
    const dialog = await openUnblock();
    fireEvent.click(within(dialog).getByRole("button", { name: "Unblock node" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("That node was already unblocked.");
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("keeps the dialog open with a general message on another failure", async () => {
    unblockAnswer = () => Response.json({ status: 500, title: "boom" }, { status: 500 });
    await mountLoaded();
    const dialog = await openUnblock();
    fireEvent.click(within(dialog).getByRole("button", { name: "Unblock node" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Couldn't unblock the node. Try again.",
    );
  });
});

describe("NodeTable on a large swarm", () => {
  it("keeps 50 nodes inside a scrolling card, problems first, every row's actions reachable", async () => {
    agents = Array.from({ length: 49 }, (_, i) =>
      agent(`n${i}`, `node-${String(i).padStart(2, "0")}`),
    );
    agents.push(DOWN);
    mount();
    await screen.findByText("node-00");
    expect(rows()).toHaveLength(50);
    expect(nodeNames()[0]).toBe("wrk-2");
    const table = screen.getByRole("table");
    const scroller = table.closest("[data-scroll]") as HTMLElement | null;
    expect(scroller).toBeTruthy();
    expect(scroller?.className).toContain("overflow-auto");
    expect(scroller?.className).toContain("max-h-");
    expect(screen.getAllByRole("button", { name: /^Actions for / })).toHaveLength(50);
  });
});
