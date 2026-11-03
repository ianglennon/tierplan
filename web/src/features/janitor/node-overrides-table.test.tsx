// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { JanitorNode, JanitorPolicy } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { PolicyCard } from "@/features/janitor/policy-card";
import "@/i18n";
import { janitorPolicy, preview, previewItem } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const problem = (type: string, status: number) =>
  new Response(JSON.stringify({ type, title: "Error", status }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

// A node as the policy lists it: reporting, and following the environment (on, keeping the
// environment's 2) unless a test says otherwise.
function node(over: Partial<JanitorNode> & { nodeId: string; name: string }): JanitorNode {
  return {
    reporting: true,
    enabled: { value: true, overridden: false },
    keepN: { value: 2, overridden: false },
    ...over,
  };
}

// What a node reads like after the server stored { enabled, keepN } for it: a field sent as null
// follows the environment (on, keeping the environment's keep-N).
function stored(
  from: JanitorNode,
  body: { enabled?: boolean | null; keepN?: number | null },
  envKeep: number,
) {
  return {
    ...from,
    enabled: { value: body.enabled ?? true, overridden: body.enabled != null },
    keepN: { value: body.keepN ?? envKeep, overridden: body.keepN != null },
  };
}

let me: ReturnType<typeof makeMe>;
let policyBody: JanitorPolicy;
let puts: { nodeId: string; body: Record<string, unknown> }[];
let deletes: string[];
let previews: (Record<string, unknown> | null)[];
let previewBody: ReturnType<typeof preview>;
// What the stubbed server answers to a node change instead of the usual.
let nodeAnswer: (() => Response) | undefined;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  policyBody = janitorPolicy({
    nodes: [
      node({ nodeId: "n1", name: "node-1" }),
      node({ nodeId: "n2", name: "node-2" }),
      node({ nodeId: "n3", name: "node-3", reporting: false }),
    ],
  });
  puts = [];
  deletes = [];
  previews = [];
  nodeAnswer = undefined;
  previewBody = preview({ items: [previewItem({ name: "web.1.aaa", ref: "c1" })] });
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/janitor/policy` && req.method === "GET") {
        return Response.json(policyBody);
      }
      const m = /\/janitor\/nodes\/([^/]+)$/.exec(url.pathname);
      if (m?.[1] && (req.method === "PUT" || req.method === "DELETE")) {
        const nodeId = m[1];
        const found = (policyBody.nodes ?? []).find((n) => n.nodeId === nodeId);
        if (!found) return problem("node_not_found", 404);
        let body: Record<string, unknown> = {};
        if (req.method === "PUT") {
          body = (await req.json()) as Record<string, unknown>;
          puts.push({ nodeId, body });
        } else {
          deletes.push(nodeId);
        }
        if (nodeAnswer) return nodeAnswer();
        const next = stored(found, body, policyBody.keepN.value);
        policyBody = {
          ...policyBody,
          nodes: (policyBody.nodes ?? []).map((n) => (n.nodeId === nodeId ? next : n)),
        };
        return Response.json(next);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/preview`) {
        const text = await req.text();
        previews.push(text ? (JSON.parse(text) as Record<string, unknown>) : null);
        return Response.json(previewBody);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(props: { swarm?: boolean } = {}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <TestRouter>
          <PolicyCard envId={ENV} envName="Prod" swarm={props.swarm ?? true} nodeCount={3} />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const table = () => screen.findByRole("table", { name: "Node overrides" });
const rowOf = (name: string) => screen.getByRole("row", { name: new RegExp(`^${name}\\b`) });
const switchOf = (name: string) => screen.getByRole("switch", { name: `Cleanup on ${name}` });
const keepOf = (name: string) =>
  screen.getByRole("spinbutton", { name: `Keep newest on ${name}` }) as HTMLInputElement;
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
// A keep-newest number is applied when the person leaves the field.
const typeAndLeave = (el: HTMLElement, value: string) => {
  type(el, value);
  fireEvent.blur(el);
};

describe("NodeOverridesRows", () => {
  it("draws the Nodes section under the policy rows with a row per node and the state of each", async () => {
    mount();
    const t = await table();
    expect(screen.getByRole("heading", { name: "Nodes" })).toBeTruthy();
    expect(
      screen.getByText(
        "Skip a fragile node, or keep more of its task containers. Every other setting follows the environment.",
      ),
    ).toBeTruthy();
    const heads = within(t)
      .getAllByRole("columnheader")
      .map((h) => ({ text: h.textContent, scope: h.getAttribute("scope") }));
    expect(heads.slice(0, 4)).toEqual([
      { text: "Node", scope: "col" },
      { text: "Cleanup", scope: "col" },
      { text: "Keep newest", scope: "col" },
      { text: "State", scope: "col" },
    ]);
    // One header row and three node rows.
    expect(within(t).getAllByRole("row")).toHaveLength(4);
    for (const name of ["node-1", "node-2", "node-3"]) {
      expect(switchOf(name).getAttribute("aria-checked")).toBe("true");
      expect(keepOf(name).value).toBe("2");
      expect(within(rowOf(name)).getByText("Default")).toBeTruthy();
    }
    expect(within(rowOf("node-1")).getByText("On")).toBeTruthy();
    // Only the node no agent reaches says so.
    expect(within(rowOf("node-3")).getByText("Not reporting")).toBeTruthy();
    expect(within(rowOf("node-1")).queryByText("Not reporting")).toBeNull();
    // A node that follows the environment has nothing to reset.
    expect(screen.queryByRole("button", { name: /^Reset node-\d to default$/ })).toBeNull();
  });

  it("reads a node that set its own values as Overridden, Off and with its number", async () => {
    policyBody = janitorPolicy({
      nodes: [
        node({
          nodeId: "n1",
          name: "node-1",
          enabled: { value: false, overridden: true },
          keepN: { value: 6, overridden: true },
        }),
      ],
    });
    mount();
    await table();
    expect(switchOf("node-1").getAttribute("aria-checked")).toBe("false");
    expect(within(rowOf("node-1")).getByText("Off")).toBeTruthy();
    expect(keepOf("node-1").value).toBe("6");
    expect(within(rowOf("node-1")).getByText("Overridden")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reset node-1 to default" })).toBeTruthy();
  });

  it("shows a node that follows an environment that is switched off as still cleaned", async () => {
    // The server reports the environment's own switch as the node's value; Clean now still cleans
    // the node, so the row must not say Off.
    policyBody = janitorPolicy({
      nodes: [node({ nodeId: "n1", name: "node-1", enabled: { value: false, overridden: false } })],
    });
    mount();
    await table();
    expect(switchOf("node-1").getAttribute("aria-checked")).toBe("true");
  });

  it("draws no Nodes section on a standalone environment", async () => {
    mount({ swarm: false });
    expect(
      await screen.findByRole("spinbutton", { name: "Exited containers to keep" }),
    ).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Node overrides" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Nodes" })).toBeNull();
  });

  it("draws no Nodes section on a swarm whose nodes cannot be read", async () => {
    policyBody = janitorPolicy({ nodes: null });
    mount();
    expect(
      await screen.findByRole("spinbutton", { name: "Exited task containers to keep" }),
    ).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Node overrides" })).toBeNull();
  });
});

describe("NodeOverridesSwitchOff", () => {
  it("turns a node off at once, with no preview, and reads Overridden", async () => {
    mount();
    await table();
    fireEvent.click(switchOf("node-1"));
    await waitFor(() =>
      expect(puts).toEqual([{ nodeId: "n1", body: { enabled: false, keepN: null } }]),
    );
    expect(previews).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(within(rowOf("node-1")).getByText("Overridden")).toBeTruthy());
    expect(switchOf("node-1").getAttribute("aria-checked")).toBe("false");
    expect(within(rowOf("node-1")).getByText("Off")).toBeTruthy();
    // The other nodes are untouched.
    expect(within(rowOf("node-2")).getByText("Default")).toBeTruthy();
    expect(toasts.success).toHaveBeenCalledWith("Saved the janitor setting for node-1.");
  });

  it("keeps a node's own number when it is switched off", async () => {
    policyBody = janitorPolicy({
      nodes: [node({ nodeId: "n1", name: "node-1", keepN: { value: 4, overridden: true } })],
    });
    mount();
    await table();
    fireEvent.click(switchOf("node-1"));
    await waitFor(() =>
      expect(puts).toEqual([{ nodeId: "n1", body: { enabled: false, keepN: 4 } }]),
    );
  });
});

