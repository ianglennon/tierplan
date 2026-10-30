// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { FoldCard } from "@/features/environments/fold-card";
import "@/i18n";

function env(extra: Record<string, unknown>): EnvironmentItem {
  return {
    id: "e2",
    name: "edge swarm",
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state: "online" },
    envType: "swarm",
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    foldOffered: true,
    ...extra,
  } as EnvironmentItem;
}

const SWARM = env({});
const LOCAL = env({
  id: "e1",
  name: "lab-1",
  kind: "local",
  isSelfHost: true,
  isLocalSocket: true,
  envType: "swarm",
});

interface Sent {
  method: string;
  path: string;
  csrf: string | null;
}

let sent: Sent[];
let answer: () => Response | Promise<Response>;

beforeEach(() => {
  sent = [];
  answer = () => Response.json({ ...SWARM, foldOffered: undefined });
  setCsrfToken("csrf-1");
  queryClient.setQueryData(environmentsQuery.queryKey, { items: [LOCAL, SWARM] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ method: req.method, path, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/environments/e2/fold-local") return answer();
      return Response.json({ items: [] });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
  setCsrfToken(null);
});

function show(target: EnvironmentItem = SWARM, onFolded = vi.fn()) {
  render(
    <QueryClientProvider client={queryClient}>
      <FoldCard env={target} onFolded={onFolded} />
      <Toaster />
    </QueryClientProvider>,
  );
  return onFolded;
}

const posts = () => sent.filter((s) => s.method === "POST");

describe("FoldCard", () => {
  it("says this host is a manager of the swarm and offers Fold into {swarm} under the Local host title", () => {
    show();
    const card = screen.getByRole("region", { name: "Local host" });
    expect(
      within(card).getByText(
        "This Coaming host is a manager of edge swarm. It can be reached through the Docker socket, so it needs no agent.",
      ),
    ).toBeTruthy();
    expect(within(card).getByRole("button", { name: "Fold into edge swarm" })).toBeTruthy();
  });

  it("is not rendered unless a fold is offered", () => {
    show(env({ foldOffered: false }));
    expect(screen.queryByRole("region", { name: "Local host" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Fold into/ })).toBeNull();
  });

  it("is not rendered when the offer is on a local environment and no swarm environment offers one", () => {
    queryClient.setQueryData(environmentsQuery.queryKey, { items: [LOCAL] });
    show(LOCAL);
    expect(screen.queryByRole("button", { name: /Fold into/ })).toBeNull();
  });

  it("opens Fold this host into {swarm}? with the UI-SPEC body, Cancel and the primary confirm, and posts nothing yet", async () => {
    show();
    fireEvent.click(screen.getByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge swarm?" });
    expect(dialog.textContent).toContain(
      "Coaming will reach this node through the socket, and the separate Local socket environment is retired. Nothing changes on the host. This is recorded in the audit log.",
    );
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Fold into edge swarm" })).toBeTruthy();
    expect(posts()).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posts()).toEqual([]);
  });

  it("posts once however often confirm is clicked, shows the spinner, reads the environments and the nodes again, closes and says it is done", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const onFolded = show();
    fireEvent.click(screen.getByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge swarm?" });
    const confirm = within(dialog).getByRole("button", { name: "Fold into edge swarm" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() =>
      expect(within(dialog).getByRole("button", { name: "Folding…" })).toBeTruthy(),
    );
    expect(
      (within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect(posts()).toEqual([
      { method: "POST", path: "/api/environments/e2/fold-local", csrf: "csrf-1" },
    ]);

    await act(async () => release(Response.json({})));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const keys = invalidate.mock.calls.map((c) => JSON.stringify(c[0]?.queryKey));
    expect(keys).toContain(JSON.stringify(["environments"]));
    expect(keys).toContain(JSON.stringify(["agents", "e2"]));
    expect(
      await screen.findByText("edge swarm now reaches this host through the Docker socket."),
    ).toBeTruthy();
    expect(onFolded).toHaveBeenCalledWith("e2");
    expect(posts()).toHaveLength(1);
  });

  it("from the local host's own page names the swarm and posts to the swarm environment", async () => {
    show(LOCAL);
    expect(
      screen.getByText(
        "This Coaming host is a manager of edge swarm. It can be reached through the Docker socket, so it needs no agent.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe("/api/environments/e2/fold-local");
  });

  it("keeps the dialog open with an inline alert when the fold is no longer offered, and reads the environments again", async () => {
    answer = () =>
      Response.json(
        { title: "Conflict", status: 409, type: "fold_not_offered", detail: "no longer offered" },
        { status: 409 },
      );
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    show();
    fireEvent.click(screen.getByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain(
      "This host is no longer a manager of that swarm, or it was folded a moment ago. Nothing was changed.",
    );
    expect(screen.getByRole("dialog")).toBeTruthy();
    await waitFor(() =>
      expect(invalidate.mock.calls.map((c) => JSON.stringify(c[0]?.queryKey))).toContain(
        JSON.stringify(["environments"]),
      ),
    );
    // The confirm is usable again, for another try.
    expect(
      (within(dialog).getByRole("button", { name: "Fold into edge swarm" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });

  it("says Couldn't fold this host for any other failure", async () => {
    answer = () => Response.json({ title: "Internal", status: 500 }, { status: 500 });
    show();
    fireEvent.click(screen.getByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("Couldn't fold this host. Try again.");
  });
});
