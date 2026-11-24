// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { NodeImpact, NodeImpactAction } from "@/api/nodes";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodeActionDialog } from "@/features/nodes/node-action-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

// The virtual IP in the Drain dialog (plan 26-27, VIP-04, UI-SPEC S3): two warnings before a
// drain of the node that holds the address, and one calm toast when the handoff could not move it
// first. The drain itself is never shown as failed because of the handoff (D-25).

const toasts = vi.hoisted(() => ({
  toastSuccess: vi.fn(),
  toastInfo: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@/lib/toast", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/toast")>()),
  ...toasts,
}));

const ENV = "e1";
const NODE = { id: "n3", hostname: "wkr-1" };

const impact = (extra: Partial<NodeImpact> = {}): NodeImpact => ({
  action: "drain",
  hostname: "wkr-1",
  version: 4,
  tasks: [],
  managers: { total: 3, reachable: 3, after: 3 },
  warnings: [],
  needsConfirm: false,
  ...extra,
});

const HOLDS = "wkr-1 holds the virtual IP. TierPlan moves it to another ready node first, ";
const HOLDS_ALONE =
  "wkr-1 holds the virtual IP and no other node is ready to take it. Draining it leaves the address unanswered until a proxy is ready on another node.";

let answerImpact: () => Response;
let answerPost: () => Response;
let impactGate: Promise<void> | null;
let posted: number;

beforeEach(() => {
  answerImpact = () => Response.json(impact());
  answerPost = () => new Response(null, { status: 204 });
  impactGate = null;
  posted = 0;
  for (const fn of Object.values(toasts)) fn.mockClear();
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      }
      if (url.pathname === `/api/env/${ENV}/nodes/${NODE.id}/impact`) {
        if (impactGate) await impactGate;
        return answerImpact();
      }
      if (req.method === "POST" && url.pathname.startsWith(`/api/env/${ENV}/nodes/${NODE.id}/`)) {
        posted += 1;
        return answerPost();
      }
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm one", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(mode: NodeImpactAction = "drain", node = NODE) {
  const onClose = vi.fn();
  const onDone = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <NodeActionDialog envId={ENV} node={node} mode={mode} onClose={onClose} onDone={onDone} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onClose, onDone };
}

async function dialogOf(name: string) {
  const dialog = await screen.findByRole("dialog", { name });
  await waitFor(() => expect(within(dialog).queryByText("Checking what this will do…")).toBeNull());
  return dialog;
}

describe("NodeActionDialog virtual IP warnings", () => {
  it("says the node holds the virtual IP and where it goes first", async () => {
    answerImpact = () => Response.json(impact({ warnings: ["holds_vip"] }));
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(
      within(dialog).getByText(`${HOLDS}and the address may go unanswered for a few seconds.`),
    ).toBeTruthy();
    expect(within(dialog).queryByText(HOLDS_ALONE)).toBeNull();
  });

  it("says no other node is ready to take it when the node is alone", async () => {
    answerImpact = () => Response.json(impact({ warnings: ["holds_vip_alone"] }));
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(within(dialog).getByText(HOLDS_ALONE)).toBeTruthy();
    expect(within(dialog).queryByText(/TierPlan moves it to another ready node/)).toBeNull();
  });

  it("draws the warnings in the order the server sent them", async () => {
    answerImpact = () => Response.json(impact({ warnings: ["holds_vip", "runs_tierplan"] }));
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    const first = within(dialog).getByText(/holds the virtual IP/);
    const second = within(dialog).getByText(/TierPlan runs on wkr-1/);
    expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps the plain confirm, with no typed name, because the drain still proceeds", async () => {
    answerImpact = () => Response.json(impact({ warnings: ["holds_vip"], needsConfirm: false }));
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    const confirm = within(dialog).getByRole("button", { name: "Drain node" });
    expect(confirm.className).not.toContain("bg-destructive-fill");
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");
  });

  it("shows nothing extra for a drain of a node that holds no address", async () => {
    mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    expect(within(dialog).queryByText(/virtual IP/)).toBeNull();
  });

  it("never shows them for a pause", async () => {
    answerImpact = () => Response.json(impact({ action: "pause", warnings: [] }));
    mount("pause");
    const dialog = await dialogOf("Pause wkr-1?");
    expect(within(dialog).queryByText(/virtual IP/)).toBeNull();
  });

  it("holds the spinner and the confirm off until the answer arrives, so the warning is not skipped", async () => {
    let open: () => void = () => {};
    impactGate = new Promise<void>((resolve) => {
      open = resolve;
    });
    answerImpact = () => Response.json(impact({ warnings: ["holds_vip"] }));
    mount("drain");
    const dialog = await screen.findByRole("dialog", { name: "Drain wkr-1?" });
    expect(within(dialog).getByText("Checking what this will do…")).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Drain node" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(posted).toBe(0);
    open();
    expect(await within(dialog).findByText(/wkr-1 holds the virtual IP/)).toBeTruthy();
  });

  it("wraps the warning for a 64-character node name inside the alert, never cutting it", async () => {
    const long = "w".repeat(64);
    answerImpact = () => Response.json(impact({ hostname: long, warnings: ["holds_vip"] }));
    mount("drain", { id: "n3", hostname: long });
    const dialog = await dialogOf(`Drain ${long}?`);
    const text = within(dialog).getByText(new RegExp(`^${long} holds the virtual IP`));
    expect(text.textContent).toContain(long);
    expect(text.className).not.toContain("truncate");
    expect(text.className).not.toContain("line-clamp");
  });
});