describe("NodeOverridesSwitchOnReview", () => {
  const offNode = () =>
    node({
      nodeId: "n1",
      name: "node-1",
      enabled: { value: false, overridden: true },
      keepN: { value: 4, overridden: true },
    });

  it("previews the candidate node before turning a node back on, and saves it from the sheet", async () => {
    policyBody = janitorPolicy({ nodes: [offNode()] });
    mount();
    await table();
    fireEvent.click(switchOf("node-1"));

    // The sheet asks what the node would clean, judged by exactly the settings a save would store.
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    await waitFor(() => expect(previews).toHaveLength(1));
    expect(previews[0]).toEqual({ nodes: { n1: { enabled: null, keepN: 4 } } });
    expect(puts).toHaveLength(0);
    expect(await within(sheet).findAllByText(/^Would remove 1 container/)).not.toHaveLength(0);

    fireEvent.click(within(sheet).getByRole("button", { name: "Save policy" }));
    await waitFor(() =>
      expect(puts).toEqual([{ nodeId: "n1", body: { enabled: null, keepN: 4 } }]),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(switchOf("node-1").getAttribute("aria-checked")).toBe("true");
    expect(toasts.success).toHaveBeenCalledWith("Saved the janitor setting for node-1.");
  });

  it("saves nothing when the sheet is closed without saving", async () => {
    policyBody = janitorPolicy({ nodes: [offNode()] });
    mount();
    await table();
    fireEvent.click(switchOf("node-1"));
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    fireEvent.keyDown(sheet, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(puts).toHaveLength(0);
    // The switch still says what is saved.
    expect(switchOf("node-1").getAttribute("aria-checked")).toBe("false");
  });
});

describe("NodeOverridesKeepNewest", () => {
  it("previews a lower number before saving it", async () => {
    mount();
    await table();
    typeAndLeave(keepOf("node-1"), "1");
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    await waitFor(() => expect(previews).toHaveLength(1));
    expect(previews[0]).toEqual({ nodes: { n1: { enabled: null, keepN: 1 } } });
    expect(puts).toHaveLength(0);
    fireEvent.click(within(sheet).getByRole("button", { name: "Save policy" }));
    await waitFor(() =>
      expect(puts).toEqual([{ nodeId: "n1", body: { enabled: null, keepN: 1 } }]),
    );
    await waitFor(() => expect(keepOf("node-1").value).toBe("1"));
    expect(within(rowOf("node-1")).getByText("Overridden")).toBeTruthy();
  });

  it("saves a higher number at once, on Enter or on leaving the field", async () => {
    mount();
    await table();
    type(keepOf("node-1"), "5");
    fireEvent.keyDown(keepOf("node-1"), { key: "Enter" });
    await waitFor(() =>
      expect(puts).toEqual([{ nodeId: "n1", body: { enabled: null, keepN: 5 } }]),
    );
    expect(previews).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();

    typeAndLeave(keepOf("node-2"), "3");
    await waitFor(() => expect(puts).toHaveLength(2));
    expect(puts[1]).toEqual({ nodeId: "n2", body: { enabled: null, keepN: 3 } });
  });

  it("does nothing when the number is left as it was, or typed back", async () => {
    mount();
    await table();
    fireEvent.blur(keepOf("node-1"));
    typeAndLeave(keepOf("node-1"), "2");
    expect(puts).toHaveLength(0);
    expect(previews).toHaveLength(0);
    expect(keepOf("node-1").value).toBe("2");
  });

  it("refuses a number outside 0 to 20 and sends nothing", async () => {
    mount();
    await table();
    typeAndLeave(keepOf("node-1"), "21");
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Use a whole number from 0 to 20.");
    expect(keepOf("node-1").getAttribute("aria-invalid")).toBe("true");
    expect(puts).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
    // Typing a good number clears it.
    type(keepOf("node-1"), "3");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not preview lowering the number of a node that is switched off", async () => {
    policyBody = janitorPolicy({
      nodes: [
        node({
          nodeId: "n1",
          name: "node-1",
          enabled: { value: false, overridden: true },
          keepN: { value: 4, overridden: true },
        }),
      ],
    });
    mount();
    await table();
    typeAndLeave(keepOf("node-1"), "1");
    await waitFor(() =>
      expect(puts).toEqual([{ nodeId: "n1", body: { enabled: false, keepN: 1 } }]),
    );
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("NodeOverridesReset", () => {
  it("deletes the node's settings, reads Default and moves focus to the row's switch", async () => {
    policyBody = janitorPolicy({
      nodes: [node({ nodeId: "n1", name: "node-1", keepN: { value: 1, overridden: true } })],
    });
    mount();
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Reset node-1 to default" }));
    await waitFor(() => expect(deletes).toEqual(["n1"]));
    expect(previews).toHaveLength(0);
    await waitFor(() => expect(within(rowOf("node-1")).getByText("Default")).toBeTruthy());
    expect(keepOf("node-1").value).toBe("2");
    expect(screen.queryByRole("button", { name: "Reset node-1 to default" })).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(switchOf("node-1")));
    expect(toasts.success).toHaveBeenCalledWith("Saved the janitor setting for node-1.");
  });

  it("previews a reset that would switch a node back on", async () => {
    policyBody = janitorPolicy({
      nodes: [node({ nodeId: "n1", name: "node-1", enabled: { value: false, overridden: true } })],
    });
    mount();
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Reset node-1 to default" }));
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    await waitFor(() => expect(previews).toHaveLength(1));
    expect(previews[0]).toEqual({ nodes: { n1: { enabled: null, keepN: null } } });
    expect(deletes).toHaveLength(0);
    fireEvent.click(within(sheet).getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(deletes).toEqual(["n1"]));
  });

  it("previews a reset that would lower a node's number to the environment's", async () => {
    policyBody = janitorPolicy({
      nodes: [node({ nodeId: "n1", name: "node-1", keepN: { value: 5, overridden: true } })],
      keepN: { value: 1, overridden: false },
    });
    mount();
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Reset node-1 to default" }));
    expect(
      await screen.findByRole("dialog", { name: "What this policy would remove" }),
    ).toBeTruthy();
    expect(deletes).toHaveLength(0);
  });
});

describe("NodeOverridesFailures", () => {
  it("words a refused change, leaves the row as saved and does not toast success", async () => {
    nodeAnswer = () => problem("internal", 500);
    mount();
    await table();
    fireEvent.click(switchOf("node-1"));
    const alert = await screen.findByText(/^Couldn't change the janitor setting for node-1\./);
    expect(alert.textContent).toBe("Couldn't change the janitor setting for node-1. Try again.");
    expect(alert.getAttribute("role")).toBe("alert");
    expect(switchOf("node-1").getAttribute("aria-checked")).toBe("true");
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("says a node that has left the swarm is gone", async () => {
    nodeAnswer = () => problem("node_not_found", 404);
    mount();
    await table();
    fireEvent.click(switchOf("node-2"));
    expect(
      await screen.findByText(
        "node-2 is no longer in this swarm. Reload the page to see the current nodes.",
      ),
    ).toBeTruthy();
  });
});

describe("NodeOverridesNeedsPolicy", () => {
  const reason = "Needs the janitor policy permission on Prod";

  it("shows the nodes read-only and changes nothing without janitor.policy", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    policyBody = janitorPolicy({
      nodes: [node({ nodeId: "n1", name: "node-1", keepN: { value: 5, overridden: true } })],
    });
    mount();
    await table();
    expect(switchOf("node-1").getAttribute("aria-disabled")).toBe("true");
    expect(keepOf("node-1").readOnly).toBe(true);
    expect(
      screen.getByRole("button", { name: "Reset node-1 to default" }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(screen.getAllByText(reason).length).toBeGreaterThan(0);

    fireEvent.click(switchOf("node-1"));
    fireEvent.click(screen.getByRole("button", { name: "Reset node-1 to default" }));
    typeAndLeave(keepOf("node-1"), "1");
    expect(keepOf("node-1").value).toBe("5");
    expect(toasts.info).toHaveBeenCalledWith(reason);
    expect(puts).toHaveLength(0);
    expect(deletes).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