describe("NodeActionDialog drain result", () => {
  async function drain(name = "wkr-1", mode: NodeImpactAction = "drain") {
    const { onClose, onDone } = mount(mode, { id: "n3", hostname: name });
    const dialog = await dialogOf(mode === "drain" ? `Drain ${name}?` : `Pause ${name}?`);
    fireEvent.click(
      within(dialog).getByRole("button", { name: mode === "drain" ? "Drain node" : "Pause node" }),
    );
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    return { onClose, onDone };
  }

  const handoff = (outcome: string, holder?: string) => () =>
    Response.json({ vipHandoff: { outcome, ...(holder ? { holder } : {}) } });

  it("shows the success toast for a 204", async () => {
    const { onDone } = await drain();
    expect(toasts.toastSuccess).toHaveBeenCalledExactlyOnceWith("Drained wkr-1.");
    expect(toasts.toastInfo).not.toHaveBeenCalled();
    expect(toasts.toastError).not.toHaveBeenCalled();
    expect(onDone).toHaveBeenCalledWith({ nodeId: "n3", action: "drain" });
  });

  it("shows the same success toast when the handoff moved the address", async () => {
    answerPost = handoff("moved", "wkr-2");
    await drain();
    expect(toasts.toastSuccess).toHaveBeenCalledExactlyOnceWith("Drained wkr-1.");
    expect(toasts.toastInfo).not.toHaveBeenCalled();
    expect(toasts.toastError).not.toHaveBeenCalled();
  });

  it.each([
    [
      "no_candidate",
      "Drained wkr-1. No other node was ready to take the virtual IP, so it may go unanswered until one is.",
    ],
    [
      "agent_unreachable",
      "Drained wkr-1. TierPlan couldn't reach its agent to move the virtual IP first, so the address may drop for a few seconds.",
    ],
    [
      "timeout",
      "Drained wkr-1. The virtual IP hadn't moved after 10 seconds, so the address may drop for a few seconds.",
    ],
  ])("shows one info toast for %s, never an error", async (outcome, text) => {
    answerPost = handoff(outcome);
    const { onClose, onDone } = await drain();
    expect(toasts.toastInfo).toHaveBeenCalledExactlyOnceWith(text);
    expect(toasts.toastSuccess).not.toHaveBeenCalled();
    expect(toasts.toastError).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
    expect(onDone).toHaveBeenCalledWith({ nodeId: "n3", action: "drain" });
  });

  it("carries a 64-character node name whole into the toast, with its point in the first two lines", async () => {
    const long = "w".repeat(64);
    answerImpact = () => Response.json(impact({ hostname: long }));
    answerPost = handoff("timeout");
    await drain(long);
    const [text] = toasts.toastInfo.mock.calls[0] as [string];
    expect(text).toContain(long);
    expect(text).not.toContain("…");
    // The name wraps with break-words; the sentence that matters follows it at once.
    expect(text.startsWith(`Drained ${long}. The virtual IP hadn't moved after 10 seconds`)).toBe(
      true,
    );
  });

  it("does not read a handoff for a pause", async () => {
    answerImpact = () => Response.json(impact({ action: "pause" }));
    await drain("wkr-1", "pause");
    expect(toasts.toastSuccess).toHaveBeenCalledExactlyOnceWith("Paused wkr-1.");
    expect(toasts.toastInfo).not.toHaveBeenCalled();
  });

  it("keeps the in-dialog alert and no toast when the drain is refused", async () => {
    answerPost = () =>
      Response.json({ status: 409, type: "node_changed", title: "x" }, { status: 409 });
    const { onClose } = mount("drain");
    const dialog = await dialogOf("Drain wkr-1?");
    fireEvent.click(within(dialog).getByRole("button", { name: "Drain node" }));
    expect(
      await within(dialog).findByText(
        "That changed while you were looking at it. Reload the page and try again.",
      ),
    ).toBeTruthy();
    expect(onClose).not.toHaveBeenCalled();
    expect(toasts.toastSuccess).not.toHaveBeenCalled();
    expect(toasts.toastInfo).not.toHaveBeenCalled();
  });
});
